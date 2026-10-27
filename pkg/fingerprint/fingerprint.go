// Package fingerprint holds the embedded tokenizer fingerprint database
// and the ranking logic that turns observed prompt-token counts into a
// ranked list of candidate model families (or an inconclusive result).
package fingerprint

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed db.json
var embeddedDB []byte

// Probe is one fingerprint probe string sent to an endpoint.
type Probe struct {
	ID      string `json:"id"`
	Stage   int    `json:"stage"`
	Special bool   `json:"special,omitempty"`
	Text    string `json:"text"`
}

// Entry is one tokenizer variant in the database. Several entries may share
// a Family (e.g. OpenAI publishes multiple encodings).
type Entry struct {
	ID           string   `json:"id"`
	Family       string   `json:"family"`
	Display      string   `json:"display"`
	Tokenizer    string   `json:"tokenizer"`
	Verified     bool     `json:"verified"`
	Source       string   `json:"source"`
	Counts       []int    `json:"counts"` // one raw-text count per probe, in probe order
	RejectProbes []string `json:"reject_probes"`
	Aliases      []string `json:"aliases"`
}

// DB is the embedded fingerprint database.
type DB struct {
	Version string  `json:"version"`
	Note    string  `json:"note"`
	Probes  []Probe `json:"probes"`
	Entries []Entry `json:"entries"`
}

// Load parses and validates the embedded database.
func Load() (*DB, error) {
	var db DB
	if err := json.Unmarshal(embeddedDB, &db); err != nil {
		return nil, fmt.Errorf("embedded fingerprint db: %w", err)
	}
	if err := db.Validate(); err != nil {
		return nil, err
	}
	return &db, nil
}

// Parse validates a database decoded from raw JSON (used by tests/fixtures).
func Parse(raw []byte) (*DB, error) {
	var db DB
	if err := json.Unmarshal(raw, &db); err != nil {
		return nil, err
	}
	if err := db.Validate(); err != nil {
		return nil, err
	}
	return &db, nil
}

func (db *DB) Validate() error {
	if len(db.Probes) == 0 {
		return fmt.Errorf("fingerprint db: no probes")
	}
	seen := map[string]bool{}
	for _, p := range db.Probes {
		if p.ID == "" || seen[p.ID] {
			return fmt.Errorf("fingerprint db: duplicate or empty probe id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Stage < 1 {
			return fmt.Errorf("fingerprint db: probe %s has stage < 1", p.ID)
		}
	}
	for _, e := range db.Entries {
		if len(e.Counts) != len(db.Probes) {
			return fmt.Errorf("fingerprint db: entry %s has %d counts for %d probes", e.ID, len(e.Counts), len(db.Probes))
		}
		for _, c := range e.Counts {
			if c <= 0 {
				return fmt.Errorf("fingerprint db: entry %s has non-positive count", e.ID)
			}
		}
		for _, rp := range e.RejectProbes {
			if !seen[rp] {
				return fmt.Errorf("fingerprint db: entry %s rejects unknown probe %q", e.ID, rp)
			}
		}
	}
	return nil
}

// Families returns canonical family names present in the DB, in DB order.
func (db *DB) Families() []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range db.Entries {
		if !seen[e.Family] {
			seen[e.Family] = true
			out = append(out, e.Family)
		}
	}
	return out
}

// NormalizeFamily maps a user-supplied family name, alias, or model name to
// a canonical family. It returns "" when nothing matches.
func (db *DB) NormalizeFamily(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	for _, e := range db.Entries {
		if s == e.Family {
			return e.Family
		}
		for _, a := range e.Aliases {
			if s == strings.ToLower(a) {
				return e.Family
			}
		}
	}
	return ""
}

// aliasMatches reports whether alias occurs in a model name. Short
// aliases (o1, o3) must sit on a separator boundary so they do not match
// inside unrelated words; longer aliases match as substrings.
func aliasMatches(model, alias string) bool {
	if alias == "" {
		return false
	}
	if len(alias) >= 4 {
		return strings.Contains(model, alias)
	}
	for from := 0; ; {
		i := strings.Index(model[from:], alias)
		if i < 0 {
			return false
		}
		i += from
		before := i == 0 || strings.ContainsRune("-_/. ", rune(model[i-1]))
		end := i + len(alias)
		after := end == len(model) || strings.ContainsRune("-_/. ", rune(model[end]))
		if before && after {
			return true
		}
		from = i + 1
	}
}

// FamilyForModel infers a claimed family from a model name by alias match.
func (db *DB) FamilyForModel(model string) string {
	m := strings.ToLower(model)
	best, bestLen := "", 0
	match := func(alias, fam string) {
		a := strings.ToLower(alias)
		if aliasMatches(m, a) && len(a) > bestLen {
			best, bestLen = fam, len(a)
		}
	}
	for _, e := range db.Entries {
		match(e.Family, e.Family)
		for _, a := range e.Aliases {
			match(a, e.Family)
		}
	}
	return best
}

// IsVerified reports whether any entry of the family is verified.
func (db *DB) IsVerified(family string) bool {
	for _, e := range db.Entries {
		if e.Family == family && e.Verified {
			return true
		}
	}
	return false
}

// SharedTokenizerFamilies groups verified entries whose full expected-count
// vectors are identical; such families cannot be separated by token counts
// (e.g. MiMo-7B ships a byte-identical tokenizer.json to Qwen2.5).
func (e Entry) sameCounts(o Entry) bool {
	if len(e.Counts) != len(o.Counts) {
		return false
	}
	for i := range e.Counts {
		if e.Counts[i] != o.Counts[i] {
			return false
		}
	}
	return true
}
