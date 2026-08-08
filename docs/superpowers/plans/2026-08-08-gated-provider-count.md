# Gated Provider Count Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `/network/provider-locations`'s `provider_count` report only providers a probe has measured healthy **and** observed egressing from the country they claim.

**Architecture:** `UpdateClientLocations` currently counts every connected provider with a Public provide key, with no health or location check. Add one shared predicate — `providerCountsTowardLocation` — used by both `UpdateClientLocations` and `UpdateClientScores`, so the advertised count and the gated membership cannot drift apart. Both jobs load their inputs with one bulk query per pass, never per provider.

**Tech Stack:** Go 1.26, PostgreSQL 16, Redis 7, `docker compose -f docker-compose.beta.yml`.

## Global Constraints

- Repo root for all paths: `/root/urnetwork/server`.
- **Never run `git checkout` in `/root/urnetwork/server`.** It deletes the untracked vault secrets in `beta-vault/vault/` and takes the live beta down. Use `git worktree`, `git show <ref>:<path>`, or `git diff` instead.
- Health threshold is integer arithmetic: `minEgressHealthOKDenominator*OKCount >= minEgressHealthOKNumerator*Total`. Never reintroduce a float ratio; the 90% boundary must not drift.
- Fail closed: no health record, or `Total <= 0`, or no observed location → the provider does **not** count.
- Do not add any field to a gob-serialised struct (`ClientScore`, `ClientLocation`, `ClientFilter`). Stale cache entries decode new fields as the zero value; see the `NetworkOnly` comment at `model/network_client_location_model.go:2413`.
- Bulk-load per pass. A per-provider query inside the counting loop is a ~2400x amplification on the live box.
- Go builds run in a container; there is no host toolchain:
  `docker run --rm -v /root/urnetwork:/src -w /src/server golang:1.26 go build ./model/...`
- Nothing in this plan deploys. Deployment is a separate, explicit step after review.

---

### Task 1: Bulk loader for probe-observed country codes

**Files:**
- Modify: `model/provider_egress_location_model.go` (add after `GetProviderEgressLocation`, which ends near `:283`)
- Test: `model/provider_egress_location_model_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func GetAllProviderEgressCountryCodes(ctx context.Context) map[server.Id]string` — client id → lowercase ISO country code the probe observed. Clients with no observed location are **absent from the map** (not present with `""`). Task 2 and Task 3 depend on this exact name and signature.

- [ ] **Step 1: Write the failing test**

Add to `model/provider_egress_location_model_test.go`:

```go
func TestGetAllProviderEgressCountryCodes(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(func() {
		ctx := context.Background()

		observed := server.NewId()
		unobserved := server.NewId()

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    observed,
			CountryCode: "GB",
			Verdict:     "verified",
			ObservedAt:  server.NowUtc(),
		})

		codes := GetAllProviderEgressCountryCodes(ctx)

		// observed providers come back lowercased, so callers can compare
		// against location.country_code without normalising at each site
		assert.Equal(t, codes[observed], "gb")

		// a provider with no observed location is ABSENT, not "".
		// Callers rely on the two-value lookup to fail closed.
		_, ok := codes[unobserved]
		assert.Equal(t, ok, false)
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
docker run --rm -v /root/urnetwork:/src -w /src/server golang:1.26 \
  go vet ./model/ 2>&1 | grep -i 'GetAllProviderEgressCountryCodes'
```
Expected: FAIL — `undefined: GetAllProviderEgressCountryCodes`.

- [ ] **Step 3: Write minimal implementation**

Add to `model/provider_egress_location_model.go`:

```go
// GetAllProviderEgressCountryCodes reads every provider's latest observed
// egress country in one query, for callers that need to check many providers
// in a single pass. Mirrors GetAllProviderEgressHealthCounts: the counting
// loop in UpdateClientLocations runs over the whole provider population, so a
// per-provider query there would be one round trip per provider.
//
// Codes are lowercased so callers can compare directly against
// location.country_code without normalising at each site.
//
// A provider with no observed location is ABSENT from the map rather than
// present with an empty string. Callers use the two-value lookup and treat
// absence as "not verified", which fails closed.
func GetAllProviderEgressCountryCodes(ctx context.Context) map[server.Id]string {
	countryCodes := map[server.Id]string{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT client_id, country_code
			FROM provider_egress_location
			WHERE country_code IS NOT NULL AND country_code != ''
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var countryCode string
				server.Raise(result.Scan(&clientId, &countryCode))
				countryCodes[clientId] = strings.ToLower(countryCode)
			}
		})
	})

	return countryCodes
}
```

