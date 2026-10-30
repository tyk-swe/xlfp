package numprint

import (
	"fmt"
	"math"
	"sort"
)

// Answer is one model reply to score, live or loaded offline.
type Answer struct {
	ChallengeID   string `json:"challenge_id,omitempty"`
	ExpectedCount int    `json:"expected_count,omitempty"`
	Text          string `json:"text"`
	// Truncated marks a reply stopped early (streaming cap);
	// Completed marks a naturally finished reply. In strict mode
	// only completed, non-truncated answers count.
	Truncated bool   `json:"truncated,omitempty"`
	Completed bool   `json:"completed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Candidate is one ranked bank entry.
type Candidate struct {
	EntryID    string  `json:"entry_id"`
	Family     string  `json:"family"`
	Display    string  `json:"display"`
	Model      string  `json:"model"`
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence,omitempty"` // only when calibrated
}

// ReplyCheck reports the sequence-shape check outcome.
type ReplyCheck struct {
	AutocorrMedian  float64 `json:"autocorr_median"`
	SmallStepMedian float64 `json:"small_step_median"`
	Passed          bool    `json:"passed"`
}

// Result is the number-fingerprint outcome for one round.
type Result struct {
	Status       string      `json:"status"`
	Reason       string      `json:"reason,omitempty"`
	ReasonCode   string      `json:"reason_code,omitempty"`
	TopEntryID   string      `json:"top_entry_id,omitempty"`
	TopFamily    string      `json:"top_family,omitempty"`
	Candidates   []Candidate `json:"candidates"`
	Calibrated   bool        `json:"calibrated"`
	ValidAnswers int         `json:"valid_answers"`
	ReplyCheck   *ReplyCheck `json:"reply_check,omitempty"`
	Warnings     []string    `json:"warnings,omitempty"`
	BankVersion  string      `json:"bank_version"`
}

// ScoreOptions controls scoring strictness.
type ScoreOptions struct {
	Strict bool
}

type parsedAnswer struct {
	seq   []int
	feat  []float64
	stats SequenceStats
}

// parseAnswers keeps scoreable answers under the strictness rules:
// relaxed mode trims over-long replies to the requested count and
// accepts early-stopped ones; strict mode requires naturally
// completed replies and never trims.
func parseAnswers(answers []Answer, opts ScoreOptions) []parsedAnswer {
	var out []parsedAnswer
	for _, a := range answers {
		if a.Error != "" {
			continue
		}
		if opts.Strict && (a.Truncated || !a.Completed) {
			continue
		}
		seq := ParseSequence(a.Text)
		if !opts.Strict && a.ExpectedCount > 0 && len(seq) > a.ExpectedCount {
			seq = seq[:a.ExpectedCount]
		}
		if ValidateSequence(seq, a.ExpectedCount) != nil {
			continue
		}
		out = append(out, parsedAnswer{seq: seq, feat: Featurize(seq), stats: Stats(seq)})
	}
	return out
}

// entryFeatures featurizes an entry's samples, optionally excluding
// one sample index (leave-one-out in training).
func entryFeatures(e Entry, exclude int) [][]float64 {
	var out [][]float64
	for i, s := range e.Samples {
		if i == exclude {
			continue
		}
		out = append(out, Featurize(s.Sequence))
	}
	return out
}

// combinedScores returns the ensemble score per eligible entry.
// Each scorer (centroid cosine, kNN distance, optional LDA) is
// z-standardized across entries and averaged with equal weight, so
// no scorer's raw scale can dominate.
func combinedScores(entries []Entry, entryFeats [][][]float64, answers []parsedAnswer, lda *LDAParams) []float64 {
	n := len(entries)
	scorers := [][]float64{}
	// Centroid cosine, averaged over answers.
	centroid := make([]float64, n)
	for ei := range entries {
		c := meanVec(entryFeats[ei])
		sum := 0.0
		for _, a := range answers {
			sum += cosine(a.feat, c)
		}
		centroid[ei] = sum / float64(len(answers))
	}
	scorers = append(scorers, centroid)
	// kNN: mean of the 7 smallest squared distances, median over
	// answers, negated so higher is better.
	knn := make([]float64, n)
	for ei := range entries {
		var per []float64
		for _, a := range answers {
			var ds []float64
			for _, f := range entryFeats[ei] {
				ds = append(ds, sqDist(a.feat, f))
			}
			sort.Float64s(ds)
			k := min(7, len(ds))
			sum := 0.0
			for i := 0; i < k; i++ {
				sum += ds[i]
			}
			per = append(per, sum/float64(k))
		}
		knn[ei] = -medianFloat(per)
	}
	scorers = append(scorers, knn)
	// Optional LDA discriminant over the mean answer feature.
	if lda != nil && len(lda.Projection) == featureDims {
		ldaScores := make([]float64, n)
		feats := make([][]float64, len(answers))
		for i, a := range answers {
			feats[i] = a.feat
		}
		proj := projectLDA(lda, meanVec(feats))
		ok := true
		for ei, e := range entries {
			m, has := lda.Means[e.ID]
			if !has || len(m) != len(proj) {
				ok = false
				break
			}
			ldaScores[ei] = -sqDist(proj, m)
		}
		if ok {
			scorers = append(scorers, ldaScores)
		}
	}
	combined := make([]float64, n)
	for _, s := range scorers {
		z := zscore(s)
		for i := range combined {
			combined[i] += z[i] / float64(len(scorers))
		}
	}
	return combined
}

