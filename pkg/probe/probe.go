// Package probe sends fingerprint probes to LLM endpoints over three
// transports (OpenAI-compatible, Anthropic Messages, Gemini native) and
// reports their prompt-token usage.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

// Client probes one endpoint.
type Client struct {
	Spec     config.EndpointSpec
	HTTP     *http.Client
	URL      string // fully built request URL (no key in it, ever)
	requests int
	altShape bool // OpenAI token-cap shape the endpoint accepted last
	mu       sync.Mutex
}

// New builds a probing client for a resolved endpoint spec.
func New(spec config.EndpointSpec, timeout time.Duration) (*Client, error) {
	if !config.ValidTransport(spec.Transport) {
		return nil, fmt.Errorf("unsupported transport %q (want openai, anthropic, gemini, or responses)", spec.Transport)
	}
	u, err := BuildURL(spec)
	if err != nil {
		return nil, err
	}
	// Never send an API key over cleartext HTTP except to loopback
	// (local development servers), where it cannot leave the machine.
	if spec.APIKey != "" {
		if parsed, perr := url.Parse(u); perr == nil && strings.EqualFold(parsed.Scheme, "http") && !isLoopbackHost(parsed.Hostname()) {
			return nil, fmt.Errorf("refusing to send an API key over cleartext http to %s; use https or a loopback endpoint", sanitizedEndpoint(u))
		}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		Spec: spec,
		// Redirects are never followed: a probing target must not be able
		// to bounce API-key headers (x-api-key/x-goog-api-key are not
		// stripped cross-host by the stdlib) or extra requests elsewhere.
		HTTP: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		URL: u,
	}, nil
}

// Requests returns how many HTTP requests this client has sent.
func (c *Client) Requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func (c *Client) bumpRequests() {
	c.mu.Lock()
	c.requests++
	c.mu.Unlock()
}

// sanitizedEndpoint renders an endpoint for error messages with userinfo,
// query, and fragment (all of which can carry keys) removed.
func sanitizedEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return "(unparseable endpoint)"
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
	return u.String()
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	// Unlike exact localhost and loopback IPs, .localhost subdomains are
	// not exempt from the standard transport's environment proxy rules.
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// joinResource appends a transport resource to a base path: an empty path
// gains the default version prefix, a version-only or any other non-empty
// base (Azure deployments, Gemini's /v1beta/openai, proxies) is treated as
// a complete prefix and only the resource itself is appended.
func joinResource(path, version, resource string) string {
	if path == "" {
		return "/" + version + "/" + resource
	}
	if strings.HasSuffix(strings.ToLower(path), "/"+strings.ToLower(resource)) {
		return path
	}
	return path + "/" + resource
}