If `strings` is not already imported in this file, add it to the import block.

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
docker run --rm -v /root/urnetwork:/src -w /src/server golang:1.26 \
  go build ./model/... && echo BUILD_OK
```
Expected: `BUILD_OK`.

- [ ] **Step 5: Commit**

```bash
cd /root/urnetwork/server
git add model/provider_egress_location_model.go model/provider_egress_location_model_test.go
git commit -m "feat(model): bulk loader for probe-observed egress country codes

Mirrors GetAllProviderEgressHealthCounts. The provider-count loop runs over
the whole provider population, so this has to be one query, not one per
provider. Absent means unobserved, so the two-value lookup fails closed."
```

---

### Task 2: Extract the shared predicate

**Files:**
- Modify: `model/network_client_location_model.go` (the `passesEgressHealth` closure sits inside `UpdateClientScores` near `:2962`)
- Test: `model/network_client_location_model_test.go`

**Interfaces:**
- Consumes: `GetAllProviderEgressCountryCodes` from Task 1.
- Produces:
  - `type providerCountFilter struct { healthCounts map[server.Id]ProviderEgressHealthCounts; countryCodes map[server.Id]string }`
  - `func newProviderCountFilter(ctx context.Context) providerCountFilter` — performs both bulk loads.
  - `func (f providerCountFilter) passesHealth(clientId server.Id) bool`
  - `func (f providerCountFilter) countsTowardCountry(clientId server.Id, countryCode string) bool` — true when health passes **and** the observed country equals `countryCode` (both compared lowercase).

  Task 3 calls `newProviderCountFilter` and `countsTowardCountry`. `UpdateClientScores` keeps using `passesHealth` only — it must not gain the country check, because its candidate pool is not country-scoped.

- [ ] **Step 1: Write the failing test**

Add to `model/network_client_location_model_test.go`:

```go
func TestProviderCountFilter(t *testing.T) {
	healthy := server.NewId()
	degraded := server.NewId()
	unmeasured := server.NewId()
	zeroTotal := server.NewId()
	wrongCountry := server.NewId()
	unobserved := server.NewId()

	f := providerCountFilter{
		healthCounts: map[server.Id]ProviderEgressHealthCounts{
			// 118/131 is the first passing value: 10*118 >= 9*131 (1180 >= 1179)
			healthy: {OKCount: 118, Total: 131},
			// 117/131 is the last failing value: 1170 < 1179
			degraded:     {OKCount: 117, Total: 131},
			zeroTotal:    {OKCount: 0, Total: 0},
			wrongCountry: {OKCount: 131, Total: 131},
			unobserved:   {OKCount: 131, Total: 131},
		},
		countryCodes: map[server.Id]string{
			healthy:      "us",
			degraded:     "us",
			zeroTotal:    "us",
			wrongCountry: "gb",
			// unobserved deliberately absent
		},
	}

	// the 90% boundary, asserted on the integer comparison
	assert.Equal(t, f.passesHealth(healthy), true)
	assert.Equal(t, f.passesHealth(degraded), false)

	// fail closed: never probed, and probed-with-no-destinations
	assert.Equal(t, f.passesHealth(unmeasured), false)
	assert.Equal(t, f.passesHealth(zeroTotal), false)

	// counts only where health passes AND the observed country matches
	assert.Equal(t, f.countsTowardCountry(healthy, "us"), true)
	assert.Equal(t, f.countsTowardCountry(degraded, "us"), false)

	// healthy but egressing from somewhere else: not counted in the claim
	assert.Equal(t, f.countsTowardCountry(wrongCountry, "us"), false)
	// ...and it does count where it actually is
	assert.Equal(t, f.countsTowardCountry(wrongCountry, "gb"), true)

	// healthy but never located: fail closed
	assert.Equal(t, f.countsTowardCountry(unobserved, "us"), false)

	// comparison is case insensitive on the caller's side
	assert.Equal(t, f.countsTowardCountry(healthy, "US"), true)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
docker run --rm -v /root/urnetwork:/src -w /src/server golang:1.26 \
  go vet ./model/ 2>&1 | grep -i 'providerCountFilter'
```
Expected: FAIL — `undefined: providerCountFilter`.

- [ ] **Step 3: Write minimal implementation**

First move the two threshold constants to package scope. They are currently
declared **inside** `UpdateClientScores` at `:2897-2898`, so a package-scope
method cannot see them and the code below will not compile until they move.

Delete these two lines from inside `UpdateClientScores`:

```go
	const minEgressHealthOKNumerator = 9
	const minEgressHealthOKDenominator = 10
```

and re-declare them at package scope, immediately above `func UpdateClientLocations`,
keeping the explanatory comment that sits above them today:

```go
// The share of destinations a provider must reach to count as healthy: 90%,
// as 9/10. Compared exactly as `10*ok >= 9*total` rather than through a float
// division, so the boundary is the same for every denominator.
//
// Package scope, not function scope: both UpdateClientScores and
// UpdateClientLocations gate on this now, and two copies could drift.
const minEgressHealthOKNumerator = 9
const minEgressHealthOKDenominator = 10
```

Then add, immediately below them:

```go
// providerCountFilter answers one question: does this provider count as real,
// reachable supply?
//
// It exists so the advertised provider_count (UpdateClientLocations) and the
// gated membership (UpdateClientScores) apply an IDENTICAL predicate. They ran
// different rules before: membership was gated on egress health while the count
// was not, so a location could survive the gate and still advertise providers
// that no probe had ever reached.
//
// Both maps are loaded once per pass. These loops run over the entire provider
// population, so a per-provider query here is one round trip per provider.
type providerCountFilter struct {
	healthCounts map[server.Id]ProviderEgressHealthCounts
	countryCodes map[server.Id]string
}

func newProviderCountFilter(ctx context.Context) providerCountFilter {
	return providerCountFilter{
		healthCounts: GetAllProviderEgressHealthCounts(ctx),
		countryCodes: GetAllProviderEgressCountryCodes(ctx),
	}
}

// passesHealth reports whether a probe has MEASURED this provider healthy.
// Fail closed: no record at all (never probed) does not pass, and neither does
// a record with no destinations in it, which is not a measurement of anything.
// Guarding total also keeps the ratio well defined.
//
// Compared exactly as 10*ok >= 9*total rather than through a float, so the 90%
// boundary cannot drift with rounding.
func (f providerCountFilter) passesHealth(clientId server.Id) bool {
	counts, ok := f.healthCounts[clientId]
	if !ok {
		return false
	}
	if counts.Total <= 0 {
		return false
	}
	return minEgressHealthOKDenominator*counts.OKCount >= minEgressHealthOKNumerator*counts.Total
}

// countsTowardCountry reports whether this provider counts as supply for
// countryCode. It must both be measured healthy and have been OBSERVED
// egressing from that country.
//
// The two locations are different claims. network_client_location is where the
// provider says it is, derived from its own connection. provider_egress_location
// is where a probe actually watched its traffic leave. Counting on the claim
// alone advertises providers in countries they do not egress from -- measured
// on beta at 3 of 152 healthy providers claiming `at` while egressing from `gb`
// -- which is what an adversarial provider would exploit at scale.
//
// A provider with no observed location is not counted, matching the health rule.
func (f providerCountFilter) countsTowardCountry(clientId server.Id, countryCode string) bool {
	if !f.passesHealth(clientId) {
		return false
	}
	observed, ok := f.countryCodes[clientId]
	if !ok {
		return false
	}
	return observed == strings.ToLower(countryCode)
}
```

If `strings` is not already imported in this file, add it to the import block.

Then replace the `passesEgressHealth` closure inside `UpdateClientScores` so both jobs share one definition. Delete the `egressHealthCounts := GetAllProviderEgressHealthCounts(ctx)` line and the `passesEgressHealth := func(...)` closure, and put in their place:

```go
	// Shared with UpdateClientLocations so the gated membership and the
	// advertised count can never disagree about what "healthy" means.
	// UpdateClientScores uses passesHealth ONLY: its candidate pool is not
	// country-scoped, so the observed-country check does not apply here.
	countFilter := newProviderCountFilter(ctx)
```

and change the single call site from `passesEgressHealth(clientScore.ClientId)` to `countFilter.passesHealth(clientScore.ClientId)`.

Leave the long comment block above the old closure in place — it documents the stale-record asymmetry and the ungated due-queue, both still true.

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
docker run --rm -v /root/urnetwork:/src -w /src/server golang:1.26 \
  go build ./model/... && echo BUILD_OK
```
Expected: `BUILD_OK`.

Then confirm the old closure is gone and exactly one definition of each
threshold constant remains:
```bash
cd /root/urnetwork/server
grep -c 'passesEgressHealth' model/network_client_location_model.go
grep -c 'minEgressHealthOKNumerator = 9' model/network_client_location_model.go
```
Expected: `0`, then `1`. A `2` on the second means the constants were copied
rather than moved, which is the drift this refactor exists to prevent.

- [ ] **Step 5: Commit**

```bash
cd /root/urnetwork/server
git add model/network_client_location_model.go model/network_client_location_model_test.go
git commit -m "refactor(model): one shared predicate for provider supply

The gated membership and the advertised provider_count applied different
rules: membership was gated on egress health, the count was not. Extract the
health check into providerCountFilter, shared by both jobs, and add the
observed-country check the count needs. UpdateClientScores keeps using
passesHealth alone because its pool is not country scoped."
```

---

### Task 3: Gate the count in UpdateClientLocations

**Files:**
- Modify: `model/network_client_location_model.go:1645-1737` (the query and its scan loop inside `UpdateClientLocations`)
- Test: `model/network_client_location_model_test.go`

**Interfaces:**
- Consumes: `newProviderCountFilter` and `countsTowardCountry` from Task 2.
- Produces: no new symbols. `ClientLocation.ClientCount` changes meaning: it now counts only verified-healthy providers.

- [ ] **Step 1: Write the failing test**

Add to `model/network_client_location_model_test.go`:

```go
func TestUpdateClientLocationsCountIsGated(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		countryId := server.NewId()

		healthy := server.NewId()
		unhealthy := server.NewId()
		unprobed := server.NewId()

		// three providers in the same country, all connected with a Public
		// provide key; only `healthy` is measured healthy and observed in US
		for _, clientId := range []server.Id{healthy, unhealthy, unprobed} {
			Testing_CreateProviderAtLocation(ctx, networkId, clientId, countryId, "US")
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: healthy, OKCount: 131, Total: 131, MeasuredAt: server.NowUtc(),
		})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: unhealthy, OKCount: 0, Total: 131, MeasuredAt: server.NowUtc(),
		})
		for _, clientId := range []server.Id{healthy, unhealthy} {
			SetProviderEgressLocation(ctx, &ProviderEgressLocation{
				ClientId: clientId, CountryCode: "US",
				Verdict: "verified", ObservedAt: server.NowUtc(),
			})
		}

		UpdateClientLocations(ctx, 1*time.Hour)

		clientLocations, err := loadClientLocations(ctx, map[server.Id]bool{countryId: true})
		assert.Equal(t, err, nil)

		// only the measured-healthy, observed-in-US provider is counted.
		// Before this change all three counted.
		assert.Equal(t, clientLocations[countryId].ClientCount, 1)
	})
}
```

If `Testing_CreateProviderAtLocation` does not exist, create it in `model/network_client_location_model_test.go` as a helper that inserts the `network_client`, `provide_key` (`ProvideModePublic`), and `network_client_location_reliability` rows a provider needs to be counted, with `connected = true` and `valid = true` and all three location columns set to `countryId`.

- [ ] **Step 2: Run test to verify it fails**

Run:
```bash
docker run --rm -v /root/urnetwork:/src -w /src/server golang:1.26 \
  go build ./model/... && echo BUILD_OK
```
Expected: `BUILD_OK` (the test compiles but asserts 3 != 1 when run against a live DB).

- [ ] **Step 3: Write minimal implementation**

In the query at `:1645`, add the two columns the filter needs. Change the `SELECT` list from:

```sql
	        SELECT
	        	network_client_location_reliability.city_location_id,
	        	network_client_location_reliability.region_location_id,
	        	network_client_location_reliability.country_location_id
```

to:

```sql
	        SELECT
	        	network_client_location_reliability.client_id,
	        	network_client_location_reliability.city_location_id,
	        	network_client_location_reliability.region_location_id,
	        	network_client_location_reliability.country_location_id,
	        	-- the country the provider CLAIMS, to check against the country a
	        	-- probe observed it egressing from
	        	country_location.country_code
```

and add this join immediately after the existing `LEFT JOIN client_connection_reliability_score ...` block:

```sql
	        LEFT JOIN location AS country_location ON
	        	country_location.location_id = network_client_location_reliability.country_location_id
```

Immediately before `server.Tx(ctx, func(tx server.PgTx) {`, add the bulk load:

```go
	// one bulk load per pass, outside the tx: this loop runs over the whole
	// provider population
	countFilter := newProviderCountFilter(ctx)
```

Then change the scan loop to read the new columns and apply the filter:

```go
			for result.Next() {
				var clientId server.Id
				var cityLocationId server.Id
				var regionLocationId server.Id
				var countryLocationId server.Id
				var countryCode *string
				server.Raise(result.Scan(
					&clientId,
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
					&countryCode,
				))

				// This is the number every app shows when a user picks a
				// location, so count only providers a probe has MEASURED
				// healthy and OBSERVED egressing from the country they claim.
				// Counting on the claim alone advertised providers that were
				// either unreachable or in a different country entirely.
				//
				// countryCode is NULL when the claimed country has no location
				// row, which cannot be verified against anything -- fail closed,
				// same as an unobserved provider.
				if countryCode == nil {
					continue
				}
				if !countFilter.countsTowardCountry(clientId, *countryCode) {
					continue
				}

				// count each client at most once per distinct location id. A
				// client whose geo lookup resolved neither a city nor a region
				// is stored with city = region = country (see
				// SetConnectionLocation's country fallback), so incrementing
				// all three unconditionally counted that one client three
				// times in its own country. Distinct ids -- a real
				// city-granular client -- still roll up into their region and
				// country exactly as before.
				//
				// This is a live fix on beta, where that fallback exists, and a
				// forward guard against upstream/main, where it does not yet --
				// see distinctIds.
				for _, locationId := range distinctIds(
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
				) {
					locationClientCounts[locationId] += 1
				}
			}
```

- [ ] **Step 4: Run test to verify it passes**

Run:
```bash
docker run --rm -v /root/urnetwork:/src -w /src/server golang:1.26 \
  go build ./model/... && echo BUILD_OK
```
Expected: `BUILD_OK`.

- [ ] **Step 5: Commit**

```bash
cd /root/urnetwork/server
git add model/network_client_location_model.go model/network_client_location_model_test.go
git commit -m "fix(model): count only verified-healthy providers per location

provider_count counted every connected provider with a Public provide key,
with no health or location check, so a location that survived the gate still
over-reported. Beta advertised 305 for the US against 151 health-passing
providers fleet-wide, 3 of which claimed a country they did not egress from.

Apply the shared providerCountFilter at the increment: a provider counts only
where a probe measured it healthy and observed it egressing from the country
it claims. NULL claimed country fails closed."
```

---

### Task 4: Regression-test the direct client-ID bypass

**Files:**
- Test: `model/network_client_location_model_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: nothing. This task only pins existing behaviour so a future change to the gate cannot silently break it.

- [ ] **Step 1: Write the test**

`spec.ClientId` (`model/network_client_location_model.go:3351`) appends a provider straight to the result without consulting the score sets. That is what keeps an unhealthy provider reachable by ID, and nothing currently tests it.

```go
func TestFindProviders2ClientIdBypassesHealthGate(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		countryId := server.NewId()
		unhealthy := server.NewId()

		Testing_CreateProviderAtLocation(ctx, networkId, unhealthy, countryId, "US")
		// measured comprehensively dead, so the gate excludes it everywhere else
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: unhealthy, OKCount: 0, Total: 131, MeasuredAt: server.NowUtc(),
		})

		session := session.Testing_CreateClientSession(ctx, jwt.NewByJwt(networkId, server.NewId(), "test", false))

		result, err := FindProviders2(&FindProviders2Args{
			Specs: []*ProviderSpec{{ClientId: &unhealthy}},
			Count: 1,
		}, session)
		assert.Equal(t, err, nil)

		// an explicit client id is a deliberate choice by the caller, so the
		// public-list health gate must not apply to it
		assert.Equal(t, len(result.Providers), 1)
		assert.Equal(t, result.Providers[0].ClientId, unhealthy)
	})
}
```

Adjust the `session`/`jwt` construction to match the helpers the surrounding tests in this file already use.

- [ ] **Step 2: Run it and confirm it passes now**

Run:
```bash
docker run --rm -v /root/urnetwork:/src -w /src/server golang:1.26 \
  go build ./model/... && echo BUILD_OK
