# Provider Egress Location — Server Ingest & Integration (Sub-project B) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator's egress prober submit cross-checked provider locations, store them, and prefer them over the mmdb lookup when a provider connects.

**Architecture:** A new `provider_egress_location` table keyed by `client_id`; a model layer that upserts and reads it (resolving the reported country to a canonical `location` row via the existing `CreateLocation` path); an operator-authenticated ingest endpoint guarded by a vault shared secret; and a change in `controller.SetConnectionLocation` to prefer a fresh stored egress location, falling back to the existing mmdb lookup on miss/stale.

**Tech Stack:** Go 1.26.3, Postgres, the existing `server`/`model`/`controller`/`router`/`api` packages and their conventions (`server.Tx`, `server.Db`, `server.RaisePgResult`, `router.NewRoute`, vault `RequireSimpleResource`).

**Repo/branch:** server repo, branch `feat/provider-egress-geolocation` (off `beta/self-contained-env`). Cherry-pick to the upstream-facing branch as a **NEW** upstream PR, then dispatch Opus review agents (standing workflow).

**Source spec:** `docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md`, section "Sub-project B".

## Global Constraints

- Follow existing model conventions: `server.Tx` / `server.Db` with `server.RaisePgResult` / `server.WithPgResult` / `server.Raise`; ids are `server.Id`.
- Migrations are **append-only** at the tail of the `migrations` slice in `db_migrations.go`. Never edit or reorder an existing migration.
- Timestamps: naive `timestamp` columns holding UTC via `server.NowUtc()`. **Never** compare a naive timestamp against SQL `now()` — compute cutoffs in Go and bind them (this repo has a fixed bug from exactly that; see `model/account_action_rate_limit.go`).
- Country codes are stored/compared lowercased (matches `CreateLocation`, which lowercases `CountryCode`).
- `ProviderEgressLocationMaxAge = 7 * 24 * time.Hour` — a stored location older than this is stale and ignored (falls back to mmdb).
- The ingest endpoint is operator-only, authenticated by a vault shared secret at `provider_egress.yml` key `ingest_secret`, compared with `hmac.Equal` (constant time). It is NOT a normal client-JWT route.
- Only a **country-confident** submission is accepted; city/region are stored only when the submission is also city-confident. A non-country-confident submission is rejected with a clear error (the prober is expected not to send these).
- Beta-only concerns do not apply here — this is upstream-intended behavior. Use ordinary commit messages (no `fix(beta)` prefix).

---

### Task 1: Migration and model layer

**Files:**
- Modify: `db_migrations.go` (append at the tail of the `migrations` slice, immediately before the closing `}`)
- Create: `model/provider_egress_location_model.go`
- Test: `model/provider_egress_location_model_test.go`

**Interfaces:**
- Consumes: existing `CreateLocation(ctx, *Location)`, `Location`, `LocationTypeCountry`, `server.Id`, `server.NowUtc()`.
- Produces:
  - `type ProviderEgressLocation struct { ClientId server.Id; LocationId server.Id; CountryCode string; ASN int; Org string; Hosting bool; Proxy bool; Mobile bool; CityConfident bool; ObservedAt time.Time; UpdateTime time.Time }`
  - `const ProviderEgressLocationMaxAge = 7 * 24 * time.Hour`
  - `func SetProviderEgressLocation(ctx context.Context, e *ProviderEgressLocation)` — upsert by `client_id`
  - `func GetProviderEgressLocation(ctx context.Context, clientId server.Id) *ProviderEgressLocation` — nil if absent
  - `func GetFreshProviderEgressLocation(ctx context.Context, clientId server.Id, maxAge time.Duration) *ProviderEgressLocation` — nil if absent or `observed_at` older than `maxAge`
  - `func RemoveExpiredProviderEgressLocations(ctx context.Context, minObservedAt time.Time)`

- [ ] **Step 1: Write the failing model tests**

