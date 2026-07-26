# Provider Probing P0 Prerequisites — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the three blockers that make automated provider geo/bandwidth probing impossible or meaningless — provider enumeration returning almost nothing, a provider count that advertises clients nobody can reach, and a provider identity that changes on every restart.

**Architecture:** Three independent changes across three repos. The prober asks the API for every reachable provider instead of only those meeting a user-facing quality bar. The server counts a provider only if it actually holds a Public provide key. The provider CLI persists the client identity the server issues it and reuses it, so `client_id` becomes stable across restarts and probe results survive.

**Tech Stack:** Go 1.26.5, PostgreSQL 16, the `connect` client library, `docopt` CLI parsing.

**Source spec:** `docs/superpowers/specs/2026-07-25-enforced-provider-geo-probing-design.md`

## Global Constraints

- `geolocate/` in the operator-proxy repo must remain **standard-library-only**. No `golang.org/x/...`.
- **Two branches per server-side change.** A feature branch off `beta/self-contained-env` PR'd to the beta fork `Ryanmello07/server`, plus a second branch carrying the same commits cherry-picked, PR'd to upstream `urnetwork/server` `main`. The same applies to `sn`.
- Remotes differ per checkout — confirm with `git remote -v` before pushing. In `/tmp/sandbox/server` they are **inverted**: `origin` = `urnetwork/server` (UPSTREAM), `fork` = `Ryanmello07/server`. In `/root/urnetwork/server`: `origin` = `Ryanmello07/server`, `upstream` = `urnetwork/server`. In `/root/urnetwork/sn`: `origin` = `Ryanmello07/sn`, `upstream` = `urfoundation/sn`. Never push a feature branch to an upstream repo.
- Provider provide modes are `None=0, Network=1, FriendsAndFamily=2, Public=3, Stream=4`. Only `Public` (3) allows serving a client outside the provider's own network.
- Do not change marketplace gating. Nothing in this plan may alter `PassesMinimums`, scoring weights, or which providers `find-providers2` is willing to return once enumeration is correct.
- Server tests need the local stack: `local/run-local.sh`, then `./test.sh -run TestName`. If Docker or `sudo` is unavailable, fall back to `go build ./...` and `go vet ./...` and **say so explicitly** rather than claiming tests passed.

---

## File Structure

| File | Responsibility | Change |
| --- | --- | --- |
| `cmd/egress-prober/main.go` (operator-proxy) | Prober CLI; builds the `find-providers2` request | Modify — add `force_minimum` |
| `cmd/egress-prober/main_test.go` (operator-proxy) | CLI-level tests | Modify — assert the wire field |
| `model/network_client_location_model.go` (server) | `UpdateClientLocations` + `UpdateClientScores` | Modify — require a Public provide key |
| `model/network_client_location_model_test.go` (server) | Model tests | Modify — add exclusion tests |
| `miner/clientidentity.go` (sn) | **New.** Read/write the persisted client id | Create |
| `miner/clientidentity_test.go` (sn) | **New.** Round-trip tests | Create |
| `miner/run.go` (sn) | `provideAuth` — mints a client every run | Modify — reuse persisted id |

---

## Task 1: Prober enumerates every reachable provider

**Repo:** `/tmp/sandbox/urnetwork-operator-proxy`, branch `main` (single repo, no cherry-pick).

**Context:** `find-providers2` filters candidates through `loadClientScores`, which drops any provider failing `PassesMinimums`. Measured on beta: 39 providers advertised, **1** returned. A geolocation census wants every provider that can accept a contract, not only those meeting a user-facing quality bar. `FindProviders2Args` already exposes `ForceMinimum bool \`json:"force_minimum"\`` server-side; the prober simply never sets it.

