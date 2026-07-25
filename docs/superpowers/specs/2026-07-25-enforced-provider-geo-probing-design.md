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

## Decomposition

This is too large for a single implementation plan. It splits into four, each
producing working, testable software on its own:

**P0 — Prerequisites** (small, independent, ship first). Pass
`force_minimum: true` from the prober's enumeration, and make the provider count
provide-mode aware so the location list stops advertising clients that cannot
accept a contract. Neither depends on anything else here, and the second is a
user-visible bug today.

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
- Cross-operator sharing or attestation of probe results.
- Probing non-public providers, which would require a protocol change giving
  every provider a counterparty it cannot refuse.
- Automatic action on `suspect` verdicts.
