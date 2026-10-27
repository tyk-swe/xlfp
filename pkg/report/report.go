// Package report renders fingerprint and audit results in human-readable
// form. JSON rendering is plain encoding/json over the result structs in
// pkg/fingerprint and pkg/compare. Secrets never reach this package.
package report

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/tyk-swe/xlfp/pkg/compare"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

// SanitizeURL strips userinfo and query (which can carry keys) for display.
func SanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return "(unparseable endpoint)"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func writeRankings(w io.Writer, title string, ranks []fingerprint.FamilyRank) {
	if len(ranks) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", title)
	for i, r := range ranks {
		shared := ""
		if len(r.SharedWith) > 0 {
			shared = fmt.Sprintf(" [tokenizer shared with: %s]", strings.Join(r.SharedWith, ", "))
		}
		fmt.Fprintf(w, "  %d. %-10s confidence %.3f  MAE %.2f  overhead %.1f  entry %s (%s)%s\n",
			i+1, r.Family, r.Confidence, r.MAE, r.Overhead, r.EntryID, r.Tokenizer, shared)
	}
}

func writeObservations(w io.Writer, obs []fingerprint.Observation) {
	if len(obs) == 0 {
		return
	}
	fmt.Fprintln(w, "Probe evidence:")
	for _, o := range obs {
		switch {
		case o.Rejected:
			fmt.Fprintf(w, "  %-14s rejected (HTTP %d)\n", o.ProbeID, o.HTTPStatus)
		case o.HasUsage:
			fmt.Fprintf(w, "  %-14s %d prompt tokens\n", o.ProbeID, o.PromptTokens)
		case o.Error != "":
			fmt.Fprintf(w, "  %-14s no usable count (%s)\n", o.ProbeID, o.Error)
		}
	}
}

// Uncover renders a fingerprint result for a named endpoint.
func Uncover(w io.Writer, endpointLabel, model, transport string, r fingerprint.Result) {
	fmt.Fprintf(w, "Endpoint: %s  Model: %s  Transport: %s\n", SanitizeURL(endpointLabel), model, transport)
	fmt.Fprintf(w, "Database: v%s  Requests spent: %d\n", r.DBVersion, r.RequestsSpent)
	if r.Status == fingerprint.StatusIdentified && len(r.Rankings) > 0 {
		top := r.Rankings[0]
		fmt.Fprintf(w, "Result: IDENTIFIED as family %q (entry %s, tokenizer %s, confidence %.3f)\n",
			r.TopFamily, top.EntryID, top.Tokenizer, top.Confidence)
		fmt.Fprintln(w, "Note: family-level identification from tokenizer evidence; not proof of an exact checkpoint.")
	} else {
		fmt.Fprintln(w, "Result: INCONCLUSIVE")
		if r.Reason != "" {
			fmt.Fprintf(w, "Reason: %s\n", r.Reason)
		}
	}
	writeRankings(w, "Ranked source-verified families", r.Rankings)
	writeRankings(w, "Unverified families (scored, never a verdict)", r.Unverified)
	writeObservations(w, r.Observations)
}

// Audit renders an audit result.
func Audit(w io.Writer, targetLabel, officialLabel string, r compare.Result) {
	fmt.Fprintf(w, "Target: %s\n", SanitizeURL(targetLabel))
	if officialLabel != "" {
		fmt.Fprintf(w, "Official: %s\n", SanitizeURL(officialLabel))
	}
	fmt.Fprintf(w, "Claimed family: %s  Requests spent: %d\n", r.ClaimedFamily, r.RequestsSpent)
	switch r.Verdict {
	case compare.VerdictMatch:
		fmt.Fprintln(w, "Verdict: MATCH — target fingerprints as the claimed family")
	case compare.VerdictMismatch:
		fmt.Fprintln(w, "Verdict: MISMATCH — target fingerprints as a different family than claimed")
	default:
		fmt.Fprintln(w, "Verdict: INCONCLUSIVE — no verdict can be rendered from this evidence")
	}
	if r.Reason != "" {
		fmt.Fprintf(w, "Reason: %s\n", r.Reason)
	}
	fmt.Fprintln(w, "\nTarget fingerprint:")
	writeRankings(w, "  Ranked source-verified families", r.Target.Rankings)
	writeRankings(w, "  Unverified families (scored, never a verdict)", r.Target.Unverified)
	writeObservations(w, r.Target.Observations)
	if r.Official != nil {
		fmt.Fprintln(w, "\nOfficial fingerprint:")
		writeRankings(w, "  Ranked source-verified families", r.Official.Rankings)
		writeObservations(w, r.Official.Observations)
	}
	if r.Divergence != nil {
		fmt.Fprintf(w, "\nToken-count divergence (overhead-normalized): mean %.2f, max %.2f over %d probes\n",
			r.Divergence.MeanAbsDiff, r.Divergence.MaxAbsDiff, len(r.Divergence.PerProbe))
		fmt.Fprintf(w, "Note: %s.\n", r.Divergence.Note)
	}
}

// Families renders the embedded database summary.
func Families(w io.Writer, db *fingerprint.DB) {
	fmt.Fprintf(w, "Fingerprint database v%s (%d probes, %d entries)\n", db.Version, len(db.Probes), len(db.Entries))
	for _, e := range db.Entries {
		status := "unverified"
		if e.Verified {
			status = "source-verified"
		}
		fmt.Fprintf(w, "  %-18s family %-10s %-10s tokenizer %s\n    source: %s\n", e.ID, e.Family, status, e.Tokenizer, e.Source)
	}
}
