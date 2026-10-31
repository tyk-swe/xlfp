package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

// openAIStyleMock emulates an OpenAI-compatible endpoint whose prompt_token
// counts match the given DB entry plus a constant overhead.
func openAIStyleMock(t *testing.T, entryID string, overhead int) *httptest.Server {
	t.Helper()
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range db.Entries {
		if e.ID == entryID {
			for i, p := range db.Probes {
				counts[p.Text] = e.Counts[i] + overhead
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if len(body.Messages) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c, ok := counts[body.Messages[0].Content]
		if !ok {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "x"}}},
			"usage":   map[string]any{"prompt_tokens": c, "completion_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cleanEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// os.UserConfigDir prefers XDG_CONFIG_HOME on Linux; isolate it too.
	t.Setenv("XDG_CONFIG_HOME", home)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "XLFP_") {
			t.Setenv(k, "")
		}
	}
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(k, "")
	}
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestFamiliesAndVersion(t *testing.T) {
	cleanEnv(t)
	code, out, _ := runCLI(t, "families")
	if code != exitOK || !strings.Contains(out, "deepseek-v3") {
		t.Fatalf("families: code %d out %q", code, out)
	}
	for _, id := range []string{"kimi-k2", "kimi-dev"} {
		if !strings.Contains(out, id) {
			t.Errorf("families output omits %s: %q", id, out)
		}
	}
	code, out, _ = runCLI(t, "version")
	if code != exitOK || !strings.Contains(out, "xlfp") {
		t.Fatalf("version: code %d out %q", code, out)
	}
	if code, _, _ = runCLI(t, "bogus"); code != exitError {
		t.Fatalf("bogus command code = %d", code)
	}
	if code, _, _ = runCLI(t, "uncover", "--model", "x"); code != exitError {
		t.Fatalf("uncover without endpoint code = %d", code)
	}
}

func TestUncoverEndToEnd(t *testing.T) {
	cleanEnv(t)
	srv := openAIStyleMock(t, "deepseek-v3", 6)
	code, out, errOut := runCLI(t, "uncover", "--endpoint", srv.URL, "--model", "stealth-model-x")
	if code != exitOK {
		t.Fatalf("code %d stderr %q stdout %q", code, errOut, out)
	}
	if !strings.Contains(out, "IDENTIFIED") || !strings.Contains(out, "deepseek") {
		t.Fatalf("out %q", out)
	}
}