Create `model/provider_egress_location_model_test.go`:
```go
package model

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

func TestProviderEgressLocationUpsertAndGet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		clientId := server.NewId()
		now := server.NowUtc()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  country.LocationId,
			CountryCode: "us",
			ASN:         401486,
			Org:         "RAVNIX LLC",
			Hosting:     true,
			ObservedAt:  now,
		})

		got := GetProviderEgressLocation(ctx, clientId)
		if got == nil {
			t.Fatal("expected a stored egress location")
		}
		assert.Equal(t, got.LocationId, country.LocationId)
		assert.Equal(t, got.CountryCode, "us")
		assert.Equal(t, got.ASN, 401486)
		assert.Equal(t, got.Hosting, true)
		assert.Equal(t, got.Proxy, false)

		// upsert replaces
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  country.LocationId,
			CountryCode: "us",
			ASN:         999,
			Hosting:     false,
			Proxy:       true,
			ObservedAt:  now,
		})
		got = GetProviderEgressLocation(ctx, clientId)
		assert.Equal(t, got.ASN, 999)
		assert.Equal(t, got.Hosting, false)
		assert.Equal(t, got.Proxy, true)
	})
}

func TestProviderEgressLocationFreshness(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		fresh := server.NewId()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: fresh, LocationId: country.LocationId, CountryCode: "us",
			ObservedAt: server.NowUtc(),
		})
		stale := server.NewId()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: stale, LocationId: country.LocationId, CountryCode: "us",
			ObservedAt: server.NowUtc().Add(-8 * 24 * time.Hour),
		})

		if GetFreshProviderEgressLocation(ctx, fresh, ProviderEgressLocationMaxAge) == nil {
			t.Fatal("fresh entry must be returned")
		}
		if GetFreshProviderEgressLocation(ctx, stale, ProviderEgressLocationMaxAge) != nil {
			t.Fatal("stale entry must not be returned")
		}
		// absent
		if GetFreshProviderEgressLocation(ctx, server.NewId(), ProviderEgressLocationMaxAge) != nil {
			t.Fatal("absent entry must return nil")
		}
	})
}

func TestRemoveExpiredProviderEgressLocations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		keep := server.NewId()
		drop := server.NewId()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: keep, LocationId: country.LocationId, CountryCode: "us",
			ObservedAt: server.NowUtc(),
		})
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId: drop, LocationId: country.LocationId, CountryCode: "us",
			ObservedAt: server.NowUtc().Add(-30 * 24 * time.Hour),
		})

		RemoveExpiredProviderEgressLocations(ctx, server.NowUtc().Add(-14*24*time.Hour))

		if GetProviderEgressLocation(ctx, keep) == nil {
			t.Fatal("recent entry must survive the sweep")
		}
		if GetProviderEgressLocation(ctx, drop) != nil {
			t.Fatal("old entry must be swept")
		}
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:
```bash
WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 \
BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
go test ./model/ -run TestProviderEgressLocation -v
```
Expected: FAIL — compile error, `ProviderEgressLocation` / `SetProviderEgressLocation` undefined.

- [ ] **Step 3: Append the migration**

In `db_migrations.go`, append at the very tail of the `migrations` slice (immediately before the closing `}` of the slice literal):
```go
	// provider egress locations: locations learned by probing a provider's own
	// egress (see docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md).
	// Keyed by client_id, one row per provider, upserted by the operator's
	// prober. location_id is the canonical country (or city, when the probe was
	// city-confident) location row. observed_at is when the probe ran, and is
	// what freshness is judged against.
	newSqlMigration(`
        CREATE TABLE IF NOT EXISTS provider_egress_location (
            client_id      uuid NOT NULL PRIMARY KEY,
            location_id    uuid NOT NULL,
            country_code   varchar(2) NOT NULL,
            asn            int NOT NULL DEFAULT 0,
            org            varchar(256) NOT NULL DEFAULT '',
            hosting        bool NOT NULL DEFAULT false,
            proxy          bool NOT NULL DEFAULT false,
            mobile         bool NOT NULL DEFAULT false,
            city_confident bool NOT NULL DEFAULT false,
            observed_at    timestamp NOT NULL,
            update_time    timestamp NOT NULL
        )
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS provider_egress_location_observed_at
            ON provider_egress_location (observed_at)
    `),
```

- [ ] **Step 4: Write the model**

Create `model/provider_egress_location_model.go`:
```go
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// ProviderEgressLocationMaxAge bounds how long a probed egress location is
// trusted. Past this, the location is ignored and the caller falls back to the
// mmdb lookup on the observed control ip.
const ProviderEgressLocationMaxAge = 7 * 24 * time.Hour

// ProviderEgressLocation is a provider location learned by probing the
// provider's own egress, rather than by looking up its control-connection ip.
type ProviderEgressLocation struct {
	ClientId      server.Id
	LocationId    server.Id
	CountryCode   string
	ASN           int
	Org           string
	Hosting       bool
	Proxy         bool
	Mobile        bool
	CityConfident bool
	ObservedAt    time.Time
	UpdateTime    time.Time
}

