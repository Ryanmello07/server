# Geolocation Consensus Engine (Sub-project A1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A pure Go library that, given an injected `*http.Client`, queries three free geolocation APIs and returns a cross-checked consensus location.

**Architecture:** Three small files in one package `geolocate`: types + the fan-out entrypoint (`geolocate.go`), the pure consensus algorithm (`consensus.go`), and the three source definitions + JSON parsers (`sources.go`). The engine takes an injected `*http.Client` and never constructs its own — in production that client egresses through a provider, so the engine structurally cannot call the APIs directly.

**Tech Stack:** Go 1.26.3, standard library only (`net/http`, `encoding/json`, `context`, `sync`, `io`, `time`, `strings`, `strconv`, `errors`). No dependency on `urnetwork/connect`, `urnetwork/server`, or any tunnel code.

**Target repo:** `github.com/urnetwork/urnetwork-operator-proxy` (fork: `Ryanmello07/urnetwork-operator-proxy`). This plan doc is co-located with the design spec in the server repo for durability; the code it describes is created in the operator-proxy repo. When implementation starts, scaffold the module there (Task 1).

**Source spec:** `docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md`, section "Sub-project A1".

## Global Constraints

- Pure library: standard library only. No `urnetwork/*` imports, no tunnel/socket/provider code.
- The engine MUST take an injected `*http.Client` and MUST NOT construct its own `http.Client` or make any network call except through the injected client. (Enforces spec hard-constraint #1: the operator server never calls the APIs directly.)
- `MinSources = 2` (const): a country verdict requires at least 2 sources to respond AND at least 2 to agree on the country.
- `MaxResponseBytes = 64 * 1024` (const): per-source response body cap.
- `PerSourceTimeout = 5 * time.Second` (package var, so tests can lower it).
- Country codes are normalized to lowercase ISO-3166 alpha-2 in all output and comparisons.
- Consensus: country = majority vote (≥ `MinSources` agree) → `CountryConfident`; city set only if ≥ 2 sources agree on the normalized city; ASN = plurality over non-zero ASNs; `Hosting`/`Proxy`/`Mobile` = logical OR across responders.
- Deviation from spec (deliberate): TLS certificate pinning is NOT in A1. Pinning is a property of the client, which A2 builds; A1 stays transport-agnostic. A2's plan owns pinning.
- License header/module: repo is MPL-2.0 (LICENSE already present). No per-file license headers required (matches sibling repos).

---

### Task 1: Module scaffold, types, and consensus algorithm

**Files:**
- Create: `go.mod`
- Create: `geolocate/geolocate.go` (types + constants + `ErrNoConsensus` only in this task; `Locate`/`fetchSource` added in Task 3)
- Create: `geolocate/consensus.go`
- Test: `geolocate/consensus_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces:
  - `type SourceResult struct { Name string; OK bool; Err string; CountryCode string; Country string; City string; Region string; ASN int; Org string; Hosting bool; Proxy bool; Mobile bool }`
  - `type ConsensusLocation struct { CountryCode string; Country string; CountryConfident bool; City string; Region string; CityConfident bool; ASN int; Org string; Hosting bool; Proxy bool; Mobile bool; Sources []SourceResult; ProbedAt time.Time }`
  - `const MinSources = 2`, `const MaxResponseBytes = 64 * 1024`, `var PerSourceTimeout = 5 * time.Second`
  - `var ErrNoConsensus error`
  - `func consensus(ok []SourceResult) ConsensusLocation` (unexported; consumes only results with `OK == true`)

- [ ] **Step 1: Initialize the module**

Run (in the operator-proxy repo root):
```bash
go mod init github.com/urnetwork/urnetwork-operator-proxy
```
Expected: creates `go.mod`. Then edit `go.mod` so the go directive reads exactly:
```
module github.com/urnetwork/urnetwork-operator-proxy

go 1.26.3
```

- [ ] **Step 2: Write the failing consensus tests**

Create `geolocate/consensus_test.go`:
```go
package geolocate

import "testing"

func TestConsensusCountryMajorityCityDisagree(t *testing.T) {
	ok := []SourceResult{
		{Name: "a", OK: true, CountryCode: "US", Country: "United States", City: "Fairfax", ASN: 401486},
		{Name: "b", OK: true, CountryCode: "US", Country: "United States", City: "Denver", Region: "Colorado", ASN: 401486},
		{Name: "c", OK: true, CountryCode: "US", City: "Atlanta", Region: "Georgia", ASN: 401486},
	}
	loc := consensus(ok)
	if !loc.CountryConfident {
		t.Fatal("expected CountryConfident with 3 agreeing sources")
	}
	if loc.CountryCode != "us" {
		t.Fatalf("CountryCode = %q, want \"us\"", loc.CountryCode)
	}
	if loc.Country != "United States" {
		t.Fatalf("Country = %q, want \"United States\"", loc.Country)
	}
	if loc.CityConfident {
		t.Fatal("cities disagree (Fairfax/Denver/Atlanta); CityConfident must be false")
	}
	if loc.City != "" {
		t.Fatalf("City = %q, want empty on disagreement", loc.City)
	}
	if loc.ASN != 401486 {
		t.Fatalf("ASN = %d, want 401486", loc.ASN)
	}
}

func TestConsensusCityAgreementNormalized(t *testing.T) {
	ok := []SourceResult{
		{Name: "a", OK: true, CountryCode: "US", City: "Denver", Region: "Colorado"},
		{Name: "b", OK: true, CountryCode: "US", City: "denver ", Region: "CO"},
		{Name: "c", OK: true, CountryCode: "US", City: "Atlanta"},
	}
	loc := consensus(ok)
	if !loc.CityConfident {
		t.Fatal("two sources agree on Denver (normalized); CityConfident must be true")
	}
	if got := loc.City; got != "Denver" && got != "denver" {
		t.Fatalf("City = %q, want a Denver display value", got)
	}
	if loc.Region == "" {
		t.Fatal("Region should be carried from an agreeing source")
	}
}

func TestConsensusCountryDisagreementNotConfident(t *testing.T) {
	ok := []SourceResult{
		{Name: "a", OK: true, CountryCode: "US"},
		{Name: "b", OK: true, CountryCode: "CA"},
	}
	loc := consensus(ok)
	if loc.CountryConfident {
		t.Fatal("split country (US vs CA) must not be confident")
	}
	if loc.CountryCode != "" {
		t.Fatalf("CountryCode = %q, want empty on split", loc.CountryCode)
	}
}

func TestConsensusFlagsOr(t *testing.T) {
	ok := []SourceResult{
		{Name: "a", OK: true, CountryCode: "US", Hosting: false, Proxy: false, Mobile: false},
		{Name: "b", OK: true, CountryCode: "US", Hosting: true, Proxy: true, Mobile: false},
	}
	loc := consensus(ok)
	if !loc.Hosting {
		t.Fatal("Hosting must be OR-ed true")
	}
	if !loc.Proxy {
		t.Fatal("Proxy must be OR-ed true")
	}
	if loc.Mobile {
		t.Fatal("Mobile must remain false")
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./geolocate/ -run TestConsensus -v`
Expected: FAIL — compile errors (`SourceResult`, `ConsensusLocation`, `consensus` undefined).

- [ ] **Step 4: Write the types and constants**

Create `geolocate/geolocate.go`:
```go
// Package geolocate resolves a location by cross-checking several free
// geolocation APIs. All network access goes through an injected *http.Client;
// the package never constructs its own client, so in production every request
// egresses through whatever provider tunnel the caller supplies.
package geolocate

import (
	"errors"
	"time"
)

// MinSources is the number of sources that must both respond and agree on the
// country for a confident country verdict, and the quorum below which Locate
// returns ErrNoConsensus.
const MinSources = 2

// MaxResponseBytes caps a single source's response body.
const MaxResponseBytes = 64 * 1024

// PerSourceTimeout bounds each individual source request. It is a var so tests
// can lower it.
var PerSourceTimeout = 5 * time.Second

// ErrNoConsensus is returned by Locate when fewer than MinSources sources
// responded successfully.
var ErrNoConsensus = errors.New("geolocate: fewer than MinSources sources responded")

// SourceResult is one source's normalized observation. It doubles as the
// per-source record attached to ConsensusLocation.Sources for observability.
// On a failed fetch/parse, OK is false and Err is set.
type SourceResult struct {
	Name        string
	OK          bool
	Err         string
	CountryCode string // ISO-3166 alpha-2 as returned by the source (not normalized)
	Country     string // human-readable country name, when the source provides one
	City        string
	Region      string
	ASN         int
	Org         string
	Hosting     bool
	Proxy       bool
	Mobile      bool
}

// ConsensusLocation is the cross-checked result across sources.
type ConsensusLocation struct {
	CountryCode      string // lowercased alpha-2; "" when no country majority
	Country          string
	CountryConfident bool // true iff >= MinSources agreed on CountryCode

	City          string // set only when >= 2 sources agree on the normalized city
	Region        string
	CityConfident bool

	ASN int
	Org string

	Hosting bool
	Proxy   bool
	Mobile  bool

	Sources  []SourceResult // every source's outcome (including failures)
	ProbedAt time.Time
}
```

- [ ] **Step 5: Write the consensus algorithm**

Create `geolocate/consensus.go`:
```go
package geolocate

import "strings"

func normalizeCountry(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

func normalizeCity(city string) string {
	return strings.ToLower(strings.TrimSpace(city))
}

// consensus computes a ConsensusLocation from successful source results.
// Callers pass only results with OK == true. It does not enforce the quorum
// (Locate does that before calling); with a single result it simply yields a
// non-confident country.
func consensus(ok []SourceResult) ConsensusLocation {
	var loc ConsensusLocation

	// country: plurality over non-empty normalized codes; confident only at >= MinSources.
	countryCounts := map[string]int{}
	countryName := map[string]string{}
	for _, r := range ok {
		c := normalizeCountry(r.CountryCode)
		if c == "" {
			continue
		}
		countryCounts[c]++
		if r.Country != "" {
			countryName[c] = r.Country
		}
	}
	bestCountry, bestCountryN := "", 0
	for c, n := range countryCounts {
		if n > bestCountryN || (n == bestCountryN && c < bestCountry) {
			bestCountry, bestCountryN = c, n
		}
	}
	if bestCountryN >= MinSources {
		loc.CountryCode = bestCountry
		loc.Country = countryName[bestCountry]
		loc.CountryConfident = true
	}

	// city: set only if >= 2 sources agree on the normalized city.
	cityCounts := map[string]int{}
	cityDisplay := map[string]string{}
	cityRegion := map[string]string{}
	for _, r := range ok {
		c := normalizeCity(r.City)
		if c == "" {
			continue
		}
		cityCounts[c]++
		if _, seen := cityDisplay[c]; !seen {
			cityDisplay[c] = strings.TrimSpace(r.City)
		}
		if r.Region != "" {
			cityRegion[c] = r.Region
		}
	}
	bestCity, bestCityN := "", 0
	for c, n := range cityCounts {
		if n > bestCityN || (n == bestCityN && c < bestCity) {
			bestCity, bestCityN = c, n
		}
	}
	if bestCityN >= 2 {
		loc.City = cityDisplay[bestCity]
		loc.Region = cityRegion[bestCity]
		loc.CityConfident = true
	}

	// asn: plurality over non-zero ASNs (a single vote is enough; it's a bonus signal).
	asnCounts := map[int]int{}
	asnOrg := map[int]string{}
	for _, r := range ok {
		if r.ASN == 0 {
			continue
		}
		asnCounts[r.ASN]++
		if r.Org != "" {
			asnOrg[r.ASN] = r.Org
		}
	}
	bestASN, bestASNN := 0, 0
	for a, n := range asnCounts {
		if n > bestASNN || (n == bestASNN && a < bestASN) {
			bestASN, bestASNN = a, n
		}
	}
	loc.ASN = bestASN
	loc.Org = asnOrg[bestASN]

	// net_type flags: OR across sources.
	for _, r := range ok {
		loc.Hosting = loc.Hosting || r.Hosting
		loc.Proxy = loc.Proxy || r.Proxy
		loc.Mobile = loc.Mobile || r.Mobile
	}

	return loc
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./geolocate/ -run TestConsensus -v`
Expected: PASS — all four `TestConsensus*` tests.

- [ ] **Step 7: Commit**

```bash
git add go.mod geolocate/geolocate.go geolocate/consensus.go geolocate/consensus_test.go
git commit -m "feat(geolocate): types and consensus algorithm"
```

---

### Task 2: Source definitions and JSON parsers

**Files:**
- Create: `geolocate/sources.go`
- Test: `geolocate/sources_test.go`

**Interfaces:**
- Consumes: `SourceResult` (Task 1).
- Produces:
  - `type source struct { Name string; URL string; Parse func([]byte) (SourceResult, error) }`
  - `var sources []source` — the three production sources (ip.pn, freeipapi, ipinfo) with their real URLs.
  - `func parseIpPn(b []byte) (SourceResult, error)`
  - `func parseFreeIpApi(b []byte) (SourceResult, error)`
  - `func parseIpInfo(b []byte) (SourceResult, error)`
  - `func parseASNOrg(org string) (int, string)`

- [ ] **Step 1: Write the failing parser tests**

Create `geolocate/sources_test.go` (payloads are real responses captured 2026-07-24, trimmed):
```go
package geolocate

import "testing"

func TestParseIpPn(t *testing.T) {
	b := []byte(`{"query":"74.50.11.113","status":"success","country":"United States","countryCode":"US","city":"Fairfax","regionName":"","asn":401486,"mobile":false,"proxy":false,"hosting":false}`)
	r, err := parseIpPn(b)
	if err != nil {
		t.Fatalf("parseIpPn err = %v", err)
	}
	if r.CountryCode != "US" || r.Country != "United States" || r.City != "Fairfax" || r.ASN != 401486 {
		t.Fatalf("parseIpPn = %+v", r)
	}
}

func TestParseIpPnFailStatus(t *testing.T) {
	b := []byte(`{"status":"fail","message":"reserved range"}`)
	if _, err := parseIpPn(b); err == nil {
		t.Fatal("expected error on status != success")
	}
}

func TestParseFreeIpApi(t *testing.T) {
	b := []byte(`{"ipVersion":6,"countryName":"United States","countryCode":"US","cityName":"Denver (North Capitol Hill)","regionName":"Colorado","asn":"401486","isProxy":false}`)
	r, err := parseFreeIpApi(b)
	if err != nil {
		t.Fatalf("parseFreeIpApi err = %v", err)
	}
	if r.CountryCode != "US" || r.Country != "United States" || r.City != "Denver (North Capitol Hill)" || r.Region != "Colorado" || r.ASN != 401486 {
		t.Fatalf("parseFreeIpApi = %+v", r)
	}
}

func TestParseIpInfo(t *testing.T) {
	b := []byte(`{"ip":"74.50.11.113","city":"Atlanta","region":"Georgia","country":"US","org":"AS401486 RAVNIX LLC"}`)
	r, err := parseIpInfo(b)
	if err != nil {
		t.Fatalf("parseIpInfo err = %v", err)
	}
	if r.CountryCode != "US" || r.City != "Atlanta" || r.Region != "Georgia" || r.ASN != 401486 || r.Org != "RAVNIX LLC" {
		t.Fatalf("parseIpInfo = %+v", r)
	}
	if r.Country != "" {
		t.Fatalf("ipinfo provides no country name; Country should be empty, got %q", r.Country)
	}
}

func TestParseASNOrg(t *testing.T) {
	asn, org := parseASNOrg("AS401486 RAVNIX LLC")
	if asn != 401486 || org != "RAVNIX LLC" {
		t.Fatalf("parseASNOrg = (%d, %q)", asn, org)
	}
	if a, o := parseASNOrg(""); a != 0 || o != "" {
		t.Fatalf("empty org = (%d, %q)", a, o)
	}
}

func TestSourcesTable(t *testing.T) {
	if len(sources) != 3 {
		t.Fatalf("len(sources) = %d, want 3", len(sources))
	}
	for _, s := range sources {
		if s.Name == "" || s.URL == "" || s.Parse == nil {
			t.Fatalf("incomplete source %+v", s)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./geolocate/ -run 'TestParse|TestSources' -v`
Expected: FAIL — `parseIpPn`, `parseFreeIpApi`, `parseIpInfo`, `parseASNOrg`, `sources` undefined.

- [ ] **Step 3: Write the source definitions and parsers**

Create `geolocate/sources.go`:
```go
package geolocate

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type source struct {
	Name  string
	URL   string
	Parse func([]byte) (SourceResult, error)
}

// sources are the production geolocation endpoints. Each uses the no-IP
// "my location" form, so a request routed through a provider returns that
// provider's egress location.
var sources = []source{
	{Name: "ip.pn", URL: "https://ip.pn/json", Parse: parseIpPn},
	{Name: "freeipapi", URL: "https://free.freeipapi.com/api/json", Parse: parseFreeIpApi},
	{Name: "ipinfo", URL: "https://ipinfo.io/json", Parse: parseIpInfo},
}

func parseIpPn(b []byte) (SourceResult, error) {
	var v struct {
		Status      string `json:"status"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		City        string `json:"city"`
		RegionName  string `json:"regionName"`
		ASN         int    `json:"asn"`
		Mobile      bool   `json:"mobile"`
		Proxy       bool   `json:"proxy"`
		Hosting     bool   `json:"hosting"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return SourceResult{}, err
	}
	if v.Status != "" && v.Status != "success" {
		return SourceResult{}, fmt.Errorf("ip.pn status %q", v.Status)
	}
	return SourceResult{
		CountryCode: v.CountryCode,
		Country:     v.Country,
		City:        v.City,
		Region:      v.RegionName,
		ASN:         v.ASN,
		Mobile:      v.Mobile,
		Proxy:       v.Proxy,
		Hosting:     v.Hosting,
	}, nil
}

func parseFreeIpApi(b []byte) (SourceResult, error) {
	var v struct {
		CountryName string `json:"countryName"`
		CountryCode string `json:"countryCode"`
		CityName    string `json:"cityName"`
		RegionName  string `json:"regionName"`
		ASN         string `json:"asn"`
		IsProxy     bool   `json:"isProxy"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return SourceResult{}, err
	}
	asn := 0
	if s := strings.TrimPrefix(strings.TrimSpace(v.ASN), "AS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			asn = n
		}
	}
	return SourceResult{
		CountryCode: v.CountryCode,
		Country:     v.CountryName,
		City:        v.CityName,
		Region:      v.RegionName,
		ASN:         asn,
		Proxy:       v.IsProxy,
	}, nil
}

func parseIpInfo(b []byte) (SourceResult, error) {
	var v struct {
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"` // alpha-2, e.g. "US"
		Org     string `json:"org"`     // e.g. "AS401486 RAVNIX LLC"
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return SourceResult{}, err
	}
	asn, org := parseASNOrg(v.Org)
	// ipinfo gives only the alpha-2 country code, no human-readable name.
	return SourceResult{
		CountryCode: v.Country,
		City:        v.City,
		Region:      v.Region,
		ASN:         asn,
		Org:         org,
	}, nil
}

// parseASNOrg splits ipinfo's org field "AS401486 RAVNIX LLC" into
// (401486, "RAVNIX LLC"). On any unexpected shape it returns (0, org).
func parseASNOrg(org string) (int, string) {
	org = strings.TrimSpace(org)
	if org == "" {
		return 0, ""
	}
	fields := strings.SplitN(org, " ", 2)
	name := ""
	if len(fields) == 2 {
		name = fields[1]
	}
	if strings.HasPrefix(fields[0], "AS") {
		if n, err := strconv.Atoi(fields[0][2:]); err == nil {
			return n, name
		}
	}
	return 0, org
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./geolocate/ -run 'TestParse|TestSources' -v`
Expected: PASS — all parser and source-table tests.

- [ ] **Step 5: Commit**

```bash
git add geolocate/sources.go geolocate/sources_test.go
git commit -m "feat(geolocate): source definitions and JSON parsers"
```

---

### Task 3: `Locate` fan-out over the injected client

**Files:**
- Modify: `geolocate/geolocate.go` (add `Locate`, `locate`, `fetchSource`)
- Test: `geolocate/locate_test.go`

**Interfaces:**
- Consumes: `sources` and the parsers (Task 2); `consensus`, `SourceResult`, `ConsensusLocation`, `ErrNoConsensus`, `MinSources`, `MaxResponseBytes`, `PerSourceTimeout` (Task 1).
- Produces:
  - `func Locate(ctx context.Context, client *http.Client) (*ConsensusLocation, error)` — the public entrypoint; fans out to the production `sources` through `client`.
  - `func locate(ctx context.Context, client *http.Client, srcs []source) (*ConsensusLocation, error)` — unexported, injectable-sources variant used by tests.
  - `func fetchSource(ctx context.Context, client *http.Client, s source) SourceResult`

- [ ] **Step 1: Write the failing fan-out tests**

Create `geolocate/locate_test.go`:
```go
package geolocate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func jsonServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestLocateAllAgree(t *testing.T) {
	s1 := jsonServer(`{"status":"success","countryCode":"US","country":"United States","city":"Fairfax","asn":401486}`)
	defer s1.Close()
	s2 := jsonServer(`{"countryCode":"US","countryName":"United States","cityName":"Denver","regionName":"Colorado","asn":"401486","isProxy":true}`)
	defer s2.Close()
	s3 := jsonServer(`{"country":"US","city":"Atlanta","region":"Georgia","org":"AS401486 RAVNIX LLC"}`)
	defer s3.Close()

	srcs := []source{
		{Name: "ip.pn", URL: s1.URL, Parse: parseIpPn},
		{Name: "freeipapi", URL: s2.URL, Parse: parseFreeIpApi},
		{Name: "ipinfo", URL: s3.URL, Parse: parseIpInfo},
	}
	loc, err := locate(context.Background(), &http.Client{}, srcs)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	if !loc.CountryConfident || loc.CountryCode != "us" {
		t.Fatalf("country = %q confident=%v", loc.CountryCode, loc.CountryConfident)
	}
	if loc.CityConfident {
		t.Fatal("cities disagree; CityConfident must be false")
	}
	if loc.ASN != 401486 {
		t.Fatalf("ASN = %d", loc.ASN)
	}
	if !loc.Proxy {
		t.Fatal("Proxy flag from freeipapi must OR true")
	}
	if len(loc.Sources) != 3 {
		t.Fatalf("expected 3 source records, got %d", len(loc.Sources))
	}
}

func TestLocateQuorumFail(t *testing.T) {
	ok := jsonServer(`{"status":"success","countryCode":"US","country":"United States"}`)
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	srcs := []source{
		{Name: "ip.pn", URL: ok.URL, Parse: parseIpPn},
		{Name: "freeipapi", URL: bad.URL, Parse: parseFreeIpApi},
		{Name: "ipinfo", URL: bad.URL, Parse: parseIpInfo},
	}
	if _, err := locate(context.Background(), &http.Client{}, srcs); err != ErrNoConsensus {
		t.Fatalf("err = %v, want ErrNoConsensus", err)
	}
}

func TestLocateTimeoutCountsAsFailure(t *testing.T) {
	old := PerSourceTimeout
	PerSourceTimeout = 100 * time.Millisecond
	defer func() { PerSourceTimeout = old }()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{"country":"US"}`))
	}))
	defer slow.Close()
	good := jsonServer(`{"status":"success","countryCode":"US","country":"United States"}`)
	defer good.Close()

	// two good, one slow -> still quorum; slow one recorded as a failure.
	srcs := []source{
		{Name: "ip.pn", URL: good.URL, Parse: parseIpPn},
		{Name: "freeipapi", URL: good.URL, Parse: parseIpPn},
		{Name: "ipinfo", URL: slow.URL, Parse: parseIpInfo},
	}
	loc, err := locate(context.Background(), &http.Client{}, srcs)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	var slowRec *SourceResult
	for i := range loc.Sources {
		if loc.Sources[i].Name == "ipinfo" {
			slowRec = &loc.Sources[i]
		}
	}
	if slowRec == nil || slowRec.OK {
		t.Fatal("slow source should be recorded as a failure")
	}
	if slowRec.Err == "" {
		t.Fatal("slow source should carry an error string")
	}
}

func TestLocateOversizedResponseRejected(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{" + strings.Repeat(" ", MaxResponseBytes+10) + "}"))
	}))
	defer big.Close()
	good := jsonServer(`{"status":"success","countryCode":"US","country":"United States"}`)
	defer good.Close()

	srcs := []source{
		{Name: "ip.pn", URL: good.URL, Parse: parseIpPn},
		{Name: "freeipapi", URL: good.URL, Parse: parseIpPn},
		{Name: "ipinfo", URL: big.URL, Parse: parseIpInfo},
	}
	loc, err := locate(context.Background(), &http.Client{}, srcs)
	if err != nil {
		t.Fatalf("locate err = %v", err)
	}
	for _, s := range loc.Sources {
		if s.Name == "ipinfo" && (s.OK || !strings.Contains(s.Err, "too large")) {
			t.Fatalf("oversized source not rejected: %+v", s)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./geolocate/ -run TestLocate -v`
Expected: FAIL — `locate` / `fetchSource` undefined.

- [ ] **Step 3: Add `Locate`, `locate`, and `fetchSource`**

Append to `geolocate/geolocate.go` (add the imports `context`, `fmt`, `io`, `net/http`, `sync` to the existing import block):
```go
// Locate queries the production sources through client and returns a
// cross-checked consensus. client, in production, egresses through a specific
// provider, so each source's no-IP endpoint returns that provider's egress
// location. Returns ErrNoConsensus if fewer than MinSources sources responded.
func Locate(ctx context.Context, client *http.Client) (*ConsensusLocation, error) {
	return locate(ctx, client, sources)
}

func locate(ctx context.Context, client *http.Client, srcs []source) (*ConsensusLocation, error) {
	results := make([]SourceResult, len(srcs))
	var wg sync.WaitGroup
	for i := range srcs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = fetchSource(ctx, client, srcs[i])
		}(i)
	}
	wg.Wait()

	ok := make([]SourceResult, 0, len(results))
	for _, r := range results {
		if r.OK {
			ok = append(ok, r)
		}
	}
	if len(ok) < MinSources {
		return nil, ErrNoConsensus
	}
	loc := consensus(ok)
	loc.Sources = results
	loc.ProbedAt = time.Now()
	return &loc, nil
}

func fetchSource(ctx context.Context, client *http.Client, s source) SourceResult {
	r := SourceResult{Name: s.Name}
	ctx, cancel := context.WithTimeout(ctx, PerSourceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	resp, err := client.Do(req)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.Err = fmt.Sprintf("status %d", resp.StatusCode)
		return r
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		r.Err = err.Error()
		return r
	}
	if len(body) > MaxResponseBytes {
		r.Err = "response too large"
		return r
	}
	parsed, err := s.Parse(body)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	parsed.Name = s.Name
	parsed.OK = true
	return parsed
}
```

The updated import block at the top of `geolocate/geolocate.go`:
```go
import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./geolocate/ -run TestLocate -v`
Expected: PASS — all four `TestLocate*` tests.

- [ ] **Step 5: Run the full package suite and vet**

Run: `go test ./geolocate/ -v && go vet ./...`
Expected: all tests PASS; vet clean.

- [ ] **Step 6: Commit**

```bash
git add geolocate/geolocate.go geolocate/locate_test.go
git commit -m "feat(geolocate): Locate fan-out over injected http.Client"
```

---

## Self-Review

**1. Spec coverage (A1 section):**
- Interface `Locate(ctx, client)` → Task 3 ✓
- No-IP "my location" endpoints → `sources` URLs in Task 2 ✓
- 3 sources + per-source parsers → Task 2 ✓
- Country majority ≥2 → `consensus` Task 1 + `TestConsensusCountryMajorityCityDisagree` ✓
- City only on ≥2 agreement → Task 1 + `TestConsensusCityAgreementNormalized` ✓
- ASN plurality → Task 1 ✓
- Flags OR → Task 1 + `TestConsensusFlagsOr` ✓
- Quorum → `ErrNoConsensus` in Task 3 + `TestLocateQuorumFail` ✓
- Per-source timeout, concurrent → Task 3 + `TestLocateTimeoutCountsAsFailure` ✓
- Response cap → Task 3 + `TestLocateOversizedResponseRejected` ✓
- Injected client / never-direct constraint → engine takes `*http.Client`, constructs none ✓
- mmdb fallback lives in the caller (B), not the engine → correctly absent here ✓
- TLS pinning deliberately deferred to A2 → noted in Global Constraints ✓

**2. Placeholder scan:** none — every step has complete, runnable code and exact commands.

**3. Type consistency:** `SourceResult`, `ConsensusLocation`, `source`, `consensus`, `locate`, `fetchSource`, `Locate`, `parseIpPn/parseFreeIpApi/parseIpInfo/parseASNOrg`, `MinSources`, `MaxResponseBytes`, `PerSourceTimeout`, `ErrNoConsensus` are used identically across Tasks 1–3.

## Next plans (not this document)

- **B — server ingest + integration** (server repo): consumes this engine's `ConsensusLocation` contract; can be planned next now that the contract is fixed.
- **A2 — egress prober** (operator-proxy repo): builds the provider-routed, TLS-pinned `*http.Client` (reusing `connect.NewApiMultiClientGenerator` + `NewRemoteUserNatMultiClientWithDefaults` + `ProviderSpec{ClientId}` as in `/proxy socks/main.go`) and passes it to `Locate`. Owns TLS pinning and orchestration/scheduling.
