package numprint

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// synthSeq builds a model-like sequence: hops between preferred
// centers (never the same center twice in a row), so small steps
// are rare and the value distribution is family-specific.
func synthSeq(r *rand.Rand, centers []int, n int) []int {
	seq := make([]int, 0, n)
	prev := -1
	for len(seq) < n {
		c := centers[r.Intn(len(centers))]
		if c == prev {
			continue
		}
		prev = c
		v := c + r.Intn(31) - 15
		v = min(max(v, ValueMin), ValueMax)
		seq = append(seq, v)
	}
	return seq
}

var centersA = []int{37, 73, 137, 210, 286, 337}
var centersB = []int{58, 99, 164, 243, 301, 352}

func synthBank(t *testing.T, samplesPerEntry int) *Bank {
	t.Helper()
	r := rand.New(rand.NewSource(42))
	mk := func(id string, centers []int) Entry {
		e := Entry{ID: id, Family: "fam-" + id, Display: id, Model: "model-" + id}
		for i := 0; i < samplesPerEntry; i++ {
			e.Samples = append(e.Samples, Sample{
				ID: fmt.Sprintf("%s-s%d", id, i+1), Environment: "nosys-plain",
				ExpectedCount: 300, Sequence: synthSeq(r, centers, 300),
			})
		}
		return e
	}
	b := &Bank{Version: "test", Entries: []Entry{mk("a", centersA), mk("b", centersB)}}
	if err := b.Validate(); err != nil {
		t.Fatalf("synthetic bank invalid: %v", err)
	}
	return b
}

func answersFromSeqs(seqs [][]int) []Answer {
	var out []Answer
	for i, s := range seqs {
		out = append(out, Answer{ChallengeID: fmt.Sprintf("c%d", i+1), ExpectedCount: len(s), Text: sequenceString(s), Completed: true})
	}
	return out
}

func TestParseSequence(t *testing.T) {
	text := "Sure! Here you go: 12, 300; 45 9\nand then 7 8 9 10 (version 3.5 aside) 400 12"
	got := ParseSequence(text)
	want := []int{12, 300, 45, 9}
	if len(got) != len(want) {
		t.Fatalf("ParseSequence = %v, want longest run %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParseSequence = %v, want %v", got, want)
		}
	}
	if seq := ParseSequence("no numbers here"); seq != nil {
		t.Fatalf("expected nil, got %v", seq)
	}
	// Trailing partial digits still parse as the integers present.
	if seq := ParseSequence("10 20 30 4"); len(seq) != 4 {
		t.Fatalf("expected 4 integers, got %v", seq)
	}
}

func TestGenerateChallenges(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	chs := GenerateChallenges(r, 3)
	if len(chs) != 3 {
		t.Fatalf("got %d challenges", len(chs))
	}
	seen := map[int]bool{}
	for _, c := range chs {
		if c.ExpectedCount < 292 || c.ExpectedCount > 332 || seen[c.ExpectedCount] {
			t.Fatalf("bad count %d", c.ExpectedCount)
		}
		seen[c.ExpectedCount] = true
		if !strings.Contains(c.Prompt, fmt.Sprint(c.ExpectedCount)) {
			t.Fatalf("prompt missing count: %q", c.Prompt)
		}
	}
	if got := len(SamplingSet()); got != 36 {
		t.Fatalf("SamplingSet = %d challenges, want 36", got)
	}
}

func TestScoreIdentifiesSyntheticFamily(t *testing.T) {
	b := synthBank(t, 6)
	r := rand.New(rand.NewSource(99))
	var seqs [][]int
	for i := 0; i < 3; i++ {
		seqs = append(seqs, synthSeq(r, centersA, 300))
	}
	res := b.Score(answersFromSeqs(seqs), ScoreOptions{})
	if res.Status != StatusRanked {
		t.Fatalf("status = %s (%s)", res.Status, res.Reason)
	}
	if res.TopEntryID != "a" {
		t.Fatalf("top = %s, want a (candidates %+v)", res.TopEntryID, res.Candidates)
	}
	if res.Calibrated {
		t.Fatal("uncalibrated bank must not report confidence")
	}
	if len(res.Candidates) != 2 || res.Candidates[0].Confidence != 0 {
		t.Fatalf("candidates = %+v", res.Candidates)
	}
}

func TestScoreRejectsNonModelSequences(t *testing.T) {
	b := synthBank(t, 6)
	// Sorted counting sequence.
	sorted := make([]int, 300)
	for i := range sorted {
		sorted[i] = i + 1
	}
	res := b.Score(answersFromSeqs([][]int{sorted, sorted, sorted}), ScoreOptions{})
	if res.Status != StatusNotModelReply {
		t.Fatalf("sorted: status = %s, want not_model_reply (check %+v)", res.Status, res.ReplyCheck)
	}
	// Program-generated uniform random numbers.
	r := rand.New(rand.NewSource(1))
	var seqs [][]int
	for k := 0; k < 3; k++ {
		s := make([]int, 320)
		for i := range s {
			s[i] = r.Intn(ValueMax) + 1
		}
		seqs = append(seqs, s)
	}
	res = b.Score(answersFromSeqs(seqs), ScoreOptions{})
	if res.Status != StatusNotModelReply {
		t.Fatalf("uniform PRNG: status = %s, want not_model_reply (check %+v)", res.Status, res.ReplyCheck)
	}
	// Refusal / too few numbers.
	res = b.Score([]Answer{{Text: "I cannot do that.", Completed: true}}, ScoreOptions{})
	if res.Status != StatusInconclusive || res.ReasonCode != ReasonInsufficientAnswers {
		t.Fatalf("refusal: status = %s code = %s", res.Status, res.ReasonCode)
	}
}

func TestScoreEmbeddedBankInsufficient(t *testing.T) {
	b, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	res := b.Score(answersFromSeqs([][]int{synthSeq(rand.New(rand.NewSource(3)), centersA, 300)}), ScoreOptions{})
	if res.Status != StatusInconclusive || res.ReasonCode != ReasonInsufficientBank {
		t.Fatalf("embedded bank: status = %s code = %s, want insufficient_bank", res.Status, res.ReasonCode)
	}
}

