package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tyk-swe/xlfp/pkg/config"
)

// ChatRequest is one text-generating request. It is the input side of
// the number fingerprint: unlike Send, it returns the model's reply
// text. MaxTokens <= 0 omits the cap where the transport allows it
// (Anthropic requires one and gets a generous default).
type ChatRequest struct {
	System      string
	User        string
	MaxTokens   int
	Stream      bool
	Effort      string // reasoning effort passthrough, "" = not sent
	ServiceTier string // "" = not sent
	// StopWhen, when set on a streaming request, is consulted after
	// every chunk; returning true stops reading and marks the result
	// Truncated (relaxed-mode early stop). The text received so far
	// is kept.
	StopWhen func(text string) bool
}

// ChatResult is the outcome of one Chat call. Error carries a
// redacted endpoint error for non-2xx or in-body errors; transport
// failures are returned as the Go error instead.
type ChatResult struct {
	Text         string
	PromptTokens int
	HasUsage     bool
	HTTPStatus   int
	Truncated    bool // stopped early via StopWhen
	Terminated   bool // stream reached its terminal event / full body read
	Completed    bool // model finished naturally, without a token limit or failure
	FinishReason string
	Error        string
}

const maxChatTextBytes = 2 << 20

// Chat sends one request and returns the reply text plus prompt-token
// usage when the endpoint reports it. It never retries: sampling
// callers decide how to treat a failed sample, and every HTTP attempt
// counts toward Client.Requests. The client's HTTP timeout bounds
// the whole exchange, including streaming reads.
func (c *Client) Chat(ctx context.Context, r ChatRequest) (res ChatResult, err error) {
	// Redact the assembled reply on every return path, including partial
	// streaming replies. A key can span multiple streaming events.
	defer func() {
		if c.Spec.APIKey != "" {
			res.Text = strings.ReplaceAll(res.Text, c.Spec.APIKey, "[redacted]")
		}
	}()
	if err := ctx.Err(); err != nil {
		return ChatResult{}, err
	}
	body, err := c.chatBody(r)
	if err != nil {
		return ChatResult{}, err
	}
	reqURL := c.URL
	if r.Stream && c.Spec.Transport == config.TransportGemini {
		reqURL = geminiStreamURL(reqURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat: %s", c.safeErr(err))
	}
	c.setHeaders(req, r.Stream)
	c.bumpRequests()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat: %s", c.safeErr(err))
	}
	defer resp.Body.Close()
	res = ChatResult{HTTPStatus: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
		switch {
		case len(raw) > maxResponseBytes:
			res.Error = fmt.Sprintf("HTTP %d: response exceeded size limit", resp.StatusCode)
		case c.Spec.APIKey != "" && !json.Valid(raw):
			res.Error = fmt.Sprintf("HTTP %d: non-JSON error body omitted", resp.StatusCode)
		default:
			res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, snippet([]byte(redactErrorText(string(raw), c.Spec.APIKey))))
		}
		return res, nil
	}
	if r.Stream {
		return c.readChatStream(ctx, resp.Body, r, res)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return ChatResult{}, fmt.Errorf("chat: reading response: %s", c.safeErr(err))
	}
	if len(raw) > maxResponseBytes {
		res.Error = "response exceeded size limit"
		return res, nil
	}
	res.Terminated = true
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if msg, isErr := topLevelError(raw); isErr {
		res.Error = "endpoint error in 200 response: " + snippet([]byte(redactErrorText(msg, c.Spec.APIKey)))
		return res, nil
	}
	res.Text = extractChatText(c.Spec.Transport, raw)
	res.FinishReason = extractFinishReason(c.Spec.Transport, raw)
	res.Completed = naturalChatCompletion(c.Spec.Transport, res.FinishReason)
	if n, ok := parseUsage(c.Spec.Transport, raw); ok && n > 0 {
		res.PromptTokens, res.HasUsage = n, true
	}
	return res, nil
}

func geminiStreamURL(u string) string {
	base, query, hasQuery := strings.Cut(u, "?")
	base = strings.Replace(base, ":generateContent", ":streamGenerateContent", 1)
	if hasQuery {
		return base + "?" + query + "&alt=sse"
	}
	return base + "?alt=sse"
}

