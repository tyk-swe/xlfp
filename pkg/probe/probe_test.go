package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestBuildURL(t *testing.T) {
	cases := []struct{ transport, endpoint, model, want string }{
		{"openai", "https://api.example.com", "m", "https://api.example.com/v1/chat/completions"},
		{"openai", "https://api.example.com/v1", "m", "https://api.example.com/v1/chat/completions"},
		{"openai", "https://api.example.com/v1/chat/completions", "m", "https://api.example.com/v1/chat/completions"},
		{"openai", "https://api.example.com/v1?api-version=2024-02-01", "m", "https://api.example.com/v1/chat/completions?api-version=2024-02-01"},
		{"openai", "https://api.example.com?api-version=2024-02-01#frag", "m", "https://api.example.com/v1/chat/completions?api-version=2024-02-01"},
		{"anthropic", "https://api.anthropic.com", "m", "https://api.anthropic.com/v1/messages"},
		{"anthropic", "https://api.anthropic.com/v1", "m", "https://api.anthropic.com/v1/messages"},
		{"anthropic", "https://api.anthropic.com/v1/messages/count_tokens", "m", "https://api.anthropic.com/v1/messages"},
		{"openai", "https://generativelanguage.googleapis.com/v1beta/openai", "m", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
		{"openai", "https://api.example.com/v2", "m", "https://api.example.com/v2/chat/completions"},
		{"openai", "https://res.openai.azure.com/openai/deployments/dep1", "m", "https://res.openai.azure.com/openai/deployments/dep1/chat/completions"},
		{"gemini", "https://generativelanguage.googleapis.com", "gemini-2.0-flash", "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent"},
		{"gemini", "https://x.example/v1beta", "m", "https://x.example/v1beta/models/m:generateContent"},
		{"gemini", "https://x.example", "models/gemini-2.0-flash", "https://x.example/v1beta/models/gemini-2.0-flash:generateContent"},
		{"gemini", "https://x.example/v1beta/tunedModels/t1:generateContent", "tunedModels/t1", "https://x.example/v1beta/tunedModels/t1:generateContent"},
	}
	for _, c := range cases {
		got, err := BuildURL(config.EndpointSpec{Transport: c.transport, Endpoint: c.endpoint, Model: c.model})
		if err != nil || got != c.want {
			t.Errorf("BuildURL(%s %s) = %q, %v; want %q", c.transport, c.endpoint, got, err, c.want)
		}
	}
}

type mockOpts struct {
	noUsage       bool
	authFail      bool
	rejectSpecial bool
}

// mockServer emulates an endpoint whose tokenizer matches the given DB
// entry, reporting expected count + a constant template overhead.
func mockServer(t *testing.T, transport, entryID string, overhead int, mo mockOpts) (*httptest.Server, *int64) {
	t.Helper()
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	textToCount := map[string]int{}
	specialText := map[string]bool{}
	for _, e := range db.Entries {
		if e.ID == entryID {
			for i, p := range db.Probes {
				textToCount[p.Text] = e.Counts[i] + overhead
				specialText[p.Text] = p.Special
			}
		}
	}
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if mo.authFail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		text := extractText(body)
		if mo.rejectSpecial && specialText[text] && strings.Contains(text, "<|endoftext|>") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"special token not allowed"}`))
			return
		}
		count, ok := textToCount[text]
		if !ok {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte(`{"error":"unknown probe ` + "" + `"}`))
			return
		}
		if mo.noUsage {
			_, _ = w.Write([]byte(`{"id":"x"}`))
			return
		}
		switch transport {
		case config.TransportAnthropic:
			_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"input_tokens": count, "output_tokens": 1}})
		case config.TransportGemini:
			_ = json.NewEncoder(w).Encode(map[string]any{"usageMetadata": map[string]any{"promptTokenCount": count}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"prompt_tokens": count, "completion_tokens": 1}})
		}
	}))
	t.Cleanup(srv.Close)
	// Only the engine part of extractText is used; keep it here to avoid a
	// helper that has to know about mockOpts.
	return srv, &calls
}

func extractText(body map[string]any) string {
	if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
		if m, ok := msgs[0].(map[string]any); ok {
			if s, ok := m["content"].(string); ok {
				return s
			}
		}
	}
	if contents, ok := body["contents"].([]any); ok && len(contents) > 0 {
		if c, ok := contents[0].(map[string]any); ok {
			if parts, ok := c["parts"].([]any); ok && len(parts) > 0 {
				if p, ok := parts[0].(map[string]any); ok {
					if s, ok := p["text"].(string); ok {
						return s
					}
				}
			}
		}
	}
	return ""
}

func runFingerprint(t *testing.T, transport, entryID string, overhead int, mo mockOpts, opts Options) (fingerprint.Result, *int64, error) {
	t.Helper()
	srv, calls := mockServer(t, transport, entryID, overhead, mo)
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "mock-model", Transport: transport, APIKey: "test-key"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Fingerprint(context.Background(), client, db, opts)
	return res, calls, err
}

func TestFingerprintOpenAIIdentifiesDeepSeek(t *testing.T) {
	res, calls, err := runFingerprint(t, config.TransportOpenAI, "deepseek-v3", 6, mockOpts{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != fingerprint.StatusIdentified || res.TopFamily != "deepseek" {
		t.Fatalf("got %s/%q reason %q", res.Status, res.TopFamily, res.Reason)
	}
	if res.RequestsSpent != int(*calls) || res.RequestsSpent > 12 {
		t.Fatalf("requests spent %d vs calls %d", res.RequestsSpent, *calls)
	}
}

func TestFingerprintAnthropicAndGeminiTransports(t *testing.T) {
	res, _, err := runFingerprint(t, config.TransportAnthropic, "mistral-v3", 9, mockOpts{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TopFamily != "mistral" {
		t.Fatalf("anthropic transport: got %s/%q reason %q", res.Status, res.TopFamily, res.Reason)
	}
	res, _, err = runFingerprint(t, config.TransportGemini, "glm4", 2, mockOpts{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TopFamily != "glm" {
		t.Fatalf("gemini transport: got %s/%q reason %q", res.Status, res.TopFamily, res.Reason)
	}
}

func TestFingerprintCapEnforced(t *testing.T) {
	res, calls, err := runFingerprint(t, config.TransportOpenAI, "deepseek-v3", 6, mockOpts{}, Options{MaxRequests: 2})
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || res.RequestsSpent != 2 {
		t.Fatalf("cap violated: calls=%d spent=%d", *calls, res.RequestsSpent)
	}
	if res.Status == fingerprint.StatusIdentified {
		t.Fatal("two probes must not produce a verdict")
	}
}

func TestFingerprintMissingUsageInconclusiveNotError(t *testing.T) {
	res, _, err := runFingerprint(t, config.TransportOpenAI, "deepseek-v3", 6, mockOpts{noUsage: true}, Options{})
	if err != nil {
		t.Fatalf("missing usage should be inconclusive, not error: %v", err)
	}
	if res.Status != fingerprint.StatusInconclusive {
		t.Fatalf("got %s", res.Status)
	}
}

func TestFingerprintAuthFailureAborts(t *testing.T) {
	_, calls, err := runFingerprint(t, config.TransportOpenAI, "deepseek-v3", 6, mockOpts{authFail: true}, Options{})
	if err == nil {
		t.Fatal("401 must be an operational error")
	}
	if *calls != 1 {
		t.Fatalf("must abort after first failed probe, got %d calls", *calls)
	}
}

func TestBuildURLErrors(t *testing.T) {
	for _, ep := range []string{
		"https://user:pass@example.com",
		"https://example.com/v1?key=secret-value",
		"https://example.com/v1?api_key=secret-value",
		"ftp://example.com",
		"https:opaque-secret",
	} {
		_, err := BuildURL(config.EndpointSpec{Transport: "openai", Endpoint: ep, Model: "m"})
		if err == nil {
			t.Errorf("endpoint %q accepted", ep)
		} else if strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "user:pass") || strings.Contains(err.Error(), "opaque-secret") {
			t.Errorf("error for %q leaks a secret: %v", ep, err)
		}
	}
	if _, err := BuildURL(config.EndpointSpec{Transport: "gemini", Endpoint: "https://x.example/v1beta/models/other:generateContent", Model: "wanted"}); err == nil {
		t.Error("conflicting Gemini full-URL model accepted")
	}
	if _, err := New(config.EndpointSpec{Transport: "openai", Endpoint: "http://example.com", Model: "m", APIKey: "k"}, 0); err == nil {
		t.Error("cleartext http with a key to a non-loopback host accepted")
	}
	// A DNS name that merely starts with "127." can resolve anywhere.
	if _, err := New(config.EndpointSpec{Transport: "openai", Endpoint: "http://127.attacker.example", Model: "m", APIKey: "k"}, 0); err == nil {
		t.Error("cleartext http with a key to a 127.-prefixed hostname accepted")
	}
	for _, ep := range []string{"http://127.0.0.1:9999", "http://127.1.2.3", "http://[::1]:9999", "http://localhost:9999"} {
		if _, err := New(config.EndpointSpec{Transport: "openai", Endpoint: ep, Model: "m", APIKey: "k"}, 0); err != nil {
			t.Errorf("loopback http with a key should be allowed for %s: %v", ep, err)
		}
	}
}

func TestCredentialQueryParametersRejected(t *testing.T) {
	for _, query := range []string{
		"api-key=DUMMY-SECRET", "API-KEY=DUMMY-SECRET", "api%2Dkey=DUMMY-SECRET",
		"access-token=DUMMY-SECRET", "x-api-key=DUMMY-SECRET", "x-goog-api-key=DUMMY-SECRET",
		"key=DUMMY-SECRET;other=value", "other=%zz&key=DUMMY-SECRET",
	} {
		for _, scheme := range []string{"http", "https"} {
			t.Run(scheme+"/"+query, func(t *testing.T) {
				_, err := New(config.EndpointSpec{Endpoint: scheme + "://provider.example/v1?" + query, Model: "m", Transport: "openai"}, 0)
				if err == nil {
					t.Fatal("URL credential accepted, including without a separately resolved API key")
				}
				if strings.Contains(err.Error(), "DUMMY-SECRET") {
					t.Fatalf("URL credential leaked in error: %v", err)
				}
			})
		}
	}
}

func TestCleartextKeyRejectsLocalhostSubdomains(t *testing.T) {
	for _, endpoint := range []string{"http://dev.localhost", "http://nested.dev.localhost:8080", "http://DEV.LOCALHOST"} {
		if _, err := New(config.EndpointSpec{Endpoint: endpoint, Model: "m", Transport: "openai", APIKey: "dummy-key"}, 0); err == nil {
			t.Errorf("keyed cleartext endpoint %s accepted; localhost subdomains can use HTTP_PROXY", endpoint)
		}
		// The restriction is about credentials: keyless local gateways still work.
		if _, err := New(config.EndpointSpec{Endpoint: endpoint, Model: "m", Transport: "openai"}, 0); err != nil {
			t.Errorf("keyless endpoint %s rejected: %v", endpoint, err)
		}
	}
	if _, err := New(config.EndpointSpec{Endpoint: "https://dev.localhost", Model: "m", Transport: "openai", APIKey: "dummy-key"}, 0); err != nil {
		t.Fatalf("HTTPS localhost subdomain rejected: %v", err)
	}
}

// Retries count toward the cap, so they must not push a run past it.
func TestFingerprintCapIncludesRetries(t *testing.T) {
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&calls, 1) == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":42}}`))
	}))
	defer srv.Close()
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = Fingerprint(context.Background(), client, db, Options{MaxRequests: 2})
	if calls != 2 || client.Requests() != 2 {
		t.Fatalf("cap 2 exceeded: calls=%d requests=%d", calls, client.Requests())
	}
}

