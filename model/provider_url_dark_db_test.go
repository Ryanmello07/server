package model

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// URL-dark against the database: history rows are written as accepted ingest
// stores a counted URL turn, and every verdict is read back through the paths
// the server publishes and serves from.

// Writes one counted URL turn: cycle-admitted, one URL, the selected policy.
// A nil evidence document stores SQL NULL.
func urlDarkTestRow(ctx context.Context, clientId server.Id, at time.Time, ok int, evidence any) {
	urlDarkTestRowAs(ctx, clientId, at, ok, evidence, true, SelectedProviderUrlProbePolicyVersion())
}

// urlDarkTestRow with the admission bit and policy version given.
func urlDarkTestRowAs(ctx context.Context, clientId server.Id, at time.Time, ok int, evidence any, urlProbe bool, policyVersion int) {
	var encoded *string
	if evidence != nil {
		var raw string
		if s, isString := evidence.(string); isString {
			raw = s
		} else {
			b, err := json.Marshal(evidence)
			server.Raise(err)
			raw = string(b)
		}
		encoded = &raw
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
			(run_id, client_id, measured_at, ok_count, total_count, class_results, tls_authentication_failure,
			 cycle_started_at, url_probe, url_probe_evidence, url_probe_policy_version)
			VALUES ($1, $2, $3, $4, 1, '{}', false, $3, $7, $5::jsonb, $6)`,
			server.NewId(), clientId, at.UTC(), ok, encoded, policyVersion, urlProbe))
	})
}

func urlDarkTestTurn(ctx context.Context, clientId server.Id, at time.Time, turn providerUrlDarkTurn) {
	switch turn {
	case providerUrlDarkSuccess:
		urlDarkTestRow(ctx, clientId, at, 1, fp2TestUrlProbeEvidence(at, true))
	case providerUrlDarkForwarded:
		// a captcha page: status, first byte and authenticated TLS came back
		urlDarkTestRow(ctx, clientId, at, 0, fp2TestUrlProbeEvidence(at, false))
	default:
		urlDarkTestRow(ctx, clientId, at, 0, urlDarkTestNoResponseEvidence(at))
	}
}

// Successes from `count` other, unconnected providers just before `at`: the
// positive control that lets a no-response turn at `at` count.
func urlDarkTestControls(ctx context.Context, at time.Time, count int, before time.Duration) {
	for range count {
		urlDarkTestTurn(ctx, server.NewId(), at.Add(-before), providerUrlDarkSuccess)
	}
}

// No-response turns at each time, each with `controls` control successes a
// minute before it.
func urlDarkTestDark(ctx context.Context, clientId server.Id, controls int, times ...time.Time) {
	for _, at := range times {
		urlDarkTestControls(ctx, at, controls, time.Minute)
		urlDarkTestTurn(ctx, clientId, at, providerUrlDarkNoResponse)
	}
}

// Connected public providers with no URL history, so the published population
// is large enough that the serving floor does not hide a verdict.
func urlDarkTestFleet(ctx context.Context, t testing.TB, city *Location, count int) []*egressTestProvider {
	// A read that failed open in an earlier test of this process must not
	// leave this one inside the retry delay. Fixtures that build their own
	// provider_egress_health_history without the URL columns fail it
	// (provider_count_cohort_query_test.go), and every cohort filter reads it.
	providerUrlDarkLast.Lock()
	providerUrlDarkLast.failedAt = time.Time{}
	providerUrlDarkLast.Unlock()
	providers := []*egressTestProvider{}
	for range count {
		providers = append(providers, egressTestConnect(ctx, t, city, egressTestFast, nil, nil))
	}
	return providers
}

// Publishes, then requires the same excluded set through the warm Redis
// snapshot, the cold read-through, the dashboard reasons, the public location
// count and a request naming each excluded provider.
func urlDarkTestAssert(ctx context.Context, t testing.TB, city *Location, population int, candidates []*egressTestProvider, want ...*egressTestProvider) {
	t.Helper()
	egressTestPasses(ctx, t)
	candidateIds := []server.Id{}
	for _, provider := range candidates {
		candidateIds = append(candidateIds, provider.clientId)
	}
	wantIds := map[server.Id]bool{}
	for _, provider := range want {
		wantIds[provider.clientId] = true
	}

	warm, err := getProviderHardExclusions(ctx, candidateIds)
	connect.AssertEqual(t, err, nil)
	if !maps.Equal(warm, wantIds) {
		t.Fatalf("published snapshot excluded %v, want %v", warm, wantIds)
	}
	connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts[ProviderExcludedUrlDark], int64(len(want)))
	connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), population-len(want))
	for _, provider := range want {
		providers := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &provider.clientId}}, RankModeQuality, 1, true, server.NewId())
		connect.AssertEqual(t, len(providers), 0)
		inspection := InspectProviderEgress(ctx, provider.clientId)
		connect.AssertEqual(t, inspection.UrlDark, true)
		connect.AssertEqual(t, inspection.Reason, ProviderExcludedUrlDark)
	}

	hardExclusionTestClear(ctx, t)
	cold, err := getProviderHardExclusions(ctx, candidateIds)
	connect.AssertEqual(t, err, nil)
	if !maps.Equal(cold, wantIds) {
		t.Fatalf("read-through excluded %v, want %v", cold, wantIds)
	}
}

// The SQL that serves classifies exactly as the Go statement of the rule, and
// anything that cannot prove no-response is not no-response.
func TestProviderUrlDarkSqlClassifierMatchesGo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		status := urlDarkTestNoResponseEvidence(now)
		status.StatusCode = 502
		firstByte := urlDarkTestNoResponseEvidence(now)
		firstByte.FirstByteReceived = true
		tlsEvent := urlDarkTestNoResponseEvidence(now)
		tlsEvent.Security = []egresshealth.UrlProbeSecurityEvent{{MeasuredAt: now, TlsFailure: true}}
		emptySecurity := urlDarkTestNoResponseEvidence(now)
		emptySecurity.Security = []egresshealth.UrlProbeSecurityEvent{}
		runEnded := urlDarkTestNoResponseEvidence(now)
		runEnded.FailureStage = "run_ended"
		typed := []struct {
			ok       int
			evidence *egresshealth.UrlProbeEvidence
		}{
			{1, fp2TestUrlProbeEvidence(now, true)},
			{0, fp2TestUrlProbeEvidence(now, false)},
			{0, nil},
			{0, status},
			{0, firstByte},
			{0, tlsEvent},
			{0, urlDarkTestNoResponseEvidence(now)},
			{0, emptySecurity},
			{0, runEnded},
			// a passing turn is never no-response whatever its document says
			{1, urlDarkTestNoResponseEvidence(now)},
		}
		raw := []struct {
			name     string
			evidence string
			want     bool
		}{
			{"json null", `null`, false},
			{"missing first byte", `{"status_code":0,"security":null}`, false},
			{"missing status", `{"first_byte_received":false,"security":null}`, false},
			{"null first byte", `{"first_byte_received":null,"status_code":0}`, false},
			{"missing security", `{"first_byte_received":false,"status_code":0}`, true},
			{"object security", `{"first_byte_received":false,"status_code":0,"security":{}}`, false},
			{"run ended", `{"first_byte_received":false,"status_code":0,"security":null,"failure_stage":"run_ended"}`, false},
			{"null failure stage", `{"first_byte_received":false,"status_code":0,"security":null,"failure_stage":null}`, true},
		}
		query := `SELECT ` + providerUrlDarkNoResponseSql("turn") + ` FROM provider_egress_health_history AS turn WHERE client_id = $1`
		read := func(clientId server.Id) bool {
			var noResponse bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, query, clientId).Scan(&noResponse))
			})
			return noResponse
		}
		for i, c := range typed {
			clientId := server.NewId()
			if c.evidence == nil {
				urlDarkTestRow(ctx, clientId, now, c.ok, nil)
			} else {
				urlDarkTestRow(ctx, clientId, now, c.ok, c.evidence)
			}
			want := classifyProviderUrlDarkTurn(c.ok, c.evidence) == providerUrlDarkNoResponse
			if got := read(clientId); got != want {
				t.Errorf("typed case %d: sql %t, go %t", i, got, want)
			}
		}
		for _, c := range raw {
			clientId := server.NewId()
			urlDarkTestRow(ctx, clientId, now, 0, c.evidence)
			if got := read(clientId); got != c.want {
				t.Errorf("%s: sql %t, want %t", c.name, got, c.want)
			}
		}
	})
}

// (a) Three no-response turns, each with the fleet answering around it: the
// provider is hard-excluded everywhere with reason url_dark, and its status is
// not eligible.
func TestUrlDarkExcludedEverywhere(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		dark := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestDark(ctx, dark.clientId, 3, now.Add(-50*time.Minute), now.Add(-40*time.Minute), now.Add(-30*time.Minute))

		urlDarkTestAssert(ctx, t, city, 11, append(fleet, dark), dark)
		status := GetClientProviderStatus(ctx, dark.networkId, dark.clientId)
		connect.AssertEqual(t, status.Reason, ProviderStatusReasonNotEligible)
	})
}

// (b) A forwarded turn as the newest breaks the run, and so does a turn whose
// evidence is missing: neither can prove the exit is dark.
func TestUrlDarkForwardedTurnBreaksTheRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		forwarded := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		unknown := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		for _, provider := range []*egressTestProvider{forwarded, unknown} {
			urlDarkTestDark(ctx, provider.clientId, 3, now.Add(-50*time.Minute), now.Add(-40*time.Minute))
		}
		urlDarkTestControls(ctx, now.Add(-30*time.Minute), 3, time.Minute)
		urlDarkTestTurn(ctx, forwarded.clientId, now.Add(-30*time.Minute), providerUrlDarkForwarded)
		urlDarkTestRow(ctx, unknown.clientId, now.Add(-30*time.Minute), 0, nil)

		urlDarkTestAssert(ctx, t, city, 12, append(fleet, forwarded, unknown))
	})
}

// (c) A passing turn after the verdict readmits at the next publication.
func TestUrlDarkPassingTurnReadmits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		dark := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestDark(ctx, dark.clientId, 3, now.Add(-50*time.Minute), now.Add(-40*time.Minute), now.Add(-30*time.Minute))
		urlDarkTestAssert(ctx, t, city, 11, append(fleet, dark), dark)

		urlDarkTestTurn(ctx, dark.clientId, now.Add(-10*time.Minute), providerUrlDarkSuccess)
		urlDarkTestAssert(ctx, t, city, 11, append(fleet, dark))
		status := GetClientProviderStatus(ctx, dark.networkId, dark.clientId)
		if status.Reason == ProviderStatusReasonNotEligible {
			t.Fatal("a readmitted provider is still not eligible")
		}
	})
}

// (d) Breaker layer 1: with fewer than three other providers succeeding in the
// half hour before a no-response turn, the turn is excused (a fleet outage).
// An excused turn is ignored entirely: it neither counts nor breaks a run.
func TestUrlDarkFleetOutageIsExcused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)

		// Each provider's turns are an hour apart and hours from the others,
		// so no control window reaches another turn's controls.

		// two controls only
		thin := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestDark(ctx, thin.clientId, 2, now.Add(-12*time.Hour), now.Add(-11*time.Hour), now.Add(-10*time.Hour))

		// three controls, but 31 minutes before each turn: outside the window
		stale := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		for _, at := range []time.Time{now.Add(-9 * time.Hour), now.Add(-8 * time.Hour), now.Add(-7 * time.Hour)} {
			urlDarkTestControls(ctx, at, 3, 31*time.Minute)
			urlDarkTestTurn(ctx, stale.clientId, at, providerUrlDarkNoResponse)
		}

		// an excused turn in the middle of a counted run neither counts nor
		// breaks it: the three counted turns still make a verdict
		excusedInside := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestDark(ctx, excusedInside.clientId, 3, now.Add(-5*time.Hour), now.Add(-4*time.Hour))
		urlDarkTestTurn(ctx, excusedInside.clientId, now.Add(-150*time.Minute), providerUrlDarkNoResponse)
		urlDarkTestDark(ctx, excusedInside.clientId, 3, now.Add(-60*time.Minute))

		urlDarkTestAssert(ctx, t, city, 13, append(fleet, thin, stale, excusedInside), excusedInside)
	})
}

// (e) A success within eight hours of the newest failure keeps a provider out
// of darkness; one older than eight hours does not.
func TestUrlDarkRecentSuccessWithinEightHours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		newest := now.Add(-30 * time.Minute)
		times := []time.Time{now.Add(-50 * time.Minute), now.Add(-40 * time.Minute), newest}
		recent := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestTurn(ctx, recent.clientId, newest.Add(-7*time.Hour), providerUrlDarkSuccess)
		old := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestTurn(ctx, old.clientId, newest.Add(-9*time.Hour), providerUrlDarkSuccess)
		for _, at := range times {
			urlDarkTestControls(ctx, at, 3, time.Minute)
			urlDarkTestTurn(ctx, recent.clientId, at, providerUrlDarkNoResponse)
			urlDarkTestTurn(ctx, old.clientId, at, providerUrlDarkNoResponse)
		}

		urlDarkTestAssert(ctx, t, city, 12, append(fleet, recent, old), old)
	})
}

// (f) Breaker layer 2: when nearly everyone is dark, the longest-standing
// verdicts are honored and at least max(10, 10%) of the population still
// serves.
func TestUrlDarkServingFloorHonorsOldest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 3)
		dark := []*egressTestProvider{}
		for i := range 12 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			first := now.Add(-3*time.Hour + time.Duration(i)*5*time.Minute)
			// it last answered more than eight hours before its failures
			urlDarkTestTurn(ctx, provider.clientId, first.Add(-9*time.Hour), providerUrlDarkSuccess)
			urlDarkTestDark(ctx, provider.clientId, 3, first, first.Add(time.Minute), first.Add(2*time.Minute))
			dark = append(dark, provider)
		}
		// population 15, floor 10: the five that answered longest ago stand
		connect.AssertEqual(t, providerUrlDarkServingFloor(15), 10)
		urlDarkTestAssert(ctx, t, city, 15, append(fleet, dark...), dark[:5]...)

		// Scoped readers see the fleet's honored set, not a floor over their
		// own clients: a released verdict stays released for a request or a
		// status that names only it.
		connect.AssertEqual(t, GetClientProviderStatus(ctx, dark[0].networkId, dark[0].clientId).Reason, ProviderStatusReasonNotEligible)
		if GetClientProviderStatus(ctx, dark[11].networkId, dark[11].clientId).Reason == ProviderStatusReasonNotEligible {
			t.Fatal("a released verdict is not eligible in provider status")
		}
		hardExclusionTestClear(ctx, t)
		for _, provider := range []*egressTestProvider{dark[0], dark[11]} {
			excluded, err := getProviderHardExclusions(ctx, []server.Id{provider.clientId})
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, excluded[provider.clientId], provider == dark[0])
			connect.AssertEqual(t, len(excluded), map[bool]int{true: 1, false: 0}[provider == dark[0]])
		}
	})
}

// (g) A partially failing provider is never URL-dark: a success inside eight
// hours, or successes interleaved with failures, keep it online.
func TestUrlDarkPartialFailureStaysOnline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		earlierSuccess := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		interleaved := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestTurn(ctx, earlierSuccess.clientId, now.Add(-3*time.Hour), providerUrlDarkSuccess)
		urlDarkTestDark(ctx, earlierSuccess.clientId, 3, now.Add(-50*time.Minute), now.Add(-40*time.Minute), now.Add(-30*time.Minute))
		pattern := []providerUrlDarkTurn{
			providerUrlDarkNoResponse, providerUrlDarkNoResponse, providerUrlDarkSuccess,
			providerUrlDarkNoResponse, providerUrlDarkNoResponse, providerUrlDarkSuccess,
			providerUrlDarkNoResponse, providerUrlDarkNoResponse,
		}
		for i, turn := range pattern {
			at := now.Add(-4*time.Hour + time.Duration(i)*20*time.Minute)
			urlDarkTestControls(ctx, at, 3, time.Minute)
			urlDarkTestTurn(ctx, interleaved.clientId, at, turn)
		}

		urlDarkTestAssert(ctx, t, city, 12, append(fleet, earlierSuccess, interleaved))
		for _, provider := range []*egressTestProvider{earlierSuccess, interleaved} {
			inspection := InspectProviderEgress(ctx, provider.clientId)
			if inspection.UrlDark || inspection.HardExcluded || !inspection.Online {
				t.Errorf("%s: partial failure left online: %s", provider.clientId, fmt.Sprintf("%+v", inspection.Reason))
			}
		}
	})
}

// Breaker layer 1 counts only other providers' successes, each provider once:
// failures from the rest of the fleet, or one healthy provider answering
// repeatedly, cannot vouch for the prober.
func TestUrlDarkControlsAreDistinctOtherSuccesses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		candidates := append([]*egressTestProvider{}, fleet...)

		// a fleet-wide outage: four providers fail at the same instants and
		// nobody succeeds, so each one's failures sit in the others' windows
		for range 4 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			for _, at := range []time.Time{now.Add(-50 * time.Minute), now.Add(-40 * time.Minute), now.Add(-30 * time.Minute)} {
				urlDarkTestTurn(ctx, provider.clientId, at, providerUrlDarkNoResponse)
			}
			candidates = append(candidates, provider)
		}

		// three successes before each turn, all from one other provider, then
		// from only two
		for i, controlCount := range []int{1, 2} {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			controlIds := []server.Id{}
			for range controlCount {
				controlIds = append(controlIds, server.NewId())
			}
			first := now.Add(-6*time.Hour + time.Duration(i)*2*time.Hour)
			for _, at := range []time.Time{first, first.Add(10 * time.Minute), first.Add(20 * time.Minute)} {
				for j := range 3 {
					urlDarkTestTurn(ctx, controlIds[j%controlCount], at.Add(-time.Duration(j+1)*time.Minute), providerUrlDarkSuccess)
				}
				urlDarkTestTurn(ctx, provider.clientId, at, providerUrlDarkNoResponse)
			}
			candidates = append(candidates, provider)
		}

		urlDarkTestAssert(ctx, t, city, 16, candidates)
	})
}

// A verdict needs K counted no-response turns: a provider with fewer, even
// all controlled, has not shown it is dark.
func TestUrlDarkNeedsKTurns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		connect.AssertEqual(t, GetProviderEgressRules().DarkConsecutiveFailures, 3)
		short := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestDark(ctx, short.clientId, 3, now.Add(-50*time.Minute), now.Add(-40*time.Minute))

		urlDarkTestAssert(ctx, t, city, 11, append(fleet, short))
	})
}

// Breaker layer 2 counts only providers no other hard exclusion removes: a
// TLS-quarantined provider serves no one, so it neither raises the floor's
// budget nor takes a verdict's place.
func TestUrlDarkServingFloorIgnoresOtherExclusions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 3)
		for range 5 {
			quarantined := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security(client_id,url_key,destination,measured_at,tls_failure)
					VALUES($1,'synthetic','{}',$2,true)`, quarantined.clientId, now))
			})
		}
		dark := []*egressTestProvider{}
		for i := range 12 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			first := now.Add(-3*time.Hour + time.Duration(i)*5*time.Minute)
			// it last answered more than eight hours before its failures
			urlDarkTestTurn(ctx, provider.clientId, first.Add(-9*time.Hour), providerUrlDarkSuccess)
			urlDarkTestDark(ctx, provider.clientId, 3, first, first.Add(time.Minute), first.Add(2*time.Minute))
			dark = append(dark, provider)
		}
		// 20 connected public providers, but only 15 not otherwise excluded:
		// floor 10 allows 5 verdicts, not the 10 a count of 20 would
		urlDarkTestAssert(ctx, t, city, 15, append(fleet, dark...), dark[:5]...)
	})
}

