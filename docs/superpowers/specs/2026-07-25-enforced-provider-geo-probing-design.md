# Enforced Provider Geo Probing — Design

**Date:** 2026-07-25
**Status:** Approved for planning
**Follows:** `2026-07-24-provider-egress-geolocation-design.md`

## Goal

Probe every public provider's egress location automatically, on a cadence, from
the operator's own deployment — with results that a provider cannot forge, and
with a recorded verdict that can later gate listing or payment without rework.

The prior design proved the mechanism: a probe routed through a provider's
tunnel reports the provider's egress, and is verified end to end (a US host
probing a Spanish provider returned Spain / AS8560 IONOS, with tcpdump showing
zero packets to any geolocation API). This design turns that one-shot operator
tool into an enforced, automated, tamper-resistant subsystem.

## The hard constraint

Unchanged and absolute: **the operator's server must never send a request
directly to a geolocation API.** Every lookup egresses through a provider.

Running the prober on the operator's server does not violate this — the egress
is still the provider — but it does place geolocation code inside the process
that must never reach those APIs. This design therefore stops relying on
code-level discipline alone (see *The jail*).

## Decisions

| Question | Decision |
| --- | --- |
| Consequence of no fresh probe | **Advisory.** Provider stays listed, falls back to its mmdb location, and is flagged unverified. |
| Where the prober runs | **Taskworker, in a network-namespace jail** (a separate child process). |
| Tamper resistance | **Rotating identity + corroboration + multi-hop probing.** |
| Which providers | **Public providers only** (`provide_mode = 3`). |

Advisory enforcement is deliberately a starting point. It is enforcement in
name only until something consumes the flag, so the schema below is shaped so
that flipping to a hard gate is a policy change, not a migration.

## Threat model

The provider is untrusted and has an incentive to misreport its location — to
appear in a higher-paying country, or to hide the real one.

| Attack | Defence |
| --- | --- |
| Forge the geolocation response (MITM) | SPKI pinning over the **verified chain**. Already implemented. |
| Chain its own egress through another country | **Not an attack.** The API honestly reports where traffic exits, and user traffic exits there too. That is the correct answer. |
| Detect the probe and route *it* through a clean exit while sending user traffic elsewhere | **The real attack.** Addressed by identity rotation and multi-hop; bounded by RTT corroboration. |
| Block the geolocation hosts so no probe succeeds | Probe fails, provider stays `unverified`. Under advisory enforcement this costs the provider nothing — a known gap, closed when the gate is enabled. |
| Claim a country it cannot physically be in | **RTT floor.** Catches the cheap version of this lie; see the honest limits below. |

The probe-detection attack cannot be fully eliminated: a provider can in
principle fingerprint traffic shape. The goal is to make detection expensive
and unreliable, and to keep an independent physical check that does not depend
on the provider's cooperation at all.

## Architecture

Four units with clean boundaries:

### `egressprober` (new binary, operator-proxy repo)

Wraps the existing `providertunnel`, `geolocate` and `ingest` libraries in a
long-lived worker that reads probe jobs and reports results. It is a separate
process specifically so it can be network-jailed; Go cannot confine a subset of
goroutines to a namespace.

### `provider_egress_probe_work.go` (new taskworker job, server repo)

Selects due providers, dispatches jobs to the prober process, applies results.
Uses the existing task framework (`ScheduleTaskInTx`, `RunOnce`, `RunAt`)
alongside the `provider_egress_location` sweep job already present.

### `probeverdict` (new package, server repo)

Pure decision logic: no I/O, fully table-testable.

```go
func Evaluate(in Input) Verdict
```

### `provider_egress_location` (existing table)

Extended, not replaced.

### Flow per probe

1. Taskworker selects a due public provider.
2. Prober takes the next identity from its rotating pool.
3. Prober picks a random intermediary from the public-provider pool, excluding
   the target and the identity's own network.
4. Tunnel opens to the target **through** the intermediary.
5. Three geolocation lookups run over that tunnel; consensus is computed.
6. `probeverdict` corroborates against the mmdb location and observed RTT.
7. Row upserted with location, verdict, reason, intermediary and RTT.

## The jail

The prober process runs in a network namespace whose only route is the platform
websocket endpoint. Not firewall rules inside a shared namespace — a distinct
netns, so no route to a geolocation API exists at all.

The Go-level fail-closed behaviour stays as defence in depth, but correctness of
the hard constraint stops depending on it. A future refactor that introduces a
default `http.Client` then fails loudly instead of silently leaking.

The namespace is asserted by an integration test (see *Testing*), so the
constraint is verified as infrastructure rather than trusted as code.

