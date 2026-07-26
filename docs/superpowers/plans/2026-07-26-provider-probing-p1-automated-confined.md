# Provider Probing P1 — Automated, Confined Probing

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the operator's one-shot geolocation prober into an automated worker that runs confined, picks its work from durable server-side state, and refuses to start unless its confinement is actually in place.

**Architecture:** The prober stays an unprivileged process that assumes nothing about how it is confined. Each deployment supplies confinement natively — a restricted Docker network on beta, systemd `IPAddressDeny`/`IPAddressAllow` upstream — and the prober verifies it at startup by attempting a direct connection to a geolocation address and exiting if it succeeds. Due-provider selection moves from the prober's in-memory TTL cache to a server endpoint backed by `provider_egress_location` freshness, so a restart no longer re-probes everything.

**Tech Stack:** Go 1.26.5, PostgreSQL 16, Redis, Docker Compose (beta only), systemd (mainstream).

**Source spec:** `docs/superpowers/specs/2026-07-25-enforced-provider-geo-probing-design.md`
**Predecessor:** `docs/superpowers/plans/2026-07-26-provider-probing-p0-prerequisites.md` (complete)

## Global Constraints

- **The hard constraint:** the operator's server must never send a request directly to a geolocation API. Every lookup egresses through a provider. Nothing in this plan may add a code path that could contact `ip.pn`, `free.freeipapi.com` or `ipinfo.io` other than through a provider tunnel.
- `geolocate/` in the operator-proxy repo must remain **standard-library-only**. No `golang.org/x/...`.
- The prober must require **no Linux capabilities** and must not create namespaces or firewall rules itself. Beta runs Docker Compose; the mainstream deployment does not use Docker at all. Anything that only works under one is wrong.
- **Two branches per server-side change:** a feature branch off `beta/self-contained-env` PR'd to `Ryanmello07/server`, plus a cherry-picked branch PR'd to `urnetwork/server` `main`.
- Remotes differ per checkout — run `git remote -v` before pushing. `/root/urnetwork/server`: `origin` = `Ryanmello07/server`, `upstream` = `urnetwork/server`. `/tmp/sandbox/server` has these **inverted**. Never push a feature branch to an upstream repo.
- Do not alter `PassesMinimums`, scoring weights, or thresholds.
- P1 is **direct probing only** — no identity rotation, no multi-hop, no verdict logic. Those are P3 and P2. Results land exactly as they do today.
- Server tests need the local stack: `local/run-local.sh`, then `./test.sh -run TestName`. A whole-package `go test ./model` is not achievable in this environment (pre-existing missing `subsidy.yml`); say so explicitly rather than claiming it passed.
- A **live beta deployment** runs on the dev machine via `docker-compose.beta.yml`. Never stop, recreate or `down` it.

---

## File Structure

| File | Responsibility | Change |
| --- | --- | --- |
| `model/network_client_location_model.go` (server) | `loadLocationStables` hardcodes `forceMinimum=false` | Modify — thread the flag through |
| `model/provider_egress_location_model.go` (server) | Egress location storage | Modify — add due-provider query |
| `api/handlers/provider_egress_location_handlers.go` (server) | Operator-authenticated ingest | Modify — add the due-list handler |
| `api/api.go` (server) | Route table | Modify — register the route |
| `confinement/confinement.go` (operator-proxy) | **New.** Startup self-check | Create |
| `cmd/egress-prober/main.go` (operator-proxy) | Prober CLI | Modify — self-check + server-driven due list |
| `docker-compose.beta.yml` + `BETA.md` (server) | Beta deployment | Modify — confined prober service |
| `docs/operator/prober-systemd.md` (server) | **New.** Non-Docker deployment | Create |

---

## Task 1: Location enumeration honours `force_minimum`

**Repo:** `/root/urnetwork/server`. Two branches: `feat/probe-location-force-minimum` and `-upstream`.