// SetProviderEgressLocation upserts the probed location for a provider.
func SetProviderEgressLocation(ctx context.Context, e *ProviderEgressLocation) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_egress_location (
				client_id,
				location_id,
				country_code,
				asn,
				org,
				hosting,
				proxy,
				mobile,
				city_confident,
				observed_at,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (client_id) DO UPDATE
			SET
				location_id = $2,
				country_code = $3,
				asn = $4,
				org = $5,
				hosting = $6,
				proxy = $7,
				mobile = $8,
				city_confident = $9,
				observed_at = $10,
				update_time = $11
			`,
			e.ClientId,
			e.LocationId,
			e.CountryCode,
			e.ASN,
			e.Org,
			e.Hosting,
			e.Proxy,
			e.Mobile,
			e.CityConfident,
			e.ObservedAt.UTC(),
			server.NowUtc(),
		))
	})
}

// GetProviderEgressLocation returns the stored location for a provider, or nil.
func GetProviderEgressLocation(ctx context.Context, clientId server.Id) *ProviderEgressLocation {
	var e *ProviderEgressLocation
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				location_id,
				country_code,
				asn,
				org,
				hosting,
				proxy,
				mobile,
				city_confident,
				observed_at,
				update_time
			FROM provider_egress_location
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				e = &ProviderEgressLocation{}
				server.Raise(result.Scan(
					&e.ClientId,
					&e.LocationId,
					&e.CountryCode,
					&e.ASN,
					&e.Org,
					&e.Hosting,
					&e.Proxy,
					&e.Mobile,
					&e.CityConfident,
					&e.ObservedAt,
					&e.UpdateTime,
				))
			}
		})
	})
	return e
}

// GetFreshProviderEgressLocation is GetProviderEgressLocation, filtered to
// entries probed within maxAge. The cutoff is computed in Go and bound as a
// parameter: observed_at is a naive timestamp holding utc, and comparing it
// against sql now() would cast through the session timezone.
func GetFreshProviderEgressLocation(
	ctx context.Context,
	clientId server.Id,
	maxAge time.Duration,
) *ProviderEgressLocation {
	e := GetProviderEgressLocation(ctx, clientId)
	if e == nil {
		return nil
	}
	if e.ObservedAt.Before(server.NowUtc().Add(-maxAge)) {
		return nil
	}
	return e
}

// RemoveExpiredProviderEgressLocations drops entries probed before
// minObservedAt.
func RemoveExpiredProviderEgressLocations(ctx context.Context, minObservedAt time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM provider_egress_location WHERE observed_at < $1`,
			minObservedAt.UTC(),
		))
	})
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run:
```bash
WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 \
BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
go test ./model/ -run TestProviderEgressLocation -v
```
Expected: PASS — `TestProviderEgressLocationUpsertAndGet`, `TestProviderEgressLocationFreshness`, `TestRemoveExpiredProviderEgressLocations`.

- [ ] **Step 6: Commit**

```bash
git add db_migrations.go model/provider_egress_location_model.go model/provider_egress_location_model_test.go
git commit -m "feat(model): provider_egress_location storage"
```

---

### Task 2: Controller — resolve a submission into a stored location

**Files:**
- Create: `controller/provider_egress_location_controller.go`
- Test: `controller/provider_egress_location_controller_test.go`

**Interfaces:**
- Consumes: `model.SetProviderEgressLocation`, `model.ProviderEgressLocation`, `model.CreateLocation`, `model.Location`, `model.LocationTypeCountry`, `model.LocationTypeCity`, `model.GetNetworkClientNetwork`.
- Produces:
  - `type SubmitProviderEgressLocationArgs struct { ClientId server.Id `json:"client_id"`; CountryCode string `json:"country_code"`; Country string `json:"country"`; Region string `json:"region,omitempty"`; City string `json:"city,omitempty"`; ASN int `json:"asn,omitempty"`; Org string `json:"org,omitempty"`; Hosting bool `json:"hosting,omitempty"`; Proxy bool `json:"proxy,omitempty"`; Mobile bool `json:"mobile,omitempty"`; CountryConfident bool `json:"country_confident"`; CityConfident bool `json:"city_confident,omitempty"`; ObservedAt time.Time `json:"observed_at"` }`
  - `type SubmitProviderEgressLocationResult struct { LocationId server.Id `json:"location_id"` }`
  - `func SubmitProviderEgressLocation(ctx context.Context, args *SubmitProviderEgressLocationArgs) (*SubmitProviderEgressLocationResult, error)`

- [ ] **Step 1: Write the failing controller tests**

Create `controller/provider_egress_location_controller_test.go`:
```go
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestSubmitProviderEgressLocationCountryOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "US",
			Country:          "United States",
			ASN:              401486,
			Org:              "RAVNIX LLC",
			Hosting:          true,
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		assert.Equal(t, err, nil)
		if res.LocationId == (server.Id{}) {
			t.Fatal("expected a resolved location id")
		}

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		assert.Equal(t, stored.CountryCode, "us")
		assert.Equal(t, stored.ASN, 401486)
		assert.Equal(t, stored.Hosting, true)
		assert.Equal(t, stored.CityConfident, false)
	})
}

func TestSubmitProviderEgressLocationRejectsNotCountryConfident(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			CountryConfident: false,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("a submission that is not country-confident must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

func TestSubmitProviderEgressLocationRejectsUnknownClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         server.NewId(), // never created
			CountryCode:      "us",
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("unknown client_id must be rejected")
		}
	})
}

func TestSubmitProviderEgressLocationCityConfidentStoresCity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			Region:           "Colorado",
			City:             "Denver",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		assert.Equal(t, err, nil)

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		assert.Equal(t, stored.CityConfident, true)

		// the resolved location must be the city-granular row
		loc := model.GetLocation(ctx, stored.LocationId)
		if loc == nil {
			t.Fatal("expected the resolved location row to exist")
		}
		assert.Equal(t, loc.LocationType, model.LocationTypeCity)
	})
}

func TestSubmitProviderEgressLocationRejectsStaleObservedAt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			CountryConfident: true,
			ObservedAt:       server.NowUtc().Add(-30 * 24 * time.Hour),
		})
		if err == nil {
			t.Fatal("a submission observed long ago must be rejected")
		}
	})
}
```

