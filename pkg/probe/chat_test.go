package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
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