func (c *Client) chatBody(r ChatRequest) ([]byte, error) {
	switch c.Spec.Transport {
	case config.TransportAnthropic:
		maxTokens := r.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 8192
		}
		body := map[string]any{
			"model": c.Spec.Model, "max_tokens": maxTokens,
			"messages": []map[string]any{{"role": "user", "content": r.User}},
		}
		if r.System != "" {
			body["system"] = r.System
		}
		if r.Stream {
			body["stream"] = true
		}
		return json.Marshal(body)
	case config.TransportGemini:
		gen := map[string]any{}
		if r.MaxTokens > 0 {
			gen["maxOutputTokens"] = r.MaxTokens
		}
		body := map[string]any{
			"contents": []map[string]any{{"role": "user", "parts": []map[string]any{{"text": r.User}}}},
		}
		if len(gen) > 0 {
			body["generationConfig"] = gen
		}
		if r.System != "" {
			body["systemInstruction"] = map[string]any{"parts": []map[string]any{{"text": r.System}}}
		}
		return json.Marshal(body)
	case config.TransportResponses:
		body := map[string]any{"model": c.Spec.Model, "input": r.User}
		if r.System != "" {
			body["instructions"] = r.System
		}
		if r.MaxTokens > 0 {
			body["max_output_tokens"] = r.MaxTokens
		}
		if r.Stream {
			body["stream"] = true
		}
		if r.Effort != "" {
			body["reasoning"] = map[string]any{"effort": r.Effort}
		}
		if r.ServiceTier != "" {
			body["service_tier"] = r.ServiceTier
		}
		return json.Marshal(body)
	default: // openai-compatible chat completions
		msgs := []map[string]any{}
		if r.System != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": r.System})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": r.User})
		body := map[string]any{"model": c.Spec.Model, "messages": msgs}
		if r.MaxTokens > 0 {
			if reasoningShapeModel(c.Spec.Model, false) {
				body["max_completion_tokens"] = r.MaxTokens
			} else {
				body["max_tokens"] = r.MaxTokens
			}
		}
		if r.Stream {
			body["stream"] = true
			body["stream_options"] = map[string]any{"include_usage": true}
		}
		if r.Effort != "" {
			body["reasoning_effort"] = r.Effort
		}
		if r.ServiceTier != "" {
			body["service_tier"] = r.ServiceTier
		}
		return json.Marshal(body)
	}
}

// extractChatText pulls assistant text out of a non-streaming body.
func extractChatText(transport string, raw []byte) string {
	switch transport {
	case config.TransportAnthropic:
		var v struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return ""
		}
		var sb strings.Builder
		for _, b := range v.Content {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	case config.TransportGemini:
		return geminiText(raw)
	case config.TransportResponses:
		var v struct {
			OutputText string `json:"output_text"`
			Output     []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return ""
		}
		if v.OutputText != "" {
			return v.OutputText
		}
		var sb strings.Builder
		for _, item := range v.Output {
			for _, part := range item.Content {
				if part.Type == "output_text" || part.Type == "text" {
					sb.WriteString(part.Text)
				}
			}
		}
		return sb.String()
	default:
		var v struct {
			Choices []struct {
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &v) != nil || len(v.Choices) == 0 {
			return ""
		}
		return contentText(v.Choices[0].Message.Content)
	}
}

// contentText accepts both the plain-string and parts-array OpenAI
// content shapes.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "output_text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

func geminiText(raw []byte) string {
	var v struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &v) != nil || len(v.Candidates) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, p := range v.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
}

func extractFinishReason(transport string, raw []byte) string {
	switch transport {
	case config.TransportAnthropic:
		var v struct {
			StopReason string `json:"stop_reason"`
		}
		if json.Unmarshal(raw, &v) == nil {
			return v.StopReason
		}
	case config.TransportGemini:
		var v struct {
			Candidates []struct {
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
		}
		if json.Unmarshal(raw, &v) == nil && len(v.Candidates) > 0 {
			return v.Candidates[0].FinishReason
		}
	case config.TransportResponses:
		var v struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(raw, &v) == nil {
			return v.Status
		}
	default:
		var v struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &v) == nil && len(v.Choices) > 0 {
			return v.Choices[0].FinishReason
		}
	}
	return ""
}

func naturalChatCompletion(transport, reason string) bool {
	switch transport {
	case config.TransportAnthropic:
		return reason == "end_turn" || reason == "stop_sequence"
	case config.TransportGemini:
		return reason == "STOP"
	case config.TransportResponses:
		return reason == "completed"
	default:
		return reason == "stop"
	}
}

// readChatStream consumes an SSE body until the transport's terminal
// event, EOF, StopWhen, or the text cap. A later event reporting a
// null/zero usage never replaces a count already read.
func (c *Client) readChatStream(ctx context.Context, body io.Reader, r ChatRequest, res ChatResult) (ChatResult, error) {
	// Bound raw reads before ReadString can allocate a complete line.
	// Comments and metadata count toward the same limit as JSON data.
	br := bufio.NewReaderSize(io.LimitReader(body, maxResponseBytes+1), 32<<10)
	var text strings.Builder
	rawBytes := 0
	for {
		if err := ctx.Err(); err != nil {
			res.Text = text.String()
			return res, err
		}
		line, err := br.ReadString('\n')
		rawBytes += len(line)
		if rawBytes > maxResponseBytes {
			res.Error = "streamed response exceeded size limit"
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				res.Terminated = true
				break
			}
			if payload != "" {
				terminal := c.consumeStreamChunk([]byte(payload), &text, &res)
				if text.Len() > maxChatTextBytes {
					res.Error = "streamed reply exceeded size limit"
					break
				}
				if terminal {
					res.Terminated = true
					break
				}
				if r.StopWhen != nil && r.StopWhen(text.String()) {
					res.Truncated = true
					break
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			res.Text = text.String()
			return res, fmt.Errorf("chat: reading stream: %s", c.safeErr(err))
		}
	}
	res.Text = text.String()
	res.Completed = res.Terminated && res.Error == "" && !res.Truncated && naturalChatCompletion(c.Spec.Transport, res.FinishReason)
	return res, nil
}

// consumeStreamChunk folds one SSE data payload into the result and
// reports whether it was the terminal event.
func (c *Client) consumeStreamChunk(raw []byte, text *strings.Builder, res *ChatResult) (terminal bool) {
	if msg, isErr := topLevelError(raw); isErr {
		res.Error = "endpoint error in stream: " + snippet([]byte(redactErrorText(msg, c.Spec.APIKey)))
		res.FinishReason = "failed"
		return true
	}
	var event struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &event) == nil && event.Type == "error" {
		res.Error = "endpoint error in stream: " + snippet([]byte(redactErrorText(event.Message, c.Spec.APIKey)))
		res.FinishReason = "failed"
		return true
	}
	switch c.Spec.Transport {
	case config.TransportAnthropic:
		var v struct {
			Type  string `json:"type"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Message json.RawMessage `json:"message"`
			Usage   json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		switch v.Type {
		case "content_block_delta":
			if v.Delta.Type == "text_delta" {
				text.WriteString(v.Delta.Text)
			}
		case "message_start":
			if len(v.Message) > 0 {
				if n, ok := parseUsage(c.Spec.Transport, wrapUsage(v.Message)); ok && n > 0 {
					res.PromptTokens, res.HasUsage = n, true
				}
			}
		case "message_delta":
			if v.Delta.StopReason != "" {
				res.FinishReason = v.Delta.StopReason
			}
			if !res.HasUsage && len(v.Usage) > 0 {
				if n, ok := parseUsage(c.Spec.Transport, wrapUsage(v.Usage)); ok && n > 0 {
					res.PromptTokens, res.HasUsage = n, true
				}
			}
		case "message_stop":
			return true
		}
		return false
	case config.TransportGemini:
		text.WriteString(geminiText(raw))
		if n, ok := parseUsage(c.Spec.Transport, raw); ok && n > 0 {
			res.PromptTokens, res.HasUsage = n, true
		}
		if fr := extractFinishReason(c.Spec.Transport, raw); fr != "" {
			res.FinishReason = fr
			return true
		}
		return false
	case config.TransportResponses:
		var v struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		switch v.Type {
		case "response.output_text.delta":
			text.WriteString(v.Delta)
		case "response.completed", "response.incomplete", "response.failed":
			if len(v.Response) > 0 {
				if n, ok := parseUsage(c.Spec.Transport, v.Response); ok && n > 0 {
					res.PromptTokens, res.HasUsage = n, true
				}
			}
			res.FinishReason = strings.TrimPrefix(v.Type, "response.")
			if v.Type == "response.failed" {
				msg, ok := topLevelError(v.Response)
				if !ok {
					msg = "response.failed"
				}
				res.Error = "endpoint error in stream: " + snippet([]byte(redactErrorText(msg, c.Spec.APIKey)))
			}
			return true
		}
		return false
	default: // openai-compatible
		var v struct {
			Choices []struct {
				Delta struct {
					Content json.RawMessage `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &v) == nil {
			for _, ch := range v.Choices {
				text.WriteString(contentText(ch.Delta.Content))
				if ch.FinishReason != nil && *ch.FinishReason != "" {
					res.FinishReason = *ch.FinishReason
				}
			}
		}
		if n, ok := parseUsage(c.Spec.Transport, raw); ok && n > 0 {
			res.PromptTokens, res.HasUsage = n, true
		}
		return false
	}
}

// wrapUsage reshapes a nested streaming usage object (or a
// message_start message) so parseUsage can read it.
func wrapUsage(raw json.RawMessage) []byte {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil {
		if _, has := probe["usage"]; has {
			return raw
		}
	}
	out, _ := json.Marshal(map[string]json.RawMessage{"usage": raw})
	return out
}