// Once the alternate token-cap shape is accepted, later probes use it
// directly instead of paying for the same rejection every time.
func TestAltShapeRemembered(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["max_tokens"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens'. Use 'max_completion_tokens' instead."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":42}}`))
	}))
	defer srv.Close()
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "custom-deployment", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
		if err != nil || !obs.HasUsage {
			t.Fatalf("send %d: obs=%+v err=%v", i, obs, err)
		}
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4 (one rejection, then the accepted shape)", calls)
	}
}

func TestSnippetKeepsUTF8Valid(t *testing.T) {
	if s := snippet([]byte("a" + strings.Repeat("错", 100))); !utf8.ValidString(s) {
		t.Fatalf("snippet split a multi-byte character: %q", s)
	}
}

func TestParseUsageShapes(t *testing.T) {
	cases := []struct {
		transport, raw string
		want           int
		ok             bool
	}{
		{"openai", `{"usage":{"prompt_tokens":42}}`, 42, true},
		{"openai", `{"usage":{"prompt_tokens":42.0}}`, 42, true},
		{"openai", `{"usage":{"prompt_tokens":"42"}}`, 42, true},
		{"openai", `{"usage":{"prompt_tokens":"42.0"}}`, 42, true},
		{"openai", `{"usage":{"prompt_tokens":"1e3"}}`, 1000, true},
		{"openai", `{"usage":{"prompt_tokens":"42garbage"}}`, 0, false},
		{"openai", `{"usage":{"prompt_tokens":"42,000"}}`, 0, false},
		{"openai", `{"usage":{"prompt_tokens":"42 99"}}`, 0, false},
		{"openai", `{"usage":{"prompt_tokens":1e3}}`, 1000, true},
		{"openai", `{"usage":{"prompt_tokens":42.5}}`, 0, false},
		{"anthropic", `{"usage":{"input_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":3}}`, 105, true},
		{"anthropic", `{"usage":{"input_tokens":"42garbage"}}`, 0, false},
		{"gemini", `{"usageMetadata":{"promptTokenCount":17}}`, 17, true},
		{"gemini", `{"usageMetadata":{"promptTokenCount":"42,000"}}`, 0, false},
		{"openai", `{"usage":{}}`, 0, false},
	}
	for _, c := range cases {
		got, ok := parseUsage(c.transport, []byte(c.raw))
		if got != c.want || ok != c.ok {
			t.Errorf("parseUsage(%s %s) = %d,%v want %d,%v", c.transport, c.raw, got, ok, c.want, c.ok)
		}
	}
}

