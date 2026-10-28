package probe

import (
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/xlfp/pkg/config"
)

func TestOpenCodeDirectURLsBlocked(t *testing.T) {
	for _, endpoint := range []string{
		"https://opencode.ai/zen/v1", "https://opencode.ai/inference/openai/v1/chat/completions",
		"https://API.OpenCode.AI.:443/anything", "http://nested.api.opencode.ai/v1",
	} {
		_, err := New(config.EndpointSpec{Endpoint: endpoint, Model: "paid-or-free", Transport: "openai"}, time.Second)
		if err == nil || !strings.Contains(err.Error(), "xlfp opencode") {
			t.Errorf("%s: want OpenCode guidance, got %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"https://opencode.ai.example.com", "https://notopencode.ai", "http://localhost:8000"} {
		if _, err := New(config.EndpointSpec{Endpoint: endpoint, Model: "test", Transport: "openai"}, time.Second); err != nil {
			t.Errorf("unrelated endpoint blocked: %s: %v", endpoint, err)
		}
	}
}
