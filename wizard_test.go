package main

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWizardRejectsCredentialURLsBeforeSuggestions(t *testing.T) {
	for name, input := range map[string]string{
		"target query":      "1\nhttps://provider.example/v1?key=DUMMY-SECRET\nmodel-x\nn\n",
		"target userinfo":   "1\nhttps://user:DUMMY-SECRET@provider.example\nmodel-x\nn\n",
		"official query":    "2\nhttps://provider.example\nmodel-x\nopenai\nhttps://official.example?api-key=DUMMY-SECRET\nn\n",
		"official userinfo": "2\nhttps://provider.example\nmodel-x\nopenai\nhttps://user:DUMMY-SECRET@official.example\nn\n",
	} {
		t.Run(name, func(t *testing.T) {
			cleanEnv(t)
			var out, errout bytes.Buffer
			code := runWizard(strings.NewReader(input), &out, &errout)
			if code != exitError || !strings.Contains(errout.String(), "invalid endpoint") {
				t.Fatalf("credential URL was not rejected: code=%d stderr=%s", code, &errout)
			}
			if strings.Contains(out.String()+errout.String(), "DUMMY-SECRET") {
				t.Fatal("wizard printed URL credentials")
			}
		})
	}
}

func TestWizardSuggestionOmitsFragment(t *testing.T) {
	cleanEnv(t)
	var out, errout bytes.Buffer
	code := runWizard(strings.NewReader("1\nhttps://provider.example/v1?api-version=1#DUMMY-SECRET\nmodel-x\nn\n"), &out, &errout)
	if code != exitError || !strings.Contains(out.String(), "xlfp uncover") || !strings.Contains(out.String(), "api-version=1") {
		t.Fatalf("missing suggestion or allowed query: code=%d stdout=%s stderr=%s", code, &out, &errout)
	}
	if strings.Contains(out.String()+errout.String(), "DUMMY-SECRET") {
		t.Fatal("wizard printed URL fragment")
	}
}

func TestEquivalentCommandPreservesArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell replay; Windows suggestions use PowerShell quoting")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	const endpoint = "https://provider.example/v1?one=1&two=2"
	const model = "model name's $(printf UNEXPECTED) `printf UNEXPECTED`"
	const official = "https://official.example/v1?api-version=1&region=us"
	for _, mode := range []string{"uncover", "audit"} {
		t.Run(mode, func(t *testing.T) {
			want := []string{mode, "--endpoint", endpoint, "--model", model}
			if mode == "audit" {
				want = []string{mode, "--target-url", endpoint, "--model", model, "--claim", "openai", "--official-url", official}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stub := "xlfp() { printf '%s\\000' \"$@\"; }\n"
			out, err := exec.CommandContext(ctx, "sh", "-c", stub+equivalentCommand(mode, endpoint, model, "openai", official)).CombinedOutput()
			if err != nil {
				t.Fatalf("suggestion failed: %v: %s", err, out)
			}
			got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
			if !slices.Equal(got, want) {
				t.Fatalf("arguments changed: got %q want %q", got, want)
			}
		})
	}
}