## Identity rotation

`model/network_client_model.go:65` states client ids "are never revoked once
allocated", and that both creation rate and active count are capped per network.
Minting an identity per probe would permanently consume the prober network's
allocation.

**Design:** a fixed pool of 16 client identities in a dedicated prober network,
used round-robin, with the oldest retired on a slow schedule (weekly). Bounded
id growth; the target sees a different `source_id` on almost every probe.

Self-identifying metadata is removed: `DeviceSpec` and `DeviceDescription` must
not contain "prober" or any operator marker. Today they read `egress-prober` and
`egress prober`, which announce the probe to any provider that looks.

Probe scheduling is jittered rather than periodic, so arrival time is not a
fingerprint.

## Multi-hop probing

`FindProvidersProvider.IntermediaryIds` exists in the API but `FindProviders2`
never populates it, while `controller/connect_controller.go:516` does honour
client-supplied intermediaries when creating a contract. **Hop selection is
therefore the prober's responsibility**, which is what we want: we control
randomness rather than letting the server pick a predictable path.

The prober creates the contract with `IntermediaryIds = [intermediary]` and
destination = target. The target observes the intermediary as source.

**Pool-size guard.** Unlinkability is bounded by how many public providers exist
to draw intermediaries from. With two, the only paths are A→B and B→A, which
proves the mechanism but provides no anonymity. Therefore:

- `min_intermediary_pool` (default 8). Below it, multi-hop is skipped, the probe
  runs direct, and the row is recorded with `assurance = 'direct'`.
- At or above it, the probe runs multi-hop and records `assurance = 'hopped'`.

The fallback is explicit and recorded. A thin pool must never silently produce a
result that looks as trustworthy as a hopped one.

## Corroboration: `probeverdict`

```go
type Input struct {
    Consensus     ConsensusSummary // country code, confident flag
    MmdbCountry   string           // from the control-connection ip
    ObservedRTT   time.Duration    // measured to the provider
    PreviousCode  string           // last recorded country, "" if none
    PreviousAt    time.Time
}

type Verdict struct {
    State  State  // Verified | Unverified | Suspect
    Reason string // machine-readable, e.g. "rtt_impossible"
}
```

Rules, in order:

1. **No country consensus** → `Unverified("no_consensus")`.
2. **RTT floor violated** → `Suspect("rtt_impossible")`. See below.
3. **Country changed since the previous probe within 24h** →
   `Suspect("unstable")`.
4. Otherwise → `Verified`.

**Probed country differing from the mmdb country is explicitly NOT suspicious.**
That divergence is the entire purpose of this project — the egress legitimately
differs from where the control connection originates. It is recorded as
`divergent_from_mmdb` for observability and never penalised.

### The RTT floor

A check grounded in physics rather than in the provider's cooperation — but with
a real limit, stated plainly below rather than glossed over.

`ObservedRTT` is the operator's own measurement of the provider's **control
connection** (`network_client_connection.expected_latency_ms`, already
populated — 48 ms and 89 ms for the two providers observed in testing). It is
measured server-side and independently of the probe path, so the multi-hop route
does not distort it.

For claimed country C, take the great-circle distance `d` (km) from the
operator's probe origin to the nearest point of C. The absolute floor is:

```
rtt_floor_ms = 2 * d / 300_000 km/s * 1000
```

Speed of light **in vacuum**, deliberately — not the ~200,000 km/s of fibre.
Using the vacuum figure makes the bound strictly unachievable rather than merely
unlikely, so an honest provider on an unusually good path can never be flagged.
False negatives are acceptable here; false positives are not, because the
penalty is a `Suspect` verdict on a real operator's provider.

Worked example: Atlanta → Spain is ~7,000 km, so the floor is ~47 ms. A provider
answering in 8 ms while claiming Spain is impossible and gets flagged. The
measured value for the real Spanish provider in testing was well above the floor.

**What this does not catch.** The floor is one-sided. It detects a provider that
claims a distant country while answering too quickly to be there — but a
provider can *inflate* its latency by adding artificial delay, and thereby fake
being further away than it is. The check is therefore cost-imposing rather than
absolute: faking distance requires degrading your own measured latency, which
lowers the provider's quality score and, with it, its selection rate and
earnings. That is a real deterrent, not a proof. It should be described that way
in any operator-facing documentation, so the verdict is not read as a stronger
guarantee than it is.

`suspect` verdicts alert and are recorded. **They trigger no automatic action**
under advisory enforcement.

## Data model

Extend `provider_egress_location` (additive; append-only migration, never edit
an applied one):

