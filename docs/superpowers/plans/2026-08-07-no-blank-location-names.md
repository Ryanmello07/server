# No Blank Location Names

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** make a location row with an empty `location_name` impossible — fix the code path that creates them, fill in the names that are missing, backfill the rows that already exist, and add a database constraint so the class of bug cannot recur silently.

**Architecture:** the name gap is closed in Go rather than in config, because the config file is per-deployment and a config-only fix would not travel. A complete ISO-3166-1 alpha-2 code→name table lives in the model and is consulted whenever a caller supplies a country code without a name; the existing `iso-country-list.yml` continues to win where it has an entry, so no deployment loses its current naming. A `CHECK` constraint on `location` is the structural backstop, added only after the backfill so it cannot fail on existing data.

**Tech Stack:** Go 1.26.5, PostgreSQL 16.

## The bug, already diagnosed — do not re-derive it

`model.AddDefaultLocations` creates countries by two paths and only one supplies a name:

```go
// path 1 -- createCountry, from iso-country-list.yml: HAS the name
createCountry(countryCode, country.(string))

// path 2 -- createLocationGroup's member `case string`: NO name
location := &Location{
    LocationType: LocationTypeCountry,
    CountryCode:  v,          // Country is never set
}
CreateLocation(ctx, location)
```

`CreateLocation` writes `location_name` from `location.Country`, so path 2 inserts `''`.

Evidence from live beta, for reference (all verified):

- 220 country rows: **58 named**, **161 blank**. `iso-country-list.yml` has exactly **58 entries**.
- All 161 blanks are location-group members; none appear in the ISO list.
- Every one of the 220 rows has `location_full_name = country_code`, so all came through the same insert.
- All 219 initial rows were created within 1.2s on 2026-07-18 — one `AddDefaultLocations` run.
- 2 **region** rows are blank the same way (`hk`, `sg`), and their `location_full_name` is `", hk"` / `", sg"` — the empty name concatenated in.
- User-visible: `GET /network/provider-locations` returns 5 of 28 locations with `"name": ""`, including `ru` (246 providers) and `cn` (272). **17** blank-named countries have clients.
- The shortfall is not beta-only: `/srv/warp/config/iso-country-list.yml` has **57** entries.

## Global Constraints

- **The fix must be in code, not config.** `iso-country-list.yml` is a per-deployment vault resource; only `beta-vault/`'s copy is in the repo. A config-only change would fix beta and leave every other deployment broken.
- **Config still wins.** Where `iso-country-list.yml` has a name, use it. The Go table is a fallback for codes the file omits, not a replacement — some deployments may deliberately use their own naming.
- **Never invent a name.** If a code is in neither the config nor the ISO table, that is a real error and must surface, not be papered over with the code as the name. See Task 1 on what to do instead.
- **Backfill before constraint.** Adding the `CHECK` first would fail the migration on the 163 existing rows.
- **Migrations append only** — live beta index is **546**; re-check with `SELECT max(end_version_number) FROM migration_audit WHERE status='success'` at write time.
- **Two branches** (beta + upstream cherry-pick), remotes inverted between checkouts, `git remote -v` before every push, merge nothing, never amend or force-push.
- **Never stop, recreate or `down` the live beta stack.** `git worktree` under `/root/wt/`, never `git checkout` in `/root/urnetwork/server`.

## File Structure

| File | Responsibility | Change |
| --- | --- | --- |
| `model/iso_country_names.go` | **New.** Complete ISO-3166-1 alpha-2 code→name table | Create |
| `model/iso_country_names_test.go` | Tests | Create |
| `model/network_client_location_model.go` | Resolve names in `CreateLocation`; fix the group-member path | Modify |
| `db_migrations.go` | Backfill + `CHECK` constraint | Modify — appended only |

---

## Task 1: Resolve the name at creation time

**Repo:** server, two branches.

**Add** `model/iso_country_names.go`: `func ISOCountryName(countryCode string) (string, bool)` over a complete ISO-3166-1 alpha-2 table (~249 entries), lowercase-keyed. Generate the table from a real source rather than hand-typing it; state in the file comment where it came from and on what date.

**Change `CreateLocation`** so a country row can never be written with an empty name. When `location.Country` is empty, resolve in this order:
1. the `iso-country-list.yml` entry for the code, if present (config wins — see Global Constraints);
2. `ISOCountryName(code)`.

