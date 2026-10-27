package fingerprint

import (
	"fmt"
	"math"
	"sort"
)

// Result statuses for uncover-style fingerprinting.
const (
	StatusIdentified   = "identified"
	StatusInconclusive = "inconclusive"
)

// Machine-stable reason codes for inconclusive results. Reason carries the
// full technical explanation; ReasonCode lets renderers and agents switch
// on the cause without parsing prose. Identified results have no code.
const (
	ReasonInsufficientCounts  = "insufficient_counts" // too few usable token counts
	ReasonNoVerifiedFamilies  = "no_verified_families"
	ReasonUnreliableCounts    = "unreliable_counts" // counts vary implausibly little
	ReasonImplausibleOverhead = "implausible_overhead"
	ReasonScaledCounts        = "scaled_counts" // counts look scaled/estimated
	ReasonInconsistentCounts  = "inconsistent_counts"
	ReasonPoorFit             = "poor_fit" // no known family fits
	ReasonSharedTokenizer     = "shared_tokenizer"
	ReasonUnverifiedBest      = "unverified_best"
	ReasonLowConfidence       = "low_confidence"
)

// Ranking thresholds. They are deliberately conservative so thin or
// tied evidence yields inconclusive.
const (
	DefaultMinConfidence = 0.70 // minimum top confidence for a verdict
	StopConfidence       = 0.90 // adaptive probing may stop at/above this
	MaxVerdictMAE        = 2.5  // best-fit raw error above this is inconclusive
	MinUsableProbes      = 3    // fewer usable counts than this cannot be ranked
	MinVerdictProbes     = 6    // a verdict needs at least this many usable counts
	MaxVerdictResidual   = 10.0 // one probe off by more than this blocks a verdict
	MinPlausibleOverhead = -2.0 // template overhead cannot meaningfully be negative
	MaxPlausibleOverhead = 8192.0
	MinPlausibleSlope    = 0.8 // observed-vs-expected count scaling bounds
	MaxPlausibleSlope    = 1.25
)

// Observation is the outcome of sending one probe to an endpoint.
type Observation struct {
	ProbeID      string `json:"probe_id"`
	PromptTokens int    `json:"prompt_tokens,omitempty"`
	HasUsage     bool   `json:"has_usage"`
	Rejected     bool   `json:"rejected,omitempty"` // endpoint refused the probe (special tokens)
	HTTPStatus   int    `json:"http_status,omitempty"`
	Error        string `json:"error,omitempty"`
}

// UsableCount reports whether a successful observation carries usable usage.
// HTTPStatus zero is allowed for locally constructed observations.
func (o Observation) UsableCount() bool {
	return o.HasUsage && o.PromptTokens > 0 && !o.Rejected && o.Error == "" &&
		(o.HTTPStatus == 0 || (o.HTTPStatus >= 200 && o.HTTPStatus < 300))
}

// FamilyRank is the scored evidence for one candidate family.
type FamilyRank struct {
	Family     string   `json:"family"`
	Display    string   `json:"display"`
	EntryID    string   `json:"entry_id"` // best-fitting entry of this family
	Tokenizer  string   `json:"tokenizer"`
	Verified   bool     `json:"verified"`
	Confidence float64  `json:"confidence"`
	MAE        float64  `json:"mae"`      // mean |residual| after overhead fit; lower is better
	Overhead   float64  `json:"overhead"` // fitted constant chat-template overhead
	MaxResid   float64  `json:"max_residual"`
	Slope      float64  `json:"slope"` // observed-vs-expected scaling; ~1 for a true match
	SharedWith []string `json:"shared_with,omitempty"`
}

// Result is the full fingerprint outcome for one endpoint.
type Result struct {
	Status        string        `json:"status"` // StatusIdentified | StatusInconclusive
	TopFamily     string        `json:"top_family,omitempty"`
	Reason        string        `json:"reason,omitempty"`
	ReasonCode    string        `json:"reason_code,omitempty"` // stable machine-readable cause (see Reason* consts)
	Rankings      []FamilyRank  `json:"rankings"`              // verified families only
	Unverified    []FamilyRank  `json:"unverified_rankings"`   // scored, never rendered as verdicts
	Observations  []Observation `json:"observations"`
	RequestsSpent int           `json:"requests_spent"`
	DBVersion     string        `json:"db_version"`
}

