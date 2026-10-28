package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/probe"
)

// The test executable doubles as OpenCode, without a shell or network access.
func TestMain(m *testing.M) {
	if os.Getenv("XLFP_OPENCODE_UNIT_HELPER") == "1" {
		fakeCLI()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeCLI() {
	mode := os.Getenv("XLFP_OPENCODE_TEST_MODE")
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		if mode == "bad_version" {
			fmt.Println("2.0.0 secret-diagnostics")
			return
		}
		fmt.Println("1.19.4")
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "debug" && os.Args[2] == "config" {
		switch mode {
		case "config_invalid":
			fmt.Println("managed-secret invalid configuration")
			return
		case "config_error":
			fmt.Fprintln(os.Stderr, "managed-secret unsupported command")
			os.Exit(1)
		case "config_huge":
			fmt.Print(strings.Repeat("x", 5<<20))
			return
		case "config_hang":
			time.Sleep(5 * time.Second)
			return
		}
		var config, overrides map[string]any
		_ = json.Unmarshal([]byte(os.Getenv("OPENCODE_CONFIG_CONTENT")), &config)
		_ = json.Unmarshal([]byte(os.Getenv("XLFP_OPENCODE_CONFIG_OVERRIDE")), &overrides)
		if path := os.Getenv("XLFP_OPENCODE_CONFIG_OVERRIDE_FILE"); path != "" {
			raw, _ := os.ReadFile(path)
			_ = json.Unmarshal(raw, &overrides)
		}
		// These fields are added by OpenCode v1's config loader/agent decoder.
		config["username"], config["mode"], config["plugin"] = "test-user", map[string]any{}, []any{}
		config["command"] = map[string]any{}
		mergeFakeConfig(config, overrides)
		_ = json.NewEncoder(os.Stdout).Encode(config)
		return
	}
	if mode == "descendant" {
		_ = os.WriteFile(os.Getenv("XLFP_OPENCODE_READY"), []byte("ready"), 0600)
		time.Sleep(500 * time.Millisecond)
		_ = os.WriteFile(os.Getenv("XLFP_OPENCODE_SURVIVED"), []byte("survived"), 0600)
		return
	}
	if mode == "tree" {
		exe, _ := os.Executable()
		child := exec.Command(exe)
		child.Env = childEnvironment(os.Environ(), map[string]string{"XLFP_OPENCODE_TEST_MODE": "descendant"})
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		time.Sleep(5 * time.Second)
		return
	}
	if mode == "hang" {
		time.Sleep(5 * time.Second)
		return
	}
	if mode == "error" {
		fmt.Fprintln(os.Stdout, completedStep("f1", `{"input":42}`))
		fmt.Fprintln(os.Stderr, "sk-test-secret raw diagnostic")
		os.Exit(1)
	}
	if mode == "huge" {
		fmt.Print(strings.Repeat("x", 5<<20))
		return
	}
	input, _ := io.ReadAll(os.Stdin)
	if capture := os.Getenv("XLFP_OPENCODE_CAPTURE"); capture != "" {
		wd, _ := os.Getwd()
		raw, _ := json.Marshal(map[string]any{
			"args": os.Args[1:], "input": string(input), "dir": wd, "pwd": os.Getenv("PWD"),
			"config_home": os.Getenv("XDG_CONFIG_HOME"), "data_home": os.Getenv("XDG_DATA_HOME"),
			"config_dir": os.Getenv("OPENCODE_CONFIG_DIR"), "custom_config": os.Getenv("OPENCODE_CONFIG"),
			"config": os.Getenv("OPENCODE_CONFIG_CONTENT"), "permission": os.Getenv("OPENCODE_PERMISSION"),
			"auth": os.Getenv("OPENCODE_API_KEY"), "disable_project": os.Getenv("OPENCODE_DISABLE_PROJECT_CONFIG"),
			"disable_plugins": os.Getenv("OPENCODE_DISABLE_DEFAULT_PLUGINS"),
			"test_home":       os.Getenv("OPENCODE_TEST_HOME"),
		})
		_ = os.WriteFile(capture, raw, 0600)
	}
	db, _ := fingerprint.Load()
	count := 42
	for _, entry := range db.Entries {
		if entry.ID != "deepseek-v3" {
			continue
		}
		for i, p := range db.Probes {
			if p.Text == string(input) {
				count = entry.Counts[i] + 100
			}
		}
	}
	fmt.Println(completedStep("f1", fmt.Sprintf(`{"input":%d,"cache":{"read":60,"write":20}}`, count-30)))
}

func mergeFakeConfig(dst, src map[string]any) {
	for key, value := range src {
		left, leftOK := dst[key].(map[string]any)
		right, rightOK := value.(map[string]any)
		if leftOK && rightOK {
			mergeFakeConfig(left, right)
		} else {
			dst[key] = value
		}
	}
}

func TestOpenCodeRejectsManagedConfiguration(t *testing.T) {
	for name, override := range map[string]string{
		"system_agent_prompt":       `{"agent":{"xlfp":{"prompt":"managed-secret"}}}`,
		"organization_instructions": `{"instructions":["managed-secret"]}`,
		"organization_mcp":          `{"mcp":{"managed":{"type":"local","command":["managed-secret"]}}}`,
		"agent_permissions":         `{"agent":{"xlfp":{"permission":{"bash":"allow"}}}}`,
		"global_permissions":        `{"permission":{"bash":"allow"}}`,
		"agent_steps":               `{"agent":{"xlfp":{"steps":5}}}`,
		"agent_model":               `{"agent":{"xlfp":{"model":"openai/managed-secret"}}}`,
		"agent_options":             `{"agent":{"xlfp":{"options":{"system":"managed-secret"}}}}`,
		"legacy_mode":               `{"mode":{"xlfp":{"prompt":"managed-secret"}}}`,
		"plugin":                    `{"plugin":["managed-secret"]}`,
		"provider":                  `{"provider":{"opencode":{"options":{"apiKey":"managed-secret"}}}}`,
		"configured_command":        `{"command":{"managed":{"template":"managed-secret"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XLFP_OPENCODE_CONFIG_OVERRIDE", override)
			c := newTestClient(t, "")
			err := c.Init(context.Background())
			if err == nil || !strings.Contains(err.Error(), "isolated") || strings.Contains(err.Error(), "managed-secret") {
				t.Fatalf("managed settings were accepted or exposed: %v", err)
			}
			if _, err := c.SendProbe(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"}, 1); err == nil || c.Attempts() != 0 {
				t.Fatal("runner accepted a probe after failed configuration validation")
			}
		})
	}
}

func TestOpenCodeRechecksConfigurationBeforeEachProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XLFP_OPENCODE_CONFIG_OVERRIDE_FILE", path)
	c := newTestClient(t, "")
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := fingerprint.Probe{ID: "p", Text: "hello"}
	if _, err := c.SendProbe(context.Background(), p, 2); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"instructions":["managed-secret"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendProbe(context.Background(), p, 1); err == nil || c.Attempts() != 1 {
		t.Fatal("changed configuration was not rejected before the next probe")
	}
}

func TestOpenCodeConfigurationInspectionFailsClosed(t *testing.T) {
	for _, mode := range []string{"config_invalid", "config_error", "config_huge", "config_hang"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestClient(t, mode)
			if mode == "config_hang" {
				c.timeout = 100 * time.Millisecond
			}
			err := c.Init(context.Background())
			if err == nil || strings.Contains(err.Error(), "managed-secret") || c.Attempts() != 0 {
				t.Fatalf("inspection failure was accepted or exposed: %v", err)
			}
			if _, err := c.SendProbe(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"}, 1); err == nil || c.Attempts() != 0 {
				t.Fatal("probe started without verified configuration")
			}
		})
	}
}

func newTestClient(t *testing.T, mode string) *Client {
	t.Helper()
	t.Setenv("XLFP_OPENCODE_UNIT_HELPER", "1")
	t.Setenv("XLFP_OPENCODE_TEST_MODE", mode)
	// Keep race detection enabled, but skip its exit sleep in CLI children.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(exe, "opencode/big-pickle", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func TestOpenCodeInvocationIsolationAndCleanup(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("XLFP_OPENCODE_CAPTURE", capture)
	t.Setenv("OPENCODE_CONFIG", "/unrelated/user/config.json")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"plugin":["unrelated"]}`)
	t.Setenv("OPENCODE_PERMISSION", `{"*":"allow"}`)
	t.Setenv("OPENCODE_API_KEY", "test-auth-sentinel")
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	c := newTestClient(t, "")
	if _, err := os.Stat(capture); !os.IsNotExist(err) || c.Attempts() != 0 || c.dir != "" {
		t.Fatal("construction started a process")
	}
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	workspace := c.dir
	input := "  a \"quote\"\t世界\n`literal` $(literal)  \n"
	o, err := c.SendProbe(context.Background(), fingerprint.Probe{ID: "p", Text: input}, 1)
	if err != nil || !o.HasUsage || o.PromptTokens != 92 || c.Attempts() != 1 {
		t.Fatalf("observation %+v, error %v, attempts %d", o, err, c.Attempts())
	}
	var got struct {
		TestHome                                  string `json:"test_home"`
		Args                                      []string
		Input, Dir, Pwd, Config, Permission, Auth string
		ConfigHome                                string `json:"config_home"`
		ConfigDir                                 string `json:"config_dir"`
		DataHome                                  string `json:"data_home"`
		CustomConfig                              string `json:"custom_config"`
		DisableProject                            string `json:"disable_project"`
		DisablePlugins                            string `json:"disable_plugins"`
	}
	raw, err := os.ReadFile(capture)
	if err != nil || json.Unmarshal(raw, &got) != nil {
		t.Fatal("capture unavailable")
	}
	wantArgs := []string{"run", "--format", "json", "--model", "opencode/big-pickle", "--agent", "xlfp", "--title", "xlfp tokenizer probe", "--dir", workspace}
	if !reflect.DeepEqual(got.Args, wantArgs) || got.Input != input || got.Dir != workspace || got.Pwd != workspace {
		t.Fatalf("prompt or invocation changed: %+v", got)
	}
	if got.TestHome != workspace || got.ConfigHome != filepath.Join(workspace, "config") || got.ConfigDir != filepath.Join(workspace, "config", "opencode") || got.CustomConfig != "" || got.DataHome != dataHome || got.Auth != "test-auth-sentinel" || got.DisableProject != "1" || got.DisablePlugins != "1" || got.Permission != `{"*":"deny"}` {
		t.Fatal("child configuration or existing auth was not isolated correctly")
	}
	var config struct {
		EnabledProviders []string          `json:"enabled_providers"`
		Permission       map[string]string `json:"permission"`
		Agent            map[string]struct {
			Steps      int
			Prompt     string
			Permission map[string]string
		}
	}
	if json.Unmarshal([]byte(got.Config), &config) != nil || !reflect.DeepEqual(config.EnabledProviders, []string{"opencode"}) || config.Permission["*"] != "deny" || config.Agent["xlfp"].Steps != 1 || config.Agent["xlfp"].Prompt == "" || config.Agent["xlfp"].Permission["*"] != "deny" {
		t.Fatal("probe agent is not restricted")
	}
	if os.Getenv("OPENCODE_CONFIG") != "/unrelated/user/config.json" || strings.Contains(fmt.Sprintf("%#v", c), "test-auth-sentinel") {
		t.Fatal("parent environment changed or client formatting leaked auth")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("workspace was not removed")
	}
}

func TestOpenCodeAdaptiveStoppingAndBudget(t *testing.T) {
	for _, max := range []int{1, 12} {
		t.Run(fmt.Sprint(max), func(t *testing.T) {
			c := newTestClient(t, "")
			if err := c.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			db, _ := fingerprint.Load()
			last := 0
			r, err := probe.FingerprintWithBackend(context.Background(), c, db, probe.Options{MaxRequests: max, OnProbe: func(n int) { last = n }})
			if err != nil || c.Attempts() > max || r.RequestsSpent != c.Attempts() || last != c.Attempts() {
				t.Fatalf("budget/progress: attempts %d, result %+v, err %v", c.Attempts(), r, err)
			}
			if max == 1 && r.Status != fingerprint.StatusInconclusive {
				t.Fatal("one probe produced a verdict")
			}
			if max == 12 && (r.TopFamily != "deepseek" || r.Status != fingerprint.StatusIdentified || c.Attempts() >= max) {
				t.Fatalf("adaptive identification failed: %+v", r)
			}
		})
	}
}

func TestOpenCodeFailuresAreSafeAndBounded(t *testing.T) {
	for _, mode := range []string{"error", "huge", "hang", "bad_version"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestClient(t, mode)
			if mode == "hang" {
				c.timeout = 100 * time.Millisecond
			}
			err := c.Init(context.Background())
			if mode != "bad_version" && err != nil {
				t.Fatal(err)
			}
			if err == nil {
				_, err = c.SendProbe(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"}, 1)
			}
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "raw diagnostic") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
			if mode == "huge" && !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("output limit was bypassed: %v", err)
			}
			if mode != "bad_version" && c.Attempts() != 1 {
				t.Fatalf("unexpected retries: %d", c.Attempts())
			}
		})
	}
}

func TestOpenCodeCancellationStopsSubprocessTree(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	survived := filepath.Join(t.TempDir(), "survived")
	t.Setenv("XLFP_OPENCODE_READY", ready)
	t.Setenv("XLFP_OPENCODE_SURVIVED", survived)
	c := newTestClient(t, "tree")
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.SendProbe(ctx, fingerprint.Probe{ID: "p", Text: "hello"}, 1)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled child was not reaped")
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(survived); !os.IsNotExist(err) {
		t.Fatal("descendant survived cancellation")
	}
	before := c.Attempts()
	if _, err := c.SendProbe(ctx, fingerprint.Probe{}, 1); err != context.Canceled || c.Attempts() != before {
		t.Fatal("cancelled probe started another process")
	}
}

func TestOpenCodeValidation(t *testing.T) {
	exe, _ := os.Executable()
	for _, model := range []string{"", "big-pickle", "openai/gpt", "opencode/x --auto", "opencode/x\nsecret", "opencode/../x"} {
		if _, err := New(exe, model, time.Second); err == nil {
			t.Errorf("accepted %q", model)
		}
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing"), "opencode/big-pickle", time.Second); err == nil {
		t.Fatal("missing executable accepted")
	}
}
