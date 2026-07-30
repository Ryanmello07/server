# Provider Probing P2 — Bandwidth Measurement and Verdict Data Model

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn raw geo-probe results into recorded judgements (verdict and failure taxonomy), and add a bandwidth signal that measures real throughput — passively from settled traffic first, actively only where passive history doesn't exist yet — all under a byte-spend budget that applies identically on beta and mainstream.

**Architecture:** Bandwidth has two independent sources feeding one stored figure per provider: a passive aggregate computed from already-settled `transfer_escrow` bytes (zero additional cost, cannot be gamed selectively), and a sampled active probe that shares the geo-probe's tunnel when a provider has no passive history yet. Both write through the same table, tagged by source, so consumers never have to know which produced a given row. A pure `probeverdict` package turns a submission into `verified` / `unverified` / `suspect` from consensus and stability alone -- an RTT-distance corroboration was designed and deliberately dropped (see Task 3) -- decoupled entirely from the transport that produced the submission. Active probing is rationed by an hourly byte-budget reservation that mirrors the existing bulk-delete quota exactly, because probe bytes are real paid contracts on any deployment where payouts are planned.

**Tech Stack:** Go 1.26.5, PostgreSQL 16, Redis (bucket reservations), the `connect` client library.

**Source spec:** `docs/superpowers/specs/2026-07-25-enforced-provider-geo-probing-design.md` — read the *Bandwidth measurement*, *Provider lifecycle*, and *Threat model* sections before starting.
**Predecessors:** P0 (`2026-07-26-provider-probing-p0-prerequisites.md`) and P1 (`2026-07-26-provider-probing-p1-automated-confined.md`), both complete and live-verified on beta.

## Global Constraints

- **One code path, one sampling-rate constant, both deployments.** Never branch on environment. If beta needs a different value for the sampling-rate constant to reach useful coverage at its provider count, that is a *value* chosen so it behaves sensibly at both scales — not a flag, not a second path. A knob only one environment ever exercises is how the `transportVersion < 2` gap went unnoticed.
- **A zero-cost balance code does not make active probing free.** `transfer_balance.paid` can be false, but the payout planner sums paid and unpaid traffic identically before computing payouts (`account_payment_model_plan.go`). The byte budget in Task 4 is a spend limit unconditionally, not only where payouts happen to be live.
- **Bandwidth results are advisory.** Nothing in this plan may alter `PassesMinimums`, scoring weights, or which providers `find-providers2` returns. A measured or missing bandwidth number must never gate selection — queue coverage will be partial for a long time, and an advisory number is what keeps that from becoming an outage.
- **The probed country diverging from the mmdb country is not suspicious.** That divergence is the entire point of the project. There is no RTT-based corroboration in this plan (designed and dropped, see Task 3) -- the only two verdict rules are absence of consensus and instability over time.
- **Migrations apply by slice index** (`for i := DbVersion(ctx); i < upTo; i++`) — always append, never edit or reorder an applied migration.
- **Two branches per server-side change:** a feature branch off `beta/self-contained-env` PR'd to `Ryanmello07/server`, plus a cherry-picked branch PR'd to `urnetwork/server` `main`. Check `git remote -v` before pushing in any checkout — remotes are inverted between `/tmp/sandbox/server` and `/root/urnetwork/server`.
- Server tests need the local stack (`local/run-local.sh --keep-up`). A whole-package `go test ./model` is blocked by a pre-existing missing `subsidy.yml`; say so explicitly rather than claiming it passed.
- **Never stop, recreate or `down` the live beta stack** (`docker-compose.beta.yml`) while implementing — it serves real traffic being probed by this very system.

---

## File Structure

| File | Responsibility | Change |
| --- | --- | --- |
| `model/provider_bandwidth_model.go` (server) | **New.** Passive aggregation + storage for both bandwidth sources | Create |
| `model/provider_bandwidth_model_test.go` | Tests | Create |
| `probeverdict/probeverdict.go` (server) | **New.** Pure verdict logic: consensus absence, staleness | Create |
| `probeverdict/probeverdict_test.go` | Table-driven tests | Create |
| `db_migrations.go` | Verdict columns + bandwidth table | Modify — appended migrations only |
| `model/provider_bandwidth_rate_limit.go` (server) | **New.** Hourly byte-budget reservation, mirrors bulk-delete | Create |
| `controller/provider_egress_location_controller.go` | Verdict computed on submit | Modify |
| `api/handlers/provider_bandwidth_handlers.go` (server) | **New.** Operator-secret-gated download endpoint for the active probe | Create |
| `api/api.go` | Route registration | Modify |
| `bandwidth/bandwidth.go` (operator-proxy) | **New.** Adaptive-size throughput measurement over a `*http.Client` | Create |
| `cmd/egress-prober/main.go` (operator-proxy) | Wire bandwidth into the pass loop, opportunistic tunnel sharing | Modify |

---

## Task 1: Passive bandwidth signal

**Repo:** `/root/urnetwork/server`. Two branches: `feat/provider-bandwidth-passive` and `-upstream`.

**Context:** `transfer_escrow` already records settled bytes per contract as a byproduct of billing. Deriving provider throughput from it costs nothing and cannot be gamed selectively — a provider cannot inflate real user traffic without actually being fast for real users.

**Files:**
- Create: `model/provider_bandwidth_model.go`, `model/provider_bandwidth_model_test.go`

