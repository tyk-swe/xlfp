package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func clearKeyEnv(t *testing.T) {
	t.Helper()
	// Scrub every XLFP_* variable actually present (including purpose
	// variants resolved dynamically), plus provider variables. Resolve
	// treats empty as unset. Point config discovery at an empty dir.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "XLFP_") {
			t.Setenv(k, "")
		}
	}
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func writeConfig(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

const profileCfg = `{
  "endpoint": "https://default.example",
  "model": "default-model",
  "profiles": {
    "target1": {"endpoint": "https://provider.example", "model": "deepseek-chat",
                "transport": "openai", "api_key": "config-secret"}
  }
}`

func TestEnvWinsOverConfigKey(t *testing.T) {
	clearKeyEnv(t)
	cfg, err := Load(writeConfig(t, profileCfg, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XLFP_API_KEY", "env-secret")
	spec, err := cfg.Resolve(Overrides{Profile: "target1"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.APIKey != "env-secret" {
		t.Fatalf("key = %q, want env value to win over config", spec.APIKey)
	}
	if spec.Endpoint != "https://provider.example" || spec.Model != "deepseek-chat" {
		t.Fatalf("profile merge wrong: %+v", spec)
	}
}

func TestConfigKeyFallbackAndFlagOverride(t *testing.T) {
	clearKeyEnv(t)
	cfg, err := Load(writeConfig(t, profileCfg, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := cfg.Resolve(Overrides{Profile: "target1", Model: "flag-model"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.APIKey != "config-secret" || spec.Model != "flag-model" {
		t.Fatalf("endpoint=%q model=%q key set=%v", spec.Endpoint, spec.Model, spec.APIKey != "")
	}
	// Regression (security audit): retargeting a profile to another host
	// must not carry the profile's literal key along.
	if _, err := cfg.Resolve(Overrides{Profile: "target1", Endpoint: "https://flag.example"}); err == nil {
		t.Fatal("profile key retargeted to a substitute host without complaint")
	}
	// An explicitly named env key is the sanctioned way to retarget.
	t.Setenv("RETARGET_KEY", "retarget-secret")
	spec, err = cfg.Resolve(Overrides{Profile: "target1", Endpoint: "https://flag.example", APIKeyEnv: "RETARGET_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.APIKey != "retarget-secret" {
		t.Fatalf("key did not come from the explicitly named env var")
	}
}

func TestLooseConfigPermissionsRejected(t *testing.T) {
	clearKeyEnv(t)
	_, err := Load(writeConfig(t, profileCfg, 0o644))
	if runtime.GOOS == "windows" {
		// Windows reports 0666 for every writable file; there are no
		// group/world bits to check.
		if err != nil {
			t.Fatalf("config with keys rejected on windows: %v", err)
		}
	} else if err == nil {
		t.Fatal("world-readable config with literal keys must be rejected")
	}
	// Same permissions without any literal key are fine.
	noKeys := `{"profiles": {"p": {"endpoint": "https://x.example", "model": "m"}}}`
	if _, err := Load(writeConfig(t, noKeys, 0o644)); err != nil {
		t.Fatalf("keyless config rejected: %v", err)
	}
}

func TestPurposeEnvKeys(t *testing.T) {
	clearKeyEnv(t)
	cfg, err := Load(writeConfig(t, profileCfg, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XLFP_TARGET_API_KEY", "target-secret")
	t.Setenv("XLFP_OFFICIAL_API_KEY", "official-secret")
	ts, err := cfg.Resolve(Overrides{Profile: "target1", Purpose: "target"})
	if err != nil {
		t.Fatal(err)
	}
	off, err := cfg.Resolve(Overrides{Profile: "target1", Purpose: "official"})
	if err != nil {
		t.Fatal(err)
	}
	if ts.APIKey != "target-secret" || off.APIKey != "official-secret" {
		t.Fatalf("keys = %q / %q", ts.APIKey, off.APIKey)
	}
}

func TestNamedAPIKeyEnvMustBeSet(t *testing.T) {
	clearKeyEnv(t)
	cfg := &Config{}
	if _, err := cfg.Resolve(Overrides{Endpoint: "https://x.example", Model: "m", APIKeyEnv: "DEFINITELY_UNSET_XLFP_KEY"}); err == nil {
		t.Fatal("explicitly named but unset key env must error")
	}
	t.Setenv("MY_KEY", "from-my-key")
	spec, err := cfg.Resolve(Overrides{Endpoint: "https://x.example", Model: "m", APIKeyEnv: "MY_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.APIKey != "from-my-key" {
		t.Fatalf("key = %q", spec.APIKey)
	}
}

func TestDetectTransport(t *testing.T) {
	cases := map[string]string{
		"https://api.anthropic.com":                 TransportAnthropic,
		"https://generativelanguage.googleapis.com": TransportGemini,
		"https://api.deepseek.com":                  TransportOpenAI,
		"http://127.0.0.1:8080":                     TransportOpenAI,
		// Provider names in the path or a lookalike host must not select a
		// transport (and thereby a credential).
		"https://evil.example/anthropic/v1beta": TransportOpenAI,
		"https://anthropic.evil.example":        TransportOpenAI,
	}
	for in, want := range cases {
		if got := DetectTransport(in); got != want {
			t.Errorf("DetectTransport(%q) = %q, want %q", in, got, want)
		}
	}
}

// Regression (review): provider-standard ambient keys must never be sent
// to arbitrary untrusted endpoints; they apply to official hosts only.
func TestAmbientProviderKeyOnlyForOfficialHost(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("OPENAI_API_KEY", "official-openai-secret")
	cfg := &Config{}

	spec, err := cfg.Resolve(Overrides{Endpoint: "https://evil.example", Model: "stealth-x"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.APIKey != "" {
		t.Fatalf("OPENAI_API_KEY leaked to untrusted endpoint: key resolved to %q", spec.APIKey)
	}

	spec, err = cfg.Resolve(Overrides{Endpoint: "https://api.openai.com/v1", Model: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.APIKey != "official-openai-secret" {
		t.Fatalf("official host key = %q, want ambient key", spec.APIKey)
	}

	// Cleartext and subdomain lookalikes never qualify for ambient keys.
	for _, ep := range []string{"http://api.openai.com/v1", "https://evil.api.openai.com/v1"} {
		spec, err = cfg.Resolve(Overrides{Endpoint: ep, Model: "gpt-4o"})
		if err != nil {
			t.Fatal(err)
		}
		if spec.APIKey != "" {
			t.Fatalf("ambient key resolved for %q", ep)
		}
	}

	// The tool-specific key is an explicit opt-in and works anywhere.
	t.Setenv("XLFP_API_KEY", "probe-secret")
	spec, err = cfg.Resolve(Overrides{Endpoint: "https://evil.example", Model: "stealth-x"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.APIKey != "probe-secret" {
		t.Fatalf("XLFP_API_KEY opt-in key = %q", spec.APIKey)
	}
}