| Column | Type | Purpose |
| --- | --- | --- |
| `verdict` | `varchar(16) NOT NULL DEFAULT 'unverified'` | `verified` / `unverified` / `suspect`. The switch a future hard gate reads. |
| `verdict_reason` | `varchar(64) NOT NULL DEFAULT ''` | Machine-readable reason. |
| `assurance` | `varchar(16) NOT NULL DEFAULT 'direct'` | `hopped` / `direct`. |
| `intermediary_client_id` | `uuid NULL` | Which hop carried the probe, for attribution. |
| `rtt_millis` | `int NOT NULL DEFAULT 0` | Feeds the floor check and diagnosis. |
| `probe_attempt_at` | `timestamp NULL` | Last attempt, successful or not. |
| `probe_failure` | `varchar(64) NOT NULL DEFAULT ''` | Failure class when the attempt failed. |

`asn` is already `bigint`. Note the beta table was created `int4` and widened by
a later migration; upstream carries `bigint` in the create itself.

## Scheduling and failure handling

- A taskworker job runs on a short cadence, selecting public providers whose
  last successful probe is older than the TTL (24h) or absent.
- Selection is jittered across the window so probes are not clock-aligned.
- Concurrency is capped (default 4 simultaneous tunnels).
- Per-provider exponential backoff on repeated failure, so one broken provider
  cannot consume a pass.

Failure classes, recorded in `probe_failure`:

| Class | Meaning | Handling |
| --- | --- | --- |
| `contract_failed` | Provider would not create a contract | Backoff. Common for non-public providers. |
| `tunnel_failed` | Tunnel never established | Backoff. |
| `no_consensus` | Fewer than `MinSources` responded, or sources disagreed | Backoff. |
| `submit_rejected` | Server rejected the submission | **Alert** — indicates a contract mismatch between prober and server. |
| `pin_mismatch` | Certificate pin did not match | **Alert, never silently retry.** This is an active MITM attempt, not an operational blip. |

## Enforcement surface

Under advisory enforcement, `SetConnectionLocation` continues to prefer a fresh
probed location and fall back to mmdb. The `verdict` column is written but
consumed by nothing except observability.

Enabling a hard gate later means joining on `verdict = 'verified'` in the
provider-selection path. No schema change, no backfill.

## Prerequisites

Two independent problems block or distort this work. Neither is caused by it.

1. **`find-providers2` must be called with `force_minimum: true`.**
   `loadClientScores` filters on `PassesMinimums`, which returned 1 provider of
   39 in testing. A census wants every reachable provider, not those meeting a
   user-facing quality bar.

2. **Provider registration is broken on beta.** Of 38 connected CLI providers,
   36 have no `provide_key` row at all — not the wrong mode, absent entirely.
   They are connected, counted as providers, and cannot accept any contract.
   Being chased with the provider owner; not caused by this project. Until it is
   resolved the probeable pool is 2, which is below `min_intermediary_pool`, so
   multi-hop will correctly fall back to direct.

Separately, and worth its own fix: `UpdateClientLocations` counts providers from
`network_client_location_reliability` filtered only on `connected AND valid`,
with **no provide-mode check**. Every connected client is advertised as a public
provider. The same `provide_key … provide_mode = 3` join that defines this
project's probe population would also give an honest count.

## Testing

- **`probeverdict`**: table-driven over every rule, including RTT-impossible
  cases with real distances, and an explicit case asserting mmdb divergence does
  **not** produce `Suspect`.
- **The jail**: an integration test asserting the namespace has *no route* to a
  geolocation IP — the constraint verified as infrastructure, not trusted as
  code. Complemented by a tcpdump check in staging.
- **Multi-hop**: assert the target observes the intermediary's `source_id`, not
  the prober's.
- **Pool guard**: assert that below `min_intermediary_pool` the probe runs direct
  and records `assurance = 'direct'` rather than silently claiming `hopped`.
- **Identity rotation**: assert the pool is bounded and reused, so a long run
  does not allocate unbounded client ids.
- **Live check**: the committed manual single-provider probe
  (`cmd/egress-prober/manual_probe_test.go`, env-guarded).

## Egress bandwidth measurement

The location probe answers *where* a provider exits. The other half of what the
market needs is *how fast*, and the existing measurement answers the wrong
question.

`connect/transport_announce.go` speed-tests the **server-to-provider transport**
and averages samples into `network_client_speed`, keyed by `connection_id`. That
is throughput to our own platform, not the throughput a user gets when their
traffic exits to the internet. Egress bandwidth is measured the same way the
location is: through the provider's tunnel.

