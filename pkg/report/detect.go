package report

import (
	"fmt"
	"io"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/numprint"
)

// TokProbe renders a tokenizer-probe result in plain words, with
// the candidate posteriors under --verbose-style detail always
// available through JSON.
func TokProbe(w io.Writer, endpointLabel, model, transport string, r fingerprint.TokResult, verbose bool) {
	fmt.Fprintf(w, "Endpoint: %s  Model: %s  Transport: %s\n", endpointLabel, model, transport)
	fmt.Fprintf(w, "Sent %d tiny differential requests (wrapped probes minus a wrapper-only baseline).\n\n", r.RequestsSpent)
	switch r.Verdict {
	case fingerprint.TokVerdictExact:
		fmt.Fprintf(w, "Tokenizer: EXACT — the endpoint counts with the %s tokenizer (probability %.0f%%).\n", tokLabel(r), r.Probability*100)
	case fingerprint.TokVerdictRelated:
		fmt.Fprintf(w, "Tokenizer: RELATED — the endpoint's tokenizer is closest to %s, but is probably an unlisted relative of it (probability %.0f%%).\n", tokLabel(r), r.Probability*100)
	case fingerprint.TokVerdictUnknown:
		fmt.Fprintf(w, "Tokenizer: UNKNOWN — the endpoint's tokenizer is most likely not in the reference list (unlisted probability %.0f%%).\n", r.UnlistedProb*100)
	default:
		fmt.Fprintf(w, "Tokenizer: INCONCLUSIVE — %s\n", r.Reason)
	}
	if r.BaselineDrift {
		fmt.Fprintln(w, "Warning: the opening and closing baselines differ, so the endpoint adds hidden input of varying length.")
	}
	if r.ProbeError != "" {
		fmt.Fprintf(w, "Note: probing stopped early: %s\n", r.ProbeError)
	}
	if r.Claim != nil {
		switch r.Claim.Status {
		case "consistent":
			fmt.Fprintf(w, "Claim check: the tokenizer is consistent with model %q (%s family, %.0f%%).\n", r.Claim.Model, r.Claim.Family, r.Claim.Probability*100)
		case "inconsistent":
			fmt.Fprintf(w, "Claim check: the tokenizer is INCONSISTENT with model %q (%s family, %.0f%%).\n", r.Claim.Model, r.Claim.Family, r.Claim.Probability*100)
		case "unclear":
			fmt.Fprintf(w, "Claim check: not yet clear for model %q (%s family, %.0f%%).\n", r.Claim.Model, r.Claim.Family, r.Claim.Probability*100)
		default:
			fmt.Fprintf(w, "Claim check: model %q does not map to a known family, so it cannot be checked.\n", r.Claim.Model)
		}
	}
	fmt.Fprintln(w, "\nA tokenizer match only shows which tokenizer counts the text; several models can share one, so this never proves an exact model version.")
	if verbose && len(r.Candidates) > 0 {
		fmt.Fprintln(w, "\nCandidates (exact / close-relative probability):")
		for _, c := range r.Candidates {
			fmt.Fprintf(w, "  %-24s %-10s %.1f%% / %.1f%%%s\n", c.EntryID, c.Family, c.ExactProb*100, c.RelatedProb*100, verifiedMark(c.Verified))
		}
		fmt.Fprintf(w, "  %-24s %.1f%%\n", "(unlisted overall)", r.UnlistedProb*100)
	}
}

func tokLabel(r fingerprint.TokResult) string {
	for _, c := range r.Candidates {
		if c.EntryID == r.TopEntryID {
			if c.Tokenizer != "" {
				return c.Tokenizer
			}
			return c.Display
		}
	}
	return r.TopFamily
}

func verifiedMark(v bool) string {
	if v {
		return " [verified]"
	}
	return " [unverified]"
}

// NumRound renders one number-fingerprint round.
func NumRound(w io.Writer, round int, r numprint.Result, verbose bool) {
	if round > 0 {
		fmt.Fprintf(w, "Round %d: ", round)
	}
	switch r.Status {
	case numprint.StatusRanked:
		top := r.Candidates[0]
		if r.Calibrated {
			fmt.Fprintf(w, "closest in the reference bank is %s (%s family, confidence %.0f%%).\n", top.Display, top.Family, top.Confidence*100)
		} else {
			fmt.Fprintf(w, "closest in the reference bank is %s (%s family) — ranking only, no calibrated confidence.\n", top.Display, top.Family)
		}
		if verbose {
			for _, c := range r.Candidates {
				if r.Calibrated {
					fmt.Fprintf(w, "  %-24s %-10s score %.3f  confidence %.1f%%\n", c.EntryID, c.Family, c.Score, c.Confidence*100)
				} else {
					fmt.Fprintf(w, "  %-24s %-10s score %.3f\n", c.EntryID, c.Family, c.Score)
				}
			}
		}
	case numprint.StatusNotModelReply:
		fmt.Fprintf(w, "NOT A MODEL REPLY — %s\n", r.Reason)
	default:
		fmt.Fprintf(w, "INCONCLUSIVE — %s\n", r.Reason)
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "Warning: %s\n", warning)
	}
}

// NumbersHeader renders the standard header plus the honesty
// footer for a numbers run.
func NumbersHeader(w io.Writer, endpointLabel, model, transport string, requestsSpent int) {
	fmt.Fprintf(w, "Endpoint: %s  Model: %s  Transport: %s\n", endpointLabel, model, transport)
	fmt.Fprintf(w, "Sent %d number-writing request(s) and compared the value preferences with the reference bank.\n\n", requestsSpent)
}

func NumbersFooter(w io.Writer) {
	fmt.Fprintln(w, "\nThis is a closed-set ranking within the reference bank: a model outside the bank still gets a closest candidate. Rankings and confidence are not proof of identity — repeat rounds and combine with tokenizer evidence.")
}
