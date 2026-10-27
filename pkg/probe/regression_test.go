package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestUsageIntegersAreExactAndBounded(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, raw := range []string{fmt.Sprint(maxInt), fmt.Sprintf("%d.0", maxInt), fmt.Sprintf(`"%de0"`, maxInt)} {
		got, ok := parseUsage("openai", []byte(`{"usage":{"prompt_tokens":`+raw+`}}`))
		if !ok || got != maxInt {
			t.Errorf("%s parsed as %d,%v; want %d,true", raw, got, ok, maxInt)
		}
	}
	for _, raw := range []string{`42.000000000000000001`, `"42.000000000000000001"`, `-1`, `"NaN"`, `"Inf"`, `"0x1p2"`, `1e999999999`, `9223372036854775808`} {
		if n, ok := parseUsage("openai", []byte(`{"usage":{"prompt_tokens":`+raw+`}}`)); ok {
			t.Errorf("invalid token count %s accepted as %d", raw, n)
		}
	}
	if strconv.IntSize == 64 {
		n, ok := parseUsage("openai", []byte(`{"usage":{"prompt_tokens":9007199254740993}}`))
		if !ok || int64(n) != 9007199254740993 {
			t.Errorf("integer rounded: %d,%v", n, ok)
		}
	} else {
		if _, ok := parseUsage("openai", []byte(`{"usage":{"prompt_tokens":4294967338}}`)); ok {
			t.Fatal("32-bit overflow accepted")
		}
	}
	for _, raw := range []string{
		`{"usage":{"input_tokens":42,"cache_read_input_tokens":-1}}`,
		fmt.Sprintf(`{"usage":{"input_tokens":%d,"cache_read_input_tokens":1}}`, maxInt),
	} {
		if n, ok := parseUsage("anthropic", []byte(raw)); ok {
			t.Errorf("invalid cache total accepted as %d", n)
		}
	}
}

func TestBuildURLPreservesEscapedPath(t *testing.T) {
	for _, tc := range []struct{ transport, endpoint, model, want string }{
		{"openai", "https://example.test/proxy/a%2Fb", "m", "https://example.test/proxy/a%2Fb/chat/completions"},
		{"anthropic", "https://example.test/proxy/a%2Fb/messages/count_tokens", "m", "https://example.test/proxy/a%2Fb/messages"},
		{"gemini", "https://example.test/proxy/a%2Fb", "m", "https://example.test/proxy/a%2Fb/models/m:generateContent"},
	} {
		got, err := BuildURL(config.EndpointSpec{Transport: tc.transport, Endpoint: tc.endpoint, Model: tc.model})
		if err != nil || got != tc.want {
			t.Errorf("got %q, %v; want %q", got, err, tc.want)
		}
	}
}

func TestGeminiModelNamespacesMustAgree(t *testing.T) {
	for _, tc := range []struct{ endpoint, model string }{
		{"https://example.test/v1beta/models/same:generateContent", "tunedModels/same"},
		{"https://example.test/v1beta/tunedModels/same:generateContent", "models/same"},
	} {
		if _, err := BuildURL(config.EndpointSpec{Transport: "gemini", Endpoint: tc.endpoint, Model: tc.model}); err == nil {
			t.Errorf("model namespace conflict accepted: %+v", tc)
		}
	}
}

func TestCanceledSendSpendsNoRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request sent") }))
	defer srv.Close()
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Send(ctx, fingerprint.Probe{ID: "p", Text: "hello"}); err == nil {
		t.Fatal("canceled send succeeded")
	}
	if client.Requests() != 0 {
		t.Errorf("canceled send charged %d requests", client.Requests())
	}
}

