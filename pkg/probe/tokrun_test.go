package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

// stubTokenizerServer counts like the named DB entry: baseline
// requests cost 10, wrapped probes cost 10 + the entry's count.
func stubTokenizerServer(t *testing.T, db *fingerprint.DB, entryID string, drift bool) *httptest.Server {
	return httptest.NewServer(stubTokenizerHandler(t, db, entryID, drift))
}

func stubTokenizerHandler(t *testing.T, db *fingerprint.DB, entryID string, drift bool) http.Handler {
	t.Helper()
	var entry fingerprint.Entry
	for _, e := range db.Entries {
		if e.ID == entryID {
			entry = e
		}
	}
	if entry.ID == "" {
		t.Fatalf("entry %s not found", entryID)
	}
	byWrapped := map[string]int{}
	for i, p := range db.Probes {
		byWrapped[fingerprint.WrapTokText(p.Text)] = 10 + entry.Counts[i]
	}
	baselines := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		text := ""
		if len(body.Messages) > 0 {
			text = body.Messages[0].Content
		}
		tokens := 0
		if text == fingerprint.WrapTokText("") {
			baselines++
			tokens = 10
			if drift && baselines > 1 {
				tokens = 14
			}
		} else if n, ok := byWrapped[text]; ok {
			tokens = n
		} else {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"unknown probe"}}`)
			return
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d}}`, tokens)
	})
}

func TestTokenizerProbeIdentifiesStub(t *testing.T) {
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	srv := stubTokenizerServer(t, db, "deepseek-v3", false)
	defer srv.Close()
	c, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "deepseek-chat", Transport: config.TransportOpenAI}, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := TokenizerProbe(context.Background(), c, db, "deepseek-chat", TokOptions{MaxRequests: 24})
	if err != nil {
		t.Fatalf("TokenizerProbe: %v", err)
	}
	if res.Verdict != fingerprint.TokVerdictExact || res.TopFamily != "deepseek" {
		t.Fatalf("verdict = %s family = %s prob = %.4f", res.Verdict, res.TopFamily, res.Probability)
	}
	if res.BaselineDrift {
		t.Fatal("unexpected baseline drift")
	}
	if res.RequestsSpent < 6 || res.RequestsSpent > 24 {
		t.Fatalf("requests spent = %d", res.RequestsSpent)
	}
}

func TestTokenizerProbeBaselineDrift(t *testing.T) {
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	srv := stubTokenizerServer(t, db, "deepseek-v3", true)
	defer srv.Close()
	c, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "deepseek-chat", Transport: config.TransportOpenAI}, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := TokenizerProbe(context.Background(), c, db, "deepseek-chat", TokOptions{MaxRequests: 24})
	if err != nil {
		t.Fatalf("TokenizerProbe: %v", err)
	}
	if !res.BaselineDrift || res.Verdict != fingerprint.TokVerdictInconclusive || res.ReasonCode != fingerprint.TokReasonBaselineDrift {
		t.Fatalf("drift result = %+v", res)
	}
	// Candidates still show the underlying evidence.
	if len(res.Candidates) == 0 || res.Candidates[0].Family != "deepseek" {
		t.Fatalf("candidates = %+v", res.Candidates)
	}
}

func TestTokenizerProbeClosingBaselineFailure(t *testing.T) {
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"http_error", "no_usage", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			ordinary := stubTokenizerHandler(t, db, "deepseek-v3", false)
			baselines := 0
			c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var request struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				_ = json.Unmarshal(raw, &request)
				if request.Messages[0].Content == fingerprint.WrapTokText("") {
					baselines++
					if baselines > 1 {
						switch failure {
						case "http_error":
							w.WriteHeader(http.StatusBadRequest)
							fmt.Fprint(w, `{"error":{"message":"closing failed"}}`)
						case "no_usage":
							fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
						case "timeout":
							<-r.Context().Done()
						}
						return
					}
				}
				ordinary.ServeHTTP(w, r)
			})
			if failure == "timeout" {
				c.HTTP.Timeout = 100 * time.Millisecond
			}
			res, err := TokenizerProbe(context.Background(), c, db, "deepseek-chat", TokOptions{MaxProbes: 8, MaxRequests: 24})
			if err != nil || res.Verdict != fingerprint.TokVerdictInconclusive || res.ReasonCode != fingerprint.TokReasonBaselineUnvalidated || res.BaselineDrift {
				t.Fatalf("failed closing baseline = %+v, %v", res, err)
			}
			if !strings.Contains(res.ProbeError, "closing baseline") || res.Claim == nil || res.Claim.Status != "unchecked" {
				t.Fatalf("closing failure not surfaced: %+v", res)
			}
			if len(res.Candidates) == 0 || res.Candidates[0].Family != "deepseek" {
				t.Fatalf("lost partial evidence: %+v", res)
			}
		})
	}
}

func TestTokenizerProbeReservesClosingBaselineAcrossRetry(t *testing.T) {
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	ordinary := stubTokenizerHandler(t, db, "deepseek-v3", false)
	baselines, calls := 0, 0
	c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &request)
		baseline := request.Messages[0].Content == fingerprint.WrapTokText("")
		if baseline {
			baselines++
		}
		// After four usable probes, the next probe has two requests
		// left. Its transient failure must not consume the closing one.
		if calls == 6 && !baseline {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"message":"temporarily unavailable"}}`)
			return
		}
		ordinary.ServeHTTP(w, r)
	})
	res, err := TokenizerProbe(context.Background(), c, db, "deepseek-chat", TokOptions{MaxRequests: 7, Batch: 4})
	if err != nil || res.RequestsSpent != 7 || baselines != 2 || strings.Contains(res.ProbeError, "closing baseline") {
		t.Fatalf("closing baseline not reserved: baselines=%d, result=%+v, err=%v", baselines, res, err)
	}
	if res.BaselineDrift || res.ReasonCode == fingerprint.TokReasonBaselineUnvalidated {
		t.Fatalf("closing baseline not validated: %+v", res)
	}
}

func TestTokenizerProbeBaselineOnlyClaimUnchecked(t *testing.T) {
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":{"prompt_tokens":10}}`)
	})
	res, err := TokenizerProbe(context.Background(), c, db, "gpt-4o", TokOptions{MaxRequests: 1})
	if err != nil || res.RequestsSpent != 1 || res.Claim == nil || res.Claim.Status != "unchecked" || res.Verdict != fingerprint.TokVerdictInconclusive {
		t.Fatalf("baseline-only result = %+v, %v", res, err)
	}
}
