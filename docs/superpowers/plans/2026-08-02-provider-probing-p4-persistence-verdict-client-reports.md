# Provider Probing P4 — Health Persistence, Verdict Wiring, Client Reports

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Connect the instruments this project has already built to things that can act on them. Egress-health results currently exist only as container log lines; `probeverdict` is dead code with zero call sites; and the client-side reliability work (see source spec 2) has a verdict struct ready to send with no endpoint to receive it. Three tasks: persist health, compute verdicts, receive client reports.

**Architecture:** One new table (`provider_egress_health`) written through one new operator-secret ingest endpoint, mirroring the bandwidth-result pattern exactly. `probeverdict` gets called from the one place a submission already flows through (`SubmitProviderEgressLocation`), so verdicts appear as a side effect of the existing probe cadence with no new scheduler. Client verdicts land in an append-only `provider_client_verdict` table through an authed-session endpoint; aggregation runs at read time with a distinct-reporter-network quorum, and a met quorum **only reprioritises the provider for probing** — it never excludes. The prober, armed with the multi-destination table from this week's work, remains the sole authority for demotion.

**Tech Stack:** Go 1.26.5, PostgreSQL 16, the existing operator-secret ingest auth, standard client-JWT session auth.

**Source specs:**
1. `docs/superpowers/specs/2026-07-25-enforced-provider-geo-probing-design.md` — verdict lifecycle.
2. The client-reliability punishment spec (uploaded 2026-07-30, "Punishing egress-dead providers using client verdicts") — client verdict shape and aggregation. **Amended by review:** demotion-on-quorum is replaced by probe-prioritisation-on-quorum. Rationale recorded in the session ledger: client verdicts are the harder signal to game, but the spec's "prober rehabilitates immediately" rule turns immediate exclusion into a laundering mechanism, and `NetworkCreateDailyLimit = 5` makes a 3-network quorum cost ~15 minutes of sybil effort. Trigger and punishment are therefore separated.

**Predecessors:** P0, P1, P2 Tasks 1–6 — all complete and live-verified on beta.

## Global Constraints

Everything from the P2 plan applies unchanged, plus:

- **Reputation is not health.** The health table stores the reputation figures (they are measured in the same pass) but nothing downstream may fold them into a health score. The `egresshealth` package comment explains why; the same reasoning holds server-side.
- **Client verdicts never gate directly.** A met quorum reprioritises the provider in the probe due-queue. It must not touch filter sets, scores, `PassesMinimums`, or selection. If the prober then confirms, the *existing* probation machinery does the gating — this plan adds no new gate.
- **One reservation of counted-verdict per reporter network per provider per window.** Raw verdict rows are append-only and unbounded per reporter; the dedup happens at aggregation time. Cap what a single network can *count for*, not what it can *say*.
- **Migrations append only.** The migration index on the deployed beta db is at 542; anything this plan adds starts there. Check `SELECT max(end_version_number) FROM migration_audit` before writing the migration, not after.
- **Two branches per server change** (beta + upstream cherry-pick), remotes are inverted between checkouts, check `git remote -v` before every push, merge nothing, never amend or force-push.
- **Never stop, recreate or `down` the live beta stack.** Use `git worktree` under `/root/wt/`, never `git checkout` in `/root/urnetwork/server`.

---

## File Structure

| File | Responsibility | Change |
| --- | --- | --- |
| `model/provider_egress_health_model.go` (server) | **New.** Health result storage, upsert per (client_id) | Create |
| `api/handlers/provider_egress_health_handlers.go` | **New.** Operator-secret POST | Create |
| `controller/provider_egress_location_controller.go` | Call `probeverdict` on submit | Modify |
| `model/provider_client_verdict_model.go` (server) | **New.** Append-only client verdict rows + quorum aggregation | Create |
| `api/handlers/provider_client_verdict_handlers.go` | **New.** Authed-session POST | Create |
| `db_migrations.go` | Two appended migrations (health table; client-verdict table) | Modify |
| `api/api.go` | Route registration | Modify |
| `ingest/` + `prober/` (operator-proxy) | Submit health results after each check | Modify |

---

## Task 1: Persist egress-health results

**Repos:** server (two branches off the beta tip / stacked upstream) + operator-proxy (`main`).

**Context:** the prober computes `ok/total` per class plus reputation per provider per pass and logs it. Nothing stores it. The bandwidth-result flow (Task 5/6 of P2) is the exact template: operator-secret POST, validated payload, model upsert.

