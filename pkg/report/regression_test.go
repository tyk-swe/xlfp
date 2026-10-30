package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/compare"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestPlainReportsDisclosePartialProbeFailure(t *testing.T) {
	r := fingerprint.Result{Status: fingerprint.StatusIdentified, TopFamily: "deepseek", Reason: "probing stopped early: HTTP 401", Rankings: []fingerprint.FamilyRank{{Family: "deepseek", Confidence: 0.98}}}
	var out bytes.Buffer
	Uncover(&out, "https://example.test", "m", "openai", r, false)
	if !strings.Contains(out.String(), "stopped early") {
		t.Fatal("default uncover hides partial failure")
	}
	out.Reset()
	Audit(&out, "https://example.test", "", compare.Result{Verdict: compare.VerdictMatch, ClaimedFamily: "deepseek", Target: r}, false)
	if !strings.Contains(out.String(), "stopped early") {
		t.Fatal("default audit hides partial failure")
	}
}

func TestTokProbeUncheckedKnownClaim(t *testing.T) {
	var out bytes.Buffer
	TokProbe(&out, "https://example.test", "gpt-4o", "openai", fingerprint.TokResult{
		Verdict: fingerprint.TokVerdictInconclusive,
		Reason:  "insufficient counts",
		Claim:   &fingerprint.TokClaim{Model: "gpt-4o", Family: "openai", Status: "unchecked"},
	}, false)
	if strings.Contains(out.String(), "does not map to a known family") || strings.Contains(out.String(), "INCONSISTENT") {
		t.Fatalf("unchecked known-family claim misrepresented: %s", out.String())
	}
	if !strings.Contains(out.String(), "cannot yet be checked") {
		t.Fatalf("missing unchecked claim explanation: %s", out.String())
	}
}
