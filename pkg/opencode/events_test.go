package opencode

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func completedStep(id, tokens string) string {
	return fmt.Sprintf(`{"type":"step_finish","sessionID":"s1","part":{"type":"step-finish","id":%q,"sessionID":"s1","messageID":"m1","tokens":%s}}`, id, tokens)
}

func TestOpenCodeUsageEvents(t *testing.T) {
	max := strconv.FormatUint(uint64(^uint(0)>>1), 10)
	for _, tc := range []struct {
		name, tokens string
		want         int
	}{
		{"input", `{"input":42,"output":999,"reasoning":123}`, 42},
		{"cached", `{"input":2,"cache":{"read":100,"write":3}}`, 105},
		{"all_cached", `{"input":0,"cache":{"read":42,"write":0}}`, 42},
		{"numeric_string", `{"input":"42"}`, 42},
		{"missing", `{}`, 0}, {"null", `null`, 0},
		{"null_input", `{"input":null}`, 0}, {"zero", `{"input":0}`, 0},
		{"negative", `{"input":-1,"cache":{"read":100}}`, 0},
		{"fraction", `{"input":42.5}`, 0}, {"bad_string", `{"input":"42x"}`, 0},
		{"overflow", `{"input":999999999999999999999999}`, 0},
		{"sum_overflow", `{"input":` + max + `,"cache":{"read":1}}`, 0},
		{"negative_cache", `{"input":42,"cache":{"write":-1}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEvents([]byte(completedStep("f1", tc.tokens)))
			if (err != nil) != (tc.want == 0) || got != tc.want {
				t.Fatalf("count %d, err %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestOpenCodeRejectsAmbiguousOrFailedEvents(t *testing.T) {
	good := completedStep("f1", `{"input":42}`)
	start := `{"type":"step_start","sessionID":"s1","part":{"type":"step-start","id":"b1","sessionID":"s1","messageID":"m1"}}`
	for _, raw := range []string{
		"", "not json", `{"type":"text","part":{"text":"42 tokens"}}`,
		good + "\n" + completedStep("f2", `{"input":42}`),
		good + "\n" + completedStep("f1", `{"input":43}`),
		start + "\n" + strings.ReplaceAll(start, "b1", "b2") + "\n" + good,
		good + "\n" + strings.ReplaceAll(good, "s1", "s2"),
		good + "\n" + strings.ReplaceAll(good, "m1", "m2"),
		strings.Replace(good, `"sessionID":"s1"`, `"sessionID":"wrong"`, 1),
		strings.Replace(good, `"id":"f1"`, `"id":""`, 1),
		good + "\n" + `{"type":"tool_use"}`,
		strings.Replace(good, `"tokens":`, `"reason":"tool-calls","tokens":`, 1),
		good + "\n" + start,
		good + "\n" + `{"type":"error","error":{"message":"sk-test-secret"}}`,
		good + "\n" + "unstructured secret diagnostics",
	} {
		_, err := parseEvents([]byte(raw))
		if err == nil || strings.Contains(err.Error(), "sk-test-secret") || strings.Contains(err.Error(), "unstructured secret") {
			t.Errorf("expected safe rejection for %q: %v", raw, err)
		}
	}
	for _, raw := range []string{good, start + "\n" + good, good + "\n" + good} {
		if got, err := parseEvents([]byte(raw)); err != nil || got != 42 {
			t.Errorf("valid or duplicate event rejected: %d, %v", got, err)
		}
	}
}
