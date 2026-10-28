package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func setTUIField(t *testing.T, m *tuiModel, name, value string) {
	t.Helper()
	for i := range m.fields {
		if m.fields[i].name == name {
			m.fields[i].input.SetValue(value)
			return
		}
	}
	t.Fatalf("field %q not found", name)
}

func finishTUI(t *testing.T, m *tuiModel) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for m.screen == "running" {
		select {
		case msg := <-m.events:
			m.Update(msg)
		case <-timer.C:
			t.Fatal("dashboard run did not finish")
		}
	}
}

func TestTUIConfirmationProgressAndHistory(t *testing.T) {
	cleanEnv(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer srv.Close()
	m := newTUI()
	defer m.shutdown()
	m.newForm("uncover")
	setTUIField(t, m, "target_url", srv.URL)
	setTUIField(t, m, "target_model", "test-model")
	setTUIField(t, m, "max", "1")
	m.prepare()
	if m.screen != "review" || calls.Load() != 0 {
		t.Fatalf("preflight: screen %s, requests %d, error %s", m.screen, calls.Load(), m.message)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	finishTUI(t, m)
	if calls.Load() != 1 || m.screen != "result" || len(m.history) != 1 {
		t.Fatalf("run: requests %d, screen %s, history %d", calls.Load(), m.screen, len(m.history))
	}
	if !strings.Contains(m.current.plain, "failed") && !strings.Contains(m.current.title, "failed") {
		t.Fatalf("failure not reported: %#v", m.current)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.menu = 4
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.screen != "result" || !json.Valid([]byte(m.current.json)) {
		t.Fatal("session result or JSON view unavailable")
	}
}

func TestTUIOfficialOptOutAndUnverifiedClaim(t *testing.T) {
	cleanEnv(t)
	var officialCalls atomic.Int64
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { officialCalls.Add(1); http.Error(w, "unexpected", 401) }))
	defer official.Close()
	t.Setenv("XLFP_OFFICIAL_ENDPOINT", official.URL)
	target := openAIStyleMock(t, "deepseek-v3", 6)
	m := newTUI()
	defer m.shutdown()
	m.newForm("audit")
	setTUIField(t, m, "target_url", target.URL)
	setTUIField(t, m, "both_model", "deepseek-chat")
	setTUIField(t, m, "max", "1")
	m.prepare()
	if m.screen != "review" || m.prepared.official != nil {
		t.Fatalf("baseline enabled unexpectedly: %s %s", m.screen, m.message)
	}
	result := m.prepared.execute(context.Background(), nil)
	if result.err != nil || officialCalls.Load() != 0 {
		t.Fatalf("official baseline contacted: %d, %v", officialCalls.Load(), result.err)
	}
	m.newForm("audit")
	setTUIField(t, m, "target_url", target.URL)
	setTUIField(t, m, "both_model", "gemini")
	setTUIField(t, m, "claim", "google")
	m.prepare()
	if m.screen != "review" {
		t.Fatal(m.message)
	}
	result = m.prepared.execute(context.Background(), nil)
	if result.err != nil || result.audit == nil || result.targetSpent != 0 || officialCalls.Load() != 0 {
		t.Fatalf("unverified claim spent requests: %+v", result)
	}
}

func TestTUICancellationRetainsEvidenceAndSkipsOfficial(t *testing.T) {
	cleanEnv(t)
	target := openAIStyleMock(t, "deepseek-v3", 6)
	var officialCalls atomic.Int64
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { officialCalls.Add(1); http.Error(w, "unexpected", 401) }))
	defer official.Close()
	p, err := prepareCheck("audit", endpointFlags{endpoint: target.URL, model: "deepseek-chat"}, endpointFlags{endpoint: official.URL, model: "deepseek-chat"}, "deepseek", true, commonFlags{maxRequests: 12, timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := p.execute(ctx, func(side string, n int) {
		if side == "target" && n >= 3 {
			cancel()
		}
	})
	if !r.cancelled || r.targetSpent < 3 || r.audit == nil || len(r.audit.Target.Observations) < 3 || officialCalls.Load() != 0 {
		t.Fatalf("cancellation lost evidence or contacted official: %+v, calls %d", r, officialCalls.Load())
	}
	record := makeTUIRecord(p, r)
	if !strings.Contains(record.plain, "CANCELLED") || strings.Contains(record.plain, "Verdict: MATCH") || !json.Valid([]byte(record.json)) {
		t.Fatalf("cancelled result misleading: %s", record.plain)
	}
}

func TestTUIRedactsCredentialsAndTerminalControls(t *testing.T) {
	cleanEnv(t)
	m := newTUI()
	defer m.shutdown()
	m.newForm("uncover")
	setTUIField(t, m, "target_url", "https://user:secret123@example.com/path?api_key=secret123")
	setTUIField(t, m, "target_model", "test")
	m.prepare()
	if m.screen != "form" || strings.Contains(m.View(), "secret123") || strings.Contains(m.message, "secret123") {
		t.Fatalf("credential URL leaked: %s", m.View())
	}
	secret := `abc"secret`
	t.Setenv("XLFP_API_KEY", secret)
	p, err := prepareCheck("uncover", endpointFlags{endpoint: "http://localhost:8000", model: "test-" + secret + "\x1b[31m"}, endpointFlags{}, "", false, commonFlags{maxRequests: 1, timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r := fingerprint.Result{Status: fingerprint.StatusInconclusive}
	record := makeTUIRecord(p, checkResult{uncover: &r})
	for _, s := range []string{record.title, record.plain, record.verbose, record.json} {
		if strings.Contains(s, secret) || strings.ContainsRune(s, '\x1b') {
			t.Fatalf("secret or terminal control leaked: %q", s)
		}
	}
	if !json.Valid([]byte(record.json)) {
		t.Fatalf("redacted JSON is invalid: %s", record.json)
	}
}

func TestTUIRouting(t *testing.T) {
	cleanEnv(t)
	stubStdin(t, false, "")
	for _, cmd := range []string{"tui", "wizard"} {
		if code, _, _ := runCLI(t, cmd); code != exitError {
			t.Errorf("%s without terminal: %d", cmd, code)
		}
		if code, out, _ := runCLI(t, cmd, "--help"); code != exitOK || !strings.Contains(out, "Usage:") {
			t.Errorf("%s help: %d %s", cmd, code, out)
		}
	}
	stubStdin(t, true, "q\n")
	if code, out, _ := runCLI(t, "tui"); code != exitError || !strings.Contains(out, "What do you want to do?") {
		t.Errorf("terminal fallback: %d %s", code, out)
	}
}

func TestTUICancelDuringOfficialProbe(t *testing.T) {
	cleanEnv(t)
	target := openAIStyleMock(t, "deepseek-v3", 6)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer official.Close()
	defer close(release)
	m := newTUI()
	defer m.shutdown()
	m.newForm("audit")
	setTUIField(t, m, "target_url", target.URL)
	setTUIField(t, m, "both_model", "deepseek-chat")
	setTUIField(t, m, "official_url", official.URL)
	setTUIField(t, m, "official_enabled", "yes")
	m.prepare()
	if m.screen != "review" {
		t.Fatal(m.message)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("official probe did not start")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	finishTUI(t, m)
	if !strings.Contains(m.current.plain, "CANCELLED") || !strings.Contains(m.current.plain, "target_evidence") || !strings.Contains(m.current.plain, "Official requests: 1") {
		t.Fatalf("missing cancelled official evidence: %s", m.current.plain)
	}
}

func TestTUIReviewAndOfficialPreflight(t *testing.T) {
	cleanEnv(t)
	var calls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	m := newTUI()
	defer m.shutdown()
	m.newForm("audit")
	setTUIField(t, m, "target_url", target.URL)
	setTUIField(t, m, "both_model", "deepseek-chat")
	setTUIField(t, m, "official_url", "https://user:secret999@example.com")
	setTUIField(t, m, "official_enabled", "yes")
	m.prepare()
	if m.screen != "form" || calls.Load() != 0 || strings.Contains(m.View(), "secret999") {
		t.Fatalf("official validation: screen %s, calls %d: %s", m.screen, calls.Load(), m.View())
	}
	setTUIField(t, m, "official_url", "https://example.com")
	m.height = 10
	m.prepare()
	view := m.View()
	if m.screen != "review" || !strings.Contains(view, "WARNING") || !strings.Contains(view, "Enter confirm") || calls.Load() != 0 {
		t.Fatalf("small-screen review: %s", view)
	}
}

func TestTUIResizePreventsBlindConfirmation(t *testing.T) {
	cleanEnv(t)
	m := newTUI()
	defer m.shutdown()
	m.newForm("uncover")
	setTUIField(t, m, "target_url", "http://localhost:8080")
	setTUIField(t, m, "target_model", "test")
	m.prepare()
	if m.screen != "review" {
		t.Fatal(m.message)
	}
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.screen != "review" || m.started {
		t.Fatal("confirmed a check without a visible review")
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	if !strings.Contains(m.View(), "Enter confirm") {
		t.Fatalf("review not usable after resize: %s", m.View())
	}
}

func TestTUINonInteractiveDevNull(t *testing.T) {
	cleanEnv(t)
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = null
	t.Cleanup(func() { os.Stdin = old; null.Close() })
	code, out, stderr := runCLI(t)
	if code != exitError || out != "" || !strings.Contains(stderr, "Usage:") {
		t.Fatalf("nonterminal input launched an interactive mode: code %d, stdout %q, stderr %q", code, out, stderr)
	}
}

func TestTUICancelWithFewerThanThreeObservations(t *testing.T) {
	cleanEnv(t)
	target := openAIStyleMock(t, "deepseek-v3", 6)
	p, err := prepareCheck("uncover", endpointFlags{endpoint: target.URL, model: "deepseek-chat"}, endpointFlags{}, "", false, commonFlags{maxRequests: 12, timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := p.execute(ctx, func(_ string, n int) {
		if n >= 1 {
			cancel()
		}
	})
	if !r.cancelled || r.uncover == nil || r.targetSpent != 1 || len(r.uncover.Observations) < 1 {
		t.Fatalf("lost early evidence: %+v", r)
	}
	record := makeTUIRecord(p, r)
	if !strings.Contains(record.plain, "CANCELLED") || !strings.Contains(record.json, "observations") {
		t.Fatalf("early evidence missing: %s", record.json)
	}
}

func TestTUIPasteFromClipboard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake clipboard command is available on Unix systems")
	}
	if os.Getenv("XLFP_TEST_FAKE_CLIPBOARD") == "1" {
		m := newTUI()
		defer m.shutdown()
		m.newForm("uncover")
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
		if cmd == nil {
			t.Fatal("Ctrl+V did not request clipboard contents")
		}
		m.Update(cmd())
		if got := m.fields[0].input.Value(); got != "https://example.test/v1" {
			t.Fatalf("pasted value = %q", got)
		}
		m.changeFocus(1)
		_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
		if cmd == nil {
			t.Fatal("Ctrl+V did not request clipboard contents in the second field")
		}
		m.Update(cmd())
		if got := m.fields[1].input.Value(); got != "https://example.test/v1" {
			t.Fatalf("pasted value in focused field = %q", got)
		}
		return
	}

	// Clipboard backends select their command during package initialization,
	// so launch a fresh test process with a fake clipboard on its PATH.
	dir := t.TempDir()
	for _, name := range []string{"xclip", "pbpaste"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf '%s' 'https://example.test/v1'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTUIPasteFromClipboard$")
	cmd.Env = append(os.Environ(), "PATH="+dir, "WAYLAND_DISPLAY=", "XLFP_TEST_FAKE_CLIPBOARD=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clipboard test failed: %v\n%s", err, output)
	}
}