**Interfaces:**
- Produces: `type ProviderBandwidth struct { ClientId server.Id; BytesPerSecond float64; Source string; SampleByteCount int64; WindowStart, WindowEnd time.Time }` — `Source` is `"passive"` or `"active"`, used later by Task 6.
- Produces: `ComputePassiveProviderBandwidth(ctx context.Context, clientId server.Id, window time.Duration) (*ProviderBandwidth, error)` — nil, nil when the provider has no settled bytes in the window (no history, not zero throughput).

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/server
git checkout beta/self-contained-env && git pull --ff-only origin beta/self-contained-env
git checkout -b feat/provider-bandwidth-passive
```

- [ ] **Step 2: Read before writing**

Read `contract_close` and `transfer_escrow`'s schema (`\d contract_close`, `\d transfer_escrow` against the live beta DB) and the query shapes in `model/account_payment_model_plan.go` around the `payout_byte_count` sums — that file already computes exactly this kind of aggregate for payout planning, so mirror its join shape rather than inventing a new one.

- [ ] **Step 3: Write the failing test**

Add to `model/provider_bandwidth_model_test.go`:

```go
func TestComputePassiveProviderBandwidthDerivesFromSettledBytes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destNetworkId := server.NewId()
		destId := server.NewId()
		Testing_CreateDevice(ctx, sourceNetworkId, server.NewId(), sourceId, "", "")
		Testing_CreateDevice(ctx, destNetworkId, server.NewId(), destId, "", "")

		// a contract that settled 32 MiB over exactly 10 seconds of wall time
		windowStart := server.NowUtc().Add(-1 * time.Hour)
		contractId := Testing_CreateSettledContract(ctx, sourceId, destId,
			windowStart, windowStart.Add(10*time.Second), 32*1024*1024)

		bw, err := ComputePassiveProviderBandwidth(ctx, destId, 2*time.Hour)
		connect.AssertEqual(t, err, nil)
		if bw == nil {
			t.Fatal("expected a passive bandwidth result, got nil")
		}
		connect.AssertEqual(t, bw.Source, "passive")
		// 32 MiB / 10s ~= 3355443 bytes/sec
		if bw.BytesPerSecond < 3_000_000 || 3_700_000 < bw.BytesPerSecond {
			t.Errorf("BytesPerSecond = %.0f, want ~3355443", bw.BytesPerSecond)
		}
		_ = contractId
	})
}

func TestComputePassiveProviderBandwidthNilWhenNoHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bw, err := ComputePassiveProviderBandwidth(ctx, server.NewId(), 2*time.Hour)
		connect.AssertEqual(t, err, nil)
		if bw != nil {
			t.Errorf("expected nil for a provider with no settled bytes, got %+v", bw)
		}
	})
}
```

If `Testing_CreateSettledContract` does not exist, write it as a small test helper in the same file that inserts directly into `transfer_contract` and `contract_close` with the party set to `"destination"` — read `controller/network_client_controller_test.go` for the existing pattern of hand-inserting contract rows in tests, and match it rather than inventing a different shape.

- [ ] **Step 4: Run it and confirm it fails**

```bash
cd /root/urnetwork/server && ./local/run-local.sh --keep-up
./test.sh -run TestComputePassiveProviderBandwidth
```

Expected: compile failure, `undefined: ComputePassiveProviderBandwidth`.

- [ ] **Step 5: Implement**

Query `contract_close` joined to `transfer_contract` where `destination_id = clientId`, `party = 'destination'`, `companion_contract_id IS NULL` (excluding return-traffic legs, which are not provider egress — see the session note on companion contracts), and `close_time` within the window. Sum `used_transfer_byte_count`; compute elapsed wall time as `max(close_time) - min(create_time)` across the matched rows, guarding against a zero or negative denominator (return nil rather than dividing by zero or a negative number).

Comment the companion exclusion:

```go
	// company_contract_id IS NULL excludes return-traffic legs: a client's
	// return traffic settles as a contract where the CLIENT is the
	// destination, which would otherwise be misread as that client acting as
	// a fast provider. See docs/superpowers/specs/2026-07-25-... "Threat
	// model" -- confirmed empirically: on beta, every non-Public-key
	// "earner" turned out to be exactly this.
```

- [ ] **Step 6: Run the tests and regressions**

```bash
./test.sh -run TestComputePassiveProviderBandwidth
go build ./... && go vet ./...
```

- [ ] **Step 7: Commit, push, open both PRs**

```bash
git add model/provider_bandwidth_model.go model/provider_bandwidth_model_test.go
git commit -m "feat(model): derive provider bandwidth from settled contract bytes

transfer_escrow already records real bytes delivered per contract as a
byproduct of billing. Deriving throughput from it costs nothing
additional and cannot be gamed selectively -- a provider cannot inflate
real user traffic without actually being fast for real users.

Excludes companion (return-traffic) contracts: a client's return leg
settles with the client as destination, which would otherwise misread
ordinary users as fast providers."
git push -u origin feat/provider-bandwidth-passive
gh pr create --repo Ryanmello07/server --base beta/self-contained-env \
  --head feat/provider-bandwidth-passive \
  --title "feat(model): derive provider bandwidth from settled contract bytes" \
  --body "Passive bandwidth signal from already-settled transfer_escrow bytes. Zero additional cost, cannot be gamed selectively. Excludes companion/return-traffic contracts."

git fetch upstream main
git checkout -b feat/provider-bandwidth-passive-upstream upstream/main
git cherry-pick feat/provider-bandwidth-passive
go build ./... && go vet ./...
git push -u origin feat/provider-bandwidth-passive-upstream
gh pr create --repo urnetwork/server --base main \
  --head Ryanmello07:feat/provider-bandwidth-passive-upstream \
  --title "feat(model): derive provider bandwidth from settled contract bytes" \
  --body "Passive bandwidth signal from already-settled transfer_escrow bytes."
