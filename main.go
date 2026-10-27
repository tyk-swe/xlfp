// Command xlfp (xo llm fingerprinter) identifies the model family behind
// an LLM endpoint from its reported token usage, and audits whether a
// provider's claimed family matches its fingerprint. See PROJECT.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/tyk-swe/xlfp/pkg/compare"
	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/probe"
	"github.com/tyk-swe/xlfp/pkg/report"
)

// version is overridable at release time via -ldflags -X main.version=...
var version = "0.1.0"

// Exit codes (PROJECT.md Q11, values fixed at implementation stage):
const (
	exitOK           = 0 // uncover: identified; audit: match
	exitError        = 1 // operational or usage error
	exitInconclusive = 2 // uncover/audit: inconclusive
	exitMismatch     = 3 // audit: mismatch
)

const usage = `xlfp — xo llm fingerprinter

Usage:
  xlfp uncover --endpoint URL --model NAME [flags]
  xlfp audit --target-url URL --model NAME --claim FAMILY [--official-url URL] [flags]
  xlfp families [--json]
  xlfp version

Uncover ranks the likely model family of an endpoint from its reported
token usage, or returns inconclusive. Audit checks a target endpoint
against a claimed family and, optionally, a live official endpoint.
Audit detects family substitution; it does not prove quantization or
degradation of a same-family backend.

API keys come from the environment or a config file only (env wins).
There is no literal key flag. See README.md for the config format.

Exit codes: 0 identified/match, 1 error, 2 inconclusive, 3 audit mismatch.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitError
	}
	switch args[0] {
	case "uncover":
		return cmdUncover(args[1:], stdout, stderr)
	case "audit":
		return cmdAudit(args[1:], stdout, stderr)
	case "families":
		return cmdFamilies(args[1:], stdout, stderr)
	case "version", "--version", "-version", "-v":
		dbv := "?"
		if db, err := fingerprint.Load(); err == nil {
			dbv = db.Version
		}
		fmt.Fprintf(stdout, "xlfp %s (fingerprint db %s)\n", version, dbv)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return exitError
	}
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "xlfp: %v\n", err)
	return exitError
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type endpointFlags struct {
	endpoint, model, transport, profile, apiKeyEnv string
}

type commonFlags struct {
	configPath    string
	jsonOut       bool
	verbose       bool
	maxRequests   int
	timeout       time.Duration
	minConfidence float64
}

func validateCommon(c commonFlags) error {
	if c.maxRequests <= 0 {
		return fmt.Errorf("--max-requests must be positive")
	}
	if math.IsNaN(c.minConfidence) || math.IsInf(c.minConfidence, 0) || c.minConfidence < 0 || c.minConfidence > 1 {
		return fmt.Errorf("--min-confidence must be between 0 and 1")
	}
	if c.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	return nil
}

func resolveClient(cfg *config.Config, ef endpointFlags, purpose string, timeout time.Duration) (*probe.Client, config.EndpointSpec, error) {
	spec, err := cfg.Resolve(config.Overrides{
		Endpoint: ef.endpoint, Model: ef.model, Transport: ef.transport,
		APIKeyEnv: ef.apiKeyEnv, Profile: ef.profile, Purpose: purpose,
	})
	if err != nil {
		return nil, config.EndpointSpec{}, err
	}
	client, err := probe.New(spec, timeout)
	return client, spec, err
}

type uncoverOutput struct {
	Command   string `json:"command"`
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model"`
	Transport string `json:"transport"`
	fingerprint.Result
}

