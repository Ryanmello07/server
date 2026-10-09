// URL-dark: a hard exclusion for providers whose recent URL turns carried
// nothing back at all. It is computed from accepted URL history at each
// publication, with no stored state, so a beta-carried patch adds no migration.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// Breaker layer 1, positive control: a no-response turn at t counts only when
// at least this many other providers had a counted success in (t-window, t].
// Otherwise the prober, not the provider, is the likelier fault, and the turn
// is ignored entirely: it neither extends nor resets a streak.
const providerUrlDarkControlWindow = 30 * time.Minute
const providerUrlDarkControlProviders = 3

// The shortest run of failures, first to last, that is a verdict: the rules'
// DarkMinimumSpan, but never shorter than the control window. That floor is
// what closes the outage onset. In the first control window of a prober-side
// outage the control still sees successes from before it, so its failures
// cannot be excused yet; a run that spans at least the window cannot fit
// inside that blind interval, so it cannot complete from outage failures alone.
func providerUrlDarkMinimumSpan(rules ProviderEgressRules) time.Duration {
	return max(rules.DarkMinimumSpan(), providerUrlDarkControlWindow)
}

// Only counted turns this recent are walked. The prober paces a dark provider
// in bursts about four hours apart (providerUrlProbeRunWindowSql), so several
// always fall inside; one it has not reached in a day is not judged, which
// fails toward serving. It also bounds the walk when every turn is excused.
const providerUrlDarkHorizon = 24 * time.Hour

// Budgets for one URL-dark read on the publication and request paths. Past
// one the read fails open (loadProviderUrlDarkHonored). Vars for tests.
var providerUrlDarkFleetTimeout = 15 * time.Second
var providerUrlDarkScopedTimeout = 5 * time.Second

// After a read fails, this process serves its last honored set without
// reading again for a while, so a slow outage walk does not run every pass.
var providerUrlDarkRetryAfter = time.Minute

// Breaker layer 2, serving floor: verdicts are released, newest first, until
// at least max(10, ceil(10%)) of the population remains.
const providerUrlDarkMinimumServing = 10
const providerUrlDarkMinimumServingPercent = 10

// How one counted URL turn bears on darkness.
type providerUrlDarkTurn int

const (
	providerUrlDarkSuccess providerUrlDarkTurn = iota
	// failed, but something came back (a status, a first byte or a TLS
	// event), or the evidence cannot prove nothing did
	providerUrlDarkForwarded
	// failed with provably nothing back from the exit
	providerUrlDarkNoResponse
)

// The prober's own budget ended before the load's first attempt (retry.go): a
// failure on the run, but not a provider that answered nothing.
const providerUrlDarkRunEndedStage = "run_ended"

// The Go statement of providerUrlDarkNoResponseSql, which is what serves. Nil
// evidence is forwarded on purpose: it cannot prove no-response, so it must
// never push a provider toward darkness.
func classifyProviderUrlDarkTurn(okCount int, evidence *egresshealth.UrlProbeEvidence) providerUrlDarkTurn {
	switch {
	case okCount == 1:
		return providerUrlDarkSuccess
	case evidence == nil || evidence.FirstByteReceived || evidence.StatusCode != 0 || len(evidence.Security) > 0 ||
		evidence.FailureStage == providerUrlDarkRunEndedStage:
		return providerUrlDarkForwarded
	default:
		return providerUrlDarkNoResponse
	}
}

// A counted turn at alias that provably received nothing. Each key must be
// present with its exact empty value; a missing key or NULL/JSON-null
// evidence is forwarded, and so is a run_ended load. COALESCE keeps it a
// strict boolean, because a NULL would be skipped by bool_and and could not
// break a streak.
func providerUrlDarkNoResponseSql(alias string) string {
	return fmt.Sprintf(`COALESCE(%[1]s.ok_count = 0
		AND %[1]s.url_probe_evidence->'first_byte_received' = 'false'::jsonb
		AND %[1]s.url_probe_evidence->'status_code' = '0'::jsonb
		AND COALESCE(%[1]s.url_probe_evidence->'security', 'null'::jsonb) IN ('null'::jsonb, '[]'::jsonb)
		AND COALESCE(%[1]s.url_probe_evidence->>'failure_stage', '') <> '%[2]s', false)`, alias, providerUrlDarkRunEndedStage)
}

// Counted URL turns: accepted cycle-admitted single-URL receipts of the
// selected policy. The literal version lets the planner use the partial
// url_run/url_success indexes, as providerEgressHealthWindowSql does.
func providerUrlDarkCountedSql(alias string, version int) string {
	return fmt.Sprintf(`%[1]s.url_probe AND %[1]s.url_probe_policy_version = %[2]d AND %[1]s.total_count = 1
		AND (%[1]s.ok_count = 0 OR %[1]s.ok_count = 1)`, alias, version)
}

