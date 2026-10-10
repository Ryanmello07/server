package controller

// Account deletion on a deployment whose vault has no stripe.yml. Beta-only,
// with network_remove_stripe_beta.go: the beta runs with payments disabled.
//
// These tests go through NetworkRemove and use only what upstream's deletion
// tests use. Without the beta's Stripe step in the deletion's path they still
// compile, and the first two fail with the vault's own panic, which is what
// POST /auth/network-delete answered as HTTP 500.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// a resource name no vault has
const networkRemoveAbsentStripeResource = "synthetic-absent-stripe.yml"

// networkRemoveStripeNeverCalled points the Stripe API at a synthetic server
// that fails the test on any request, and replaces the Stripe token read. It
// returns the number of requests that reached the server.
func networkRemoveStripeNeverCalled(t testing.TB, tokenFunc func() string) func() int64 {
	t.Helper()
	requestCount := &atomic.Int64{}
	stripeServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		t.Errorf("Stripe was called without credentials: %s %s", request.Method, request.URL.Path)
		http.Error(response, "synthetic Stripe must not be called", http.StatusInternalServerError)
	}))

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	stripeApiBaseUrl = stripeServer.URL
	stripeApiTokenFunc = tokenFunc
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		stripeServer.Close()
	})
	return requestCount.Load
}

// networkRemoveStripeVaultAbsent makes the Stripe token read behave as it does
// on a vault with no stripe.yml. The read is stripeApiToken's own, the same
// resolver call cached by a sync.OnceValue, for a resource no vault has.
func networkRemoveStripeVaultAbsent(t testing.TB) func() int64 {
	t.Helper()
	absentToken := sync.OnceValue(func() string {
		c := server.Vault.RequireSimpleResource(networkRemoveAbsentStripeResource).Parse()
		return c["api"].(map[string]any)["token"].(string)
	})
	return networkRemoveStripeNeverCalled(t, func() string { return absentToken() })
}

// networkRemoveNoPanic runs the deletion and reports a panic as a failure, as
// the router reports it to the client as an HTTP 500.
func networkRemoveNoPanic(t testing.TB, clientSession *session.ClientSession) (result *NetworkRemoveResult, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf(
				"account deletion panicked (the api answers HTTP 500): %v. Is networkRemoveUnsubscribeStripe (network_remove_stripe_beta.go) still the Stripe step of defaultNetworkRemoveSteps?",
				r,
			)
		}
	}()
	return NetworkRemove(clientSession)
}

// TestNetworkRemoveWithoutStripeVaultRemovesNetwork proves that an account
// with nothing at Stripe, as far as the server can know without Stripe, is
// deleted on a deployment whose vault has no stripe.yml. Before the beta's
// step every such deletion panicked.
func TestNetworkRemoveWithoutStripeVaultRemovesNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		stripeRequestCount := networkRemoveStripeVaultAbsent(t)
		now := server.NowUtc()

		for i, testCase := range []struct {
			name string
			seed func(networkId server.Id)
		}{
			{
				name: "no subscription record",
				seed: func(networkId server.Id) {},
			},
			{
				// the one renewal on the beta is of this kind
				name: "a renewal in another market",
				seed: func(networkId server.Id) {
					if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
						NetworkId:          networkId,
						SubscriptionType:   model.SubscriptionTypeSupporter,
						StartTime:          now.Add(-time.Hour),
						EndTime:            now.Add(time.Hour),
						SubscriptionMarket: model.SubscriptionMarketManual,
					}); err != nil {
						t.Fatalf("add manual renewal: %v", err)
					}
				},
			},
			{
				// history, not a renewal that could bill: upstream's step does not
				// start from it either
				name: "a Stripe renewal that has ended",
				seed: func(networkId server.Id) {
					addNetworkRemoveStripeRenewal(t, ctx, networkId, "in_synthetic_ended", now.Add(-2*time.Hour), now.Add(-time.Hour))
				},
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-no-stripe-vault-%d", i), userId)
			userSession := networkRemoveTestSession(ctx, networkId, userId)
			testCase.seed(networkId)

			result, err := networkRemoveNoPanic(t, userSession)
			if err != nil || result == nil || result.Error != nil {
				t.Fatalf("%s: remove = %#v, %v, want success", testCase.name, result, err)
			}
			if model.GetNetwork(userSession) != nil {
				t.Fatalf("%s: the network is still there after a successful deletion", testCase.name)
			}
		}
		if count := stripeRequestCount(); count != 0 {
			t.Fatalf("Stripe requests = %d, want 0", count)
		}
	})
}

