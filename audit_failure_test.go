package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/compare"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestAuditOfficialFailureTimingDoesNotChangeVerdict(t *testing.T) {
	for _, claim := range []string{"deepseek", "qwen"} {
		for _, successes := range []int{0, 2, 3, 4, 6} {
			t.Run(fmt.Sprintf("%s/successes=%d", claim, successes), func(t *testing.T) {
				cleanEnv(t)
				target := openAIStyleMock(t, "deepseek-v3", 6)
				var calls atomic.Int64
				official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) > int64(successes) {
						http.Error(w, "baseline failed", http.StatusInternalServerError)
						return
					}
					target.Config.Handler.ServeHTTP(w, r)
				}))
				t.Cleanup(official.Close)
				code, out, stderr := runCLI(t, "audit", "--target-url", target.URL, "--official-url", official.URL,
					"--model", "deepseek-chat", "--claim", claim, "--json")
				var got auditOutput
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("invalid JSON: %v; exit=%d stderr=%s output=%s", err, code, stderr, out)
				}
				if code != exitInconclusive || got.Verdict != compare.VerdictInconclusive || got.ReasonCode != compare.ReasonOfficialFailed {
					t.Errorf("exit=%d verdict=%s reason_code=%s; failed baseline must be inconclusive/official_failed", code, got.Verdict, got.ReasonCode)
				}
				if got.Target.Status != fingerprint.StatusIdentified || got.Target.TopFamily != "deepseek" {
					t.Fatalf("target evidence was not preserved: %+v", got.Target)
				}
				if got.RequestsSpent != got.Target.RequestsSpent+successes+1 || calls.Load() != int64(successes+1) {
					t.Errorf("incorrect spent requests: total=%d target=%d official=%d", got.RequestsSpent, got.Target.RequestsSpent, calls.Load())
				}
				if got.Divergence != nil {
					t.Error("a failed baseline must not report corroborating divergence")
				}
				if successes >= fingerprint.MinUsableProbes {
					if got.Official == nil || len(got.Official.Observations) != successes+1 || got.Official.RequestsSpent != successes+1 {
						t.Fatalf("partial baseline evidence was not preserved: %+v", got.Official)
					}
					if got.Official.ProbeError == "" {
						t.Error("partial baseline omitted the structured probe error")
					}
					if successes == 6 && got.Official.Status != fingerprint.StatusIdentified {
						t.Fatal("fixture must exercise an identified but operationally failed baseline")
					}
				}
			})
		}
	}
}

func TestAuditCompletedInconclusiveOfficialKeepsTargetVerdict(t *testing.T) {
	for _, tc := range []struct {
		claim, verdict string
		exit           int
	}{
		{"deepseek", compare.VerdictMatch, exitOK},
		{"qwen", compare.VerdictMismatch, exitMismatch},
	} {
		t.Run(tc.claim, func(t *testing.T) {
			cleanEnv(t)
			target := openAIStyleMock(t, "deepseek-v3", 6)
			official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{}`) // Completed response, but no usable token counts.
			}))
			t.Cleanup(official.Close)
			code, out, stderr := runCLI(t, "audit", "--target-url", target.URL, "--official-url", official.URL,
				"--model", "deepseek-chat", "--claim", tc.claim, "--json")
			var got auditOutput
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("invalid JSON: %v; exit=%d stderr=%s output=%s", err, code, stderr, out)
			}
			if code != tc.exit || got.Verdict != tc.verdict || got.ReasonCode == compare.ReasonOfficialFailed {
				t.Fatalf("completed inconclusive baseline changed policy: exit=%d result=%+v", code, got.Result)
			}
			if got.Official == nil || got.Official.Status != fingerprint.StatusInconclusive || got.Official.ProbeError != "" {
				t.Fatalf("expected a completed inconclusive baseline: %+v", got.Official)
			}
		})
	}
}
