package fingerprint

import "testing"

func tokObsForEntry(t *testing.T, db *DB, entryID string, perturb func(i, v int) int) []TokObservation {
	t.Helper()
	var e *Entry
	for i := range db.Entries {
		if db.Entries[i].ID == entryID {
			e = &db.Entries[i]
		}
	}
	if e == nil {
		t.Fatalf("entry %s not found", entryID)
	}
	var obs []TokObservation
	for i, p := range db.Probes {
		if p.Special {
			continue
		}
		v := e.expectedDiff(i)
		if perturb != nil {
			v = perturb(i, v)
		}
		obs = append(obs, TokObservation{ProbeID: p.ID, Diff: v, HasDiff: true})
	}
	return obs
}

func TestTokRankExactMatch(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	res := db.TokRank(tokObsForEntry(t, db, "deepseek-v3", nil), "deepseek-chat", TokParams{})
	if res.Verdict != TokVerdictExact || res.TopFamily != "deepseek" {
		t.Fatalf("verdict = %s family = %s, want exact/deepseek (prob %.4f)", res.Verdict, res.TopFamily, res.Probability)
	}
	if res.Probability < 0.9 {
		t.Fatalf("exact probability %.4f too low", res.Probability)
	}
	if res.Claim == nil || res.Claim.Status != "consistent" {
		t.Fatalf("claim = %+v, want consistent", res.Claim)
	}
}

func TestTokRankNoisyCountsStillTopFamily(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Simulated noisy upstream: every third count off by +2, one
	// count off by -3 (boundary merges / relay miscounts).
	obs := tokObsForEntry(t, db, "deepseek-v3", func(i, v int) int {
		switch {
		case i == 4:
			return v - 3
		case i%3 == 0:
			return v + 2
		default:
			return v
		}
	})
	res := db.TokRank(obs, "", TokParams{})
	if res.TopFamily != "deepseek" || res.Verdict == TokVerdictInconclusive {
		t.Fatalf("noisy verdict = %s family = %s prob = %.4f", res.Verdict, res.TopFamily, res.Probability)
	}
	if len(res.Candidates) == 0 || res.Candidates[0].Family != "deepseek" {
		t.Fatalf("candidates = %+v", res.Candidates)
	}
}

func TestTokRankClaimInconsistent(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	res := db.TokRank(tokObsForEntry(t, db, "deepseek-v3", nil), "gpt-4o", TokParams{})
	if res.Claim == nil || res.Claim.Status != "inconsistent" {
		t.Fatalf("claim = %+v, want inconsistent", res.Claim)
	}
}

