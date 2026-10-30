package fingerprint

import (
	"fmt"
	"math"
	"sort"
)

// Tokenizer probe v2: differential counts with a Bayesian open-set
// decision. Each probe text is sent inside a fixed wrapper; the
// reported count minus a wrapper-only baseline cancels hidden input
// (chat template, injected system text, special tokens) exactly,
// instead of fitting it as a constant like Rank does. The posterior
// is computed over three kinds of atoms per database entry — exactly
// this tokenizer, an unlisted close relative of it — plus one
// unrelated-unlisted atom, so "none of the above" is a first-class
// outcome. See docs/clean-room.md for provenance of the method and
// its constants.

// Wrapper for differential probes. Both ends start/end with
// non-whitespace so a template that trims the message cannot change
// the count.
const (
	TokWrapperPrefix = "Respond with OK only.\n[[\n"
	TokWrapperSuffix = "\n]]"
)

// WrapTokText wraps one probe text for a differential request. The
// baseline request is WrapTokText("").
func WrapTokText(text string) string { return TokWrapperPrefix + text + TokWrapperSuffix }

// TokReason codes (machine-stable, mirroring the Rank reason codes).
const (
	TokReasonInsufficient  = "insufficient_counts"
	TokReasonBaselineDrift = "baseline_drift"
	TokReasonNoEntries     = "no_entries"
)

// TokVerdict values.
const (
	TokVerdictExact        = "exact"
	TokVerdictRelated      = "related"
	TokVerdictUnknown      = "unknown"
	TokVerdictInconclusive = "inconclusive"
)

// MinTokProbes is the fewest differential counts a verdict needs.
const MinTokProbes = 4

// TokParams holds the open-set posterior hyper-parameters. The zero
// value means DefaultTokParams. These are xlfp's own choices: a
// two-component discrete Laplace residual (narrow component for
// exact counting, wide component absorbing boundary merges and
// relay miscounts, plus a floor so one outlier cannot veto an
// entry), with the contamination rate and a small hidden-length
// shift marginalized out.
type TokParams struct {
	PriorExact           float64
	PriorRelated         float64
	PriorUnrelated       float64
	NarrowP              float64
	WideP                float64
	Floor                float64
	ExactContamination   []float64
	RelatedContamination []float64
	Shifts               []int
}

func DefaultTokParams() TokParams {
	return TokParams{
		PriorExact: 0.70, PriorRelated: 0.15, PriorUnrelated: 0.15,
		NarrowP: 0.04, WideP: 0.80, Floor: 1e-6,
		ExactContamination:   []float64{0.03, 0.08, 0.18},
		RelatedContamination: []float64{0.25, 0.45, 0.70},
		Shifts:               []int{-2, -1, 0, 1, 2},
	}
}

func (p TokParams) withDefaults() TokParams {
	d := DefaultTokParams()
	if p.PriorExact <= 0 {
		p.PriorExact = d.PriorExact
	}
	if p.PriorRelated <= 0 {
		p.PriorRelated = d.PriorRelated
	}
	if p.PriorUnrelated <= 0 {
		p.PriorUnrelated = d.PriorUnrelated
	}
	if p.NarrowP <= 0 || p.NarrowP >= 1 {
		p.NarrowP = d.NarrowP
	}
	if p.WideP <= 0 || p.WideP >= 1 {
		p.WideP = d.WideP
	}
	if p.Floor <= 0 {
		p.Floor = d.Floor
	}
	if len(p.ExactContamination) == 0 {
		p.ExactContamination = d.ExactContamination
	}
	if len(p.RelatedContamination) == 0 {
		p.RelatedContamination = d.RelatedContamination
	}
	if len(p.Shifts) == 0 {
		p.Shifts = d.Shifts
	}
	return p
}

// TokObservation is one differential count: wrapped minus baseline.
type TokObservation struct {
	ProbeID string `json:"probe_id"`
	Diff    int    `json:"diff"`
	HasDiff bool   `json:"has_diff"`
}