func TestTransientRetryOnce(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":42}}`))
	}))
	defer srv.Close()
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p1", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !obs.HasUsage || obs.PromptTokens != 42 || calls != 2 || client.Requests() != 2 {
		t.Fatalf("obs=%+v calls=%d requests=%d", obs, calls, client.Requests())
	}
}

func TestErrorBodyKeyRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad credential super-secret-key-123"}`))
	}))
	defer srv.Close()
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai", APIKey: "super-secret-key-123"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p1", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(obs.Error, "super-secret-key-123") || !strings.Contains(obs.Error, "[redacted]") {
		t.Fatalf("error not redacted: %q", obs.Error)
	}
}

func TestErrorBodyEscapedKeyRedacted(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusOK} {
		for name, body := range map[string]string{
			"escaped slash":  `{"error":{"message":"invalid key sk-dummy\/secret"}}`,
			"unicode escape": `{"error":"invalid key \u0073k-dummy/secret"}`,
			"nested detail":  `{"error":{"details":[{"credential":"sk-dummy\/secret"}]}}`,
			"member name":    `{"error":{"sk-dummy\/secret":"invalid key"}}`,
			"large number":   `{"error":{"credential":"sk-dummy\/secret","code":1e400}}`,
		} {
			t.Run(fmt.Sprintf("%d/%s", status, name), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(status)
					_, _ = w.Write([]byte(body))
				}))
				defer srv.Close()
				client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai", APIKey: "sk-dummy/secret"}, 0)
				if err != nil {
					t.Fatal(err)
				}
				obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(obs.Error, "[redacted]") || strings.Contains(obs.Error, "sk-dummy") || strings.Contains(obs.Error, `\u0073k-dummy`) {
					t.Fatalf("escaped key survived error redaction: %s", obs.Error)
				}
			})
		}
	}
}