```

---

## Task 2: Schema — bandwidth storage and verdict columns

**Repo:** `/root/urnetwork/server`. Branch from `feat/provider-bandwidth-passive` (this task depends on Task 1's types). Two branches: `feat/provider-bandwidth-schema` and `-upstream`.

**Files:**
- Modify: `db_migrations.go` — append only
- Modify: `model/provider_egress_location_model.go` — add verdict fields to the read/write path
- Modify: `model/provider_bandwidth_model.go` — add `StoreProviderBandwidth`

**Interfaces:**
- Consumes: `ProviderBandwidth` from Task 1.
- Produces: a `provider_bandwidth` table, and three new columns on `provider_egress_location`: `verdict varchar(16) NOT NULL DEFAULT 'unverified'`, `verdict_reason varchar(64) NOT NULL DEFAULT ''`, `assurance varchar(16) NOT NULL DEFAULT 'direct'`. (No `rtt_millis` or `intermediary_client_id`: the RTT check was designed and dropped — see Task 3 — and multi-hop's intermediary tracking is P3's concern. An unused column with no writer and no test is dead weight; add it when something actually writes it, not before.)

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/server
git checkout -q feat/provider-bandwidth-passive
git checkout -b feat/provider-bandwidth-schema
```

- [ ] **Step 2: Write the failing migration test**

Add to `model/network_client_location_model_test.go` (or a new `db_migrations_test.go` if one does not already cover schema shape — check first):

```go
func TestProviderBandwidthTableExists(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT to_regclass('provider_bandwidth')`)
			connect.AssertEqual(t, err, nil)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var name *string
					connect.AssertEqual(t, result.Scan(&name), nil)
					if name == nil {
						t.Fatal("provider_bandwidth table does not exist")
					}
				}
			})
		})
	})
}

func TestProviderEgressLocationHasVerdictColumns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, col := range []string{"verdict", "verdict_reason", "assurance"} {
			var exists bool
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx,
					`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'provider_egress_location' AND column_name = $1)`,
					col)
				connect.AssertEqual(t, err, nil)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						connect.AssertEqual(t, result.Scan(&exists), nil)
					}
				})
			})
			if !exists {
				t.Errorf("provider_egress_location missing column %q", col)
			}
		}
	})
}
```

- [ ] **Step 3: Run it and confirm it fails**

Run: `./test.sh -run TestProviderBandwidthTableExists`
Expected: table does not exist / columns missing.

- [ ] **Step 4: Append the migrations**

Find the tail of the `migrations` slice in `db_migrations.go` (it currently ends after the `provider_egress_probe_attempt` index from P1) and append, never insert earlier:

```go
	newSqlMigration(`
        ALTER TABLE provider_egress_location
            ADD COLUMN IF NOT EXISTS verdict varchar(16) NOT NULL DEFAULT 'unverified',
            ADD COLUMN IF NOT EXISTS verdict_reason varchar(64) NOT NULL DEFAULT '',
            ADD COLUMN IF NOT EXISTS assurance varchar(16) NOT NULL DEFAULT 'direct'
    `),
	newSqlMigration(`
        CREATE TABLE IF NOT EXISTS provider_bandwidth (
            client_id          uuid NOT NULL PRIMARY KEY,
            bytes_per_second   double precision NOT NULL,
            source             varchar(16) NOT NULL,
            sample_byte_count  bigint NOT NULL,
            window_start       timestamp NOT NULL,
            window_end         timestamp NOT NULL,
            update_time        timestamp NOT NULL
        )
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS provider_bandwidth_window_end
            ON provider_bandwidth (window_end)
    `),
