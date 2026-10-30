// Package numprint implements xlfp's behavioral number
// fingerprint: a model writes a few hundred first-instinct integers
// in a fixed range, and the distribution of values it prefers is
// compared against a reference bank collected by xlfp itself. The
// result is a closed-set ranking within that bank — never proof of
// an exact model version. See docs/clean-room.md for how this
// implementation was derived independently.
package numprint

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// Value range of the challenge task (a structural method choice).
const (
	ValueMin = 1
	ValueMax = 355
)

// Status values for a scoring result.
const (
	StatusRanked        = "ranked"
	StatusInconclusive  = "inconclusive"
	StatusNotModelReply = "not_model_reply"
)

// Machine-stable reason codes.
const (
	ReasonInsufficientBank    = "insufficient_bank"
	ReasonInsufficientAnswers = "insufficient_answers"
	ReasonNotModelReply       = "not_model_reply"
)

// Sample is one reference answer stored in the bank.
type Sample struct {
	ID            string `json:"id"`
	Environment   string `json:"environment,omitempty"`
	ExpectedCount int    `json:"expected_count"`
	Sequence      []int  `json:"sequence"`
	CollectedAt   string `json:"collected_at,omitempty"`
	EndpointHost  string `json:"endpoint_host,omitempty"`
}

// Entry is one candidate model in the reference bank.
type Entry struct {
	ID      string   `json:"id"`
	Family  string   `json:"family"`
	Display string   `json:"display"`
	Model   string   `json:"model"`
	Samples []Sample `json:"samples"`
}

// SeqLimits bounds the language-model reply check. Fitted=false
// marks the provisional first-principles defaults: an i.i.d.
// uniform sequence over 1..355 has lag-1 autocorrelation near 0
// and a small-step (|Δ| ≤ 20) share of about 0.109, while model
// replies collected for the bank are expected to sit below both.
// `xlfp retrain` replaces these with bank-fitted quantiles.
type SeqLimits struct {
	Autocorr  float64 `json:"autocorr"`
	SmallStep float64 `json:"small_step"`
	Fitted    bool    `json:"fitted"`
}

func provisionalSeqLimits() SeqLimits {
	return SeqLimits{Autocorr: -0.10, SmallStep: 0.08, Fitted: false}
}

// LDAParams is an optional linear-discriminant projection fitted
// offline (tools/train_numprint.py) over features exported by
// `xlfp retrain --export-features`.
type LDAParams struct {
	// Projection maps a feature vector to LDA space: rows are
	// feature dimensions, columns are discriminant axes.
	Projection [][]float64          `json:"projection"`
	Means      map[string][]float64 `json:"means"` // per entry id
}

// Params holds fitted detector parameters. Temperature <= 0 means
// confidence is uncalibrated: rankings are reported without
// confidence values.
type Params struct {
	SeqLimits   SeqLimits  `json:"seq_limits"`
	Temperature float64    `json:"temperature,omitempty"`
	LDA         *LDAParams `json:"lda,omitempty"`
}

// Bank is the reference bank plus its fitted parameters.
type Bank struct {
	Version string  `json:"version"`
	Note    string  `json:"note,omitempty"`
	Entries []Entry `json:"entries"`
	Params  Params  `json:"params"`
}

//go:embed bank.json
var embeddedBank []byte

// Load parses and validates the embedded reference bank.
func Load() (*Bank, error) { return Parse(embeddedBank) }

// Parse validates a bank decoded from raw JSON.
func Parse(raw []byte) (*Bank, error) {
	var b Bank
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("number bank: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}

func (b *Bank) Validate() error {
	seen := map[string]bool{}
	for _, e := range b.Entries {
		if strings.TrimSpace(e.ID) == "" || seen[e.ID] {
			return fmt.Errorf("number bank: duplicate or empty entry id %q", e.ID)
		}
		seen[e.ID] = true
		if strings.TrimSpace(e.Family) == "" {
			return fmt.Errorf("number bank: entry %s has an empty family", e.ID)
		}
		sampleIDs := map[string]bool{}
		for _, s := range e.Samples {
			if s.ID == "" || sampleIDs[s.ID] {
				return fmt.Errorf("number bank: entry %s has duplicate or empty sample id %q", e.ID, s.ID)
			}
			sampleIDs[s.ID] = true
			if err := ValidateSequence(s.Sequence, s.ExpectedCount); err != nil {
				return fmt.Errorf("number bank: entry %s sample %s: %w", e.ID, s.ID, err)
			}
		}
	}
	if b.Params.LDA != nil {
		for id := range b.Params.LDA.Means {
			if !seen[id] {
				return fmt.Errorf("number bank: LDA mean for unknown entry %q", id)
			}
		}
	}
	return nil
}

// ValidateSequence checks a parsed sequence is usable as evidence.
func ValidateSequence(seq []int, expected int) error {
	if len(seq) < MinValid(expected) {
		return fmt.Errorf("sequence has %d valid integers, need at least %d", len(seq), MinValid(expected))
	}
	for _, v := range seq {
		if v < ValueMin || v > ValueMax {
			return fmt.Errorf("value %d outside %d..%d", v, ValueMin, ValueMax)
		}
	}
	return nil
}

// MinValid is the fewest valid integers an answer must contain to
// be scored: max(80, ceil(0.55 * requested)).
func MinValid(expected int) int {
	m := 80
	if expected > 0 {
		if v := (expected*55 + 99) / 100; v > m {
			m = v
		}
	}
	return m
}

// seqLimits returns the effective reply-check limits.
func (b *Bank) seqLimits() SeqLimits {
	l := b.Params.SeqLimits
	if l.Autocorr == 0 && l.SmallStep == 0 && !l.Fitted {
		return provisionalSeqLimits()
	}
	return l
}

// EligibleEntries returns entries carrying at least one sample.
func (b *Bank) EligibleEntries() []Entry {
	var out []Entry
	for _, e := range b.Entries {
		if len(e.Samples) > 0 {
			out = append(out, e)
		}
	}
	return out
}