// TokCandidate is the posterior evidence for one database entry.
type TokCandidate struct {
	EntryID     string  `json:"entry_id"`
	Family      string  `json:"family"`
	Display     string  `json:"display"`
	Tokenizer   string  `json:"tokenizer"`
	Verified    bool    `json:"verified"`
	ExactProb   float64 `json:"exact_probability"`
	RelatedProb float64 `json:"related_probability"`
}

// TokClaim is the check of the probe result against the model name
// the user entered.
type TokClaim struct {
	Model       string  `json:"model"`
	Family      string  `json:"family,omitempty"`
	Probability float64 `json:"probability"`
	Status      string  `json:"status"` // consistent | inconsistent | unclear | unchecked
}

// TokResult is the full tokenizer-probe outcome for one endpoint.
type TokResult struct {
	Verdict       string           `json:"verdict"` // exact | related | unknown | inconclusive
	TopEntryID    string           `json:"top_entry_id,omitempty"`
	TopFamily     string           `json:"top_family,omitempty"`
	Probability   float64          `json:"probability"` // probability of the reported verdict class
	UnlistedProb  float64          `json:"unlisted_probability"`
	Candidates    []TokCandidate   `json:"candidates"`
	Claim         *TokClaim        `json:"claim,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	ReasonCode    string           `json:"reason_code,omitempty"`
	BaselineDrift bool             `json:"baseline_drift,omitempty"`
	Observations  []TokObservation `json:"observations"`
	RequestsSpent int              `json:"requests_spent"`
	DBVersion     string           `json:"db_version"`
	ProbeError    string           `json:"probe_error,omitempty"`
}

// expectedDiff returns the entry's expected differential count for a
// probe index.
func (e Entry) expectedDiff(idx int) int {
	if len(e.DiffCounts) == len(e.Counts) && len(e.DiffCounts) > 0 {
		return e.DiffCounts[idx]
	}
	return e.Counts[idx]
}

// laplaceDensity is the two-sided geometric (discrete Laplace)
// density at residual r.
func laplaceDensity(p float64, r int) float64 {
	if r < 0 {
		r = -r
	}
	return (1 - p) / (1 + p) * math.Pow(p, float64(r))
}

// residualDensity is the contaminated residual density.
func (tp TokParams) residualDensity(rho float64, r int) float64 {
	return (1-rho)*laplaceDensity(tp.NarrowP, r) + rho*laplaceDensity(tp.WideP, r) + tp.Floor
}

// atomLogLik marginalizes contamination and hidden-length shift for
// one entry atom over all observations.
func (tp TokParams) atomLogLik(e Entry, obs []tokObsAt, grid []float64) float64 {
	terms := make([]float64, 0, len(grid)*len(tp.Shifts))
	for _, rho := range grid {
		for _, shift := range tp.Shifts {
			ll := 0.0
			for _, o := range obs {
				d := tp.residualDensity(rho, o.diff-shift-e.expectedDiff(o.idx))
				ll += math.Log(d)
			}
			terms = append(terms, ll)
		}
	}
	return logSumExp(terms) - math.Log(float64(len(terms)))
}

// unrelatedLogLik scores the "unrelated to every entry" atom: per
// probe, the smoothed average predictive density over all entries
// under the related (wide) contamination grid.
func (tp TokParams) unrelatedLogLik(entries []Entry, obs []tokObsAt) float64 {
	ll := 0.0
	for _, o := range obs {
		sum := 0.0
		for _, e := range entries {
			dens := 0.0
			for _, rho := range tp.RelatedContamination {
				for _, shift := range tp.Shifts {
					dens += tp.residualDensity(rho, o.diff-shift-e.expectedDiff(o.idx))
				}
			}
			sum += dens / float64(len(tp.RelatedContamination)*len(tp.Shifts))
		}
		avg := sum / float64(len(entries))
		if avg <= 0 {
			avg = tp.Floor
		}
		ll += math.Log(avg)
	}
	return ll
}

type tokObsAt struct {
	idx  int
	diff int
}

type tokPosterior struct {
	exact     []float64 // per entry
	related   []float64 // per entry
	unrelated float64
}

func (db *DB) tokPosterior(obs []TokObservation, tp TokParams) (tokPosterior, []tokObsAt) {
	probeIdx := make(map[string]int, len(db.Probes))
	for i, p := range db.Probes {
		probeIdx[p.ID] = i
	}
	seen := map[string]bool{}
	var at []tokObsAt
	for _, o := range obs {
		i, ok := probeIdx[o.ProbeID]
		if !ok || seen[o.ProbeID] || !o.HasDiff || db.Probes[i].Special {
			continue
		}
		seen[o.ProbeID] = true
		at = append(at, tokObsAt{idx: i, diff: o.Diff})
	}
	n := len(db.Entries)
	post := tokPosterior{exact: make([]float64, n), related: make([]float64, n)}
	if n == 0 || len(at) == 0 {
		return post, at
	}
	logs := make([]float64, 0, 2*n+1)
	for _, e := range db.Entries {
		logs = append(logs, math.Log(tp.PriorExact/float64(n))+tp.atomLogLik(e, at, tp.ExactContamination))
	}
	for _, e := range db.Entries {
		logs = append(logs, math.Log(tp.PriorRelated/float64(n))+tp.atomLogLik(e, at, tp.RelatedContamination))
	}
	logs = append(logs, math.Log(tp.PriorUnrelated)+tp.unrelatedLogLik(db.Entries, at))
	total := logSumExp(logs)
	for i := range db.Entries {
		post.exact[i] = math.Exp(logs[i] - total)
		post.related[i] = math.Exp(logs[n+i] - total)
	}
	post.unrelated = math.Exp(logs[2*n] - total)
	return post, at
}

// TokRank computes the open-set tokenizer verdict for differential
// observations. model, when non-empty, adds the claim check.
func (db *DB) TokRank(obs []TokObservation, model string, params TokParams) TokResult {
	tp := params.withDefaults()
	res := TokResult{Verdict: TokVerdictInconclusive, Observations: obs, DBVersion: db.Version, Candidates: []TokCandidate{}}
	if len(db.Entries) == 0 {
		res.ReasonCode = TokReasonNoEntries
		res.Reason = "no tokenizer entries in database"
		return res
	}
	post, at := db.tokPosterior(obs, tp)
	unlisted := post.unrelated
	bestExact, bestRelated := -1, -1
	for i, e := range db.Entries {
		unlisted += post.related[i]
		res.Candidates = append(res.Candidates, TokCandidate{
			EntryID: e.ID, Family: e.Family, Display: e.Display, Tokenizer: e.Tokenizer,
			Verified: e.Verified, ExactProb: round6(post.exact[i]), RelatedProb: round6(post.related[i]),
		})
		if bestExact < 0 || post.exact[i] > post.exact[bestExact] {
			bestExact = i
		}
		if bestRelated < 0 || post.related[i] > post.related[bestRelated] {
			bestRelated = i
		}
	}
	sort.Slice(res.Candidates, func(i, j int) bool {
		si := res.Candidates[i].ExactProb + res.Candidates[i].RelatedProb
		sj := res.Candidates[j].ExactProb + res.Candidates[j].RelatedProb
		if si != sj {
			return si > sj
		}
		return res.Candidates[i].EntryID < res.Candidates[j].EntryID
	})
	res.UnlistedProb = round6(unlisted)
	if model != "" {
		res.Claim = db.tokClaimCheck(post, model)
	}
	if len(at) < MinTokProbes {
		res.ReasonCode = TokReasonInsufficient
		res.Reason = fmt.Sprintf("only %d usable differential counts; at least %d are required", len(at), MinTokProbes)
		return res
	}
	switch {
	case bestExact >= 0 && post.exact[bestExact] >= 0.5 && post.exact[bestExact] >= unlisted:
		e := db.Entries[bestExact]
		res.Verdict, res.TopEntryID, res.TopFamily = TokVerdictExact, e.ID, e.Family
		res.Probability = round6(post.exact[bestExact])
	case bestRelated >= 0 && post.related[bestRelated] >= post.unrelated && post.related[bestRelated] > post.exact[bestExact]:
		e := db.Entries[bestRelated]
		res.Verdict, res.TopEntryID, res.TopFamily = TokVerdictRelated, e.ID, e.Family
		res.Probability = round6(post.related[bestRelated])
	default:
		res.Verdict = TokVerdictUnknown
		res.Probability = round6(unlisted)
		if bestExact >= 0 {
			res.TopEntryID, res.TopFamily = db.Entries[bestExact].ID, db.Entries[bestExact].Family
		}
	}
	return res
}

func (db *DB) tokClaimCheck(post tokPosterior, model string) *TokClaim {
	fam := db.FamilyForModel(model)
	claim := &TokClaim{Model: model, Status: "unchecked"}
	if fam == "" {
		return claim
	}
	claim.Family = fam
	prob := 0.0
	verified := db.IsVerified(fam)
	for i, e := range db.Entries {
		if e.Family != fam {
			continue
		}
		prob += post.exact[i]
		if !verified {
			// A family without a source-verified tokenizer can only
			// be checked loosely: a close relative also counts.
			prob += post.related[i]
		}
	}
	claim.Probability = round6(prob)
	switch {
	case prob >= 0.9:
		claim.Status = "consistent"
	case prob <= 0.1:
		claim.Status = "inconsistent"
	default:
		claim.Status = "unclear"
	}
	return claim
}

// tokAtom is one weighted hypothesis in batch selection. Exact atoms
// form one group per entry; related atoms and the unrelated atom
// share the "unlisted" group.
type tokAtom struct {
	entry int // -1 = unrelated
	grid  []float64
	w     float64
	group int // entry index for exact atoms, -1 for unlisted group
}

// predictiveDensity returns the atom's marginalized density for an
// observed diff on one probe.
func (tp TokParams) predictiveDensity(entries []Entry, a tokAtom, probeIdx, diff int) float64 {
	if a.entry < 0 {
		sum := 0.0
		for _, e := range entries {
			sum += tp.entryPredictive(e, tp.RelatedContamination, probeIdx, diff)
		}
		return sum / float64(len(entries))
	}
	return tp.entryPredictive(entries[a.entry], a.grid, probeIdx, diff)
}

func (tp TokParams) entryPredictive(e Entry, grid []float64, probeIdx, diff int) float64 {
	sum := 0.0
	for _, rho := range grid {
		for _, shift := range tp.Shifts {
			sum += tp.residualDensity(rho, diff-shift-e.expectedDiff(probeIdx))
		}
	}
	return sum / float64(len(grid)*len(tp.Shifts))
}

// bhattacharyya computes BC between two atoms' predictive
// distributions on one probe over a diff support window.
func (tp TokParams) bhattacharyya(entries []Entry, a, b tokAtom, probeIdx int) float64 {
	lo, hi := math.MaxInt, math.MinInt
	for _, e := range entries {
		v := e.expectedDiff(probeIdx)
		lo = min(lo, v)
		hi = max(hi, v)
	}
	lo -= 8
	hi += 8
	// Densities must be normalized over the window before BC.
	diffs := make([]int, 0, hi-lo+1)
	pa := make([]float64, 0, hi-lo+1)
	pb := make([]float64, 0, hi-lo+1)
	sa, sb := 0.0, 0.0
	for d := lo; d <= hi; d++ {
		va := tp.predictiveDensity(entries, a, probeIdx, d)
		vb := tp.predictiveDensity(entries, b, probeIdx, d)
		diffs = append(diffs, d)
		pa = append(pa, va)
		pb = append(pb, vb)
		sa += va
		sb += vb
	}
	if sa <= 0 || sb <= 0 {
		return 1
	}
	bc := 0.0
	for i := range diffs {
		bc += math.Sqrt(pa[i] / sa * pb[i] / sb)
	}
	return math.Min(1, bc)
}

// TokSelectBatch greedily picks up to batch unused ordinary probes
// maximizing the pairwise Bhattacharyya error bound between groups
// (monotone submodular, so greedy is within 1-1/e of optimal). It
// also returns the marginal gain of the full batch, for the stop
// rule. Selection is deterministic: ties break by probe order.
func (db *DB) TokSelectBatch(obs []TokObservation, batch int, params TokParams) ([]Probe, float64) {
	tp := params.withDefaults()
	if batch <= 0 || len(db.Entries) == 0 {
		return nil, 0
	}
	used := map[string]bool{}
	for _, o := range obs {
		used[o.ProbeID] = true
	}
	post, _ := db.tokPosterior(obs, tp)
	var atoms []tokAtom
	n := len(db.Entries)
	for i := range db.Entries {
		w := post.exact[i]
		if len(obs) == 0 {
			w = tp.PriorExact / float64(n)
		}
		atoms = append(atoms, tokAtom{entry: i, grid: tp.ExactContamination, w: w, group: i})
	}
	for i := range db.Entries {
		w := post.related[i]
		if len(obs) == 0 {
			w = tp.PriorRelated / float64(n)
		}
		atoms = append(atoms, tokAtom{entry: i, grid: tp.RelatedContamination, w: w, group: -1})
	}
	wu := post.unrelated
	if len(obs) == 0 {
		wu = tp.PriorUnrelated
	}
	atoms = append(atoms, tokAtom{entry: -1, grid: tp.RelatedContamination, w: wu, group: -1})
	totalW := 0.0
	for _, a := range atoms {
		totalW += a.w
	}
	if totalW <= 0 {
		return nil, 0
	}
	type pair struct{ a, b int }
	var pairs []pair
	for i := range atoms {
		for j := i + 1; j < len(atoms); j++ {
			if atoms[i].group != atoms[j].group {
				pairs = append(pairs, pair{i, j})
			}
		}
	}
	// prod[p] = product of BC over the selected set for pair p.
	prod := make([]float64, len(pairs))
	for i := range prod {
		prod[i] = 1
	}
	objective := func(prod []float64) float64 {
		f := 0.0
		for pi, pr := range pairs {
			f += math.Sqrt(atoms[pr.a].w/totalW*atoms[pr.b].w/totalW) * totalW * (1 - prod[pi])
		}
		return f
	}
	var candidates []int
	for i, p := range db.Probes {
		if !p.Special && !used[p.ID] {
			candidates = append(candidates, i)
		}
	}
	bcCache := map[[3]int]float64{}
	bc := func(ai, bi, probeIdx int) float64 {
		k := [3]int{ai, bi, probeIdx}
		if v, ok := bcCache[k]; ok {
			return v
		}
		v := tp.bhattacharyya(db.Entries, atoms[ai], atoms[bi], probeIdx)
		bcCache[k] = v
		return v
	}
	var picked []Probe
	gain := 0.0
	for len(picked) < batch && len(candidates) > 0 {
		best, bestGain := -1, math.Inf(-1)
		var bestProd []float64
		base := objective(prod)
		for _, ci := range candidates {
			trial := append([]float64(nil), prod...)
			for pi, pr := range pairs {
				trial[pi] *= bc(pr.a, pr.b, ci)
			}
			if g := objective(trial) - base; g > bestGain {
				best, bestGain, bestProd = ci, g, trial
			}
		}
		if best < 0 {
			break
		}
		prod = bestProd
		gain += bestGain
		picked = append(picked, db.Probes[best])
		next := candidates[:0]
		for _, ci := range candidates {
			if ci != best {
				next = append(next, ci)
			}
		}
		candidates = next
	}
	return picked, gain
}

// TokShouldStop reports whether adaptive probing may stop: one
// group (an exact entry, or unlisted overall) holds at least 99%
// posterior after MinTokProbes, or the next batch would improve the
// error bound negligibly.
func TokShouldStop(res TokResult, nextGain float64, usable int) bool {
	if usable >= MinTokProbes && (res.Probability >= 0.99 && res.Verdict != TokVerdictInconclusive) {
		return true
	}
	return usable >= MinTokProbes && nextGain < 1e-4
}

func logSumExp(xs []float64) float64 {
	if len(xs) == 0 {
		return math.Inf(-1)
	}
	m := math.Inf(-1)
	for _, x := range xs {
		m = math.Max(m, x)
	}
	if math.IsInf(m, -1) {
		return m
	}
	sum := 0.0
	for _, x := range xs {
		sum += math.Exp(x - m)
	}
	return m + math.Log(sum)
}

func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }
