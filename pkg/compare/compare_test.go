package compare

import (
	"strings"
	"testing"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func rankFor(t *testing.T, db *fingerprint.DB, entryID string, overhead int) fingerprint.Result {
	t.Helper()
	var e fingerprint.Entry
	for _, en := range db.Entries {
		if en.ID == entryID {
			e = en
		}
	}
	obs := make([]fingerprint.Observation, len(db.Probes))
	for i, p := range db.Probes {
		obs[i] = fingerprint.Observation{ProbeID: p.ID, PromptTokens: e.Counts[i] + overhead, HasUsage: true, HTTPStatus: 200}
	}
	r := db.Rank(obs, 0)
	r.RequestsSpent = len(obs)
	return r
}

func loadDB(t *testing.T) *fingerprint.DB {
	t.Helper()
	db, err := fingerprint.Load()
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestEvaluateMatchMismatch(t *testing.T) {
	db := loadDB(t)
	target := rankFor(t, db, "deepseek-v3", 5)
	if got := Evaluate(db, "deepseek", target, nil); got.Verdict != VerdictMatch {
		t.Fatalf("verdict = %s reason %q", got.Verdict, got.Reason)
	}
	if got := Evaluate(db, "mistral", target, nil); got.Verdict != VerdictMismatch {
		t.Fatalf("verdict = %s reason %q", got.Verdict, got.Reason)
	}
}

func TestEvaluateUnverifiedClaimInconclusive(t *testing.T) {
	db := loadDB(t)
	target := rankFor(t, db, "deepseek-v3", 5)
	got := Evaluate(db, "google", target, nil)
	if got.Verdict != VerdictInconclusive || !strings.Contains(got.Reason, "not a verified family") {
		t.Fatalf("verdict = %s reason %q", got.Verdict, got.Reason)
	}
}

// When target and official agree with each other but not the claim, the
// baseline corroborates the fingerprint: the mismatch verdict stands.
func TestEvaluateCorroboratedMismatchStands(t *testing.T) {
	db := loadDB(t)
	target := rankFor(t, db, "deepseek-v3", 5)
	official := rankFor(t, db, "deepseek-v3", 9)
	got := Evaluate(db, "mistral", target, &official)
	if got.Verdict != VerdictMismatch {
		t.Fatalf("verdict = %s reason %q, want corroborated mismatch", got.Verdict, got.Reason)
	}
}

// The clearest mismatch: the official matches the claim and the target
// does not. The baseline must not downgrade this to inconclusive.
func TestEvaluateClearMismatchWithOfficial(t *testing.T) {
	db := loadDB(t)
	target := rankFor(t, db, "mistral-v3", 5)
	official := rankFor(t, db, "deepseek-v3", 5)
	got := Evaluate(db, "deepseek", target, &official)
	if got.Verdict != VerdictMismatch {
		t.Fatalf("verdict = %s reason %q, want mismatch", got.Verdict, got.Reason)
	}
}

func TestEvaluateOfficialInDoubtDowngrades(t *testing.T) {
	db := loadDB(t)
	target := rankFor(t, db, "deepseek-v3", 5)
	official := rankFor(t, db, "mistral-v3", 5) // reference contradicts the claim
	got := Evaluate(db, "deepseek", target, &official)
	if got.Verdict != VerdictInconclusive {
		t.Fatalf("verdict = %s, want inconclusive when the official baseline itself mismatches the claim", got.Verdict)
	}
	if got.Divergence == nil || got.Divergence.MeanAbsDiff == 0 {
		t.Fatalf("expected nonzero divergence signal, got %+v", got.Divergence)
	}
	if got.RequestsSpent != target.RequestsSpent+official.RequestsSpent {
		t.Fatalf("requests = %d", got.RequestsSpent)
	}
}

func TestDivergenceOverheadNormalized(t *testing.T) {
	db := loadDB(t)
	target := rankFor(t, db, "deepseek-v3", 5)
	official := rankFor(t, db, "deepseek-v3", 40)
	d := ComputeDivergence(db, target, official)
	if d == nil || d.MeanAbsDiff != 0 || d.MaxAbsDiff != 0 {
		t.Fatalf("same tokenizer, different templates must diverge by 0 after normalization, got %+v", d)
	}
}