func TestUncoverJSONAndKeyHygiene(t *testing.T) {
	cleanEnv(t)
	t.Setenv("OPENAI_API_KEY", "SECRETKEY-DO-NOT-LEAK")
	srv := openAIStyleMock(t, "deepseek-v3", 6)
	code, out, _ := runCLI(t, "uncover", "--endpoint", srv.URL, "--model", "stealth-model-x", "--json")
	if code != exitOK {
		t.Fatalf("code %d out %q", code, out)
	}
	if strings.Contains(out, "SECRETKEY-DO-NOT-LEAK") {
		t.Fatal("API key leaked into JSON output")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["command"] != "uncover" || parsed["status"] != "identified" || parsed["top_family"] != "deepseek" {
		t.Fatalf("parsed = %v", parsed)
	}
	if parsed["db_version"] == nil || parsed["requests_spent"] == nil || parsed["rankings"] == nil || parsed["observations"] == nil {
		t.Fatalf("missing stable JSON fields: %v", keys(parsed))
	}
}

func keys(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestAuditMatchMismatchInconclusive(t *testing.T) {
	cleanEnv(t)
	srv := openAIStyleMock(t, "deepseek-v3", 6)

	code, out, _ := runCLI(t, "audit", "--target-url", srv.URL, "--model", "deepseek-chat", "--claim", "deepseek")
	if code != exitOK || !strings.Contains(out, "MATCH") {
		t.Fatalf("match: code %d out %q", code, out)
	}
	code, out, _ = runCLI(t, "audit", "--target-url", srv.URL, "--model", "qwen-max", "--claim", "qwen")
	if code != exitMismatch || !strings.Contains(out, "MISMATCH") {
		t.Fatalf("mismatch: code %d out %q", code, out)
	}
	// Unverified claimed family cannot be judged.
	code, out, _ = runCLI(t, "audit", "--target-url", srv.URL, "--model", "gemini-2.0-flash", "--claim", "google")
	if code != exitInconclusive || !strings.Contains(out, "INCONCLUSIVE") {
		t.Fatalf("unverified claim: code %d out %q", code, out)
	}
	// Claim inferred from the model name.
	code, _, _ = runCLI(t, "audit", "--target-url", srv.URL, "--model", "deepseek-chat")
	if code != exitOK {
		t.Fatalf("inferred claim: code %d", code)
	}
	if code, _, _ = runCLI(t, "audit", "--target-url", srv.URL, "--model", "mystery-1"); code != exitError {
		t.Fatalf("uninferable claim should be a usage error, code %d", code)
	}
}

func TestKimiAuditAndSharedTokenizer(t *testing.T) {
	cleanEnv(t)
	k2 := openAIStyleMock(t, "kimi-k2", 6)
	for _, args := range [][]string{
		{"audit", "--target-url", k2.URL, "--model", "moonshotai/Kimi-K2-Instruct"},
		{"audit", "--target-url", k2.URL, "--model", "unknown", "--claim", "moonshot"},
	} {
		code, out, errOut := runCLI(t, args...)
		if code != exitOK || !strings.Contains(out, "MATCH") || !strings.Contains(out, "kimi") {
			t.Errorf("Kimi match: code %d stdout %q stderr %q", code, out, errOut)
		}
	}
	code, out, errOut := runCLI(t, "audit", "--target-url", k2.URL, "--model", "kimi-k2", "--claim", "deepseek")
	if code != exitMismatch || !strings.Contains(out, "MISMATCH") {
		t.Errorf("Kimi mismatch: code %d stdout %q stderr %q", code, out, errOut)
	}

	dev := openAIStyleMock(t, "kimi-dev", 6)
	code, out, errOut = runCLI(t, "audit", "--target-url", dev.URL, "--model", "Kimi-Dev-72B")
	if code != exitInconclusive || !strings.Contains(out, "INCONCLUSIVE") {
		t.Errorf("Kimi-Dev audit: code %d stdout %q stderr %q", code, out, errOut)
	}
	for _, name := range []string{"Kimi", "Qwen", "MiMo"} {
		if !strings.Contains(out, name) {
			t.Errorf("shared-tokenizer report omits %s: %q", name, out)
		}
	}
	if strings.Contains(out, "these two") {
		t.Errorf("three-way tie described as two: %q", out)
	}
	code, out, errOut = runCLI(t, "uncover", "--endpoint", dev.URL, "--model", "stealth", "--json")
	var res fingerprint.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("uncover JSON: %v, output %q", err, out)
	}
	if code != exitInconclusive || res.ReasonCode != fingerprint.ReasonSharedTokenizer {
		t.Fatalf("shared-tokenizer JSON: code %d reason %q stderr %q", code, res.ReasonCode, errOut)
	}
	for _, family := range []string{"kimi", "mimo", "qwen"} {
		found := false
		for _, rank := range res.Rankings {
			if rank.Family == family && len(rank.SharedWith) == 2 {
				found = true
			}
		}
		if !found || !strings.Contains(res.Reason, family) {
			t.Errorf("JSON does not show %s in shared tie: %+v", family, res)
		}
	}
}

func TestAuditWithLiveOfficial(t *testing.T) {
	cleanEnv(t)
	target := openAIStyleMock(t, "deepseek-v3", 6)
	official := openAIStyleMock(t, "deepseek-v3", 11) // different template overhead
	code, out, _ := runCLI(t, "audit", "--target-url", target.URL, "--official-url", official.URL,
		"--model", "deepseek-chat", "--claim", "deepseek", "--json")
	if code != exitOK {
		t.Fatalf("code %d out %q", code, out)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["verdict"] != "match" || parsed["official"] == nil || parsed["divergence"] == nil {
		t.Fatalf("parsed verdict/official/divergence = %v %v %v", parsed["verdict"], parsed["official"] != nil, parsed["divergence"] != nil)
	}
	div := parsed["divergence"].(map[string]any)
	if div["mean_abs_diff"].(float64) != 0 {
		t.Fatalf("same tokenizer with different overheads must normalize to zero divergence, got %v", div["mean_abs_diff"])
	}
}

// Requests already sent to a failing official endpoint are still spent.
func TestAuditOfficialFailureCountsRequests(t *testing.T) {
	cleanEnv(t)
	target := openAIStyleMock(t, "deepseek-v3", 6)
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(official.Close)
	code, out, _ := runCLI(t, "audit", "--target-url", target.URL, "--official-url", official.URL,
		"--model", "deepseek-chat", "--claim", "deepseek", "--json")
	var parsed struct {
		ReasonCode    string `json:"reason_code"`
		RequestsSpent int    `json:"requests_spent"`
		Target        struct {
			RequestsSpent int `json:"requests_spent"`
		} `json:"target"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("code %d out %q: %v", code, out, err)
	}
	if code != exitInconclusive || parsed.ReasonCode != "official_failed" {
		t.Fatalf("code %d reason_code %q", code, parsed.ReasonCode)
	}
	if parsed.RequestsSpent != parsed.Target.RequestsSpent+1 {
		t.Fatalf("requests_spent %d, want target %d + 1 official", parsed.RequestsSpent, parsed.Target.RequestsSpent)
	}
}

func stubStdin(t *testing.T, tty bool, input string) {
	t.Helper()
	oldReader, oldTTY := stdinReader, stdinIsTTY
	stdinReader = strings.NewReader(input)
	stdinIsTTY = func() bool { return tty }
	t.Cleanup(func() { stdinReader, stdinIsTTY = oldReader, oldTTY })
}

func TestNoArgsNonTTYShowsUsage(t *testing.T) {
	cleanEnv(t)
	stubStdin(t, false, "")
	code, _, errOut := runCLI(t)
	if code != exitError || !strings.Contains(errOut, "guided mode") {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
}

func TestWizardMissingKeyStopsBeforeSending(t *testing.T) {
	cleanEnv(t)
	stubStdin(t, true, "1\nhttps://provider.example\nmodel-x\nn\n")
	code, out, _ := runCLI(t)
	if code != exitError {
		t.Fatalf("code %d out %q", code, out)
	}
	if !strings.Contains(out, "export XLFP_API_KEY") || !strings.Contains(out, "Nothing was sent") {
		t.Fatalf("out %q", out)
	}
}

func TestWizardUncoverLoopbackEndToEnd(t *testing.T) {
	cleanEnv(t)
	srv := openAIStyleMock(t, "deepseek-v3", 6)
	// Loopback endpoints need no key prompt; blank line accepts the confirm.
	stubStdin(t, true, "1\n"+srv.URL+"\nstealth-x\n\n")
	code, out, _ := runCLI(t)
	if code != exitOK || !strings.Contains(out, "IDENTIFIED") || !strings.Contains(out, "DeepSeek") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestWizardDeclineAtConfirm(t *testing.T) {
	cleanEnv(t)
	srv := openAIStyleMock(t, "deepseek-v3", 6)
	stubStdin(t, true, "1\n"+srv.URL+"\nstealth-x\nn\n")
	code, out, _ := runCLI(t)
	if code != exitError || !strings.Contains(out, "Nothing was sent") || strings.Contains(out, "IDENTIFIED") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestExplainAndHints(t *testing.T) {
	cleanEnv(t)
	code, out, _ := runCLI(t, "explain")
	if code != exitOK || !strings.Contains(out, "plain words") || !strings.Contains(out, "INCONCLUSIVE") {
		t.Fatalf("explain: code %d out %q", code, out)
	}
	code, _, errOut := runCLI(t, "uncover", "--model", "x")
	if code != exitError || !strings.Contains(errOut, "Fix:") || !strings.Contains(errOut, "--endpoint") {
		t.Fatalf("hint: code %d stderr %q", code, errOut)
	}
}
