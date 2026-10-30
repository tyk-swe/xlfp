package numprint

import (
	"fmt"
	"math"
	"sort"
)

// EvalReport summarizes single-answer leave-one-out evaluation and
// held-out-round calibration from Retrain.
type EvalReport struct {
	Samples     int                `json:"samples"`
	Top1        int                `json:"top1_correct"`
	Accuracy    float64            `json:"accuracy"`
	PerEntry    map[string]float64 `json:"per_entry_accuracy"`
	Temperature float64            `json:"temperature"`
	SeqLimits   SeqLimits          `json:"seq_limits"`
	LDAExcluded bool               `json:"lda_excluded,omitempty"` // accuracy covers centroid/kNN only; LDA confidence remains uncalibrated
}

// quantile returns the q-quantile (nearest-rank) of xs.
func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	idx := int(math.Ceil(q*float64(len(s)))) - 1
	idx = min(max(idx, 0), len(s)-1)
	return s[idx]
}

// fitSeqLimits derives reply-check limits from the bank: samples
// are grouped by (entry, environment) into triples, per-group
// medians are collected, and the limits are their 99.5% quantiles —
// real model replies in the bank almost never exceed them on both
// statistics at once.
func fitSeqLimits(entries []Entry) SeqLimits {
	var auts, steps []float64
	for _, e := range entries {
		byEnv := map[string][]Sample{}
		var envOrder []string
		for _, s := range e.Samples {
			if _, ok := byEnv[s.Environment]; !ok {
				envOrder = append(envOrder, s.Environment)
			}
			byEnv[s.Environment] = append(byEnv[s.Environment], s)
		}
		for _, env := range envOrder {
			samples := byEnv[env]
			for i := 0; i+3 <= len(samples); i += 3 {
				var ga, gs []float64
				for _, s := range samples[i : i+3] {
					st := Stats(evidenceSequence(s.Sequence, s.ExpectedCount))
					ga = append(ga, st.Autocorr)
					gs = append(gs, st.SmallStep)
				}
				auts = append(auts, medianFloat(ga))
				steps = append(steps, medianFloat(gs))
			}
		}
	}
	if len(auts) == 0 {
		// Fall back to per-sample statistics.
		for _, e := range entries {
			for _, s := range e.Samples {
				st := Stats(evidenceSequence(s.Sequence, s.ExpectedCount))
				auts = append(auts, st.Autocorr)
				steps = append(steps, st.SmallStep)
			}
		}
	}
	if len(auts) == 0 {
		return provisionalSeqLimits()
	}
	return SeqLimits{Autocorr: round4(quantile(auts, 0.995)), SmallStep: round4(quantile(steps, 0.995)), Fitted: true}
}

// looScores computes leave-one-out combined scores for every sample
// scored as a single answer against the bank without itself.
// Returns scores[sample] aligned with entries, plus the true entry
// index per sample.
func looScores(entries []Entry) ([][]float64, []int) {
	allFeats := make([][][]float64, len(entries))
	for i, e := range entries {
		allFeats[i] = entryFeatures(e, -1)
	}
	var out [][]float64
	var truth []int
	for ei, e := range entries {
		for si := range e.Samples {
			feats := make([][][]float64, len(entries))
			for j := range entries {
				if j == ei {
					feats[j] = entryFeatures(entries[j], si)
				} else {
					feats[j] = allFeats[j]
				}
				if len(feats[j]) == 0 {
					feats[j] = allFeats[j]
				}
			}
			s := e.Samples[si]
			seq := evidenceSequence(s.Sequence, s.ExpectedCount)
			pa := parsedAnswer{seq: seq, feat: Featurize(seq), stats: Stats(seq)}
			// An imported LDA projection and its class means were fitted
			// on the full bank, including this held-out sample and label.
			out = append(out, combinedScores(entries, feats, []parsedAnswer{pa}, nil))
			truth = append(truth, ei)
		}
	}
	return out, truth
}

const calibrationAnswers = 3

// calibrationScores holds out complete three-answer rounds from the
// reference features and uses exactly the inference aggregation.
func calibrationScores(entries []Entry) ([][]float64, []int) {
	all := make([][][]float64, len(entries))
	for i, e := range entries {
		if len(e.Samples) <= calibrationAnswers {
			return nil, nil // A held-out round must leave reference samples.
		}
		all[i] = entryFeatures(e, -1)
	}
	var scores [][]float64
	var truth []int
	for ei, e := range entries {
		for start := 0; start+calibrationAnswers <= len(e.Samples); start += calibrationAnswers {
			end := start + calibrationAnswers
			feats := append([][][]float64(nil), all...)
			feats[ei] = append([][]float64(nil), all[ei][:start]...)
			feats[ei] = append(feats[ei], all[ei][end:]...)
			var answers []parsedAnswer
			for _, f := range all[ei][start:end] {
				answers = append(answers, parsedAnswer{feat: f})
			}
			scores = append(scores, combinedScores(entries, feats, answers, nil))
			truth = append(truth, ei)
		}
	}
	return scores, truth
}

// fitTemperature minimizes held-out negative log-likelihood, allowing
// a uniform baseline. Zero means no candidate improved on chance.
func fitTemperature(scores [][]float64, truth []int) float64 {
	if len(scores) == 0 {
		return 0
	}
	bestT, bestNLL := 0.0, math.Log(float64(len(scores[0])))
	for _, t := range []float64{0.05, 0.08, 0.12, 0.18, 0.27, 0.4, 0.6, 0.9, 1.35, 2.0, 4, 8, 16, 32, 64, 128} {
		nll := 0.0
		for i, s := range scores {
			p := softmaxTemp(s, t)[truth[i]]
			nll += -math.Log(math.Max(p, 1e-12))
		}
		nll /= float64(len(scores))
		if nll < bestNLL-1e-9 {
			bestNLL, bestT = nll, t
		}
	}
	return bestT
}

// Retrain refits the detector parameters from the bank's own
// samples: reply-check limits and the calibration temperature,
// with a leave-one-out evaluation report. Imported LDA models are
// excluded from evaluation and remain uncalibrated. Every eligible
// entry needs at least 3 samples for evaluation, and at least 4 to
// leave reference data after holding out a calibration round.
func Retrain(b *Bank) (EvalReport, error) {
	entries := b.EligibleEntries()
	if len(entries) < 2 {
		return EvalReport{}, fmt.Errorf("retrain: bank has %d entries with samples, need at least 2", len(entries))
	}
	for _, e := range entries {
		if len(e.Samples) < 3 {
			return EvalReport{}, fmt.Errorf("retrain: entry %s has %d samples, need at least 3", e.ID, len(e.Samples))
		}
	}
	limits := fitSeqLimits(entries)
	scores, truth := looScores(entries)
	temp := 0.0
	if b.Params.LDA == nil {
		calScores, calTruth := calibrationScores(entries)
		temp = fitTemperature(calScores, calTruth)
	}
	rep := EvalReport{
		Samples: len(scores), Temperature: temp, SeqLimits: limits,
		PerEntry: map[string]float64{}, LDAExcluded: b.Params.LDA != nil,
	}
	perTotal := map[string]int{}
	perCorrect := map[string]int{}
	for i, s := range scores {
		top := 0
		for j := range s {
			if s[j] > s[top] {
				top = j
			}
		}
		perTotal[entries[truth[i]].ID]++
		if top == truth[i] {
			rep.Top1++
			perCorrect[entries[truth[i]].ID]++
		}
	}
	if rep.Samples > 0 {
		rep.Accuracy = round4(float64(rep.Top1) / float64(rep.Samples))
	}
	for id, total := range perTotal {
		rep.PerEntry[id] = round4(float64(perCorrect[id]) / float64(total))
	}
	b.Params.SeqLimits = limits
	b.Params.Temperature = temp
	b.Params.CalibrationAnswers = 0
	if temp > 0 {
		b.Params.CalibrationAnswers = calibrationAnswers
	}
	return rep, b.Validate()
}

// FeatureExport is the interchange format for the offline LDA
// trainer: one labeled feature vector per bank sample.
type FeatureExport struct {
	Version string             `json:"version"`
	Dims    int                `json:"dims"`
	Rows    []FeatureExportRow `json:"rows"`
}

type FeatureExportRow struct {
	EntryID  string    `json:"entry_id"`
	SampleID string    `json:"sample_id"`
	Feature  []float64 `json:"feature"`
}

// ExportFeatures renders every bank sample's feature vector.
func (b *Bank) ExportFeatures() FeatureExport {
	out := FeatureExport{Version: b.Version, Dims: featureDims}
	for _, e := range b.EligibleEntries() {
		for _, s := range e.Samples {
			out.Rows = append(out.Rows, FeatureExportRow{EntryID: e.ID, SampleID: s.ID, Feature: Featurize(evidenceSequence(s.Sequence, s.ExpectedCount))})
		}
	}
	return out
}