**Note for the implementer:** this test uses `model.GetLocation(ctx, locationId)`. If no such function exists in `model`, add a small one in Task 2 alongside the controller (in `model/provider_egress_location_model.go`) with this exact signature and behavior:
```go
// GetLocation returns the canonical location row, or nil.
func GetLocation(ctx context.Context, locationId server.Id) *Location {
	var loc *Location
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT location_id, location_type, location_name, city_location_id, region_location_id, country_location_id, country_code
			FROM location
			WHERE location_id = $1
			`,
			locationId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				loc = &Location{}
				server.Raise(result.Scan(
					&loc.LocationId,
					&loc.LocationType,
					&loc.Name,
					&loc.CityLocationId,
					&loc.RegionLocationId,
					&loc.CountryLocationId,
					&loc.CountryCode,
				))
			}
		})
	})
	return loc
}
```
Verify the `Location` struct's field names against `model/network_client_location_model.go` before writing this and adjust the scan targets to match exactly.

- [ ] **Step 2: Run the tests to verify they fail**

Run:
```bash
WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 \
BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
go test ./controller/ -run TestSubmitProviderEgressLocation -v
```
Expected: FAIL — `SubmitProviderEgressLocation` undefined.

- [ ] **Step 3: Write the controller**

Create `controller/provider_egress_location_controller.go`:
```go
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// MaxProviderEgressLocationSubmissionAge rejects a submission whose probe is
// already older than this when it arrives. It bounds replay of an old probe.
const MaxProviderEgressLocationSubmissionAge = 24 * time.Hour

type SubmitProviderEgressLocationArgs struct {
	ClientId         server.Id `json:"client_id"`
	CountryCode      string    `json:"country_code"`
	Country          string    `json:"country"`
	Region           string    `json:"region,omitempty"`
	City             string    `json:"city,omitempty"`
	ASN              int       `json:"asn,omitempty"`
	Org              string    `json:"org,omitempty"`
	Hosting          bool      `json:"hosting,omitempty"`
	Proxy            bool      `json:"proxy,omitempty"`
	Mobile           bool      `json:"mobile,omitempty"`
	CountryConfident bool      `json:"country_confident"`
	CityConfident    bool      `json:"city_confident,omitempty"`
	ObservedAt       time.Time `json:"observed_at"`
}

type SubmitProviderEgressLocationResult struct {
	LocationId server.Id `json:"location_id"`
}

// SubmitProviderEgressLocation records a probed egress location for a provider.
// Only country-confident submissions are accepted; city/region are stored only
// when the probe was also city-confident (free geolocation sources disagree on
// city often enough that an unconfirmed city is worse than none).
func SubmitProviderEgressLocation(
	ctx context.Context,
	args *SubmitProviderEgressLocationArgs,
) (*SubmitProviderEgressLocationResult, error) {
	if !args.CountryConfident {
		return nil, fmt.Errorf("Submission is not country-confident.")
	}
	countryCode := strings.ToLower(strings.TrimSpace(args.CountryCode))
	if len(countryCode) != 2 {
		return nil, fmt.Errorf("Country code must be alpha-2.")
	}
	if args.ObservedAt.IsZero() {
		return nil, fmt.Errorf("Missing observed_at.")
	}
	if args.ObservedAt.Before(server.NowUtc().Add(-MaxProviderEgressLocationSubmissionAge)) {
		return nil, fmt.Errorf("Submission is too old.")
	}
	if networkId := model.GetNetworkClientNetwork(ctx, args.ClientId); networkId == nil {
		return nil, fmt.Errorf("Unknown client.")
	}

	// resolve to a canonical location row. city granularity only when the
	// probe agreed on a city; otherwise country.
	location := &model.Location{
		LocationType: model.LocationTypeCountry,
		Country:      args.Country,
		CountryCode:  countryCode,
	}
	if args.CityConfident && args.City != "" {
		location = &model.Location{
			LocationType: model.LocationTypeCity,
			City:         args.City,
			Region:       args.Region,
			Country:      args.Country,
			CountryCode:  countryCode,
		}
	}
	model.CreateLocation(ctx, location)

	model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
		ClientId:      args.ClientId,
		LocationId:    location.LocationId,
		CountryCode:   countryCode,
		ASN:           args.ASN,
		Org:           args.Org,
		Hosting:       args.Hosting,
		Proxy:         args.Proxy,
		Mobile:        args.Mobile,
		CityConfident: args.CityConfident,
		ObservedAt:    args.ObservedAt,
	})

	return &SubmitProviderEgressLocationResult{LocationId: location.LocationId}, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 \
BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
go test ./controller/ -run TestSubmitProviderEgressLocation -v
```
Expected: PASS — all five `TestSubmitProviderEgressLocation*` tests.

- [ ] **Step 5: Commit**

```bash
git add controller/provider_egress_location_controller.go controller/provider_egress_location_controller_test.go model/provider_egress_location_model.go
git commit -m "feat(controller): resolve and store provider egress location submissions"
```

---

### Task 3: Operator-authenticated ingest endpoint

**Files:**
- Create: `api/handlers/provider_egress_location_handlers.go`
- Modify: `api/api.go` (add one route next to the other `/network/...` routes)
- Create: `beta-vault/config/provider_egress.yml.example` (documented example; the real secret file is generated/ignored)
- Test: `api/handlers/provider_egress_location_handlers_test.go`

**Interfaces:**
- Consumes: `controller.SubmitProviderEgressLocation`, `controller.SubmitProviderEgressLocationArgs`, `server.Vault`.
- Produces:
  - `func ProviderEgressLocationSubmit(w http.ResponseWriter, r *http.Request)` — the HTTP handler
  - `func operatorIngestSecret() string` — reads `provider_egress.yml` key `ingest_secret` from the vault (memoized)
  - Route: `POST /network/provider-egress-location`

- [ ] **Step 1: Write the failing handler tests**

Create `api/handlers/provider_egress_location_handlers_test.go`:
```go
package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderEgressLocationSubmitRejectsMissingSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-location", bytes.NewReader(body))
	w := httptest.NewRecorder()

	ProviderEgressLocationSubmit(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

func TestProviderEgressLocationSubmitRejectsWrongSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-location", bytes.NewReader(body))
	req.Header.Set(operatorSecretHeader, "definitely-not-the-secret")
	w := httptest.NewRecorder()

	ProviderEgressLocationSubmit(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
}
```

**Note for the implementer:** these two tests exercise the auth gate only, and must not require a vault secret to be configured — see the "fail closed" rule in Step 3 (an unconfigured secret rejects every request, which is what makes the first test pass deterministically). Full happy-path ingest is covered by the controller tests in Task 2 and by the manual verification in Step 6.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./api/handlers/ -run TestProviderEgressLocationSubmit -v`
Expected: FAIL — `ProviderEgressLocationSubmit` / `operatorSecretHeader` undefined.

- [ ] **Step 3: Write the handler**

Create `api/handlers/provider_egress_location_handlers.go`:
```go
package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// operatorSecretHeader carries the operator ingest secret. This endpoint is
// operator-to-server, not a client route: it is authenticated by a shared
// secret from the vault, not by a network jwt.
const operatorSecretHeader = "X-UR-Operator-Secret"

// maxProviderEgressLocationBody bounds the request body.
const maxProviderEgressLocationBody = 16 * 1024

// operatorIngestSecret reads the operator ingest secret from the vault. It
// returns "" when the resource or key is absent, which makes the endpoint fail
// closed (every request is rejected) rather than open.
var operatorIngestSecret = sync.OnceValue(func() string {
	res, err := server.Vault.SimpleResource("provider_egress.yml")
	if err != nil || res == nil {
		glog.Infof("[pegl]no provider_egress.yml in the vault; ingest endpoint disabled\n")
		return ""
	}
	secret, err := res.String("ingest_secret")
	if err != nil || secret == "" {
		glog.Infof("[pegl]no ingest_secret in provider_egress.yml; ingest endpoint disabled\n")
		return ""
	}
	return secret
})

// ProviderEgressLocationSubmit ingests a probed provider egress location from
// the operator's prober. See
// docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md.
func ProviderEgressLocationSubmit(w http.ResponseWriter, r *http.Request) {
	secret := operatorIngestSecret()
	provided := r.Header.Get(operatorSecretHeader)
	if secret == "" || provided == "" || !hmac.Equal([]byte(secret), []byte(provided)) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxProviderEgressLocationBody+1))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if len(body) > maxProviderEgressLocationBody {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return
	}

	var args controller.SubmitProviderEgressLocationArgs
	if err := json.Unmarshal(body, &args); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	result, err := controller.SubmitProviderEgressLocation(r.Context(), &args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[pegl]could not write response. err = %s\n", err)
	}
}
```

**Note for the implementer:** `server.Vault.SimpleResource(...)` / `res.String(...)` are the non-panicking lookups. Check `env.go` for the exact non-`Require` accessor names (the `Require*` variants panic when absent, which would crash the api at startup and must NOT be used here — the endpoint must fail closed, not crash). If only `Require*` variants exist, wrap the lookup in a `func() (s string) { defer func(){ recover() }(); ... }()` helper so an absent resource yields `""`.

- [ ] **Step 4: Register the route**

In `api/api.go`, add immediately after the `/network/remove-clients` route:
```go
		router.NewRoute("POST", "/network/provider-egress-location", handlers.ProviderEgressLocationSubmit),
```

- [ ] **Step 5: Add the vault example file**

Create `beta-vault/config/provider_egress.yml.example`:
```yaml
# Operator ingest secret for POST /network/provider-egress-location.
# The operator's egress prober sends this value in the X-UR-Operator-Secret
# header. Generate with: openssl rand -base64 32
# If this file or key is absent, the ingest endpoint rejects every request.
ingest_secret: "replace-me"
```

- [ ] **Step 6: Run the tests and verify the route compiles**

Run:
```bash
go build ./... && go test ./api/handlers/ -run TestProviderEgressLocationSubmit -v
```
Expected: build OK; both auth-gate tests PASS.

- [ ] **Step 7: Commit**

```bash
git add api/handlers/provider_egress_location_handlers.go api/handlers/provider_egress_location_handlers_test.go api/api.go beta-vault/config/provider_egress.yml.example
git commit -m "feat(api): operator-authenticated provider egress location ingest"
```

---

### Task 4: Prefer the stored egress location in SetConnectionLocation

**Files:**
- Modify: `controller/network_client_controller.go:54-74` (`SetConnectionLocation`)
- Test: `controller/network_client_controller_test.go` (create if absent)

**Interfaces:**
- Consumes: `model.GetFreshProviderEgressLocation`, `model.ProviderEgressLocationMaxAge`, `model.SetConnectionLocation`, `model.ConnectionLocationScores`, existing `GetLocationForIp`.
- Produces: no new exported symbols — `SetConnectionLocation` keeps its signature `func SetConnectionLocation(ctx context.Context, connectionId server.Id, clientIp string) error`.

- [ ] **Step 1: Write the failing integration test**

Create (or append to) `controller/network_client_controller_test.go`:
```go
package controller

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A provider with a fresh probed egress location must be located from that
// entry, not from the mmdb lookup on its control ip.
func TestSetConnectionLocationPrefersEgressLocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// the probed egress location: japan
		probed := &model.Location{
			LocationType: model.LocationTypeCountry,
			Country:      "Japan",
			CountryCode:  "jp",
		}
		model.CreateLocation(ctx, probed)

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "8.8.8.8:0", handlerId)
		assert.Equal(t, err, nil)

		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  probed.LocationId,
			CountryCode: "jp",
			ObservedAt:  server.NowUtc(),
		})

		err = SetConnectionLocation(ctx, connectionId, "8.8.8.8")
		assert.Equal(t, err, nil)

		var countryLocationId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(
				ctx,
				`SELECT country_location_id FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&countryLocationId))
				}
			})
		})
		assert.Equal(t, countryLocationId, probed.CountryLocationId)
	})
}

