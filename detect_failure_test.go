package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/numprint"
	"github.com/tyk-swe/xlfp/pkg/probe"
)

func TestNumbersReportsOperationalFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantError string
		status                int
		stream                bool
	}{
		{"unauthorized", `{"error":{"message":"invalid API key secret-for-test"}}`, "HTTP 401", http.StatusUnauthorized, false},
		{"endpoint_error", `{"error":{"message":"model unavailable"}}`, "model unavailable", http.StatusOK, false},
		{"stream_error", "data: {\"error\":{\"message\":\"stream failed\"}}\n\n", "stream failed", http.StatusOK, true},
	} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", tc.name, jsonOut), func(t *testing.T) {
				cleanEnv(t)
				t.Setenv("XLFP_API_KEY", "secret-for-test")
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}))
				defer srv.Close()
				saved := filepath.Join(t.TempDir(), "report.json")
				args := []string{"numbers", "--endpoint", srv.URL, "--model", "m", "--count", "1", "--rounds", "2", "--output", saved}
				if !tc.stream {
					args = append(args, "--no-stream")
				}
				if jsonOut {
					args = append(args, "--json")
				}
				code, out, stderr := runCLI(t, args...)
				if code != exitError || !strings.Contains(stderr, tc.wantError) {
					t.Errorf("exit=%d, stderr=%q; want operational error containing %q", code, stderr, tc.wantError)
				}
				if strings.Contains(out+stderr, "secret-for-test") {
					t.Fatal("API key leaked in failure output")
				}
				raw, err := os.ReadFile(saved)
				if err != nil {
					t.Fatal(err)
				}
				if jsonOut {
					raw = []byte(out)
				}
				var report numbersOutput
				if err := json.Unmarshal(raw, &report); err != nil {
					t.Fatalf("failure report is not valid JSON: %v", err)
				}
				if report.RequestsSpent != 2 || len(report.Rounds) != 2 {
					t.Fatalf("failure discarded later rounds: %+v", report.NumRunResult)
				}
				for _, round := range report.Rounds {
					if len(round.Answers) != 1 || !strings.Contains(round.Answers[0].Error, tc.wantError) {
						t.Fatalf("failure evidence missing: %+v", round.Answers)
					}
				}
			})
		}
	}
}

func TestNumbersOfflineFailurePrecedesRankingStatus(t *testing.T) {
	cleanEnv(t)
	text := strings.Repeat("37 173 ", 150)
	bank := &numprint.Bank{Version: "test", Entries: []numprint.Entry{
		{ID: "a", Family: "a", Samples: []numprint.Sample{{ID: "a1", Sequence: numprint.ParseSequence(text)}}},
		{ID: "b", Family: "b", Samples: []numprint.Sample{{ID: "b1", Sequence: numprint.ParseSequence(strings.Repeat("73 274 ", 150))}}},
	}}
	bankPath := filepath.Join(t.TempDir(), "bank.json")
	if err := numprint.SaveBankFile(bankPath, bank); err != nil {
		t.Fatal(err)
	}
	for _, initialInconclusive := range []bool{false, true} {
		t.Run(fmt.Sprintf("initial_inconclusive=%v", initialInconclusive), func(t *testing.T) {
			var rounds []probe.NumRound
			if initialInconclusive {
				rounds = append(rounds, probe.NumRound{Answers: []numprint.Answer{{Text: "too short"}}})
			}
			rounds = append(rounds, probe.NumRound{Answers: []numprint.Answer{
				{Text: text, Completed: true},
				{Error: "HTTP 401: \x1b[31mdenied\x1b[0m"},
			}})
			raw, _ := json.Marshal(map[string]any{"rounds": rounds})
			input := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(input, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := runCLI(t, "numbers", "--input", input, "--bank", bankPath, "--json")
			if code != exitError || !strings.Contains(stderr, "HTTP 401") || strings.Contains(stderr, "\x1b") {
				t.Errorf("exit=%d, stderr=%q; want sanitized operational error", code, stderr)
			}
			var report numbersOutput
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			last := report.Rounds[len(report.Rounds)-1]
			if last.Result.Status != numprint.StatusRanked || last.Result.ValidAnswers != 1 || report.RequestsSpent != 0 {
				t.Fatalf("offline failure discarded usable evidence: %+v", report.NumRunResult)
			}
		})
	}
}