If neither resolves, the code is not a real country: **return an error / do not insert**, rather than inserting a blank or using the code as the name. Today `CreateLocation` returns nothing and panics via `server.Raise`; follow the surrounding convention, but the row must not be created. Log the offending code.

**Fix the group-member path** in `AddDefaultLocations` so its `case string` supplies the resolved name instead of a bare code. That is the specific line that produced all 161 rows.

**Regions:** the same guard applies — the 2 blank regions (`hk`, `sg`) come from a region row created with an empty `Region`. A region or city with an empty name must not be inserted either. Note their `location_full_name` shows the empty name was concatenated (`", hk"`), so fixing the insert also fixes the full name for new rows.

- [ ] Failing test: `CreateLocation` with `CountryCode:"cn"` and no `Country` stores `China`, not `''`
- [ ] Failing test: the config file wins over the Go table when both have the code
- [ ] Failing test: an unknown/garbage code does **not** create a row
- [ ] Failing test: a region with an empty name does not create a row
- [ ] `ISOCountryName` covers every one of the 161 codes observed blank on beta — assert against that exact list (it is in this plan's diagnosis section; pull the live list with the query in Task 3 if you want to regenerate it)
- [ ] **Teeth-check:** revert the resolution in `CreateLocation`, show a blank row being written, restore, show it named

## Task 2: Backfill and forbid

**Repo:** server, two branches. Appended migration only.

**Backfill**, in this order:
1. `UPDATE location SET location_name = <resolved> WHERE location_name = '' AND location_type = 'country'` — resolve from the same ISO table. The migration cannot call Go, so emit the mapping as a SQL `VALUES` join generated from `ISOCountryName`; do not hand-write 161 UPDATE statements.
2. Fix `location_full_name` on the rows you touch — for countries it is currently the bare code; for the 2 regions it is `", hk"`-shaped. Make it consistent with how a correctly-created row of that type looks (check `CreateLocation` for the exact composition rather than guessing).
3. Region rows: resolve from the country plus the region name if recoverable; if a region's name genuinely cannot be recovered, **delete the row** only if nothing references it, otherwise leave it and say so — do not invent a name.

**Then** add the constraint, in the same migration file but after the backfill:

```sql
ALTER TABLE location
    ADD CONSTRAINT location_name_not_blank
    CHECK (location_name <> '')
```

Verify against the live beta count first: 163 rows need backfilling (161 country + 2 region). If the constraint would still fail after your backfill, that means a row you did not account for — investigate it, do not weaken the constraint.

- [ ] Backfill migration appended after the live index
- [ ] Constraint added after the backfill in the same append
- [ ] Test: the migration leaves zero rows with a blank name
- [ ] Test: inserting a blank-named location now fails at the database
- [ ] **Teeth-check:** attempt a blank insert directly against a scratch database with the constraint applied; show the rejection

## Task 3: Verify on beta

Deploy and confirm, with the queries used in the diagnosis:

```sql
SELECT location_type, count(*) FILTER (WHERE location_name='') FROM location GROUP BY 1;
SELECT country_code FROM location WHERE location_type='country' AND location_name='' ORDER BY 1;
```

- [ ] Migrate beta; `db_version` advances; zero blank names remain
- [ ] `GET /network/provider-locations` returns **0** entries with `"name": ""` (it returns 5 today, incl. `ru` and `cn`)
- [ ] The 17 blank-named countries that have clients now render with names
- [ ] A fresh `AddDefaultLocations` run on a scratch database produces zero blank names — this is the real regression test, since that function created the mess
- [ ] Existing named countries are unchanged (58 named before, still named, same names)

## Verification Summary

| Task | Gate |
| --- | --- |
| 1 | No path can create a blank-named location; config wins over the Go table; unknown codes error rather than insert |
| 2 | 163 existing rows backfilled; `CHECK` constraint present and enforcing |
| 3 | Beta shows zero blanks in db and API; `AddDefaultLocations` is idempotently clean on a fresh db |

## Out of Scope

- Renaming or re-curating existing non-blank names.
- Changing which countries the location groups reference.
- `location_full_name` composition for rows that are already correct.
- Extending `iso-country-list.yml` in any deployment's vault — the Go fallback makes that unnecessary, and editing another deployment's config is not ours to do.
