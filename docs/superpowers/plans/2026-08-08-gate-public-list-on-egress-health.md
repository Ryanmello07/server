# Gate the Public Provider List on Egress Health

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** stop bad providers being handed to clients. A provider appears in the public list only if a probe has **measured it healthy**. Everything else — measured bad, or never measured — is silently absent.

**Architecture:** the gate goes in `UpdateClientScores`, at the point `PassesMinimums` is computed. That is the single writer of the precomputed score/filter sets that *both* `GET /network/provider-locations` and `POST /network/find-providers2` read, so one gate covers both surfaces without touching either read path. The probe due-queue is deliberately **not** gated, so a hidden provider is still probed and can graduate.

**Tech Stack:** Go 1.26.5, PostgreSQL 16, redis-backed score sets.

**Predecessors:** P0–P5 (probing, health persistence, verdicts, observed pins) — all live on beta.

---

## Why this exists — read before designing

The beta pool has **deliberately seeded free/bad proxies**. They are the test subjects, not noise. The entire probing effort exists to find them and **prevent them from ever serving clients**; a provider that fails should stay out indefinitely and only return if it later passes.

Until now the system only **measured**. Nothing acted on the measurement:

```go
// PassesMinimums, model/network_client_location_model.go — the whole gate today
if lookbackClientScore.IndependentReliabilityWeight < minFilter.minIndependentReliabilityWeights[i] { … }
if minFilter.maxScore <= lookbackClientScore.Scores[rankMode] { … }
```

Reliability weight and score. **Nothing reads egress health, the geolocation verdict, or probe failures.** A seeded proxy answering `0/131` still passes, because it stays *connected*, and connectivity is what reliability measures.

Live evidence (2026-08-07/08, verified):

| Cohort | Providers | Never tested | Dead `0/131` | Healthy ≥90% |
| --- | --- | --- | --- | --- |
| Original fleet (created pre-08-07) | 40 | 0 | 0 | **40** |
| Seeded batch (created 08-07) | 2337 | 2038 | 158 | 33 |

158 dead proxies were connected, Public and selectable. The original 40 all score 129–131/131.

## The decisions, already made — do not relitigate

Both were chosen by the user against this data:

1. **Pass = egress health ≥ 90% of destinations.** The healthy fleet sits at 129–131/131, so 90% cleanly separates working from broken.
2. **Never-tested = excluded.** Fail closed. This is the user's model: out until you pass. It hides ~2038 providers immediately and means a genuinely new provider is invisible until the prober reaches it (accepted cold-start cost).

Public list goes from ~2377 to **~73** on deploy. That is the intended outcome, not a regression.

## Global Constraints

- **Silent.** No error, no notification, no field telling a provider it was excluded. It simply does not appear in the public list. Do not add a user-visible "excluded" state to any API response.
- **Do not gate the probe due-queue.** `GetProviderEgressLocationDue` reads `network_client_location_reliability` + `provide_key` directly and is independent of `PassesMinimums` — verified. It must stay that way, or hidden providers can never graduate. Assert this in a test.
- **Do not touch `forceMinimum`.** It already exists so an operator census can see providers that fail the minimums gate; the health gate must behave the same way — `forceMinimum=true` callers keep seeing everything.
- **Recovery must be automatic.** A provider that later measures ≥90% must reappear with no manual step, on the next `UpdateClientScores` pass.
- **Stale health must not hide a good provider forever.** Decide explicitly how an old measurement is treated and say so — if health data ages out, the provider must not be silently stuck excluded because nothing re-probes it. Check the sweep/retention on `provider_egress_health` before choosing.
- **No migration unless genuinely needed.** This is a read-side gate over data that already exists.
- **Two branches** (beta + upstream cherry-pick), remotes inverted between checkouts, `git remote -v` before every push, merge nothing, never amend or force-push.
- **Never stop, recreate or `down` the live beta stack.** `git worktree` under `/root/wt/`, never `git checkout` in `/root/urnetwork/server`. Disk is tight — clean worktrees up.

## File Structure

| File | Responsibility | Change |
| --- | --- | --- |
| `model/network_client_location_model.go` | Load health, apply the gate in `PassesMinimums` | Modify |
| `model/provider_egress_health_model.go` | Bulk health lookup for scoring | Modify |
| `model/network_client_location_model_test.go` | Gate tests | Modify |

---

## Task 1: Gate `PassesMinimums` on measured health

**Repo:** server, two branches, based on the current beta tip.

**Load health once per pass.** `UpdateClientScores` already walks every client; add a single bulk read of `provider_egress_health` (client_id → ok_count, total_count, measured_at) rather than a per-client query. The table is small (hundreds of rows) — one query, held in a map.

**Apply the gate** where `PassesMinimums[rankMode]` is set. A provider passes only if it already passes today's checks **and** has a health record with `ok_count >= 0.9 * total_count`. Absent record → does not pass. Guard `total_count = 0` (treat as not passing; never divide by zero).

Put the 90% threshold in a named constant next to the other minimums, with a comment stating it was chosen because the healthy fleet scores 129–131/131.

**Do not change** the score/weight arithmetic. This is a gate, not a re-ranking: a provider either qualifies or is absent, and qualifying providers keep exactly today's ordering.

- [ ] Failing test: a provider with `0/131` is absent from the public/filter sets
- [ ] Failing test: a provider with no health record at all is absent
- [ ] Failing test: a provider at `129/131` is present, and its score/ordering is unchanged from today
- [ ] Failing test: a provider at exactly 90% is present; at 89% is absent (boundary)
- [ ] Failing test: `total_count = 0` does not panic and does not pass
- [ ] Test: `forceMinimum=true` still sees excluded providers (operator census unaffected)
- [ ] Test: **the probe due-queue still returns an excluded provider** — this is the graduation path, and if it breaks, a failed provider is stuck forever
- [ ] Test: a provider whose health improves to ≥90% reappears on the next pass with no manual step
- [ ] **Teeth-check:** remove the gate, show a `0/131` provider present in the filter set; restore, show it absent. Real before/after output.

## Task 2: Verify on beta

- [ ] Deploy; confirm `UpdateClientScores` completes without error
- [ ] `GET /network/provider-locations` shows only healthy providers; provider counts drop to roughly the healthy population
- [ ] Confirm **all 40 original providers survive** — they score 129–131/131 and must be unaffected. If any drops, the gate is wrong.
- [ ] Confirm a sample of the 158 dead proxies is absent from both `provider-locations` and `find-providers2`
- [ ] Confirm those same dead providers still appear in the probe due-queue
- [ ] Record the before/after public-list size

## Verification Summary

| Task | Gate |
| --- | --- |
| 1 | Only measured-healthy providers pass; untested and failing are absent; due-queue and `forceMinimum` unaffected; recovery automatic |
| 2 | All 40 originals survive; dead proxies gone from the public list but still probeable |

## Out of Scope

- Changing probe coverage or cadence (86% of the pool is unprobed — that is the next problem, and this gate makes it safe to leave unprobed providers hidden meanwhile).
- Demoting on client verdicts — the quorum still only reprioritises, by design.
- Any user-visible "why was I excluded" surface. This is deliberately silent.
- Bandwidth or geolocation verdict as gating inputs; health is the trustworthy instrument, bandwidth is advisory.