// entryScore is the intermediate per-entry fit.
type entryScore struct {
	entry    Entry
	mae      float64 // special-probe-adjusted MAE used for ranking order
	rawMAE   float64 // unadjusted MAE used for verdict gates
	overhead float64
	maxResid float64
	slope    float64
	usable   int
}

// Rank scores observations against the database. minConfidence <= 0 uses
// DefaultMinConfidence. Ranking fits a constant per-endpoint overhead
// (chat-template tokens) as the median of (observed - expected), then
// measures the mean absolute residual (MAE). Confidence is a softmax over
// exp(-MAE*n/4) across families, where n is the number of usable probes:
// evidence accumulates per probe, so an exact fit over many probes is far
// stronger than the same MAE gap over few probes.
func (db *DB) Rank(obs []Observation, minConfidence float64) Result {
	if math.IsNaN(minConfidence) || math.IsInf(minConfidence, 0) || minConfidence <= 0 || minConfidence > 1 {
		minConfidence = DefaultMinConfidence
	}
	res := Result{Status: StatusInconclusive, Observations: obs, DBVersion: db.Version}
	for _, o := range obs {
		if o.HTTPStatus != 0 || o.Error != "" || o.HasUsage || o.Rejected {
			res.RequestsSpent++
		}
	}

	probeIdx := make(map[string]int, len(db.Probes))
	for i, p := range db.Probes {
		probeIdx[p.ID] = i
	}
	type obsAt struct {
		idx int
		o   Observation
	}
	var usable []obsAt
	// First observation per probe wins; duplicates never double-count.
	// Special-token probes contribute acceptance/rejection evidence only:
	// servers differ on whether the literal text is parsed as special
	// tokens, so their counts are excluded from the MAE fit.
	specialOutcome := map[string]Observation{}
	seenProbe := map[string]bool{}
	for _, o := range obs {
		i, ok := probeIdx[o.ProbeID]
		if !ok || seenProbe[o.ProbeID] {
			continue
		}
		seenProbe[o.ProbeID] = true
		if db.Probes[i].Special {
			specialOutcome[o.ProbeID] = o
			continue
		}
		if o.UsableCount() {
			usable = append(usable, obsAt{i, o})
		}
	}
	if len(usable) < MinUsableProbes {
		res.ReasonCode = ReasonInsufficientCounts
		res.Reason = fmt.Sprintf("only %d usable token counts (usage missing, non-positive, or probes rejected); at least %d are required", len(usable), MinUsableProbes)
		res.Rankings, res.Unverified = []FamilyRank{}, []FamilyRank{}
		return res
	}

	scoreEntry := func(e Entry) entryScore {
		diffs := make([]float64, 0, len(usable))
		for _, u := range usable {
			diffs = append(diffs, float64(u.o.PromptTokens-e.Counts[u.idx]))
		}
		overhead := median(diffs)
		var sum, maxResid float64
		for _, d := range diffs {
			r := math.Abs(d - overhead)
			sum += r
			maxResid = math.Max(maxResid, r)
		}
		rawMAE := sum / float64(len(diffs))
		mae := rawMAE
		// Least-squares slope of observed on expected (scale plausibility).
		var meanE, meanO float64
		for _, u := range usable {
			meanE += float64(e.Counts[u.idx])
			meanO += float64(u.o.PromptTokens)
		}
		meanE /= float64(len(usable))
		meanO /= float64(len(usable))
		var cov, varE float64
		for _, u := range usable {
			dE := float64(e.Counts[u.idx]) - meanE
			cov += dE * (float64(u.o.PromptTokens) - meanO)
			varE += dE * dE
		}
		slope := 1.0
		if varE > 0 {
			slope = cov / varE
		}
		// Special-token evidence is a small symmetric tie-breaker only:
		// it is scaled by 1/n so its total weight equals half of one
		// ordinary probe, and verdict gates use rawMAE rather than mae.
		expectReject := map[string]bool{}
		for _, rp := range e.RejectProbes {
			expectReject[rp] = true
		}
		for id, o := range specialOutcome {
			if o.Error != "" || (!o.Rejected && !o.UsableCount()) {
				continue
			}
			want, got := expectReject[id], o.Rejected
			unit := 0.5 / float64(len(usable))
			if want == got {
				mae -= unit
			} else {
				mae += unit
			}
		}
		if mae < 0 {
			mae = 0
		}
		return entryScore{entry: e, mae: mae, rawMAE: rawMAE, overhead: overhead, maxResid: maxResid, slope: slope, usable: len(usable)}
	}

	// Best entry per family. A family is verified when any of its entries
	// is (consistent with DB.IsVerified); its representative is then its
	// best verified entry.
	famVerified := map[string]bool{}
	for _, e := range db.Entries {
		famVerified[e.Family] = famVerified[e.Family] || e.Verified
	}
	best := map[string]entryScore{}
	for _, e := range db.Entries {
		if famVerified[e.Family] && !e.Verified {
			continue
		}
		s := scoreEntry(e)
		if cur, ok := best[e.Family]; !ok || s.mae < cur.mae {
			best[e.Family] = s
		}
	}
	// One softmax over all families keeps confidences globally comparable;
	// the verified/unverified split happens afterwards.
	allFams := make([]entryScore, 0, len(best))
	for _, s := range best {
		allFams = append(allFams, s)
	}
	sort.Slice(allFams, func(i, j int) bool {
		if allFams[i].mae != allFams[j].mae {
			return allFams[i].mae < allFams[j].mae
		}
		return allFams[i].entry.Family < allFams[j].entry.Family
	})
	var denom float64
	weights := make([]float64, len(allFams))
	for i, s := range allFams {
		// Shift by the best score so at least one weight is always 1.
		weights[i] = math.Exp(-(s.mae - allFams[0].mae) * float64(len(usable)) / 4)
		denom += weights[i]
	}
	res.Rankings, res.Unverified = []FamilyRank{}, []FamilyRank{}
	for i, s := range allFams {
		conf := 0.0
		if denom > 0 {
			conf = weights[i] / denom
		}
		fr := FamilyRank{
			Family: s.entry.Family, Display: s.entry.Display, EntryID: s.entry.ID,
			Tokenizer: s.entry.Tokenizer, Verified: famVerified[s.entry.Family],
			Confidence: conf, MAE: round3(s.mae), Overhead: round3(s.overhead),
			MaxResid: round3(s.maxResid), Slope: round3(s.slope),
		}
		for _, o := range db.Entries {
			if o.Family != s.entry.Family && s.entry.sameCounts(o) {
				if !contains(fr.SharedWith, o.Family) {
					fr.SharedWith = append(fr.SharedWith, o.Family)
				}
			}
		}
		sort.Strings(fr.SharedWith)
		if fr.Verified {
			res.Rankings = append(res.Rankings, fr)
		} else {
			res.Unverified = append(res.Unverified, fr)
		}
	}
	if len(res.Rankings) == 0 {
		res.ReasonCode = ReasonNoVerifiedFamilies
		res.Reason = "no verified families in database"
		return res
	}

	top := res.Rankings[0]
	// Plausibility guard against fabricated usage: real tokenizers
	// produce counts that vary with probe content roughly as much as the
	// best entry predicts. (Near-)constant counts fit a low-variance
	// entry spuriously after the overhead shift, so they are declared
	// unreliable rather than identified.
	obsMin, obsMax := math.Inf(1), math.Inf(-1)
	expMin, expMax := math.Inf(1), math.Inf(-1)
	topEntry := best[top.Family].entry
	for _, u := range usable {
		o := float64(u.o.PromptTokens)
		obsMin, obsMax = math.Min(obsMin, o), math.Max(obsMax, o)
		e := float64(topEntry.Counts[u.idx])
		expMin, expMax = math.Min(expMin, e), math.Max(expMax, e)
	}
	obsRange, expRange := obsMax-obsMin, expMax-expMin
	degenerate := (expRange >= 6 && obsRange <= math.Max(1, expRange/4)) || (expRange >= 10 && obsRange < expRange/2)
	sharedTie := ""
	for _, r := range res.Rankings[1:] {
		if contains(top.SharedWith, r.Family) {
			sharedTie = r.Family
			break
		}
	}
	unverifiedBetter := len(res.Unverified) > 0 && res.Unverified[0].MAE <= MaxVerdictMAE && res.Unverified[0].MAE+1.0 < top.MAE
	topScore := best[top.Family]
	setReason := func(code, reason string) { res.ReasonCode, res.Reason = code, reason }
	switch {
	case degenerate:
		setReason(ReasonUnreliableCounts, fmt.Sprintf("reported counts vary implausibly little (observed range %.0f vs expected %.0f for %s); usage reporting is unreliable or fabricated", obsRange, expRange, top.Family))
	case len(usable) < MinVerdictProbes:
		setReason(ReasonInsufficientCounts, fmt.Sprintf("only %d usable token counts; at least %d are required for a verdict", len(usable), MinVerdictProbes))
	case topScore.overhead < MinPlausibleOverhead || topScore.overhead > MaxPlausibleOverhead:
		setReason(ReasonImplausibleOverhead, fmt.Sprintf("fitted chat-template overhead %.1f for %s is implausible (allowed %.0f..%.0f); counts are not consistent with raw-text tokenization plus a template", topScore.overhead, top.Family, MinPlausibleOverhead, MaxPlausibleOverhead))
	case topScore.slope < MinPlausibleSlope || topScore.slope > MaxPlausibleSlope:
		setReason(ReasonScaledCounts, fmt.Sprintf("observed counts scale by %.2f relative to %s (allowed %.2f..%.2f); usage appears scaled or estimated rather than counted", topScore.slope, top.Family, MinPlausibleSlope, MaxPlausibleSlope))
	case topScore.maxResid > MaxVerdictResidual:
		setReason(ReasonInconsistentCounts, fmt.Sprintf("one probe deviates by %.1f tokens from %s after overhead fit (max allowed %.0f); evidence is internally inconsistent", topScore.maxResid, top.Family, MaxVerdictResidual))
	case topScore.rawMAE > MaxVerdictMAE:
		reason := fmt.Sprintf("best family %s still fits poorly (MAE %.2f > %.2f); unknown tokenizer or unreliable usage reporting", top.Family, topScore.rawMAE, MaxVerdictMAE)
		if unverifiedBetter {
			reason += fmt.Sprintf("; closest overall match is unverified family %s (MAE %.2f), which cannot be rendered as a verdict", res.Unverified[0].Family, res.Unverified[0].MAE)
		}
		setReason(ReasonPoorFit, reason)
	case sharedTie != "":
		setReason(ReasonSharedTokenizer, fmt.Sprintf("families %s and %s share an identical tokenizer and cannot be separated by token counts", top.Family, sharedTie))
	case unverifiedBetter:
		setReason(ReasonUnverifiedBest, fmt.Sprintf("closest overall match is unverified family %s (MAE %.2f), ahead of verified %s (MAE %.2f); unverified families cannot be rendered as verdicts", res.Unverified[0].Family, res.Unverified[0].MAE, top.Family, top.MAE))
	case top.Confidence < minConfidence:
		setReason(ReasonLowConfidence, fmt.Sprintf("top family %s confidence %.3f is below the %.3f verdict threshold", top.Family, top.Confidence, minConfidence))
	default:
		res.Status = StatusIdentified
		res.TopFamily = top.Family
	}
	return res
}

// ShouldStop reports whether adaptive probing may stop after a stage.
func (r Result) ShouldStop() bool {
	return r.Status == StatusIdentified && len(r.Rankings) > 0 && r.Rankings[0].Confidence >= StopConfidence && r.Rankings[0].MAE <= 1.0
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