// BuildURL joins a base endpoint with the transport's request path. The
// endpoint may be a bare host, a versioned base (/v1, /v2, /v1beta), any
// other prefix base (Azure deployment, proxy prefix), or a full URL.
// Existing non-secret query parameters (e.g. Azure api-version) are
// preserved; userinfo and credential query parameters are rejected so a
// key can never ride in the URL.
func BuildURL(spec config.EndpointSpec) (string, error) {
	raw := strings.TrimSpace(spec.Endpoint)
	if raw == "" {
		return "", fmt.Errorf("empty endpoint")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		// Never echo the parse error: it embeds the raw URL verbatim.
		return "", fmt.Errorf("invalid endpoint %s", sanitizedEndpoint(raw))
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("invalid endpoint %s: scheme must be http or https", sanitizedEndpoint(raw))
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("invalid endpoint %s: missing host", sanitizedEndpoint(raw))
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid endpoint %s: userinfo is not supported; pass keys via environment or config", sanitizedEndpoint(raw))
	}
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai") {
		return "", fmt.Errorf("OpenCode-hosted endpoints require the OpenCode client for free-tier models; use xlfp opencode --model opencode/<model-id> (direct HTTP probing is disabled for all OpenCode models)")
	}
	// The environment proxy bypass recognizes the exact lowercase spelling.
	// Canonicalize this loopback alias so mixed case cannot proxy a local key.
	if strings.EqualFold(u.Hostname(), "localhost") {
		host := "localhost"
		if port := u.Port(); port != "" {
			host = net.JoinHostPort(host, port)
		}
		u.Host = host
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %s: malformed query parameters", sanitizedEndpoint(raw))
	}
	for name := range query {
		normalized := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(name), "-", ""), "_", "")
		switch normalized {
		case "key", "apikey", "xapikey", "xgoogapikey", "accesstoken", "token", "password", "secret":
			return "", fmt.Errorf("invalid endpoint %s: credential query parameter %q is not supported; pass keys via environment or config", sanitizedEndpoint(raw), name)
		}
	}
	u.Fragment, u.RawFragment = "", ""
	path := strings.TrimRight(u.EscapedPath(), "/")
	lower := strings.ToLower(path)
	switch spec.Transport {
	case config.TransportAnthropic:
		if strings.HasSuffix(lower, "/count_tokens") {
			path = path[:len(path)-len("/count_tokens")]
		} else {
			path = joinResource(path, "v1", "messages")
		}
	case config.TransportResponses:
		if strings.HasSuffix(lower, "/chat/completions") {
			path = path[:len(path)-len("/chat/completions")] + "/responses"
		} else {
			path = joinResource(path, "v1", "responses")
		}
	case config.TransportGemini:
		if strings.Contains(lower, ":streamgeneratecontent") {
			return "", fmt.Errorf("invalid endpoint %s: streaming Gemini URLs are not supported; use the base URL", sanitizedEndpoint(raw))
		}
		model := strings.TrimSpace(spec.Model)
		if !strings.HasPrefix(model, "models/") && !strings.HasPrefix(model, "tunedModels/") {
			model = "models/" + model
		}
		if strings.HasSuffix(path, ":generateContent") {
			if m := geminiModelInPath(path); m != "" && spec.Model != "" && m != model {
				return "", fmt.Errorf("endpoint %s already names model %q, conflicting with --model %q", sanitizedEndpoint(raw), m, spec.Model)
			}
		} else {
			kind, name, _ := strings.Cut(model, "/")
			if name == "" || strings.Contains(name, "/") || name == "." || name == ".." {
				return "", fmt.Errorf("invalid Gemini model name")
			}
			resource := kind + "/" + url.PathEscape(name) + ":generateContent"
			path = joinResource(path, "v1beta", resource)
		}
	default: // openai-compatible
		path = joinResource(path, "v1", "chat/completions")
	}
	u.Path, err = url.PathUnescape(path)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint path")
	}
	u.RawPath = path
	return u.String(), nil
}

func geminiModelInPath(path string) string {
	i := max(strings.LastIndex(path, "/models/"), strings.LastIndex(path, "/tunedModels/"))
	if i < 0 {
		return ""
	}
	rest := path[i+1:]
	seg := rest
	if j := strings.Index(seg, ":"); j >= 0 {
		seg = seg[:j]
	}
	if un, err := url.PathUnescape(seg); err == nil {
		return un
	}
	return seg
}

const maxResponseBytes = 4 << 20

func (c *Client) isAzureHost() bool {
	u, err := url.Parse(c.URL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return strings.HasSuffix(h, ".openai.azure.com") || strings.HasSuffix(h, ".cognitiveservices.azure.com") || strings.HasSuffix(h, ".services.ai.azure.com")
}

// setHeaders applies the transport's auth and content headers. The
// key travels only in a header, never in the URL.
func (c *Client) setHeaders(req *http.Request, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "xlfp/0.1 (+https://github.com/tyk-swe/xlfp)")
	switch c.Spec.Transport {
	case config.TransportAnthropic:
		req.Header.Set("anthropic-version", "2023-06-01")
		if c.Spec.APIKey != "" {
			req.Header.Set("x-api-key", c.Spec.APIKey)
		}
	case config.TransportGemini:
		if c.Spec.APIKey != "" {
			req.Header.Set("x-goog-api-key", c.Spec.APIKey)
		}
	default:
		if c.Spec.APIKey != "" {
			if c.isAzureHost() {
				req.Header.Set("api-key", c.Spec.APIKey)
			} else {
				req.Header.Set("Authorization", "Bearer "+c.Spec.APIKey)
			}
		}
	}
}

// reasoningShapeModel reports whether a model name looks like a
// reasoning-class OpenAI model that wants max_completion_tokens and
// no explicit temperature.
func reasoningShapeModel(model string, flip bool) bool {
	m := strings.ToLower(model)
	base := m[strings.LastIndex(m, "/")+1:]
	shape := strings.HasPrefix(base, "gpt-5") || strings.HasPrefix(base, "o1") ||
		strings.HasPrefix(base, "o3") || strings.HasPrefix(base, "o4")
	if flip {
		return !shape
	}
	return shape
}

func transientStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryDelay(h http.Header) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := time.ParseDuration(v + "s"); err == nil && secs > 0 {
			return min(secs, 2*time.Second)
		}
	}
	return 250 * time.Millisecond
}

func waitRetry(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Send sends one probe text and returns its observation. Transient
// failures (429/502/503/504 and transport errors) are retried once, and
// an OpenAI parameter-shape rejection is retried once with the alternate
// token-cap shape, which later probes then start with. Every HTTP
// attempt counts toward Client.Requests.
func (c *Client) Send(ctx context.Context, p fingerprint.Probe) (fingerprint.Observation, error) {
	return c.send(ctx, p, 0)
}

// send is Send with at most maxAttempts HTTP attempts (<= 0: no limit);
// retries that would exceed the limit are skipped.
func (c *Client) send(ctx context.Context, p fingerprint.Probe, maxAttempts int) (fingerprint.Observation, error) {
	flipped := false
	retriedTransient := false
	for attempt := 1; ; attempt++ {
		obs, raw, header, err := c.sendOnce(ctx, p, c.altShape)
		canRetry := maxAttempts <= 0 || attempt < maxAttempts
		if err != nil {
			if canRetry && !retriedTransient && ctx.Err() == nil {
				retriedTransient = true
				if werr := waitRetry(ctx, 250*time.Millisecond); werr == nil {
					continue
				}
			}
			return fingerprint.Observation{}, err
		}
		if canRetry && c.Spec.Transport == config.TransportOpenAI && !flipped && obs.HTTPStatus == http.StatusBadRequest && paramShapeRejected(raw) {
			c.altShape = !c.altShape
			flipped = true
			continue
		}
		if canRetry && transientStatus(obs.HTTPStatus) && !retriedTransient {
			retriedTransient = true
			if werr := waitRetry(ctx, retryDelay(header)); werr == nil {
				continue
			}
		}
		return obs, nil
	}
}

func paramShapeRejected(raw []byte) bool {
	s := strings.ToLower(string(raw))
	return strings.Contains(s, "max_tokens") || strings.Contains(s, "max_completion_tokens") || strings.Contains(s, "temperature")
}

func (c *Client) sendOnce(ctx context.Context, p fingerprint.Probe, altShape bool) (fingerprint.Observation, []byte, http.Header, error) {
	if err := ctx.Err(); err != nil {
		return fingerprint.Observation{}, nil, nil, err
	}
	body, err := c.requestBody(p.Text, altShape)
	if err != nil {
		return fingerprint.Observation{}, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return fingerprint.Observation{}, nil, nil, fmt.Errorf("probe %s: %s", p.ID, c.safeErr(err))
	}
	c.setHeaders(req, false)
	c.bumpRequests()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fingerprint.Observation{}, nil, nil, fmt.Errorf("probe %s: %s", p.ID, c.safeErr(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fingerprint.Observation{}, nil, resp.Header, fmt.Errorf("probe %s: reading response: %s", p.ID, c.safeErr(err))
	}
	truncated := len(raw) > maxResponseBytes
	if truncated {
		raw = raw[:maxResponseBytes]
	}
	// Normalize the BOM before either error redaction or usage parsing.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	obs := fingerprint.Observation{ProbeID: p.ID, HTTPStatus: resp.StatusCode}
	redact := func(s string) string {
		return redactErrorText(s, c.Spec.APIKey)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch {
		case truncated:
			// Truncation can make JSON undecodable and prevent safe redaction.
			obs.Error = fmt.Sprintf("HTTP %d: response exceeded size limit", resp.StatusCode)
		case c.Spec.APIKey != "" && !json.Valid(raw):
			// Prefixes and malformed JSON can conceal an escaped key from
			// literal replacement. Keep the status, but fail closed on the body.
			obs.Error = fmt.Sprintf("HTTP %d: non-JSON error body omitted", resp.StatusCode)
		default:
			obs.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet([]byte(redact(string(raw)))))
		}
		if p.Special && !truncated && !paramShapeRejected(raw) && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) {
			obs.Rejected = true
			obs.Error = ""
		}
		return obs, raw, resp.Header, nil
	}
	// A valid JSON prefix is not evidence that an oversized body is valid.
	if truncated {
		obs.Error = "response exceeded size limit"
		return obs, nil, resp.Header, nil
	}
	if msg, isErr := topLevelError(raw); isErr {
		obs.Error = "endpoint error in 200 response: " + snippet([]byte(redact(msg)))
		return obs, raw, resp.Header, nil
	}
	tokens, ok := parseUsage(c.Spec.Transport, raw)
	switch {
	case ok && tokens > 0:
		obs.PromptTokens, obs.HasUsage = tokens, true
	case ok:
		obs.Error = "response reported non-positive prompt-token usage"
	default:
		obs.Error = "response carried no prompt-token usage"
	}
	return obs, raw, resp.Header, nil
}

