package probe

import (
	"context"
	"errors"
	"fmt"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

// TokOptions controls a differential tokenizer-probe run.
type TokOptions struct {
	MaxRequests int // hard cap on HTTP requests, baselines included
	MaxProbes   int // cap on probe texts (default: all ordinary probes)
	Batch       int // probes per adaptive batch (default 4)
	Params      fingerprint.TokParams
	OnProbe     func(requestsSpent int)
}

// TokenizerProbe runs the differential tokenizer probe: a
// wrapper-only baseline, adaptively selected wrapped probes, and a
// closing baseline whose disagreement with the opening one marks
// the result as drifted (the upstream adds hidden input of varying
// length, so differences cannot be trusted). Every HTTP attempt,
// retries included, counts against MaxRequests.
func TokenizerProbe(ctx context.Context, c *Client, db *fingerprint.DB, model string, opts TokOptions) (fingerprint.TokResult, error) {
	batch := opts.Batch
	if batch <= 0 {
		batch = 4
	}
	maxProbes := opts.MaxProbes
	if maxProbes <= 0 {
		maxProbes = len(db.Probes)
	}
	start := c.Requests()
	spent := func() int { return c.Requests() - start }
	remaining := func() int {
		if opts.MaxRequests <= 0 {
			return 1 << 30
		}
		return opts.MaxRequests - spent()
	}
	notify := func() {
		if opts.OnProbe != nil {
			opts.OnProbe(spent())
		}
	}
	var obs []fingerprint.TokObservation
	finish := func(drift bool, probeErr string) fingerprint.TokResult {
		res := db.TokRank(obs, model, opts.Params)
		// Recompute including drift downgrade below.
		res.RequestsSpent = spent()
		res.BaselineDrift = drift
		if probeErr != "" {
			res.ProbeError = probeErr
		}
		if drift && res.Verdict != fingerprint.TokVerdictInconclusive {
			res.Reason = "opening and closing baseline counts differ; the endpoint adds hidden input of varying length, so differential counts are not trustworthy"
			res.ReasonCode = fingerprint.TokReasonBaselineDrift
			res.Verdict = fingerprint.TokVerdictInconclusive
		}
		return res
	}
	sendCount := func(id, text string) (int, error) {
		if remaining() <= 0 {
			return 0, errors.New("request cap reached")
		}
		o, err := c.send(ctx, fingerprint.Probe{ID: id, Text: text}, remaining())
		notify()
		if err != nil {
			return 0, err
		}
		if !o.UsableCount() {
			if o.Error != "" {
				return 0, errors.New(o.Error)
			}
			return 0, errors.New("response carried no usable prompt-token usage")
		}
		return o.PromptTokens, nil
	}
	baseline, err := sendCount("__baseline", fingerprint.WrapTokText(""))
	if err != nil {
		return finish(false, ""), fmt.Errorf("baseline probe failed: %w", err)
	}
	usable := 0
	for usable < maxProbes {
		picked, gain := db.TokSelectBatch(obs, min(batch, maxProbes-usable), opts.Params)
		if len(picked) == 0 {
			break
		}
		if usable >= fingerprint.MinTokProbes {
			res := db.TokRank(obs, model, opts.Params)
			if fingerprint.TokShouldStop(res, gain, usable) {
				break
			}
		}
		progressed := false
		for _, p := range picked {
			// Keep one request in reserve for the closing baseline
			// once a verdict is already reachable.
			if remaining() <= 0 || (usable >= fingerprint.MinTokProbes && remaining() <= 1) {
				break
			}
			n, err := sendCount(p.ID, fingerprint.WrapTokText(p.Text))
			if err != nil {
				if usable < fingerprint.MinTokProbes {
					return finish(false, ""), fmt.Errorf("probe %s failed: %w", p.ID, err)
				}
				return finish(false, fmt.Sprintf("probe %s failed: %v", p.ID, err)), nil
			}
			obs = append(obs, fingerprint.TokObservation{ProbeID: p.ID, Diff: n - baseline, HasDiff: true})
			usable++
			progressed = true
		}
		if !progressed {
			break
		}
	}
	drift := false
	if remaining() > 0 && usable > 0 {
		if final, err := sendCount("__baseline_final", fingerprint.WrapTokText("")); err == nil {
			drift = final != baseline
		}
	}
	return finish(drift, ""), nil
}
