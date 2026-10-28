package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/tyk-swe/xlfp/pkg/compare"
	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/opencode"
	"github.com/tyk-swe/xlfp/pkg/probe"
)

// preparedCheck holds the exact clients reviewed before a run. Specs kept for
// display have their keys removed; clients are discarded after the run.
type preparedCheck struct {
	mode                         string
	common                       commonFlags
	db                           *fingerprint.DB
	target, official             *probe.Client
	opencode                     *opencode.Client
	targetSpec, officialSpec     config.EndpointSpec
	targetHasKey, officialHasKey bool
	claimed                      string
}

type checkSetupError struct {
	err  error
	hint bool
}

func (e *checkSetupError) Error() string { return e.err.Error() }
func (e *checkSetupError) Unwrap() error { return e.err }
func setupErr(err error, hint bool) error {
	if err == nil {
		return nil
	}
	return &checkSetupError{err, hint}
}

func prepareCheck(mode string, target, official endpointFlags, claim string, hasOfficial bool, cf commonFlags) (*preparedCheck, error) {
	if err := validateCommon(cf); err != nil {
		return nil, setupErr(err, false)
	}
	db, err := fingerprint.Load()
	if err != nil {
		return nil, setupErr(err, false)
	}
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		return nil, setupErr(err, false)
	}
	purpose := ""
	if mode == "audit" {
		purpose = "target"
	}
	tc, ts, err := resolveClient(cfg, target, purpose, cf.timeout)
	if err != nil {
		if mode == "audit" {
			err = fmt.Errorf("target: %w", err)
		}
		return nil, setupErr(err, true)
	}
	p := &preparedCheck{mode: mode, common: cf, db: db, target: tc, targetSpec: ts, targetHasKey: ts.APIKey != ""}
	p.targetSpec.APIKey = ""
	if mode != "audit" {
		return p, nil
	}
	p.claimed = db.NormalizeFamily(claim)
	if claim != "" && p.claimed == "" {
		return nil, setupErr(fmt.Errorf("unknown claimed family %q (known families: %s)", claim, strings.Join(db.Families(), ", ")), true)
	}
	if p.claimed == "" {
		p.claimed = db.FamilyForModel(ts.Model)
		if p.claimed == "" {
			return nil, setupErr(fmt.Errorf("cannot infer claimed family from model %q; pass --claim (known families: %s)", ts.Model, strings.Join(db.Families(), ", ")), true)
		}
	}
	if hasOfficial {
		oc, spec, err := resolveClient(cfg, official, "official", cf.timeout)
		if err != nil {
			return nil, setupErr(fmt.Errorf("official: %w", err), true)
		}
		p.official, p.officialSpec, p.officialHasKey = oc, spec, spec.APIKey != ""
		p.officialSpec.APIKey = ""
	}
	return p, nil
}

func hasOfficialEndpoint(official endpointFlags, explicit bool) bool {
	return official.endpoint != "" || official.profile != "" || (!explicit && os.Getenv("XLFP_OFFICIAL_ENDPOINT") != "")
}

type checkResult struct {
	uncover                    *fingerprint.Result
	audit                      *compare.Result
	err                        error
	cancelled                  bool
	targetSpent, officialSpent int
}

// execute is called once per prepared check. The callback runs in the probe
// goroutine, and counts actual HTTP requests (including retries), or CLI
// probe runs for OpenCode. Provider requests inside OpenCode are unknown.
func (p *preparedCheck) execute(ctx context.Context, progress func(string, int)) (out checkResult) {
	defer func() {
		out.cancelled = ctx.Err() != nil
		if p.opencode != nil {
			out.targetSpent = p.opencode.Attempts()
		} else {
			out.targetSpent = p.target.Requests()
		}
		if p.official != nil {
			out.officialSpent = p.official.Requests()
		}
	}()
	if p.opencode != nil {
		defer func() {
			if err := p.opencode.Close(); out.err == nil {
				out.err = err
			}
		}()
		if err := p.opencode.Init(ctx); err != nil {
			out.err = err
			return
		}
		opts := probe.Options{MaxRequests: p.common.maxRequests, MinConfidence: p.common.minConfidence}
		if progress != nil {
			opts.OnProbe = func(n int) { progress("target", n) }
		}
		r, err := probe.FingerprintWithBackend(ctx, p.opencode, p.db, opts)
		out.uncover, out.err = &r, err
		return
	}
	if p.mode == "audit" && !p.db.IsVerified(p.claimed) {
		reason := fmt.Sprintf("claimed family %q is not a verified family in database v%s, so no verdict can be rendered for it", p.claimed, p.db.Version)
		r := compare.Result{Verdict: compare.VerdictInconclusive, ClaimedFamily: p.claimed, Reason: reason, ReasonCode: compare.ReasonUnverifiedClaim,
			Target: fingerprint.Result{Status: fingerprint.StatusInconclusive, Reason: reason + "; endpoint not probed", DBVersion: p.db.Version, Rankings: []fingerprint.FamilyRank{}, Unverified: []fingerprint.FamilyRank{}, Observations: []fingerprint.Observation{}}}
		out.audit = &r
		return
	}
	opts := probe.Options{MaxRequests: p.common.maxRequests, MinConfidence: p.common.minConfidence}
	if progress != nil {
		opts.OnProbe = func(n int) { progress("target", n) }
	}
	t, err := probe.Fingerprint(ctx, p.target, p.db, opts)
	if err != nil {
		if ctx.Err() != nil && t.RequestsSpent > 0 {
			if p.mode == "audit" {
				out.audit = &compare.Result{Target: t}
			} else {
				out.uncover = &t
			}
		}
		if p.mode == "audit" {
			err = fmt.Errorf("target: %w", err)
		}
		out.err = err
		return
	}
	if p.mode == "uncover" {
		out.uncover = &t
		return
	}
	if ctx.Err() != nil {
		r := compare.Evaluate(p.db, p.claimed, t, nil)
		if p.official != nil {
			r.Verdict, r.ReasonCode = compare.VerdictInconclusive, compare.ReasonOfficialFailed
			r.Reason = "official baseline probing cancelled; target-only result is shown but no verdict is safe"
		}
		out.audit = &r
		return
	}
	var official *fingerprint.Result
	if p.official != nil {
		if progress != nil {
			opts.OnProbe = func(n int) { progress("official", n) }
		}
		o, err := probe.Fingerprint(ctx, p.official, p.db, opts)
		if err != nil {
			r := compare.Evaluate(p.db, p.claimed, t, nil)
			r.RequestsSpent += p.official.Requests()
			if ctx.Err() != nil && o.RequestsSpent > 0 {
				r.Official = &o
			}
			r.Verdict, r.ReasonCode = compare.VerdictInconclusive, compare.ReasonOfficialFailed
			r.Reason = fmt.Sprintf("official baseline probing failed (%v); target-only result is shown but no verdict is safe", err)
			out.audit = &r
			return
		}
		official = &o
	}
	r := compare.Evaluate(p.db, p.claimed, t, official)
	out.audit = &r
	return
}
