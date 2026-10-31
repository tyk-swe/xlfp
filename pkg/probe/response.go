package probe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxResponseBytes = 4 << 20

// readResponseBody bounds the body before removing a BOM, so the limit
// applies to every byte read. The bounded body is retained for retry decisions.
func readResponseBody(body io.Reader) ([]byte, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, false, err
	}
	truncated := len(raw) > maxResponseBytes
	if truncated {
		raw = raw[:maxResponseBytes]
	}
	return bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}), truncated, nil
}

func (c *Client) httpError(status int, raw []byte, truncated bool) string {
	switch {
	case truncated:
		// Truncation can make JSON undecodable and prevent safe redaction.
		return fmt.Sprintf("HTTP %d: response exceeded size limit", status)
	case c.Spec.APIKey != "" && !json.Valid(raw):
		// Malformed JSON can conceal an escaped key from literal replacement.
		return fmt.Sprintf("HTTP %d: non-JSON error body omitted", status)
	default:
		return fmt.Sprintf("HTTP %d: %s", status, c.errorSnippet(string(raw)))
	}
}

// Redact before truncating: an API key can span the snippet boundary.
func (c *Client) errorSnippet(s string) string {
	return snippet([]byte(redactErrorText(s, c.Spec.APIKey)))
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