**Files:**
- Modify: `cmd/egress-prober/main.go:404-412` (the request struct in `findProvidersAtLocation`)
- Test: `cmd/egress-prober/main_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: nothing other tasks depend on.

- [ ] **Step 1: Write the failing test**

Add to `cmd/egress-prober/main_test.go`. It starts a stub server, captures the request body, and asserts the wire field — testing the actual JSON sent, not the Go struct, because the server only sees the JSON.

```go
func TestFindProvidersAtLocationRequestsForceMinimum(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %s", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"providers":[]}`))
	}))
	defer srv.Close()

	_, err := findProvidersAtLocation(context.Background(), srv.Client(), srv.URL, "jwt", "loc-1")
	if err != nil {
		t.Fatalf("findProvidersAtLocation: %s", err)
	}

	// a geolocation census must not be filtered by the user-facing quality
	// gate; without this the api returned 1 of 39 providers on beta
	if got["force_minimum"] != true {
		t.Errorf("force_minimum = %v, want true", got["force_minimum"])
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `cd /tmp/sandbox/urnetwork-operator-proxy && go test ./cmd/egress-prober/ -run TestFindProvidersAtLocationRequestsForceMinimum -v`

Expected: FAIL with `force_minimum = <nil>, want true`.

- [ ] **Step 3: Add the field**

Replace the request struct in `findProvidersAtLocation`:

```go
	// Mirrors model.FindProviders2Args (model/network_client_location_model.go).
	// ForceMinimum bypasses the PassesMinimums filter in loadClientScores.
	// That filter exists to keep low-quality providers out of user-facing
	// selection; a geolocation census wants every provider that can accept a
	// contract, so leaving it on returned 1 of 39 providers on beta.
	reqBody, err := json.Marshal(struct {
		Specs        []map[string]string `json:"specs"`
		Count        int                 `json:"count"`
		RankMode     string              `json:"rank_mode"`
		ForceMinimum bool                `json:"force_minimum"`
	}{
		Specs:        []map[string]string{{"location_id": locationId}},
		Count:        findProvidersCountPerLocation,
		RankMode:     "quality",
		ForceMinimum: true,
	})
```

- [ ] **Step 4: Run the test and the suite**

Run: `go test ./cmd/egress-prober/ -run TestFindProvidersAtLocationRequestsForceMinimum -v`
Expected: PASS

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -race -count=1`
Expected: all packages ok, `gofmt -l` prints nothing.

- [ ] **Step 5: Commit and push**

```bash
git add cmd/egress-prober/main.go cmd/egress-prober/main_test.go
git commit -m "fix(cmd): enumerate every reachable provider, not just those passing minimums"
git push origin main
```

---

## Task 2: Count a provider only if it can actually serve a stranger

**Repo:** `/root/urnetwork/server`. **Two branches** (see Global Constraints).

**Context:** `UpdateClientLocations` counts providers from `network_client_location_reliability` filtered only on `connected = true AND valid = true`, with no provide-mode check. Every connected client is advertised as a public provider. Measured on beta: 39 advertised for the US, while only **2** clients held a `provide_mode = 3` key — so ~95% of advertised supply could not accept a contract from any user. This is a user-visible bug today, independent of probing.

**Critical:** there are **two** filters, not one. The comment at `model/network_client_location_model_test.go:1294` records that `GetProviderLocations` also gates on `loadLocationStables`, populated by `UpdateClientScores`. The earlier reliability fix had to change both; so does this one. Changing only `UpdateClientLocations` will not fix the symptom.

**Files:**
- Modify: `model/network_client_location_model.go:1492-1522` (`UpdateClientLocations` query)
- Modify: `model/network_client_location_model.go:2436-2470` (`UpdateClientScores` source query)
- Test: `model/network_client_location_model_test.go`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: nothing other tasks depend on.

- [ ] **Step 1: Create both branches**

```bash
cd /root/urnetwork/server
git checkout beta/self-contained-env && git pull --ff-only origin beta/self-contained-env
git checkout -b feat/provider-count-provide-mode
```

- [ ] **Step 2: Write the failing test**

Add to `model/network_client_location_model_test.go`. Two connected+valid clients in the same country; only one holds a Public key.

```go
func TestUpdateClientLocationsCountsOnlyPublicProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		handlerId := CreateNetworkClientHandler(ctx)

		// connect a client and give it the provide modes supplied
		connectOne := func(modes map[ProvideMode][]byte) server.Id {
			networkId := server.NewId()
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.1:0", handlerId)
			connect.AssertEqual(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
			connect.AssertEqual(t, err, nil)
			if modes != nil {
				SetProvide(ctx, clientId, modes)
			}
			return clientId
		}

		// serves strangers -- must be counted
		connectOne(map[ProvideMode][]byte{
			ProvideModePublic:  []byte("public-secret"),
			ProvideModeNetwork: []byte("network-secret"),
		})
		// own network only -- must NOT be counted, it cannot accept a
		// contract from a user outside its network
		connectOne(map[ProvideMode][]byte{
			ProvideModeNetwork: []byte("network-secret"),
		})
		// no provide key at all -- must NOT be counted
		connectOne(nil)

		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		err := UpdateClientLocations(ctx, time.Hour)
		connect.AssertEqual(t, err, nil)

		initialClientLocations, err := loadInitialClientLocations(ctx)
		connect.AssertEqual(t, err, nil)
		if initialClientLocations == nil {
			t.Fatal("expected a populated client locations cache, got nil")
		}

		found := false
		for _, clientLocation := range initialClientLocations.Locations {
			if clientLocation.LocationId == city.CountryLocationId {
				found = true
				// exactly one of the three connected clients holds a Public
				// key; counting the other two advertises supply no user can
				// reach (39 advertised vs 2 reachable, observed on beta)
				connect.AssertEqual(t, clientLocation.ClientCount, 1)
			}
		}
		connect.AssertEqual(t, found, true)
	})
}
```

`ClientCount` is the field on `ClientLocation` (`model/network_client_location_model.go:1440`). The API-facing `ProviderCount` is populated from it at line 1994 — assert on `ClientCount` here, since that is what `loadInitialClientLocations` returns.

- [ ] **Step 3: Run it and confirm it fails**

```bash
cd /root/urnetwork/server && ./local/run-local.sh
./test.sh -run TestUpdateClientLocationsCountsOnlyPublicProviders
```

Expected: FAIL with a provider count of 3 (all connected clients counted) rather than 1.

If the local stack cannot start, stop and report — do not proceed on an unverified test.

- [ ] **Step 4: Add the filter to `UpdateClientLocations`**

In the query at `model/network_client_location_model.go:1492`, add the clause below to the `WHERE`, and pass `ProvideModePublic` as the query argument:

```sql
	        WHERE
	        	network_client_location_reliability.connected = true AND
	        	network_client_location_reliability.valid = true AND
	        	-- a provider that does not hold a Public provide key cannot
	        	-- accept a contract from a client outside its own network, so
	        	-- counting it advertises supply nobody can reach. GetProvideRelationship
	        	-- returns ProvideModePublic for cross-network pairs, and the
	        	-- destination must hold a key for exactly that mode.
	        	EXISTS (
	        		SELECT 1 FROM provide_key
	        		WHERE
	        			provide_key.client_id = network_client_location_reliability.client_id AND
	        			provide_key.provide_mode = $1
	        	)
```

Change the `tx.Query(ctx, ...)` call to pass the argument:

```go
		result, err := tx.Query(
			ctx,
			`
	        ... query text ...
	        `,
			ProvideModePublic,
		)
```

- [ ] **Step 5: Add the same filter to `UpdateClientScores`**

The source query in `UpdateClientScores` (`model/network_client_location_model.go:2436`, the one selecting `network_client_location_reliability.city_location_id, ... max_net_type_score, ...`) has the identical `WHERE connected = true AND valid = true`. Add the same `EXISTS` clause and pass `ProvideModePublic` as an additional argument, keeping any existing arguments in order.

Without this, `loadLocationStables` still contains non-public providers and `GetProviderLocations` keeps showing them — the exact trap the earlier reliability fix hit.

- [ ] **Step 6: Run the test and the model suite**

```bash
./test.sh -run TestUpdateClientLocationsCountsOnlyPublicProviders
./test.sh -run TestUpdateClientLocations
./test.sh -run TestGetProviderLocations
go build ./... && go vet ./...
```

Expected: the new test PASSes, no previously-passing test regresses.

- [ ] **Step 7: Commit and open the beta PR**

```bash
git add model/network_client_location_model.go model/network_client_location_model_test.go
git commit -m "fix(model): count only providers holding a Public provide key

