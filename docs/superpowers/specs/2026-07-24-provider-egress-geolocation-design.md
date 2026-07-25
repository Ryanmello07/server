# Provider Egress Geolocation — Design

**Status:** Design approved (2026-07-24), pending implementation.
**Author:** Ryan Mello + Claude
**Branch:** `feat/provider-egress-geolocation` (off `beta/self-contained-env`), cherry-pick to upstream as a NEW PR.

> **Note on repo layout:** Sub-project A (the geolocation engine + egress prober)
> is destined for a **new operator-tools repo** (owner will create it under
> `urnetwork/` and fork it, e.g. `urnetwork/provider-locator`). Until that repo
> exists, this design doc lives in the server repo so the work is captured
> durably. When the repo is created, migrate the sub-project A portion of this
> doc (and its plan) there. Sub-project B is server-side and stays in this repo.

---

## Goal

Replace the paid IP-geolocation database (ipinfo, ~$50k/yr) with a free,
decentralized alternative: geolocate each provider by its **egress IP**, learned
by proxying geolocation lookups **through the provider itself**, cross-checked
across three independent free APIs, with the small free MaxMind GeoLite2 mmdb as
a fallback. This removes a five-figure recurring cost and — critically for a
decentralized product — lets **every** network operator run location resolution
for free, with no central paywall or rate limit, regardless of network size.

## Motivation & context

- Upstream pays ipinfo for a large DB (~$50k/yr). Any other operator running the
  software would face the same cost — antithetical to a decentralized product.
- The beta uses only the free GeoLite2 mmdb, which resolves most datacenter/
  mobile/VPN IPs to **country granularity only**. This recently surfaced a
  connection-teardown bug (country-only locations panicked on a NOT NULL insert
  and tore down connections); fixed in `7b69b5ae`, which also makes country-only
  location a fully first-class outcome — the foundation this design builds on.
- Empirical finding (three free APIs curled against the same host, 2026-07-24):
  - **Country: reliable.** All three agreed (US).
  - **City: unusable from free sources.** Three sources returned three different
    cities in three different states (~1,000 mi apart).
  - **ASN/org: reliable.** All three agreed (AS401486). Feeds the net_type
    "foreign/hosting" scoring that is currently all-zero in beta.
  - **IPv4 vs IPv6 matters** — one source resolved the v6 address and gave a
    different answer. Probes must pin the address family the provider egresses on.
- Conclusion: what the paid DB really buys is *reliable city-level* data. For a
  product where users pick **countries**, free multi-source consensus nails the
  part that matters and is honest about the part no free source can deliver.

## Hard constraints

1. **The operator's server must NEVER call the geolocation APIs directly.** Every
   geolocation request egresses **through a provider**, so the API observes the
   *provider's* egress IP and returns *its* location. A direct server call would
   geolocate the server (useless) and centralize/fingerprint the operator.
2. **Country is the product-critical, security-relevant signal.** City is
   best-effort. The design must degrade cleanly to country-only (already
   first-class after `7b69b5ae`).
3. **Providers are untrusted.** A provider is the network path for its own
   geolocation probe. The design must make it unable to *forge* a favorable
   location.

## Architecture overview

Two sub-projects across two repos, joined by one authenticated ingest endpoint:

```
  ┌─────────────────── NEW OPERATOR-TOOLS REPO (Sub-project A) ───────────────────┐
  │                                                                               │
  │  Spec A1: geolocation consensus engine (pure library)                         │
  │    Locate(ctx, httpClient) -> ConsensusLocation                               │
  │      - fans out to ip.pn / freeipapi / ipinfo through the INJECTED client     │
  │      - country majority vote; city only on agreement; ASN; net_type flags     │
  │      - transport-agnostic: cannot call out directly by construction           │
  │                                                                               │
  │  Spec A2: egress prober (operator tool, connect CLIENT role)                  │
  │    - builds connect.RemoteUserNatMultiClient targeted at ONE provider         │
  │      via ProviderSpec{ClientId: <provider>} (same primitive /proxy uses)      │
  │    - wraps it as an *http.Client with TLS cert pinning                        │
  │    - runs the engine through it -> ConsensusLocation for that provider        │
  │    - submits the result to the server's ingest endpoint (Sub-project B)       │
  │    - enumerates providers, schedules, caches, re-probes on change             │
  └───────────────────────────────────────┬───────────────────────────────────────┘
                                           │  authenticated HTTPS
                                           ▼
  ┌─────────────────────── SERVER REPO (Sub-project B) ───────────────────────────┐
  │  Spec B1: authenticated ingest endpoint                                        │
  │    POST /internal/provider-egress-location  {client_id, country, region?,      │
  │      city?, asn?, net_type flags, source_confidence, observed_at}              │
  │  Spec B2: storage (provider_egress_location table, keyed by client_id)         │
  │  Spec B3: integration — SetConnectionLocation prefers a stored egress          │
  │    location for a provider client over the mmdb lookup; mmdb is the fallback   │
  │    when no stored location exists (or it is stale).                            │
  └───────────────────────────────────────────────────────────────────────────────┘
```

