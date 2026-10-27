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
	"strings"
	"time"
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
}

// New builds a probing client for a resolved endpoint spec.
func New(spec config.EndpointSpec, timeout time.Duration) (*Client, error) {
	if !config.ValidTransport(spec.Transport) {
		return nil, fmt.Errorf("unsupported transport %q (want openai, anthropic, or gemini)", spec.Transport)
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
func (c *Client) Requests() int { return c.requests }

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
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
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
	for name := range u.Query() {
		switch strings.ToLower(name) {
		case "key", "api_key", "apikey", "access_token", "token", "password", "secret":
			return "", fmt.Errorf("invalid endpoint %s: credential query parameter %q is not supported; pass keys via environment or config", sanitizedEndpoint(raw), name)
		}
	}
	u.Fragment, u.RawFragment, u.RawPath = "", "", ""
	path := strings.TrimRight(u.Path, "/")
	lower := strings.ToLower(path)
	switch spec.Transport {
	case config.TransportAnthropic:
		if strings.HasSuffix(lower, "/count_tokens") {
			path = path[:len(path)-len("/count_tokens")]
		} else {
			path = joinResource(path, "v1", "messages")
		}
	case config.TransportGemini:
		if strings.Contains(lower, ":streamgeneratecontent") {
			return "", fmt.Errorf("invalid endpoint %s: streaming Gemini URLs are not supported; use the base URL", sanitizedEndpoint(raw))
		}
		if strings.Contains(path, ":generateContent") {
			if m := geminiModelInPath(path); m != "" && spec.Model != "" && m != strings.TrimPrefix(strings.TrimPrefix(spec.Model, "models/"), "tunedModels/") {
				return "", fmt.Errorf("endpoint %s already names model %q, conflicting with --model %q", sanitizedEndpoint(raw), m, spec.Model)
			}
		} else {
			model := strings.TrimSpace(spec.Model)
			resource := "models/" + strings.TrimPrefix(model, "models/") + ":generateContent"
			if strings.HasPrefix(model, "tunedModels/") {
				resource = model + ":generateContent"
			}
			path = joinResource(path, "v1beta", resource)
		}
	default: // openai-compatible
		path = joinResource(path, "v1", "chat/completions")
	}
	u.Path = path
	return u.String(), nil
}

func geminiModelInPath(path string) string {
	i := strings.LastIndex(path, "/models/")
	if i < 0 {
		if i = strings.LastIndex(path, "/tunedModels/"); i < 0 {
			return ""
		}
	}
	rest := path[i+1:]
	seg := rest[strings.Index(rest, "/")+1:]
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
	body, err := c.requestBody(p.Text, altShape)
	if err != nil {
		return fingerprint.Observation{}, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return fingerprint.Observation{}, nil, nil, fmt.Errorf("probe %s: %s", p.ID, c.safeErr(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
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
	c.requests++
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
	obs := fingerprint.Observation{ProbeID: p.ID, HTTPStatus: resp.StatusCode}
	redact := func(s string) string {
		if c.Spec.APIKey != "" {
			s = strings.ReplaceAll(s, c.Spec.APIKey, "[redacted]")
		}
		return s
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		obs.Error = redact(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet(raw)))
		if p.Special && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) {
			obs.Rejected = true
			obs.Error = ""
		}
		return obs, raw, resp.Header, nil
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if msg, isErr := topLevelError(raw); isErr {
		obs.Error = redact("endpoint error in 200 response: " + msg)
		return obs, raw, resp.Header, nil
	}
	tokens, ok := parseUsage(c.Spec.Transport, raw)
	switch {
	case ok && tokens > 0:
		obs.PromptTokens, obs.HasUsage = tokens, true
	case ok:
		obs.Error = "response reported non-positive prompt-token usage"
	case truncated:
		obs.Error = "response exceeded size limit and carried no parseable prompt-token usage"
	default:
		obs.Error = "response carried no prompt-token usage"
	}
	return obs, raw, resp.Header, nil
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
		return snippet([]byte(obj.Message)), true
	}
	var s string
	if json.Unmarshal(v.Error, &s) == nil && s != "" {
		return snippet([]byte(s)), true
	}
	return snippet(v.Error), true
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
	default:
		body := map[string]any{
			"model":    c.Spec.Model,
			"messages": []map[string]any{{"role": "user", "content": text}},
		}
		// Reasoning-class OpenAI models (gpt-5, o-series) reject max_tokens
		// and non-default temperature; classic chat models and most
		// compatible providers expect max_tokens. Prompt-token counts do
		// not depend on either choice.
		m := strings.ToLower(c.Spec.Model)
		base := m[strings.LastIndex(m, "/")+1:]
		reasoningShape := strings.HasPrefix(base, "gpt-5") || strings.HasPrefix(base, "o1") ||
			strings.HasPrefix(base, "o3") || strings.HasPrefix(base, "o4")
		if altShape {
			reasoningShape = !reasoningShape
		}
		if reasoningShape {
			body["max_completion_tokens"] = 1
		} else {
			body["max_tokens"] = 1
			body["temperature"] = 0
		}
		return json.Marshal(body)
	}
}

// flexInt accepts JSON numbers that are integral (42, 42.0, 1e3) and
// numeric strings ("42"), as some gateways emit those shapes.
type flexInt int

func (f *flexInt) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "\"") {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
	}
	var n float64
	if _, err := fmt.Sscan(s, &n); err != nil {
		return fmt.Errorf("not a number: %q", s)
	}
	if n != float64(int64(n)) {
		return fmt.Errorf("not an integer: %v", n)
	}
	*f = flexInt(int64(n))
	return nil
}