**Opportunistically shared tunnel.** When a provider is due for both, the geo
probe and the speed test run in one tunnel session — open once, geolocate
(~100 KB measured), then measure throughput. Contract setup dominates the cost,
so sharing the session roughly halves the per-provider price. The sharing is
conditional, not automatic: see *Queueing and load*, where bandwidth is
budget-gated and must never hold up a location refresh.

**Two targets, recorded separately.** Each round measures against an
operator-hosted endpoint *and* a public CDN object, stored as distinct columns.
They are never averaged: the whole value of two targets is that a provider
prioritising one and not the other becomes visible. Divergence beyond 3x yields
`suspect("bandwidth_divergence")`, advisory like every other verdict.

**Adaptive and bounded.** A fixed payload fails at both ends — 10 MB is 0.8 s on
100 Mbit but 0.08 s on 1 Gbit, where the result is TCP slow-start noise rather
than throughput. Each target streams until **5 s elapsed or 25 MB transferred,
whichever comes first**, and throughput is computed over the steady-state window
after discarding the first 500 ms. Cost is hard-capped at 50 MB per provider per
round: a fast link hits the byte cap, a slow one hits the time cap.

### Bandwidth results are advisory

Measured bandwidth is **recorded and never gates the market**. It does not feed
`PassesMinimums`, it does not exclude a provider, and it does not change who is
selected. The market keeps behaving exactly as it does today; the probe only
adds a column.

This is deliberate. Everything below about queueing means bandwidth coverage
will be partial and uneven for a long time — providers deep in the queue may go
days without a measurement. If an absent or stale number could gate selection,
our own capacity limits would silently remove real supply from the market. An
advisory number cannot cause that outage no matter how far behind the queue
falls.

### Queueing and load

Every probe byte crosses the operator's own platform: the path is
prober -> platform -> provider -> internet, so a 50 MB test costs the operator
50 MB of `connect` throughput plus the transport encryption CPU, not just the
provider's bandwidth. Bandwidth probing is therefore capacity-limited by the
operator, and must never be scheduled as "probe everything that is due".

**Reuse the fixed hourly bucket reservation already in this repo.**
`model/bulk_client_removal_rate_limit.go` solves precisely this problem for bulk
deletes and is running on beta: a deployment-wide per-hour budget
(`BulkClientRemovalBucketDuration = time.Hour`), reservations against a specific
`bucket_start`, a `MaxBulkClientRemovalLookaheadBuckets = 24` lookahead that
makes the daily cap fall out of the hourly one, and a jittered `RunAt` within
the bucket so deferred work does not stampede the hour boundary.

Bandwidth probing takes the same shape, budgeted in **bytes** rather than row
counts: each probe reserves its worst-case 50 MB against an hourly byte budget,
and when the current hour is full it is queued into the next hour with lookahead
rather than rejected. The operator's exposure per hour is then a configured
constant, independent of how many providers are due.

Alongside the budget, a **global concurrency cap** (default 2) limits
simultaneous bandwidth tests. This is deliberately separate from, and much
tighter than, the geo probe's concurrency of 4: a geo probe moves ~100 KB while
a bandwidth test moves up to 50 MB, so one shared limit would either throttle
geolocation pointlessly or let bandwidth saturate the platform.

**Queue ordering prioritises providers with more valid connections.** Budget is
scarce, so it goes first to providers actually carrying traffic, ordered by
validated connection count and reliability weight, with age as the tie-break so
nothing starves indefinitely.

**This changes the shared-tunnel decision above.** Sharing one tunnel for geo and
bandwidth is only correct when both are due *and* bandwidth budget is available.
Binding them unconditionally would drag location freshness down to the bandwidth
queue's much slower cadence. The rule is therefore: always run the geo probe on
its own schedule, and attach a bandwidth test to that same tunnel opportunistically
when the provider is due for one and has a reservation. Otherwise the tunnel
carries geolocation alone. Bandwidth never delays a location update.

**Marketplace gating is a separate decision.** Today a provider on
`transportVersion < 2` gets `V0TestConfig()` (`connect/transport.go:380`), so no
speed or latency test ever runs. Scoring then adds 40 for the missing latency
test and 40 for the missing speed test; the total is capped at
`MaxClientScore = 50`, while `PassesMinimums` fails whenever
`maxScore (2 * scorePerTier = 40) <= score`. Since 50 >= 40 always, **an
untested provider is mathematically guaranteed to fail the minimums gate** and
can never be returned by `find-providers2`. That is the root cause of the
observed "39 listed, 1 returned".