func cmdUncover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uncover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var ef endpointFlags
	var cf commonFlags
	fs.StringVar(&ef.endpoint, "endpoint", "", "endpoint base URL (host, /v1 base, or full request URL)")
	fs.StringVar(&ef.endpoint, "url", "", "alias for --endpoint")
	fs.StringVar(&ef.model, "model", "", "model name to probe")
	fs.StringVar(&ef.transport, "transport", "", "openai | anthropic | gemini (default: auto-detect from URL)")
	fs.StringVar(&ef.profile, "profile", "", "config profile name")
	fs.StringVar(&ef.apiKeyEnv, "api-key-env", "", "name of env var holding the API key (never the key itself)")
	fs.StringVar(&cf.configPath, "config", "", "config file path")
	fs.BoolVar(&cf.jsonOut, "json", false, "emit JSON")
	fs.BoolVar(&cf.verbose, "verbose", false, "also print technical evidence (tokenizer scores, per-probe counts)")
	fs.IntVar(&cf.maxRequests, "max-requests", 12, "hard cap on probe requests")
	fs.DurationVar(&cf.timeout, "timeout", 30*time.Second, "per-request timeout")
	fs.Float64Var(&cf.minConfidence, "min-confidence", 0, "verdict confidence threshold (0 = default)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if err := validateCommon(cf); err != nil {
		return fail(stderr, err)
	}
	db, err := fingerprint.Load()
	if err != nil {
		return fail(stderr, err)
	}
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		return fail(stderr, err)
	}
	client, spec, err := resolveClient(cfg, ef, "", cf.timeout)
	if err != nil {
		return fail(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := probe.Fingerprint(ctx, client, db, probe.Options{MaxRequests: cf.maxRequests, MinConfidence: cf.minConfidence})
	if err != nil {
		return fail(stderr, err)
	}
	if cf.jsonOut {
		out := uncoverOutput{"uncover", report.SanitizeURL(spec.Endpoint), spec.Model, spec.Transport, res}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
	} else {
		report.Uncover(stdout, spec.Endpoint, spec.Model, spec.Transport, res, cf.verbose)
	}
	if res.Status == fingerprint.StatusIdentified {
		return exitOK
	}
	return exitInconclusive
}

func cmdFamilies(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("families", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	db, err := fingerprint.Load()
	if err != nil {
		return fail(stderr, err)
	}
	if *jsonOut {
		if err := writeJSON(stdout, db); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	report.Families(stdout, db)
	return exitOK
}

type auditOutput struct {
	Command        string `json:"command"`
	TargetEndpoint string `json:"target_endpoint"`
	TargetModel    string `json:"target_model"`
	// OfficialEndpoint is empty when no live baseline was probed.
	OfficialEndpoint string `json:"official_endpoint,omitempty"`
	OfficialModel    string `json:"official_model,omitempty"`
	compare.Result
}

func cmdAudit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var target, official endpointFlags
	var bothTransport, bothModel, bothKeyEnv, claim string
	var cf commonFlags
	fs.StringVar(&target.endpoint, "target-url", "", "target endpoint base URL")
	fs.StringVar(&target.endpoint, "target-endpoint", "", "alias for --target-url")
	fs.StringVar(&official.endpoint, "official-url", "", "official endpoint base URL (optional live baseline)")
	fs.StringVar(&official.endpoint, "official-endpoint", "", "alias for --official-url")
	fs.StringVar(&bothModel, "model", "", "model name for both endpoints")
	fs.StringVar(&target.model, "target-model", "", "target model (overrides --model)")
	fs.StringVar(&official.model, "official-model", "", "official model (overrides --model)")
	fs.StringVar(&bothTransport, "transport", "", "transport for both endpoints")
	fs.StringVar(&target.transport, "target-transport", "", "target transport: openai | anthropic | gemini")
	fs.StringVar(&official.transport, "official-transport", "", "official transport: openai | anthropic | gemini")
	fs.StringVar(&target.profile, "target-profile", "", "config profile for target")
	fs.StringVar(&official.profile, "official-profile", "", "config profile for official")
	fs.StringVar(&bothKeyEnv, "api-key-env", "", "name of env var holding the API key for both endpoints")
	fs.StringVar(&target.apiKeyEnv, "target-api-key-env", "", "name of env var holding the target API key")
	fs.StringVar(&official.apiKeyEnv, "official-api-key-env", "", "name of env var holding the official API key")
	fs.StringVar(&claim, "claim", "", "claimed family, e.g. deepseek (default: inferred from model)")
	fs.StringVar(&cf.configPath, "config", "", "config file path")
	fs.BoolVar(&cf.jsonOut, "json", false, "emit JSON")
	fs.BoolVar(&cf.verbose, "verbose", false, "also print technical evidence (tokenizer scores, per-probe counts)")
	fs.IntVar(&cf.maxRequests, "max-requests", 12, "hard cap on probe requests per endpoint")
	fs.DurationVar(&cf.timeout, "timeout", 30*time.Second, "per-request timeout")
	fs.Float64Var(&cf.minConfidence, "min-confidence", 0, "verdict confidence threshold (0 = default)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if err := validateCommon(cf); err != nil {
		return fail(stderr, err)
	}
	applyBoth := func(ef *endpointFlags) {
		if ef.model == "" {
			ef.model = bothModel
		}
		if ef.transport == "" {
			ef.transport = bothTransport
		}
		if ef.apiKeyEnv == "" {
			ef.apiKeyEnv = bothKeyEnv
		}
	}
	applyBoth(&target)
	applyBoth(&official)
	// The target endpoint may come from flags, XLFP_* purpose env vars, a
	// profile, or config defaults; Resolve reports a precise error if none
	// of them provides one.

	db, err := fingerprint.Load()
	if err != nil {
		return fail(stderr, err)
	}
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		return fail(stderr, err)
	}
	tClient, tSpec, err := resolveClient(cfg, target, "target", cf.timeout)
	if err != nil {
		return fail(stderr, fmt.Errorf("target: %w", err))
	}
	claimed := db.NormalizeFamily(claim)
	if claim != "" && claimed == "" {
		return fail(stderr, fmt.Errorf("unknown claimed family %q (known families: %s)", claim, strings.Join(db.Families(), ", ")))
	}
	if claimed == "" {
		claimed = db.FamilyForModel(tSpec.Model)
		if claimed == "" {
			return fail(stderr, fmt.Errorf("cannot infer claimed family from model %q; pass --claim (known families: %s)", tSpec.Model, strings.Join(db.Families(), ", ")))
		}
	}
	// Resolve the official endpoint before spending any probe budget, so a
	// bad official configuration fails before the target is touched.
	var oClient *probe.Client
	var oSpec config.EndpointSpec
	hasOfficial := official.endpoint != "" || official.profile != "" || os.Getenv("XLFP_OFFICIAL_ENDPOINT") != ""
	if hasOfficial {
		client, spec, err := resolveClient(cfg, official, "official", cf.timeout)
		if err != nil {
			return fail(stderr, fmt.Errorf("official: %w", err))
		}
		oClient, oSpec = client, spec
	}

	var oPtr *fingerprint.Result
	emit := func(res compare.Result) int {
		if cf.jsonOut {
			out := auditOutput{Command: "audit", TargetEndpoint: report.SanitizeURL(tSpec.Endpoint), TargetModel: tSpec.Model, Result: res}
			if oPtr != nil {
				out.OfficialEndpoint = report.SanitizeURL(oSpec.Endpoint)
				out.OfficialModel = oSpec.Model
			}
			if err := writeJSON(stdout, out); err != nil {
				return fail(stderr, err)
			}
		} else {
			officialLabel := ""
			if oPtr != nil {
				officialLabel = oSpec.Endpoint
			}
			report.Audit(stdout, tSpec.Endpoint, officialLabel, res, cf.verbose)
		}
		switch res.Verdict {
		case compare.VerdictMatch:
			return exitOK
		case compare.VerdictMismatch:
			return exitMismatch
		default:
			return exitInconclusive
		}
	}

	// A claim no verified family can judge is predetermined inconclusive;
	// report it without spending probe budget. The fingerprint itself is
	// still available via `xlfp uncover`.
	if !db.IsVerified(claimed) {
		reason := fmt.Sprintf("claimed family %q is not a verified family in database v%s, so no verdict can be rendered for it", claimed, db.Version)
		return emit(compare.Result{
			Verdict: compare.VerdictInconclusive, ClaimedFamily: claimed, Reason: reason, ReasonCode: compare.ReasonUnverifiedClaim,
			Target: fingerprint.Result{Status: fingerprint.StatusInconclusive, Reason: reason + "; endpoint not probed", DBVersion: db.Version, Rankings: []fingerprint.FamilyRank{}, Unverified: []fingerprint.FamilyRank{}, Observations: []fingerprint.Observation{}},
		})
	}

	opts := probe.Options{MaxRequests: cf.maxRequests, MinConfidence: cf.minConfidence}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	tRes, err := probe.Fingerprint(ctx, tClient, db, opts)
	if err != nil {
		return fail(stderr, fmt.Errorf("target: %w", err))
	}
	if hasOfficial {
		oRes, err := probe.Fingerprint(ctx, oClient, db, opts)
		if err != nil {
			// Preserve the completed (paid) target evidence: report the
			// target-only comparison, downgraded to inconclusive because
			// the requested baseline could not be probed.
			res := compare.Evaluate(db, claimed, tRes, nil)
			res.Verdict = compare.VerdictInconclusive
			res.ReasonCode = compare.ReasonOfficialFailed
			res.Reason = fmt.Sprintf("official baseline probing failed (%v); target-only result is shown but no verdict is safe", err)
			return emit(res)
		}
		oPtr = &oRes
	}
	return emit(compare.Evaluate(db, claimed, tRes, oPtr))
}
