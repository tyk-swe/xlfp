package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func synthSeqForStub(r *rand.Rand, centers []int, n int) []int {
	seq := make([]int, 0, n)
	prev := -1
	for len(seq) < n {
		c := centers[r.Intn(len(centers))]
		if c == prev {
			continue
		}
		prev = c
		v := c + r.Intn(31) - 15
		if v < 1 {
			v = 1
		}
		if v > 355 {
			v = 355
		}
		seq = append(seq, v)
	}
	return seq
}

// numbersStubServer answers every chat request with a fresh
// model-like number sequence around the given centers.
func numbersStubServer(t *testing.T, centers []int) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := calls.Add(1)
		r := rand.New(rand.NewSource(int64(1000 + n*77)))
		seq := synthSeqForStub(r, centers, 320)
		var sb strings.Builder
		for i, v := range seq {
			if i > 0 {
				sb.WriteByte(' ')
			}
			fmt.Fprintf(&sb, "%d", v)
		}
		body, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": sb.String()}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 50},
		})
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSampleEnrollRetrainNumbersFlow(t *testing.T) {
	cleanEnv(t)
	centersA := []int{37, 73, 137, 210, 286, 337}
	centersB := []int{58, 99, 164, 243, 301, 352}
	srvA := numbersStubServer(t, centersA)
	srvB := numbersStubServer(t, centersB)
	dir := t.TempDir()
	bankPath := filepath.Join(dir, "bank.json")
	for _, tc := range []struct {
		srv           *httptest.Server
		model, family string
	}{
		{srvA, "model-a", "fam-a"},
		{srvB, "model-b", "fam-b"},
	} {
		batch := filepath.Join(dir, tc.model+".json")
		code, _, stderr := runCLI(t, "sample", "--endpoint", tc.srv.URL, "--model", tc.model, "--family", tc.family, "--out", batch, "--no-stream")
		if code != 0 {
			t.Fatalf("sample %s exit %d: %s", tc.model, code, stderr)
		}
		if code, _, stderr := runCLI(t, "enroll", batch, "--bank", bankPath); code != 0 {
			t.Fatalf("enroll %s exit %d: %s", tc.model, code, stderr)
		}
	}
	code, out, stderr := runCLI(t, "retrain", "--bank", bankPath)
	if code != 0 {
		t.Fatalf("retrain exit %d: %s", code, stderr)
	}
	if !strings.Contains(out, "leave-one-out accuracy") {
		t.Fatalf("retrain output: %s", out)
	}
	// Live numbers against stub A must rank fam-a first, calibrated.
	saved := filepath.Join(dir, "result.json")
	code, out, stderr = runCLI(t, "numbers", "--endpoint", srvA.URL, "--model", "model-a", "--bank", bankPath, "--no-stream", "--json", "--output", saved)
	if code != 0 {
		t.Fatalf("numbers exit %d: %s\n%s", code, stderr, out)
	}
	var live numbersOutput
	if err := json.Unmarshal([]byte(out), &live); err != nil {
		t.Fatalf("numbers JSON: %v", err)
	}
	if len(live.Rounds) != 1 || live.Rounds[0].Result.TopFamily != "fam-a" || !live.Rounds[0].Result.Calibrated {
		t.Fatalf("live result: %+v", live.Rounds)
	}
	if live.RequestsSpent != 3 {
		t.Fatalf("requests spent = %d, want 3", live.RequestsSpent)
	}
	// Offline recomputation from the saved report must agree and
	// spend no requests.
	code, out, _ = runCLI(t, "numbers", "--input", saved, "--bank", bankPath, "--json")
	if code != 0 {
		t.Fatalf("offline numbers exit %d: %s", code, out)
	}
	var offline numbersOutput
	if err := json.Unmarshal([]byte(out), &offline); err != nil {
		t.Fatalf("offline JSON: %v", err)
	}
	if offline.Rounds[0].Result.TopEntryID != live.Rounds[0].Result.TopEntryID {
		t.Fatalf("offline top %s != live top %s", offline.Rounds[0].Result.TopEntryID, live.Rounds[0].Result.TopEntryID)
	}
	if offline.RequestsSpent != 0 {
		t.Fatalf("offline requests spent = %d, want 0", offline.RequestsSpent)
	}
}

func TestNumbersOfflineEmbeddedBankInconclusive(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	paste := filepath.Join(dir, "paste.txt")
	seq := synthSeqForStub(rand.New(rand.NewSource(4)), []int{37, 210, 337}, 300)
	var sb strings.Builder
	for i, v := range seq {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%d", v)
	}
	if err := os.WriteFile(paste, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "numbers", "--input", paste, "--json")
	if code != exitInconclusive {
		t.Fatalf("exit %d, want inconclusive: %s", code, out)
	}
	if !strings.Contains(out, "insufficient_bank") {
		t.Fatalf("expected insufficient_bank reason: %s", out)
	}
}

func TestTokprobeCommandAgainstStub(t *testing.T) {
	cleanEnv(t)
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	var entry fingerprint.Entry
	for _, e := range db.Entries {
		if e.ID == "deepseek-v3" {
			entry = e
		}
	}
	byWrapped := map[string]int{}
	for i, p := range db.Probes {
		byWrapped[fingerprint.WrapTokText(p.Text)] = 10 + entry.Counts[i]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		text := ""
		if len(body.Messages) > 0 {
			text = body.Messages[0].Content
		}
		tokens := 10
		if n, ok := byWrapped[text]; ok {
			tokens = n
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d}}`, tokens)
	}))
	defer srv.Close()
	code, out, stderr := runCLI(t, "tokprobe", "--endpoint", srv.URL, "--model", "deepseek-chat", "--json")
	if code != 0 {
		t.Fatalf("tokprobe exit %d: %s\n%s", code, stderr, out)
	}
	var res tokprobeOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("tokprobe JSON: %v", err)
	}
	if res.Verdict != fingerprint.TokVerdictExact || res.TopFamily != "deepseek" {
		t.Fatalf("verdict = %s family = %s", res.Verdict, res.TopFamily)
	}
	if res.Claim == nil || res.Claim.Status != "consistent" {
		t.Fatalf("claim = %+v", res.Claim)
	}
}