**Context:** P0 made the prober send `force_minimum: true` to `find-providers2`, which bypasses `PassesMinimums` for *provider* selection. But the prober gets its **locations** from `GET /network/provider-locations`, which only emits a location if `loadLocationStables` has an entry — and that reads `clientScoreLocationFilterKey(false, rankMode, ...)` at `model/network_client_location_model.go:1877` with `forceMinimum` **hardcoded false**.

Consequence: a location where every provider fails minimums is invisible to the prober, so those providers are never probed and can never graduate probation. `force_minimum` on the second call cannot recover what the first call never listed. This was the Important finding from the P0 final review.

**Files:**
- Modify: `model/network_client_location_model.go:1858-1890` (`loadLocationStables`) and its callers
- Test: `model/network_client_location_model_test.go`

**Interfaces:**
- Produces: `loadLocationStables(ctx, rankMode, forceMinimum bool, locationIds, ...)` — exact parameter order to be matched to the existing signature; add `forceMinimum` adjacent to `rankMode`.

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/server
git checkout beta/self-contained-env && git pull --ff-only origin beta/self-contained-env
git checkout -b feat/probe-location-force-minimum
```

- [ ] **Step 2: Read before writing**

Read `loadLocationStables` in full (`model/network_client_location_model.go:1858` onward) and find every caller with `grep -n 'loadLocationStables' model/*.go controller/*.go`. `GetProviderLocations` is the caller that matters. Note whether the writer side (`UpdateClientScores`, around `:2848`) already writes **both** the `forceMinimum=false` and `forceMinimum=true` key families — it does, since `exportClientScores` is invoked in a `for _, forceMinimum := range []bool{false, true}` loop. This task only fixes the read side.

- [ ] **Step 3: Write the failing test**

Add to `model/network_client_location_model_test.go`. One connected+valid Public provider whose score falls below the minimums, asserting it is invisible with `forceMinimum=false` and visible with `true`:

```go
func TestLoadLocationStablesHonoursForceMinimum(t *testing.T) {
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

		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "0.0.0.1:0", handlerId)
		connect.AssertEqual(t, err, nil)
		err = SetConnectionLocation(ctx, connectionId, city.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic:  []byte("public-secret"),
			ProvideModeNetwork: []byte("network-secret"),
		})

		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())
		err = UpdateClientScores(ctx)
		connect.AssertEqual(t, err, nil)

		locationIds := map[server.Id]bool{city.CountryLocationId: true}

		// this provider has no latency or speed test, so scoring penalises it
		// past the minimums gate -- exactly the population the prober needs to
		// reach and currently cannot see
		strict, err := loadLocationStables(ctx, RankModeQuality, false, locationIds, server.Id{})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(strict), 0)

		forced, err := loadLocationStables(ctx, RankModeQuality, true, locationIds, server.Id{})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(forced), 1)
	})
}
```

Adapt the argument list to `loadLocationStables`'s real signature — read it first; do not guess the parameter order. If the function returns a map keyed differently, assert on presence of `city.CountryLocationId` rather than length.

- [ ] **Step 4: Run it and confirm it fails**

```bash
cd /root/urnetwork/server && ./local/run-local.sh --keep-up
./test.sh -run TestLoadLocationStablesHonoursForceMinimum
```

Expected: a compile failure (`too many arguments`) because `loadLocationStables` does not yet take `forceMinimum`. That is the correct first failure.

- [ ] **Step 5: Thread the flag through**

Add a `forceMinimum bool` parameter to `loadLocationStables` and pass it to `clientScoreLocationFilterKey` in place of the literal `false` at `:1877`. Update every caller. `GetProviderLocations` must pass `false` — user-facing location listing keeps today's behaviour and must not change.

Add this comment above the parameter:

```go
	// forceMinimum selects which pre-computed key family to read. The writer
	// (UpdateClientScores) populates both, so this only chooses between them.
	// User-facing listing passes false and keeps today's behaviour; an operator
	// census passes true, because a location where every provider fails the
	// minimums gate is otherwise invisible and its providers can never be
	// probed or graduate probation.
```

- [ ] **Step 6: Run the test and the regressions**

```bash
./test.sh -run TestLoadLocationStablesHonoursForceMinimum
./test.sh -run TestUpdateClientScores
./test.sh -run TestUpdateClientLocations
go build ./... && go vet ./...
```

Expected: the new test passes; no previously-passing test regresses.

- [ ] **Step 7: Commit, push, open both PRs**

```bash
git add model/network_client_location_model.go model/network_client_location_model_test.go
git commit -m "fix(model): let loadLocationStables honour forceMinimum

The read side hardcoded forceMinimum=false, so a location where every
provider fails the minimums gate was invisible even to callers that
explicitly bypass minimums. Providers there could never be probed and so
could never graduate probation. The writer already populates both key
families; this only chooses between them. User-facing listing still
passes false."
git push -u origin feat/probe-location-force-minimum
gh pr create --repo Ryanmello07/server --base beta/self-contained-env \
  --head feat/probe-location-force-minimum \
  --title "fix(model): let loadLocationStables honour forceMinimum" \
  --body "loadLocationStables hardcoded forceMinimum=false on the read side, hiding locations where every provider fails minimums. The writer already populates both key families. User-facing listing is unchanged."

git fetch upstream main
git checkout -b feat/probe-location-force-minimum-upstream upstream/main
git cherry-pick feat/probe-location-force-minimum
go build ./... && go vet ./...
git push -u origin feat/probe-location-force-minimum-upstream
gh pr create --repo urnetwork/server --base main \
  --head Ryanmello07:feat/probe-location-force-minimum-upstream \
  --title "fix(model): let loadLocationStables honour forceMinimum" \
  --body "The read side hardcoded forceMinimum=false, hiding locations where every provider fails the minimums gate from callers that explicitly bypass minimums."
```

If the cherry-pick conflicts, keep upstream's surrounding code and carry only the parameter and its use.

---

## Task 2: Server-side due-provider selection

**Repo:** `/root/urnetwork/server`. Two branches: `feat/probe-due-endpoint` and `-upstream`.

**Context:** the prober currently decides what to probe from an in-memory TTL cache, so a restart re-probes everything and there is no durable record of what is due. Freshness already lives server-side in `provider_egress_location.observed_at`. Expose it, so selection is durable and the server owns the schedule.

The endpoint is operator-to-server, authenticated by the same shared secret as the ingest endpoint — not a network JWT.

**Files:**
- Modify: `model/provider_egress_location_model.go` — add the query
- Modify: `api/handlers/provider_egress_location_handlers.go` — add the handler
- Modify: `api/api.go:57` area — register the route
- Test: `model/provider_egress_location_model_test.go`, `api/handlers/provider_egress_location_handlers_test.go`

**Interfaces:**
- Produces: `model.GetProviderEgressLocationDue(ctx context.Context, minObservedAt time.Time, limit int) []server.Id` — client ids of Public providers whose newest probe is older than `minObservedAt` or absent.
- Produces: `GET /network/provider-egress-due?limit=N` returning `{"client_ids":["..."]}`.

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/server
git checkout beta/self-contained-env && git pull --ff-only origin beta/self-contained-env
git checkout -b feat/probe-due-endpoint
```

- [ ] **Step 2: Write the failing model test**

Add to `model/provider_egress_location_model_test.go`:

```go
func TestGetProviderEgressLocationDue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		fresh := server.NewId()
		stale := server.NewId()
		never := server.NewId()

		location := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, location)

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: fresh, LocationId: location.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-1 * time.Hour),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: stale, LocationId: location.LocationId,
			CountryCode: "us", ObservedAt: now.Add(-72 * time.Hour),
		})
		// `never` deliberately gets no row at all

		due := GetProviderEgressLocationDue(ctx, now.Add(-24*time.Hour), 100)

		// a provider probed an hour ago must not be re-probed; one probed three
		// days ago must be; one never probed must be
		connect.AssertEqual(t, slices.Contains(due, fresh), false)
		connect.AssertEqual(t, slices.Contains(due, stale), true)
		connect.AssertEqual(t, slices.Contains(due, never), true)
	})
}
```

Note: `never` only appears if the query sources candidates from connected Public providers rather than from `provider_egress_location` alone. That is the point — the set of things needing a probe is mostly things with no row yet. The test will need `never` set up as a connected client with a Public provide key, the same way Task 2 of the P0 plan did (`Testing_CreateDevice` + `ConnectNetworkClient` + `SetConnectionLocation` + `SetProvide`). Do that for all three ids so the only variable is probe freshness.

- [ ] **Step 3: Run it and confirm it fails**

Run: `./test.sh -run TestGetProviderEgressLocationDue`
Expected: compile failure, `undefined: GetProviderEgressLocationDue`.

- [ ] **Step 4: Implement the query**

Add to `model/provider_egress_location_model.go`. Source candidates from connected+valid Public providers, LEFT JOIN their egress row, and select those whose `observed_at` is missing or older than the cutoff. Mirror the P0 filter exactly — a provider without `provide_mode = 3` is not probeable and must not be returned. Order oldest-first so the longest-unprobed go first, and honour `limit`.

Compute the cutoff in Go and pass it as an argument. **Do not** compare a naive `timestamp` column against SQL `now()` — this codebase has shipped that bug before; the comparison casts through the session timezone and silently skips a window.

- [ ] **Step 5: Run the model test**

Run: `./test.sh -run TestGetProviderEgressLocationDue`
Expected: PASS.

- [ ] **Step 6: Write the failing handler test**

Add to `api/handlers/provider_egress_location_handlers_test.go`, following the existing auth tests in that file. Cover: no secret → 401; wrong secret → 401; correct secret → 200 with a JSON body containing `client_ids`. The existing tests already show how `operatorIngestSecret` is stubbed — reuse that, and confirm your accept-case would fail against an always-401 handler (an earlier review caught exactly that gap in this file).

- [ ] **Step 7: Implement the handler and route**

Add `ProviderEgressLocationDue` to `api/handlers/provider_egress_location_handlers.go`, reusing `operatorSecretHeader`, the `hmac.Equal` comparison and the fail-closed `operatorIngestSecret()` already in that file. Parse `limit` from the query string, clamp it to a sane maximum (500) and default it (100). Register in `api/api.go` beside the existing route:

```go
		router.NewRoute("GET", "/network/provider-egress-due", handlers.ProviderEgressLocationDue),
```

- [ ] **Step 8: Verify and ship both branches**

```bash
./test.sh -run TestGetProviderEgressLocationDue
./test.sh -run TestProviderEgressLocation
go build ./... && go vet ./...
```

Then commit, push, open the beta PR, cherry-pick onto `feat/probe-due-endpoint-upstream` based on `upstream/main`, and open the upstream PR — same shape as Task 1 Step 7.

---

## Task 3: Prober confinement self-check and server-driven work

**Repo:** `/tmp/sandbox/urnetwork-operator-proxy`, branch `main`. Single repo, no cherry-pick.

**Context:** the prober must refuse to run unless its confinement is real. Because beta uses Docker and mainstream does not, the check cannot inspect namespaces or firewall rules — it must test the property directly: try to reach a geolocation API without a tunnel, and exit if that works.

**Files:**
- Create: `confinement/confinement.go`, `confinement/confinement_test.go`
- Modify: `cmd/egress-prober/main.go`

**Interfaces:**
- Consumes: `GET /network/provider-egress-due` from Task 2.
- Produces: `confinement.Verify(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), addrs []string, timeout time.Duration) error`

- [ ] **Step 1: Write the failing test**

Create `confinement/confinement_test.go`. The dialer is injected so the test never touches the real network:

```go
func TestVerifyFailsWhenDirectConnectionSucceeds(t *testing.T) {
	// a dialer that connects means nothing is stopping the prober reaching a
	// geolocation api directly -- the confinement is absent and the whole
	// guarantee is void, so Verify must refuse
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c1, _ := net.Pipe()
		return c1, nil
	}
	err := Verify(context.Background(), dial, []string{"34.117.59.81:443"}, time.Second)
	if err == nil {
		t.Fatal("Verify returned nil when a direct connection succeeded; it must refuse to run")
	}
}

func TestVerifyPassesWhenDirectConnectionRefused(t *testing.T) {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("connect: network is unreachable")
	}
	if err := Verify(context.Background(), dial, []string{"34.117.59.81:443"}, time.Second); err != nil {
		t.Fatalf("Verify errored when the connection was refused: %s", err)
	}
}

func TestVerifyRequiresAtLeastOneAddress(t *testing.T) {
	// an empty address list would vacuously "pass" and silently disable the
	// entire check
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("unreachable")
	}
	if err := Verify(context.Background(), dial, nil, time.Second); err == nil {
		t.Fatal("Verify accepted an empty address list; that would disable the check")
	}
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `cd /tmp/sandbox/urnetwork-operator-proxy && go test ./confinement/ -v`
Expected: build failure, `undefined: Verify`.

- [ ] **Step 3: Implement**

Create `confinement/confinement.go`:

```go
// Package confinement verifies at startup that this process cannot reach a
// geolocation api directly.
//
// The prober's entire guarantee is that every geolocation lookup egresses
// through a provider, so the api reports the provider's address and never the
// operator's. That is enforced outside this process -- a restricted network
// under docker compose, systemd IPAddressDeny/IPAddressAllow otherwise -- and
// the mechanism differs per deployment.
//
// Rather than inspect a mechanism it cannot portably know, the prober tests the
// property: it attempts a direct connection and refuses to run if one succeeds.
// Operator configuration therefore stops being an assumption and becomes a
// precondition.
package confinement

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// ErrNotConfined reports that a direct connection succeeded.
var ErrNotConfined = errors.New("confinement: a direct connection to a geolocation address succeeded; this process is not confined")

// ErrNoAddresses reports an empty address list, which would make the check
// vacuous.
var ErrNoAddresses = errors.New("confinement: at least one address is required")

// DialFunc matches net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Verify returns nil only when every address refuses a direct connection.
//
// A dial error is the expected, healthy outcome. A successful connection means
// the confinement is missing and returns ErrNotConfined. A timeout counts as
// refused: a dropped packet is what a deny rule looks like from inside.
func Verify(ctx context.Context, dial DialFunc, addrs []string, timeout time.Duration) error {
	if len(addrs) == 0 {
		return ErrNoAddresses
	}
	for _, addr := range addrs {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := dial(attemptCtx, "tcp", addr)
		cancel()
		if err == nil {
			if conn != nil {
				conn.Close()
			}
			return fmt.Errorf("%w: %s", ErrNotConfined, addr)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./confinement/ -v`
Expected: all three PASS.

- [ ] **Step 5: Wire the check into the prober**

In `cmd/egress-prober/main.go`, immediately after flag parsing and before any tunnel or API work, call `confinement.Verify` with a real `net.Dialer{}.DialContext`, the geolocation IPs, and a short timeout (3s). On error, log and `os.Exit(1)`.

Add a `--skip-confinement-check` flag defaulting to **false**, for the operator running the tool interactively as a one-shot diagnostic (which is how the manual probe is used). It must log loudly when set. Do not let it default true — a check that is off by default is not a check.

Source the addresses from the same host list `geolocate` already knows, resolved to IPs at startup; do not hardcode a second copy of the endpoint list that can drift.

- [ ] **Step 6: Replace the in-memory due cache with the server list**

Add a `--due-url` flag (default: `<api-url>/network/provider-egress-due`). When the operator secret is present, fetch the due client ids from Task 2's endpoint and probe exactly those, instead of enumerating everything and filtering by the in-memory TTL. Keep the existing enumeration path as the fallback when the endpoint returns 404, so the prober still works against a server that has not deployed Task 2.

- [ ] **Step 7: Verify**

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./... -race -count=1
```

Expected: all packages ok, `gofmt -l` silent.

- [ ] **Step 8: Commit and push**

```bash
git add confinement/ cmd/egress-prober/main.go
git commit -m "feat(confinement): refuse to run unless direct geolocation egress is blocked"
git push origin main
```

---

## Task 4: Deployment confinement for both environments

**Repo:** `/root/urnetwork/server`. Two branches: `feat/probe-deployment-confinement` and `-upstream`.

**Context:** the prober is only as confined as its deployment makes it. Beta gets a Compose service on a restricted network; mainstream gets a documented systemd unit. Task 3's self-check is what proves either one actually works.

**Files:**
- Modify: `docker-compose.beta.yml`
- Modify: `BETA.md`
- Create: `docs/operator/prober-systemd.md`

- [ ] **Step 1: Add the beta service**

Add an `egress-prober` service to `docker-compose.beta.yml` on a **dedicated internal network** that reaches `api` and `connect` only. Give it the operator secret and prober JWT via the vault mount, `restart: unless-stopped`, and no added capabilities.

The service must NOT be attached to the default network that reaches the public internet — that attachment is the confinement, and Task 3's self-check will fail the container at startup if it is wrong. That is the intended feedback loop: a misconfigured network stops the prober instead of silently leaking.

- [ ] **Step 2: Verify the confinement actually holds**

Bring up only the new service against the running beta stack, without recreating anything else:

```bash
cd /root/urnetwork/server
docker compose -f docker-compose.beta.yml up -d --no-deps egress-prober
docker compose -f docker-compose.beta.yml logs --tail=30 egress-prober
```

Expected: the service starts and the self-check passes. Then prove the check has teeth by attaching it to the default network temporarily and confirming it **exits non-zero** with `ErrNotConfined`. Report both outcomes.

- [ ] **Step 3: Document the non-Docker deployment**

Create `docs/operator/prober-systemd.md` with a complete unit file using `IPAddressDeny=any` plus `IPAddressAllow=` entries for the platform api/connect addresses and localhost, `DynamicUser=yes`, `NoNewPrivileges=yes`, and no capabilities. State explicitly that `IPAddressAllow` takes addresses, not hostnames, so the operator must list the platform's addresses and update them if they change — and that Task 3's self-check is what catches it if they forget.

- [ ] **Step 4: Update BETA.md**

Document the new service, the secret it needs, and how to confirm the self-check passed.

- [ ] **Step 5: Ship both branches**

Commit, push, open the beta PR, cherry-pick onto `-upstream` based on `upstream/main`, open the upstream PR. Note that `docker-compose.beta.yml` is beta-only; if it does not exist upstream, carry only `docs/operator/prober-systemd.md` in the upstream cherry-pick and say so in the PR body.

---

## Verification Summary

| Task | Gate |
| --- | --- |
| 1 | New test proves a below-minimums location is hidden with `false` and visible with `true`; `GetProviderLocations` behaviour unchanged |
| 2 | Model test covers fresh / stale / never-probed; handler tests cover 401, 401, 200 and would fail against an always-401 handler |
| 3 | Three `confinement` tests including the empty-address case; full race suite green |
| 4 | Beta service starts confined; attaching it to the default network makes it exit non-zero |
| All | Both PRs open per server-side change |

## Out of Scope

- Identity rotation and multi-hop probing (P3).
- Verdict logic, RTT corroboration, the schema extension (P2).
- Bandwidth measurement and its byte budget (P2/P3).
- Letting measured bandwidth satisfy `PassesMinimums`, and whether providers should be penalised for tests their transport version cannot run.
