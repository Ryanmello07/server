package controller

import (
	"errors"
	"fmt"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// networkRemoveUnsubscribeStripe is the Stripe step of an account deletion on a
// deployment that may have no Stripe credentials.
//
// Beta-only. Upstream's step, UnsubscribeStripe, asks Stripe about every
// network it deletes: besides the subscriptions the local rows name, it
// searches Stripe for subscriptions that carry the network id as metadata. It
// reads the API token from the vault's stripe.yml for that, and the read
// panics when the vault has no such file. The beta's vault has none (payments
// are disabled), so POST /auth/network-delete answered HTTP 500 for every
// account.
//
// With a token, nothing changes: upstream's step runs as it is. Without
// stripe.yml the server cannot ask Stripe anything, so the step decides on the
// two things upstream's step reads locally before it calls Stripe: a Stripe
// renewal that has not ended, and the network's Stripe customer id. With
// neither there is nothing the server knows of to cancel, and the deletion
// goes on. With either, the step fails closed, as upstream's deletion does for
// a renewal it cannot stop: the caller answers upstream's
// networkRemoveStripeFailedMessage and the network stays.
//
// What it cannot do without a token is upstream's search of Stripe by network
// id, so a subscription that only Stripe knows of is not found. The server
// cannot have created one on a deployment that never had Stripe credentials;
// it can have on one whose stripe.yml was taken away.
//
// It lives in its own file, with one changed line in
// defaultNetworkRemoveSteps, so that the fork carries no edit inside
// upstream's functions (as api/cors_beta.go does for the runner).
func networkRemoveUnsubscribeStripe(clientSession *session.ClientSession) error {
	if stripeApiTokenReadable() {
		return UnsubscribeStripe(clientSession)
	}

	renewalNotEnded := false
	server.Db(clientSession.Ctx, func(conn server.PgConn) {
		// the renewals RemoveNetworkWithStoreSnapshot refuses to delete over
		result, err := conn.Query(
			clientSession.Ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM subscription_renewal
					WHERE network_id = $1
						AND market = $2
						AND now() < end_time
				)
			`,
			clientSession.ByJwt.NetworkId,
			model.SubscriptionMarketStripe,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&renewalNotEnded))
			}
		})
	})
	stripeCustomerId, err := model.GetStripeCustomer(clientSession)
	if err != nil {
		return fmt.Errorf("[unsubscribe] failed to query Stripe customer: %w", err)
	}

	if renewalNotEnded || stripeCustomerId != nil {
		return fmt.Errorf(
			"[unsubscribe] the vault has no stripe.yml to cancel with, and the network has a Stripe record (renewal not ended: %t, customer id: %t)",
			renewalNotEnded,
			stripeCustomerId != nil,
		)
	}
	glog.Infof(
		"[network]remove %s: the vault has no stripe.yml and the network has no Stripe record; nothing to cancel at Stripe\n",
		clientSession.ByJwt.NetworkId,
	)
	return nil
}

// stripeApiTokenReadable reports whether the Stripe API token can be read. It
// is false only when the read fails because the vault has no stripe.yml, the
// one failure that says Stripe is not set up on this deployment. Every other
// failure of the read (a file that cannot be inspected, a file without a
// token) is raised again, so it ends the deletion as it does in upstream's
// step.
//
// The read goes through stripeApiTokenFunc, where every raw Stripe call takes
// its token, so a test's synthetic Stripe counts as set up.
func stripeApiTokenReadable() (readable bool) {
	defer func() {
		if r := recover(); r != nil {
			err, ok := r.(error)
			if !ok || !errors.Is(err, server.ErrResourceNotFound) || errors.Is(err, server.ErrResourceUnavailable) {
				panic(r)
			}
			readable = false
		}
	}()
	stripeApiTokenFunc()
	return true
}
