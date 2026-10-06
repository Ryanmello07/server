// URL-dark: a hard exclusion for providers whose recent URL turns carried
// nothing back at all. It is computed from accepted URL history at each
// publication, with no stored state, so a beta-carried patch adds no migration.
package model

import (
	"bytes"
	"context"
	"fmt"
	"sort"
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

// Breaker layer 2, serving floor: verdicts are released, newest first, until
// at least max(10, ceil(10%)) of the published population remains.
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

// One row per call: the published population's size, then each dark member
// (NULL when none) with the oldest of its K failures. The population is the
// public count's (CountProviderEgress), so the floor and the verdicts share
// it. With clientScoped only members in $6 are walked; the count stays fleet.
//
// A member is dark when its K most recent non-excused counted turns are all
// no-response and it has no counted success within ProviderEgressHealthMaxAge
// of the newest of them. Any later success or forwarded turn breaks the run.
//
// $1 public provide mode, $2 K, $3 control window seconds, $4 control
// providers, $5 success lookback seconds, $6 client ids when scoped.
func providerUrlDarkSql(version int, clientScoped bool) string {
	scope := ""
	if clientScoped {
		scope = "WHERE population.client_id = ANY($6)"
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
	), recent AS MATERIALIZED (
		SELECT population.client_id, turn.measured_at, turn.no_response
		FROM population
		CROSS JOIN LATERAL (
			SELECT turn.measured_at, turn.run_id, %[2]s AS no_response
			FROM provider_egress_health_history AS turn
			WHERE turn.client_id = population.client_id AND %[3]s
			AND CASE WHEN %[2]s THEN (
				SELECT count(DISTINCT control.client_id)
				FROM provider_egress_health_history AS control
				WHERE control.measured_at > turn.measured_at - make_interval(secs => $3)
				AND control.measured_at <= turn.measured_at
				AND control.client_id <> turn.client_id
				AND %[4]s AND control.ok_count = 1
			) >= $4 ELSE true END
			ORDER BY turn.measured_at DESC, turn.run_id DESC
			LIMIT $2
		) AS turn
		%[5]s
	), dark AS (
		SELECT recent.client_id, min(recent.measured_at) AS oldest_failure_at
		FROM recent
		GROUP BY recent.client_id
		HAVING count(*) = $2 AND bool_and(recent.no_response)
		AND NOT EXISTS (
			SELECT 1 FROM provider_egress_health_history AS success
			WHERE success.client_id = recent.client_id AND %[6]s AND success.ok_count = 1
			AND success.measured_at > max(recent.measured_at) - make_interval(secs => $5)
		)
	)
	SELECT population_count.count, dark.client_id, dark.oldest_failure_at
	FROM (SELECT count(*) AS count FROM population) AS population_count
	LEFT JOIN dark ON true
	`,
		version,
		providerUrlDarkNoResponseSql("turn"),
		providerUrlDarkCountedSql("turn", version),
		providerUrlDarkCountedSql("control", version),
		scope,
		providerUrlDarkCountedSql("success", version),
	)
}

// A dark member of the published population.
type providerUrlDarkVerdict struct {
	clientId        server.Id
	oldestFailureAt time.Time
}

// The fewest providers URL-dark may leave serving: max(10, ceil(10%)).
func providerUrlDarkServingFloor(populationCount int) int {
	return max(providerUrlDarkMinimumServing, (populationCount*providerUrlDarkMinimumServingPercent+99)/100)
}

// Breaker layer 2. Honors every verdict while the floor holds; otherwise only
// the oldest (by first failure, then client id), as many as keep the floor. A
// wave the positive control missed then cannot empty the list, while verdicts
// that predate it stand. A population at or below the floor honors none.
func honorProviderUrlDarkVerdicts(populationCount int, verdicts []providerUrlDarkVerdict) map[server.Id]bool {
	honored := map[server.Id]bool{}
	allowed := max(0, populationCount-providerUrlDarkServingFloor(populationCount))
	if len(verdicts) > allowed {
		verdicts = append([]providerUrlDarkVerdict(nil), verdicts...)
		sort.Slice(verdicts, func(i, j int) bool {
			if !verdicts[i].oldestFailureAt.Equal(verdicts[j].oldestFailureAt) {
				return verdicts[i].oldestFailureAt.Before(verdicts[j].oldestFailureAt)
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
	query := providerUrlDarkSql(SelectedProviderUrlProbePolicyVersion(), clientIds != nil)
	args := []any{
		ProvideModePublic,
		int64(rules.DarkConsecutiveFailures),
		providerUrlDarkControlWindow.Seconds(),
		int64(providerUrlDarkControlProviders),
		ProviderEgressHealthMaxAge.Seconds(),
	}
	if clientIds != nil {
		args = append(args, clientIds)
	}
	// A safety decision: read the primary, as the hard-exclusion read-through does.
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
			var oldestFailureAt *time.Time
			if err := rows.Scan(&count, &clientId, &oldestFailureAt); err != nil {
				returnErr = err
				return
			}
			populationCount = int(count)
			if clientId != nil && oldestFailureAt != nil {
				verdicts = append(verdicts, providerUrlDarkVerdict{clientId: *clientId, oldestFailureAt: *oldestFailureAt})
			}
		}
		returnErr = rows.Err()
	}, server.OptReadOnly())
	return
}

// The honored URL-dark set: the single source every consumer reads. With
// clientIds it is the fleet set restricted to them. Honored verdicts are a
// subset of dark ones, so the fleet read runs only when a requested client is
// dark at all.
func loadProviderUrlDarkHonored(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
	if clientIds != nil {
		if len(clientIds) == 0 {
			return map[server.Id]bool{}, nil
		}
		_, verdicts, err := loadProviderUrlDarkVerdicts(ctx, clientIds)
		if err != nil {
			return nil, err
		}
		if len(verdicts) == 0 {
			return map[server.Id]bool{}, nil
		}
	}
	populationCount, verdicts, err := loadProviderUrlDarkVerdicts(ctx, nil)
	if err != nil {
		return nil, err
	}
	honored := honorProviderUrlDarkVerdicts(populationCount, verdicts)
	if clientIds == nil {
		return honored, nil
	}
	scoped := map[server.Id]bool{}
	for _, clientId := range clientIds {
		if honored[clientId] {
			scoped[clientId] = true
		}
	}
	return scoped, nil
}