// With no probed entry, the existing mmdb path still applies.
func TestSetConnectionLocationFallsBackToMmdb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		handlerId := model.CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, "8.8.8.8:0", handlerId)
		assert.Equal(t, err, nil)

		// no SetProviderEgressLocation call -> mmdb path
		err = SetConnectionLocation(ctx, connectionId, "8.8.8.8")
		assert.Equal(t, err, nil)

		var count int
		server.Db(ctx, func(conn server.PgConn) {
			result, qerr := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM network_client_location WHERE connection_id = $1`,
				connectionId,
			)
			server.WithPgResult(result, qerr, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}
			})
		})
		assert.Equal(t, count, 1)
	})
}
```

**Note for the implementer:** `model.ConnectNetworkClient` returns `(connectionId, clientIp, clientPort, clientAddressHash, err)` — confirm the arity against `model/network_client_model.go` and adjust the blank identifiers if it differs.

- [ ] **Step 2: Run the tests to verify the first one fails**

Run:
```bash
WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 \
BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
go test ./controller/ -run TestSetConnectionLocation -v
```
Expected: `TestSetConnectionLocationPrefersEgressLocation` FAILS (the connection is located from the mmdb lookup on 8.8.8.8, not japan); `TestSetConnectionLocationFallsBackToMmdb` PASSES already.

- [ ] **Step 3: Prefer the stored egress location**

In `controller/network_client_controller.go`, replace the body of `SetConnectionLocation` (currently lines 54-74) with:
```go
func SetConnectionLocation(
	ctx context.Context,
	connectionId server.Id,
	clientIp string,
) error {
	// a provider probed through its own egress is located from that probe, not
	// from a lookup on its control-connection ip: the egress is where user
	// traffic actually exits, and the probed value is cross-checked across
	// several sources. see
	// docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md
	if clientId := model.GetNetworkClientForConnection(ctx, connectionId); clientId != nil {
		if egress := model.GetFreshProviderEgressLocation(
			ctx,
			*clientId,
			model.ProviderEgressLocationMaxAge,
		); egress != nil {
			scores := &model.ConnectionLocationScores{}
			if egress.Hosting {
				scores.NetTypeHosting = 1
			}
			if egress.Proxy {
				scores.NetTypePrivacy = 1
			}
			if egress.Mobile {
				scores.NetTypeVirtual = 1
			}
			err := model.SetConnectionLocation(ctx, connectionId, egress.LocationId, scores)
			if err == nil {
				return nil
			}
			// fall through to the mmdb path on a storage error
			glog.Infof("[ncc][%s]could not set probed egress location. err = %s\n", connectionId, err)
		}
	}

	location, connectionLocationScores, err := GetLocationForIp(ctx, clientIp)
	if err != nil {
		glog.Infof("[ncc][%s]could not find client location. err = %s\n", connectionId, err)
		return err
	}

	model.CreateLocation(ctx, location)
	err = model.SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)
	if err != nil {
		glog.Infof("[ncc][%s]could set connection location. err = %s\n", connectionId, err)
		return err
	}
	return nil
}
```

**Note for the implementer:** this needs a way to get the `client_id` for a `connection_id`. If `model.GetNetworkClientForConnection(ctx, connectionId) *server.Id` does not already exist, add it to `model/provider_egress_location_model.go`:
```go
// GetNetworkClientForConnection returns the client id for a connection, or nil.
func GetNetworkClientForConnection(ctx context.Context, connectionId server.Id) *server.Id {
	var clientId *server.Id
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT client_id FROM network_client_connection WHERE connection_id = $1`,
			connectionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&clientId))
			}
		})
	})
	return clientId
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run:
```bash
WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 \
BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
go test ./controller/ -run TestSetConnectionLocation -v
```
Expected: PASS — both tests.