**This project does not fix that**, and that must not be misread. Because
measured bandwidth is advisory, it does not satisfy the minimums, so providers on
`transportVersion < 2` stay excluded from `find-providers2` exactly as they are
today. Recording an egress bandwidth number for a provider the market will never
return is still useful — it is the evidence needed to decide the gating question
later — but nobody should expect this work to put those 36 providers into
service.

Admitting them requires a separate, deliberate decision: either let a measured
egress bandwidth satisfy the minimums, or stop penalising a provider for tests
that its transport version makes it structurally incapable of running. The
second is arguably the real bug — the current rule punishes providers for a
protocol version rather than for anything they did — but either way it changes
who receives user traffic and earns, so it is out of scope here.

## Provider lifecycle

A provider is not probed the moment it appears, and being probed once is not
permanent.

1. **Probation** — a newly seen provider must demonstrate sustained reliability
   across the **12-hour** reliability window before it is offered to anyone. Not
   eligible for selection.
2. **Pending verification** — probation satisfied; a combined geo + bandwidth
   probe is scheduled at a **random** time within the window, never a fixed
   offset from graduation.
3. **Verified** — all checks passed. The provider becomes eligible for
   selection through the API.
4. **Suspect / failed** — a check failed. Not eligible; re-probed later under
   the per-provider backoff.

Re-verification recurs at random times, so `verified` decays rather than being
granted once. Randomness serves two purposes at once: a provider cannot prepare
for a known probe window, and load spreads naturally across the fleet.

**Two constraints this places on the rest of the design.**

*Probation window: 12 hours (decided).* `ClientLookbacks` is `[5m, 60m, 12h]`;
there is no 24-hour window, and the only longer entry (6 days) is commented out.
Probation therefore uses the existing **12h** lookback rather than introducing a
new one, which would add rollup rows for every client on every window purely to
express a rounder number. The 12h index is already computed, already fed into
`PassesMinimums` through `minIndependentReliabilityWeights`, and already the
longest signal the reliability pipeline maintains — so the gate is the strongest
evidence available, at no additional storage or write cost.

The practical consequence is that a provider becomes eligible roughly half a day
after first being seen instead of a full day. Given identity churn resets
probation entirely (below), a shorter window is also the more forgiving choice
for honest providers that restart.

*Identity stability is now load-bearing.* Probation accrues per `client_id`, and
**every provider restart mints a new one** — verified by restarting a real
provider four times and observing four distinct client ids, under both the
`auth-provide` and `provide` subcommands. A provider that restarts more often
than every 12 hours never accumulates a full window and can never graduate; with a
`Restart=always` service unit this is the expected case, not an edge case. The
same churn discards any verified location and measured bandwidth, since both are
keyed by `client_id`.

Stable identity keying is consequently a **hard prerequisite** for both this
lifecycle and the probe results themselves, not an optimisation. It belongs in
P0.

## Decomposition

This is too large for a single implementation plan. It splits into four, each
producing working, testable software on its own:

**P0 — Prerequisites** (small, independent, ship first). Three items:
`force_minimum: true` in the prober's enumeration; a provide-mode aware provider
count so the location list stops advertising clients that cannot accept a
contract; and **stable identity keying**, so probation, verified locations and
measured bandwidth survive a provider restart. The second is a user-visible bug
today. The third gates everything else — without it P1's results and the
lifecycle's probation both evaporate on restart.

**P1 — Automated probing in a jail.** The `egressprober` binary, the network
namespace, and the taskworker job. Direct probing only, single fixed identity,
no verdict logic — results land exactly as they do today. Delivers the
automation and, critically, makes the hard constraint structural.

**P2 — Verdict and data model.** The `probeverdict` package, the schema
extension, RTT corroboration, the failure taxonomy and alerting. Turns raw
results into recorded judgements and creates the switch a hard gate will read.

**P3 — Tamper resistance.** The rotating identity pool, multi-hop probing and
the pool-size guard. Deliberately last: it is the largest piece, and it cannot
be meaningfully validated until the public-provider pool exceeds
`min_intermediary_pool`. Until then P1's direct probing is the correct
behaviour, honestly recorded as `assurance = 'direct'`.

## Out of scope

- Turning on the hard gate.
- Letting measured bandwidth satisfy `PassesMinimums`, and the related question
  of whether a provider should be penalised for tests its transport version
  cannot run. Both change who earns.
- Cross-operator sharing or attestation of probe results.
- Probing non-public providers, which would require a protocol change giving
  every provider a counterparty it cannot refuse.
- Automatic action on `suspect` verdicts, including
  `bandwidth_divergence`.