func projectLDA(lda *LDAParams, feat []float64) []float64 {
	if len(lda.Projection) == 0 || len(feat) != len(lda.Projection) {
		return nil
	}
	axes := len(lda.Projection[0])
	out := make([]float64, axes)
	for i, row := range lda.Projection {
		for j := 0; j < axes && j < len(row); j++ {
			out[j] += feat[i] * row[j]
		}
	}
	return out
}

func zscore(xs []float64) []float64 {
	out := make([]float64, len(xs))
	if len(xs) == 0 {
		return out
	}
	mean := 0.0
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	varr := 0.0
	for _, x := range xs {
		varr += (x - mean) * (x - mean)
	}
	varr /= float64(len(xs))
	sd := math.Sqrt(varr)
	if sd <= 0 {
		return out
	}
	for i, x := range xs {
		out[i] = (x - mean) / sd
	}
	return out
}

func softmaxTemp(scores []float64, temp float64) []float64 {
	out := make([]float64, len(scores))
	if len(scores) == 0 || temp <= 0 {
		return out
	}
	m := math.Inf(-1)
	for _, s := range scores {
		m = math.Max(m, s/temp)
	}
	sum := 0.0
	for i, s := range scores {
		out[i] = math.Exp(s/temp - m)
		sum += out[i]
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}

// Score ranks the bank entries for one round of answers. The
// sequence-shape check runs first: when the median lag-1
// autocorrelation and the median small-step share both exceed the
// fitted (or provisional) limits, the replies do not look like
// language-model output and no ranking is produced.
func (b *Bank) Score(answers []Answer, opts ScoreOptions) Result {
	res := Result{Status: StatusInconclusive, Candidates: []Candidate{}, BankVersion: b.Version}
	entries := b.EligibleEntries()
	if len(entries) < 2 {
		res.ReasonCode = ReasonInsufficientBank
		res.Reason = fmt.Sprintf("reference bank has %d entries with samples; at least 2 are required (collect with `xlfp sample`, enroll, then retrain)", len(entries))
		return res
	}
	parsed := parseAnswers(answers, opts)
	res.ValidAnswers = len(parsed)
	if len(parsed) == 0 {
		res.ReasonCode = ReasonInsufficientAnswers
		res.Reason = "no answer contained enough valid integers to score"
		return res
	}
	var auts, steps []float64
	for _, p := range parsed {
		auts = append(auts, p.stats.Autocorr)
		steps = append(steps, p.stats.SmallStep)
		if span := p.stats.Max - p.stats.Min; span < (ValueMax-ValueMin)*2/5 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("one answer covers an abnormally narrow value range (%d..%d); its distribution is unreliable", p.stats.Min, p.stats.Max))
		}
	}
	limits := b.seqLimits()
	check := ReplyCheck{AutocorrMedian: round4(medianFloat(auts)), SmallStepMedian: round4(medianFloat(steps)), Passed: true}
	if check.AutocorrMedian > limits.Autocorr && check.SmallStepMedian > limits.SmallStep {
		check.Passed = false
		res.Status = StatusNotModelReply
		res.ReasonCode = ReasonNotModelReply
		res.Reason = "the number sequences do not look like language-model replies (weak value jumps and many small steps); program-generated or sorted numbers are not ranked"
		res.ReplyCheck = &check
		return res
	}
	res.ReplyCheck = &check
	entryFeats := make([][][]float64, len(entries))
	for i, e := range entries {
		entryFeats[i] = entryFeatures(e, -1)
	}
	scores := combinedScores(entries, entryFeats, parsed, b.Params.LDA)
	// Imported LDA parameters have no independent calibration data.
	// Also guard older banks that retained a temperature fitted with leakage.
	res.Calibrated = b.Params.LDA == nil && b.Params.Temperature > 0 && len(parsed) >= 3
	var confs []float64
	if res.Calibrated {
		confs = softmaxTemp(scores, b.Params.Temperature)
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(x, y int) bool {
		if scores[order[x]] != scores[order[y]] {
			return scores[order[x]] > scores[order[y]]
		}
		return entries[order[x]].ID < entries[order[y]].ID
	})
	for _, ei := range order {
		e := entries[ei]
		c := Candidate{EntryID: e.ID, Family: e.Family, Display: e.Display, Model: e.Model, Score: round4(scores[ei])}
		if res.Calibrated {
			c.Confidence = round4(confs[ei])
		}
		res.Candidates = append(res.Candidates, c)
	}
	res.Status = StatusRanked
	res.TopEntryID = res.Candidates[0].EntryID
	res.TopFamily = res.Candidates[0].Family
	if !res.Calibrated {
		res.Reason = "ranking only: confidence needs a fitted calibration and at least 3 valid answers"
		if b.Params.LDA != nil {
			res.Reason = "ranking only: the imported LDA model has no independent confidence calibration"
		}
	}
	return res
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }
