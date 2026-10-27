package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadRejectsTrailingJSON(t *testing.T) {
	for _, raw := range []string{`{} {"api_key":"hidden"}`, `{} garbage`, `null`} {
		if _, err := Load(writeConfig(t, raw, 0o600)); err == nil {
			t.Errorf("accepted invalid config %s", raw)
		}
	}
	if _, err := Load(writeConfig(t, "{} \n\t", 0o600)); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultNamedKeyFailsClosed(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("MISSING_XLFP_KEY", "")
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	cfg := &Config{Profile: Profile{Endpoint: "https://api.openai.com", Model: "m", APIKeyEnv: "MISSING_XLFP_KEY", APIKey: "fallback-secret"}}
	if _, err := cfg.Resolve(Overrides{}); err == nil {
		t.Fatal("missing configured key silently fell back to a different credential")
	}
	t.Setenv("MISSING_XLFP_KEY", "  \t")
	if _, err := cfg.Resolve(Overrides{}); err == nil {
		t.Fatal("whitespace-only configured key was accepted")
	}
	if _, err := cfg.Resolve(Overrides{APIKeyEnv: "MISSING_XLFP_KEY"}); err == nil {
		t.Fatal("whitespace-only explicit key was accepted")
	}
}

func TestConfigCredentialsStayOnOrigin(t *testing.T) {
	clearKeyEnv(t)
	cfg := &Config{Profile: Profile{Endpoint: "https://localhost:443/v1", Model: "m", APIKey: "secret"}}
	for _, endpoint := range []string{"https://localhost:8443/v1", "http://localhost/v1"} {
		if _, err := cfg.Resolve(Overrides{Endpoint: endpoint}); err == nil {
			t.Errorf("credential escaped source origin to %s", endpoint)
		}
	}
	for _, endpoint := range []string{"https://localhost/v2", "https://LOCALHOST.:443/v2"} {
		if _, err := cfg.Resolve(Overrides{Endpoint: endpoint}); err != nil {
			t.Errorf("same origin rejected: %v", err)
		}
	}
}

func TestAmbientKeysRequireOfficialPort(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	for _, endpoint := range []string{"https://api.openai.com:8443/v1", "https://api.openai.com:0/v1"} {
		spec, err := (&Config{}).Resolve(Overrides{Endpoint: endpoint, Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		if spec.APIKey != "" {
			t.Errorf("ambient credential selected for %s", endpoint)
		}
	}
	if !IsOfficialHost("https://api.openai.com:443/v1", TransportOpenAI) {
		t.Fatal("explicit default port rejected")
	}
}

func TestEndpointSpecGoSyntaxDoesNotLeakKey(t *testing.T) {
	spec := EndpointSpec{Endpoint: "https://example.test", Model: "m", APIKey: "DO-NOT-PRINT-THIS"}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(verb, spec), spec.APIKey) {
			t.Errorf("%s leaks the credential", verb)
		}
	}
}