// One row per call: the population's size, then each dark member (NULL when
// none) with its last answer within the horizon (NULL when none).
//
// A member's walk starts after its newest counted turn within the horizon
// that is not no-response (answered: a success or a forwarded turn, never
// excused), so a provider that answered last costs one index probe, and
// every turn walked is no-response. A member whose latest counted success is
// within ProviderEgressHealthMaxAge of its newest counted turn is not walked:
// the success clause already clears it for any K turns, and this keeps a
// prober outage shorter than that from walking every provider's excused
// turns. The population is the public
// count's (CountProviderEgress) less providers another hard exclusion already
// removes (providerEgressEligibilitySql, the rules providerHardExclusionsSql
// negates): they serve no one either way, so they must not count toward the
// floor or spend its budget, and their verdicts return statelessly once that
// exclusion clears. With clientScoped only members in $6 are walked; the
// count stays fleet.
//
// A member is dark when its K most recent non-excused counted turns are all
// no-response and it has no counted success within ProviderEgressHealthMaxAge
// of the newest of them, and those K span at least providerUrlDarkMinimumSpan
// from first to last. Any later success or forwarded turn breaks the run.
//
// $1 public provide mode, $2 K, $3 control window seconds, $4 success
// lookback seconds, $5 horizon start, $6 client ids when scoped.
func providerUrlDarkSql(version int, clientScoped bool, minimumSpan time.Duration) string {
	scope := ""
	if clientScoped {
		scope = "AND population.client_id = ANY($6)"
	}
	return fmt.Sprintf(`
	WITH population AS MATERIALIZED (
		SELECT provider_location.client_id
		FROM network_client_location_reliability AS provider_location
		INNER JOIN network_client ON network_client.client_id = provider_location.client_id
		WHERE network_client.active = true AND network_client.source_client_id IS NULL
		AND provider_location.connected = true AND provider_location.valid = true
		AND EXISTS (
			SELECT 1 FROM provide_key
			WHERE provide_key.client_id = provider_location.client_id AND provide_key.provide_mode = $1
		)
		AND %[7]s
	), member AS MATERIALIZED (
		SELECT population.client_id, answered.measured_at AS answered_at, answered.run_id AS answered_run_id
		FROM population
		LEFT JOIN LATERAL (
			SELECT answered.measured_at, answered.run_id
			FROM provider_egress_health_history AS answered
			WHERE answered.client_id = population.client_id AND %[8]s
			AND answered.measured_at > $5 AND NOT %[2]s
			ORDER BY answered.measured_at DESC, answered.run_id DESC
			LIMIT 1
		) AS answered ON true
		WHERE NOT EXISTS (
			SELECT 1 FROM provider_egress_health_history AS success
			WHERE success.client_id = population.client_id AND %[6]s AND success.ok_count = 1
			AND success.measured_at > (
				SELECT max(newest.measured_at) FROM provider_egress_health_history AS newest
				WHERE newest.client_id = population.client_id AND %[9]s
			) - make_interval(secs => $4)
		)
		%[5]s
	), recent AS MATERIALIZED (
		SELECT member.client_id, member.answered_at, turn.measured_at
		FROM member
		CROSS JOIN LATERAL (
			SELECT turn.measured_at, turn.run_id
			FROM provider_egress_health_history AS turn
			WHERE turn.client_id = member.client_id AND %[3]s
			AND turn.measured_at > $5
			AND (member.answered_at IS NULL OR (turn.measured_at, turn.run_id) > (member.answered_at, member.answered_run_id))
			AND %[4]s
			ORDER BY turn.measured_at DESC, turn.run_id DESC
			LIMIT $2
		) AS turn
	), dark AS (
		SELECT recent.client_id, recent.answered_at
		FROM recent
		GROUP BY recent.client_id, recent.answered_at
		HAVING count(*) = $2
		AND max(recent.measured_at) - min(recent.measured_at) >= make_interval(secs => %[10]d)
		AND NOT EXISTS (
			SELECT 1 FROM provider_egress_health_history AS success
			WHERE success.client_id = recent.client_id AND %[6]s AND success.ok_count = 1
			AND success.measured_at > max(recent.measured_at) - make_interval(secs => $4)
		)
	)
	SELECT population_count.count, dark.client_id, dark.answered_at
	FROM (SELECT count(*) AS count FROM population) AS population_count
	LEFT JOIN dark ON true
	`,
		version,
		providerUrlDarkNoResponseSql("answered"),
		providerUrlDarkCountedSql("turn", version),
		providerUrlDarkControlSql(version),
		scope,
		providerUrlDarkCountedSql("success", version),
		providerEgressEligibilitySql("provider_location"),
		providerUrlDarkCountedSql("answered", version),
		providerUrlDarkCountedSql("newest", version),
		int64(minimumSpan/time.Second),
	)
}

