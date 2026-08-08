# Gated provider locations — design

Date: 2026-08-08
Status: approved (approach B)

## Problem

`/network/provider-locations` is the endpoint every app pulls to build its
location list. Its `provider_count` ignores the egress-health gate. On the beta
deployment today it advertises **305** providers for the United States while
only **151** providers in the entire fleet pass the health gate, and only a
subset of those are in the US.

The membership of the list is already gated — `UpdateClientScores` folds
`passesEgressHealth` into `PassesMinimums`
(`model/network_client_location_model.go:2962`), and every consumer reads that
flag. The **count** is not: it comes from a different job,
`UpdateClientLocations`, which increments `locationClientCounts[locationId]`
at `:1734` with no health predicate, stores it as `ClientLocation.ClientCount`,
and it is surfaced as `ProviderCount` at `:2200` and `:2288`.

So a location that survives the gate still over-reports how many providers it
can actually offer.

## Non-goals

Two access paths must keep working, and both already do. No work is planned for
either; they exist here as regression tests, not as changes.

* **Direct client-ID connections.** `spec.ClientId` at `:3351` appends the
  provider straight to the result and never consults the score sets or
  `PassesMinimums`. It only checks `excludeFinalDestinations()`. A gated
  provider remains reachable by ID today.
* **Same-network connections.** Confirmed with the user that same-network
  clients reach their own providers by client ID, which is the path above. No
  caller-network exception is needed in the location path, and the location
  path therefore stays fully gated.

Because neither exception is needed, health does **not** need to be unfused
from `PassesMinimums`. An earlier draft of this design proposed splitting them;
that is dropped as unnecessary.

## Approach

Filter the count in Go at the increment, sharing one predicate with the score
job.

`UpdateClientLocations` loads egress health once per pass via
`GetAllProviderEgressHealthCounts` — the same bulk call `UpdateClientScores`
already makes, one query, not one per provider — and skips clients that fail
health when incrementing `locationClientCounts`.

The predicate itself is extracted from `UpdateClientScores` into a shared
helper so both jobs call the identical function. This is the point of the
design: the count and the membership become incapable of disagreeing, because
there is only one definition of "healthy" to disagree about.

Rejected alternatives:

* **Filter in SQL** (add an `EXISTS` against `provider_egress_health` to the
  query at `:1645`). Fewer lines, but it puts the 90% threshold in two places —
  Go and SQL — which can drift silently. That drift is exactly the failure this
  design exists to prevent.
* **Derive the count from the gated score sets at read time.** Truest single
  source of truth, but it reworks a hot read path that every app polls, for no
  behavioural gain over the shared predicate.

## Behaviour

The health predicate is unchanged from the deployed gate: a provider passes
when it has a health record and `10*ok >= 9*total` — integer arithmetic, no
float, so the 90% boundary cannot drift. **A provider with no health record
fails** (fail-closed), matching the deployed behaviour and the explicit choice
made when the gate shipped.

`ClientLocation` is gob-encoded into redis with a TTL, so for up to one TTL
after deploy the endpoint serves the previous, ungated counts. This is a
self-correcting stale read, not a leak: membership is gated independently by
`PassesMinimums`, so no hidden provider becomes reachable. It is called out
here so the first post-deploy reading is not mistaken for a failure.

No new field is added to any gob-serialised struct, so the zero-value decode
hazard that `NetworkOnly` documents at `:2413` does not apply.

## Testing

Unit, in `model/`:

1. A location whose providers all fail health reports `ClientCount == 0`.
2. A location with a mix reports only the passing providers.
3. A provider with **no** health record is excluded (fail-closed).
4. The 90% boundary: `117/131` fails, `118/131` passes — asserted against the
   integer comparison, not a float.
5. The count produced by `UpdateClientLocations` equals the number of providers
   `UpdateClientScores` admits for the same fixture. This is the regression
   test for the shared predicate; it fails if the two ever diverge.

Regression, protecting the non-goals:

6. A provider failing health is still returned for an explicit
   `spec.ClientId` request.

Live verification on beta, after deploy:

7. `provider_count` for each returned location equals the count of
   health-passing providers for that location in Postgres.
8. A known-unhealthy provider is still reachable by client ID.

## Out of scope — recorded, not fixed

Diagnosed while investigating this, kept separate because it is a different
defect with a different cause:

**Locations with healthy providers are missing from the list.** The endpoint
returns 1 location for a US caller, yet healthy providers exist in Germany
(25), the UK (16), Spain (14) and others. Traced to `loadLocationStables`
(`:2036`), which drops any location whose pre-computed `ClientFilter.Count` is
zero. Sampling redis for a US caller: of 60 locations, 8 have providers under
the gated key family and 36 under the census family — so ~28 locations hold
providers that fail the minimums.

This is **not** caused by the health gate. Germany's 25 providers pass health;
they fail the pre-existing reliability/score minimums that `PassesMinimums`
also requires. The most likely cause is that the entire fleet reconnected
during the 2026-08-08 server migration, resetting the lookback reliability
history those minimums depend on, in which case it recovers as history
accumulates. That hypothesis is **not yet confirmed** and needs a time series
before anyone acts on it.

Related: the `forceMinimum=true` ("operator census") key family is written by
`UpdateClientScores` but read by no endpoint — both callers of
`loadLocationStables` pass `false`. Operators therefore have no way to see
locations whose providers all fail the minimums, which is precisely the
visibility needed to diagnose the above.