// TestNetworkRemoveWithoutStripeVaultRefusesLocalStripeRecord proves that a
// network the server knows at Stripe is not deleted when there are no
// credentials to cancel with: the deletion is refused with upstream's Stripe
// message and nothing is removed or closed.
func TestNetworkRemoveWithoutStripeVaultRefusesLocalStripeRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		stripeRequestCount := networkRemoveStripeVaultAbsent(t)
		now := server.NowUtc()

		for i, testCase := range []struct {
			name      string
			invoiceId string
			startTime time.Time
			endTime   time.Time
			customer  string
		}{
			{
				name:      "a renewal that has not ended",
				invoiceId: "in_synthetic_no_vault_active",
				startTime: now.Add(-time.Hour),
				endTime:   now.Add(time.Hour),
			},
			{
				// the model's removal refuses it too, with a transport error
				name:      "a renewal queued to start later",
				invoiceId: "in_synthetic_no_vault_queued",
				startTime: now.Add(time.Hour),
				endTime:   now.Add(2 * time.Hour),
			},
			{
				name:     "a Stripe customer id",
				customer: "cus_synthetic_no_vault",
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-no-stripe-vault-record-%d", i), userId)
			userSession := networkRemoveTestSession(ctx, networkId, userId)
			if testCase.invoiceId != "" {
				addNetworkRemoveStripeRenewal(t, ctx, networkId, testCase.invoiceId, testCase.startTime, testCase.endTime)
			}
			if testCase.customer != "" {
				if err := model.CreateStripeCustomer(testCase.customer, userSession); err != nil {
					t.Fatalf("%s: create Stripe customer: %v", testCase.name, err)
				}
			}

			result, err := networkRemoveNoPanic(t, userSession)
			if err != nil {
				t.Fatalf("%s: remove returned transport error: %v", testCase.name, err)
			}
			if result == nil || result.Error == nil || result.Error.Message != networkRemoveStripeFailedMessage {
				t.Fatalf("%s: remove result = %#v, want the Stripe failure message", testCase.name, result)
			}
			if model.GetNetwork(userSession) == nil {
				t.Fatalf("%s: the network was deleted without cancelling at Stripe", testCase.name)
			}
			if testCase.invoiceId != "" && !networkRemoveStripeRenewalActive(ctx, networkId, testCase.invoiceId) {
				t.Fatalf("%s: the refusal closed the local renewal", testCase.name)
			}
			if testCase.customer != "" {
				customerId, err := model.GetStripeCustomer(userSession)
				if err != nil || customerId == nil || *customerId != testCase.customer {
					t.Fatalf("%s: Stripe customer after the refusal = %v, %v", testCase.name, customerId, err)
				}
			}
		}
		if count := stripeRequestCount(); count != 0 {
			t.Fatalf("Stripe requests = %d, want 0", count)
		}
	})
}

// TestNetworkRemoveStripeVaultUnprovenAbsenceStillFails proves that only a
// vault that has no stripe.yml counts as a deployment without Stripe. Any
// other failure of the token read ends the deletion as it does in upstream's
// step: with the panic, and with the network in place.
func TestNetworkRemoveStripeVaultUnprovenAbsenceStillFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for i, testCase := range []struct {
			name       string
			panicValue any
		}{
			{
				name:       "unavailable",
				panicValue: fmt.Errorf("%w: synthetic vault that cannot be read", server.ErrResourceUnavailable),
			},
			{
				name: "unavailable and not found",
				panicValue: errors.Join(
					fmt.Errorf("%w: synthetic vault home that cannot be read", server.ErrResourceUnavailable),
					fmt.Errorf("%w in vault (stripe.yml)", server.ErrResourceNotFound),
				),
			},
			{
				name:       "not an error",
				panicValue: "synthetic malformed stripe.yml",
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-stripe-vault-unproven-%d", i), userId)
			userSession := networkRemoveTestSession(ctx, networkId, userId)

			stripeRequestCount := networkRemoveStripeNeverCalled(t, func() string { panic(testCase.panicValue) })
			var result *NetworkRemoveResult
			var err error
			panicValue := func() (panicValue any) {
				defer func() {
					panicValue = recover()
				}()
				result, err = NetworkRemove(userSession)
				return
			}()

			if panicValue == nil {
				t.Fatalf("%s: remove = %#v, %v, want the token read's panic", testCase.name, result, err)
			}
			if panicErr, ok := testCase.panicValue.(error); ok {
				if recovered, ok := panicValue.(error); !ok || !errors.Is(recovered, panicErr) {
					t.Fatalf("%s: remove panicked with %v, want %v", testCase.name, panicValue, testCase.panicValue)
				}
			} else if panicValue != testCase.panicValue {
				t.Fatalf("%s: remove panicked with %v, want %v", testCase.name, panicValue, testCase.panicValue)
			}
			if model.GetNetwork(userSession) == nil {
				t.Fatalf("%s: the network was deleted although the Stripe token could not be read", testCase.name)
			}
			if count := stripeRequestCount(); count != 0 {
				t.Fatalf("%s: Stripe requests = %d, want 0", testCase.name, count)
			}
		}
	})
}
