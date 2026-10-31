package probe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

type responseRoundTripFunc func(*http.Request) (*http.Response, error)

func (f responseRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func responseClient(t *testing.T, status int, key string, body func() io.ReadCloser) *Client {
	t.Helper()
	c, err := New(config.EndpointSpec{
		Endpoint: "https://example.test", Model: "m", Transport: config.TransportOpenAI, APIKey: key,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP.Transport = responseRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: body(), Request: r}, nil
	})
	return c
}

func TestHTTPResponseErrors(t *testing.T) {
	const key = "sk-review/secret"
	const escaped = `{"error":"invalid sk-review\/secret"}`
	const redacted = `HTTP 401: {"error":"invalid [redacted]"}`
	const omitted = "HTTP 401: non-JSON error body omitted"
	const tooLarge = "HTTP 401: response exceeded size limit"
	padded := func(prefix string, size int) string {
		return prefix + strings.Repeat(" ", size-len(prefix))
	}
	cases := []struct {
		name, body, key, want string
	}{
		{"escaped key in JSON name and value", `{"sk-review\/secret":"invalid \u0073k-review\/secret"}`, key, `HTTP 401: {"[redacted]":"invalid [redacted]"}`},
		{"BOM", "\ufeff" + escaped, key, redacted},
		{"malformed JSON", "gateway: " + escaped, key, omitted},
		{"plaintext with key", "invalid key " + key, key, omitted},
		{"keyless plaintext", "authentication failed", "", "HTTP 401: authentication failed"},
		{"at limit", padded(escaped, maxResponseBytes), key, redacted},
		{"BOM at limit", padded("\ufeff"+escaped, maxResponseBytes), key, redacted},
		{"over limit", padded(escaped, maxResponseBytes+1), key, tooLarge},
		{"BOM over limit", padded("\ufeff"+escaped, maxResponseBytes+1), key, tooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"probe", "chat", "streaming chat"} {
				t.Run(mode, func(t *testing.T) {
					c := responseClient(t, http.StatusUnauthorized, tc.key, func() io.ReadCloser {
						return io.NopCloser(strings.NewReader(tc.body))
					})
					var status int
					var got string
					if mode == "probe" {
						obs, err := c.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
						if err != nil {
							t.Fatal(err)
						}
						if obs.HasUsage || obs.Rejected {
							t.Fatalf("error response became evidence: %+v", obs)
						}
						status, got = obs.HTTPStatus, obs.Error
					} else {
						res, err := c.Chat(context.Background(), ChatRequest{User: "hello", Stream: mode == "streaming chat"})
						if err != nil {
							t.Fatal(err)
						}
						if res.HasUsage || res.Text != "" || res.Completed || res.Terminated {
							t.Fatalf("error response became a completed result: %+v", res)
						}
						status, got = res.HTTPStatus, res.Error
					}
					if status != http.StatusUnauthorized || got != tc.want || c.Requests() != 1 {
						t.Fatalf("status=%d error=%q requests=%d; want status=401 error=%q requests=1", status, got, c.Requests(), tc.want)
					}
				})
			}
		})
	}
}

func TestOversizedChatSuccessIsNotPartialEvidence(t *testing.T) {
	prefix := "\ufeff" + `{"usage":{"prompt_tokens":42},"choices":[{"message":{"content":"partial"}}]}`
	body := prefix + strings.Repeat(" ", maxResponseBytes+1-len(prefix))
	c := responseClient(t, http.StatusOK, "", func() io.ReadCloser {
		return io.NopCloser(strings.NewReader(body))
	})
	res, err := c.Chat(context.Background(), ChatRequest{User: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != "response exceeded size limit" || res.HasUsage || res.Text != "" || res.Terminated || res.Completed {
		t.Fatalf("oversized response became evidence: %+v", res)
	}
}

func TestHTTPResponseBodyReadsAreBounded(t *testing.T) {
	body := strings.Repeat("x", maxResponseBytes+1024)
	for _, mode := range []string{"probe", "chat", "streaming chat"} {
		t.Run(mode, func(t *testing.T) {
			reader := &io.LimitedReader{R: strings.NewReader(body), N: int64(len(body))}
			c := responseClient(t, http.StatusUnauthorized, "", func() io.ReadCloser {
				return io.NopCloser(reader)
			})
			var got string
			if mode == "probe" {
				obs, err := c.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
				if err != nil {
					t.Fatal(err)
				}
				got = obs.Error
			} else {
				res, err := c.Chat(context.Background(), ChatRequest{User: "hello", Stream: mode == "streaming chat"})
				if err != nil {
					t.Fatal(err)
				}
				got = res.Error
			}
			if read := int64(len(body)) - reader.N; read != maxResponseBytes+1 || got != "HTTP 401: response exceeded size limit" {
				t.Fatalf("read %d bytes, error=%q; want %d bytes and a size-limit error", read, got, maxResponseBytes+1)
			}
		})
	}
}

type failingResponseBody struct {
	partial string
	err     error
}

func (b *failingResponseBody) Read(p []byte) (int, error) {
	n := copy(p, b.partial)
	b.partial = b.partial[n:]
	if b.partial != "" {
		return n, nil
	}
	return n, b.err
}

func (*failingResponseBody) Close() error { return nil }

func TestHTTPResponseReadFailure(t *testing.T) {
	const key = "sk-read-secret"
	const partial = `{"error":"partial response"}`
	for _, tc := range []struct {
		name   string
		status int
		probe  bool
		stream bool
	}{
		{"probe error", http.StatusUnauthorized, true, false},
		{"probe success", http.StatusOK, true, false},
		{"chat error", http.StatusUnauthorized, false, false},
		{"chat success", http.StatusOK, false, false},
		{"streaming chat error", http.StatusUnauthorized, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := responseClient(t, tc.status, key, func() io.ReadCloser {
				return &failingResponseBody{partial: partial, err: errors.New("connection interrupted for " + key)}
			})
			var err error
			prefix := "chat: "
			if tc.probe {
				var obs fingerprint.Observation
				obs, err = c.send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"}, 1)
				if obs != (fingerprint.Observation{}) {
					t.Fatalf("partial response retained: %+v", obs)
				}
				prefix = "probe p: "
			} else {
				var res ChatResult
				res, err = c.Chat(context.Background(), ChatRequest{User: "hello", Stream: tc.stream})
				if res != (ChatResult{}) {
					t.Fatalf("partial response retained: %+v", res)
				}
			}
			want := prefix + "reading response: connection interrupted for [redacted]"
			if err == nil || err.Error() != want || c.Requests() != 1 {
				t.Fatalf("error=%v requests=%d; want %q and one request", err, c.Requests(), want)
			}
		})
	}
}

func TestResponseReadFailureRetriesWithinBudget(t *testing.T) {
	calls := 0
	c := responseClient(t, http.StatusOK, "", func() io.ReadCloser {
		calls++
		if calls == 1 {
			return &failingResponseBody{partial: "partial", err: errors.New("connection interrupted")}
		}
		return io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":42}}`))
	})
	obs, err := c.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
	if err != nil || !obs.HasUsage || obs.PromptTokens != 42 || c.Requests() != 2 || calls != 2 {
		t.Fatalf("retry lost: observation=%+v error=%v requests=%d calls=%d", obs, err, c.Requests(), calls)
	}
}