func TestRetrainAndCalibratedScore(t *testing.T) {
	b := synthBank(t, 8)
	rep, err := Retrain(b)
	if err != nil {
		t.Fatalf("Retrain: %v", err)
	}
	if rep.Temperature <= 0 || !rep.SeqLimits.Fitted {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Accuracy < 0.8 {
		t.Fatalf("leave-one-out accuracy %.2f too low (per-entry %+v)", rep.Accuracy, rep.PerEntry)
	}
	r := rand.New(rand.NewSource(123))
	var seqs [][]int
	for i := 0; i < 3; i++ {
		seqs = append(seqs, synthSeq(r, centersB, 300))
	}
	res := b.Score(answersFromSeqs(seqs), ScoreOptions{})
	if res.Status != StatusRanked || res.TopEntryID != "b" || !res.Calibrated {
		t.Fatalf("calibrated score = %+v", res)
	}
	if res.Candidates[0].Confidence <= 0.5 {
		t.Fatalf("top confidence %.4f too low", res.Candidates[0].Confidence)
	}
	// Fewer than 3 answers: ranking only, no confidence.
	res = b.Score(answersFromSeqs(seqs[:1]), ScoreOptions{})
	if res.Calibrated || (len(res.Candidates) > 0 && res.Candidates[0].Confidence != 0) {
		t.Fatalf("single answer must be uncalibrated: %+v", res)
	}
}

func TestImportedLDARemainsUncalibrated(t *testing.T) {
	b := synthBank(t, 4)
	base, err := Retrain(b)
	if err != nil {
		t.Fatal(err)
	}
	lda := &LDAParams{Projection: make([][]float64, featureDims), Means: map[string][]float64{}}
	for i := range lda.Projection {
		lda.Projection[i] = []float64{0}
	}
	lda.Projection[0][0] = 1
	for _, e := range b.Entries {
		lda.Means[e.ID] = projectLDA(lda, meanVec(entryFeatures(e, -1)))
	}
	b.Params.LDA = lda
	r := rand.New(rand.NewSource(123))
	var seqs [][]int
	for i := 0; i < 3; i++ {
		seqs = append(seqs, synthSeq(r, centersB, 300))
	}
	assertUncalibrated := func() {
		t.Helper()
		res := b.Score(answersFromSeqs(seqs), ScoreOptions{})
		if res.Status != StatusRanked || res.Calibrated {
			t.Fatalf("imported LDA result = %+v", res)
		}
		for _, candidate := range res.Candidates {
			if candidate.Confidence != 0 {
				t.Fatalf("uncalibrated LDA confidence = %+v", candidate)
			}
		}
	}
	// Even a pre-existing fitted temperature cannot calibrate an imported LDA.
	assertUncalibrated()
	rep, err := Retrain(b)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.LDAExcluded || rep.Temperature != 0 || b.Params.Temperature != 0 || b.Params.LDA != lda {
		t.Fatalf("imported LDA calibration report = %+v", rep)
	}
	if rep.Accuracy != base.Accuracy || rep.Top1 != base.Top1 || rep.Samples != base.Samples {
		t.Fatalf("LDA contaminated base-scorer evaluation: before=%+v after=%+v", base, rep)
	}
	assertUncalibrated()
}

func TestEnrollBatch(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	seq := synthSeq(r, centersA, 300)
	batch := &Batch{
		Version: "1", EntryID: "a", Family: "fam-a", Model: "model-a",
		EndpointHost: "api.example.com", CollectedAt: "2026-10-30T00:00:00Z",
		Samples:  []BatchSample{{ID: "nosys-plain-1", Environment: "nosys-plain", ExpectedCount: 300, Text: sequenceString(seq), Sequence: seq}},
		Failures: []BatchFailure{{ID: "nosys-plain-2", Error: "HTTP 500"}},
	}
	b := &Bank{Version: "test"}
	added, err := b.Enroll(batch)
	if err != nil || added != 1 {
		t.Fatalf("Enroll = %d, %v", added, err)
	}
	if _, err := b.Enroll(batch); err == nil {
		t.Fatal("re-enrolling the same sample id must fail")
	}
	// Tampered sequence vs retained text is rejected.
	bad := *batch
	bad.Samples = []BatchSample{{ID: "x-1", ExpectedCount: 300, Text: sequenceString(seq), Sequence: synthSeq(r, centersB, 300)}}
	if err := bad.Validate(); err == nil {
		t.Fatal("sequence/text mismatch must fail validation")
	}
}

func TestStrictModeRejectsTruncated(t *testing.T) {
	b := synthBank(t, 6)
	seq := synthSeq(rand.New(rand.NewSource(11)), centersA, 300)
	ans := []Answer{{ExpectedCount: 300, Text: sequenceString(seq), Truncated: true, Completed: false}}
	if res := b.Score(ans, ScoreOptions{Strict: true}); res.ValidAnswers != 0 {
		t.Fatalf("strict accepted truncated answer: %+v", res)
	}
	if res := b.Score(ans, ScoreOptions{}); res.ValidAnswers != 1 {
		t.Fatalf("relaxed rejected truncated answer: %+v", res)
	}
}

func TestCountIntegersWaitsForDelimiter(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"10 20 3", 2},
		{"10 20 355", 2},
		{"10 20 355 ", 3},
		{"10 20 355,", 3},
		{"10 20 355、", 3},
		{"10 20 355\n", 3},
		{"10 20 30 done 4", 3},
		{"10 20 3.5 ", 2},
		{"10 20 3x ", 2},
	} {
		if got := CountIntegers(tc.text); got != tc.want {
			t.Errorf("CountIntegers(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestEnrollInvalidatesFittedParams(t *testing.T) {
	for _, id := range []string{"a", "new"} {
		t.Run(id, func(t *testing.T) {
			b := synthBank(t, 4)
			if _, err := Retrain(b); err != nil {
				t.Fatal(err)
			}
			seq := synthSeq(rand.New(rand.NewSource(123)), centersA, 300)
			batch := &Batch{EntryID: id, Family: "fam-" + id, Model: "model-" + id,
				Samples: []BatchSample{{ID: "extra", ExpectedCount: 300, Sequence: seq}},
			}
			// Imported projections and class means also depend on the old samples.
			b.Params.LDA = &LDAParams{Means: map[string][]float64{"a": {1}}}
			if added, err := b.Enroll(batch); err != nil || added != 1 {
				t.Fatalf("Enroll = %d, %v", added, err)
			}
			if b.Params.Temperature != 0 || b.Params.SeqLimits != (SeqLimits{}) || b.Params.LDA != nil {
				t.Fatalf("enrollment kept stale fitted parameters: %+v", b.Params)
			}
			res := b.Score(answersFromSeqs([][]int{seq, seq, seq}), ScoreOptions{})
			if res.Status != StatusRanked || res.Calibrated {
				t.Fatalf("changed bank must be ranking-only: %+v", res)
			}
			for _, c := range res.Candidates {
				if c.Confidence != 0 {
					t.Fatalf("changed bank retained confidence: %+v", c)
				}
			}
		})
	}
}

func TestEnrollDuplicateDoesNotChangeCalibratedBank(t *testing.T) {
	b := synthBank(t, 4)
	if _, err := Retrain(b); err != nil {
		t.Fatal(err)
	}
	params := b.Params
	sample := b.Entries[0].Samples[0]
	batch := &Batch{EntryID: "a", Family: "fam-a", Model: "model-a", Samples: []BatchSample{
		{ID: "extra", ExpectedCount: 300, Sequence: sample.Sequence},
		{ID: sample.ID, ExpectedCount: 300, Sequence: sample.Sequence},
	}}
	if added, err := b.Enroll(batch); err == nil || added != 0 {
		t.Fatalf("duplicate batch: added=%d err=%v", added, err)
	}
	if len(b.Entries[0].Samples) != 4 || b.Params != params {
		t.Fatal("failed enrollment changed the calibrated bank")
	}
}
