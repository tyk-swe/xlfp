package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

// stubTokenizerServer counts like the named DB entry: baseline
// requests cost 10, wrapped probes cost 10 + the entry's count.
func stubTokenizerServer(t *testing.T, db *fingerprint.DB, entryID string, drift bool) *httptest.Server {
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
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
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