```

`client_id` as primary key means one row per provider, overwritten on each measurement (mirrors `provider_egress_location`'s own shape) — a history table is not needed for a ranking input.

- [ ] **Step 5: Implement `StoreProviderBandwidth`**

In `model/provider_bandwidth_model.go`, add an upsert keyed on `client_id`:

```go
func StoreProviderBandwidth(ctx context.Context, bw *ProviderBandwidth) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_bandwidth (
				client_id, bytes_per_second, source, sample_byte_count,
				window_start, window_end, update_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (client_id) DO UPDATE SET
				bytes_per_second = EXCLUDED.bytes_per_second,
				source = EXCLUDED.source,
				sample_byte_count = EXCLUDED.sample_byte_count,
				window_start = EXCLUDED.window_start,
				window_end = EXCLUDED.window_end,
				update_time = EXCLUDED.update_time
			`,
			bw.ClientId, bw.BytesPerSecond, bw.Source, bw.SampleByteCount,
			bw.WindowStart, bw.WindowEnd, server.NowUtc(),
		))
	})
}
```

- [ ] **Step 6: Run tests and regressions**

```bash
./test.sh -run TestProviderBandwidth
./test.sh -run TestApplyDbMigrations
go build ./... && go vet ./...
```

- [ ] **Step 7: Ship both branches** (same shape as Task 1 Step 7; commit message should note the columns are additive with safe defaults, so existing rows and existing readers of `provider_egress_location` are unaffected).

---

## Task 3: `probeverdict` — pure verdict logic

**Repo:** `/root/urnetwork/server`. New standalone package, so branch from `beta/self-contained-env` directly rather than stacking — it has no dependency on Tasks 1-2's schema, only on their *types* being stable, and stacking unnecessarily would make this task's PR show unrelated schema diffs. Two branches: `feat/provider-egress-verdict` and `-upstream`.

**Context:** the spec's threat model requires: (1) no country consensus → `unverified`; (2) country flip-flopping within 24h → `suspect`; (3) probed country differing from mmdb is explicitly **not** suspicious — that divergence is the entire point.

**An RTT-distance floor was designed for this package and deliberately dropped before implementation — do not add it back.** See the spec's "The RTT floor was designed, then dropped" section for the full reasoning. In short: the check needs a fixed reference point (where the RTT was measured *from*), and `network_client_connection.connection_block` exists specifically because there can be more than one such point. A wrong reference point does not fail safe — it can flag an honest, correctly-placed provider as `suspect`, not merely loosen the check. Weighed against a check that was already one-sided and defeatable by a provider willing to degrade its own latency, it was not worth carrying the reference-point problem. `Input` therefore has no RTT or coordinate fields at all, and `Verdict.Reason` never takes the value `"rtt_impossible"`.

**Files:**
- Create: `probeverdict/probeverdict.go`, `probeverdict/probeverdict_test.go`

**Interfaces:**
- Produces: `type Input struct { CountryConfident bool; CountryCode string; PreviousCountryCode string; PreviousObservedAt time.Time; Now time.Time }`
- Produces: `type Verdict struct { State string; Reason string }` — `State` is `"verified"`, `"unverified"`, or `"suspect"`.
- Produces: `func Evaluate(in Input) Verdict`

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/server
git checkout beta/self-contained-env && git pull --ff-only origin beta/self-contained-env
git checkout -b feat/provider-egress-verdict
```

- [ ] **Step 2: Write the failing tests**

Create `probeverdict/probeverdict_test.go`:

```go
package probeverdict

import (
	"testing"
	"time"
)

func TestEvaluateNoConsensusIsUnverified(t *testing.T) {
	v := Evaluate(Input{CountryConfident: false})
	if v.State != "unverified" || v.Reason != "no_consensus" {
		t.Errorf("got %+v, want unverified/no_consensus", v)
	}
}

func TestEvaluateCountryFlipFlopIsSuspect(t *testing.T) {
	now := time.Now()
	v := Evaluate(Input{
		CountryConfident:    true,
		CountryCode:         "de",
		PreviousCountryCode: "es",
		PreviousObservedAt:  now.Add(-2 * time.Hour),
		Now:                 now,
	})
	if v.State != "suspect" || v.Reason != "unstable" {
		t.Errorf("got %+v, want suspect/unstable", v)
	}
}

func TestEvaluateCountryChangeOutsideWindowIsVerified(t *testing.T) {
	// a country change is only "unstable" within the 24h window -- after it,
	// a changed country is a legitimate correction, not a flip-flop
	now := time.Now()
	v := Evaluate(Input{
		CountryConfident:    true,
		CountryCode:         "de",
		PreviousCountryCode: "es",
		PreviousObservedAt:  now.Add(-25 * time.Hour),
		Now:                 now,
	})
	if v.State != "verified" {
		t.Errorf("got %+v, want verified (change is outside the 24h window)", v)
	}
}

func TestEvaluateMmdbDivergenceAloneIsNotSuspect(t *testing.T) {
	// this test asserts the single most important safety property in the
	// package: a country that differs from what mmdb would have said is NOT
	// an input to Evaluate at all -- there is no field for it on Input, so a
	// clean first-time probe always verifies regardless of what mmdb would
	// have said about the same connection.
	v := Evaluate(Input{
		CountryConfident: true,
		CountryCode:      "es",
	})
	if v.State != "verified" {
		t.Errorf("a clean probe with no prior history must verify regardless of what mmdb would have said, got %+v", v)
	}
}
```

- [ ] **Step 3: Run and confirm failure**

Run: `cd /root/urnetwork/server && go test ./probeverdict/ -v`
Expected: build failure, `undefined: Evaluate`.

- [ ] **Step 4: Implement**

```go
// Package probeverdict turns a geolocation probe submission into a verdict:
// verified, unverified, or suspect. It is pure decision logic with no I/O, so
// it is fully table-testable independent of how a submission arrived.
//
// Deliberately absent from Input: the mmdb-derived country for the same
// connection, and any RTT or coordinate fields. A probed country differing
// from what the free mmdb would have said is the entire point of this
// project and must never be treated as suspicious -- keeping that field off
// Input makes the omission structural. An RTT-distance corroboration was
// designed and dropped before implementation (see the spec's "The RTT floor
// was designed, then dropped"): it needs a fixed reference point that this
// system does not have a single answer for once more than one deployment
// instance exists, and a wrong reference point does not fail safe -- it can
// flag an honest provider as suspect. Do not reintroduce ObservedRTT or
// coordinate fields here without first resolving that reference-point
// problem in the spec.
package probeverdict

import "time"

type Input struct {
	CountryConfident bool
	CountryCode      string

	PreviousCountryCode string
	PreviousObservedAt  time.Time
	Now                 time.Time
}

type Verdict struct {
	State  string
	Reason string
}

// unstableWindow is how recently a prior, different country counts as a
// flip-flop rather than a legitimate correction.
const unstableWindow = 24 * time.Hour

func Evaluate(in Input) Verdict {
	if !in.CountryConfident {
		return Verdict{State: "unverified", Reason: "no_consensus"}
	}

	if in.PreviousCountryCode != "" && in.PreviousCountryCode != in.CountryCode {
		now := in.Now
		if now.IsZero() {
			now = time.Now()
		}
		if now.Sub(in.PreviousObservedAt) < unstableWindow {
			return Verdict{State: "suspect", Reason: "unstable"}
		}
	}

	return Verdict{State: "verified"}
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./probeverdict/ -v`
Expected: all five PASS.

- [ ] **Step 6: Ship both branches** (same shape as prior tasks; this package has no server-side dependency so the upstream cherry-pick should apply with zero conflicts — note that explicitly in the PR body).

---

## Task 4: Hourly byte-budget reservation for active probes

**Repo:** `/root/urnetwork/server`. Branch from `beta/self-contained-env` (independent of Tasks 1-3). Two branches: `feat/provider-bandwidth-quota` and `-upstream`.

**Context:** mirror `model/bulk_client_removal_rate_limit.go` exactly, substituting bytes for row counts. Read that file in full before writing this one — the fixed hourly bucket, the lookahead, and the jittered `RunAt` pattern all transfer unchanged.

**Files:**
- Create: `model/provider_bandwidth_rate_limit.go`, `model/provider_bandwidth_rate_limit_test.go`
- Modify: `db_migrations.go` — append a `provider_bandwidth_quota` table, structurally identical to `bulk_client_removal_quota`

**Interfaces:**
- Produces: `const ProviderBandwidthBucketDuration = time.Hour`
- Produces: `const MaxActiveBandwidthProbesPerBucket = 40` and `const MaxProviderBandwidthBytesPerBucket = MaxActiveBandwidthProbesPerBucket * 5 * 1024 * 1024` (200 MB/hour, 4.8 GB/day worst case)
- Produces: `func ReserveProviderBandwidthSlot(ctx context.Context, clientId server.Id, byteCount int64) (reservationId server.Id, bucketStart time.Time, err error)`
- Produces: `func CancelProviderBandwidthReservation(ctx context.Context, reservationId server.Id)`

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/server
git checkout beta/self-contained-env && git pull --ff-only origin beta/self-contained-env
git checkout -b feat/provider-bandwidth-quota
```

- [ ] **Step 2: Write the failing test**

Add to `model/provider_bandwidth_rate_limit_test.go`, mirroring the structure of the existing bulk-delete rate-limit tests (read `model/bulk_client_removal_rate_limit_test.go` first and match its fixture style exactly — same `server.DefaultTestEnv()` pattern, same assertion style):

```go
func TestReserveProviderBandwidthSlotFillsBucketThenSpillsToNext(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		half := MaxProviderBandwidthBytesPerBucket / 2

		_, bucket1, err := ReserveProviderBandwidthSlot(ctx, clientId, half)
		connect.AssertEqual(t, err, nil)

		_, bucket2, err := ReserveProviderBandwidthSlot(ctx, clientId, half+1)
		connect.AssertEqual(t, err, nil)

		if !bucket2.After(bucket1) {
			t.Errorf("second reservation exceeding the bucket must spill to a later bucket: bucket1=%v bucket2=%v", bucket1, bucket2)
		}
	})
}

func TestReserveProviderBandwidthSlotErrorsWhenAllBucketsFull(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for i := 0; i < MaxProviderBandwidthLookaheadBuckets; i++ {
			_, _, err := ReserveProviderBandwidthSlot(ctx, server.NewId(), MaxProviderBandwidthBytesPerBucket)
			connect.AssertEqual(t, err, nil)
		}
		_, _, err := ReserveProviderBandwidthSlot(ctx, server.NewId(), 1)
		if err == nil {
			t.Fatal("expected an error once every lookahead bucket is full")
		}
	})
}
```

- [ ] **Step 3: Run and confirm failure**

Run: `./test.sh -run TestReserveProviderBandwidthSlot`
Expected: compile failure.

- [ ] **Step 4: Append the migration**

`MaxActiveBandwidthProbesPerBucket = 40` is derived from the population this
probes, not picked arbitrarily: active sampling only ever runs against
providers with **no passive history** (Task 6), which on beta today is the
entire fleet (0 rows in `account_payment`, so nothing has passive history
yet) and on a mature deployment is the trickle of newly-joined providers
before their first settled contract. 40/hour comfortably covers a beta-sized
fleet in one pass and remains a small, bounded spend on a mature one where the
no-history population is naturally small. Revisit this constant with real
production data once Task 6 has run for a week; it is a tuned value, not a
structural one.

Mirror `bulk_client_removal_quota`'s shape exactly, substituting a byte count and a client id for a network id and client count:

```go
	newSqlMigration(`
        CREATE TABLE IF NOT EXISTS provider_bandwidth_quota (
            provider_bandwidth_quota_id uuid NOT NULL PRIMARY KEY,
            client_id                   uuid NOT NULL,
            byte_count                  bigint NOT NULL,
            bucket_start                timestamp NOT NULL,
            create_time                 timestamp NOT NULL
        )
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS provider_bandwidth_quota_bucket_start
            ON provider_bandwidth_quota (bucket_start)
    `),
```

- [ ] **Step 5: Implement, copying the structure of `ReserveBulkClientRemovalSlot`**

Set `ProviderBandwidthBucketDuration = time.Hour` and `MaxProviderBandwidthLookaheadBuckets = 24`. For `MaxProviderBandwidthBytesPerBucket`, size it from the spec's 5 MB-per-probe figure: pick a value that admits a meaningful sample of the deployment's eligible-for-active-probing population per hour without being a number nobody chose deliberately — document the arithmetic in a comment (e.g. "N probes/hour x 5MB = budget", where N is a named constant you also define and justify against the sampling-rate decision in the spec's Global Constraints). Do not hardcode a number with no derivation shown.

The function body is `ReserveBulkClientRemovalSlot` with `network_id`/`count` renamed to `client_id`/`byte_count` and the table name swapped — copy the SQL shape (the `WITH slice`-free simple loop-and-check version, not the CTE version used elsewhere) exactly, including the comment explaining why a plain transaction is used instead of `SELECT ... FOR UPDATE`.

- [ ] **Step 6: Run tests and regressions**

```bash
./test.sh -run TestReserveProviderBandwidthSlot
./test.sh -run TestReserveBulkClientRemovalSlot   # must still pass unchanged
go build ./... && go vet ./...
```

- [ ] **Step 7: Ship both branches.**

---

## Task 5: Operator bandwidth-test endpoint

**Repo:** `/root/urnetwork/server`. Branch from `beta/self-contained-env`. Two branches: `feat/provider-bandwidth-endpoint` and `-upstream`.

**Context:** the active probe needs something to download *through* the provider tunnel. This endpoint is that target: operator-secret-gated (mirroring the existing ingest/due/attempt endpoints exactly — same header, same `hmac.Equal`, same fail-closed `sync.OnceValue` pattern), it streams a bounded number of bytes and stops.

**Files:**
- Create: `api/handlers/provider_bandwidth_handlers.go`, `api/handlers/provider_bandwidth_handlers_test.go`
- Modify: `api/api.go`

**Interfaces:**
- Produces: `GET /network/provider-bandwidth-test?bytes=N` — streams `min(N, maxProviderBandwidthTestBytes)` bytes of arbitrary content (e.g. repeated zero blocks; content is irrelevant, only byte count matters) with `Content-Type: application/octet-stream`, authenticated by `X-UR-Operator-Secret`.
- Produces: `POST /network/provider-bandwidth-result` — the prober's result submission (Task 6 depends on this; without it there is nowhere to send an active measurement). Body:

```go
type SubmitProviderBandwidthArgs struct {
	ClientId        server.Id `json:"client_id"`
	BytesPerSecond  float64   `json:"bytes_per_second"`
	SampleByteCount int64     `json:"sample_byte_count"`
}
```

  Rejects `BytesPerSecond <= 0` or `SampleByteCount <= 0` with 400 (a zero or
  negative measurement is not a usable sample and must not overwrite a real
  one). On success, calls `model.StoreProviderBandwidth` (Task 2) with
  `Source: "active"`, `WindowStart`/`WindowEnd` both set to the request's
  arrival time (an active probe is a point measurement, not a window).
  Response: `200 {}`.

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/server
git checkout beta/self-contained-env && git pull --ff-only origin beta/self-contained-env
git checkout -b feat/provider-bandwidth-endpoint
```

- [ ] **Step 2: Write the failing tests**

Add to `api/handlers/provider_bandwidth_handlers_test.go`, following the exact structure of `provider_egress_location_handlers_test.go` (same secret-stubbing helper, same reject/accept shape):

```go
func TestProviderBandwidthTestRejectsMissingSecret(t *testing.T) {
	// mirror the existing ingest/due tests' setup exactly
}

func TestProviderBandwidthTestRejectsWrongSecret(t *testing.T) {
}

func TestProviderBandwidthTestStreamsRequestedByteCount(t *testing.T) {
	// with the correct secret and ?bytes=1048576, the response body must be
	// exactly 1048576 bytes -- this is the test that would fail against a
	// handler that accepts auth but streams nothing, or streams the wrong
	// amount
}

func TestProviderBandwidthTestClampsToMaximum(t *testing.T) {
	// ?bytes=<a number far larger than maxProviderBandwidthTestBytes> must
	// stream exactly maxProviderBandwidthTestBytes, not the requested amount
	// -- an unclamped stream is an open-ended resource commitment per request
}
```

- [ ] **Step 3: Run and confirm failure**

Expected: compile failure, `undefined: ProviderBandwidthTest`.

- [ ] **Step 4: Implement the download endpoint**

Reuse `operatorSecretHeader` and `operatorIngestSecret()` from `provider_egress_location_handlers.go` (same package, so no new plumbing). Parse `bytes` from the query string, default to 1 MiB if absent or malformed, clamp to `maxProviderBandwidthTestBytes = 5 * 1024 * 1024` (matching the spec's per-target sizing). Stream from a `io.LimitReader` wrapping a repeating byte source — do not allocate the full byte count in memory.

- [ ] **Step 5: Write the failing test for the result endpoint**

```go
func TestProviderBandwidthResultRejectsMissingSecret(t *testing.T) {
	// mirror the download endpoint's rejection test
}

func TestProviderBandwidthResultRejectsNonPositiveMeasurement(t *testing.T) {
	// bytes_per_second = 0 and sample_byte_count = 0 must both independently
	// return 400 -- a zero or negative measurement is not usable and must
	// never overwrite a real one
}

func TestProviderBandwidthResultStoresAnActiveMeasurement(t *testing.T) {
	// a valid submission returns 200, and a subsequent read of
	// provider_bandwidth for that client_id shows Source == "active" and the
	// exact bytes_per_second submitted -- this is the test that would fail
	// against a handler that authenticates correctly but never calls
	// model.StoreProviderBandwidth
}
```

- [ ] **Step 6: Run and confirm failure**

Expected: compile failure, `undefined: ProviderBandwidthResult`.

- [ ] **Step 7: Implement the result endpoint**

`SubmitProviderBandwidthArgs` as specified in this task's Interfaces block. Validate both fields are positive before calling `model.StoreProviderBandwidth(ctx, &model.ProviderBandwidth{ClientId: args.ClientId, BytesPerSecond: args.BytesPerSecond, Source: "active", SampleByteCount: args.SampleByteCount, WindowStart: now, WindowEnd: now})`, where `now := server.NowUtc()`.

- [ ] **Step 8: Register both routes**

```go
		router.NewRoute("GET", "/network/provider-bandwidth-test", handlers.ProviderBandwidthTest),
		router.NewRoute("POST", "/network/provider-bandwidth-result", handlers.ProviderBandwidthResult),
```

- [ ] **Step 9: Run tests and regressions, ship both branches.**

---

## Task 6: Active bandwidth probe in the prober

**Repo:** `/tmp/sandbox/urnetwork-operator-proxy`, branch `main`. No cherry-pick (single-repo project).

**Context:** the active probe is a *sample*, run only when a provider has no passive history and a budget reservation succeeds. It shares the geo-probe's already-open tunnel opportunistically — never opens a second tunnel, and never delays the geo probe waiting for bandwidth budget.

**Files:**
- Create: `bandwidth/bandwidth.go`, `bandwidth/bandwidth_test.go`
- Modify: `cmd/egress-prober/main.go`

**Interfaces:**
- Consumes: `providertunnel.Tunnel.HTTPClient` (existing), the server's `GET /network/provider-bandwidth-test` (Task 5).
- Produces: `func Measure(ctx context.Context, client *http.Client, testURL string, timeout time.Duration) (bytesPerSecond float64, sampleByteCount int64, err error)`. Adaptive: streams until **5 s elapsed or 5 MB transferred**, whichever first, discarding the first 500 ms to exclude TCP slow-start, matching the spec's sizing exactly.

- [ ] **Step 1: Write the failing test**

Create `bandwidth/bandwidth_test.go`. Use `httptest.Server` to serve a controllable byte stream so the test never touches the real network:

```go
func TestMeasureStopsAtByteCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// serve far more than the 5MB cap so the test proves Measure stops itself
		io.Copy(w, io.LimitReader(zeroReader{}, 50*1024*1024))
	}))
	defer srv.Close()

	bps, sampleBytes, err := Measure(context.Background(), srv.Client(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("Measure: %s", err)
	}
	if sampleBytes > 5*1024*1024 {
		t.Errorf("sampleByteCount = %d, exceeded the 5MB cap", sampleBytes)
	}
	if bps <= 0 {
		t.Errorf("bytesPerSecond = %f, want > 0", bps)
	}
}

func TestMeasureStopsAtTimeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// serve slowly enough that the 5MB cap is never reached
		for i := 0; i < 100; i++ {
			w.Write([]byte("x"))
			time.Sleep(60 * time.Millisecond)
		}
	}))
	defer srv.Close()

	start := time.Now()
	_, _, err := Measure(context.Background(), srv.Client(), srv.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("Measure: %s", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Measure ran for %s, want it to stop at the ~2s time cap", elapsed)
	}
}
```

Add a small `zeroReader` type in the test file (an `io.Reader` that writes zero bytes forever) rather than allocating a 50 MB buffer.

- [ ] **Step 2: Run and confirm failure**

Expected: build failure, `undefined: Measure`.

- [ ] **Step 3: Implement**

```go
// Package bandwidth measures throughput to the operator's own endpoint over
// an already-open tunnel. It is deliberately adaptive rather than fixed-size:
// a fixed payload is either too small to mean anything on a fast link (inside
// TCP slow start) or wastes budget on a slow one. It streams until either
// bound is hit, whichever comes first, and reports the steady-state rate
// after discarding early slow-start noise.
package bandwidth

import (
	"context"
	"io"
	"net/http"
	"time"
)

const maxSampleBytes = 5 * 1024 * 1024
const warmupDuration = 500 * time.Millisecond

