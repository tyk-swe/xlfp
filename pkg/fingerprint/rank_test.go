package fingerprint

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func loadDB(t *testing.T) *DB {
	t.Helper()
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func entryByID(t *testing.T, db *DB, id string) Entry {
	t.Helper()
	for _, e := range db.Entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("entry %s not found", id)
	return Entry{}
}

// observationsFor builds a full observation set from an entry's expected
// counts plus a constant chat-template overhead.
func observationsFor(db *DB, e Entry, overhead int) []Observation {
	obs := make([]Observation, len(db.Probes))
	for i, p := range db.Probes {
		obs[i] = Observation{ProbeID: p.ID, PromptTokens: e.Counts[i] + overhead, HasUsage: true, HTTPStatus: 200}
	}
	return obs
}

func TestDBLoadAndFamilies(t *testing.T) {
	db := loadDB(t)
	want := map[string]bool{"openai": true, "qwen": true, "deepseek": true, "mistral": true, "glm": true, "mimo": true, "kimi": true, "google": true, "claude": true}
	for _, f := range db.Families() {
		delete(want, f)
	}
	if len(want) != 0 {
		t.Fatalf("missing families: %v", want)
	}
	if db.IsVerified("google") || db.IsVerified("claude") {
		t.Fatal("google/claude must be unverified in the shipped DB (closed server tokenizers)")
	}
	if !db.IsVerified("deepseek") {
		t.Fatal("deepseek must be verified")
	}
}

// TestDBSpotCounts pins a few generating-tokenizer counts (regression
// fixture). If these change, tools/gen_db.py or a tokenizer source
// changed and the DB regeneration must be reviewed deliberately.
func TestDBSpotCounts(t *testing.T) {
	db := loadDB(t)
	idx := map[string]int{}
	for i, p := range db.Probes {
		idx[p.ID] = i
	}
	checks := []struct {
		entry, probe string
		want         int
	}{
		{"openai-o200k", "en_short", 2},
		{"openai-o200k", "cjk_zh", 20},
		{"openai-cl100k", "cjk_zh", 36},
		{"qwen25", "numbers", 54},
		{"deepseek-v3", "en_pangram", 20},
		{"glm4", "emoji_zwj", 35},
		{"mistral-v3", "whitespace", 13},
		{"qwen25", "special_a", 5},
	}
	for _, c := range checks {
		e := entryByID(t, db, c.entry)
		if got := e.Counts[idx[c.probe]]; got != c.want {
			t.Errorf("%s %s = %d, want %d", c.entry, c.probe, got, c.want)
		}
	}
	// Qwen, MiMo, and Kimi-Dev ship a byte-identical tokenizer.json.
	q, m := entryByID(t, db, "qwen25"), entryByID(t, db, "mimo-7b")
	if !q.sameCounts(m) {
		t.Error("qwen/mimo count vectors diverged; re-verify the shared-tokenizer assumption")
	}
}

func TestKimiTokenizerCountsAndAliases(t *testing.T) {
	db := loadDB(t)
	if !db.IsVerified("kimi") {
		t.Fatal("kimi must be source-verified")
	}
	k2 := entryByID(t, db, "kimi-k2")
	want := []int{20, 13, 26, 26, 20, 28, 10, 24, 8, 2, 14, 18}
	if !slices.Equal(k2.Counts, want) {
		t.Fatalf("Kimi K2 counts = %v, want %v", k2.Counts, want)
	}
	dev := entryByID(t, db, "kimi-dev")
	for _, id := range []string{"qwen25", "mimo-7b"} {
		if !dev.sameCounts(entryByID(t, db, id)) {
			t.Errorf("Kimi-Dev counts differ from %s", id)
		}
	}
	for _, alias := range []string{"kimi", "KIMI", "moonshot", "Moonshot"} {
		if got := db.NormalizeFamily(alias); got != "kimi" {
			t.Errorf("NormalizeFamily(%q) = %q, want kimi", alias, got)
		}
	}
	for _, model := range []string{"Kimi-K2-Instruct", "moonshotai/Kimi-K3", "Kimi-Dev-72B", "moonshot-v1-8k"} {
		if got := db.FamilyForModel(model); got != "kimi" {
			t.Errorf("FamilyForModel(%q) = %q, want kimi", model, got)
		}
	}
}

func TestRankKimiAndDeepSeekWithOverhead(t *testing.T) {
	db := loadDB(t)
	for _, c := range []struct{ id, family string }{{"kimi-k2", "kimi"}, {"deepseek-v3", "deepseek"}} {
		obs := observationsFor(db, entryByID(t, db, c.id), 7)
		for _, n := range []int{8, len(obs)} {
			res := db.Rank(obs[:n], 0)
			if res.Status != StatusIdentified || res.TopFamily != c.family || !res.ShouldStop() {
				t.Errorf("%s with %d observations: %s/%q reason %q", c.id, n, res.Status, res.TopFamily, res.Reason)
			}
		}
	}
}

func TestRankExactWithOverhead(t *testing.T) {
	db := loadDB(t)
	e := entryByID(t, db, "deepseek-v3")
	res := db.Rank(observationsFor(db, e, 7), 0)
	if res.Status != StatusIdentified || res.TopFamily != "deepseek" {
		t.Fatalf("got %s/%q reason %q", res.Status, res.TopFamily, res.Reason)
	}
	if res.Rankings[0].MAE != 0 || res.Rankings[0].Overhead != 7 {
		t.Fatalf("fit = MAE %v overhead %v, want 0/7", res.Rankings[0].MAE, res.Rankings[0].Overhead)
	}
	if !res.ShouldStop() {
		t.Fatal("exact full-corpus match should satisfy the stop rule")
	}
}

// Regression: confidence must accumulate per probe. An exact fit over the
// full 12-probe corpus is decisive evidence; a flat exp(-MAE/2) softmax
// scored it at only ~0.52 and made every endpoint inconclusive.
func TestRankExactMatchConfidenceAccumulates(t *testing.T) {
	db := loadDB(t)
	e := entryByID(t, db, "deepseek-v3")
	res := db.Rank(observationsFor(db, e, 7), 0)
	if got := res.Rankings[0].Confidence; got < 0.9 {
		t.Fatalf("exact full-corpus confidence = %.3f, want >= 0.9", got)
	}
}

func TestRankSharedTokenizerTieIsInconclusive(t *testing.T) {
	db := loadDB(t)
	e := entryByID(t, db, "qwen25")
	res := db.Rank(observationsFor(db, e, 3), 0)
	if res.Status != StatusInconclusive || res.ReasonCode != ReasonSharedTokenizer {
		t.Fatalf("shared tokenizer: status %s reason %q", res.Status, res.Reason)
	}
	if !strings.Contains(res.Reason, "share an identical tokenizer") {
		t.Fatalf("reason should explain the tie, got %q", res.Reason)
	}
	for _, family := range []string{"kimi", "mimo", "qwen"} {
		if !strings.Contains(res.Reason, family) {
			t.Errorf("reason %q omits %s", res.Reason, family)
		}
	}
	shared := map[string][]string{}
	for _, r := range res.Rankings {
		shared[r.Family] = r.SharedWith
	}
	for family, want := range map[string][]string{
		"kimi": {"mimo", "qwen"}, "mimo": {"kimi", "qwen"}, "qwen": {"kimi", "mimo"},
	} {
		if !slices.Equal(shared[family], want) {
			t.Errorf("%s SharedWith = %v, want %v", family, shared[family], want)
		}
	}
}

func TestRankUnverifiedNeverVerdict(t *testing.T) {
	db := loadDB(t)
	e := entryByID(t, db, "google-gemma-sp")
	res := db.Rank(observationsFor(db, e, 4), 0)
	if res.TopFamily == "google" || res.Status == StatusIdentified {
		t.Fatalf("exact unverified-family counts must stay inconclusive, got %s/%q reason %q", res.Status, res.TopFamily, res.Reason)
	}
	for _, r := range res.Rankings {
		if r.Family == "google" || !r.Verified {
			t.Fatalf("unverified family %s present in verified rankings", r.Family)
		}
	}
	if len(res.Unverified) == 0 || res.Unverified[0].Family != "google" || res.Unverified[0].MAE != 0 {
		t.Fatalf("unverified rankings = %+v", res.Unverified)
	}
}

// Regression: constant fabricated usage must never identify.
// A constant count fitted a low-variance entry (OpenAI o200k on stage 1)
// spuriously once the median overhead absorbed it.
func TestRankConstantFakeUsageInconclusive(t *testing.T) {
	db := loadDB(t)
	mk := func(n, value int) []Observation {
		obs := make([]Observation, n)
		for i := 0; i < n; i++ {
			obs[i] = Observation{ProbeID: db.Probes[i].ID, PromptTokens: value, HasUsage: true, HTTPStatus: 200}
		}
		return obs
	}
	alternating := make([]Observation, 6)
	for i := range alternating {
		alternating[i] = Observation{ProbeID: db.Probes[i].ID, PromptTokens: 30 + i%2, HasUsage: true, HTTPStatus: 200}
	}
	for _, obs := range [][]Observation{mk(len(db.Probes), 30), mk(4, 30), mk(len(db.Probes), 20), alternating} {
		res := db.Rank(obs, 0)
		if res.Status == StatusIdentified {
			t.Fatalf("constant usage identified as %q", res.TopFamily)
		}
		if !strings.Contains(res.Reason, "vary implausibly") && !strings.Contains(res.Reason, "fits poorly") {
			t.Fatalf("reason %q should call the usage unreliable", res.Reason)
		}
	}
}

// Regression vectors from the multi-agent audit: fabricated, scaled, and
// partial counts that an earlier Rank identified must stay inconclusive.
// Vectors use the 10 non-special probes in DB order.
func TestRankAuditAttackVectorsInconclusive(t *testing.T) {
	db := loadDB(t)
	nonSpecial := []int{}
	for i, p := range db.Probes {
		if !p.Special {
			nonSpecial = append(nonSpecial, i)
		}
	}
	mk := func(counts ...int) []Observation {
		obs := make([]Observation, len(counts))
		for j, c := range counts {
			obs[j] = Observation{ProbeID: db.Probes[nonSpecial[j]].ID, PromptTokens: c, HasUsage: true, HTTPStatus: 200}
		}
		return obs
	}
	cases := map[string][]Observation{
		"stage1 tiny fake":       mk(2, 2, 3, 4),
		"scaled 0.75x deepseek":  mk(15, 10, 20, 18, 15, 18, 8, 14, 6, 2),
		"one catastrophic probe": mk(52, 20, 33, 31, 27, 31, 17, 26, 15, 9),
		"glm/claude midpoint":    mk(25, 26, 31, 39, 26, 36, 16, 31, 11, 7),
	}
	// Overhead 100000 on an exact DeepSeek pattern must be implausible.
	e := entryByID(t, db, "deepseek-v3")
	cases["huge overhead"] = observationsFor(db, e, 100000)
	cases["negative overhead"] = observationsFor(db, e, -5)
	for name, obs := range cases {
		res := db.Rank(obs, 0)
		if res.Status == StatusIdentified {
			t.Errorf("%s: identified as %q (conf %.3f), want inconclusive; rankings=%+v", name, res.TopFamily, res.Rankings[0].Confidence, res.Rankings)
		}
	}
	// NaN must not disable the confidence gate.
	res := db.Rank(mk(2, 2, 3, 4), math.NaN())
	if res.Status == StatusIdentified {
		t.Errorf("NaN minConfidence identified %q", res.TopFamily)
	}
}

func TestRankMissingUsageInconclusive(t *testing.T) {
	db := loadDB(t)
	obs := []Observation{
		{ProbeID: db.Probes[0].ID, HasUsage: false, HTTPStatus: 200, Error: "no usage"},
		{ProbeID: db.Probes[1].ID, PromptTokens: 20, HasUsage: true, HTTPStatus: 200},
	}
	res := db.Rank(obs, 0)
	if res.Status != StatusInconclusive || !strings.Contains(res.Reason, "usable token counts") {
		t.Fatalf("got %s reason %q", res.Status, res.Reason)
	}
}

func TestRankUnknownCountsInconclusive(t *testing.T) {
	db := loadDB(t)
	obs := make([]Observation, len(db.Probes))
	for i, p := range db.Probes {
		obs[i] = Observation{ProbeID: p.ID, PromptTokens: 100 + i*37%13, HasUsage: true, HTTPStatus: 200}
	}
	res := db.Rank(obs, 0)
	if res.Status != StatusInconclusive {
		t.Fatalf("arbitrary counts identified as %q (MAE %v)", res.TopFamily, res.Rankings[0].MAE)
	}
}

func TestRankSpecialRejectionEvidence(t *testing.T) {
	db := loadDB(t)
	e := entryByID(t, db, "openai-o200k")
	obs := observationsFor(db, e, 5)
	for i, o := range obs {
		if o.ProbeID == "special_a" {
			obs[i] = Observation{ProbeID: o.ProbeID, Rejected: true, HTTPStatus: 400}
		}
	}
	res := db.Rank(obs, 0)
	if res.Status != StatusIdentified || res.TopFamily != "openai" {
		t.Fatalf("got %s/%q reason %q", res.Status, res.TopFamily, res.Reason)
	}
}

func TestNormalizeAndInferFamily(t *testing.T) {
	db := loadDB(t)
	cases := map[string]string{"deepseek-chat": "deepseek", "gpt-4o": "openai", "qwen2.5-72b": "qwen", "claude-sonnet-4": "claude", "Stealth-X": ""}
	for model, want := range cases {
		if got := db.FamilyForModel(model); got != want {
			t.Errorf("FamilyForModel(%q) = %q, want %q", model, got, want)
		}
	}
	if got := db.NormalizeFamily("GPT-4"); got != "openai" {
		t.Errorf("NormalizeFamily(GPT-4) = %q", got)
	}
	if got := db.NormalizeFamily("nonsense"); got != "" {
		t.Errorf("NormalizeFamily(nonsense) = %q", got)
	}
}

func TestRankReasonCodes(t *testing.T) {
	db := loadDB(t)

	// Identified results carry no reason code.
	res := db.Rank(observationsFor(db, entryByID(t, db, "deepseek-v3"), 6), 0)
	if res.Status != StatusIdentified || res.ReasonCode != "" {
		t.Fatalf("identified: status %s code %q", res.Status, res.ReasonCode)
	}

	// No usable counts at all.
	res = db.Rank(nil, 0)
	if res.ReasonCode != ReasonInsufficientCounts {
		t.Fatalf("empty obs: code %q reason %q", res.ReasonCode, res.Reason)
	}

	// Constant fabricated counts are unreliable, never a verdict.
	obs := observationsFor(db, entryByID(t, db, "deepseek-v3"), 0)
	for i := range obs {
		obs[i].PromptTokens = 50
	}
	res = db.Rank(obs, 0)
	if res.Status != StatusInconclusive || res.ReasonCode != ReasonUnreliableCounts {
		t.Fatalf("constant counts: status %s code %q reason %q", res.Status, res.ReasonCode, res.Reason)
	}

	// Qwen, MiMo, and Kimi-Dev share one tokenizer: a tie, not a guess.
	res = db.Rank(observationsFor(db, entryByID(t, db, "qwen25"), 6), 0)
	if res.Status != StatusInconclusive || res.ReasonCode != ReasonSharedTokenizer {
		t.Fatalf("qwen tie: status %s code %q reason %q", res.Status, res.ReasonCode, res.Reason)
	}
}
