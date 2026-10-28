package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/opencode"
	"github.com/tyk-swe/xlfp/pkg/report"
)

const opencodeHelp = `xlfp opencode — identify a stealth model family through OpenCode

Install OpenCode from https://opencode.ai/docs and sign in with
opencode auth login. List models with opencode models opencode.

Example:
  xlfp opencode --model opencode/big-pickle --json

Uses the real OpenCode CLI with fresh sessions and isolated configuration.
--max-probes limits CLI probe runs. OpenCode controls internal requests and
retries, so this is not a provider-request cap. No direct HTTP fallback.
Requires OpenCode v1 JSON events and debug config (reference v1.19.4).
Refuses probes when resolved settings differ from the isolated probe configuration.

Flags:
`

func prepareOpenCode(binary, model string, cf commonFlags) (*preparedCheck, error) {
	if cf.maxRequests <= 0 {
		return nil, fmt.Errorf("--max-probes must be positive")
	}
	if err := validateCommon(cf); err != nil {
		return nil, err
	}
	client, err := opencode.New(binary, model, cf.timeout)
	if err != nil {
		return nil, err
	}
	db, err := fingerprint.Load()
	if err != nil {
		return nil, err
	}
	return &preparedCheck{mode: "opencode", common: cf, db: db, opencode: client,
		targetSpec: config.EndpointSpec{Model: model}}, nil
}

func cmdOpenCode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("opencode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, opencodeHelp); fs.PrintDefaults() }
	var model, binary string
	var cf commonFlags
	fs.StringVar(&model, "model", "", "required model selector: opencode/<model-id>")
	fs.StringVar(&binary, "opencode-bin", "opencode", "installed OpenCode executable")
	fs.IntVar(&cf.maxRequests, "max-probes", 12, "maximum CLI probe runs (not provider requests)")
	fs.DurationVar(&cf.timeout, "timeout", 60*time.Second, "per-probe timeout")
	fs.Float64Var(&cf.minConfidence, "min-confidence", 0, "verdict confidence threshold (0 = default)")
	fs.BoolVar(&cf.jsonOut, "json", false, "emit JSON")
	fs.BoolVar(&cf.verbose, "verbose", false, "also print technical evidence")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	p, err := prepareOpenCode(binary, model, cf)
	if err != nil {
		return fail(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	onProbe, done := progressCounter(stderr, !cf.jsonOut || cf.verbose, "OpenCode", "probe run(s) completed")
	r := p.execute(ctx, func(_ string, n int) {
		if onProbe != nil {
			onProbe(n)
		}
	})
	done()
	return emitCheckResult(p, r, stdout, stderr)
}

func runOpenCodeWizard(ask func(string) (string, bool), abort func() int, stdout, stderr io.Writer) int {
	model, ok := ask("\nList models with `opencode models opencode`.\nModel (e.g. opencode/big-pickle): ")
	if !ok || model == "" {
		return abort()
	}
	binary, ok := ask("OpenCode executable [opencode]: ")
	if !ok {
		return abort()
	}
	if binary == "" {
		binary = "opencode"
	}
	if _, err := prepareOpenCode(binary, model, commonFlags{maxRequests: 12, timeout: 60 * time.Second}); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "\nEquivalent command:\n  xlfp opencode --model %s --opencode-bin %s\n", shellQuote(model), shellQuote(binary))
	answer, ok := ask("\nRun up to 12 probes through OpenCode using your existing login?\nOpenCode controls internal requests and retries; usage may be billable.\nOnly continue if you are authorized to test this model.\nContinue? [Y/n]: ")
	if !ok || !isYes(answer, true) {
		return abort()
	}
	return cmdOpenCode([]string{"--model", model, "--opencode-bin", binary}, stdout, stderr)
}

// fingerprintEvidence deliberately omits the HTTP-only requests_spent field.
// Both successful reports and cancelled dashboard records use this projection.
type fingerprintEvidence struct {
	Status       string                    `json:"status"`
	TopFamily    string                    `json:"top_family,omitempty"`
	Reason       string                    `json:"reason,omitempty"`
	ReasonCode   string                    `json:"reason_code,omitempty"`
	Rankings     []fingerprint.FamilyRank  `json:"rankings"`
	Unverified   []fingerprint.FamilyRank  `json:"unverified_rankings"`
	Observations []fingerprint.Observation `json:"observations"`
	DBVersion    string                    `json:"db_version"`
}

func evidenceFor(r fingerprint.Result) fingerprintEvidence {
	return fingerprintEvidence{r.Status, r.TopFamily, r.Reason, r.ReasonCode,
		r.Rankings, r.Unverified, r.Observations, r.DBVersion}
}

type opencodeOutput struct {
	Command         string `json:"command"`
	Model           string `json:"model"`
	OpenCodeVersion string `json:"opencode_version"`
	ProbesSpent     int    `json:"probes_spent"`
	fingerprintEvidence
}

func emitOpenCodeResult(p *preparedCheck, r checkResult, stdout, stderr io.Writer) int {
	if r.uncover == nil {
		return fail(stderr, fmt.Errorf("OpenCode check produced no result"))
	}
	res := *r.uncover
	if p.common.jsonOut {
		out := opencodeOutput{"opencode", p.opencode.Model(), p.opencode.Version(), r.targetSpent, evidenceFor(res)}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
	} else {
		report.OpenCode(stdout, p.opencode.Model(), p.opencode.Version(), res, p.common.verbose)
	}
	if res.Status == fingerprint.StatusIdentified {
		return exitOK
	}
	return exitInconclusive
}
