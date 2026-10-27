// Package config resolves endpoints, models, transports and API keys from
// flags, environment variables and a JSON config file.
// API keys are accepted from the environment or a config file only; there
// is deliberately no literal --api-key flag value.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Profile is one named endpoint configuration.
type Profile struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Model     string `json:"model,omitempty"`
	Transport string `json:"transport,omitempty"` // openai | anthropic | gemini
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// Config is the on-disk JSON config. Top-level fields act as defaults for
// every profile.
type Config struct {
	Profile                     // defaults (embedded fields marshal at top level except api keys stay explicit)
	Profiles map[string]Profile `json:"profiles,omitempty"`
	// path the config was loaded from, "" when no file was used
	path string
}

// Transports.
const (
	TransportOpenAI    = "openai"
	TransportAnthropic = "anthropic"
	TransportGemini    = "gemini"
)

// DefaultPath returns the config path that would be used without a flag:
// $XLFP_CONFIG, else <UserConfigDir>/xlfp/config.json. A config in the
// current directory is never auto-loaded (an untrusted checkout must not
// silently redirect endpoints or keys); pass it explicitly with --config.
// The second result is false when no file exists.
func DefaultPath() (string, bool) {
	if p := os.Getenv("XLFP_CONFIG"); p != "" {
		return p, fileExists(p)
	}
	if dir, err := os.UserConfigDir(); err == nil {
		if p := filepath.Join(dir, "xlfp", "config.json"); fileExists(p) {
			return p, true
		}
	}
	return "", false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Load reads the config at path. An empty path means "use DefaultPath if a
// file exists, else an empty config". An explicit missing file is an error.
// A config file that stores literal API keys must not be group/world
// readable (mode & 0o077 must be zero).
func Load(path string) (*Config, error) {
	if path == "" {
		p, ok := DefaultPath()
		if !ok {
			if os.Getenv("XLFP_CONFIG") != "" {
				return nil, fmt.Errorf("config %s (from XLFP_CONFIG) does not exist", os.Getenv("XLFP_CONFIG"))
			}
			return &Config{}, nil
		}
		path = p
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.path = path
	if cfg.hasLiteralKeys() && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("config %s stores API keys but is group/world accessible (mode %04o); run: chmod 600 %s", path, info.Mode().Perm(), path)
	}
	return &cfg, nil
}

func (c *Config) hasLiteralKeys() bool {
	if c.APIKey != "" {
		return true
	}
	for _, p := range c.Profiles {
		if p.APIKey != "" {
			return true
		}
	}
	return false
}

// Path returns the file the config was loaded from, if any.
func (c *Config) Path() string { return c.path }

// profile returns the named profile merged over the top-level defaults.
func (c *Config) mergedProfile(name string) (Profile, error) {
	out := c.Profile
	if name == "" {
		return out, nil
	}
	p, ok := c.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("config: profile %q not found", name)
	}
	if p.Endpoint != "" {
		out.Endpoint = p.Endpoint
	}
	if p.Model != "" {
		out.Model = p.Model
	}
	if p.Transport != "" {
		out.Transport = p.Transport
	}
	if p.APIKey != "" {
		out.APIKey = p.APIKey
	}
	if p.APIKeyEnv != "" {
		out.APIKeyEnv = p.APIKeyEnv
	}
	return out, nil
}

// Overrides holds explicit flag values; empty fields fall through to env
// and config.
type Overrides struct {
	Endpoint  string
	Model     string
	Transport string
	// APIKeyEnv names the environment variable holding the key. It is a
	// name, never the key itself.
	APIKeyEnv string
	Profile   string
	// Purpose distinguishes audit sides for env lookups: "", "target",
	// or "official" (enables XLFP_TARGET_API_KEY / XLFP_OFFICIAL_API_KEY).
	Purpose string
}

// EndpointSpec is a fully resolved endpoint ready for probing.
type EndpointSpec struct {
	Endpoint  string
	Model     string
	Transport string
	APIKey    string `json:"-"` // resolved secret; never log, print, or marshal this
}

// String renders the spec without the secret, so accidental %v/%+v
// formatting in logs or errors cannot leak the key.
func (s EndpointSpec) String() string {
	key := ""
	if s.APIKey != "" {
		key = " key=[redacted]"
	}
	return fmt.Sprintf("{endpoint:%s model:%s transport:%s%s}", s.Endpoint, s.Model, s.Transport, key)
}