// Breaker layer 1 for the no-response turn at alias turn, as P probes: each
// takes the newest counted success in the window from a client not yet seen
// (the turn's own included). A probe walks the (measured_at) index newest
// first and stops at its first match, so a window full of successes costs P
// index steps, where a count(DISTINCT) reads the whole window.
func providerUrlDarkControlSql(version int) string {
	seen := []string{"turn.client_id"}
	probes := []string{}
	for i := range providerUrlDarkControlProviders {
		alias := fmt.Sprintf("control_%d", i)
		probes = append(probes, fmt.Sprintf(`CROSS JOIN LATERAL (
				SELECT control.client_id
				FROM provider_egress_health_history AS control
				WHERE control.measured_at > turn.measured_at - make_interval(secs => $3)
				AND control.measured_at <= turn.measured_at
				AND control.client_id <> ALL(ARRAY[%s])
				AND %s AND control.ok_count = 1
				ORDER BY control.measured_at DESC
				LIMIT 1
			) AS %s`, strings.Join(seen, ", "), providerUrlDarkCountedSql("control", version), alias))
		seen = append(seen, alias+".client_id")
	}
	return "EXISTS (\n\t\t\tSELECT 1 FROM (SELECT) AS start\n\t\t\t" + strings.Join(probes, "\n\t\t\t") + "\n\t\t)"
}

// A dark member of the population, with its last answer (a success or a
// forwarded turn) within the horizon, or nil when there is none.
type providerUrlDarkVerdict struct {
	clientId   server.Id
	answeredAt *time.Time
}

// The fewest providers URL-dark may leave serving: max(10, ceil(10%)).
func providerUrlDarkServingFloor(populationCount int) int {
	return max(providerUrlDarkMinimumServing, (populationCount*providerUrlDarkMinimumServingPercent+99)/100)
}

// Breaker layer 2. Honors every verdict while the floor holds; otherwise only
// the longest standing, as many as keep the floor: by last answer within the
// horizon (none first), then client id. That key holds still while a provider
// stays dark, where the oldest of its K most recent failures moves with each
// paced burst, so the honored set does not churn between passes, and a wave
// of providers that answered just before it is released first. A population
// at or below the floor honors none.
func honorProviderUrlDarkVerdicts(populationCount int, verdicts []providerUrlDarkVerdict) map[server.Id]bool {
	honored := map[server.Id]bool{}
	allowed := max(0, populationCount-providerUrlDarkServingFloor(populationCount))
	if len(verdicts) > allowed {
		verdicts = append([]providerUrlDarkVerdict(nil), verdicts...)
		sort.Slice(verdicts, func(i, j int) bool {
			a, b := verdicts[i].answeredAt, verdicts[j].answeredAt
			if (a == nil) != (b == nil) {
				return a == nil
			}
			if a != nil && !a.Equal(*b) {
				return a.Before(*b)
			}
			return bytes.Compare(verdicts[i].clientId[:], verdicts[j].clientId[:]) < 0
		})
		glog.Warningf(
			"[egress]url_dark serving floor: population=%d dark=%d floor=%d honored=%d released=%d\n",
			populationCount, len(verdicts), providerUrlDarkServingFloor(populationCount), allowed, len(verdicts)-allowed,
		)
		verdicts = verdicts[:allowed]
	}
	for _, verdict := range verdicts {
		honored[verdict.clientId] = true
	}
	return honored
}

// Reads the population size and its dark members, or with clientIds only
// those members (the size is still the fleet's).
func loadProviderUrlDarkVerdicts(ctx context.Context, clientIds []server.Id) (populationCount int, verdicts []providerUrlDarkVerdict, returnErr error) {
	rules := GetProviderEgressRules()
	query := providerUrlDarkSql(SelectedProviderUrlProbePolicyVersion(), clientIds != nil, providerUrlDarkMinimumSpan(rules))
	args := []any{
		ProvideModePublic,
		int64(rules.DarkConsecutiveFailures),
		providerUrlDarkControlWindow.Seconds(),
		ProviderEgressHealthMaxAge.Seconds(),
		server.NowUtc().Add(-providerUrlDarkHorizon),
	}
	if clientIds != nil {
		args = append(args, clientIds)
	}
	// A safety decision: read the primary, as the hard-exclusion read-through
	// does. server.Db raises a connection or deadline failure; recover it so
	// the caller can fail open.
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, query, args...)
			if err != nil {
				returnErr = err
				return
			}
			defer rows.Close()
			for rows.Next() {
				var count int64
				var clientId *server.Id
				var answeredAt *time.Time
				if err := rows.Scan(&count, &clientId, &answeredAt); err != nil {
					returnErr = err
					return
				}
				populationCount = int(count)
				if clientId != nil {
					verdicts = append(verdicts, providerUrlDarkVerdict{clientId: *clientId, answeredAt: answeredAt})
				}
			}
			returnErr = rows.Err()
		}, server.OptReadOnly(), server.OptNoRetry())
	}, func(err error) {
		returnErr = err
	})
	return
}