**The seam between A1 and A2** is an injected `*http.Client`. A1 never
constructs its own client, so it *cannot* call out directly — in production the
only client it ever receives routes through a provider. This structurally
enforces hard constraint #1 and makes A1 unit-testable with a mock client.

---

## Sub-project A1 — Geolocation consensus engine

**One responsibility:** given an `*http.Client` (which, in production, egresses
through a provider), return a cross-checked `ConsensusLocation`, or an error if
too few sources responded. Pure HTTP + parsing + consensus. No connect/server
dependency. No knowledge of *how* the client is tunneled.

### Interface

```go
// Locate queries the configured geolocation sources through client and returns
// a cross-checked consensus. client, in production, egresses through a specific
// provider, so each source's no-IP "my location" endpoint returns that
// provider's egress location. Returns ErrNoConsensus if fewer than
// MinSources responded successfully.
func Locate(ctx context.Context, client *http.Client) (*ConsensusLocation, error)

type ConsensusLocation struct {
    CountryCode     string // ISO-3166 alpha-2, lowercased; "" if no country consensus
    Country         string
    CountryConfident bool  // true iff >= MinSources agreed on CountryCode

    // City/region are set ONLY when >= 2 sources agree exactly; otherwise "".
    City       string
    Region     string
    CityConfident bool

    ASN        int    // majority ASN across responders; 0 if none
    Org        string

    // net_type flags, OR-ed across sources (any source flagging -> true).
    Hosting bool
    Proxy   bool
    Mobile  bool

    Sources  []SourceResult // per-source raw outcome, for observability/debugging
    ProbedAt time.Time
}
```

### Sources (each behind a small parser normalizing to a common struct)

| Source | Endpoint (no-IP "my location" form) | Fields used |
|---|---|---|
| ip.pn | `https://ip.pn/json` | country, countryCode, city, asn, proxy/hosting/mobile |
| freeipapi | `https://free.freeipapi.com/api/json` | countryCode, cityName, regionName, asn, isProxy |
| ipinfo | `https://ipinfo.io/json` | country, city, region, org (→ ASN) |

Sources are defined declaratively (endpoint + parser + the pinned cert set) so
adding/removing a source is a one-line change. Each source is queried
**concurrently** with a per-source timeout (`PerSourceTimeout`, default 5s).

### Consensus rules

- **Country:** majority vote over responders. Accept iff `>= MinSources`
  (default 2) agree on the same `countryCode` → `CountryConfident = true`.
- **City / region:** set only if `>= 2` responders return the **same** city
  (normalized) — otherwise left empty (`CityConfident = false`). Live data
  proved free-source city disagreement is the norm, so this is expected.
- **ASN:** majority over responders (`0` if none/tie).
- **net_type flags:** logical OR across responders — a single source flagging
  `proxy`/`hosting`/`mobile` sets the flag (conservative; these feed abuse/
  quality scoring where over-flagging is safer than under-flagging).
- **Quorum:** if fewer than `MinSources` responded successfully, return
  `ErrNoConsensus` (the caller falls back to the mmdb — see B3). The engine does
  not itself do mmdb fallback; that keeps it pure and network-only.

### Security within A1

- Each source endpoint carries a **pinned certificate/SPKI set**. The engine
  applies pinning via the injected client's `TLSClientConfig` (A2 builds the
  client but A1 owns the pin material tied to each endpoint). A provider on the
  network path cannot present a forged cert for `ipinfo.io` etc., so it cannot
  MITM/alter responses. It can only choose which egress IP to present.
- Response size is capped (`MaxResponseBytes`, default 64KiB) to bound a hostile
  source/path.

### Testing (A1)

