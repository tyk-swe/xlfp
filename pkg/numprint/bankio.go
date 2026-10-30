package numprint

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// LoadBankFile reads a bank JSON file (empty path: embedded bank).
func LoadBankFile(path string) (*Bank, error) {
	if path == "" {
		return Load()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// SaveBankFile writes a bank as indented JSON.
func SaveBankFile(path string, b *Bank) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// BatchSample is one collected sample in a sampling batch. The raw
// text is retained so enrollment validation stays auditable.
type BatchSample struct {
	ID            string `json:"id"`
	Environment   string `json:"environment,omitempty"`
	ExpectedCount int    `json:"expected_count"`
	Text          string `json:"text"`
	Sequence      []int  `json:"sequence"`
}

// BatchFailure records one failed collection attempt. Failures stay
// on record; enrollment never silently drops them.
type BatchFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// Batch is one portable sampling result for a single model,
// collected by `xlfp sample`.
type Batch struct {
	Version      string         `json:"version"`
	EntryID      string         `json:"entry_id"`
	Family       string         `json:"family"`
	Display      string         `json:"display,omitempty"`
	Model        string         `json:"model"`
	EndpointHost string         `json:"endpoint_host"`
	CollectedAt  string         `json:"collected_at"`
	Samples      []BatchSample  `json:"samples"`
	Failures     []BatchFailure `json:"failures,omitempty"`
}

// LoadBatchFile reads and validates a sampling batch.
func LoadBatchFile(path string) (*Batch, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var batch Batch
	if err := json.Unmarshal(raw, &batch); err != nil {
		return nil, fmt.Errorf("sampling batch: %w", err)
	}
	if err := batch.Validate(); err != nil {
		return nil, err
	}
	return &batch, nil
}

// SaveBatchFile writes a batch as indented JSON.
func SaveBatchFile(path string, batch *Batch) error {
	raw, err := json.MarshalIndent(batch, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func (batch *Batch) Validate() error {
	if strings.TrimSpace(batch.EntryID) == "" || strings.TrimSpace(batch.Family) == "" || strings.TrimSpace(batch.Model) == "" {
		return fmt.Errorf("sampling batch: entry_id, family and model are required")
	}
	seen := map[string]bool{}
	for _, s := range batch.Samples {
		if s.ID == "" || seen[s.ID] {
			return fmt.Errorf("sampling batch: duplicate or empty sample id %q", s.ID)
		}
		seen[s.ID] = true
		seq := s.Sequence
		if seq == nil {
			seq = ParseSequence(s.Text)
		}
		if err := ValidateSequence(seq, s.ExpectedCount); err != nil {
			return fmt.Errorf("sampling batch: sample %s: %w", s.ID, err)
		}
		// The stored sequence must match the retained raw text.
		if s.Text != "" && s.Sequence != nil {
			reparsed := ParseSequence(s.Text)
			if len(reparsed) != len(s.Sequence) {
				return fmt.Errorf("sampling batch: sample %s sequence does not match its raw text", s.ID)
			}
			for i := range reparsed {
				if reparsed[i] != s.Sequence[i] {
					return fmt.Errorf("sampling batch: sample %s sequence does not match its raw text", s.ID)
				}
			}
		}
	}
	return nil
}

// Enroll validates a batch and merges it into the bank: the entry
// with the batch's entry id gains the new samples (an existing
// sample id is an error, never an overwrite), or a new entry is
// created. It returns how many samples were added.
func (b *Bank) Enroll(batch *Batch) (int, error) {
	if err := batch.Validate(); err != nil {
		return 0, err
	}
	display := batch.Display
	if display == "" {
		display = batch.Model
	}
	idx := -1
	for i := range b.Entries {
		if b.Entries[i].ID == batch.EntryID {
			idx = i
			break
		}
	}
	if idx < 0 {
		b.Entries = append(b.Entries, Entry{ID: batch.EntryID, Family: batch.Family, Display: display, Model: batch.Model})
		idx = len(b.Entries) - 1
	} else if b.Entries[idx].Family != batch.Family || b.Entries[idx].Model != batch.Model {
		return 0, fmt.Errorf("enroll: entry %s already exists as family %q model %q, batch says %q/%q", batch.EntryID, b.Entries[idx].Family, b.Entries[idx].Model, batch.Family, batch.Model)
	}
	existing := map[string]bool{}
	for _, s := range b.Entries[idx].Samples {
		existing[s.ID] = true
	}
	added := 0
	for _, s := range batch.Samples {
		if existing[s.ID] {
			return added, fmt.Errorf("enroll: sample id %q already enrolled in entry %s", s.ID, batch.EntryID)
		}
		seq := s.Sequence
		if seq == nil {
			seq = ParseSequence(s.Text)
		}
		b.Entries[idx].Samples = append(b.Entries[idx].Samples, Sample{
			ID: s.ID, Environment: s.Environment, ExpectedCount: s.ExpectedCount,
			Sequence: seq, CollectedAt: batch.CollectedAt, EndpointHost: batch.EndpointHost,
		})
		added++
	}
	return added, b.Validate()
}