**Schema** (appended migration, index from live db):

```sql
CREATE TABLE IF NOT EXISTS provider_egress_health (
    client_id uuid NOT NULL,
    measured_at timestamp NOT NULL,
    ok_count int NOT NULL,
    total_count int NOT NULL,
    class_results jsonb NOT NULL,      -- {"dns":{"ok":7,"total":7}, ...}
    reputation_ok int NOT NULL,
    reputation_total int NOT NULL,
    failed_names text NOT NULL DEFAULT '',
    reputation_failed_names text NOT NULL DEFAULT '',
    PRIMARY KEY (client_id)
)
```

Upsert on submit — latest result per provider, same lifecycle as `provider_egress_location`. History can move to a partitioned append table later if trending is wanted; do not build that now.

**Endpoint:** `POST /network/provider-egress-health` — operator secret header, reject unknown fields, `ok_count <= total_count`, class totals must sum consistently with `total_count`, non-negative everything. 400 on violation, never store-then-flag.

**Prober:** submit after each health check, fire-and-forget with the same dedup-logged-error pattern `Attempts` uses. A submit failure must not fail the probe.

- [ ] Failing model test: store + read back, upsert replaces
- [ ] Migration appended; verify against live index first
- [ ] Handler tests: auth fails closed (401 both no-secret and wrong-secret), validation 400s, valid 200 + row
- [ ] Prober submit + tests (httptest ingest stub; submit failure does not fail the pass)
- [ ] Deploy beta: migrate, api, prober; fleet pass; **46 rows** (or current fleet size) in the table
- [ ] Teeth-check: validation rule (ok>total rejected) — break it, show the failure, restore

## Task 2: Wire probeverdict into ingest (P2 Task 7)

