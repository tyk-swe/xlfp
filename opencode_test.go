package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestMain(m *testing.M) {
	if os.Getenv("XLFP_OPENCODE_CLI_HELPER") == "1" {
		fakeOpenCodeRoot()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeOpenCodeRoot() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("1.19.4")
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "debug" && os.Args[2] == "config" {
		if os.Getenv("XLFP_OPENCODE_MODE") == "managed_config" {
			fmt.Println(`{"agent":{"xlfp":{"prompt":"sk-root-secret"}}}`)
		} else {
			var config map[string]any
			_ = json.Unmarshal([]byte(os.Getenv("OPENCODE_CONFIG_CONTENT")), &config)
			config["command"] = map[string]any{}
			_ = json.NewEncoder(os.Stdout).Encode(config)
		}
		return
	}
	if log := os.Getenv("XLFP_OPENCODE_RUN_LOG"); log != "" {
		f, _ := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if f != nil {
			wd, _ := os.Getwd()
			fmt.Fprintln(f, wd)
			f.Close()
		}
	}
	mode := os.Getenv("XLFP_OPENCODE_MODE")
	if mode == "error" {
		fmt.Println(`{"type":"error","error":{"message":"sk-root-secret"}}`)
		fmt.Fprintln(os.Stderr, "sk-root-secret diagnostics")
		os.Exit(1)
	}
	if mode == "hang" {
		time.Sleep(5 * time.Second)
		return
	}
	raw, _ := io.ReadAll(os.Stdin)
	db, _ := fingerprint.Load()
	tokens := 0
	for _, e := range db.Entries {
		if e.ID != "deepseek-v3" {
			continue
		}
		for i, p := range db.Probes {
			if p.Text == string(raw) {
				tokens = e.Counts[i] + 150
			}
		}
	}
	if tokens == 0 {
		os.Exit(2)
	}
	fmt.Printf("{\"type\":\"step_finish\",\"sessionID\":\"s1\",\"part\":{\"type\":\"step-finish\",\"id\":\"f1\",\"sessionID\":\"s1\",\"messageID\":\"m1\",\"tokens\":{\"input\":%d,\"cache\":{\"read\":80,\"write\":20}}}}\n", tokens-100)
}

func rootOpenCodeHelper(t *testing.T, mode string) (string, string) {
	t.Helper()
	cleanEnv(t)
	t.Setenv("XLFP_OPENCODE_CLI_HELPER", "1")
	t.Setenv("XLFP_OPENCODE_MODE", mode)
	// Preserve race options while avoiding a post-exit delay in CLI children.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	log := filepath.Join(t.TempDir(), "runs")
	t.Setenv("XLFP_OPENCODE_RUN_LOG", log)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe, log
}

func TestOpenCodeCLIReportsAndExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, cap string
		json            bool
		want            int
	}{
		{"identified_json", "", "12", true, exitOK},
		{"inconclusive_json", "", "1", true, exitInconclusive},
		{"plain", "", "1", false, exitInconclusive},
		{"failure", "error", "1", true, exitError},
		{"managed_configuration", "managed_config", "1", true, exitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe, log := rootOpenCodeHelper(t, tc.mode)
			args := []string{"opencode", "--model", "opencode/big-pickle", "--opencode-bin", exe, "--max-probes", tc.cap}
			if tc.json {
				args = append(args, "--json")
			}
			code, out, stderr := runCLI(t, args...)
			if code != tc.want || strings.Contains(out+stderr, "sk-root-secret") {
				t.Fatalf("code %d, out %s, stderr %s", code, out, stderr)
			}
			if tc.want == exitError {
				if tc.mode == "managed_config" {
					if _, err := os.Stat(log); !os.IsNotExist(err) {
						t.Fatal("unsafe configuration launched a probe")
					}
				}
				return
			}
			if strings.Contains(out, "requests_spent") || strings.Contains(out, "tiny test requests") {
				t.Fatalf("misleading HTTP accounting: %s", out)
			}
			if tc.json {
				var result struct {
					Command, Model string
					ProbesSpent    int    `json:"probes_spent"`
					Version        string `json:"opencode_version"`
					fingerprintEvidence
				}
				if err := json.Unmarshal([]byte(out), &result); err != nil || result.Command != "opencode" || result.Model != "opencode/big-pickle" || result.Version != "1.19.4" || result.ProbesSpent < 1 || len(result.Observations) != result.ProbesSpent {
					t.Fatalf("invalid JSON result: %s, %v", out, err)
				}
				if tc.want == exitOK && (result.TopFamily != "deepseek" || result.ProbesSpent >= 12) {
					t.Fatalf("did not identify adaptively: %s", out)
				}
			} else if !strings.Contains(out, "Probe runs: 1") || !strings.Contains(out, "OpenCode: 1.19.4") {
				t.Fatalf("missing OpenCode identity/accounting: %s", out)
			}
			if !tc.json && (strings.Contains(out, "--transport") || strings.Contains(out, "xlfp audit")) {
				t.Fatalf("HTTP setup advice in OpenCode report: %s", out)
			}
		})
	}
}

