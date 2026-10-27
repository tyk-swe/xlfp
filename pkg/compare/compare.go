// Package compare evaluates audit outcomes: whether a target endpoint's
// fingerprinted family matches a provider's claim, and (when an official
// endpoint was also probed) how far the target's observable behavior
// diverges from it. Divergence is a signal only; this package never claims
// proof of quantization or degradation (PROJECT.md Q6).
package compare

import (
	"fmt"
	"math"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

// Audit verdicts.
const (
	VerdictMatch        = "match"
	VerdictMismatch     = "mismatch"
	VerdictInconclusive = "inconclusive"
)

// Divergence summarizes target-vs-official count differences per probe,
// normalized by each side's fitted chat-template overhead.
type Divergence struct {
	MeanAbsDiff float64           `json:"mean_abs_diff"`
	MaxAbsDiff  float64           `json:"max_abs_diff"`
	PerProbe    []ProbeDivergence `json:"per_probe"`
	Note        string            `json:"note"`
}

type ProbeDivergence struct {
	ProbeID  string  `json:"probe_id"`
	Target   int     `json:"target_tokens"`
	Official int     `json:"official_tokens"`
	AbsDiff  float64 `json:"abs_diff_overhead_normalized"`
}

// Result is the full audit outcome.
type Result struct {
	Verdict       string              `json:"verdict"`
	ClaimedFamily string              `json:"claimed_family"`
	Reason        string              `json:"reason,omitempty"`
	Target        fingerprint.Result  `json:"target"`
	Official      *fingerprint.Result `json:"official,omitempty"`
	Divergence    *Divergence         `json:"divergence,omitempty"`
	RequestsSpent int                 `json:"requests_spent"`
}

// Evaluate renders an audit verdict from a target fingerprint (and an
// optional official one) against the claimed family.
func Evaluate(db *fingerprint.DB, claimed string, target fingerprint.Result, official *fingerprint.Result) Result {
	res := Result{ClaimedFamily: claimed, Target: target, Official: official, Verdict: VerdictInconclusive}
	res.RequestsSpent = target.RequestsSpent
	if official != nil {
		res.RequestsSpent += official.RequestsSpent
		// Overhead-normalized divergence is only meaningful when both
		// overheads come from an identified fit.
		if target.Status == fingerprint.StatusIdentified && official.Status == fingerprint.StatusIdentified {
			res.Divergence = ComputeDivergence(db, target, *official)
		}
	}

	if !db.IsVerified(claimed) {
		res.Reason = fmt.Sprintf("claimed family %q is not a verified family in database v%s, so no verdict can be rendered for it", claimed, db.Version)
		return res
	}
	if target.Status != fingerprint.StatusIdentified {
		res.Reason = "target fingerprint is inconclusive: " + target.Reason
		return res
	}
	if target.TopFamily == claimed {
		res.Verdict = VerdictMatch
		res.Reason = fmt.Sprintf("target fingerprints as claimed family %s (confidence %.3f)", claimed, topConfidence(target))
	} else {
		res.Verdict = VerdictMismatch
		res.Reason = fmt.Sprintf("target fingerprints as %s (confidence %.3f), not claimed family %s", target.TopFamily, topConfidence(target), claimed)
	}
	// The reference itself is only in doubt when it disagrees with both
	// the claim and the target. If the official fingerprints as the
	// claimed family and the target does not, that is the clearest
	// mismatch, not doubt; if official and target agree with each other,
	// the fingerprint is corroborated either way.
	if official != nil {
		switch {
		case official.Status == fingerprint.StatusIdentified && official.TopFamily != claimed && official.TopFamily != target.TopFamily:
			res.Verdict = VerdictInconclusive
			res.Reason = fmt.Sprintf("official baseline fingerprints as %s, which matches neither the claim %q nor the target %s; the reference itself is in doubt", official.TopFamily, claimed, target.TopFamily)
		case official.Status != fingerprint.StatusIdentified:
			res.Reason += "; official baseline was inconclusive and did not corroborate this result"
		}
	}
	return res
}

func topConfidence(r fingerprint.Result) float64 {
	if len(r.Rankings) == 0 {
		return 0
	}
	return r.Rankings[0].Confidence
}

func overheadOf(r fingerprint.Result) float64 {
	for _, fr := range r.Rankings {
		if fr.Family == r.TopFamily {
			return fr.Overhead
		}
	}
	if len(r.Rankings) > 0 {
		return r.Rankings[0].Overhead
	}
	return 0
}

// ComputeDivergence compares per-probe prompt-token counts between target
// and official, subtracting each side's fitted template overhead so that
// different chat templates do not register as behavioral divergence. It
// follows the same observation contract as ranking: known non-special
// probes only, positive usage only, first observation per probe, both
// sides identified, and at least three overlapping probes.
func ComputeDivergence(db *fingerprint.DB, target, official fingerprint.Result) *Divergence {
	if db == nil || target.Status != fingerprint.StatusIdentified || official.Status != fingerprint.StatusIdentified {
		return nil
	}
	usable := func(obs []fingerprint.Observation) map[string]int {
		known := map[string]bool{}
		for _, p := range db.Probes {
			if !p.Special {
				known[p.ID] = true
			}
		}
		out := map[string]int{}
		for _, o := range obs {
			if _, dup := out[o.ProbeID]; dup {
				continue
			}
			if known[o.ProbeID] && o.HasUsage && o.PromptTokens > 0 && !o.Rejected {
				out[o.ProbeID] = o.PromptTokens
			}
		}
		return out
	}
	tCounts, oCounts := usable(target.Observations), usable(official.Observations)
	tOver, oOver := overheadOf(target), overheadOf(official)
	var per []ProbeDivergence
	var sum, max float64
	for _, p := range db.Probes {
		tc, tok := tCounts[p.ID]
		oc, ook := oCounts[p.ID]
		if !tok || !ook {
			continue
		}
		d := math.Abs((float64(tc) - tOver) - (float64(oc) - oOver))
		per = append(per, ProbeDivergence{ProbeID: p.ID, Target: tc, Official: oc, AbsDiff: math.Round(d*1000) / 1000})
		sum += d
		if d > max {
			max = d
		}
	}
	if len(per) < 3 {
		return nil
	}
	return &Divergence{
		MeanAbsDiff: math.Round(sum/float64(len(per))*1000) / 1000,
		MaxAbsDiff:  math.Round(max*1000) / 1000,
		PerProbe:    per,
		Note:        "token-count divergence is a signal only; it does not by itself prove quantization or degradation of a same-family backend",
	}
}