**Repo:** server (two branches). The `probeverdict` package already exists — committed as `dd17ebdb` / `c2151c68` on `feat/provider-egress-verdict` (beta, PR #12) and `feat/provider-egress-verdict-upstream`. It is complete, tested, and has **zero call sites**. This task is only the wiring, and the beta tip that gets deployed does not currently contain the package, so the branch that carries this change must be based on / merged with `feat/provider-egress-verdict` so the package is present.

**Context:** every geolocation submission already flows through one function, `controller.SubmitProviderEgressLocation` (`controller/provider_egress_location_controller.go:65`). That is the single place a verdict should be computed — as a side effect of the existing probe cadence, with no new scheduler and no new endpoint. Today all 46 beta rows read `verdict='unverified'` because nothing calls the package.

**The two verdict rules (from the package, do not re-derive):** absence of consensus → `unverified`; country instability across the stored history window → `suspect`. A probed country differing from the mmdb country is **not** suspicious — that divergence is the whole point of the project (Global Constraints). There is no RTT corroboration.

**Interfaces:**
- Consumes: `probeverdict.Compute(probeverdict.Input{...}) probeverdict.Verdict` — feed it the submission's consensus state, the confident country, and the previously-stored country + observed_at (which `SubmitProviderEgressLocation` already reads for the same row).
- Produces: `verdict.State` and `verdict.Reason` passed through to the writer. Extend `model.SetProviderEgressLocation`'s args struct with `Verdict` / `VerdictReason` (and `Assurance` stays `'direct'`), defaulting to `"unverified"` / `""` for any other caller so nothing else regresses.

**Steps:**
- [ ] Confirm the base carries the `probeverdict` package (`go build ./probeverdict/` on the branch) before writing any wiring.
- [ ] Failing test in `controller`: a clean, consensus-backed submission stores `verified`; a country flip-flop within the history window stores `suspect` with the right reason; a no-consensus submission stores `unverified`. Assert the mmdb-divergence-is-not-suspect property explicitly.
- [ ] Wire `Compute` into `SubmitProviderEgressLocation`, threading verdict + reason into the existing write. No second read of the row — reuse the previous-country/observed-at it already loads.
- [ ] `./test.sh -run TestSubmitProviderEgressLocation`; `go build ./... && go vet ./...`.
- [ ] Deploy beta: api only (no migration — the verdict columns shipped in P2 Task 2, already at db 542). Force a fleet pass; verify rows now carry real verdicts, and that a stable provider reads `verified` rather than the `unverified` default.
- [ ] Teeth-check: feed the wiring a synthetic flip-flop and show it flips `verified`→`suspect`, then restore.

## Task 3: Client-verdict endpoint and quorum aggregation

**Repos:** server (two branches). No prober change — clients are the reporters.

**Context:** the client-reliability spec (source spec 2) has a verdict struct ready at the blackhole-removal site and an authed API client to send it; there is no endpoint to receive it. This builds the receiver and the griefing-resistant aggregation, **amended per review**: a met quorum reprioritises the provider for probing, it does **not** demote. Trigger (client verdicts, fast, hard to game) and punishment (the prober, the authority) stay separated. See the source-spec note above for why immediate exclusion would be a laundering / sybil vector.

**Schema** (appended migration, verify live index is 542 + Task-1's migration first):

```sql
CREATE TABLE IF NOT EXISTS provider_client_verdict (
    provider_client_id uuid NOT NULL,
    reporter_network_id uuid NOT NULL,
    reason text NOT NULL,                 -- the blackhole reasons: no-receive-ack etc.
    send_ack_count bigint NOT NULL,
    send_ack_bytes bigint NOT NULL,
    receive_ack_count bigint NOT NULL,
    receive_ack_bytes bigint NOT NULL,
    window_seconds int NOT NULL,
    create_time timestamp NOT NULL,
    PRIMARY KEY (provider_client_id, reporter_network_id, create_time)
)
```

Append-only. A reporter may say anything as often as it likes; the cap is on what it can *count for*, applied at aggregation, not on what it can write.

**Endpoint:** `POST /network/provider-verdict` — **authed client session** (not operator secret; the reporter is a real network). Body per the spec: `exit_client_id`, `reason`, the ack/syn stats, `window_seconds`. Validate the reason against the known blackhole set, reject unknown fields, non-negative counts. Record `reporter_network_id` from the session, never from the body.

**Aggregation** (read-time, pure/testable): a provider is *quorum-met* when `>= 3 distinct reporter_network_id` have egress-dead verdicts (`receive_ack_count == 0`) within a 15-minute window, counting **at most one verdict per reporter network per provider per window**. Decay: rows outside the window do not count. This mirrors the client-side dial-strike shape (3 strikes / 60s / any success clears).

**Effect of quorum:** reprioritise the provider in the egress-probe due-queue — i.e. make it *due now* (the same mechanism the operator diagnostics used: bring its `observed_at` forward). It must not touch filter sets, scores, `PassesMinimums`, or `find-providers2`. If the prober then confirms egress-dead, the existing P1 probation machinery gates it; this plan adds no new gate.

**Steps:**
- [ ] Migration appended after Task 1's; live index re-checked at write time.
- [ ] Failing model tests: three distinct reporters within the window → quorum met; the same reporter three times → not met (one-per-reporter cap); a verdict with `receive_ack_count > 0` → does not count; a verdict outside the window → does not count.
- [ ] Handler tests: session auth fails closed (401 unauth); `reporter_network_id` taken from the session even if the body lies; unknown reason / unknown field / negative count → 400; valid → 200 + row.
- [ ] Wire quorum-met → reprioritise-for-probe. Test that it moves the due-time and touches nothing in the selection path.
- [ ] Deploy beta: migrate, api. Simulate three reporter networks posting egress-dead verdicts for one provider; show the provider becomes due; confirm no filter-set / score change.
- [ ] Griefing teeth-check: one network posting three times does **not** meet quorum; show it, then show three distinct networks does.

## Verification Summary

| Task | Gate |
| --- | --- |
| 1 | Health rows land for the whole fleet; validation rejects `ok>total` and inconsistent class sums; auth fails closed; reputation stored but not folded into any score |
| 2 | A stable provider reads `verified`; a country flip-flop reads `suspect`; mmdb divergence alone never suspect; all other `SetProviderEgressLocation` callers unaffected by the new args |
| 3 | Three distinct reporters meet quorum; one reporter ×3 does not; `receive_ack>0` and stale rows excluded; quorum reprioritises for probe and touches nothing in selection; session auth fails closed |
| All | Both PRs open per server-side task; nothing merged; migrations appended after live index 542; beta stack never taken down |

## Out of Scope

- Any new gate. Quorum reprioritises; the prober remains the sole demotion authority.
- Folding reputation or bandwidth into health, scores, or `PassesMinimums` (advisory stays advisory).
- Client-side selection changes — none needed; the Redis filter sets already control what clients see.
- Payout/quota interaction — verdicts never touch payout logic.
- Identity rotation and multi-hop (`assurance` stays `'direct'`) — P3.
- History/trending on the health table — upsert-latest only for now.