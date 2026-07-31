package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"sync"
)

// A deployment-wide budget on the bytes active bandwidth probing is allowed
// to spend, counted across all providers together -- this does not
// distinguish which provider is being probed, only how much total probe
// traffic has been admitted. Active probing pulls real data through a
// provider's tunnel, which is a real paid contract: a zero-cost balance code
// does not make it free, because the payout planner sums paid and unpaid
// traffic identically before computing payouts
// (account_payment_model_plan.go). This is therefore a spend limit
// unconditionally, on every deployment, not only where payouts happen to be
// live.
//
// The budget is split into fixed, non-overlapping hourly buckets (UTC),
// rather than a rolling trailing window: a reservation is made against the
// earliest bucket -- starting with the current hour -- that has room for it.
// If the current hour is full, the probe is deferred to the next hour instead
// of rejected, and so on, up to MaxProviderBandwidthLookaheadBuckets hours
// out. Only once no bucket in that whole lookahead window has room -- meaning
// the deployment-wide daily budget (MaxProviderBandwidthBytesPerDay) is
// genuinely exhausted -- is the probe rejected. Fixed buckets (rather than a
// rolling window) are what make "defer to the next hour" well-defined: there
// needs to be a discrete boundary to wait for.
//
// This intentionally does NOT apply to the passive bandwidth signal, which is
// aggregated from already-settled transfer_escrow bytes and costs nothing
// additional -- there is no spend to budget there.
const ProviderBandwidthBucketDuration = time.Hour
const MaxProviderBandwidthLookaheadBuckets = 24

// activeBandwidthProbesPerBucket is the per-hour reservation budget. It is
// CAPACITY, and capacity is a property of the deployment rather than of this
// code, so it is configurable: beta runs on a 4-core box with a 40-provider
// fleet, mainnet has ~100k providers and must measure its own limits and set
// its own number. That is different from a behavioural knob like the sampling
// rate, which stays one value everywhere so both deployments exercise the same
// path.
//
// The default is deliberately the conservative one, so an environment with no
// config file cannot accidentally spend more than a small deployment can
// afford.
//
// HOW TO DERIVE IT, using the measurements taken on beta 2026-07-31:
//
//	Each provider costs TWO reservations, not one -- the operator target and
//	the cdn target are measured separately and never averaged.
//
//	A 40-provider sweep at 5 MiB per target moves:
//	  operator: 200 MiB out (the api serves it) + 200 MiB back in (the
//	            provider relays it through connect) -- it crosses the uplink
//	            TWICE
//	  cdn:      200 MiB in only (cloudflare serves it, we only relay)
//	  total:    600 MiB per sweep
//
//	Measured beta headroom under exactly that load:
//	  uplink    240-324 MB/s down, 28-120 MB/s up -> the sweep used 0.17%
//	  cpu       connect 5.4% idle -> 50% peak of ONE core, on a 4-core box
//	  memory    available flat at ~2.3 GB; connect RSS +25 MiB across the pass
//
//	So on beta NO hardware resource binds; the budget is what binds. The right
//	value is therefore set by coverage, not by capacity: 2 reservations x 40
//	providers = 80 to sweep the fleet in one hour, and 100 leaves room to grow
//	to 50 providers before anyone is skipped.
//
// Revisit against real data; this is tuned, not structural.
const defaultActiveBandwidthProbesPerBucket = 40

var activeBandwidthProbesPerBucket = sync.OnceValue(func() int {
	// OPTIONAL, exactly like pro.yml: an environment without the file must not
	// fail to boot, it must fall back to the conservative default.
	resource, err := server.Config.SimpleResource("provider_bandwidth.yml")
	if err != nil {
		glog.Infof("[bwq]provider_bandwidth.yml not present; using the default budget of %d probes/hour\n", defaultActiveBandwidthProbesPerBucket)
		return defaultActiveBandwidthProbesPerBucket
	}
	var y struct {
		ProbesPerBucket int `yaml:"probes_per_bucket"`
	}
	resource.UnmarshalYaml(&y)
	if y.ProbesPerBucket <= 0 {
		glog.Errorf("[bwq]provider_bandwidth.yml has probes_per_bucket=%d, which is not usable; using the default %d\n", y.ProbesPerBucket, defaultActiveBandwidthProbesPerBucket)
		return defaultActiveBandwidthProbesPerBucket
	}
	glog.Infof("[bwq]active bandwidth budget: %d probes/hour from provider_bandwidth.yml\n", y.ProbesPerBucket)
	return y.ProbesPerBucket
})

// MaxProviderBandwidthBytesPerBucket and PerDay are functions rather than
// consts because the budget is now configurable.
func MaxProviderBandwidthBytesPerBucket() int64 {
	return int64(activeBandwidthProbesPerBucket()) * MaxProviderBandwidthBytesPerProbe
}

func MaxProviderBandwidthBytesPerDay() int64 {
	return MaxProviderBandwidthLookaheadBuckets * MaxProviderBandwidthBytesPerBucket()
}

// Explicitly int64: a byte budget is not a row count, and callers pass an
// int64 byteCount.
const MaxProviderBandwidthBytesPerProbe int64 = 5 * 1024 * 1024