func TestOpenCodeCLIValidationAndHelp(t *testing.T) {
	exe, log := rootOpenCodeHelper(t, "")
	for _, extra := range [][]string{
		{"--model", "openai/test"}, {"--max-probes", "0"}, {"--timeout", "0s"}, {"--min-confidence", "NaN"},
		{"--endpoint", "https://opencode.ai"}, {"--max-requests", "1"}, {"unexpected"},
	} {
		args := append([]string{"opencode", "--opencode-bin", exe, "--model", "opencode/big-pickle"}, extra...)
		if code, _, _ := runCLI(t, args...); code != exitError {
			t.Errorf("accepted %v", extra)
		}
	}
	if code, _, err := runCLI(t, "opencode", "--help"); code != exitOK || !strings.Contains(err, "provider-request cap") {
		t.Fatal("OpenCode help missing")
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("invalid commands launched a probe")
	}
}

func TestOpenCodeBlockedInHTTPCommands(t *testing.T) {
	cleanEnv(t)
	var calls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer target.Close()
	for _, args := range [][]string{
		{"uncover", "--endpoint", "https://opencode.ai/zen/v1", "--model", "big-pickle"},
		{"audit", "--target-url", "https://API.OPENCODE.AI./inference/openai", "--model", "test", "--claim", "deepseek"},
		{"audit", "--target-url", target.URL, "--official-url", "https://opencode.ai", "--model", "test", "--claim", "deepseek"},
	} {
		if code, _, stderr := runCLI(t, args...); code != exitError || !strings.Contains(stderr, "xlfp opencode") {
			t.Errorf("guard failed: %v: %d, %s", args, code, stderr)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("preparation contacted target before rejecting official")
	}
}

func TestOpenCodeDashboardConfirmationAndHistory(t *testing.T) {
	exe, log := rootOpenCodeHelper(t, "")
	m := newTUI()
	defer m.shutdown()
	m.menu = 2
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != "opencode" {
		t.Fatal("home did not open OpenCode form")
	}
	setTUIField(t, m, "target_model", "opencode/big-pickle")
	setTUIField(t, m, "opencode_bin", exe)
	setTUIField(t, m, "max", "1")
	m.prepare()
	if m.screen != "review" || !strings.Contains(m.View(), "probe runs") || strings.Contains(m.View(), "without an API key") {
		t.Fatalf("review failed: %s %s", m.screen, m.message)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("prepared run sent probes")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	finishTUI(t, m)
	if len(m.history) != 1 || m.targetSpent != 1 || !json.Valid([]byte(m.current.json)) || strings.Contains(m.current.json, "requests_spent") || !strings.Contains(m.current.title, "opencode/big-pickle") {
		t.Fatalf("invalid result/history: %+v", m.current)
	}
	workspaces, _ := os.ReadFile(log)
	for _, dir := range strings.Fields(string(workspaces)) {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("workspace survived completion")
		}
	}
	// History retains all three renderings after the prepared runner is discarded.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.menu = 5
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.screen != "result" || !strings.Contains(m.current.plain, "Probe runs: 1") {
		t.Fatal("history unavailable")
	}
}

func TestOpenCodeCancelledEvidenceUsesProbeUnits(t *testing.T) {
	exe, _ := rootOpenCodeHelper(t, "")
	p, err := prepareOpenCode(exe, "opencode/big-pickle", commonFlags{maxRequests: 12, timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := p.execute(ctx, func(_ string, n int) {
		if n >= 3 {
			cancel()
		}
	})
	if !r.cancelled || r.targetSpent != 3 || r.uncover == nil || len(r.uncover.Observations) < 3 {
		t.Fatalf("partial lost: %+v", r)
	}
	record := makeTUIRecord(p, r)
	if !json.Valid([]byte(record.json)) || !strings.Contains(record.plain, "CANCELLED") || !strings.Contains(record.json, "probes_spent") || strings.Contains(record.json, "requests_spent") || strings.Contains(record.plain, "Target requests") {
		t.Fatalf("cancelled accounting: %s", record.plain)
	}
}

func TestOpenCodeWizardConfirmation(t *testing.T) {
	for _, answer := range []string{"n", "y"} {
		t.Run(answer, func(t *testing.T) {
			exe, log := rootOpenCodeHelper(t, "")
			var out, stderr strings.Builder
			input := strings.NewReader("3\nopencode/big-pickle\n" + exe + "\n" + answer + "\n")
			code := runWizard(input, &out, &stderr)
			want := exitOK
			if answer == "n" {
				want = exitError
			}
			if code != want {
				t.Fatalf("wizard failed: %d %s", code, stderr.String())
			}
			if !strings.Contains(out.String(), "OpenCode controls internal requests") {
				t.Fatal("missing request caveat")
			}
			_, err := os.Stat(log)
			if answer == "n" && !os.IsNotExist(err) {
				t.Fatal("declined wizard launched probes")
			}
			if answer == "y" && err != nil {
				t.Fatal("confirmed wizard did not execute")
			}
		})
	}
}