// Resolve merges flags > environment > config profile > config defaults.
// For the API key the environment wins over the config file. An
// empty resolved key is allowed (keyless local endpoints).
func (c *Config) Resolve(ov Overrides) (EndpointSpec, error) {
	prof, err := c.mergedProfile(ov.Profile)
	if err != nil {
		return EndpointSpec{}, err
	}
	purp := strings.ToUpper(ov.Purpose)
	envFor := func(base string) string {
		if purp != "" {
			if v := os.Getenv("XLFP_" + purp + "_" + base); v != "" {
				return v
			}
		}
		return os.Getenv("XLFP_" + base)
	}
	spec := EndpointSpec{
		Endpoint:  strings.TrimSpace(firstNonEmpty(ov.Endpoint, envFor("ENDPOINT"), prof.Endpoint)),
		Model:     strings.TrimSpace(firstNonEmpty(ov.Model, envFor("MODEL"), prof.Model)),
		Transport: NormalizeTransport(firstNonEmpty(ov.Transport, envFor("TRANSPORT"), prof.Transport)),
	}
	if spec.Endpoint == "" {
		return EndpointSpec{}, fmt.Errorf("no endpoint: pass --endpoint, set XLFP_ENDPOINT, or configure a profile")
	}
	if spec.Model == "" {
		return EndpointSpec{}, fmt.Errorf("no model: pass --model, set XLFP_MODEL, or configure a profile")
	}
	if spec.Transport == "" {
		spec.Transport = DetectTransport(spec.Endpoint)
	}

	key, fromProfile, err := c.resolveKey(ov, prof, spec.Endpoint, spec.Transport, envFor)
	if err != nil {
		return EndpointSpec{}, err
	}
	// A key stored in / named by a profile is bound to that profile's
	// endpoint host: silently retargeting the endpoint must not ship the
	// profile's credential to a substitute host.
	if fromProfile && ov.Profile != "" {
		if raw, ok := c.Profiles[ov.Profile]; ok && raw.Endpoint != "" {
			if hostname(raw.Endpoint) != hostname(spec.Endpoint) {
				return EndpointSpec{}, fmt.Errorf("profile %q key is bound to endpoint host %q; refusing to send it to %q (use --api-key-env or a matching profile)", ov.Profile, hostname(raw.Endpoint), hostname(spec.Endpoint))
			}
		}
	}
	spec.APIKey = strings.TrimSpace(key)
	return spec, nil
}

func hostname(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// resolveKey returns the API key and whether it came from the selected
// profile (literal or profile-named env var), for host binding.
func (c *Config) resolveKey(ov Overrides, prof Profile, endpoint, transport string, envFor func(string) string) (string, bool, error) {
	// Explicitly named environment variable (flag): must be set.
	if ov.APIKeyEnv != "" {
		v := os.Getenv(ov.APIKeyEnv)
		if v == "" {
			return "", false, fmt.Errorf("environment variable %s named by --api-key-env is empty or unset", ov.APIKeyEnv)
		}
		return v, false, nil
	}
	// Environment wins over config: purpose env, profile-named env,
	// transport-standard env, then generic XLFP_API_KEY.
	if v := envFor("API_KEY"); v != "" {
		return v, false, nil
	}
	if prof.APIKeyEnv != "" {
		if v := os.Getenv(prof.APIKeyEnv); v != "" {
			return v, true, nil
		}
		if ov.Profile != "" {
			return "", false, fmt.Errorf("environment variable %s named by profile %q (api_key_env) is empty or unset", prof.APIKeyEnv, ov.Profile)
		}
	}
	// Provider-standard ambient keys (OPENAI_API_KEY etc.) are credentials
	// for the official service, and xlfp exists to probe untrusted
	// endpoints: they are only ever sent to that provider's official host.
	// For any other endpoint the user must opt in explicitly via
	// XLFP_API_KEY, --api-key-env, or a config profile.
	if IsOfficialHost(endpoint, transport) {
		for _, name := range DefaultKeyEnvVars(transport) {
			if v := os.Getenv(name); v != "" {
				return v, false, nil
			}
		}
	}
	return prof.APIKey, prof.APIKey != "", nil
}

// DefaultKeyEnvVars returns the conventional key variables per transport.
func DefaultKeyEnvVars(transport string) []string {
	switch transport {
	case TransportAnthropic:
		return []string{"ANTHROPIC_API_KEY"}
	case TransportGemini:
		return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
	default:
		return []string{"OPENAI_API_KEY"}
	}
}

// NormalizeTransport canonicalizes a transport name; "" stays "".
func NormalizeTransport(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "auto":
		return ""
	case "openai", "openai-compatible", "openai-compat", "compat":
		return TransportOpenAI
	case "anthropic", "claude":
		return TransportAnthropic
	case "gemini", "google":
		return TransportGemini
	default:
		return strings.ToLower(strings.TrimSpace(t))
	}
}

// officialDomains maps a transport to the official API domains whose
// ambient provider keys may be sent there.
var officialDomains = map[string][]string{
	TransportOpenAI:    {"api.openai.com"},
	TransportAnthropic: {"api.anthropic.com"},
	TransportGemini:    {"generativelanguage.googleapis.com"},
}

func hostMatches(host string, domains []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// IsOfficialHost reports whether the endpoint is the official HTTPS API
// host for the transport (exact host only, never a substring of the
// path/query, so a lookalike URL cannot select a credential; cleartext
// http never qualifies, so ambient keys never travel unencrypted).
func IsOfficialHost(endpoint, transport string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Hostname() == "" || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, d := range officialDomains[transport] {
		if host == d {
			return true
		}
	}
	return false
}

// DetectTransport infers the transport from the endpoint's host (never
// from substrings elsewhere in the URL).
func DetectTransport(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err == nil && u.Hostname() != "" {
		host := u.Hostname()
		switch {
		case hostMatches(host, officialDomains[TransportAnthropic]):
			return TransportAnthropic
		case hostMatches(host, officialDomains[TransportGemini]):
			return TransportGemini
		}
	}
	return TransportOpenAI
}

// ValidTransport reports whether t is a supported transport.
func ValidTransport(t string) bool {
	return t == TransportOpenAI || t == TransportAnthropic || t == TransportGemini
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
