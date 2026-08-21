package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// ProberCredentialResult is the response body of ProberCredential.
//
// It is deliberately NARROWER than the prober_identity row behind it, and
// narrower than the caller's authentication would strictly justify. Exactly two
// things leave this server:
//
//   - by_client_jwt, the *delivery* credential. It is revocable and re-mintable:
//     model.clearProberIdentityClient drops it and the bootstrap task mints
//     another against the same network. A leaked one is recovered by forcing a
//     re-mint, and costs nothing else.
//   - client_id, which names the client the jwt already names. It discloses
//     nothing the jwt does not, and the prober needs it to identify itself in
//     the routes it goes on to call.
//
// Everything else on the row is omitted ON PURPOSE, not by oversight.
// network_id, user_id and network_name are the ROOT identity: they are the
// handles the account-recovery flows key on -- /auth/regenerate-seedphrase
// mints a fresh seedphrase for an account identified this way -- and unlike the
// jwt they cannot be revoked or rotated, because the account IS them. Anything
// holding them can re-derive the account and mint credentials forever.
//
// A seedphrase cannot appear here even by accident: it is never persisted.
// createProberNetwork calls NetworkCreate and keeps only the network id, the
// admin user id and the name, so there is no column on prober_identity that
// could carry one. That is a property worth preserving -- do not add one.
//
// The rule this encodes: hand out the credential that can be replaced, never
// the identity that cannot.
type ProberCredentialResult struct {
	ByClientJwt string    `json:"by_client_jwt"`
	ClientId    server.Id `json:"client_id"`
}

// ProberCredential hands the operator's prober the network client jwt that the
// bootstrap task minted for it (see model/prober_identity_model.go and
// taskworker/work/prober_bootstrap_work.go).
//
// This is the last leg of that bootstrap. The task already creates the prober's
// account, funds it and mints the credential into prober_identity, but nothing
// read that column, so the credential still had to reach the prober process by
// hand -- an env file written by an operator. A prober that can fetch its own
// credential closes the loop: no human step remains between a fresh deployment
// and a probing prober.
//
// Same auth as the provider-egress endpoints it sits beside: operator-to-server,
// the shared X-UR-Operator-Secret header rather than a network jwt, fail-closed
// when the vault resource is missing. One secret, one mechanism -- this route
// hands out a credential, which is the strongest possible reason not to invent
// a second, less-examined way in.
func ProberCredential(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)

	// The two failure branches are logged SEPARATELY, and loudly, which is the
	// one place this deviates from the endpoints next door (they 401 in
	// silence). The deviation is the point: a credential endpoint that rejects
	// everything produces a prober that probes nothing, and a fleet that probes
	// nothing is indistinguishable from a fleet of unhealthy providers. That
	// exact misreading has already cost this system eight hours. Which side is
	// misconfigured is the first question anyone asks, so the log answers it
	// before it is asked.
	if secret == "" {
		// SERVER side: no vault resource, or no ingest_secret in it. Every
		// request is rejected regardless of what the caller sends.
		glog.Errorf(
			"[probercred]this server has no operator ingest secret " +
				"(provider_egress.yml/ingest_secret); the prober cannot fetch its " +
				"credential, so egress probing will not run at all\n",
		)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		// CALLER side: the prober's configured secret is absent or does not
		// match this server's. Neither the provided value nor any prefix of it
		// is logged -- a rejected secret is still a secret, and log shipping is
		// a wider audience than the vault.
		glog.Errorf(
			"[probercred]rejected a prober credential request: missing or wrong %s header; "+
				"the caller's operator secret does not match this server's\n",
			operatorSecretHeader,
		)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	identity := model.GetProberIdentity(r.Context())

	// 404 -- not a 500, and not an empty 200. The prober polls this before the
	// bootstrap task has necessarily finished, and it must be able to tell "not
	// ready yet, keep polling" from "broken, wake someone". An empty 200 makes
	// those two the same response.
	//
	// A missing row is NOT the only not-ready state, and checking only for it
	// would produce exactly that empty 200. The row is committed by
	// createProberNetwork before mintProberClientJwt runs, so it legitimately
	// exists with by_client_jwt still NULL; clearProberIdentityClient also
	// returns it to that state when a client has to be re-provisioned. All
	// three are the same answer to this caller: there is no credential yet.
	//
	// Not logged: the bootstrap task already reports its own failures at
	// Errorf, and a poll during normal startup is not an error.
	if identity == nil || identity.ClientId == nil || identity.ByClientJwt == "" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	result := &ProberCredentialResult{
		ByClientJwt: identity.ByClientJwt,
		ClientId:    *identity.ClientId,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[probercred]could not write response. err = %s\n", err)
	}
}
