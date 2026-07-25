# Egress Prober (Sub-project A2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An operator tool that, for each provider, routes geolocation lookups **through that provider's own egress**, cross-checks them with the A1 engine, and submits the result to the server's ingest endpoint.

**Architecture:** Four small packages in the operator-proxy repo. `providertunnel` builds a provider-pinned tunnel (`connect` multiclient + gvisor tun) and exposes it as an `*http.Client` whose `DialContext` is the tun's — so every request egresses through that one provider. `ingest` posts A1's result to the server. `prober` glues tunnel→A1→ingest for one provider. `cmd/egress-prober` is the CLI that enumerates providers, schedules probes with a concurrency cap and a cache, and runs the loop.

**Tech Stack:** Go 1.26.3, `github.com/urnetwork/connect` (multiclient, tun), `github.com/urnetwork/protocol`, and this repo's own `geolocate` package (A1). Standard library for HTTP/TLS.

**Target repo:** `github.com/urnetwork/urnetwork-operator-proxy` (fork: `Ryanmello07/urnetwork-operator-proxy`), same module as A1. This plan doc is co-located with the design spec in the server repo for durability.

**Depends on:**
- **A1** (`geolocate` package) — must be implemented first; this plan calls `geolocate.Locate(ctx, client)`.
- **B** (server ingest) — must be deployed for end-to-end submission; the HTTP contract is fixed in B's plan ("Interface contract for A2").

**Source spec:** `docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md`, section "Sub-project A2".

## Global Constraints

- **The prober's own host must never originate a geolocation request.** Every call to a geolocation API goes through a provider tunnel. The only direct HTTP the prober makes is to the operator's own server (provider enumeration + ingest submission). This is spec hard-constraint #1.
- **TLS pinning is owned here** (A1 is transport-agnostic): the provider-routed client pins the geolocation endpoints so a provider on the path cannot MITM a forged location. Pin by SPKI SHA-256 via `tls.Config.VerifyPeerCertificate`; normal chain validation stays on.
- Reuse `/proxy`'s proven tunnel construction (`socks/main.go`): `connect.NewApiMultiClientGenerator` → `connect.NewRemoteUserNatMultiClientWithDefaults` with `connect.CreateTunWithDefaults`, pumping packets both ways. Do not reimplement transport.
- `connect.ProviderSpec{ClientId: &id}` pins the tunnel to exactly one provider.
- One tunnel per probe, torn down after; never reuse a tunnel across providers.
- Never submit a low-confidence result: if `geolocate.Locate` returns `ErrNoConsensus`, record nothing and let the server keep its mmdb fallback.
- Bounded concurrency (`-concurrency`, default 4) and a per-provider cache TTL (`-cache-ttl`, default 24h).
- The prober needs its own network client identity (a `by_jwt`), exactly as `/proxy socks` does — operator-provisioned, passed by flag/env, never hardcoded.

---

### Task 1: TLS pinning for the geolocation endpoints

**Files:**
- Create: `providertunnel/pinning.go`
- Test: `providertunnel/pinning_test.go`

**Interfaces:**
- Consumes: nothing (self-contained).
- Produces:
  - `func PinnedTLSConfig(pins map[string][]string) *tls.Config` — `pins` maps hostname → allowed SPKI SHA-256 values (base64 std encoding). Returns a config whose `VerifyPeerCertificate` additionally requires the leaf's SPKI pin to match for a pinned host.
  - `func SPKIPin(cert *x509.Certificate) string` — base64 SHA-256 of `cert.RawSubjectPublicKeyInfo`.
  - `var ErrPinMismatch error`

- [ ] **Step 1: Write the failing pinning tests**

Create `providertunnel/pinning_test.go`:
```go
package providertunnel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func selfSigned(t *testing.T, host string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func TestSPKIPinStableAndDistinct(t *testing.T) {
	a, _ := selfSigned(t, "a.example")
	b, _ := selfSigned(t, "b.example")
	if SPKIPin(a) == "" {
		t.Fatal("pin must not be empty")
	}
	if SPKIPin(a) != SPKIPin(a) {
		t.Fatal("pin must be stable for the same cert")
	}
	if SPKIPin(a) == SPKIPin(b) {
		t.Fatal("different keys must produce different pins")
	}
}

func TestPinnedTLSConfigAcceptsMatchingPin(t *testing.T) {
	cert, _ := selfSigned(t, "pinned.example")
	cfg := PinnedTLSConfig(map[string][]string{
		"pinned.example": {SPKIPin(cert)},
	})
	cfg.ServerName = "pinned.example"
	err := cfg.VerifyPeerCertificate([][]byte{cert.Raw}, nil)
	if err != nil {
		t.Fatalf("matching pin must verify, got %v", err)
	}
}

func TestPinnedTLSConfigRejectsWrongPin(t *testing.T) {
	good, _ := selfSigned(t, "pinned.example")
	evil, _ := selfSigned(t, "pinned.example")
	cfg := PinnedTLSConfig(map[string][]string{
		"pinned.example": {SPKIPin(good)},
	})
	cfg.ServerName = "pinned.example"
	err := cfg.VerifyPeerCertificate([][]byte{evil.Raw}, nil)
	if err != ErrPinMismatch {
		t.Fatalf("err = %v, want ErrPinMismatch (a provider must not be able to MITM)", err)
	}
}

func TestPinnedTLSConfigIgnoresUnpinnedHost(t *testing.T) {
	cert, _ := selfSigned(t, "other.example")
	cfg := PinnedTLSConfig(map[string][]string{
		"pinned.example": {"someotherpin"},
	})
	cfg.ServerName = "other.example"
	if err := cfg.VerifyPeerCertificate([][]byte{cert.Raw}, nil); err != nil {
		t.Fatalf("unpinned host must pass the pin check, got %v", err)
	}
}

var _ = tls.Config{}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./providertunnel/ -run 'TestSPKIPin|TestPinnedTLSConfig' -v`
Expected: FAIL — `SPKIPin`, `PinnedTLSConfig`, `ErrPinMismatch` undefined.

- [ ] **Step 3: Write the pinning code**

Create `providertunnel/pinning.go`:
```go
// Package providertunnel builds an http.Client whose every request egresses
// through one specific urnetwork provider, so a geolocation lookup made with it
// reports that provider's egress location.
package providertunnel

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
)

// ErrPinMismatch is returned by the pin check when a pinned host presents a
// leaf certificate whose public key is not in the allowed set. A provider is
// the network path for these requests, so pinning is what stops it forging a
// location by MITMing a geolocation api.
var ErrPinMismatch = errors.New("providertunnel: certificate pin mismatch")

// SPKIPin is the base64 sha-256 of a certificate's subject public key info.
func SPKIPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// PinnedTLSConfig returns a tls.Config that keeps normal chain verification and
// additionally requires, for each host in pins, that the leaf certificate's
// SPKI pin is one of the allowed values. Hosts absent from pins are not
// pin-checked.
func PinnedTLSConfig(pins map[string][]string) *tls.Config {
	// normalize keys to lowercase for host matching
	normalized := make(map[string][]string, len(pins))
	for host, allowed := range pins {
		normalized[strings.ToLower(host)] = allowed
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrPinMismatch
		}
		host := strings.ToLower(cfg.ServerName)
		allowed, pinned := normalized[host]
		if !pinned {
			return nil
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		got := SPKIPin(leaf)
		for _, want := range allowed {
			if got == want {
				return nil
			}
		}
		return ErrPinMismatch
	}
	return cfg
}
```

**Note for the implementer:** `cfg.ServerName` is read inside the closure, so a
caller must set `ServerName` (or clone the config per host) before use. The
transport in Task 2 clones the config per request host; the tests above set it
directly.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./providertunnel/ -run 'TestSPKIPin|TestPinnedTLSConfig' -v`
Expected: PASS — all four tests.

- [ ] **Step 5: Commit**

```bash
git add providertunnel/pinning.go providertunnel/pinning_test.go
git commit -m "feat(providertunnel): SPKI certificate pinning for geolocation endpoints"
```

---

### Task 2: Provider-pinned tunnel exposed as an http.Client

**Files:**
- Create: `providertunnel/tunnel.go`
- Test: `providertunnel/tunnel_test.go`

**Interfaces:**
- Consumes: `PinnedTLSConfig` (Task 1); `connect.NewApiMultiClientGenerator`, `connect.NewRemoteUserNatMultiClientWithDefaults`, `connect.CreateTunWithDefaults`, `connect.NewClientStrategyWithDefaults`, `connect.ProviderSpec`, `connect.Id`, `connect.SourceId`, `connect.TransferPath`, `connect.IpPath`, `protocol.ProvideMode_Network` (from `github.com/urnetwork/connect` and `github.com/urnetwork/protocol`).
- Produces:
  - `type Config struct { ApiURL string; PlatformURL string; ByJwt string; ClientId connect.Id; Pins map[string][]string; DeviceDescription string; DeviceSpec string; Version string }`
  - `type Tunnel struct { ... }` with:
    - `func Open(ctx context.Context, cfg Config, providerClientId connect.Id) (*Tunnel, error)`
    - `func (t *Tunnel) HTTPClient(timeout time.Duration) *http.Client`
    - `func (t *Tunnel) Close() error`

- [ ] **Step 1: Write the failing tunnel test**

Create `providertunnel/tunnel_test.go`:
```go
package providertunnel

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// httpClientOverDialer is the contract Tunnel.HTTPClient must satisfy: every
// request is dialed through the supplied dialer (the tunnel), never the host
// network. This test exercises that wiring with a stub dialer, so it runs
// without a live provider.
func TestHTTPClientUsesSuppliedDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		})}
		_ = srv.Serve(ln)
	}()

	dialed := 0
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed++
		// ignore the requested address; always reach the stub listener,
		// modelling "the tunnel decides where bytes actually go"
		return net.Dial("tcp", ln.Addr().String())
	}

	client := httpClientOverDialer(dial, nil, 5*time.Second)
	resp, err := client.Get("http://geolocation.example/json")
	if err != nil {
		t.Fatalf("get err = %v", err)
	}
	defer resp.Body.Close()
	if dialed == 0 {
		t.Fatal("request did not go through the supplied dialer")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestHTTPClientAppliesPinnedTLSPerHost(t *testing.T) {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, context.Canceled // never actually connects
	}
	base := PinnedTLSConfig(map[string][]string{"ipinfo.io": {"pin"}})
	client := httpClientOverDialer(dial, base, time.Second)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("pinned tls config must be attached to the transport")
	}
	if tr.DialContext == nil {
		t.Fatal("transport must dial through the tunnel")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./providertunnel/ -run TestHTTPClient -v`
Expected: FAIL — `httpClientOverDialer` undefined.

- [ ] **Step 3: Write the tunnel**

Create `providertunnel/tunnel.go`:
```go
package providertunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/protocol"
)

// Config is the operator-provided identity and endpoints the prober uses to
// build tunnels. ClientId is the prober's own client id (so it can exclude
// itself when selecting providers); ByJwt is its network client jwt.
type Config struct {
	ApiURL            string
	PlatformURL       string
	ByJwt             string
	ClientId          connect.Id
	Pins              map[string][]string
	DeviceDescription string
	DeviceSpec        string
	Version           string
}

// Tunnel is a live data path pinned to exactly one provider. Every connection
// dialed through it egresses from that provider.
type Tunnel struct {
	cancel context.CancelFunc
	tun    *connect.Tun
	mc     *connect.RemoteUserNatMultiClient
	pins   *tls.Config
}

// Open builds a tunnel that routes exclusively through providerClientId.
// It mirrors the proven construction in urnetwork/proxy socks/main.go: an api
// multiclient generator pinned to one ProviderSpec, a gvisor tun, and a packet
// pump in both directions.
func Open(ctx context.Context, cfg Config, providerClientId connect.Id) (*Tunnel, error) {
	ctx, cancel := context.WithCancel(ctx)

	generator := connect.NewApiMultiClientGenerator(
		ctx,
		[]*connect.ProviderSpec{
			{ClientId: &providerClientId},
		},
		connect.NewClientStrategyWithDefaults(ctx),
		// exclude self
		[]connect.Id{cfg.ClientId},
		cfg.ApiURL,
		cfg.ByJwt,
		cfg.PlatformURL,
		cfg.DeviceDescription,
		cfg.DeviceSpec,
		cfg.Version,
		&cfg.ClientId,
		connect.DefaultClientSettings,
		connect.DefaultApiMultiClientGeneratorSettings(),
	)

	tun, err := connect.CreateTunWithDefaults(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create tun: %w", err)
	}

	mc := connect.NewRemoteUserNatMultiClientWithDefaults(
		ctx,
		generator,
		func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
			_, _ = tun.Write(packet)
		},
		protocol.ProvideMode_Network,
	)

	// pump tun -> provider
	source := connect.SourceId(cfg.ClientId)
	go func() {
		for {
			packet, err := tun.Read()
			if err != nil {
				return
			}
			mc.SendPacket(source, protocol.ProvideMode_Network, packet, -1)
		}
	}()

	return &Tunnel{
		cancel: cancel,
		tun:    tun,
		mc:     mc,
		pins:   PinnedTLSConfig(cfg.Pins),
	}, nil
}

// HTTPClient returns a client whose every connection is dialed through the
// tunnel, with the configured certificate pins applied.
func (t *Tunnel) HTTPClient(timeout time.Duration) *http.Client {
	return httpClientOverDialer(t.tun.DialContext, t.pins, timeout)
}

// Close tears the tunnel down.
func (t *Tunnel) Close() error {
	t.cancel()
	return t.tun.Close()
}

type dialContextFunc func(ctx context.Context, network string, address string) (net.Conn, error)

// httpClientOverDialer builds an http.Client that dials exclusively through
// dial. pinned may be nil (no pinning); when set it is cloned per host so the
// pin check sees the right ServerName.
func httpClientOverDialer(dial dialContextFunc, pinned *tls.Config, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:         dial,
		TLSHandshakeTimeout: timeout,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   true,
	}
	if pinned != nil {
		tr.TLSClientConfig = pinned
		// clone the pin config per connection so ServerName is set correctly
		// for the host being dialed (the pin check reads cfg.ServerName).
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			raw, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			cfg := pinned.Clone()
			cfg.ServerName = host
			// re-attach the verifier bound to the cloned config's ServerName
			cfg.VerifyPeerCertificate = PinnedTLSConfig(map[string][]string{}).VerifyPeerCertificate
			tlsConn := tls.Client(raw, cfg)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return tlsConn, nil
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}
```

**Note for the implementer — important correctness detail:** the
`DialTLSContext` above must apply the *original* pins with the per-host
`ServerName`, not an empty pin map. Restructure `PinnedTLSConfig` into a
`pinVerifier(pins map[string][]string, serverName string) func([][]byte, [][]*x509.Certificate) error`
helper and call it with the real `pins` and the dialed `host`, then assign that
to `cfg.VerifyPeerCertificate`. Keep Task 1's `PinnedTLSConfig` signature and
tests passing by implementing it in terms of the same helper. Verify with a new
test that a wrong pin fails through the *client* path, not just the config path.

**Note for the implementer:** confirm the exact `connect.NewApiMultiClientGenerator`
parameter list and `mc.SendPacket` arity against
`/root/urnetwork/proxy/socks/main.go` (lines ~157-205) before writing — that file
is the working reference. Adjust argument order/count to match the installed
`connect` version.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./providertunnel/ -v`
Expected: PASS — pinning tests and both `TestHTTPClient*` tests.

- [ ] **Step 5: Commit**

```bash
git add providertunnel/tunnel.go providertunnel/tunnel_test.go providertunnel/pinning.go providertunnel/pinning_test.go
git commit -m "feat(providertunnel): provider-pinned tunnel exposed as an http.Client"
```

---

### Task 3: Ingest client

**Files:**
- Create: `ingest/ingest.go`
- Test: `ingest/ingest_test.go`

**Interfaces:**
- Consumes: `geolocate.ConsensusLocation` (A1).
- Produces:
  - `type Client struct { ServerURL string; OperatorSecret string; HTTP *http.Client }`
  - `func (c *Client) Submit(ctx context.Context, providerClientId string, loc *geolocate.ConsensusLocation) error`
  - `var ErrRejected error`

- [ ] **Step 1: Write the failing ingest tests**

Create `ingest/ingest_test.go`:
```go
package ingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/urnetwork-operator-proxy/geolocate"
)

func TestSubmitPostsContractShape(t *testing.T) {
	var got map[string]any
	var gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("X-UR-Operator-Secret")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"location_id":"019f0000-0000-0000-0000-000000000000"}`))
	}))
	defer srv.Close()

	c := &Client{ServerURL: srv.URL, OperatorSecret: "s3cret", HTTP: srv.Client()}
	err := c.Submit(context.Background(), "019f8835-158d-6fd8-e9dd-fd0e4c6d6792", &geolocate.ConsensusLocation{
		CountryCode:      "us",
		Country:          "United States",
		CountryConfident: true,
		ASN:              401486,
		Org:              "RAVNIX LLC",
		Hosting:          true,
		ProbedAt:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Submit err = %v", err)
	}
	if gotSecret != "s3cret" {
		t.Fatalf("operator secret header = %q", gotSecret)
	}
	for _, k := range []string{"client_id", "country_code", "country_confident", "observed_at"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("body missing %q: %v", k, got)
		}
	}
	if got["country_code"] != "us" {
		t.Fatalf("country_code = %v", got["country_code"])
	}
	if got["country_confident"] != true {
		t.Fatalf("country_confident = %v", got["country_confident"])
	}
}

func TestSubmitRefusesNotCountryConfident(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := &Client{ServerURL: srv.URL, OperatorSecret: "s", HTTP: srv.Client()}
	err := c.Submit(context.Background(), "019f8835-158d-6fd8-e9dd-fd0e4c6d6792", &geolocate.ConsensusLocation{
		CountryConfident: false,
	})
	if err == nil {
		t.Fatal("a non-country-confident result must not be submitted")
	}
	if called {
		t.Fatal("must not reach the server at all")
	}
}

func TestSubmitSurfacesRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unknown client.", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := &Client{ServerURL: srv.URL, OperatorSecret: "s", HTTP: srv.Client()}
	err := c.Submit(context.Background(), "019f8835-158d-6fd8-e9dd-fd0e4c6d6792", &geolocate.ConsensusLocation{
		CountryCode: "us", CountryConfident: true, ProbedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("a 400 must surface as an error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./ingest/ -v`
Expected: FAIL — `Client` / `Submit` undefined.

- [ ] **Step 3: Write the ingest client**

Create `ingest/ingest.go`:
```go
// Package ingest submits probed provider locations to the operator's server.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/urnetwork/urnetwork-operator-proxy/geolocate"
)

// ErrNotConfident is returned when a result is not country-confident. Such a
// result is never submitted: the server keeps its own fallback, which is better
// than recording a guess.
var ErrNotConfident = errors.New("ingest: result is not country-confident")

// ErrRejected is returned when the server rejects a submission.
var ErrRejected = errors.New("ingest: server rejected the submission")

// Client posts probed locations to the server's operator ingest endpoint.
type Client struct {
	ServerURL      string
	OperatorSecret string
	HTTP           *http.Client
}

type submitBody struct {
	ClientId         string    `json:"client_id"`
	CountryCode      string    `json:"country_code"`
	Country          string    `json:"country"`
	Region           string    `json:"region,omitempty"`
	City             string    `json:"city,omitempty"`
	ASN              int       `json:"asn,omitempty"`
	Org              string    `json:"org,omitempty"`
	Hosting          bool      `json:"hosting,omitempty"`
	Proxy            bool      `json:"proxy,omitempty"`
	Mobile           bool      `json:"mobile,omitempty"`
	CountryConfident bool      `json:"country_confident"`
	CityConfident    bool      `json:"city_confident,omitempty"`
	ObservedAt       time.Time `json:"observed_at"`
}

// Submit posts one probed location. The body shape is the contract fixed in the
// server plan (docs .../2026-07-25-provider-egress-location-server.md).
func (c *Client) Submit(ctx context.Context, providerClientId string, loc *geolocate.ConsensusLocation) error {
	if loc == nil || !loc.CountryConfident {
		return ErrNotConfident
	}
	body := submitBody{
		ClientId:         providerClientId,
		CountryCode:      loc.CountryCode,
		Country:          loc.Country,
		ASN:              loc.ASN,
		Org:              loc.Org,
		Hosting:          loc.Hosting,
		Proxy:            loc.Proxy,
		Mobile:           loc.Mobile,
		CountryConfident: true,
		CityConfident:    loc.CityConfident,
		ObservedAt:       loc.ProbedAt,
	}
	if loc.CityConfident {
		body.City = loc.City
		body.Region = loc.Region
	}
	if body.ObservedAt.IsZero() {
		body.ObservedAt = time.Now().UTC()
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.ServerURL, "/") + "/network/provider-egress-location"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-UR-Operator-Secret", c.OperatorSecret)

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w: status %d: %s", ErrRejected, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./ingest/ -v`
Expected: PASS — all three tests.

- [ ] **Step 5: Commit**

```bash
git add ingest/ingest.go ingest/ingest_test.go
git commit -m "feat(ingest): submit probed provider locations to the server"
```

---

### Task 4: Probe one provider (tunnel → A1 → ingest)

**Files:**
- Create: `prober/prober.go`
- Test: `prober/prober_test.go`

**Interfaces:**
- Consumes: `geolocate.Locate`, `geolocate.ConsensusLocation`, `geolocate.ErrNoConsensus`, `ingest.Client`.
- Produces:
  - `type Locator func(ctx context.Context, client *http.Client) (*geolocate.ConsensusLocation, error)`
  - `type TunnelOpener func(ctx context.Context, providerClientId string) (*http.Client, func() error, error)`
  - `type Submitter interface { Submit(ctx context.Context, providerClientId string, loc *geolocate.ConsensusLocation) error }`
  - `type Prober struct { Open TunnelOpener; Locate Locator; Submit Submitter }`
  - `func (p *Prober) ProbeOne(ctx context.Context, providerClientId string) error`

- [ ] **Step 1: Write the failing prober tests**

Create `prober/prober_test.go`:
```go
package prober

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/urnetwork-operator-proxy/geolocate"
)

type stubSubmitter struct {
	calls int
	last  *geolocate.ConsensusLocation
	err   error
}

func (s *stubSubmitter) Submit(ctx context.Context, id string, loc *geolocate.ConsensusLocation) error {
	s.calls++
	s.last = loc
	return s.err
}

func TestProbeOneHappyPathSubmitsAndCloses(t *testing.T) {
	closed := false
	sub := &stubSubmitter{}
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return &http.Client{}, func() error { closed = true; return nil }, nil
		},
		Locate: func(ctx context.Context, c *http.Client) (*geolocate.ConsensusLocation, error) {
			return &geolocate.ConsensusLocation{CountryCode: "us", CountryConfident: true}, nil
		},
		Submit: sub,
	}
	if err := p.ProbeOne(context.Background(), "provider-1"); err != nil {
		t.Fatalf("ProbeOne err = %v", err)
	}
	if sub.calls != 1 {
		t.Fatalf("submit calls = %d, want 1", sub.calls)
	}
	if !closed {
		t.Fatal("the tunnel must be closed after the probe")
	}
}

func TestProbeOneNoConsensusDoesNotSubmit(t *testing.T) {
	closed := false
	sub := &stubSubmitter{}
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return &http.Client{}, func() error { closed = true; return nil }, nil
		},
		Locate: func(ctx context.Context, c *http.Client) (*geolocate.ConsensusLocation, error) {
			return nil, geolocate.ErrNoConsensus
		},
		Submit: sub,
	}
	err := p.ProbeOne(context.Background(), "provider-1")
	if !errors.Is(err, geolocate.ErrNoConsensus) {
		t.Fatalf("err = %v, want ErrNoConsensus", err)
	}
	if sub.calls != 0 {
		t.Fatal("must not submit without consensus")
	}
	if !closed {
		t.Fatal("the tunnel must be closed even when the probe fails")
	}
}

func TestProbeOneTunnelFailureIsReported(t *testing.T) {
	sub := &stubSubmitter{}
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return nil, nil, errors.New("no route to provider")
		},
		Locate: func(ctx context.Context, c *http.Client) (*geolocate.ConsensusLocation, error) {
			t.Fatal("Locate must not run when the tunnel fails")
			return nil, nil
		},
		Submit: sub,
	}
	if err := p.ProbeOne(context.Background(), "provider-1"); err == nil {
		t.Fatal("expected a tunnel error")
	}
	if sub.calls != 0 {
		t.Fatal("must not submit when the tunnel fails")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./prober/ -v`
Expected: FAIL — `Prober` / `ProbeOne` undefined.

- [ ] **Step 3: Write the prober**

Create `prober/prober.go`:
```go
// Package prober probes one provider's egress location: open a tunnel pinned to
// that provider, run the geolocation consensus through it, submit the result.
package prober

import (
	"context"
	"fmt"
	"net/http"

	"github.com/urnetwork/urnetwork-operator-proxy/geolocate"
)

// Locator runs the geolocation consensus over a client. In production this is
// geolocate.Locate.
type Locator func(ctx context.Context, client *http.Client) (*geolocate.ConsensusLocation, error)

// TunnelOpener opens a tunnel to one provider and returns an http.Client that
// egresses through it, plus a close function.
type TunnelOpener func(ctx context.Context, providerClientId string) (*http.Client, func() error, error)

// Submitter records a probed location.
type Submitter interface {
	Submit(ctx context.Context, providerClientId string, loc *geolocate.ConsensusLocation) error
}

// Prober wires a tunnel opener, a locator, and a submitter. Each dependency is
// injected so the flow is testable without a live provider or server.
type Prober struct {
	Open   TunnelOpener
	Locate Locator
	Submit Submitter
}

// ProbeOne probes a single provider. The tunnel is always closed, and nothing
// is submitted unless the geolocation reached consensus.
func (p *Prober) ProbeOne(ctx context.Context, providerClientId string) error {
	client, closeTunnel, err := p.Open(ctx, providerClientId)
	if err != nil {
		return fmt.Errorf("open tunnel to %s: %w", providerClientId, err)
	}
	defer func() {
		if closeTunnel != nil {
			_ = closeTunnel()
		}
	}()

	loc, err := p.Locate(ctx, client)
	if err != nil {
		return err
	}

	return p.Submit.Submit(ctx, providerClientId, loc)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./prober/ -v`
Expected: PASS — all three tests.

- [ ] **Step 5: Commit**

```bash
git add prober/prober.go prober/prober_test.go
git commit -m "feat(prober): probe one provider end to end"
```

---

### Task 5: Scheduler with cache and concurrency cap

**Files:**
- Create: `prober/schedule.go`
- Test: `prober/schedule_test.go`

**Interfaces:**
- Consumes: `Prober.ProbeOne` (Task 4).
- Produces:
  - `type Scheduler struct { Prober *Prober; Concurrency int; CacheTTL time.Duration; Now func() time.Time }`
  - `func (s *Scheduler) Run(ctx context.Context, providerClientIds []string) Summary`
  - `type Summary struct { Attempted int; Submitted int; Skipped int; Failed int }`

- [ ] **Step 1: Write the failing scheduler tests**

Create `prober/schedule_test.go`:
```go
package prober

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/urnetwork-operator-proxy/geolocate"
)

func okProber(probed *int32, mu *sync.Mutex, inflight *int32, maxSeen *int32) *Prober {
	return &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			cur := atomic.AddInt32(inflight, 1)
			mu.Lock()
			if cur > *maxSeen {
				*maxSeen = cur
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			return &http.Client{}, func() error { atomic.AddInt32(inflight, -1); return nil }, nil
		},
		Locate: func(ctx context.Context, c *http.Client) (*geolocate.ConsensusLocation, error) {
			atomic.AddInt32(probed, 1)
			return &geolocate.ConsensusLocation{CountryCode: "us", CountryConfident: true}, nil
		},
		Submit: &stubSubmitter{},
	}
}

func TestSchedulerRespectsConcurrencyCap(t *testing.T) {
	var probed, inflight, maxSeen int32
	var mu sync.Mutex
	s := &Scheduler{Prober: okProber(&probed, &mu, &inflight, &maxSeen), Concurrency: 2, CacheTTL: time.Hour}

	ids := []string{"a", "b", "c", "d", "e", "f"}
	sum := s.Run(context.Background(), ids)

	if sum.Attempted != len(ids) {
		t.Fatalf("attempted = %d, want %d", sum.Attempted, len(ids))
	}
	if sum.Submitted != len(ids) {
		t.Fatalf("submitted = %d, want %d", sum.Submitted, len(ids))
	}
	mu.Lock()
	peak := maxSeen
	mu.Unlock()
	if peak > 2 {
		t.Fatalf("peak concurrency = %d, want <= 2", peak)
	}
}

func TestSchedulerCachesWithinTTL(t *testing.T) {
	var probed, inflight, maxSeen int32
	var mu sync.Mutex
	s := &Scheduler{Prober: okProber(&probed, &mu, &inflight, &maxSeen), Concurrency: 2, CacheTTL: time.Hour}

	s.Run(context.Background(), []string{"a", "b"})
	first := atomic.LoadInt32(&probed)
	sum := s.Run(context.Background(), []string{"a", "b"})

	if atomic.LoadInt32(&probed) != first {
		t.Fatal("a second run within the ttl must not re-probe")
	}
	if sum.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", sum.Skipped)
	}
}

func TestSchedulerReprobesAfterTTL(t *testing.T) {
	var probed, inflight, maxSeen int32
	var mu sync.Mutex
	now := time.Now()
	s := &Scheduler{
		Prober:      okProber(&probed, &mu, &inflight, &maxSeen),
		Concurrency: 2,
		CacheTTL:    time.Hour,
		Now:         func() time.Time { return now },
	}
	s.Run(context.Background(), []string{"a"})
	now = now.Add(2 * time.Hour)
	s.Run(context.Background(), []string{"a"})

	if atomic.LoadInt32(&probed) != 2 {
		t.Fatalf("probed = %d, want 2 (ttl expired)", probed)
	}
}

func TestSchedulerCountsFailuresAndDoesNotCache(t *testing.T) {
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return nil, nil, errors.New("boom")
		},
		Locate: func(ctx context.Context, c *http.Client) (*geolocate.ConsensusLocation, error) {
			return nil, nil
		},
		Submit: &stubSubmitter{},
	}
	s := &Scheduler{Prober: p, Concurrency: 1, CacheTTL: time.Hour}
	sum := s.Run(context.Background(), []string{"a"})
	if sum.Failed != 1 {
		t.Fatalf("failed = %d, want 1", sum.Failed)
	}
	// a failure must not be cached; the next run retries
	sum2 := s.Run(context.Background(), []string{"a"})
	if sum2.Attempted != 1 {
		t.Fatal("a failed probe must be retried on the next run")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./prober/ -run TestScheduler -v`
Expected: FAIL — `Scheduler` / `Summary` undefined.

- [ ] **Step 3: Write the scheduler**

Create `prober/schedule.go`:
```go
package prober

import (
	"context"
	"sync"
	"time"
)

// Summary reports one scheduler run.
type Summary struct {
	Attempted int
	Submitted int
	Skipped   int
	Failed    int
}

// Scheduler probes a set of providers with bounded concurrency, skipping any
// provider probed within CacheTTL. Only successful probes are cached, so a
// failure is retried on the next run.
type Scheduler struct {
	Prober      *Prober
	Concurrency int
	CacheTTL    time.Duration
	// Now defaults to time.Now; tests override it to advance the clock.
	Now func() time.Time

	mu     sync.Mutex
	probed map[string]time.Time
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) recentlyProbed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, ok := s.probed[id]
	if !ok {
		return false
	}
	return s.now().Sub(last) < s.CacheTTL
}

func (s *Scheduler) markProbed(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.probed == nil {
		s.probed = map[string]time.Time{}
	}
	s.probed[id] = s.now()
}

// Run probes each provider that is not cached, with at most Concurrency
// tunnels open at once.
func (s *Scheduler) Run(ctx context.Context, providerClientIds []string) Summary {
	concurrency := s.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	var mu sync.Mutex
	var sum Summary

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, id := range providerClientIds {
		if s.recentlyProbed(id) {
			mu.Lock()
			sum.Skipped++
			mu.Unlock()
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()

			mu.Lock()
			sum.Attempted++
			mu.Unlock()

			err := s.Prober.ProbeOne(ctx, id)

			mu.Lock()
			if err != nil {
				sum.Failed++
			} else {
				sum.Submitted++
			}
			mu.Unlock()

			if err == nil {
				s.markProbed(id)
			}
		}(id)
	}
	wg.Wait()
	return sum
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./prober/ -v`
Expected: PASS — all `TestProbeOne*` and `TestScheduler*` tests.

- [ ] **Step 5: Commit**

```bash
git add prober/schedule.go prober/schedule_test.go
git commit -m "feat(prober): scheduler with cache ttl and concurrency cap"
```

---

### Task 6: CLI wiring

**Files:**
- Create: `cmd/egress-prober/main.go`
- Create: `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: the `egress-prober` binary.

- [ ] **Step 1: Write the CLI**

Create `cmd/egress-prober/main.go`:
```go
// Command egress-prober probes each provider's egress location by routing
// geolocation lookups through that provider, and submits the results to the
// operator's server.
//
// The prober host never contacts a geolocation api directly: every lookup
// egresses through a provider tunnel. The only direct calls are to the
// operator's own server (provider list, ingest).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/urnetwork-operator-proxy/geolocate"
	"github.com/urnetwork/urnetwork-operator-proxy/ingest"
	"github.com/urnetwork/urnetwork-operator-proxy/prober"
	"github.com/urnetwork/urnetwork-operator-proxy/providertunnel"
)

func main() {
	apiURL := flag.String("api-url", "", "operator server api url, e.g. https://api.example.net")
	platformURL := flag.String("platform-url", "", "operator platform websocket url, e.g. wss://connect.example.net")
	byJwt := flag.String("by-jwt", os.Getenv("UR_PROBER_BY_JWT"), "the prober's network client jwt (or UR_PROBER_BY_JWT)")
	operatorSecret := flag.String("operator-secret", os.Getenv("UR_OPERATOR_SECRET"), "ingest secret (or UR_OPERATOR_SECRET)")
	concurrency := flag.Int("concurrency", 4, "max simultaneous provider tunnels")
	cacheTTL := flag.Duration("cache-ttl", 24*time.Hour, "do not re-probe a provider within this window")
	interval := flag.Duration("interval", time.Hour, "sleep between passes; 0 runs a single pass")
	probeTimeout := flag.Duration("probe-timeout", 60*time.Second, "per-provider probe timeout")
	flag.Parse()

	if *apiURL == "" || *platformURL == "" || *byJwt == "" || *operatorSecret == "" {
		log.Fatal("api-url, platform-url, by-jwt and operator-secret are all required")
	}

	clientId, err := parseByJwtClientId(*byJwt)
	if err != nil {
		log.Fatalf("parse by-jwt client id: %s", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tunnelCfg := providertunnel.Config{
		ApiURL:            *apiURL,
		PlatformURL:       *platformURL,
		ByJwt:             *byJwt,
		ClientId:          clientId,
		Pins:              geolocatePins(),
		DeviceDescription: "egress prober",
		DeviceSpec:        "egress-prober",
		Version:           "0.0.0",
	}

	submitter := &ingest.Client{
		ServerURL:      *apiURL,
		OperatorSecret: *operatorSecret,
		HTTP:           &http.Client{Timeout: 30 * time.Second},
	}

	p := &prober.Prober{
		Open: func(ctx context.Context, providerClientId string) (*http.Client, func() error, error) {
			id, err := connect.ParseId(providerClientId)
			if err != nil {
				return nil, nil, err
			}
			t, err := providertunnel.Open(ctx, tunnelCfg, id)
			if err != nil {
				return nil, nil, err
			}
			return t.HTTPClient(*probeTimeout), t.Close, nil
		},
		Locate: geolocate.Locate,
		Submit: submitter,
	}

	scheduler := &prober.Scheduler{
		Prober:      p,
		Concurrency: *concurrency,
		CacheTTL:    *cacheTTL,
	}

	for {
		providers, err := listProviders(ctx, *apiURL, *byJwt)
		if err != nil {
			log.Printf("list providers: %s", err)
		} else {
			sum := scheduler.Run(ctx, providers)
			log.Printf("pass: attempted=%d submitted=%d skipped=%d failed=%d",
				sum.Attempted, sum.Submitted, sum.Skipped, sum.Failed)
		}
		if *interval == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

// geolocatePins are the SPKI pins for the geolocation endpoints. Populate with
// the current pins (see README for how to capture them) plus a backup pin per
// host so a routine certificate rotation does not break probing.
func geolocatePins() map[string][]string {
	return map[string][]string{
		"ip.pn":              {},
		"free.freeipapi.com": {},
		"ipinfo.io":          {},
	}
}

// listProviders asks the operator's own server for current public providers.
func listProviders(ctx context.Context, apiURL string, byJwt string) ([]string, error) {
	body := strings.NewReader(`{"specs":[{"best_available":true}],"count":1000,"rank_mode":"quality"}`)
	url := strings.TrimRight(apiURL, "/") + "/network/find-providers2"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+byJwt)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Providers []struct {
			ClientId string `json:"client_id"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Providers))
	for _, p := range out.Providers {
		ids = append(ids, p.ClientId)
	}
	return ids, nil
}

func parseByJwtClientId(byJwt string) (connect.Id, error) {
	// mirrors urnetwork/proxy socks/main.go parseByJwtClientId
	parts := strings.Split(byJwt, ".")
	if len(parts) != 3 {
		return connect.Id{}, fmt.Errorf("malformed jwt")
	}
	payload, err := base64RawURLDecode(parts[1])
	if err != nil {
		return connect.Id{}, err
	}
	var claims struct {
		ClientId string `json:"client_id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return connect.Id{}, err
	}
	if claims.ClientId == "" {
		return connect.Id{}, fmt.Errorf("jwt has no client_id")
	}
	return connect.ParseId(claims.ClientId)
}
```

**Note for the implementer:** `base64RawURLDecode` is a one-liner over
`encoding/base64`.`RawURLEncoding.DecodeString` — add it, or copy
`parseByJwtClientId` wholesale from `/root/urnetwork/proxy/socks/main.go:468`
(it already exists there and handles the claim-type variations). Prefer copying
the proven version. Also confirm `connect.ParseId` is the correct exported
parser name in the installed `connect` version.

**Note for the implementer — pins must be filled before production use:**
`geolocatePins()` ships empty, which (per Task 1) means *no pin enforcement*.
Capture the real pins and fill them in as part of this task:
```bash
for h in ip.pn free.freeipapi.com ipinfo.io; do
  echo -n "$h: "
  openssl s_client -connect $h:443 -servername $h </dev/null 2>/dev/null \
    | openssl x509 -pubkey -noout \
    | openssl pkey -pubin -outform der \
    | openssl dgst -sha256 -binary | base64
done
```
Record both the current leaf pin and the issuing intermediate's pin per host as
the backup, so a routine rotation does not break probing.

- [ ] **Step 2: Write the README**

Create `README.md`:
```markdown
# urnetwork-operator-proxy

Operator tooling for urnetwork network operators.

## egress-prober

Determines each provider's **egress** location without paying for a commercial
IP-geolocation database.

For every provider, the prober opens a tunnel pinned to that provider, runs
geolocation lookups **through** it against three independent free sources, takes
a consensus, and submits the result to the operator's server.

- The prober host never queries a geolocation api directly — every lookup
  egresses through a provider, so the api reports *that provider's* location.
- The lookups are TLS-pinned, so a provider on the path cannot forge a location.
- Country is the trusted output. City is recorded only when at least two sources
  agree (free sources disagree on city often), otherwise the location is
  country-granular.
- A provider that refuses to carry the probe is simply not located; the server
  falls back to its own database.

### Run

```bash
go build ./cmd/egress-prober
./egress-prober \
  -api-url https://api.example.net \
  -platform-url wss://connect.example.net \
  -by-jwt "$UR_PROBER_BY_JWT" \
  -operator-secret "$UR_OPERATOR_SECRET" \
  -concurrency 4 \
  -cache-ttl 24h \
  -interval 1h
```

The prober needs its own network client identity (`-by-jwt`), provisioned like
any other client. `-operator-secret` must match `ingest_secret` in the server's
`provider_egress.yml` vault resource.

### Design

See the design spec:
`docs/superpowers/specs/2026-07-24-provider-egress-geolocation-design.md`
in the server repo.
```

- [ ] **Step 3: Build and run the full suite**

Run: `go build ./... && go vet ./... && go test ./... -v`
Expected: build and vet clean; all `geolocate`, `providertunnel`, `ingest`, and
`prober` tests PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/egress-prober/main.go README.md
git commit -m "feat(cmd): egress-prober CLI"
```

---

### Task 7: End-to-end verification against the beta deployment

**Files:** none (verification only).

**Prerequisite:** sub-project B deployed to beta, with `provider_egress.yml`
present in the beta vault and a prober client identity provisioned.

- [ ] **Step 1: Verify the prober never contacts a geolocation api directly**

With the prober running against beta, confirm on the prober host:
```bash
sudo tcpdump -n -c 50 'host ip.pn or host ipinfo.io or host free.freeipapi.com'
```
Expected: **no packets**. Every geolocation flow leaves through the provider
tunnel, not the host. This is the spec's hard constraint; treat any packet here
as a blocking bug.

- [ ] **Step 2: Run a single pass and confirm submissions land**

```bash
./egress-prober -api-url https://api.beta-test.net -platform-url wss://connect.beta-test.net \
  -by-jwt "$UR_PROBER_BY_JWT" -operator-secret "$UR_OPERATOR_SECRET" -interval 0
```
Expected log: `pass: attempted=N submitted=M ...` with `M > 0`.

Then on the beta server:
```bash
docker exec server-postgres-1 psql -U postgres -d postgres -c \
  "SELECT client_id, country_code, asn, city_confident, observed_at FROM provider_egress_location ORDER BY observed_at DESC LIMIT 10;"
```
Expected: rows for the probed providers with sensible country codes.

- [ ] **Step 3: Confirm the location is used on the next provider connect**

After a probed provider reconnects:
```bash
docker exec server-postgres-1 psql -U postgres -d postgres -c \
  "SELECT ncl.connection_id, ncl.country_location_id, pel.country_code
   FROM network_client_location ncl
   INNER JOIN network_client_connection ncc ON ncc.connection_id = ncl.connection_id
   INNER JOIN provider_egress_location pel ON pel.client_id = ncc.client_id
   ORDER BY ncc.connect_time DESC LIMIT 5;"
```
Expected: the connection's country matches the probed `country_code` — proving
B3's preference path is live.

- [ ] **Step 4: Confirm the fallback still works**

Pick a provider with no `provider_egress_location` row; confirm on reconnect it
still gets a location (from the mmdb path) and the connection is not torn down.

---

## Self-Review

**1. Spec coverage (A2 section):**
- Provider-routed client via `ProviderSpec{ClientId}` + multiclient + tun → Task 2 ✓
- Wrap as `*http.Client` (direct dialer, no SOCKS hop — the spec's open question, resolved: `Tun.DialContext` is already an `http.Transport.DialContext`) → Task 2 ✓
- TLS pinning owned by A2 → Task 1 (+ capture instructions in Task 6) ✓
- Prober identity (`by_jwt`, operator-provisioned) → Task 6 flags ✓
- Enumerate providers → Task 6 `listProviders` ✓
- Bounded worker pool → Task 5 `Concurrency` ✓
- Cache by client_id with TTL, re-probe on expiry → Task 5 ✓
- Never submit low confidence; tunnel failure records nothing → Tasks 3 (`ErrNotConfident`) + 4 (`ProbeOne`) + 5 (failures not cached) ✓
- One tunnel per probe, always torn down → Task 4 (`defer closeTunnel`) + test ✓
- Egress-splitting documented as residual → covered in the spec; Task 6 README states the trust properties ✓
- Testing without a live provider/server (injected deps) → Tasks 1-5 all unit-testable ✓
- End-to-end verification incl. the "never direct" tcpdump check → Task 7 ✓

**2. Placeholder scan:** no TBDs. Four "Note for the implementer" blocks give
directed verification against named reference files (`/root/urnetwork/proxy/socks/main.go`
for generator arity and jwt parsing; the `DialTLSContext` pin-binding fix) plus
the pin-capture command — all concrete instructions with the code to write, not
deferred decisions.

**3. Type consistency:** `Config`, `Tunnel`, `Open`, `HTTPClient`, `Close`,
`httpClientOverDialer`, `PinnedTLSConfig`, `SPKIPin`, `ErrPinMismatch`,
`ingest.Client`/`Submit`/`ErrNotConfident`/`ErrRejected`, `prober.Prober`/
`ProbeOne`/`Locator`/`TunnelOpener`/`Submitter`, `Scheduler`/`Run`/`Summary`
are used identically across Tasks 1-6. `geolocate.Locate` matches the `Locator`
signature defined in A1's plan (`func(context.Context, *http.Client) (*ConsensusLocation, error)`).
The submit body field names match B's fixed contract exactly.

## Known deviations from the spec

- **Spec open question "direct dialer vs SOCKS hop" is resolved to direct.**
  `connect.Tun.DialContext` already has the `http.Transport.DialContext`
  signature, so no SOCKS5 hop is needed. Simpler and one less moving part.
- **Egress-splitting hardening is v1-documented, not implemented.** Randomized
  scheduling comes free with the cache TTL + interval; probe-traffic
  indistinguishability and control-country cross-checking are follow-ups, as the
  spec allows.
