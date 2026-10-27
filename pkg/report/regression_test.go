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