A provider without a provide_mode=3 key cannot accept a contract from a
client outside its own network, so counting it advertises supply nobody
can reach. On beta this reported 39 US providers when 2 were reachable.

Filters both UpdateClientLocations and UpdateClientScores: GetProviderLocations
gates on loadLocationStables as well, so fixing only the first leaves the
symptom in place."
git push -u origin feat/provider-count-provide-mode
gh pr create --repo Ryanmello07/server --base beta/self-contained-env \
  --head feat/provider-count-provide-mode \
  --title "fix(model): count only providers holding a Public provide key" \
  --body "Filters provider counting to clients holding provide_mode=3. Observed on beta: 39 US providers advertised, 2 reachable. Both UpdateClientLocations and UpdateClientScores are filtered, since GetProviderLocations also gates on loadLocationStables."
```

- [ ] **Step 8: Cherry-pick onto the upstream branch and open the upstream PR**

```bash
cd /root/urnetwork/server
git fetch upstream main
git checkout -b feat/provider-count-provide-mode-upstream upstream/main
git cherry-pick feat/provider-count-provide-mode
```

The beta branch carries a `LEFT JOIN client_connection_reliability_score` with a long `fix(beta)` comment that **upstream does not have** — upstream still uses the INNER JOIN. Expect a conflict in the `WHERE`/join region. Resolve by keeping **upstream's** join exactly as it is and adding only the `EXISTS (... provide_mode = $1)` clause plus the new argument. Do not carry the beta join comment upstream; it describes a beta-only condition.

```bash
go build ./... && go vet ./...
git push -u origin feat/provider-count-provide-mode-upstream
gh pr create --repo urnetwork/server --base main \
  --head Ryanmello07:feat/provider-count-provide-mode-upstream \
  --title "fix(model): count only providers holding a Public provide key" \
  --body "A provider without a provide_mode=3 key cannot accept a contract from a client outside its own network, so counting it advertises unreachable supply. Filters both UpdateClientLocations and UpdateClientScores, since GetProviderLocations gates on loadLocationStables as well."
```

---

## Task 3: Provider CLI reuses its client identity across restarts

**Repo:** `/root/urnetwork/sn`. **Two branches** (beta `beta/custom-server`, upstream `main`).

**Context:** `provideAuth` (`miner/run.go:681`) reads the **network** JWT from `~/.urnetwork/jwt`, then unconditionally calls `api.AuthNetworkClient` with no `ClientId`, so the server mints a brand-new client every run. The resulting `byClientJwt` is used in memory and never persisted.

Verified empirically: one provider restarted four times produced four distinct `client_id`s **and** four distinct `device_id`s, under both the `auth-provide` and `provide` subcommands. The production ansible unit (`urnetwork/xops`, `warp-community-provider-beta.service`) runs `auth-provide` with `Restart=always`, so this churns in production too — one host in the beta data has **19** client ids.

Consequences: probe results and probation are keyed by `client_id`, so a restart discards a provider's verified location, measured bandwidth and reliability history. Under a 12h probation window, a provider restarting more often than every 12 hours can never graduate.

`AuthNetworkClientArgs` already carries `ClientId *Id \`json:"client_id,omitempty"\`` — the server reuses the identity when it is supplied and mints a new one only when omitted. The fix is to persist and send it.

**Files:**
- Create: `miner/clientidentity.go`
- Create: `miner/clientidentity_test.go`
- Modify: `miner/run.go:681-745` (`provideAuth`)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `readStoredClientId(dir string) (*connect.Id, error)` and `writeStoredClientId(dir string, clientId connect.Id) error` in package `miner`.

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/sn
git checkout beta/custom-server && git pull --ff-only origin beta/custom-server
git checkout -b feat/provider-stable-client-identity
```

- [ ] **Step 2: Write the failing test**

Create `miner/clientidentity_test.go`. Pure filesystem logic, no network. Note the package is `miner`, not `main`:

```go
package miner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
)