```
Expected: `BUILD_OK`. This test asserts current behaviour, so it must pass without any production change. If it fails, stop — the bypass is not behaving as the spec claims and that must be resolved before the gate ships.

- [ ] **Step 3: Commit**

```bash
cd /root/urnetwork/server
git add model/network_client_location_model_test.go
git commit -m "test(model): pin the direct client-id bypass of the health gate

spec.ClientId appends a provider without consulting the score sets, which is
what keeps a gated provider reachable by id. Nothing tested it, so a future
change to the gate could remove that path silently."
```

---

### Task 5: Live verification on beta

**Files:** none. This task changes nothing; it proves the deployed behaviour.

**Interfaces:**
- Consumes: Tasks 1-4, built and deployed.
- Produces: a pass/fail judgement on whether the count matches the database.

- [ ] **Step 1: Build and deploy**

```bash
cd /root/urnetwork/server
docker compose -f docker-compose.beta.yml build taskworker api
docker compose -f docker-compose.beta.yml up -d taskworker api
```

- [ ] **Step 2: Force a fresh pass**

The counts are cached with a TTL, so the endpoint keeps serving the old ungated numbers until both jobs re-run. Release them rather than waiting:

```bash
cd /root/urnetwork/server
for fn in UpdateClientLocations UpdateClientScores; do
  TID=$(docker exec server-postgres-1 psql -U postgres -tAc \
    "SELECT task_id FROM pending_task WHERE function_name LIKE '%${fn}%';" | tr -d ' ')
  docker exec server-taskworker-1 /app/bringyourctl task release "$TID"
