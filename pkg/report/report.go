// Package report renders fingerprint and audit results in human-readable
// form. The default rendering is plain English for readers who know
// nothing about tokenizers: a verdict, what it means, and what to do
// next. Verbose rendering adds the technical evidence (MAE, fitted
// overhead, per-probe counts). JSON rendering is plain encoding/json
// over the result structs in pkg/fingerprint and pkg/compare. Secrets
// never reach this package.
package report

import (
	"fmt"
	"io"
	"math"
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

// familyDisplayNames is a fallback for families missing from a result's
// rankings (whose Display field is preferred).
var familyDisplayNames = map[string]string{
	"openai": "OpenAI", "qwen": "Qwen", "deepseek": "DeepSeek", "mistral": "Mistral",
	"glm": "GLM", "mimo": "MiMo", "google": "Google", "claude": "Claude",
}

// familyLabel renders "DeepSeek (deepseek)": the readable name for humans,
// the slug for anyone scripting against the text output.
func familyLabel(family, display string) string {
	if display == "" {
		display = familyDisplayNames[family]
	}
	if display == "" || display == family {
		return family
	}
	return fmt.Sprintf("%s (%s)", display, family)
}

func labelFromRanks(family string, ranks []fingerprint.FamilyRank) string {
	for _, r := range ranks {
		if r.Family == family {
			return familyLabel(family, r.Display)
		}
	}
	return familyLabel(family, "")
}

func pct(c float64) int { return int(math.Round(c * 100)) }

// confidencePhrase turns a 0..1 confidence into words plus a percentage.
func confidencePhrase(c float64) string {
	word := "low"
	switch {
	case c >= 0.95:
		word = "very high"
	case c >= 0.85:
		word = "high"
	case c >= 0.70:
		word = "moderate"
	}
	return fmt.Sprintf("%s (%d%%)", word, pct(c))
}

// writeClosest lists the top few families in beginner terms.
func writeClosest(w io.Writer, title string, r fingerprint.Result) {
	if len(r.Rankings) == 0 && len(r.Unverified) == 0 {
		return
	}
	fmt.Fprintf(w, "%s\n", title)
	n := len(r.Rankings)
	if n > 3 {
		n = 3
	}
	for i := 0; i < n; i++ {
		fr := r.Rankings[i]
		fmt.Fprintf(w, "  %d. %s — %d%%\n", i+1, familyLabel(fr.Family, fr.Display), pct(fr.Confidence))
	}
	if len(r.Unverified) > 0 {
		fr := r.Unverified[0]
		fmt.Fprintf(w, "  Also scored, but not yet verifiable: %s — %d%%\n", familyLabel(fr.Family, fr.Display), pct(fr.Confidence))
	}
}

func writeTries(w io.Writer, tries ...string) {
	if len(tries) == 0 {
		return
	}
	fmt.Fprintln(w, "What you can try:")
	for _, t := range tries {
		fmt.Fprintf(w, "  - %s\n", t)
	}
}

// explainInconclusive writes the plain-English cause and next steps for an
// inconclusive fingerprint. Unknown codes fall back to the technical
// reason so nothing is hidden.
func explainInconclusive(w io.Writer, r fingerprint.Result) {
	explainInconclusiveFor(w, r, false)
}

func explainInconclusiveFor(w io.Writer, r fingerprint.Result, opencode bool) {
	topLabel, unverifiedLabel := "", ""
	if len(r.Rankings) > 0 {
		topLabel = familyLabel(r.Rankings[0].Family, r.Rankings[0].Display)
	}
	if len(r.Unverified) > 0 {
		unverifiedLabel = familyLabel(r.Unverified[0].Family, r.Unverified[0].Display)
	}
	why := ""
	var tries []string
	switch r.ReasonCode {
	case fingerprint.ReasonInsufficientCounts:
		if opencode {
			fmt.Fprintln(w, "Why: OpenCode did not return usable prompt-token counts for enough test messages.")
			writeTries(w, "Check your OpenCode login and selected model with `opencode models opencode`.", "Use --verbose to inspect partial evidence; check OpenCode v1 JSON support.")
			return
		}
		why = "The endpoint did not give back usable token counts for enough of the test messages. Without those counts there is nothing to compare."
		tries = []string{
			"Make sure the address really is an LLM chat API and the model name is correct.",
			"If this is a native Anthropic or Gemini API behind a custom address, add --transport anthropic or --transport gemini.",
			"Check the API key: for unknown endpoints, set it with `export XLFP_API_KEY=...` first.",
		}
	case fingerprint.ReasonUnreliableCounts:
		why = "The reported token counts barely changed from message to message, whatever the text. Real tokenizers do not behave like that, so the numbers look estimated or made up, and xlfp will not guess from numbers it cannot trust."
		tries = []string{
			"These counts fail the plausibility checks; convincing fabricated counts cannot be detected reliably.",
			"If you run this endpoint yourself, make it report real per-request token usage.",
		}
	case fingerprint.ReasonScaledCounts:
		why = "The counts grow faster or slower than the messages themselves, as if they were multiplied by a factor instead of actually counted."
		tries = []string{
			"These counts fail the scale checks; a good fit alone would not authenticate the provider's reporting.",
			"If you run this endpoint yourself, make it report real per-request token usage.",
		}
	case fingerprint.ReasonImplausibleOverhead:
		why = "The counts sit consistently far above or below every known family, as if a large hidden prompt were added to (or stripped from) every message."
		tries = []string{
			"If the provider injects its own hidden system prompt, try a model or route without it.",
			"Run with --verbose to see the size of the offset.",
		}
	case fingerprint.ReasonInconsistentCounts:
		why = "Most messages match one family, but at least one is far off, so the evidence contradicts itself."
		tries = []string{
			"Run it again. Some providers route requests to different backends.",
			"If it keeps happening, treat this endpoint's usage numbers as unreliable.",
		}
	case fingerprint.ReasonPoorFit:
		why = "The counts do not match any family xlfp knows well enough. It may be a newer or unlisted model family, or the usage numbers may be unreliable."
		tries = []string{
			"Run `xlfp families` to see which families this build knows.",
		}
	case fingerprint.ReasonSharedTokenizer:
		others := ""
		if len(r.Rankings) > 0 && len(r.Rankings[0].SharedWith) > 0 {
			var names []string
			for _, f := range r.Rankings[0].SharedWith {
				names = append(names, labelFromRanks(f, r.Rankings))
			}
			others = strings.Join(names, ", ")
		}
		why = fmt.Sprintf("This endpoint matches %s — but %s uses the exact same tokenizer, so token counts genuinely cannot tell them apart. This is a real tie, not missing data.", topLabel, others)
		tries = []string{
			"You will need other evidence (provider documentation, behavior, pricing) to separate these two.",
		}
	case fingerprint.ReasonUnverifiedBest:
		why = fmt.Sprintf("The closest match is %s, but xlfp cannot verify that family yet because its server-side tokenizer is not public, so it will not claim a match.", unverifiedLabel)
		tries = []string{
			"Treat that closest match as a hint, not a verdict.",
		}
	case fingerprint.ReasonLowConfidence:
		why = fmt.Sprintf("The best-matching family is %s, but the evidence is not strong or clear enough to call it.", topLabel)
		tries = []string{
			"Run it again to rule out a flaky response.",
			"If you are checking a provider's claim, `xlfp audit` with the official endpoint gives a stronger comparison.",
		}
		if opencode {
			tries = []string{"Run it again to rule out a flaky response; the model may be outside the known families."}
		}
	case fingerprint.ReasonNoVerifiedFamilies:
		why = "This build's fingerprint database has no verified families to compare against. That should not happen in a released build."
	default:
		if r.Reason != "" {
			fmt.Fprintf(w, "Why: %s\n", r.Reason)
		}
		return
	}
	fmt.Fprintf(w, "Why: %s\n", why)
	writeTries(w, tries...)
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

// writeTechnical renders the full technical evidence block for --verbose.
func writeTechnical(w io.Writer, r fingerprint.Result) {
	fmt.Fprintf(w, "\nTechnical details (database v%s):\n", r.DBVersion)
	if r.Reason != "" {
		fmt.Fprintf(w, "Reason: %s\n", r.Reason)
	}
	writeRankings(w, "Ranked source-verified families", r.Rankings)
	writeRankings(w, "Unverified families (scored, never a verdict)", r.Unverified)
	writeObservations(w, r.Observations)
}

// Uncover renders a fingerprint result for a named endpoint.
func Uncover(w io.Writer, endpointLabel, model, transport string, r fingerprint.Result, verbose bool) {
	fmt.Fprintf(w, "Endpoint: %s  Model: %s  Transport: %s\n", SanitizeURL(endpointLabel), model, transport)
	if r.RequestsSpent > 0 {
		fmt.Fprintf(w, "Sent %d tiny test requests and compared how the endpoint counts tokens.\n", r.RequestsSpent)
	}
	writeFingerprint(w, r, verbose, false)
}

// OpenCode renders the shared evidence with CLI-run accounting, since provider
// requests and internal retries are controlled by OpenCode and are not known.
func OpenCode(w io.Writer, model, version string, r fingerprint.Result, verbose bool) {
	fmt.Fprintf(w, "OpenCode: %s  Model: %s\n", version, model)
	fmt.Fprintf(w, "Probe runs: %d. OpenCode controls internal provider requests and retries.\n", r.RequestsSpent)
	writeFingerprint(w, r, verbose, true)
}

func writeFingerprint(w io.Writer, r fingerprint.Result, verbose, opencode bool) {
	fmt.Fprintln(w)
	identified := r.Status == fingerprint.StatusIdentified && len(r.Rankings) > 0
	if identified {
		top := r.Rankings[0]
		fmt.Fprintf(w, "Result: IDENTIFIED — this endpoint behaves like the %s family.\n", familyLabel(top.Family, top.Display))
		fmt.Fprintf(w, "Confidence: %s\n\n", confidencePhrase(top.Confidence))
		fmt.Fprintln(w, "What this means: the way this endpoint splits text into tokens matches that")
		fmt.Fprintln(w, "family. That is strong evidence about the model family, but token counts")
		fmt.Fprintln(w, "cannot prove an exact model version or checkpoint.")
		if len(top.SharedWith) > 0 {
			var names []string
			for _, f := range top.SharedWith {
				names = append(names, labelFromRanks(f, r.Rankings))
			}
			fmt.Fprintf(w, "Note: %s shares this tokenizer, so those families look identical here.\n", strings.Join(names, ", "))
		}
	} else {
		fmt.Fprintln(w, "Result: INCONCLUSIVE — xlfp cannot tell which family this endpoint is.")
		explainInconclusiveFor(w, r, opencode)
		writeClosest(w, "\nClosest matches (not conclusive):", r)
	}
	if !verbose {
		writeProbeWarning(w, "Warning", r)
	}
	if verbose {
		writeTechnical(w, r)
	} else {
		fmt.Fprintln(w, "\nRun with --verbose for the technical evidence, or --json for machine-readable output.")
	}
}

// writeProbeWarning preserves early-stop diagnostics in non-verbose reports.
func writeProbeWarning(w io.Writer, label string, r fingerprint.Result) {
	if i := strings.Index(r.Reason, "probing stopped early:"); i >= 0 {
		fmt.Fprintf(w, "\n%s: %s\n", label, r.Reason[i:])
	}
}

// Audit renders an audit result.
func Audit(w io.Writer, targetLabel, officialLabel string, r compare.Result, verbose bool) {
	fmt.Fprintf(w, "Target: %s\n", SanitizeURL(targetLabel))
	if officialLabel != "" {
		fmt.Fprintf(w, "Official: %s\n", SanitizeURL(officialLabel))
	}
	claimed := labelFromRanks(r.ClaimedFamily, r.Target.Rankings)
	fmt.Fprintf(w, "Claimed family: %s\n", claimed)
	if r.RequestsSpent > 0 {
		fmt.Fprintf(w, "Tiny test requests sent: %d\n", r.RequestsSpent)
	}
	fmt.Fprintln(w)
	switch r.Verdict {
	case compare.VerdictMatch:
		conf := ""
		if len(r.Target.Rankings) > 0 {
			conf = fmt.Sprintf(" (confidence %s)", confidencePhrase(r.Target.Rankings[0].Confidence))
		}
		fmt.Fprintf(w, "Verdict: MATCH — this endpoint fingerprints as the claimed %s family%s.\n", claimed, conf)
		fmt.Fprintln(w, "What this means: the way it counts tokens matches the claimed family. This")
		fmt.Fprintln(w, "checks the family only; it cannot prove an exact model version, and it says")
		fmt.Fprintln(w, "nothing about answer quality.")
	case compare.VerdictMismatch:
		actual := ""
		if r.Target.TopFamily != "" {
			actual = labelFromRanks(r.Target.TopFamily, r.Target.Rankings)
		}
		fmt.Fprintf(w, "Verdict: MISMATCH — this endpoint fingerprints as the %s family, not the claimed %s.\n", actual, claimed)
		fmt.Fprintln(w, "What this means: strong token-count evidence that a different model family")
		fmt.Fprintln(w, "is serving these requests than the one claimed. It is not, on its own, proof of fraud.")
		fmt.Fprintln(w, "Verify before accusing the provider — for example by re-running with the")
		fmt.Fprintln(w, "official endpoint as a live baseline (--official-url).")
	default:
		fmt.Fprintln(w, "Verdict: INCONCLUSIVE — xlfp cannot confirm or refute the claim from this evidence.")
		switch r.ReasonCode {
		case compare.ReasonUnverifiedClaim:
			fmt.Fprintf(w, "Why: xlfp cannot verify the %s family yet, because its server-side tokenizer is not public. It never renders verdicts for or against such families, so the endpoint was not probed.\n", claimed)
			writeTries(w, "Run `xlfp uncover` on the endpoint to see how it scores, as a hint only.")
		case compare.ReasonTargetInconclusive:
			explainInconclusive(w, r.Target)
		case compare.ReasonOfficialInDoubt:
			fmt.Fprintln(w, "Why: the official endpoint itself fingerprints as a third family that matches neither the claim nor the target, so the reference cannot be trusted for this check.")
			writeTries(w, "Double-check --official-url and the official model name, then run again.")
		case compare.ReasonOfficialFailed:
			fmt.Fprintln(w, "Why: the target was checked, but probing the official endpoint failed, so no verdict is safe. The target evidence is still shown.")
			writeTries(w, "Check the official URL and its API key environment variable, then run again.")
		default:
			if r.Reason != "" {
				fmt.Fprintf(w, "Why: %s\n", r.Reason)
			}
		}
		writeClosest(w, "\nTarget's closest matches (not conclusive):", r.Target)
	}
	if r.Official != nil && r.Official.Status != fingerprint.StatusIdentified && r.Verdict != compare.VerdictInconclusive {
		fmt.Fprintln(w, "Note: the official endpoint's own check was inconclusive, so it could not corroborate this result.")
	}
	if !verbose {
		writeProbeWarning(w, "Target warning", r.Target)
		if r.Official != nil {
			writeProbeWarning(w, "Official warning", *r.Official)
		}
	}
	if r.Divergence != nil && !verbose {
		fmt.Fprintf(w, "\nCompared with the official endpoint, token counts differ by about %.1f tokens\nper message on average (a signal only — not proof of a weaker model).\n", r.Divergence.MeanAbsDiff)
	}
	if verbose {
		if r.Reason != "" {
			fmt.Fprintf(w, "\nReason: %s\n", r.Reason)
		}
		fmt.Fprintln(w, "\nTarget fingerprint:")
		writeTechnical(w, r.Target)
		if r.Official != nil {
			fmt.Fprintln(w, "\nOfficial fingerprint:")
			writeTechnical(w, *r.Official)
		}
		if r.Divergence != nil {
			fmt.Fprintf(w, "\nToken-count divergence (overhead-normalized): mean %.2f, max %.2f over %d probes\n",
				r.Divergence.MeanAbsDiff, r.Divergence.MaxAbsDiff, len(r.Divergence.PerProbe))
			fmt.Fprintf(w, "Note: %s.\n", r.Divergence.Note)
		}
	} else if r.Verdict != compare.VerdictInconclusive {
		fmt.Fprintln(w, "\nRun with --verbose for the technical evidence, or --json for machine-readable output.")
	}
}

// Families renders the embedded database summary.
func Families(w io.Writer, db *fingerprint.DB) {
	fmt.Fprintf(w, "Model families this build can recognize (fingerprint database v%s, %d probes):\n", db.Version, len(db.Probes))
	for _, e := range db.Entries {
		status := "not yet verifiable (tokenizer not public)"
		if e.Verified {
			status = "source-verified"
		}
		fmt.Fprintf(w, "  %-18s family %-10s %s\n    tokenizer: %s\n    source: %s\n", e.ID, e.Family, status, e.Tokenizer, e.Source)
	}
	fmt.Fprintln(w, "\nOnly source-verified families can produce a verdict; the others are shown as hints.")
}