// redactErrorText decodes JSON string values and member names before
// redacting, so escaped slashes and Unicode escapes cannot hide a key.
// Plain-text errors retain their original format.
func redactErrorText(s, key string) string {
	if key == "" {
		return s
	}
	redact := func(s string) string { return strings.ReplaceAll(s, key, "[redacted]") }
	if !json.Valid([]byte(s)) {
		return redact(s)
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return redact(s)
	}
	var clean func(any) any
	clean = func(v any) any {
		switch v := v.(type) {
		case string:
			return redact(v)
		case []any:
			for i := range v {
				v[i] = clean(v[i])
			}
		case map[string]any:
			out := make(map[string]any, len(v))
			for name, item := range v {
				out[redact(name)] = clean(item)
			}
			return out
		}
		return v
	}
	raw, err := json.Marshal(clean(value))
	if err != nil {
		return "[unreadable endpoint error]"
	}
	return redact(string(raw))
}

// topLevelError extracts a provider error message from a 2xx JSON body.
func topLevelError(raw []byte) (string, bool) {
	var v struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &v) != nil || len(v.Error) == 0 || string(v.Error) == "null" {
		return "", false
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(v.Error, &obj) == nil && obj.Message != "" {
		return obj.Message, true
	}
	var s string
	if json.Unmarshal(v.Error, &s) == nil && s != "" {
		return s, true
	}
	return string(v.Error), true
}

// safeErr renders a transport error without leaking the request URL
// (which may carry a key in its query) or the API key itself: net/http
// wraps failures in *url.Error whose text embeds the full URL.
func (c *Client) safeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	s := err.Error()
	if c.Spec.APIKey != "" {
		s = strings.ReplaceAll(s, c.Spec.APIKey, "[redacted]")
	}
	return s
}

func (c *Client) requestBody(text string, altShape bool) ([]byte, error) {
	switch c.Spec.Transport {
	case config.TransportAnthropic:
		return json.Marshal(map[string]any{
			"model": c.Spec.Model, "max_tokens": 1, "temperature": 0,
			"messages": []map[string]any{{"role": "user", "content": text}},
		})
	case config.TransportGemini:
		return json.Marshal(map[string]any{
			"contents":         []map[string]any{{"role": "user", "parts": []map[string]any{{"text": text}}}},
			"generationConfig": map[string]any{"maxOutputTokens": 1, "temperature": 0},
		})
	case config.TransportResponses:
		return json.Marshal(map[string]any{
			"model": c.Spec.Model, "input": text, "max_output_tokens": 1,
		})
	default:
		body := map[string]any{
			"model":    c.Spec.Model,
			"messages": []map[string]any{{"role": "user", "content": text}},
		}
		// Reasoning-class OpenAI models (gpt-5, o-series) reject max_tokens
		// and non-default temperature; classic chat models and most
		// compatible providers expect max_tokens. Prompt-token counts do
		// not depend on either choice.
		reasoningShape := reasoningShapeModel(c.Spec.Model, altShape)
		if reasoningShape {
			body["max_completion_tokens"] = 1
		} else {
			body["max_tokens"] = 1
			body["temperature"] = 0
		}
		return json.Marshal(body)
	}
}

func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	// Provider errors are untrusted terminal output. Keep readable Unicode,
	// but remove escape/control characters (including OSC/CSI introducers).
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		cut := 200
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// Options controls a staged fingerprint run.
type Options struct {
	MaxRequests   int     // hard cap per endpoint; <=0 uses len(probes)
	MinConfidence float64 // verdict threshold; <=0 uses fingerprint default
	// OnProbe, when set, is called after each probe request completes
	// with the number of requests spent so far in this run (retries
	// included). It is called synchronously; keep it cheap.
	OnProbe func(requestsSpent int)
}