// parseUsage extracts the prompt-token count per transport. Pointer
// fields distinguish a missing usage field from a real zero. Anthropic
// totals include cache creation/read tokens when present.
func parseUsage(transport string, raw []byte) (int, bool) {
	switch transport {
	case config.TransportAnthropic:
		var r struct {
			Usage *struct {
				InputTokens         *flexInt `json:"input_tokens"`
				CacheCreationTokens *flexInt `json:"cache_creation_input_tokens"`
				CacheReadTokens     *flexInt `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Usage != nil && r.Usage.InputTokens != nil {
			total := int(*r.Usage.InputTokens)
			if r.Usage.CacheCreationTokens != nil {
				total += int(*r.Usage.CacheCreationTokens)
			}
			if r.Usage.CacheReadTokens != nil {
				total += int(*r.Usage.CacheReadTokens)
			}
			return total, true
		}
	case config.TransportGemini:
		var r struct {
			UsageMetadata *struct {
				PromptTokenCount *flexInt `json:"promptTokenCount"`
			} `json:"usageMetadata"`
		}
		if json.Unmarshal(raw, &r) == nil && r.UsageMetadata != nil && r.UsageMetadata.PromptTokenCount != nil {
			return int(*r.UsageMetadata.PromptTokenCount), true
		}
	default:
		var r struct {
			Usage *struct {
				PromptTokens *flexInt `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Usage != nil && r.Usage.PromptTokens != nil {
			return int(*r.Usage.PromptTokens), true
		}
	}
	return 0, false
}

func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
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
// failure before any verdict-grade evidence is an operational error.
func Fingerprint(ctx context.Context, c *Client, db *fingerprint.DB, opts Options) (fingerprint.Result, error) {
	maxReq := opts.MaxRequests
	if maxReq <= 0 {
		maxReq = len(db.Probes)
	}
	var obs []fingerprint.Observation
	startReq := c.Requests()
	spent := func() int { return c.Requests() - startReq }
	maxStage := 0
	for _, p := range db.Probes {
		if p.Stage > maxStage {
			maxStage = p.Stage
		}
	}
	usable := func() int {
		n := 0
		for _, o := range obs {
			if o.HasUsage && o.PromptTokens > 0 && !o.Rejected {
				n++
			}
		}
		return n
	}
	// fail converts a probe failure into a partial ranked result when the
	// evidence gathered so far is verdict-grade, else an error.
	fail := func(failure string) (fingerprint.Result, error) {
		if usable() < fingerprint.MinUsableProbes {
			return fingerprint.Result{}, errors.New(failure)
		}
		res := db.Rank(obs, opts.MinConfidence)
		res.RequestsSpent = spent()
		note := "probing stopped early: " + failure
		if res.Reason == "" {
			res.Reason = note
		} else {
			res.Reason += "; " + note
		}
		return res, nil
	}
	for stage := 1; stage <= maxStage; stage++ {
		for _, p := range db.Probes {
			if p.Stage != stage {
				continue
			}
			if spent() >= maxReq {
				break
			}
			o, err := c.send(ctx, p, maxReq-spent())
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
		// An endpoint that returns no usage at all will not start doing so
		// in later stages; do not burn the whole budget proving it.
		if res.ShouldStop() || spent() >= maxReq || (len(obs) > 0 && usable() == 0) {
			res.RequestsSpent = spent()
			return res, nil
		}
	}
	res := db.Rank(obs, opts.MinConfidence)
	res.RequestsSpent = spent()
	return res, nil
}
