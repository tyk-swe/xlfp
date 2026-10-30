package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/numprint"
)

func TestOfflineRoundsNumericReplies(t *testing.T) {
	for _, text := range []string{"[37, 173, 355]", "[ 37,\n173, 355 ]", "[37 173 355]"} {
		rounds, err := offlineRounds([]byte(text))
		if err != nil || len(rounds) != 1 || len(rounds[0].Answers) != 1 {
			t.Fatalf("pasted numeric reply %q: %+v, %v", text, rounds, err)
		}
		answer := rounds[0].Answers[0]
		if answer.Text != text || !answer.Completed || len(numprint.ParseSequence(answer.Text)) != 3 {
			t.Fatalf("pasted reply was changed: %+v", answer)
		}
	}
	if _, err := offlineRounds([]byte(`[{"text":`)); err == nil {
		t.Fatal("malformed structured answers were accepted as pasted text")
	}
}

func TestOfflineRoundsCompletionMetadata(t *testing.T) {
	text := strings.TrimSpace(strings.Repeat("37 173 ", 150))
	seq := numprint.ParseSequence(text)
	bank := &numprint.Bank{Entries: []numprint.Entry{
		{ID: "a", Samples: []numprint.Sample{{Sequence: seq}}},
		{ID: "b", Samples: []numprint.Sample{{Sequence: seq}}},
	}}
	for _, flags := range []map[string]bool{nil, {"completed": true}, {"truncated": true}, {"completed": true, "truncated": true}} {
		answer := map[string]any{"text": text, "expected_count": 300}
		for name, value := range flags {
			answer[name] = value
		}
		for _, doc := range []any{
			[]any{answer}, map[string]any{"answers": []any{answer}},
			map[string]any{"rounds": []any{map[string]any{"answers": []any{answer}}}},
			map[string]any{"samples": []any{answer}},
		} {
			raw, _ := json.Marshal(doc)
			rounds, err := offlineRounds(raw)
			if err != nil || len(rounds) != 1 || len(rounds[0].Answers) != 1 {
				t.Fatalf("offline rounds: %+v, %v", rounds, err)
			}
			got := rounds[0].Answers[0]
			if got.Completed != flags["completed"] || got.Truncated != flags["truncated"] {
				t.Fatalf("replay changed flags %v: %+v", flags, got)
			}
			wantValid := 0
			if flags["completed"] && !flags["truncated"] {
				wantValid = 1
			}
			if result := bank.Score(rounds[0].Answers, numprint.ScoreOptions{Strict: true}); result.ValidAnswers != wantValid {
				t.Fatalf("strict replay accepted incomplete evidence: %+v", result)
			}
		}
	}
}

func TestSamplePersistsCompletion(t *testing.T) {
	cleanEnv(t)
	for _, mode := range []string{"stream", "stop", "length"} {
		t.Run(mode, func(t *testing.T) {
			text := strings.Repeat("37 173 ", 160)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "stream" {
					raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
				} else {
					raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": text}, "finish_reason": mode}}})
					_, _ = w.Write(raw)
				}
			}))
			defer srv.Close()
			set := numprint.SamplingSet()
			batch := &numprint.Batch{Version: "1", EntryID: "m", Family: "f", Model: "m", EndpointHost: endpointHost(srv.URL)}
			for _, sc := range set[:len(set)-1] {
				batch.Samples = append(batch.Samples, numprint.BatchSample{ID: sc.ID, ExpectedCount: sc.Challenge.ExpectedCount, Text: text})
			}
			path := filepath.Join(t.TempDir(), "batch.json")
			if err := numprint.SaveBatchFile(path, batch); err != nil {
				t.Fatal(err)
			}
			args := []string{"sample", "--endpoint", srv.URL, "--model", "m", "--family", "f", "--out", path, "--resume"}
			if mode != "stream" {
				args = append(args, "--no-stream")
			}
			if code, _, stderr := runCLI(t, args...); code != exitOK {
				t.Fatalf("sample exit %d: %s", code, stderr)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := offlineRounds(raw)
			if err != nil {
				t.Fatal(err)
			}
			answers := rounds[len(rounds)-1].Answers
			answer := answers[len(answers)-1]
			if answer.Completed != (mode == "stop") || answer.Truncated != (mode == "stream") {
				t.Fatalf("sample lost completion metadata in %s: %+v", mode, answer)
			}
		})
	}
}