done
```

- [ ] **Step 3: Compare the advertised count against the database**

```bash
cd /root/urnetwork/server
curl -s --max-time 20 --resolve api.beta-test.net:443:74.50.11.17 \
  https://api.beta-test.net/network/provider-locations \
  | python3 -c "
import json,sys
d=json.load(sys.stdin)
for x in d.get('locations') or []:
    print(f\"{x.get('country_code')} advertised={x.get('provider_count')}\")"

docker exec server-postgres-1 psql -U postgres -c "
SELECT lower(lc.country_code) AS cc, count(DISTINCT r.client_id) AS verified_healthy
FROM network_client_location_reliability r
JOIN provider_egress_health h ON h.client_id = r.client_id
JOIN network_client_location ncl ON ncl.client_id = r.client_id
JOIN location lc ON lc.location_id = ncl.country_location_id
JOIN provider_egress_location pel ON pel.client_id = r.client_id
WHERE r.connected AND r.valid
  AND 10*h.ok_count >= 9*h.total_count
  AND lower(pel.country_code) = lower(lc.country_code)
  AND EXISTS (SELECT 1 FROM provide_key pk
              WHERE pk.client_id = r.client_id AND pk.provide_mode = 3)
GROUP BY 1 ORDER BY 2 DESC;"
```

Expected: for every country the endpoint returns, `advertised` equals `verified_healthy`. Report any row where they differ; do not explain a mismatch away.

- [ ] **Step 4: Confirm the bypass still works live**

```bash
cd /root/urnetwork/server
DEAD=$(docker exec server-postgres-1 psql -U postgres -tAc \
  "SELECT client_id FROM provider_egress_health WHERE ok_count = 0 LIMIT 1;" | tr -d ' ')
echo "gated provider: $DEAD"
```

Confirm this id is absent from the public list yet still resolvable via a `client_id` spec through `FindProviders2`.

- [ ] **Step 5: Record the result**

Append the measured before/after numbers to
`docs/superpowers/specs/2026-08-08-gated-provider-locations-design.md` under a
`## Result` heading, and commit. State what was verified and what was not.