func TestTokRankUnknownForUnseenTokenizer(t *testing.T) {
	db, err := Parse([]byte(`{
		"version":"test","note":"",
		"probes":[
			{"id":"a","stage":1,"text":"a"},{"id":"b","stage":1,"text":"b"},
			{"id":"c","stage":1,"text":"c"},{"id":"d","stage":1,"text":"d"},
			{"id":"e","stage":1,"text":"e"},{"id":"f","stage":1,"text":"f"}
		],
		"entries":[
			{"id":"e1","family":"f1","display":"F1","tokenizer":"t1","verified":true,"counts":[5,9,13,17,21,25],"reject_probes":[],"aliases":[]},
			{"id":"e2","family":"f2","display":"F2","tokenizer":"t2","verified":true,"counts":[6,10,14,18,22,26],"reject_probes":[],"aliases":[]}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	// Counts far from both entries on every probe: an unlisted
	// tokenizer must win over a forced closest match.
	ids := []string{"a", "b", "c", "d", "e", "f"}
	var obs []TokObservation
	for i, id := range ids {
		obs = append(obs, TokObservation{ProbeID: id, Diff: 40 + i*3, HasDiff: true})
	}
	res := db.TokRank(obs, "", TokParams{})
	if res.Verdict == TokVerdictExact {
		t.Fatalf("unseen tokenizer rendered exact: %+v", res)
	}
	if res.UnlistedProb < 0.5 {
		t.Fatalf("unlisted probability %.4f, want >= 0.5", res.UnlistedProb)
	}
}

func TestTokRankInsufficient(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	obs := tokObsForEntry(t, db, "deepseek-v3", nil)[:2]
	res := db.TokRank(obs, "", TokParams{})
	if res.Verdict != TokVerdictInconclusive || res.ReasonCode != TokReasonInsufficient {
		t.Fatalf("verdict = %s code = %s, want inconclusive/insufficient_counts", res.Verdict, res.ReasonCode)
	}
}

func TestTokRankFloorDominatedClaimsUnchecked(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, varied := range []bool{false, true} {
		obs := tokObsForEntry(t, db, "deepseek-v3", func(i, _ int) int {
			if varied {
				return 1000 + i*17
			}
			return 1000
		})
		got := db.TokRank(obs, "deepseek-chat", TokParams{})
		if got.Verdict != TokVerdictInconclusive || got.ReasonCode != "unreliable_counts" || got.Claim == nil || got.Claim.Status != "unchecked" {
			t.Fatalf("floor-only likelihoods became claim evidence (varied=%v): %+v", varied, got)
		}
	}
}

func TestTokRankInsufficientClaimUnchecked(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	all := tokObsForEntry(t, db, "deepseek-v3", nil)
	for n := 0; n < MinTokProbes; n++ {
		res := db.TokRank(all[:n], "gpt-4o", TokParams{})
		if res.Claim == nil || res.Claim.Status != "unchecked" {
			t.Fatalf("%d observations: claim = %+v", n, res.Claim)
		}
	}
}

func TestTokRankSharedTokenizerInconclusive(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"qwen25", "mimo-7b", "kimi-dev"} {
		t.Run(id, func(t *testing.T) {
			res := db.TokRank(tokObsForEntry(t, db, id, nil), "", TokParams{})
			if res.Verdict != TokVerdictInconclusive || res.ReasonCode != TokReasonAmbiguous || res.UnlistedProb >= 0.5 {
				t.Fatalf("shared tokenizer result = %+v", res)
			}
			if len(res.Candidates) < 3 || res.TopEntryID != "" {
				t.Fatalf("shared tokenizer ambiguity lost: %+v", res)
			}
		})
	}
}

func TestTokRankRelatedAmbiguity(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	obs := tokObsForEntry(t, db, "qwen25", func(_, v int) int { return v + 4 })
	for _, reversed := range []bool{false, true} {
		if reversed {
			for i, j := 0, len(db.Entries)-1; i < j; i, j = i+1, j-1 {
				db.Entries[i], db.Entries[j] = db.Entries[j], db.Entries[i]
			}
		}
		res := db.TokRank(obs, "", TokParams{})
		if res.Verdict != TokVerdictInconclusive || res.ReasonCode != TokReasonAmbiguous || res.TopEntryID != "" || res.TopFamily != "" || res.Probability != 0 {
			t.Fatalf("tied related evidence (reversed=%v): %+v", reversed, res)
		}
	}
	unique := db.TokRank(tokObsForEntry(t, db, "deepseek-v3", func(_, v int) int { return v + 4 }), "", TokParams{})
	if unique.Verdict != TokVerdictRelated || unique.TopEntryID == "" {
		t.Fatalf("unique related evidence: %+v", unique)
	}
}

func TestTokRankUnverifiedNeverVerdictOrClaim(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, model string }{
		{"google-gemma-sp", "gemini-2.5-pro"},
		{"claude-xenova", "claude-sonnet-4"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			res := db.TokRank(tokObsForEntry(t, db, tc.id, nil), tc.model, TokParams{})
			if res.Verdict != TokVerdictInconclusive || res.ReasonCode != TokReasonUnverified || res.TopEntryID != "" {
				t.Fatalf("unverified proxy verdict = %+v", res)
			}
			if res.Claim == nil || res.Claim.Status != "unchecked" {
				t.Fatalf("unverified claim = %+v", res.Claim)
			}
			other := db.TokRank(tokObsForEntry(t, db, "deepseek-v3", nil), tc.model, TokParams{})
			if other.Claim == nil || other.Claim.Status != "unchecked" {
				t.Fatalf("unverified family checked against verified evidence: %+v", other.Claim)
			}
		})
	}
}

func TestTokSelectBatchDeterministicAndOrdinary(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	a, gainA := db.TokSelectBatch(nil, 4, TokParams{})
	b, gainB := db.TokSelectBatch(nil, 4, TokParams{})
	if len(a) != 4 || gainA <= 0 {
		t.Fatalf("batch = %d probes, gain %.6f", len(a), gainA)
	}
	if gainA != gainB {
		t.Fatalf("non-deterministic gain %.6f vs %.6f", gainA, gainB)
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Special {
			t.Fatalf("batch mismatch or special probe at %d: %+v", i, a[i])
		}
	}
	// Used probes are never re-picked.
	used := []TokObservation{{ProbeID: a[0].ID, Diff: 1, HasDiff: true}}
	c, _ := db.TokSelectBatch(used, 4, TokParams{})
	for _, p := range c {
		if p.ID == a[0].ID {
			t.Fatalf("re-picked used probe %s", p.ID)
		}
	}
}

func TestTokRankRejectsDegenerateDifferentials(t *testing.T) {
	db, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		diff func(int, int) int
	}{
		{"zero", func(_, _ int) int { return 0 }},
		{"positive_constant", func(_, _ int) int { return 7 }},
		{"negative_constant", func(_, _ int) int { return -1 }},
		{"varying_nonpositive", func(i, _ int) int { return -i }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := tokObsForEntry(t, db, "deepseek-v3", tc.diff)
			res := db.TokRank(obs, "gpt-4o", TokParams{})
			if res.Verdict != TokVerdictInconclusive || res.ReasonCode != TokReasonUnreliableCounts || res.Probability != 0 || res.TopEntryID != "" {
				t.Fatalf("verdict=%s reason=%s probability=%v top=%s", res.Verdict, res.ReasonCode, res.Probability, res.TopEntryID)
			}
			if res.Claim == nil || res.Claim.Status != "unchecked" || res.Claim.Probability != 0 {
				t.Fatalf("claim = %+v", res.Claim)
			}
		})
	}
}
