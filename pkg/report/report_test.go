package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestSanitizeURL(t *testing.T) {
	cases := map[string]string{
		"https://user:pass@example.com/v1?key=secret#frag": "https://example.com/v1",
		"https://example.com/v1?api-version=1":             "https://example.com/v1",
		"https:opaque-secret":                              "(unparseable endpoint)",
		"http://127.0.0.1:8080":                            "http://127.0.0.1:8080",
	}
	for in, want := range cases {
		if got := SanitizeURL(in); got != want {
			t.Errorf("SanitizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUncoverEmptyRankingsNoPanic(t *testing.T) {
	var buf bytes.Buffer
	Uncover(&buf, "https://example.com", "m", "openai", fingerprint.Result{Status: fingerprint.StatusIdentified})
	if !strings.Contains(buf.String(), "Endpoint:") {
		t.Fatalf("output = %q", buf.String())
	}
}
