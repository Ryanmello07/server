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

		// upsert replaces, given a strictly newer observed_at: the upsert is
		// monotonic (see TestProviderEgressLocationUpsertIgnoresOlderReplay below),
		// so a second submission at the same observed_at would not win.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  country.LocationId,
			CountryCode: "us",
			ASN:         999,
			Hosting:     false,
			Proxy:       true,
			ObservedAt:  now.Add(time.Minute),
		})
		got = GetProviderEgressLocation(ctx, clientId)
		assert.Equal(t, got.ASN, 999)
		assert.Equal(t, got.Hosting, false)
		assert.Equal(t, got.Proxy, true)
	})
}

// The upsert is monotonic in observed_at: a replayed submission older than
// what is already stored must not clobber the newer row.
func TestProviderEgressLocationUpsertIgnoresOlderReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		usCountry := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, usCountry)

		jpCountry := &Location{
			LocationType: LocationTypeCountry,
			Country:      "Japan",
			CountryCode:  "jp",
		}
		CreateLocation(ctx, jpCountry)

		clientId := server.NewId()
		newer := server.NowUtc()
		older := newer.Add(-1 * time.Hour)

		// the newer probe lands first
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  jpCountry.LocationId,
			CountryCode: "jp",
			ASN:         111,
			ObservedAt:  newer,
		})

		// a stale/replayed older probe arrives afterward and must not win
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  usCountry.LocationId,
			CountryCode: "us",
			ASN:         222,
			ObservedAt:  older,
		})

		got := GetProviderEgressLocation(ctx, clientId)
		if got == nil {
			t.Fatal("expected a stored egress location")
		}
		assert.Equal(t, got.CountryCode, "jp")
		assert.Equal(t, got.ASN, 111)
		assert.Equal(t, got.LocationId, jpCountry.LocationId)
	})
}

func TestProviderEgressLocationCountryCodeLowercased(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		country := &Location{
			LocationType: LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, country)

		clientId := server.NewId()
		// geolocation APIs return uppercase codes (e.g. "US"); the model must
		// normalize to lowercase before storing, matching CreateLocation's
		// established invariant that country codes are stored/compared lowercased.
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  country.LocationId,
			CountryCode: "US",
			ASN:         12345,
			Org:         "TEST ORG",
			ObservedAt:  server.NowUtc(),
		})

		got := GetProviderEgressLocation(ctx, clientId)
		if got == nil {
			t.Fatal("expected a stored egress location")
		}
		assert.Equal(t, got.CountryCode, "us")
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

// TestProviderEgressLocationHasVerdictColumns asserts the three verdict columns
// exist. They are additive with safe defaults, so no existing reader or row is
// affected -- but nothing can record a verdict until they are there.
func TestProviderEgressLocationHasVerdictColumns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for _, col := range []string{"verdict", "verdict_reason", "assurance"} {
			var exists bool
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT EXISTS (
						SELECT 1 FROM information_schema.columns
						WHERE table_name = 'provider_egress_location' AND column_name = $1
					)
					`,
					col,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&exists))
					}
				})
			})
			if !exists {
				t.Errorf("provider_egress_location missing column %q", col)
			}
		}
	})
}

// TestProviderEgressLocationVerdictDefaults pins the write path's normalization.
// SetProviderEgressLocation names every column explicitly, which bypasses the
// column defaults, so a caller that computes no judgement -- every caller until
// the ingest path does -- must still store unverified/direct, not the empty
// string.
func TestProviderEgressLocationVerdictDefaults(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()
		observedAt := server.NowUtc()

		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  server.NewId(),
			CountryCode: "es",
			ObservedAt:  observedAt,
		})

		stored := GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected a stored egress location")
		}
		assert.Equal(t, stored.Verdict, ProviderEgressVerdictUnverified)
		assert.Equal(t, stored.VerdictReason, "")
		assert.Equal(t, stored.Assurance, ProviderEgressAssuranceDirect)

		// an explicit judgement is stored verbatim
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:      clientId,
			LocationId:    server.NewId(),
			CountryCode:   "de",
			ObservedAt:    observedAt.Add(time.Hour),
			Verdict:       "suspect",
			VerdictReason: "unstable",
			Assurance:     ProviderEgressAssuranceDirect,
		})

		stored = GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected a stored egress location")
		}
		assert.Equal(t, stored.Verdict, "suspect")
		assert.Equal(t, stored.VerdictReason, "unstable")
		assert.Equal(t, stored.Assurance, ProviderEgressAssuranceDirect)
	})
}