func Measure(ctx context.Context, client *http.Client, testURL string, timeout time.Duration) (float64, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	start := time.Now()
	var total int64
	var steadyStart time.Time
	var steadyBytes int64
	buf := make([]byte, 32*1024)

	for {
		n, err := resp.Body.Read(buf)
		total += int64(n)
		now := time.Now()
		if steadyStart.IsZero() && warmupDuration <= now.Sub(start) {
			steadyStart = now
		}
		if !steadyStart.IsZero() {
			steadyBytes += int64(n)
		}
		if maxSampleBytes <= total {
			break
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, total, err
		}
		if timeout <= now.Sub(start) {
			break
		}
	}

	elapsed := time.Since(steadyStart)
	if steadyStart.IsZero() || elapsed <= 0 {
		// the whole sample fell inside the warmup window -- not enough
		// steady-state data to report a rate
		return 0, total, nil
	}
	return float64(steadyBytes) / elapsed.Seconds(), total, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./bandwidth/ -v`
Expected: both PASS.

- [ ] **Step 5: Extend the due response so the prober knows who needs sampling**

**Decision: extend `GET /network/provider-egress-due`'s response rather than adding a second endpoint.** The due list already enumerates exactly the population this decision is about (providers due for a geo probe this pass), so answering "does this one also need an active bandwidth sample" per entry is one extra field on a call the prober already makes — not a new round trip.

On the server side (this is a follow-up to P1's due endpoint, in the same PR pair as this task's server changes — add it to `feat/provider-bandwidth-endpoint` from Task 5, not this operator-proxy task): change the response shape from `{"client_ids": [...]}` to:

```go
type ProviderEgressDueEntry struct {
	ClientId       string `json:"client_id"`
	NeedsBandwidth bool   `json:"needs_bandwidth"`
}
```

`NeedsBandwidth` is `true` when the client has no row in `provider_bandwidth`, or its row is older than a staleness threshold matching the geo-probe's own (`ProviderEgressLocationMaxAge`, for consistency rather than a new tunable). Add a test asserting a client with a fresh `provider_bandwidth` row is *not* flagged, and one with none or a stale one *is*.

On the operator-proxy side, update `ingest.Due`'s return type to match and thread `NeedsBandwidth` through `selectProviders` into the per-provider probe call.

- [ ] **Step 6: Wire the measurement into the pass loop**

In `cmd/egress-prober/main.go`, after a successful geo probe (`p.probeOne` succeeds) and only when both hold: (a) `NeedsBandwidth` was true for this provider, and (b) a budget reservation succeeds via a new `ingest.Client.ReserveBandwidth(ctx, clientId string, byteCount int64) error` method that calls a small new server endpoint wrapping `model.ReserveProviderBandwidthSlot` (Task 4) — add this endpoint to the same `feat/provider-bandwidth-endpoint` PR, following the identical operator-secret auth pattern, returning 429 when the reservation errors (all buckets full) so the prober can skip cleanly rather than retrying — call `bandwidth.Measure` over the *same* tunnel's `HTTPClient` (never open a second tunnel), then submit the result via a new `ingest.Client.SubmitBandwidth(ctx, clientId string, bytesPerSecond float64, sampleByteCount int64) error` posting to `/network/provider-bandwidth-result` (Task 5).

Log clearly which providers were sampled and which were skipped for lack of budget, so an operator can see the sampling rate operating in practice: `bandwidth: sampled=%d skipped_no_budget=%d skipped_has_history=%d`.

- [ ] **Step 7: Verify**

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./... -race -count=1
```

- [ ] **Step 8: Commit and push to `origin main`.**

---

## Task 7: Wire `probeverdict` into the ingest submission

**Repo:** `/root/urnetwork/server`. Branch from `feat/provider-bandwidth-schema` (needs Task 2's columns) and rebase onto `feat/provider-egress-verdict` once both are up — or, simpler and independent of merge order, branch from `beta/self-contained-env` directly and cherry-pick both prerequisite branches' tip commits before starting. Two branches: `feat/provider-egress-verdict-wiring` and `-upstream`.

**Context:** Task 3 built and tested `probeverdict.Evaluate` in isolation; nothing calls it yet. This task is what actually turns a submission into a recorded judgement — without it, the `verdict` column added in Task 2 sits at its default (`'unverified'`) forever, and Task 3's package is dead code.

**No RTT plumbing here.** `probeverdict.Input` has no RTT or coordinate fields (see Task 3), and Task 2 does not add an `rtt_millis` column, so there is nothing RTT-shaped anywhere in this task.

**Files:**
- Modify: `controller/provider_egress_location_controller.go`

**Interfaces:**
- Consumes: `probeverdict.Evaluate` (Task 3), `model.GetProviderEgressLocation` (existing).

- [ ] **Step 1: Create the branch**

```bash
cd /root/urnetwork/server
git checkout beta/self-contained-env && git pull --ff-only origin beta/self-contained-env
git checkout -b feat/provider-egress-verdict-wiring
git cherry-pick feat/provider-bandwidth-schema feat/provider-egress-verdict
```

Resolve any conflict by keeping both sides' additions — they touch disjoint columns and a new package respectively, so a real conflict here means one of the two branches has drifted and should be re-based first.

- [ ] **Step 2: Write the failing test**

Add to `controller/provider_egress_location_controller_test.go`:

```go
func TestSubmitProviderEgressLocationRecordsVerifiedVerdict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId: clientId, CountryCode: "es", Country: "Spain",
			CountryConfident: true, ObservedAt: server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected a stored egress location")
		}
		connect.AssertEqual(t, stored.Verdict, "verified")
	})
}

func TestSubmitProviderEgressLocationRecordsSuspectOnCountryFlipFlop(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")

		// first probe: spain
		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId: clientId, CountryCode: "es", Country: "Spain",
			CountryConfident: true, ObservedAt: server.NowUtc().Add(-time.Hour),
		})
		connect.AssertEqual(t, err, nil)

		// second probe an hour later: germany -- a flip-flop inside the 24h window
		_, err = SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId: clientId, CountryCode: "de", Country: "Germany",
			CountryConfident: true, ObservedAt: server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected a stored egress location")
		}
		connect.AssertEqual(t, stored.Verdict, "suspect")
		connect.AssertEqual(t, stored.VerdictReason, "unstable")
	})
}
```

- [ ] **Step 3: Run and confirm failure**

Expected: the verdict assertions fail against the column's default (`'unverified'`), since nothing computes or stores a verdict yet.

- [ ] **Step 4: Implement**

In `SubmitProviderEgressLocation`, after the existing validation and before the row is written:

```go
	previous := model.GetProviderEgressLocation(ctx, args.ClientId)
	var previousCountryCode string
	var previousObservedAt time.Time
	if previous != nil {
		previousCountryCode = previous.CountryCode
		previousObservedAt = previous.ObservedAt
	}

	verdict := probeverdict.Evaluate(probeverdict.Input{
		CountryConfident:    args.CountryConfident,
		CountryCode:         countryCode,
		PreviousCountryCode: previousCountryCode,
		PreviousObservedAt:  previousObservedAt,
		Now:                 server.NowUtc(),
	})
```

then pass `verdict.State` and `verdict.Reason` through to whatever writes the row (extend `model.SetProviderEgressLocation`'s args struct with `Verdict`/`VerdictReason` fields, defaulting to `"unverified"`/`""` for any other caller).

- [ ] **Step 5: Run tests and regressions**

```bash
./test.sh -run TestSubmitProviderEgressLocation
go build ./... && go vet ./...
```

- [ ] **Step 6: Ship both branches.**

---

## Verification Summary

| Task | Gate |
| --- | --- |
| 1 | Passive bandwidth derived correctly from settled bytes; nil (not zero) when no history; companion contracts excluded |
| 2 | New table and columns present; existing `provider_egress_location` readers unaffected by the additive columns |
| 3 | All five `probeverdict` tests pass, including the mmdb-non-suspicious property |
| 4 | Budget fills then spills to the next bucket; errors only when every lookahead bucket is full; existing bulk-delete tests unaffected |
| 5 | Auth rejects/accepts correctly; streamed byte count matches the request and is clamped; the result endpoint stores an active measurement and rejects non-positive values |
| 6 | `Measure` respects both the byte and time caps; sampling only fires when `NeedsBandwidth` was true and budget was reserved |
| 7 | A clean submission verifies; a country flip-flop within 24h is flagged suspect with the right reason |
| All | Both PRs open per server-side task; nothing merged to a base; the placeholder operator-origin constant is replaced before merge |

## Out of Scope

- Turning the hard gate on (advisory stays advisory).
- Letting measured bandwidth satisfy `PassesMinimums`.
- Identity rotation and multi-hop probing (P3) — `assurance` stays `'direct'` until then.
- Automatic action on a `suspect` verdict, including `bandwidth_divergence` corroboration against a second (CDN) target — the spec allows a second target only to corroborate an already-suspected result, which needs P3's multi-hop machinery to be meaningful and is deferred with it.