func TestStoredClientIdRoundTrips(t *testing.T) {
	dir := t.TempDir()

	id, err := readStoredClientId(dir)
	if err != nil {
		t.Fatalf("read on empty dir: %s", err)
	}
	if id != nil {
		t.Fatalf("expected no stored id, got %s", id)
	}

	want := connect.NewId()
	if err := writeStoredClientId(dir, want); err != nil {
		t.Fatalf("write: %s", err)
	}

	got, err := readStoredClientId(dir)
	if err != nil {
		t.Fatalf("read after write: %s", err)
	}
	if got == nil {
		t.Fatal("expected a stored id after write, got nil")
	}
	if *got != want {
		t.Errorf("stored id = %s, want %s", got, want)
	}
}

func TestStoredClientIdIgnoresCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "client_id"), []byte("not-a-uuid"), 0600); err != nil {
		t.Fatal(err)
	}

	// a corrupt file must not wedge the provider: it re-auths and overwrites
	// rather than refusing to start
	id, err := readStoredClientId(dir)
	if err != nil {
		t.Fatalf("read corrupt file: %s", err)
	}
	if id != nil {
		t.Errorf("expected nil for a corrupt file, got %s", id)
	}
}

func TestStoredClientIdFileIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	if err := writeStoredClientId(dir, connect.NewId()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "client_id"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		t.Errorf("client_id mode = %o, want no group/other access", perm)
	}
}
```

- [ ] **Step 3: Run it and confirm it fails**

Run: `cd /root/urnetwork/sn && go test ./miner/ -run TestStoredClientId -v`
Expected: FAIL to compile — `undefined: readStoredClientId`.

- [ ] **Step 4: Implement the store**

Create `miner/clientidentity.go`:

```go
package miner

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/urnetwork/connect"
)

// clientIdFileName holds the client id the platform issued this provider.
// It sits beside the network jwt in ~/.urnetwork.
const clientIdFileName = "client_id"

// readStoredClientId returns the client id persisted in dir, or nil when none
// is stored or the stored value is unusable.
//
// A missing or corrupt file is deliberately NOT an error. The caller falls
// back to authenticating a fresh client, which is exactly the old behaviour --
// a provider must still start when its state directory is damaged.
func readStoredClientId(dir string) (*connect.Id, error) {
	b, err := os.ReadFile(filepath.Join(dir, clientIdFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	clientId, parseErr := connect.ParseId(strings.TrimSpace(string(b)))
	if parseErr != nil {
		return nil, nil
	}
	return &clientId, nil
}

// writeStoredClientId persists the client id so the next run reuses this
// identity instead of having the platform mint a new one.
func writeStoredClientId(dir string, clientId connect.Id) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, clientIdFileName), []byte(clientId.String()), 0600)
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./miner/ -run TestStoredClientId -v`
Expected: all three PASS.

- [ ] **Step 6: Wire it into `provideAuth`**

In `miner/run.go`, inside `provideAuth`, after `byJwt` is read and before `api.AuthNetworkClient` is called, load any stored id and put it on the args. Replace the `authClientArgs` construction:

```go
	urNetworkDir := filepath.Join(home, ".urnetwork")

	// Reuse the identity the platform issued us on a previous run. Without
	// this the server mints a NEW client_id on every start (AuthNetworkClient
	// creates one whenever ClientId is omitted), so a restart discards the
	// provider's probed location, measured bandwidth and reliability history,
	// and it must serve out a fresh probation before it can be selected again.
	// One host in the beta data accumulated 19 client ids this way.
	storedClientId, err := readStoredClientId(urNetworkDir)
	if err != nil {
		returnErr = err
		return
	}

	authClientArgs := &connect.AuthNetworkClientArgs{
		ClientId:    storedClientId,
		Description: fmt.Sprintf("provider %s %s", runtime.GOOS, RequireVersion()),
		DeviceSpec:  "",
	}