Fully unit-testable with an injected mock `*http.Client` backed by `httptest`
servers:
- all three agree → `CountryConfident`, city set if they match
- three-way city disagreement → city empty, country still set
- one/zero responders → `ErrNoConsensus`
- flag OR-ing (one source flags hosting → `Hosting=true`)
- per-source timeout / malformed JSON handled without failing the whole call
- oversized response truncated/rejected

No real network, no provider, no server.

---

## Sub-project A2 — Egress prober (operator tool)

**One responsibility:** for a given provider `client_id`, build a provider-routed
HTTPS client, run A1 through it, and submit the result to the server. Plus the
orchestration: which providers to probe, how often, caching.

### Provider-routed client (the reused primitive)

`/proxy`'s `socks/main.go` already demonstrates the exact machinery:

```go
generator := connect.NewApiMultiClientGenerator(
    []*connect.ProviderSpec{ { ClientId: &targetProviderId } }, // target ONE provider
    ...clientStrategy/apiUrl/platformUrl/byJwt/... ,
    connect.DefaultApiMultiClientGeneratorSettings(),
)
mc := connect.NewRemoteUserNatMultiClientWithDefaults(ctx, generator, ...)
```

`ProviderSpec{ClientId: &id}` pins the tunnel to a single provider. Wrap `mc`'s
dial path as an `http.RoundTripper`/`DialContext` (either directly, or via
`/proxy`'s SOCKS5 server pointed at `mc` and an `http.Transport` dialing that
SOCKS5 — TBD in A2's own plan, direct dialer preferred to avoid the SOCKS hop).
Apply A1's pinned TLS config. The resulting `*http.Client` egresses every
request through `targetProviderId`.

### Prober identity

The prober is a connect **client** and needs its own network client identity
(a `by_jwt` with a `client_id`), exactly like the SOCKS proxy takes a
`clientJWT`. The operator provisions a dedicated "geolocation prober" network/
client. This identity is prober config, not per-target.

### Orchestration

- **Which providers:** enumerate current providers (public provide mode) from
  the server — via an existing providers API or a small read endpoint. Probe
  a provider on first sighting and periodically thereafter.
- **Scheduling & concurrency:** bounded worker pool (N concurrent tunnels);
  each provider probe = build tunnel → A1.Locate → tear down → submit. Cap total
  in-flight tunnels to protect the operator's own resources.
- **Caching / re-probe:** cache by `client_id` with a TTL (default e.g. 24h);
  re-probe on TTL expiry. Optionally re-probe when the server signals the
  provider's control IP changed (future enhancement, not v1).
- **Failure handling:** if the tunnel can't be built, or A1 returns
  `ErrNoConsensus`, record nothing (the server keeps its mmdb fallback). Never
  submit a low-confidence guess as authoritative.

### Security within A2

- **Egress splitting** is the one residual attack: a provider routes probe
  traffic through a "clean" IP but user traffic through another. Mitigations
  (v1 documents the risk; hardening is follow-up):
  - Make probes indistinguishable from user traffic (same SNI patterns / timing;
    random schedule) so a provider can't cheaply detect "this is a probe."
  - Re-probe at random intervals.
  - Cross-check the consensus country against the provider's control-connection
    country (server side) and flag large disagreements for review.
- A provider can, at worst, **deny** being located (refuse to proxy the probe →
  `ErrNoConsensus` → server falls back to mmdb on the control IP). It cannot
  obtain a **forged** location (TLS pinning defeats response tampering).

### Testing (A2)

- Provider-routed client construction against a local fake provider / loopback
  egress; assert requests exit the intended path.
- Orchestration: scheduling, concurrency cap, cache TTL, re-probe, and
  submit-on-consensus / skip-on-no-consensus, with A1 and the server ingest
  mocked.

---

## Sub-project B — Server ingest & integration (server repo)

### B1 — Ingest endpoint

`POST /internal/provider-egress-location` (name TBD), **operator-authenticated**
(not open to normal client JWTs — only the operator's prober may write provider
locations). Auth options, decide in B's plan:
- a shared operator secret / dedicated service credential, or
- a privileged network role on the prober's JWT.

Body:
```json
{
  "client_id": "<provider client id>",
  "country_code": "us",
  "region": "",            // optional, only when city-confident
  "city": "",              // optional, only when city-confident
  "asn": 401486,
  "hosting": false, "proxy": false, "mobile": false,
  "country_confident": true,
  "city_confident": false,
  "observed_at": "2026-07-24T..."
}
```

The endpoint validates the client_id is a known provider, maps country_code →
the canonical `location` row (creating it via the existing `CreateLocation`
path), and upserts into storage (B2).

### B2 — Storage

New table `provider_egress_location`, keyed by `client_id`:
- `client_id` (PK), `location_id` (resolved canonical location, country-granular
  unless city-confident), `asn`, net_type flags, `country_confident`,
  `city_confident`, `observed_at`, `update_time`.
- Index for freshness sweeps (drop rows older than a max age so a provider that
  stops being probed eventually falls back to mmdb).

### B3 — Integration into SetConnectionLocation

Today `controller.SetConnectionLocation` → `GetLocationForIp(observed IP)` →
mmdb. Change: for a **provider** connection, first look up
`provider_egress_location[client_id]`:
- **hit + fresh:** use that location (the egress-derived, cross-checked one) and
  its net_type flags. This is the authoritative source.
- **miss / stale / not a provider:** fall back to the existing mmdb lookup on the
  observed control IP (already country-only-safe after `7b69b5ae`).

This keeps the mmdb as a graceful fallback everywhere and makes the
egress-derived location an override when available — so removing the paid DB
never leaves a provider un-located, only less-precisely located.

### Testing (B)

- Ingest auth (reject non-operator), validation (unknown client_id), upsert.
- `SetConnectionLocation` prefers a fresh stored egress location; falls back to
  mmdb on miss/stale; country-only stored location behaves (regression tie-in to
  `7b69b5ae`).

---

## End-to-end data flow

1. Prober (A2) picks provider P, builds a tunnel pinned to P via
   `ProviderSpec{ClientId: P}`, wraps it as a pinned `*http.Client`.
2. Prober runs A1.Locate through that client → the 3 APIs see P's **egress IP**
   → return P's location → consensus (country reliable, city if agreed, ASN,
   flags).
