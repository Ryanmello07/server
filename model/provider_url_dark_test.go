package model

import (
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// A synthetic no-response turn: the exit never answered DNS inside the tunnel.
func urlDarkTestNoResponseEvidence(at time.Time) *egresshealth.UrlProbeEvidence {
	evidence := fp2TestUrlProbeEvidence(at, false)
	evidence.Security = nil
	evidence.StatusCode = 0
	evidence.FirstByteReceived = false
	evidence.RequestWritten = false
	evidence.ByteCount, evidence.WireByteCount, evidence.WireSampleByteCount = 0, 0, 0
	evidence.BodyComplete = false
	evidence.TtfbMillis, evidence.BodyMillis, evidence.BodyBitsPerSecond = 0, 0, 0
	evidence.ContentClassification = ""
	evidence.FailureStage = "request_dns_timeout"
	return evidence
}

// Success needs ok; any byte, status or TLS event back is forwarded, and so is
// missing evidence; only provably nothing is no-response.
func TestProviderUrlDarkClassify(t *testing.T) {
	now := server.NowUtc()
	noResponse := urlDarkTestNoResponseEvidence(now)
	status := urlDarkTestNoResponseEvidence(now)
	status.StatusCode = 502
	firstByte := urlDarkTestNoResponseEvidence(now)
	firstByte.FirstByteReceived = true
	tlsEvent := urlDarkTestNoResponseEvidence(now)
	tlsEvent.Security = []egresshealth.UrlProbeSecurityEvent{{MeasuredAt: now, TlsFailure: true}}
	emptySecurity := urlDarkTestNoResponseEvidence(now)
	emptySecurity.Security = []egresshealth.UrlProbeSecurityEvent{}
	runEnded := urlDarkTestNoResponseEvidence(now)
	runEnded.FailureStage = "run_ended"

	for _, c := range []struct {
		name     string
		ok       int
		evidence *egresshealth.UrlProbeEvidence
		want     providerUrlDarkTurn
	}{
		{"success", 1, fp2TestUrlProbeEvidence(now, true), providerUrlDarkSuccess},
		{"success without evidence", 1, nil, providerUrlDarkSuccess},
		{"captcha page", 0, fp2TestUrlProbeEvidence(now, false), providerUrlDarkForwarded},
		{"null evidence", 0, nil, providerUrlDarkForwarded},
		{"status code", 0, status, providerUrlDarkForwarded},
		{"first byte", 0, firstByte, providerUrlDarkForwarded},
		{"tls event", 0, tlsEvent, providerUrlDarkForwarded},
		// the prober ran out of budget before the first attempt
		{"run ended", 0, runEnded, providerUrlDarkForwarded},
		{"no response", 0, noResponse, providerUrlDarkNoResponse},
		{"no response, empty security", 0, emptySecurity, providerUrlDarkNoResponse},
	} {
		if got := classifyProviderUrlDarkTurn(c.ok, c.evidence); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// URL-dark is a hard exclusion right after TLS: it beats every ratio verdict,
// and the earlier common gates keep their reasons.
func TestProviderUrlDarkDecision(t *testing.T) {
	quality := true
	for _, flag := range []bool{false, true} {
		decision := decideProviderEgress(&providerEgressFacts{urlDark: true, egressQuality: &quality}, flag)
		connect.AssertEqual(t, decision, providerEgressDecision{reason: ProviderExcludedUrlDark, hardExcluded: true})
		decision = decideProviderEgress(&providerEgressFacts{urlDark: true}, flag)
		connect.AssertEqual(t, decision, providerEgressDecision{reason: ProviderExcludedUrlDark, hardExcluded: true})
	}
	connect.AssertEqual(t, decideProviderEgress(&providerEgressFacts{urlDark: true, tlsAuthenticationFailed: true}, true).reason, ProviderExcludedTls)
	connect.AssertEqual(t, decideProviderEgress(&providerEgressFacts{urlDark: true, arinRisk: true}, true).reason, ProviderExcludedArinRisk)
	connect.AssertEqual(t, decideProviderEgress(&providerEgressFacts{urlDark: true, reliabilityFailed: true}, true).reason, ProviderExcludedReliability)
	connect.AssertEqual(t, slices.Index(ProviderExcludedReasons, ProviderExcludedUrlDark), slices.Index(ProviderExcludedReasons, ProviderExcludedTls)+1)

	clientId := server.NewId()
	filter := providerCountFilter{urlDark: map[server.Id]bool{clientId: true}}
	connect.AssertEqual(t, filter.hasHardEgressFailure(clientId), true)
	connect.AssertEqual(t, filter.hasHardEgressFailure(server.NewId()), false)
	connect.AssertEqual(t, filter.egressFacts(clientId, nil, nil, nil, DefaultEgressIndexSettings()).urlDark, true)
}

// The floor is a no-op while plenty remain, releases the verdicts whose last
// answer is newest first when it binds, and never leaves fewer than
// max(10, ceil(10%)).
func TestProviderUrlDarkServingFloor(t *testing.T) {
	connect.AssertEqual(t, providerUrlDarkServingFloor(0), 10)
	connect.AssertEqual(t, providerUrlDarkServingFloor(100), 10)
	connect.AssertEqual(t, providerUrlDarkServingFloor(101), 11)
	connect.AssertEqual(t, providerUrlDarkServingFloor(1000), 100)

	now := server.NowUtc()
	verdicts := func(n int) []providerUrlDarkVerdict {
		out := []providerUrlDarkVerdict{}
		for i := range n {
			// listed newest answer first, so the floor must sort
			answeredAt := now.Add(-time.Duration(i) * time.Minute)
			out = append(out, providerUrlDarkVerdict{clientId: server.NewId(), answeredAt: &answeredAt})
		}
		return out
	}

	plenty := verdicts(5)
	connect.AssertEqual(t, len(honorProviderUrlDarkVerdicts(100, plenty)), 5)

	for _, c := range []struct{ population, dark int }{{15, 12}, {12, 12}, {10, 3}, {5, 5}, {200, 199}, {1000, 950}} {
		dark := verdicts(c.dark)
		honored := honorProviderUrlDarkVerdicts(c.population, dark)
		floor := providerUrlDarkServingFloor(c.population)
		if c.population-len(honored) < min(c.population, floor) {
			t.Errorf("population %d dark %d: honored %d leaves fewer than the floor %d", c.population, c.dark, len(honored), floor)
		}
		connect.AssertEqual(t, len(honored), min(c.dark, max(0, c.population-floor)))
		// exactly the oldest: dark is newest first
		for i, verdict := range dark {
			connect.AssertEqual(t, honored[verdict.clientId], i >= len(dark)-len(honored))
		}
	}

	// equal answers order by client id
	a := providerUrlDarkVerdict{clientId: server.Id{1}, answeredAt: &now}
	b := providerUrlDarkVerdict{clientId: server.Id{2}, answeredAt: &now}
	connect.AssertEqual(t, honorProviderUrlDarkVerdicts(11, []providerUrlDarkVerdict{b, a}), map[server.Id]bool{a.clientId: true})
	// no answer within the horizon stands longest, whatever its id
	never := providerUrlDarkVerdict{clientId: server.Id{9}}
	connect.AssertEqual(t, honorProviderUrlDarkVerdicts(11, []providerUrlDarkVerdict{a, never}), map[server.Id]bool{never.clientId: true})
	never2 := providerUrlDarkVerdict{clientId: server.Id{3}}
	connect.AssertEqual(t, honorProviderUrlDarkVerdicts(11, []providerUrlDarkVerdict{never, never2}), map[server.Id]bool{never2.clientId: true})
}

// The verdict span never drops below the control window, whatever the rules
// say: that floor is what closes the outage onset.
func TestProviderUrlDarkMinimumSpanFloor(t *testing.T) {
	rules := DefaultProviderEgressRules()
	rules.DarkMinimumSpanSeconds = 0
	connect.AssertEqual(t, providerUrlDarkMinimumSpan(rules), providerUrlDarkControlWindow)
	rules.DarkMinimumSpanSeconds = 2 * 60 * 60
	connect.AssertEqual(t, providerUrlDarkMinimumSpan(rules), 2*time.Hour)
}