// Only turns inside the horizon are walked: a provider the prober has not
// reached for a day is not judged.
func TestUrlDarkHorizon(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		stale := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		first := now.Add(-providerUrlDarkHorizon - time.Hour)
		urlDarkTestDark(ctx, stale.clientId, 3, first, first.Add(10*time.Minute), first.Add(20*time.Minute))
		inside := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		first = now.Add(-providerUrlDarkHorizon + time.Hour)
		urlDarkTestDark(ctx, inside.clientId, 3, first, first.Add(10*time.Minute), first.Add(20*time.Minute))

		urlDarkTestAssert(ctx, t, city, 12, append(fleet, stale, inside), inside)
	})
}

// A failed or slow URL-dark read never stops publication: the last honored
// set stands while it is fresh, then none, and the other hard exclusions
// publish as before.
func TestUrlDarkReadFailsOpen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleetTimeout, scopedTimeout := providerUrlDarkFleetTimeout, providerUrlDarkScopedTimeout
		resetLast := func() {
			providerUrlDarkLast.Lock()
			providerUrlDarkLast.honored, providerUrlDarkLast.computedAt, providerUrlDarkLast.failedAt = nil, time.Time{}, time.Time{}
			providerUrlDarkLast.Unlock()
		}
		resetLast()
		defer func() {
			providerUrlDarkFleetTimeout, providerUrlDarkScopedTimeout = fleetTimeout, scopedTimeout
			resetLast()
		}()

		fleet := urlDarkTestFleet(ctx, t, city, 10)
		dark := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestDark(ctx, dark.clientId, 3, now.Add(-50*time.Minute), now.Add(-40*time.Minute), now.Add(-30*time.Minute))
		quarantined := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security(client_id,url_key,destination,measured_at,tls_failure)
				VALUES($1,'synthetic','{}',$2,true)`, quarantined.clientId, now))
		})
		candidateIds := []server.Id{dark.clientId, quarantined.clientId}
		for _, provider := range fleet {
			candidateIds = append(candidateIds, provider.clientId)
		}
		published := func(want ...*egressTestProvider) {
			t.Helper()
			// egressTestPasses requires every pass to succeed
			egressTestPasses(ctx, t)
			for _, cold := range []bool{false, true} {
				if cold {
					hardExclusionTestClear(ctx, t)
				}
				excluded, err := getProviderHardExclusions(ctx, candidateIds)
				connect.AssertEqual(t, err, nil)
				wantIds := map[server.Id]bool{quarantined.clientId: true}
				for _, provider := range want {
					wantIds[provider.clientId] = true
				}
				if !maps.Equal(excluded, wantIds) {
					t.Fatalf("cold=%t excluded %v, want %v", cold, excluded, wantIds)
				}
			}
			GetClientProviderStatus(ctx, dark.networkId, dark.clientId)
		}
		published(dark)

		// every read now outlasts its budget: the last set stands
		providerUrlDarkFleetTimeout, providerUrlDarkScopedTimeout = time.Nanosecond, time.Nanosecond
		published(dark)
		providerUrlDarkLast.Lock()
		failed := !providerUrlDarkLast.failedAt.IsZero()
		providerUrlDarkLast.Unlock()
		connect.AssertEqual(t, failed, true)

		// a fallback older than the success lookback is not used
		providerUrlDarkLast.Lock()
		providerUrlDarkLast.computedAt = time.Now().Add(-ProviderEgressHealthMaxAge - time.Minute)
		providerUrlDarkLast.Unlock()
		published()

		// budgets restored and the retry delay over: computed again
		providerUrlDarkFleetTimeout, providerUrlDarkScopedTimeout = fleetTimeout, scopedTimeout
		providerUrlDarkLast.Lock()
		providerUrlDarkLast.failedAt = time.Time{}
		providerUrlDarkLast.Unlock()
		published(dark)
	})
}

// The passes whose cohort is the fleet (the publication passes and the
// dashboard count) load their filter for the cohort they captured, and still
// read URL-dark as one fleet read on the fleet budget: with the request
// budget spent, they compute, publish and count the verdict, and no read fails.
func TestUrlDarkFleetCohortPassesReadOnTheFleetBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		scopedTimeout := providerUrlDarkScopedTimeout
		resetLast := func() {
			providerUrlDarkLast.Lock()
			providerUrlDarkLast.honored, providerUrlDarkLast.computedAt, providerUrlDarkLast.failedAt = nil, time.Time{}, time.Time{}
			providerUrlDarkLast.Unlock()
		}
		resetLast()
		defer func() {
			providerUrlDarkScopedTimeout = scopedTimeout
			resetLast()
		}()

		fleet := urlDarkTestFleet(ctx, t, city, 10)
		dark := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestDark(ctx, dark.clientId, 3, now.Add(-70*time.Minute), now.Add(-50*time.Minute), now.Add(-30*time.Minute))
		candidateIds := []server.Id{dark.clientId}
		for _, provider := range fleet {
			candidateIds = append(candidateIds, provider.clientId)
		}

		// a scoped read would now outlast its budget and fail open to no set
		providerUrlDarkScopedTimeout = time.Nanosecond
		egressTestPasses(ctx, t)
		connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts[ProviderExcludedUrlDark], int64(1))
		providerUrlDarkLast.Lock()
		computed, failed := !providerUrlDarkLast.computedAt.IsZero(), !providerUrlDarkLast.failedAt.IsZero()
		providerUrlDarkLast.Unlock()
		connect.AssertEqual(t, computed, true)
		connect.AssertEqual(t, failed, false)
		warm, err := getProviderHardExclusions(ctx, candidateIds)
		connect.AssertEqual(t, err, nil)
		if want := map[server.Id]bool{dark.clientId: true}; !maps.Equal(warm, want) {
			t.Fatalf("published snapshot excluded %v, want %v", warm, want)
		}
	})
}

// The floor honors the longest-standing verdicts by last answer, a key a
// dark provider's paced bursts do not move: a provider dark for the whole
// horizon that just had a fresh burst outranks one that answered half a day
// ago and went dark two hours ago.
func TestUrlDarkServingFloorKeyIsStable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 9)
		// created first so a tie on the key would favor it by client id
		recent := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestTurn(ctx, recent.clientId, now.Add(-12*time.Hour), providerUrlDarkSuccess)
		urlDarkTestDark(ctx, recent.clientId, 3, now.Add(-2*time.Hour), now.Add(-119*time.Minute), now.Add(-118*time.Minute))
		longDark := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		first := now.Add(-20 * time.Hour)
		urlDarkTestDark(ctx, longDark.clientId, 3, first, first.Add(time.Minute), first.Add(2*time.Minute))
		urlDarkTestDark(ctx, longDark.clientId, 3, now.Add(-20*time.Minute), now.Add(-19*time.Minute), now.Add(-18*time.Minute))

		// population 11, floor 10: one verdict stands
		urlDarkTestAssert(ctx, t, city, 11, append(fleet, recent, longDark), longDark)
	})
}

// Which turns count and where the windows sit: a turn that was not
// cycle-admitted, or is of another policy, is not counted; a control 29
// minutes before still counts; the success lookback runs from the newest of
// the K failures; and a provider's own success never vouches for it.
func TestUrlDarkCountedTurnsAndWindows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		version := SelectedProviderUrlProbePolicyVersion()
		// each case sits hours from the others, so no window reaches another's
		threeTurns := func(first time.Time) []time.Time {
			return []time.Time{first, first.Add(10 * time.Minute), first.Add(20 * time.Minute)}
		}

		notAdmitted := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		otherPolicy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		for i, provider := range []*egressTestProvider{notAdmitted, otherPolicy} {
			for _, at := range threeTurns(now.Add(-23*time.Hour + time.Duration(i)*2*time.Hour)) {
				urlDarkTestControls(ctx, at, 3, time.Minute)
				evidence := urlDarkTestNoResponseEvidence(at)
				if provider == notAdmitted {
					urlDarkTestRowAs(ctx, provider.clientId, at, 0, evidence, false, version)
				} else {
					urlDarkTestRowAs(ctx, provider.clientId, at, 0, evidence, true, version+1)
				}
			}
		}

		lateControls := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		for _, at := range threeTurns(now.Add(-19 * time.Hour)) {
			urlDarkTestControls(ctx, at, 3, 29*time.Minute)
			urlDarkTestTurn(ctx, lateControls.clientId, at, providerUrlDarkNoResponse)
		}

		// failures an hour apart; a success 6h30m before the first is more
		// than 8h before the newest
		anchored := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		first := now.Add(-15 * time.Hour)
		urlDarkTestTurn(ctx, anchored.clientId, first.Add(-6*time.Hour-30*time.Minute), providerUrlDarkSuccess)
		urlDarkTestDark(ctx, anchored.clientId, 3, first, first.Add(time.Hour), first.Add(2*time.Hour))

		// the first turn has two other controls and the provider's own
		// success: excused, so only two turns count
		ownSuccess := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		first = now.Add(-11 * time.Hour)
		urlDarkTestControls(ctx, first, 2, time.Minute)
		urlDarkTestTurn(ctx, ownSuccess.clientId, first.Add(-2*time.Minute), providerUrlDarkSuccess)
		urlDarkTestTurn(ctx, ownSuccess.clientId, first, providerUrlDarkNoResponse)
		urlDarkTestDark(ctx, ownSuccess.clientId, 3, first.Add(8*time.Hour), first.Add(8*time.Hour+10*time.Minute))

		urlDarkTestAssert(ctx, t, city, 15, append(fleet, notAdmitted, otherPolicy, lateControls, anchored, ownSuccess), lateControls, anchored)
	})
}

// Guards where the URL-dark load sits: in newProviderCommonFilterForClients.
// The score export (UpdateClientScores) builds its filter with that
// constructor directly, and leaves out of the published pools every provider
// the filter calls a hard egress failure. Were the load only in
// newProviderCountFilterForClients, which wraps it for the other consumers,
// the export would publish a URL-dark provider and no other test would
// notice: they assert through the hard-exclusion read, the counts, the status
// and the inspection, and a request refuses the provider on its own
// hard-exclusion read.
//
// So: a URL-dark provider is in no published score pool of its city, while
// the healthy providers connected exactly like it are. Same fixture as
// TestUrlDarkExcludedEverywhere.
func TestUrlDarkExcludedFromScoreExport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := egressTestCity(ctx, "Dark Test City", "Example Region", "Exampleland", "zz")
		now := server.NowUtc()
		fleet := urlDarkTestFleet(ctx, t, city, 10)
		dark := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		urlDarkTestDark(ctx, dark.clientId, 3, now.Add(-50*time.Minute), now.Add(-40*time.Minute), now.Add(-30*time.Minute))
		egressTestPasses(ctx, t)

		// control: the verdict exists, so a pool without the provider is the
		// export honoring it
		connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts[ProviderExcludedUrlDark], int64(1))
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			for _, forceMinimum := range []bool{false, true} {
				scores := egressTestCachedScores(ctx, t, city, rankMode, forceMinimum)
				healthy := 0
				for _, provider := range fleet {
					if scores[provider.clientId] != nil {
						healthy++
					}
				}
				t.Logf("mode=%s force_minimum=%t: pool=%d healthy=%d/%d dark=%t",
					rankMode, forceMinimum, len(scores), healthy, len(fleet), scores[dark.clientId] != nil)
				// control: a pool without the healthy providers would say
				// nothing about the dark one
				if healthy == 0 {
					t.Errorf("mode=%s force_minimum=%t: no healthy provider is in the published score pool", rankMode, forceMinimum)
				}
				if scores[dark.clientId] != nil {
					t.Errorf("mode=%s force_minimum=%t: the URL-dark provider is in the published score pool", rankMode, forceMinimum)
				}
			}
		}
	})
}
