package fingerprint

import (
	"fmt"
	"math"
	"testing"
)

func TestConfidenceThresholdUsesFullPrecision(t *testing.T) {
	db := &DB{Entries: []Entry{
		{ID: "a", Family: "a", Verified: true, Counts: []int{1, 11, 21, 31, 41, 51}},
		{ID: "b", Family: "b", Verified: true, Counts: []int{1, 11, 21, 31, 41, 99}},
	}}
	for i := 0; i < 6; i++ {
		db.Probes = append(db.Probes, Probe{ID: fmt.Sprint(i), Stage: 1})
	}
	obs := observationsFor(db, db.Entries[0], 7)
	res := db.Rank(obs, 1)
	if res.Status == StatusIdentified {
		t.Fatalf("rounded confidence produced false certainty: %+v", res.Rankings[0])
	}
	if res.Rankings[0].Confidence >= 1 {
		t.Fatal("nonzero competing likelihood rounded to 100%")
	}
}

func TestRankDoesNotUseFailedObservations(t *testing.T) {
	for _, kind := range []string{"error", "http"} {
		t.Run(kind, func(t *testing.T) {
			db := loadDB(t)
			obs := observationsFor(db, entryByID(t, db, "deepseek-v3"), 7)
			for i := range obs {
				if kind == "error" {
					obs[i].Error = "failed"
				} else {
					obs[i].HTTPStatus = 500
				}
			}
			if r := db.Rank(obs, 0); r.Status != StatusInconclusive || len(r.Rankings) != 0 {
				t.Fatalf("failed responses ranked: %s", r.Status)
			}
		})
	}
}

func TestSharedTokenizerCannotBeSeparatedBySpecialPolicy(t *testing.T) {
	db := loadDB(t)
	for i := range db.Entries {
		if db.Entries[i].Family == "mimo" {
			db.Entries[i].RejectProbes = []string{"special_a", "special_b"}
		}
	}
	obs := observationsFor(db, entryByID(t, db, "qwen25"), 5)
	if res := db.Rank(obs, 0.01); res.Status != StatusInconclusive || res.ReasonCode != ReasonSharedTokenizer {
		t.Fatalf("server special-token policy broke tokenizer tie: %s %s", res.Status, res.ReasonCode)
	}
}

func TestSoftmaxDoesNotUnderflow(t *testing.T) {
	db := loadDB(t)
	obs := observationsFor(db, entryByID(t, db, "deepseek-v3"), 5)
	for i := range obs {
		obs[i].PromptTokens += (i % 2) * 10000
	}
	r := db.Rank(obs, 0)
	sum := 0.0
	for _, group := range [][]FamilyRank{r.Rankings, r.Unverified} {
		for _, rank := range group {
			sum += rank.Confidence
		}
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("confidence distribution sums to %g", sum)
	}
}

func TestValidateRejectsAmbiguousEntries(t *testing.T) {
	for _, kind := range []string{"empty id", "duplicate id", "empty family"} {
		t.Run(kind, func(t *testing.T) {
			db := loadDB(t)
			switch kind {
			case "empty id":
				db.Entries[0].ID = ""
			case "duplicate id":
				db.Entries[1].ID = db.Entries[0].ID
			case "empty family":
				db.Entries[0].Family = ""
			}
			if err := db.Validate(); err == nil {
				t.Fatal("invalid database accepted")
			}
		})
	}
}

func FuzzRankInvariants(f *testing.F) {
	f.Add([]byte{20, 36, 18, 45, 22, 37, 6, 21, 8, 2})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		db, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		obs := make([]Observation, 0, len(db.Probes))
		for i, p := range db.Probes {
			n := 0
			if len(data) > 0 {
				n = int(data[i%len(data)])
			}
			obs = append(obs, Observation{ProbeID: p.ID, PromptTokens: n, HasUsage: true, HTTPStatus: 200})
		}
		r := db.Rank(obs, 0)
		for _, group := range [][]FamilyRank{r.Rankings, r.Unverified} {
			for _, rank := range group {
				if math.IsNaN(rank.Confidence) || rank.Confidence < 0 || rank.Confidence > 1 {
					t.Fatal(fmt.Sprint(rank))
				}
			}
		}
		if r.Status == StatusIdentified && !db.IsVerified(r.TopFamily) {
			t.Fatal("unverified verdict")
		}
	})
}
