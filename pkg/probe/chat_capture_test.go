package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/config"
)

func TestChatRedactsCapturedReplies(t *testing.T) {
	const key = "sk-test/echo-123"
	const reply = "37 " + key + " 173"
	for _, tc := range []struct {
		name, transport, body string
		stream, earlyStop     bool
	}{
		{"openai", config.TransportOpenAI, `{"choices":[{"message":{"content":"37 sk-test/echo-123 173"},"finish_reason":"stop"}]}`, false, false},
		{"anthropic", config.TransportAnthropic, `{"content":[{"type":"text","text":"37 sk-test/echo-123 173"}],"stop_reason":"end_turn"}`, false, false},
		{"gemini", config.TransportGemini, `{"candidates":[{"content":{"parts":[{"text":"37 sk-test/echo-123 173"}]},"finishReason":"STOP"}]}`, false, false},
		{"responses", config.TransportResponses, `{"status":"completed","output_text":"37 sk-test/echo-123 173"}`, false, false},
		{"stream", config.TransportOpenAI, "", true, false},
		{"early_stop", config.TransportOpenAI, "", true, true},
		{"stream_error", config.TransportOpenAI, "data: {\"error\":{\"message\":\"failed\"}}\n\n", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := chatClient(t, tc.transport, func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					// Split the key across events to require redacting assembled text.
					for _, delta := range []string{"37 sk-test/", "echo-123 173"} {
						raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": delta}}}})
						fmt.Fprintf(w, "data: %s\n\n", raw)
					}
					fmt.Fprint(w, tc.body)
					fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, tc.body)
				}
			})
			c.Spec.APIKey = key
			req := ChatRequest{User: "numbers", Stream: tc.stream}
			if tc.earlyStop {
				req.StopWhen = func(text string) bool { return text == reply }
			}
			res, err := c.Chat(context.Background(), req)
			if err != nil || res.Text != "37 [redacted] 173" || strings.Contains(res.Text, key) {
				t.Fatalf("captured reply was not safely retained: %+v, %v", res, err)
			}
		})
	}
}
