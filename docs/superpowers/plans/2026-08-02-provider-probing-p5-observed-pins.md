# Provider Probing P5 — Server-Observed Certificate Pins

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** stop hardcoded certificate pins going stale and silently taking the whole fleet's geolocation probing offline. Move the pin set from a compile-time constant in the prober to a value the **server observes directly** and serves to the prober, so routine CA changes self-heal instead of causing an outage.

**Architecture:** the server already has direct, unproxied internet access. A periodic taskworker job connects to each geolocation source host **directly** — never through a provider tunnel — validates the certificate under full WebPKI (hostname + chain to a public root), and records the observed leaf and intermediate SPKI hashes. The prober fetches the current pin set from a new operator-secret endpoint, exactly as it already fetches its probe schedule, and fails closed if it cannot get one. A malicious provider still cannot forge a geolocation response, because it cannot influence what the server observed on the server's own network.

**Tech Stack:** Go 1.26.5, PostgreSQL 16, the existing taskworker + operator-secret ingest auth.

**Source spec:** `docs/superpowers/specs/2026-07-25-enforced-provider-geo-probing-design.md` — the *Threat model* section defines what the pin is for and therefore what may safely replace it.

**Predecessors:** P0, P1, P2, P4 — all complete and live on beta.

---

## Why this is safe, and the exact line it must not cross

Read this before writing code; it is the whole design.

**What the pin defends against.** The geolocation lookup is issued *through the provider's own tunnel*. A provider that could MITM that lookup could forge its own apparent location, which defeats the entire project. The pin exists to stop **the provider under test** substituting a certificate. It is not a general defence against the public internet.

**Why server observation is a valid trust anchor.** The server reaches these hosts **directly**, on its own network, with no provider in the path. A provider cannot influence what the server sees. So "what the server observed, with WebPKI validation" is strictly better grounded than "what a developer pasted into a constant six weeks ago", and it is the *same* trust the rest of the system already places in the server.

**The line that must not be crossed:** a pin may only ever be recorded from a **direct** server-side connection that passed full WebPKI validation. If a pin could be learned from a connection that traversed a provider tunnel, or from a connection whose chain was not verified, the provider becomes able to teach the server its own forged certificate — which would convert this from defence-in-depth into a self-inflicted vulnerability. Every task in this plan must preserve that line.

**What this deliberately does not do.** It does not weaken to trust-on-first-use for unknown hosts, and it does not let the prober run unpinned when the server is unreachable. Both would trade a rare outage for a permanent hole.

---

## Global Constraints

Everything from P2/P4 applies unchanged, plus:

- **Observation is direct-only.** The refresh job must use the server's own network. It must never reuse a provider tunnel or any client-supplied transport. Assert this in a test.
- **WebPKI is mandatory at observation time.** Hostname verification on, chain verified to the system roots, expiry checked. A host that fails validation must leave the stored pins **unchanged** and raise — never overwrite good pins with unverified ones.
- **The prober fails closed.** No pin set from the server means no probing. It must not fall back to unpinned, and it must not fall back to an empty pin map (`providertunnel.Open` already refuses that, keep it that way).
- **Rotation must be visible, not silent.** Every observed change is logged with old and new values. The point of the outage was that a pin failure was indistinguishable from "fewer sources responded".
- **Keep the intermediate pin.** `checkPin` matches any cert in the presented chain, which is what already absorbs routine leaf renewal. This plan fixes the *intermediate/CA change* case; it does not replace that mechanism.
- **Migrations append only.** Live beta index is **545** — re-check with `SELECT max(end_version_number) FROM migration_audit WHERE status='success'` at write time.
- **Two branches per server change** (beta + upstream cherry-pick), remotes inverted between checkouts, `git remote -v` before every push, merge nothing, never amend or force-push.
- **Never stop, recreate or `down` the live beta stack.** `git worktree` under `/root/wt/`, never `git checkout` in `/root/urnetwork/server`.

---

## File Structure

| File | Responsibility | Change |
| --- | --- | --- |
| `model/geolocation_source_pin_model.go` | **New.** Observed pin storage + read | Create |
| `model/geolocation_source_pin_model_test.go` | Tests | Create |
| `work/refresh_geolocation_source_pins.go` | **New.** Direct observation job + schedule | Create |
| `api/handlers/geolocation_source_pin_handlers.go` | **New.** Operator-secret GET | Create |
| `db_migrations.go` | One appended migration | Modify |
| `api/api.go` | Route registration | Modify |
| `taskworker/taskworker.go` | Schedule the refresh | Modify |
| `cmd/egress-prober/main.go` (operator-proxy) | Fetch pins from the server, fail closed | Modify |
| `ingest/` (operator-proxy) | Pin-set client | Modify |