- [ ] **Step 5: Run the surrounding suites for regressions**

Run:
```bash
WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 \
BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
go test ./controller/ -run 'TestSetConnectionLocation|TestSubmitProviderEgressLocation' -count=1 && \
WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 \
BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
go test ./model/ -run 'TestProviderEgressLocation|TestSetConnectionLocation|TestCanonicalLocations|TestUpdateClientLocations|TestUpdateClientScores' -count=1
```
Expected: all PASS. In particular `TestSetConnectionLocationToleratesCountryOnlyLocation` (the country-only fix this design leans on) must still pass.

- [ ] **Step 6: Commit**

```bash
git add controller/network_client_controller.go controller/network_client_controller_test.go model/provider_egress_location_model.go
git commit -m "feat(controller): prefer probed provider egress location over mmdb"
```

---

### Task 5: Sweep expired entries on the taskworker

**Files:**
- Create: `taskworker/work/provider_egress_location_work.go`
- Modify: `taskworker/taskworker.go` (one `Schedule...` line in `InitTasks`, one `AddTargets` entry in `InitTaskWorkerWithSettings`)

**Interfaces:**
- Consumes: `model.RemoveExpiredProviderEgressLocations`, `model.ProviderEgressLocationMaxAge`.
- Produces: `RemoveExpiredProviderEgressLocationsArgs/Result`, `ScheduleRemoveExpiredProviderEgressLocations`, `RemoveExpiredProviderEgressLocations`, `RemoveExpiredProviderEgressLocationsPost` (mirrors the existing `wallet_auth_challenge_work.go` shape exactly).

- [ ] **Step 1: Write the work file**

Create `taskworker/work/provider_egress_location_work.go`:
```go
package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type RemoveExpiredProviderEgressLocationsArgs struct{}

type RemoveExpiredProviderEgressLocationsResult struct{}

func ScheduleRemoveExpiredProviderEgressLocations(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredProviderEgressLocations,
		&RemoveExpiredProviderEgressLocationsArgs{},
		clientSession,
		task.RunOnce("remove_expired_provider_egress_locations"),
		task.RunAt(server.NowUtc().Add(6*time.Hour)),
	)
}

// RemoveExpiredProviderEgressLocations drops probed locations well past their
// trust window, so a provider that stops being probed eventually falls back to
// the mmdb path instead of being pinned to a stale location forever. The cutoff
// is deliberately looser than ProviderEgressLocationMaxAge: reads already
// ignore stale rows, so this is only reclaiming storage.
func RemoveExpiredProviderEgressLocations(
	_ *RemoveExpiredProviderEgressLocationsArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredProviderEgressLocationsResult, error) {
	minObservedAt := server.NowUtc().Add(-4 * model.ProviderEgressLocationMaxAge)
	model.RemoveExpiredProviderEgressLocations(clientSession.Ctx, minObservedAt)
	return &RemoveExpiredProviderEgressLocationsResult{}, nil
}

func RemoveExpiredProviderEgressLocationsPost(
	_ *RemoveExpiredProviderEgressLocationsArgs,
	_ *RemoveExpiredProviderEgressLocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveExpiredProviderEgressLocations(clientSession, tx)
	return nil
}
```

- [ ] **Step 2: Register the task**

In `taskworker/taskworker.go`, in `InitTasks`, add immediately after the `work.ScheduleRemoveExpiredWalletNonces(clientSession, tx)` line:
```go
		work.ScheduleRemoveExpiredProviderEgressLocations(clientSession, tx)
```