// Fingerprint probes an endpoint in adaptive stages: probes are sent one
// stage at a time and staging stops early once the ranking is confidently
// settled, the request cap is hit, or a whole stage produced no
// usable usage. A probe failure (transport error or HTTP >= 300) stops
// probing; when enough usable counts were already collected, the partial
// evidence is still ranked and returned with the failure noted, while a
// failure before enough counts for a ranking is an operational error, with
// any collected observations returned in the result alongside the error.
func Fingerprint(ctx context.Context, c *Client, db *fingerprint.DB, opts Options) (fingerprint.Result, error) {
	return FingerprintWithBackend(ctx, httpBackend{c}, db, opts)
}

// Backend supplies observations and counts attempts in its own unit. An HTTP
// attempt is a request; an OpenCode attempt is a CLI probe run, which may make
// several provider requests internally. Implementations must honor the remaining
// budget and context, and must return errors safe for inclusion in reports.
type Backend interface {
	SendProbe(context.Context, fingerprint.Probe, int) (fingerprint.Observation, error)
	Attempts() int
}

type httpBackend struct{ *Client }

func (b httpBackend) Attempts() int { return b.Requests() }
func (b httpBackend) SendProbe(ctx context.Context, p fingerprint.Probe, remaining int) (fingerprint.Observation, error) {
	return b.send(ctx, p, remaining)
}

// FingerprintWithBackend runs the same adaptive ranking for a non-HTTP backend.
// Options.MaxRequests, OnProbe and Result.RequestsSpent use backend attempt units;
// callers must label these units correctly when presenting results.
func FingerprintWithBackend(ctx context.Context, c Backend, db *fingerprint.DB, opts Options) (fingerprint.Result, error) {
	maxReq := opts.MaxRequests
	if maxReq <= 0 {
		maxReq = len(db.Probes)
	}
	var obs []fingerprint.Observation
	startReq := c.Attempts()
	spent := func() int { return c.Attempts() - startReq }
	stages := []int{}
	seenStage := map[int]bool{}
	ordinary := map[string]bool{}
	for _, p := range db.Probes {
		ordinary[p.ID] = !p.Special
		if !seenStage[p.Stage] {
			stages = append(stages, p.Stage)
			seenStage[p.Stage] = true
		}
	}
	sort.Ints(stages)
	usable := func() int {
		n := 0
		for _, o := range obs {
			if ordinary[o.ProbeID] && o.UsableCount() {
				n++
			}
		}
		return n
	}
	// fail converts a probe failure into a partial ranked result when the
	// evidence gathered so far can be ranked, else an error.
	fail := func(failure string) (fingerprint.Result, error) {
		if usable() < fingerprint.MinUsableProbes {
			res := db.Rank(obs, opts.MinConfidence)
			res.RequestsSpent = spent()
			return res, errors.New(failure)
		}
		res := db.Rank(obs, opts.MinConfidence)
		res.RequestsSpent = spent()
		res.ProbeError = failure
		note := "probing stopped early: " + failure
		if res.Reason == "" {
			res.Reason = note
		} else {
			res.Reason += "; " + note
		}
		return res, nil
	}
	for _, stage := range stages {
		before := usable()
		for _, p := range db.Probes {
			if p.Stage != stage {
				continue
			}
			if spent() >= maxReq {
				break
			}
			o, err := c.SendProbe(ctx, p, maxReq-spent())
			if opts.OnProbe != nil {
				opts.OnProbe(spent())
			}
			if err != nil {
				obs = append(obs, fingerprint.Observation{ProbeID: p.ID, Error: err.Error()})
				return fail(fmt.Sprintf("probe %s failed: %v", p.ID, err))
			}
			obs = append(obs, o)
			if o.Error != "" && (o.HTTPStatus >= 300 || strings.HasPrefix(o.Error, "endpoint error in 200 response")) {
				return fail(fmt.Sprintf("probe %s failed: %s", p.ID, o.Error))
			}
		}
		res := db.Rank(obs, opts.MinConfidence)
		// Stop when an entire stage adds no usable ordinary counts;
		// prior-stage evidence must not keep an unproductive run going.
		if res.ShouldStop() || spent() >= maxReq || (len(obs) > 0 && usable() == before) {
			res.RequestsSpent = spent()
			return res, nil
		}
	}
	res := db.Rank(obs, opts.MinConfidence)
	res.RequestsSpent = spent()
	return res, nil
}