---

## Task 1: Observe and store pins server-side

**Repo:** server, two branches.

**Schema** (appended migration; re-check the live index first):

```sql
CREATE TABLE IF NOT EXISTS geolocation_source_pin (
    host             text NOT NULL,
    leaf_spki        text NOT NULL,
    intermediate_spki text NOT NULL,
    observed_at      timestamp NOT NULL,
    PRIMARY KEY (host)
)
```

Upsert per host — current observation only. History is not needed; the change *log line* is what matters for noticing rotation.

**Observation function.** For each host in the known source set: dial `host:443` directly, full WebPKI (`tls.Config` with `ServerName` set and **`InsecureSkipVerify` false**), read the verified chain, compute the SHA-256 SPKI hash of the leaf and of the issuing intermediate, base64 them. On any validation failure: **leave the stored row untouched**, log loudly, and continue to the next host — one bad host must not blank the set.

**The host list must come from one place.** The server needs the same host set the prober's `geolocate/sources.go` defines. Do not hand-copy it — a second copy is exactly how `ip.pn` → `api.i.pn` broke. Put the list in one server-side constant with a comment naming `geolocate/sources.go` as its counterpart, and add a test asserting the two agree if any mechanism allows it; if they genuinely cannot be linked across repos, say so explicitly and make the comment load-bearing.

**Schedule:** every 6 hours via the taskworker, following the `work.Schedule*` pattern in `taskworker/taskworker.go`.

- [ ] Failing model test: upsert stores and replaces per host
- [ ] Migration appended after the live index
- [ ] Observation function with WebPKI mandatory; test that a self-signed / hostname-mismatched server leaves the prior row **unchanged** and returns an error (use `httptest.NewTLSServer` for the bad case)
- [ ] Test that a changed pin is logged with both old and new values
- [ ] Schedule registered in `taskworker.go`
- [ ] **Teeth-check:** disable WebPKI verification, show a self-signed cert being accepted and overwriting a good pin, restore, show it rejected

## Task 2: Serve pins to the prober, and consume them

**Repos:** server (two branches) + operator-proxy (`main`).

**Endpoint:** `GET /network/geolocation-source-pins` — operator secret, same auth as `/network/provider-egress-due`. Returns `{host: {leaf, intermediate}}` for every observed host.

**Prober side.** Replace the hardcoded `geolocatePins()` with a fetch from the server:
- Fetch at startup, and refresh periodically (an hour is ample; the server observes every 6).
- **Fail closed:** if the fetch fails at startup, the prober must not start probing. If a refresh fails later, keep using the last good set and log — do not degrade to unpinned.
- Keep `TestEveryGeolocationSourceHostHasPins`' intent alive: a source host with no pin in the served set must be a hard error, not a silently-unpinned host.
- The confinement self-check's address list is derived from the source hosts and is unaffected, but re-verify it still passes.

- [ ] Handler tests: auth fails closed (401 no-secret and wrong-secret); returns the observed set
- [ ] Prober tests: startup with an unreachable server does **not** begin probing; a refresh failure keeps the previous set; a served set missing a source host is a hard error
- [ ] Deploy beta: migrate, api, taskworker, prober. Confirm the job populates the table, the endpoint serves it, and a full fleet pass still reaches consensus (`submitted=N failed=0`)
- [ ] **Teeth-check:** point a source at a host whose served pin is wrong and show probing fails closed rather than proceeding unpinned

## Verification Summary

| Task | Gate |
| --- | --- |
| 1 | Pins observed only via direct WebPKI-validated connections; a failing host leaves prior pins intact; changes logged with old and new |
| 2 | Endpoint auth fails closed; prober fails closed with no pin set and never runs unpinned; fleet still reaches consensus after the switchover |
| All | Both PRs open per server change; nothing merged; migration appended after the live index; beta stack never taken down |

## Out of Scope

- Trust-on-first-use for hosts not already in the source set.
- Any fallback that lets the prober probe unpinned.
- Certificate Transparency monitoring or backup/next-cert pinning — a larger design, and direct observation plus fail-closed already removes the outage this fixes.
- Alerting/paging on rotation beyond a log line.