func TestErrorKeyRedactedBeforeTruncation(t *testing.T) {
	const key = "super-secret-key-123"
	for _, status := range []int{http.StatusUnauthorized, http.StatusOK} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			prefixLen := 201 - len(key)
			if status != http.StatusOK {
				prefixLen -= len(`{"error":{"message":"`)
			}
			body, err := json.Marshal(map[string]any{"error": map[string]any{"message": strings.Repeat("x", prefixLen) + key}})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai", APIKey: key}, 0)
			if err != nil {
				t.Fatal(err)
			}
			obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(obs.Error, key[:len(key)-1]) || !strings.Contains(obs.Error, "[redacted]") {
				t.Fatalf("key was truncated before redaction: %q", obs.Error)
			}
		})
	}
}

func TestOversizedErrorDoesNotLeakEscapedKey(t *testing.T) {
	body := `{"error":{"message":"invalid key sk-dummy\/secret","padding":"` + strings.Repeat("x", maxResponseBytes) + `"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai", APIKey: "sk-dummy/secret"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(obs.Error, "sk-dummy") || !strings.Contains(obs.Error, "response exceeded size limit") {
		t.Fatalf("oversized error was not safely suppressed: %s", obs.Error)
	}
}

// Regression (review): redirects must never be followed, or an untrusted
// endpoint could bounce API-key headers to a host of its choosing.
func TestRedirectNotFollowed(t *testing.T) {
	var targetHits int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&targetHits, 1)
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":42}}`))
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	client, err := New(config.EndpointSpec{Endpoint: redir.URL, Model: "m", Transport: "anthropic", APIKey: "k"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p1", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if obs.HTTPStatus != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the 307 itself", obs.HTTPStatus)
	}
	if targetHits != 0 {
		t.Fatalf("redirect target was contacted %d times", targetHits)
	}
	if client.Requests() != 1 {
		t.Fatalf("requests = %d, want 1", client.Requests())
	}
}

// Regression (review): a mid-run failure must not discard verdict-grade
// evidence already collected; the partial ranking is returned, noted.
func TestFingerprintPartialEvidenceAfterFailure(t *testing.T) {
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	var e fingerprint.Entry
	for _, en := range db.Entries {
		if en.ID == "deepseek-v3" {
			e = en
		}
	}
	counts := map[string]int{}
	for i, p := range db.Probes {
		counts[p.Text] = e.Counts[i] + 6
	}
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&calls, 1)
		if n > 4 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"prompt_tokens": counts[extractText(body)]}})
	}))
	defer srv.Close()
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "mock-model", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A high threshold keeps stage 1 from settling, so probing reaches the
	// failing fifth request with four usable counts already in hand.
	res, err := Fingerprint(context.Background(), client, db, Options{MinConfidence: 0.999})
	if err != nil {
		t.Fatalf("partial evidence should rank, not error: %v", err)
	}
	if res.RequestsSpent != 5 || len(res.Observations) != 5 {
		t.Fatalf("spent=%d observations=%d", res.RequestsSpent, len(res.Observations))
	}
	if !strings.Contains(res.Reason, "probing stopped early") {
		t.Fatalf("reason should note the early stop, got %q", res.Reason)
	}
	if len(res.Rankings) == 0 || res.Rankings[0].Family != "deepseek" {
		t.Fatalf("partial rankings should still lead with deepseek: %+v", res.Rankings)
	}
}

func TestFingerprintSpecialRejection(t *testing.T) {
	// Directly: a 400 on a special probe is recorded as Rejected evidence.
	srv, _ := mockServer(t, config.TransportOpenAI, "openai-o200k", 4, mockOpts{rejectSpecial: true})
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "mock-model", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var special fingerprint.Probe
	for _, p := range db.Probes {
		if p.ID == "special_a" {
			special = p
		}
	}
	obs, err := client.Send(context.Background(), special)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Rejected || obs.Error != "" {
		t.Fatalf("special_a observation = %+v, want rejected with no error", obs)
	}
	// End to end: the rejecting endpoint still fingerprints as OpenAI
	// (adaptive staging may settle before stage 3 sends the special probes).
	res, _, err := runFingerprint(t, config.TransportOpenAI, "openai-o200k", 4, mockOpts{rejectSpecial: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TopFamily != "openai" {
		t.Fatalf("got %s/%q reason %q", res.Status, res.TopFamily, res.Reason)
	}
}

func TestFingerprintOnProbeReportsSpent(t *testing.T) {
	var seen []int
	res, calls, err := runFingerprint(t, config.TransportOpenAI, "deepseek-v3", 6, mockOpts{},
		Options{OnProbe: func(n int) { seen = append(seen, n) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("OnProbe was never called")
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("reported spent not strictly increasing: %v", seen)
		}
	}
	if seen[len(seen)-1] != res.RequestsSpent || res.RequestsSpent != int(*calls) {
		t.Fatalf("last OnProbe %d, RequestsSpent %d, server calls %d", seen[len(seen)-1], res.RequestsSpent, *calls)
	}
}
