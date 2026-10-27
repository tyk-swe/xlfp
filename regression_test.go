package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWizardBlankOfficialDoesNotProbeEnvironmentEndpoint(t *testing.T) {
	cleanEnv(t)
	target := openAIStyleMock(t, "deepseek-v3", 6)
	var calls atomic.Int64
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 401) }))
	defer official.Close()
	t.Setenv("XLFP_OFFICIAL_ENDPOINT", official.URL)
	input := "2\n" + target.URL + "\ndeepseek-chat\ndeepseek\n\ny\n"
	var out, errout bytes.Buffer
	code := runWizard(strings.NewReader(input), &out, &errout)
	if calls.Load() != 0 || code != exitOK {
		t.Fatalf("blank baseline choice was ignored: calls=%d code=%d stderr=%s", calls.Load(), code, &errout)
	}
}

func TestSkillInstallerFailsOnPartialRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer")
	}
	cleanEnv(t)
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".codex", "skills", "xlfp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("user version"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "scripts/install-skill.sh", "--target", "hermes,codex").CombinedOutput()
	if err == nil {
		t.Fatalf("partial refusal returned success: %s", out)
	}
}

func TestSkillInstallerValidatesBeforeWriting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX installer")
	}
	for _, args := range [][]string{{"--target", "codex,typo"}, {"--target", ""}, {"--dest", ""}, {"--target"}, {"--dest", "--force"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cleanEnv(t)
			out, err := exec.Command("sh", append([]string{"scripts/install-skill.sh"}, args...)...).CombinedOutput()
			if err == nil {
				t.Errorf("invalid arguments succeeded: %s", out)
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".codex")); !os.IsNotExist(err) {
				t.Error("invalid arguments modified a harness")
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("output closed") }

func TestPlainOutputWriteFailureIsNotSuccess(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"families"}, {"explain"}, {"--help"}} {
		var stderr bytes.Buffer
		if code := run(args, brokenWriter{}, &stderr); code != exitError {
			t.Errorf("%v silently ignored write failure: %d", args, code)
		}
	}
}

func TestFailedOfficialIsIncludedInJSON(t *testing.T) {
	cleanEnv(t)
	target := openAIStyleMock(t, "deepseek-v3", 6)
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "denied", 401) }))
	defer official.Close()
	code, out, errout := runCLI(t, "audit", "--target-url", target.URL, "--official-url", official.URL, "--model", "deepseek-chat", "--json")
	if code != exitInconclusive || !strings.Contains(out, `"official_endpoint": "`+official.URL+`"`) {
		t.Fatalf("failed baseline identity missing: code=%d out=%s err=%s", code, out, errout)
	}
}