func TestFingerprintStopsAfterEmptyStage(t *testing.T) {
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 4 {
			fmt.Fprintf(w, `{"usage":{"prompt_tokens":%d}}`, 10+calls*7)
		} else {
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()
	client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Fingerprint(context.Background(), client, db, Options{MinConfidence: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequestsSpent != 8 {
		t.Errorf("sent %d requests after stage 2 yielded no usage; want 8", res.RequestsSpent)
	}
}

func FuzzUsageNeverWraps(f *testing.F) {
	for _, s := range []string{"0", "42", "42.0", "1e3", "9007199254740993", "-1", "1e999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		raw, _ := json.Marshal(s)
		n, ok := parseUsage("openai", []byte(`{"usage":{"prompt_tokens":`+string(raw)+`}}`))
		if ok && n < 0 {
			t.Fatalf("negative usage accepted: %q = %d", s, n)
		}
		if i, err := strconv.ParseInt(strings.TrimSpace(s), 10, strconv.IntSize); err == nil && i >= 0 && json.Valid([]byte(strings.TrimSpace(s))) {
			if !ok || int64(n) != i {
				t.Fatalf("integer changed: %q = %d,%v", s, n, ok)
			}
		}
	})
}

func TestOversizedSuccessIsNotPartialEvidence(t *testing.T) {
	prefix := `{"usage":{"prompt_tokens":42}}`
	body := prefix + strings.Repeat(" ", maxResponseBytes-len(prefix)) + `{"error":"hidden beyond limit"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	c, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	o, err := c.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if o.HasUsage || !strings.Contains(o.Error, "size limit") {
		t.Fatalf("truncated response accepted: %+v", o)
	}
}

func TestSpecialParameterErrorIsNotTokenRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"unsupported max_tokens parameter"}`)
	}))
	defer srv.Close()
	c, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: "openai"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	o, err := c.send(context.Background(), fingerprint.Probe{ID: "special", Text: "<|endoftext|>", Special: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if o.Rejected || o.Error == "" {
		t.Fatalf("parameter failure masqueraded as special-token evidence: %+v", o)
	}
}

func TestGeminiNestedPrefixUsesFinalModelResource(t *testing.T) {
	endpoint := "https://example.test/proxy/models/cache/v1beta/tunedModels/t1:generateContent"
	got, err := BuildURL(config.EndpointSpec{Endpoint: endpoint, Model: "tunedModels/t1", Transport: "gemini"})
	if err != nil || got != endpoint {
		t.Fatalf("nested prefix confused model identity: %q %v", got, err)
	}
}

func TestErrorSnippetCannotEmitTerminalControls(t *testing.T) {
	for _, s := range []string{"bad \x1b]52;c;ZXZpbA==\a", "bad \u009b31m", "bad\x00response"} {
		got := snippet([]byte(s))
		if strings.ContainsAny(got, "\x1b\a\u009b\x00") {
			t.Errorf("untrusted response retained terminal controls: %q", got)
		}
	}
	if got := snippet([]byte("bad\n response\tこんにちは")); got != "bad response こんにちは" {
		t.Fatalf("readable Unicode error changed: %q", got)
	}
}

// ProxyFromEnvironment caches environment variables, so exercise the actual
// default transport in a fresh process rather than relying on test order.
func TestMixedCaseLocalhostCannotProxyCredentials(t *testing.T) {
	if endpoint := os.Getenv("XLFP_TEST_LOCALHOST_ENDPOINT"); endpoint != "" {
		c, err := New(config.EndpointSpec{Endpoint: endpoint, Model: "m", Transport: "openai", APIKey: "fixture-secret"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		o, err := c.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
		if err != nil || o.PromptTokens != 42 {
			t.Fatalf("local request went through proxy: %+v %v", o, err)
		}
		return
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"usage":{"prompt_tokens":42}}`) }))
	defer target.Close()
	var leaked atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		fmt.Fprint(w, `{"usage":{"prompt_tokens":999}}`)
	}))
	defer proxy.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMixedCaseLocalhostCannotProxyCredentials$")
	cmd.Env = append(os.Environ(),
		"XLFP_TEST_LOCALHOST_ENDPOINT="+strings.Replace(target.URL, "127.0.0.1", "LOCALHOST", 1),
		"HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=", "REQUEST_METHOD=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil || leaked.Load() != 0 {
		t.Fatalf("loopback credentials escaped: leaks=%d err=%v\n%s", leaked.Load(), err, out)
	}
}