var errProviderUrlDarkRetryAfter = errors.New("a recent url_dark read failed")

// The last fleet honored set this process computed, and when a read last
// failed: the fail-open fallback.
var providerUrlDarkLast struct {
	sync.Mutex
	honored    map[server.Id]bool
	computedAt time.Time
	failedAt   time.Time
}

func readProviderUrlDarkHonored(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
	providerUrlDarkLast.Lock()
	failedAt := providerUrlDarkLast.failedAt
	providerUrlDarkLast.Unlock()
	if time.Since(failedAt) < providerUrlDarkRetryAfter {
		return nil, errProviderUrlDarkRetryAfter
	}
	timeout := providerUrlDarkFleetTimeout
	if clientIds != nil {
		timeout = providerUrlDarkScopedTimeout
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if clientIds != nil {
		_, verdicts, err := loadProviderUrlDarkVerdicts(readCtx, clientIds)
		if err != nil {
			return nil, err
		}
		if len(verdicts) == 0 {
			return map[server.Id]bool{}, nil
		}
	}
	populationCount, verdicts, err := loadProviderUrlDarkVerdicts(readCtx, nil)
	if err != nil {
		return nil, err
	}
	honored := honorProviderUrlDarkVerdicts(populationCount, verdicts)
	providerUrlDarkLast.Lock()
	providerUrlDarkLast.honored = honored
	providerUrlDarkLast.computedAt = time.Now()
	providerUrlDarkLast.Unlock()
	return honored, nil
}

// Marks the context of a pass whose cohort is the whole serving population:
// the publication passes (UpdateClientLocations, UpdateClientScores) and the
// dashboard count (CountProviderEgress). They load their evidence for the
// cohort they captured, and their URL-dark read stays the one fleet read on
// the fleet budget, restricted to that cohort. The scoped read would walk the
// cohort and then the fleet again inside the request budget, and would renew
// the fail-open set only while a cohort member is dark.
type providerUrlDarkFleetCohortKey struct{}

func withProviderUrlDarkFleetCohort(ctx context.Context) context.Context {
	return context.WithValue(ctx, providerUrlDarkFleetCohortKey{}, true)
}

// The clients whose verdicts one call reads: nil, the fleet, for a fleet call
// and for any call of a fleet-cohort pass.
func providerUrlDarkReadClientIds(ctx context.Context, clientIds []server.Id) []server.Id {
	if ctx.Value(providerUrlDarkFleetCohortKey{}) != nil {
		return nil
	}
	return clientIds
}

// The honored URL-dark set: the single source every consumer reads. With
// clientIds it is the fleet set restricted to them. Honored verdicts are a
// subset of dark ones, so the fleet read runs only when a requested client is
// dark at all. A fleet-cohort pass reads the fleet set whatever cohort it
// names (withProviderUrlDarkFleetCohort).
//
// Unlike the other hard exclusions it fails open, so a slow fleet walk can
// never stop publication or a request: if a read errs or outlasts its budget,
// the last set this process computed stands while younger than
// ProviderEgressHealthMaxAge, else none. Both breaker layers already lean
// toward serving; holding the last set keeps a seeded proxy out through a
// prober outage that makes the walk slow, which no successes could readmit,
// for up to that age. A longer outage, or a fresh process, serves them.
func loadProviderUrlDarkHonored(ctx context.Context, clientIds []server.Id) map[server.Id]bool {
	if clientIds != nil && len(clientIds) == 0 {
		return map[server.Id]bool{}
	}
	honored, err := readProviderUrlDarkHonored(ctx, providerUrlDarkReadClientIds(ctx, clientIds))
	if err != nil {
		providerUrlDarkLast.Lock()
		// a caller giving up is not a failed read
		if ctx.Err() == nil && !errors.Is(err, errProviderUrlDarkRetryAfter) {
			providerUrlDarkLast.failedAt = time.Now()
			glog.Errorf("[egress]url_dark read failed (%s); the last honored set from %s stands\n", err, providerUrlDarkLast.computedAt)
		}
		honored = map[server.Id]bool{}
		if time.Since(providerUrlDarkLast.computedAt) < ProviderEgressHealthMaxAge {
			honored = maps.Clone(providerUrlDarkLast.honored)
		}
		providerUrlDarkLast.Unlock()
	}
	if clientIds == nil {
		return honored
	}
	scoped := map[server.Id]bool{}
	for _, clientId := range clientIds {
		if honored[clientId] {
			scoped[clientId] = true
		}
	}
	return scoped
}
