package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestSampleJSON(t *testing.T) {
	cleanEnv(t)
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !success {
					fmt.Fprint(w, `{"error":{"message":"collection failed"}}`)
					return
				}
				text := strings.Repeat("37 173 ", 160)
				raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": text}, "finish_reason": "stop"}}})
				_, _ = w.Write(raw)
			}))
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "batch.json")
			code, stdout, stderr := runCLI(t, "sample", "--endpoint", srv.URL, "--model", "m", "--family", "f", "--out", path, "--no-stream", "--json")
			wantCode := exitError
			if success {
				wantCode = exitOK
			}
			if code != wantCode {
				t.Fatalf("sample exit %d: %s", code, stderr)
			}
			var got numprint.Batch
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("invalid JSON stdout: %v: %q", err, stdout)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved numprint.Batch
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			savedJSON, _ := json.Marshal(saved)
			if !bytes.Equal(gotJSON, savedJSON) || len(got.Samples)+len(got.Failures) != len(numprint.SamplingSet()) {
				t.Fatalf("stdout differs from saved collection: %+v", got)
			}
		})
	}
}

func TestSampleIndependentBatchesEnrollAndResume(t *testing.T) {
	cleanEnv(t)
	var calls atomic.Int64
	text := strings.Repeat("37 173 ", 160)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"content": text}, "finish_reason": "stop",
		}}})
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	bank, err := numprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	n := len(numprint.SamplingSet())
	for i := 0; i < 2; i++ {
		path := filepath.Join(t.TempDir(), "batch.json")
		args := []string{"sample", "--endpoint", srv.URL, "--model", "m", "--family", "f", "--out", path, "--no-stream"}
		if code, _, stderr := runCLI(t, args...); code != exitOK {
			t.Fatalf("batch %d: exit %d: %s", i, code, stderr)
		}
		batch, err := numprint.LoadBatchFile(path)
		if err != nil || len(batch.Samples) != n {
			t.Fatalf("batch %d: %+v, %v", i, batch, err)
		}
		// Resume must keep identities stable and collect only the missing challenge.
		missingID := batch.Samples[n-1].ID
		batch.Samples = batch.Samples[:n-1]
		if err := numprint.SaveBatchFile(path, batch); err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		if code, _, stderr := runCLI(t, append(args, "--resume")...); code != exitOK {
			t.Fatalf("resume batch %d: exit %d: %s", i, code, stderr)
		}
		batch, err = numprint.LoadBatchFile(path)
		if err != nil || len(batch.Samples) != n || batch.Samples[n-1].ID != missingID || calls.Load() != before+1 {
			t.Fatalf("resume changed sample identities or repeated completed requests: %+v, %v", batch, err)
		}
		if added, err := bank.Enroll(batch); err != nil || added != n {
			t.Fatalf("enroll independent batch %d: added %d: %v", i, added, err)
		}
		if added, err := bank.Enroll(batch); err == nil || added != 0 {
			t.Fatalf("re-enrollment accepted batch %d: added %d: %v", i, added, err)
		}
	}
	if len(bank.Entries) != 1 || len(bank.Entries[0].Samples) != 2*n {
		t.Fatalf("independent samples were lost: %+v", bank)
	}
}

func TestSampleResumeChecksEndpointHost(t *testing.T) {
	cleanEnv(t)
	for _, host := range []string{"other.example.com", "", "same"} {
		t.Run(host, func(t *testing.T) {
			var calls atomic.Int64
			text := strings.TrimSpace(strings.Repeat("37 173 ", 150))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
					"message": map[string]any{"content": text}, "finish_reason": "stop",
				}}})
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			batch := &numprint.Batch{Version: "1", EntryID: "model-a", Family: "fam-a", Model: "model-a", EndpointHost: host}
			if host == "same" {
				batch.EndpointHost = endpointHost(srv.URL)
			}
			set := numprint.SamplingSet()
			for _, sc := range set[:len(set)-1] {
				batch.Samples = append(batch.Samples, numprint.BatchSample{
					ID: sc.ID, ExpectedCount: sc.Challenge.ExpectedCount, Text: text, Sequence: numprint.ParseSequence(text),
				})
			}
			path := filepath.Join(t.TempDir(), "batch.json")
			if err := numprint.SaveBatchFile(path, batch); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			code, _, stderr := runCLI(t, "sample", "--endpoint", srv.URL, "--model", "model-a", "--family", "fam-a", "--out", path, "--resume", "--no-stream")
			if host == "same" {
				if code != exitOK || calls.Load() != 1 {
					t.Fatalf("same-host resume: exit=%d requests=%d stderr=%s", code, calls.Load(), stderr)
				}
				return
			}
			if code != exitError || calls.Load() != 0 || !strings.Contains(stderr, "--resume") {
				t.Fatalf("mismatched-host resume: exit=%d requests=%d stderr=%s", code, calls.Load(), stderr)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected resume changed the batch: %v", err)
			}
		})
	}
}