func maxProviderBandwidthError() error {
	return fmt.Errorf(
		"The active bandwidth probe budget (%d bytes per hour, %d bytes per day) has been reached for this deployment. Please try again later.",
		MaxProviderBandwidthBytesPerBucket(),
		MaxProviderBandwidthBytesPerDay(),
	)
}

// providerBandwidthBucketStart truncates t to the start of its UTC hourly
// bucket. Truncate operates on the absolute duration since the zero time,
// so this only aligns to true UTC hour boundaries when t is already UTC.
func providerBandwidthBucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(ProviderBandwidthBucketDuration)
}

// ReserveProviderBandwidthSlot finds the earliest hourly bucket -- starting
// with the current hour -- that has room for `byteCount` more probe bytes,
// and reserves them there. It returns the reservation's id (for
// CancelProviderBandwidthReservation, if the caller can't ultimately use the
// budget it just reserved -- e.g. the provider went offline between reserving
// and dialing) and the bucket's start time, which the caller should use as
// the probe's RunAt when the bucket isn't the current hour. A probe deferred
// to a future hour should have its RunAt jittered across that hour rather
// than firing at the hour's top, the same way RemoveNetworkClients spreads
// its deferred bulk deletes -- otherwise every probe pushed into the same
// future bucket becomes eligible at the identical instant.
//
// If no bucket within MaxProviderBandwidthLookaheadBuckets hours has room,
// nothing is reserved and an error is returned -- this is the deployment's
// daily cap. A single probe can never itself be too large to fit an empty
// bucket (one probe is MaxProviderBandwidthBytesPerProbe, a fortieth of a
// bucket), so this only happens under genuine contention from other probes.
//
// Concurrency: this is a plain read-then-insert in one transaction, with no
// row locking. `SELECT ... FOR UPDATE` would not help here -- the thing being
// contended is a bucket's *aggregate*, and an empty or partly-filled bucket
// has no single row to lock; serializing reservations properly would need a
// materialized per-bucket row or an advisory lock, which this ledger shape
// deliberately doesn't have. Under the default RepeatableRead isolation two
// concurrent reservations can therefore both read the same bucket total and
// both insert (their rows are distinct, so there's no write-write conflict
// and no serialization failure), overshooting the ceiling by at most
// (concurrent reservers x their byte counts). That is acceptable: this is a
// coarse deployment-wide spend cap, not an accounting invariant, and probe
// reservations arrive at a low rate from a small number of prober processes.
func ReserveProviderBandwidthSlot(
	ctx context.Context,
	clientId server.Id,
	byteCount int64,
) (reservationId server.Id, bucketStart time.Time, err error) {
	windowStart := providerBandwidthBucketStart(server.NowUtc())
	windowEnd := windowStart.Add(MaxProviderBandwidthLookaheadBuckets * ProviderBandwidthBucketDuration)

	server.Tx(ctx, func(tx server.PgTx) {
		usedByBucket := map[time.Time]int64{}
		result, qerr := tx.Query(
			ctx,
			`
			SELECT bucket_start, SUM(byte_count) FROM provider_bandwidth_quota
			WHERE $1 <= bucket_start AND bucket_start < $2
			GROUP BY bucket_start
			`,
			windowStart, windowEnd,
		)
		server.WithPgResult(result, qerr, func() {
			for result.Next() {
				var bucket time.Time
				var used int64
				server.Raise(result.Scan(&bucket, &used))
				usedByBucket[bucket] = used
			}
		})

		for i := 0; i < MaxProviderBandwidthLookaheadBuckets; i++ {
			candidate := windowStart.Add(time.Duration(i) * ProviderBandwidthBucketDuration)
			if usedByBucket[candidate]+byteCount <= MaxProviderBandwidthBytesPerBucket() {
				id := server.NewId()
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO provider_bandwidth_quota (provider_bandwidth_quota_id, client_id, byte_count, bucket_start, create_time)
					VALUES ($1, $2, $3, $4, $5)
					`,
					id, clientId, byteCount, candidate, server.NowUtc(),
				))
				reservationId = id
				bucketStart = candidate
				return
			}
		}
		err = maxProviderBandwidthError()
	})
	return reservationId, bucketStart, err
}

// CancelProviderBandwidthReservation releases a reservation made by
// ReserveProviderBandwidthSlot that the caller ultimately couldn't use -- e.g.
// the provider turned out to be unreachable, discovered only after the budget
// was reserved (the target bucket has to be known before the probe can be
// scheduled, so reservation necessarily happens before that check).
func CancelProviderBandwidthReservation(ctx context.Context, reservationId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM provider_bandwidth_quota WHERE provider_bandwidth_quota_id = $1`,
			reservationId,
		))
	})
}

// RemoveExpiredProviderBandwidthQuota deletes quota ledger rows whose bucket
// is entirely in the past relative to minBucketStart. Callers should pass a
// cutoff safely behind the lookahead window (e.g. now minus a couple of
// days), since a bucket up to MaxProviderBandwidthLookaheadBuckets hours in
// the future can still be actively reserved against.
func RemoveExpiredProviderBandwidthQuota(ctx context.Context, minBucketStart time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM provider_bandwidth_quota WHERE bucket_start < $1`,
			minBucketStart.UTC(),
		))
	})
}
