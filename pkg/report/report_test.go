package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/compare"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestSanitizeURL(t *testing.T) {
	cases := map[string]string{
		"https://user:pass@example.com/v1?key=secret#frag": "https://example.com/v1",
		"https://example.com/v1?api-version=1":             "https://example.com/v1",
		"https:opaque-secret":                              "(unparseable endpoint)",
		"http://127.0.0.1:8080":                            "http://127.0.0.1:8080",
	}
	for in, want := range cases {
		if got := SanitizeURL(in); got != want {
			t.Errorf("SanitizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUncoverEmptyRankingsNoPanic(t *testing.T) {
	var buf bytes.Buffer
	Uncover(&buf, "https://example.com", "m", "openai", fingerprint.Result{Status: fingerprint.StatusIdentified}, false)
	if !strings.Contains(buf.String(), "Endpoint:") {
		t.Fatalf("output = %q", buf.String())
	}
}

func identifiedResult() fingerprint.Result {
	return fingerprint.Result{
		Status: fingerprint.StatusIdentified, TopFamily: "deepseek", DBVersion: "1.0.0", RequestsSpent: 8,
		Rankings: []fingerprint.FamilyRank{
			{Family: "deepseek", Display: "DeepSeek", EntryID: "deepseek-v3", Tokenizer: "DeepSeek V3 BPE", Verified: true, Confidence: 0.984, MAE: 0, Overhead: 6},
		},
		Observations: []fingerprint.Observation{{ProbeID: "en_short", PromptTokens: 8, HasUsage: true, HTTPStatus: 200}},
	}
}

func TestUncoverPlainIdentified(t *testing.T) {
	var buf bytes.Buffer
	Uncover(&buf, "https://example.com", "stealth-x", "openai", identifiedResult(), false)
	out := buf.String()
	for _, want := range []string{"IDENTIFIED", "DeepSeek", "deepseek", "98%", "cannot prove an exact model"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain output missing %q:\n%s", want, out)
		}
	}
	// Technical detail (MAE, probe table) is verbose-only.
	for _, unwanted := range []string{"MAE", "overhead", "Probe evidence:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("plain output should not contain %q:\n%s", unwanted, out)
		}
	}
}

func TestUncoverVerboseShowsTechnicalDetail(t *testing.T) {
	var buf bytes.Buffer
	Uncover(&buf, "https://example.com", "stealth-x", "openai", identifiedResult(), true)
	out := buf.String()
	for _, want := range []string{"IDENTIFIED", "MAE", "Probe evidence:"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose output missing %q:\n%s", want, out)
		}
	}
}

func TestUncoverPlainInconclusive(t *testing.T) {
	res := fingerprint.Result{
		Status: fingerprint.StatusInconclusive, DBVersion: "1.0.0", RequestsSpent: 3,
		Reason:     "only 1 usable token counts (usage missing, non-positive, or probes rejected); at least 3 are required",
		ReasonCode: fingerprint.ReasonInsufficientCounts,
		Rankings:   []fingerprint.FamilyRank{}, Unverified: []fingerprint.FamilyRank{},
	}
	var buf bytes.Buffer
	Uncover(&buf, "https://example.com", "m", "openai", res, false)
	out := buf.String()
	for _, want := range []string{"INCONCLUSIVE", "What you can try:", "--transport"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain inconclusive missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "usable token counts (usage missing") {
		t.Errorf("plain output leaked raw technical reason:\n%s", out)
	}
	buf.Reset()
	Uncover(&buf, "https://example.com", "m", "openai", res, true)
	if !strings.Contains(buf.String(), "usable token counts (usage missing") {
		t.Errorf("verbose output should include the technical reason:\n%s", buf.String())
	}
}

func TestSharedTokenizerExplanationNamesEveryFamily(t *testing.T) {
	for _, c := range []struct {
		name       string
		shared     []string
		wantLabels []string
	}{
		{"two families", []string{"kimi"}, []string{"MiMo", "Kimi"}},
		{"three families", []string{"kimi", "qwen"}, []string{"MiMo", "Kimi", "Qwen"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := fingerprint.Result{
				Status: fingerprint.StatusInconclusive, ReasonCode: fingerprint.ReasonSharedTokenizer,
				Rankings: []fingerprint.FamilyRank{{Family: "mimo", Display: "MiMo", SharedWith: c.shared}},
			}
			var buf bytes.Buffer
			Uncover(&buf, "https://example.com", "m", "openai", res, false)
			var explanation string
			for _, line := range strings.Split(buf.String(), "\n") {
				if strings.HasPrefix(line, "Why: ") {
					explanation = line
				}
			}
			for _, label := range c.wantLabels {
				if !strings.Contains(explanation, label) {
					t.Errorf("explanation %q omits %s", explanation, label)
				}
			}
			if strings.Contains(buf.String(), "these two") {
				t.Errorf("explanation assumes a two-family tie: %q", buf.String())
			}
		})
	}
}

func TestAuditPlainMismatch(t *testing.T) {
	target := identifiedResult()
	target.TopFamily = "qwen"
	target.Rankings[0].Family, target.Rankings[0].Display = "qwen", "Qwen"
	res := compare.Result{
		Verdict: compare.VerdictMismatch, ClaimedFamily: "deepseek", RequestsSpent: 8,
		Reason: "target fingerprints as qwen (confidence 0.984), not claimed family deepseek",
		Target: target,
	}
	var buf bytes.Buffer
	Audit(&buf, "https://provider.example", "", res, false)
	out := buf.String()
	for _, want := range []string{"MISMATCH", "Qwen", "DeepSeek", "not, on its own, proof of fraud"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain audit missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MAE") {
		t.Errorf("plain audit should not contain MAE:\n%s", out)
	}
}
