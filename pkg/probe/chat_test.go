package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/numprint"
)

func chatClient(t *testing.T, transport string, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m1", Transport: transport}, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestChatOpenAINonStream(t *testing.T) {
	c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"3 1 4"},"finish_reason":"stop"}],"usage":{"prompt_tokens":42}}`)
	})
	res, err := c.Chat(context.Background(), ChatRequest{User: "numbers please"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Text != "3 1 4" || !res.HasUsage || res.PromptTokens != 42 || !res.Completed || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if c.Requests() != 1 {
		t.Fatalf("Requests = %d, want 1", c.Requests())
	}
}

func TestChatGeminiSkipsThoughtParts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			thought := `{"thought":true,"text":"10 20 30 40 "}`
			answer := `{"thought":false,"text":"173 "},{"text":"286 355"}`
			c := chatClient(t, config.TransportGemini, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"parts\":[%s]}}]}\n\n", thought)
				} else {
					answer = thought + "," + answer
				}
				raw := fmt.Sprintf(`{"candidates":[{"content":{"parts":[%s]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":17}}`, answer)
				if stream {
					fmt.Fprintf(w, "data: %s\n\n", raw)
				} else {
					fmt.Fprint(w, raw)
				}
			})
			res, err := c.Chat(context.Background(), ChatRequest{User: "numbers", Stream: stream,
				StopWhen: func(text string) bool { return numprint.CountIntegers(text) >= 3 },
			})
			if err != nil || res.Error != "" || res.Text != "173 286 355" || res.Truncated || !res.Completed || res.PromptTokens != 17 {
				t.Fatalf("thought text contaminated captured reply: %+v, %v", res, err)
			}
		})
	}
}

