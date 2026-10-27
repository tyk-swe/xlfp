package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestErrorBodyBOMAndMalformedJSONRedaction(t *testing.T) {
	const key = "sk-review/secret"
	const escaped = `{"error":{"message":"invalid key sk-review\/secret"}}`
	const unicodeEscaped = `{"error":{"message":"invalid key \u0073k-review/secret"}}`
	const omitted = "HTTP 401: non-JSON error body omitted"
	const redacted = `HTTP 401: {"error":{"message":"invalid key [redacted]"}}`
	for _, tc := range []struct {
		name, body, key, want string
		status                int
	}{
		{"BOM escaped slash", "\ufeff" + escaped, key, redacted, http.StatusUnauthorized},
		{"BOM Unicode escape", "\ufeff" + unicodeEscaped, key, redacted, http.StatusUnauthorized},
		{"prefixed escaped slash", "upstream error: " + escaped, key, omitted, http.StatusUnauthorized},
		{"prefixed Unicode escape", "upstream error: " + unicodeEscaped, key, omitted, http.StatusUnauthorized},
		{"trailing gateway text", escaped + " upstream error", key, omitted, http.StatusUnauthorized},
		{"incomplete JSON", escaped[:len(escaped)-1], key, omitted, http.StatusUnauthorized},
		{"plaintext key", "invalid key " + key, key, omitted, http.StatusUnauthorized},
		{"plaintext without echoed key", "authentication failed", key, omitted, http.StatusUnauthorized},
		{"keyless plaintext preserved", "authentication failed", "", "HTTP 401: authentication failed", http.StatusUnauthorized},
		{"BOM 200 error", "\ufeff" + escaped, key, "endpoint error in 200 response: invalid key [redacted]", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: config.TransportOpenAI, APIKey: tc.key}, 0)
			if err != nil {
				t.Fatal(err)
			}
			obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if obs.Error != tc.want || obs.HTTPStatus != tc.status || obs.HasUsage || obs.Rejected {
				t.Fatalf("observation = %+v; want HTTP %d with error %q and no evidence", obs, tc.status, tc.want)
			}
		})
	}
}

func TestErrorBodySuppressionPreservesParameterRetry(t *testing.T) {
	for name, body := range map[string]string{
		"BOM JSON":      "\ufeff" + `{"error":"unsupported max_tokens parameter"}`,
		"prefixed JSON": `upstream error: {"error":"unsupported max_tokens parameter"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(body))
					return
				}
				fmt.Fprint(w, `{"usage":{"prompt_tokens":42}}`)
			}))
			defer srv.Close()
			client, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m", Transport: config.TransportOpenAI, APIKey: "sk-review/secret"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			obs, err := client.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if !obs.HasUsage || obs.PromptTokens != 42 || obs.Error != "" || calls.Load() != 2 || client.Requests() != 2 || !client.altShape {
				t.Fatalf("parameter retry lost: observation=%+v calls=%d requests=%d altShape=%v", obs, calls.Load(), client.Requests(), client.altShape)
			}
		})
	}
}