```

Then, after `clientId` has been parsed out of `byClientJwt` near the end of the function and before `return`, persist it:

```go
	// persist for the next run; a failure here must not stop the provider
	// from serving, it only means the next restart re-auths as it does today
	if writeErr := writeStoredClientId(urNetworkDir, clientId); writeErr != nil {
		fmt.Printf("could not persist client id to %s: %s\n", urNetworkDir, writeErr)
	}

	return
```

- [ ] **Step 7: Verify the whole package builds and tests**

```bash
cd /root/urnetwork/sn
go build ./... && go vet ./miner/ && gofmt -l miner/
go test ./miner/ -count=1
```

Expected: build clean, `gofmt -l` prints nothing, tests pass.

- [ ] **Step 8: Live verification against beta**

This is the claim that matters, and unit tests cannot make it. Build and run the provider twice against the beta server, and confirm the client id is identical across the restart.

```bash
cd /root/urnetwork/sn
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/urprovider ./miner
```

Deploy to a test host, run `auth-provide` once with a network JWT, note the printed `client_id`, then restart the service and note it again.

```sql
SELECT client_id, create_time FROM network_client
WHERE network_id = '<test network id>' ORDER BY create_time;
```

Expected: **one row**, not one per restart. Before this change the same procedure produced four rows for four restarts.

- [ ] **Step 9: Commit and open both PRs**

```bash
git add miner/clientidentity.go miner/clientidentity_test.go miner/run.go
git commit -m "fix(miner): reuse the issued client identity across restarts

provideAuth called AuthNetworkClient with no ClientId, so the platform
minted a new client on every start and the issued byClientJwt was never
persisted. A restart therefore discarded the provider's probed location,
measured bandwidth and reliability history, and forced a fresh probation.

Verified: four restarts previously produced four client ids and four
device ids, under both auth-provide and provide. The production ansible
unit runs auth-provide with Restart=always; one beta host accumulated 19
client ids.

A missing or corrupt client_id file falls back to authenticating a fresh
client, so a damaged state directory cannot stop a provider starting."
git push -u origin feat/provider-stable-client-identity
gh pr create --repo Ryanmello07/sn --base beta/custom-server \
  --head feat/provider-stable-client-identity \
  --title "fix(miner): reuse the issued client identity across restarts" \
  --body "Persists the client id the platform issues and sends it on re-auth, so client_id is stable across restarts. Previously every restart minted a new identity, discarding probe results and reliability history."

git fetch upstream main
git checkout -b feat/provider-stable-client-identity-upstream upstream/main
git cherry-pick feat/provider-stable-client-identity
go build ./... && go test ./miner/ -count=1
git push -u origin feat/provider-stable-client-identity-upstream
gh pr create --repo urfoundation/sn --base main \
  --head Ryanmello07:feat/provider-stable-client-identity-upstream \
  --title "fix(miner): reuse the issued client identity across restarts" \
  --body "Persists the client id the platform issues and sends it on re-auth. Previously AuthNetworkClient was called with no ClientId on every start, so each restart minted a new identity."
```

Note: `sn`'s beta branch is 4 commits ahead of main in `miner/` only. If the cherry-pick conflicts, resolve in favour of upstream's surrounding code and keep only the identity-persistence changes.

---

## Verification Summary

| Task | Gate |
| --- | --- |
| 1 | `go test ./... -race -count=1` in operator-proxy; new test asserts `force_minimum` in the JSON body |
| 2 | `./test.sh -run TestUpdateClientLocationsCountsOnlyPublicProviders` passes and no `TestUpdateClientLocations*` / `TestGetProviderLocations*` regresses |
| 3 | Three unit tests pass, plus the live two-restart check showing a single `network_client` row |
| All | Both PRs open per server-side change: beta fork and upstream |

## Out of Scope

- Letting measured bandwidth satisfy `PassesMinimums`, and whether a provider should be penalised for tests its transport version cannot run. Both change who earns.
- Any change to scoring weights or `PassesMinimums` itself.
- Migrating providers that already hold multiple client ids. Existing duplicates age out through the normal reap path once they stop connecting.