func TestChatOpenAIStreamEarlyStop(t *testing.T) {
	c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"1 2 \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"3 4 \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	res, err := c.Chat(context.Background(), ChatRequest{
		User: "x", Stream: true,
		StopWhen: func(text string) bool { return strings.Contains(text, "3 4") },
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !res.Truncated || res.Completed || res.Text != "1 2 3 4 " {
		t.Fatalf("unexpected early-stop result: %+v", res)
	}
}

func TestChatOpenAIStreamUsage(t *testing.T) {
	c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"7 \"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	res, err := c.Chat(context.Background(), ChatRequest{User: "x", Stream: true})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Text != "7 " || res.PromptTokens != 11 || !res.Completed || res.FinishReason != "stop" {
		t.Fatalf("unexpected stream result: %+v", res)
	}
}

func TestChatStreamMultilineEvents(t *testing.T) {
	for _, tc := range []struct {
		transport string
		events    []string
	}{
		{config.TransportOpenAI, []string{
			`{"choices":[{"delta":{"content":"37 173"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17}}`,
			`[DONE]`,
		}},
		{config.TransportAnthropic, []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":17}}}`,
			`{"type":"content_block_delta","delta":{"type":"text_delta","text":"37 173"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			`{"type":"message_stop"}`,
		}},
		{config.TransportGemini, []string{
			`{"candidates":[{"content":{"parts":[{"text":"37 173"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":17}}`,
		}},
		{config.TransportResponses, []string{
			`{"type":"response.output_text.delta","delta":"37 173"}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":17}}}`,
		}},
	} {
		for _, newline := range []string{"\n", "\r\n"} {
			t.Run(tc.transport+fmt.Sprintf("/%q", newline), func(t *testing.T) {
				c := chatClient(t, tc.transport, func(w http.ResponseWriter, r *http.Request) {
					for _, event := range tc.events {
						var formatted bytes.Buffer
						if event == "[DONE]" {
							formatted.WriteString(event)
						} else if err := json.Indent(&formatted, []byte(event), "", "  "); err != nil {
							t.Error(err)
						}
						fmt.Fprint(w, "event: message"+newline)
						for i, line := range strings.Split(formatted.String(), "\n") {
							prefix := "data:"
							if i%2 == 0 {
								prefix += " "
							}
							fmt.Fprint(w, prefix+line+newline+": keepalive"+newline)
						}
						fmt.Fprint(w, newline)
					}
				})
				res, err := c.Chat(context.Background(), ChatRequest{User: "numbers", Stream: true})
				if err != nil || res.Error != "" || res.Text != "37 173" || !res.Completed || !res.HasUsage || res.PromptTokens != 17 {
					t.Fatalf("multiline SSE lost evidence: %+v, %v", res, err)
				}
			})
		}
	}
}

func TestChatStreamRequiresEventDelimiter(t *testing.T) {
	c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"1 2\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n")
	})
	res, err := c.Chat(context.Background(), ChatRequest{User: "numbers", Stream: true})
	if err != nil || res.Text != "1 2" || res.Completed || res.Terminated {
		t.Fatalf("unterminated SSE event completed reply: %+v, %v", res, err)
	}
}

func TestChatAnthropicStream(t *testing.T) {
	c := chatClient(t, config.TransportAnthropic, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") == "" {
			t.Error("missing anthropic-version header")
		}
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"cache_read_input_tokens\":5}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"5 6\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	})
	res, err := c.Chat(context.Background(), ChatRequest{User: "x", Stream: true})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Text != "5 6" || res.PromptTokens != 25 || !res.Completed {
		t.Fatalf("unexpected anthropic stream result: %+v", res)
	}
}

func TestChatResponsesNonStreamAndURL(t *testing.T) {
	c := chatClient(t, config.TransportResponses, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("path = %s, want /responses", r.URL.Path)
		}
		var req struct {
			MaxOutputTokens int `json:"max_output_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.MaxOutputTokens > 0 && req.MaxOutputTokens < 16 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"max_output_tokens must be at least 16"}}`)
			return
		}
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"9 8"}]}],"usage":{"input_tokens":33}}`)
	})
	res, err := c.Chat(context.Background(), ChatRequest{User: "x", Effort: "low", ServiceTier: "flex"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Text != "9 8" || res.PromptTokens != 33 || !res.Completed {
		t.Fatalf("unexpected responses result: %+v", res)
	}
	// Token-count probes over the responses transport use input_tokens.
	obs, err := c.Send(context.Background(), fingerprint.Probe{ID: "p", Text: "hello"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !obs.HasUsage || obs.PromptTokens != 33 {
		t.Fatalf("unexpected responses usage observation: %+v", obs)
	}
}

func TestChatNaturalCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, transport, body string
		stream, completed     bool
	}{
		{"openai_length", config.TransportOpenAI, `{"choices":[{"message":{"content":"1 2"},"finish_reason":"length"}]}`, false, false},
		{"openai_length_stream", config.TransportOpenAI, "data: {\"choices\":[{\"delta\":{\"content\":\"1 2\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", true, false},
		{"openai_unknown_stream", config.TransportOpenAI, "data: [DONE]\n\n", true, false},
		{"openai_missing_terminal", config.TransportOpenAI, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n", true, false},
		{"anthropic_max_tokens", config.TransportAnthropic, `{"content":[{"type":"text","text":"1 2"}],"stop_reason":"max_tokens"}`, false, false},
		{"anthropic_max_tokens_stream", config.TransportAnthropic, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", true, false},
		{"responses_incomplete", config.TransportResponses, `{"status":"incomplete","output_text":"1 2"}`, false, false},
		{"responses_incomplete_stream", config.TransportResponses, "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n", true, false},
		{"responses_completed_stream", config.TransportResponses, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", true, true},
		{"gemini_max_tokens", config.TransportGemini, `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, false, false},
		{"gemini_max_tokens_stream", config.TransportGemini, "data: {\"candidates\":[{\"finishReason\":\"MAX_TOKENS\"}]}\n\n", true, false},
		{"gemini_stop_stream", config.TransportGemini, "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := chatClient(t, tc.transport, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
			res, err := c.Chat(context.Background(), ChatRequest{User: "x", Stream: tc.stream})
			if err != nil || res.Error != "" || res.Completed != tc.completed {
				t.Fatalf("Chat = %+v, %v; want completed=%v", res, err, tc.completed)
			}
			if tc.name != "openai_missing_terminal" && !res.Terminated {
				t.Fatalf("transport termination lost: %+v", res)
			}
		})
	}
}

func TestChatStreamProviderErrors(t *testing.T) {
	for _, tc := range []struct{ name, transport, partial, failure string }{
		{"responses_failed", config.TransportResponses, `{"type":"response.output_text.delta","delta":"1 2 3"}`, `{"type":"response.failed","response":{"error":{"message":"failure sk-test-secret"}}}`},
		{"responses_failed_without_detail", config.TransportResponses, `{"type":"response.output_text.delta","delta":"1 2 3"}`, `{"type":"response.failed"}`},
		{"responses_error", config.TransportResponses, `{"type":"response.output_text.delta","delta":"1 2 3"}`, `{"type":"error","message":"failure sk-test-secret"}`},
		{"anthropic_error", config.TransportAnthropic, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"1 2 3"}}`, `{"type":"error","error":{"type":"overloaded_error","message":"failure sk-test-secret"}}`},
		{"openai_error", config.TransportOpenAI, `{"choices":[{"delta":{"content":"1 2 3"}}]}`, `{"error":{"message":"failure sk-test-secret"}}`},
		{"gemini_error", config.TransportGemini, `{"candidates":[{"content":{"parts":[{"text":"1 2 3"}]}}]}`, `{"error":{"message":"failure sk-test-secret"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := chatClient(t, tc.transport, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", tc.partial, tc.failure)
			})
			c.Spec.APIKey = "sk-test-secret"
			res, err := c.Chat(context.Background(), ChatRequest{User: "x", Stream: true})
			if err != nil || res.Error == "" || strings.Contains(res.Error, c.Spec.APIKey) || res.Completed || res.Text != "1 2 3" {
				t.Fatalf("failed stream = %+v, %v", res, err)
			}
		})
	}
}

