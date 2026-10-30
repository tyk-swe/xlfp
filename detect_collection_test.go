package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/numprint"
)

func TestSampleResumeCollectionSettings(t *testing.T) {
	cleanEnv(t)
	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"match", nil}, {"legacy", nil}, {"effort", []string{"--effort", "low"}},
		{"tier", []string{"--service-tier", "priority"}}, {"stream", []string{"--no-stream=false"}},
		{"transport", []string{"--transport", "responses"}}, {"timeout", []string{"--timeout", "18s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			text := strings.Repeat("37 173 ", 160)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": text}, "finish_reason": "stop"}}})
				_, _ = w.Write(raw)
			}))
			defer srv.Close()
			batch := numprint.Batch{Version: "1", EntryID: "m", Model: "m", Family: "f", EndpointHost: endpointHost(srv.URL)}
			set := numprint.SamplingSet()
			for _, sc := range set[:len(set)-1] {
				batch.Samples = append(batch.Samples, numprint.BatchSample{ID: sc.ID, ExpectedCount: sc.Challenge.ExpectedCount, Text: text})
			}
			raw, _ := json.Marshal(batch)
			var doc map[string]any
			_ = json.Unmarshal(raw, &doc)
			if tc.name != "legacy" {
				doc["collection"] = map[string]any{"transport": "openai", "stream": false, "effort": "high", "service_tier": "auto", "timeout": "17s"}
			}
			before, _ := json.Marshal(doc)
			path := filepath.Join(t.TempDir(), "batch.json")
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"sample", "--endpoint", srv.URL, "--model", "m", "--family", "f", "--out", path, "--resume", "--no-stream", "--effort", "high", "--service-tier", "auto", "--timeout", "17s"}
			code, _, stderr := runCLI(t, append(args, tc.flags...)...)
			if tc.name == "match" {
				if code != exitOK || calls.Load() != 1 {
					t.Fatalf("compatible resume: exit=%d calls=%d: %s", code, calls.Load(), stderr)
				}
				return
			}
			after, err := os.ReadFile(path)
			if code != exitError || calls.Load() != 0 || err != nil || !bytes.Equal(before, after) {
				t.Fatalf("incompatible resume: exit=%d calls=%d changed=%v: %s", code, calls.Load(), !bytes.Equal(before, after), stderr)
			}
		})
	}
}

func TestSampleCollectionSettingsPersist(t *testing.T) {
	cleanEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": strings.Repeat("37 173 ", 160)}, "finish_reason": "stop"}}})
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "batch.json")
	if code, _, stderr := runCLI(t, "sample", "--endpoint", srv.URL, "--model", "m", "--family", "f", "--out", path, "--no-stream", "--effort", "high", "--service-tier", "auto", "--timeout", "17s"); code != exitOK {
		t.Fatalf("sample exit=%d: %s", code, stderr)
	}
	batch, err := numprint.LoadBatchFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bank, _ := numprint.Load()
	if _, err := bank.Enroll(batch); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"transport": "openai", "stream": false, "effort": "high", "service_tier": "auto", "timeout": "17s"}
	for _, obj := range []any{batch, bank.Entries[0].Samples[0]} {
		raw, _ := json.Marshal(obj)
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		if !reflect.DeepEqual(doc["collection"], want) {
			t.Errorf("%T lost collection settings: %v", obj, doc["collection"])
		}
	}
}