3. Prober submits the consensus to the server ingest endpoint (B1),
   operator-authenticated.
4. Server stores it (B2), keyed by P's client_id.
5. When P (re)connects, `SetConnectionLocation` (B3) uses the stored
   egress-derived location instead of the mmdb; mmdb is the fallback.
6. Provider location flows into the existing reliability/matching pipeline
   unchanged — P is now matchable at (at least) country granularity, derived for
   free.

## Security model summary

| Threat | Mitigation | Residual |
|---|---|---|
| Provider forges a favorable location | TLS cert pinning on the 3 API endpoints — provider is a dumb encrypted pipe, cannot alter responses | none (short of a CA compromise) |
| Provider denies being located | Falls back to mmdb on control IP; provider just stays coarser | acceptable |
| Provider egress-splits (clean IP for probes, dirty for users) | Indistinguishable + randomized probes; control-country cross-check flag | fundamental to decentralized systems; documented, hardening is follow-up |
| Non-operator writes provider locations | Operator-authenticated ingest endpoint | none |
| Server IP fingerprinted by API calls | Server never calls APIs; all calls egress through providers (constraint #1) | none |

## Phasing / decomposition

Each piece is independently buildable and testable; build order:

1. **A1 — consensus engine** (new repo): pure, testable now, defines the output
   contract. *Start here.*
2. **B — server ingest + integration** (server repo): consumes A1's output
   contract; can be built in parallel once the contract is fixed.
3. **A2 — egress prober** (new repo): depends on A1 + the connect client lib +
   `/proxy` tunnel primitives; wires the tunnel and submits to B. The largest,
   riskiest piece (touches the core data path).

## Open questions (resolve in each sub-project's plan)

- A2: direct `RemoteUserNatMultiClient` dialer vs a local SOCKS5 hop for the
  http.Client — prefer direct; confirm in A2's plan.
- B1: ingest auth mechanism (shared secret vs privileged network role).
- Re-probe cadence / cache TTL concrete values (start 24h).
- Egress-splitting hardening depth for v1 (document risk; ship basic randomized
  indistinguishable probes; deeper detection is follow-up).

## Rollout

- Sub-project B (server): branch `feat/provider-egress-geolocation` off
  `beta/self-contained-env` → cherry-pick to the upstream-facing branch → NEW
  upstream PR. Opus review agents after cherry-pick, per standing workflow.
- Sub-project A (new repo): created by the owner under `urnetwork/` and forked;
  this doc's A portion migrates there; its own beta → upstream PR cycle.
- The `7b69b5ae` country-only fix is already shipped and is the safe fallback
  this whole design leans on.