func TestChatStreamRawLimit(t *testing.T) {
	for _, body := range []string{
		":" + strings.Repeat("x", 5<<20),
		strings.Repeat(":"+strings.Repeat("x", 1022)+"\n", 5120),
		strings.Repeat("data: "+strings.Repeat("x", 1022)+"\n", 5120),
	} {
		c := &Client{Spec: config.EndpointSpec{Transport: config.TransportOpenAI}}
		reader := &io.LimitedReader{R: strings.NewReader(body), N: int64(len(body))}
		res, err := c.readChatStream(context.Background(), reader, ChatRequest{}, ChatResult{})
		if err != nil || res.Error == "" || res.Completed {
			t.Fatalf("oversized stream = %+v, %v", res, err)
		}
		if consumed := int64(len(body)) - reader.N; consumed > maxResponseBytes+1 {
			t.Fatalf("read %d raw bytes, limit is %d", consumed, maxResponseBytes+1)
		}
	}
}

func TestRunNumbersRejectsLimitedAndFailedStreams(t *testing.T) {
	sequence := strings.TrimSpace(strings.Repeat("37 173 ", 150))
	seq := numprint.ParseSequence(sequence)
	bank := &numprint.Bank{Version: "test", Entries: []numprint.Entry{
		{ID: "a", Samples: []numprint.Sample{{Sequence: seq}}},
		{ID: "b", Samples: []numprint.Sample{{Sequence: seq}}},
	}}
	for _, strict := range []bool{false, true} {
		c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
			text := sequence
			if !strict {
				// Enough integers for MinValid, fewer than relaxed early stop.
				text = strings.TrimSpace(strings.Repeat("37 173 ", 90))
			}
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			if strict {
				fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
			} else {
				fmt.Fprint(w, "data: {\"error\":{\"message\":\"provider failed\"}}\n\n")
			}
		})
		result := RunNumbers(context.Background(), c, bank, NumOptions{Count: 1, Stream: true, Strict: strict,
			Challenges: []numprint.Challenge{{ID: "test", ExpectedCount: 300, Prompt: "numbers"}},
		})
		if result.Rounds[0].Result.ValidAnswers != 0 || result.Rounds[0].Answers[0].Completed {
			t.Fatalf("strict=%v accepted invalid stream: %+v", strict, result)
		}
		if !strict && result.Rounds[0].Answers[0].Error == "" {
			t.Fatalf("failed sample lost error: %+v", result)
		}
	}
}

func TestChatHTTPErrorRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"bad key sk-secretvalue"}}`)
	}))
	defer srv.Close()
	c, err := New(config.EndpointSpec{Endpoint: srv.URL, Model: "m1", Transport: config.TransportOpenAI, APIKey: "sk-secretvalue"}, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := c.Chat(context.Background(), ChatRequest{User: "x"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.HTTPStatus != 401 || res.Error == "" || strings.Contains(res.Error, "sk-secretvalue") {
		t.Fatalf("error not redacted: %+v", res)
	}
}

func TestRunNumbersWaitsForFinalInteger(t *testing.T) {
	for _, delimiter := range []string{"", " "} {
		t.Run(fmt.Sprintf("delimiter=%q", delimiter), func(t *testing.T) {
			c := chatClient(t, config.TransportOpenAI, func(w http.ResponseWriter, r *http.Request) {
				for _, delta := range []string{"10 20 3", "55", delimiter} {
					chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": delta}}}})
					fmt.Fprintf(w, "data: %s\n\n", chunk)
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			})
			result := RunNumbers(context.Background(), c, &numprint.Bank{}, NumOptions{Count: 1, Stream: true,
				Challenges: []numprint.Challenge{{ID: "test", ExpectedCount: 3, Prompt: "numbers"}},
			})
			answer := result.Rounds[0].Answers[0]
			seq := numprint.ParseSequence(answer.Text)
			if answer.Text != "10 20 355"+delimiter || len(seq) != 3 || seq[2] != 355 {
				t.Fatalf("split integer corrupted: answer=%+v sequence=%v", answer, seq)
			}
			if answer.Completed != (delimiter == "") || answer.Truncated != (delimiter != "") {
				t.Fatalf("unexpected completion metadata: %+v", answer)
			}
		})
	}
}