And in `InitTaskWorkerWithSettings`'s `taskWorker.AddTargets(...)` list, add after the `work.RemoveExpiredWalletNonces` entry:
```go
		task.NewTaskTargetWithPost(
			work.RemoveExpiredProviderEgressLocations,
			work.RemoveExpiredProviderEgressLocationsPost,
		),
```

- [ ] **Step 3: Verify it builds and vets**

Run: `go build ./... && go vet ./...`
Expected: both clean.

- [ ] **Step 4: Commit**

```bash
git add taskworker/work/provider_egress_location_work.go taskworker/taskworker.go
git commit -m "feat(taskworker): sweep expired provider egress locations"
```

---

### Task 6: Cherry-pick to the upstream branch and open the PR

**Files:** none (git operations only).

- [ ] **Step 1: Verify the full build and the affected suites on the beta branch**

Run:
```bash
go build ./... && go vet ./...
```
Expected: both clean.

- [ ] **Step 2: Cherry-pick the five feature commits onto the upstream-facing branch**

```bash
git log --oneline -6   # note the five commit shas from Tasks 1-5, oldest first
git checkout feat/seedphrase-to-upstream
git cherry-pick <task1-sha> <task2-sha> <task3-sha> <task4-sha> <task5-sha>
```
Expected: clean cherry-picks. If `db_migrations.go` conflicts, resolve by keeping BOTH sides' migrations with the upstream branch's existing migrations first and this feature's appended at the tail (never reorder existing migrations).

- [ ] **Step 3: Verify no conflict markers and the build is clean**

```bash
grep -rn "^<<<<<<<\|^>>>>>>>" --include=*.go . ; go build ./...
```
Expected: no grep output; build clean.

- [ ] **Step 4: Push and open a NEW upstream PR**

```bash
git push fork feat/seedphrase-to-upstream
```
Then open a new PR against `urnetwork/server:main` titled
"feat: provider egress geolocation ingest and integration", describing the
free-geolocation motivation and linking the design spec.

- [ ] **Step 5: Dispatch Opus review agents**

Per the standing workflow, dispatch two independent Opus review agents on the
cherry-picked diff, giving them the design spec path and this plan for context.
Address any blocking findings before considering the PR done.

---

## Self-Review

**1. Spec coverage (B section):**
- B1 ingest endpoint, operator-authenticated → Task 3 ✓ (vault shared secret, constant-time compare, fail-closed)
- B1 validation: known client_id, country→canonical location via `CreateLocation` → Task 2 ✓
- B2 storage `provider_egress_location` keyed by client_id, with all listed columns → Task 1 ✓
- B2 freshness index + max-age sweep → Task 1 (index, `RemoveExpired...`) + Task 5 (scheduled sweep) ✓
- B3 `SetConnectionLocation` prefers fresh stored egress location; mmdb fallback on miss/stale/not-a-provider → Task 4 ✓
- B3 net_type flags from the probe → Task 4 (`scores` mapping) ✓
- Spec's "only country-confident accepted; city only when city-confident" → Task 2 ✓
- Rollout: beta branch → cherry-pick → NEW upstream PR → Opus reviews → Task 6 ✓
- Testing listed in spec (ingest auth, validation, upsert, prefer/fallback, country-only regression) → Tasks 1-4, plus the explicit regression run in Task 4 Step 5 ✓

**2. Placeholder scan:** none. Three steps carry explicit "Note for the implementer" verification instructions (exact `Location` field names, `ConnectNetworkClient` arity, non-panicking vault accessor names) — these are directed checks against named files with fallback code provided, not deferred work.

**3. Type consistency:** `ProviderEgressLocation` fields, `ProviderEgressLocationMaxAge`, `SetProviderEgressLocation`, `GetProviderEgressLocation`, `GetFreshProviderEgressLocation`, `RemoveExpiredProviderEgressLocations`, `SubmitProviderEgressLocationArgs/Result`, `SubmitProviderEgressLocation`, `GetNetworkClientForConnection`, `GetLocation`, `operatorSecretHeader`, `operatorIngestSecret`, `ProviderEgressLocationSubmit` are used identically across Tasks 1-5. The JSON field names in `SubmitProviderEgressLocationArgs` (Task 2) match the spec's documented ingest body and are what A2 must send.

## Interface contract for A2 (the prober)

A2 submits `POST /network/provider-egress-location` with header
`X-UR-Operator-Secret: <ingest_secret>` and this body (field names exact):
```json
{
  "client_id": "<provider client id>",
  "country_code": "us",
  "country": "United States",
  "region": "",
  "city": "",
  "asn": 401486,
  "org": "RAVNIX LLC",
  "hosting": false,
  "proxy": false,
  "mobile": false,
  "country_confident": true,
  "city_confident": false,
  "observed_at": "2026-07-25T00:00:00Z"
}
```
Response `200 {"location_id":"<uuid>"}`; `401` on a bad/absent secret; `400`
with a message on validation failure (not country-confident, unknown client,
bad country code, missing/too-old `observed_at`).
