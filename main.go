// Command xlfp (xo llm fingerprinter) identifies the model family behind
// an LLM endpoint from its reported token usage, and audits whether a
// provider's claimed family matches its fingerprint.
// See docs/how-it-works.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/tyk-swe/xlfp/pkg/compare"
	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/probe"
	"github.com/tyk-swe/xlfp/pkg/report"
)

// version is overridable at release time via -ldflags -X main.version=...
var version = "0.1.8"

// Exit codes:
const (
	exitOK           = 0 // uncover: identified; audit: match
	exitError        = 1 // operational or usage error
	exitInconclusive = 2 // uncover/audit: inconclusive
	exitMismatch     = 3 // audit: mismatch
)

const usage = `xlfp — find out which AI model family is really behind an endpoint

New here? Just run ` + "`xlfp`" + ` in a terminal to open the dashboard.

Usage:
  xlfp                                      terminal dashboard
  xlfp tui                                  terminal dashboard
  xlfp wizard                               line-by-line guided mode
  xlfp uncover --endpoint URL --model NAME [flags]
  xlfp opencode --model opencode/MODEL [flags]
  xlfp audit --target-url URL --model NAME [--claim FAMILY] [--official-url URL] [flags]
  xlfp tokprobe --endpoint URL --model NAME [flags]
  xlfp numbers --endpoint URL --model NAME [flags]   (or --input FILE offline)
  xlfp sample --endpoint URL --model NAME --family F --out batch.json
  xlfp enroll batch.json [--bank numprint-bank.json]
  xlfp retrain [--bank numprint-bank.json]
  xlfp families [--json]
  xlfp explain
  xlfp version

uncover answers "which family is this endpoint, really?" with a ranked
answer or an honest "inconclusive". audit answers "is this provider
really serving the family it claims?" with MATCH, MISMATCH, or
INCONCLUSIVE. Both work by comparing how the endpoint counts tokens
against known families; neither can prove an exact model version.

For OpenCode stealth models, use xlfp opencode --model opencode/MODEL.
It uses the installed OpenCode CLI and login; direct OpenCode URLs are
blocked. --max-probes limits CLI runs, while OpenCode controls internal calls.

API keys: xlfp never takes a key as a flag value. For unknown endpoints
set XLFP_API_KEY in your environment first. Provider keys such as
OPENAI_API_KEY are only ever sent to that provider's official address.

Exit codes: 0 identified/match, 1 error, 2 inconclusive, 3 audit mismatch.
Run ` + "`xlfp explain`" + ` for a plain-words guide, or ` + "`xlfp uncover --help`" + ` for examples.
`

const uncoverHelp = `xlfp uncover — find out which model family an endpoint really is

Example:
  export XLFP_API_KEY='the key for that endpoint'
  xlfp uncover --endpoint https://provider.example --model stealth-model-x

Sends up to --max-requests (default 12) tiny test requests and matches
the reported token counts against known families. The endpoint may be a
bare host, a /v1-style base, or a full request URL. Add --verbose for
technical evidence, --json for scripts and agents.

Flags:
`

const auditHelp = `xlfp audit — check whether a provider really serves the family it claims

Examples:
  export XLFP_API_KEY='the key for that provider'
  xlfp audit --target-url https://provider.example --model deepseek-chat --claim deepseek

  # Stronger: also probe the official API in the same run as a baseline.
  # Each side needs its own key.
  export XLFP_TARGET_API_KEY='the key for that provider'
  export XLFP_OFFICIAL_API_KEY='your key for the official API'
  xlfp audit --target-url https://provider.example --official-url https://api.deepseek.com \
      --model deepseek-chat --claim deepseek

--claim is guessed from the model name when omitted. Verdicts: MATCH,
MISMATCH, or INCONCLUSIVE (exit codes 0, 3, 2). A MISMATCH is strong
token-count evidence of a different family, not by itself proof of
fraud. Add --verbose for technical evidence, --json for scripts.

Flags:
`

const explainText = `xlfp in plain words

What it does
  Every AI model splits text into chunks called tokens before reading
  it. Different tokenizers split text in different ways, although models
  in different families can share one. xlfp sends a few tiny messages to
  an endpoint, looks at how many tokens the endpoint says each one was,
  and matches that pattern against the families it knows.

Words you will see
  family        A group of related models, which may use different tokenizers.
  IDENTIFIED    The counts match one family strongly.
  INCONCLUSIVE  Not enough trustworthy evidence to say. This is a normal,
                honest outcome — xlfp would rather say "can't tell" than guess.
  MATCH         (audit) The endpoint counts like the claimed family.
  MISMATCH      (audit) The endpoint counts like a different family.
  confidence    How strongly the counts favor the top family over the rest.

What xlfp cannot do
  - Prove an exact model version or checkpoint. Families only.
  - Detect a secretly smaller/weaker model of the same family.
  - Separate Qwen2.5, MiMo-7B, and Kimi-Dev: they share a tokenizer.
  - Give verdicts for Google/Claude yet: their tokenizers are not
    public, so they appear as scored hints, never verdicts.
  - Authenticate the token-count numbers a provider reports.
    Implausible counts are rejected, but convincing fabricated counts
    can fool the fingerprint; a verdict is not proof of honest reporting.

Costs and safety
  - Each check sends up to --max-requests (default 12) tiny, normally
    billable requests per endpoint. Only probe endpoints you are
    authorized to test, and remember the provider can see the requests.
  - API keys come from your environment or config only, never from a
    flag. Use XLFP_API_KEY for unknown endpoints; keys like
    OPENAI_API_KEY are sent only to that provider's official address,
    so probing a stranger's endpoint can never leak them.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// stdinReader and stdinIsTTY gate interactive modes. Tests replace them
// to drive the wizard without a real terminal. Non-interactive invocations
// with no arguments keep the plain usage error.
var (
	stdinReader io.Reader = os.Stdin
	stdinIsTTY            = func() bool { return term.IsTerminal(os.Stdin.Fd()) }
)

// writerIsTTY reports whether w is a terminal (character device file).
func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// probeProgress returns an OnProbe callback that shows a one-line
// progress counter on a terminal, plus a finish func that clears it.
// Off-terminal (pipes, files, agent captures) it stays silent.
func probeProgress(stderr io.Writer, enabled bool, label string) (func(int), func()) {
	return progressCounter(stderr, enabled, label, "request(s) sent")
}

func progressCounter(stderr io.Writer, enabled bool, label, unit string) (func(int), func()) {
	if !enabled || !writerIsTTY(stderr) {
		return nil, func() {}
	}
	return func(n int) {
			fmt.Fprintf(stderr, "\rProbing %s: %d %s...  ", label, n, unit)
		}, func() {
			fmt.Fprintf(stderr, "\r%s\r", strings.Repeat(" ", 48))
		}
}

// checkedWriter retains errors from plain-text renderers as well as JSON.
type checkedWriter struct {
	w   io.Writer
	err error
}

func (w *checkedWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.w.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

func run(args []string, stdout, stderr io.Writer) (code int) {
	rawOut := stdout
	out := &checkedWriter{w: stdout}
	stdout = out
	defer func() {
		if out.err != nil {
			code = fail(stderr, fmt.Errorf("writing output: %w", out.err))
		}
	}()
	if len(args) == 0 {
		if stdinIsTTY() {
			return interactiveCommand("tui", nil, rawOut, stdout, stderr)
		}
		fmt.Fprint(stderr, usage)
		return exitError
	}
	switch args[0] {
	case "tui", "wizard":
		return interactiveCommand(args[0], args[1:], rawOut, stdout, stderr)
	case "uncover":
		return cmdUncover(args[1:], stdout, stderr)
	case "opencode":
		return cmdOpenCode(args[1:], stdout, stderr)
	case "audit":
		return cmdAudit(args[1:], stdout, stderr)
	case "tokprobe":
		return cmdTokprobe(args[1:], stdout, stderr)
	case "numbers":
		return cmdNumbers(args[1:], stdout, stderr)
	case "sample":
		return cmdSample(args[1:], stdout, stderr)
	case "enroll":
		return cmdEnroll(args[1:], stdout, stderr)
	case "retrain":
		return cmdRetrain(args[1:], stdout, stderr)
	case "families":
		return cmdFamilies(args[1:], stdout, stderr)
	case "explain":
		return cmdExplain(args[1:], stdout, stderr)
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

// failWithHint is fail plus a beginner-actionable "Fix:" line for the
// common setup mistakes (missing endpoint/model/key/claim).
func failWithHint(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "xlfp: %v\n", err)
	if h := hintFor(err); h != "" {
		fmt.Fprintf(stderr, "Fix: %s\n", h)
	}
	return exitError
}

func hintFor(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no endpoint:"):
		return "pass the API address, e.g. --endpoint https://provider.example — or run `xlfp` in a terminal for guided mode"
	case strings.Contains(msg, "no model:"):
		return "pass the model name exactly as the provider calls it, e.g. --model deepseek-chat"
	case strings.Contains(msg, "is empty or unset"):
		return "set it first, e.g. export XLFP_API_KEY='your-key' (xlfp never takes a key as a flag value)"
	case strings.Contains(msg, "claimed family"), strings.Contains(msg, "cannot infer"):
		return "pass a family name like deepseek or qwen; `xlfp families` lists what this build knows"
	}
	return ""
}

func cmdExplain(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] != "-h" && args[0] != "--help" && args[0] != "help" {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(args, " ")))
	}
	fmt.Fprint(stdout, explainText)
	return exitOK
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
	fs.Usage = func() { fmt.Fprint(stderr, uncoverHelp); fs.PrintDefaults() }
	var ef endpointFlags
	var cf commonFlags
	fs.StringVar(&ef.endpoint, "endpoint", "", "endpoint base URL (host, /v1 base, or full request URL)")
	fs.StringVar(&ef.endpoint, "url", "", "alias for --endpoint")
	fs.StringVar(&ef.model, "model", "", "model name to probe")
	fs.StringVar(&ef.transport, "transport", "", "openai | anthropic | gemini | responses (default: auto-detect from URL)")
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
	p, err := prepareCheck("uncover", ef, endpointFlags{}, "", false, cf)
	if err != nil {
		return failSetup(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	onProbe, done := probeProgress(stderr, !cf.jsonOut || cf.verbose, "endpoint")
	r := p.execute(ctx, func(_ string, n int) {
		if onProbe != nil {
			onProbe(n)
		}
	})
	done()
	return emitCheckResult(p, r, stdout, stderr)
}

func cmdFamilies(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("families", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "xlfp families — list the model families this build can recognize\n\nFlags:\n")
		fs.PrintDefaults()
	}
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
	// OfficialEndpoint is empty when no live baseline was requested.
	OfficialEndpoint string `json:"official_endpoint,omitempty"`
	OfficialModel    string `json:"official_model,omitempty"`
	compare.Result
}

func cmdAudit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, auditHelp); fs.PrintDefaults() }
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
	fs.StringVar(&target.transport, "target-transport", "", "target transport: openai | anthropic | gemini | responses")
	fs.StringVar(&official.transport, "official-transport", "", "official transport: openai | anthropic | gemini | responses")
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
	explicitOfficial := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "official-url" || f.Name == "official-endpoint" || f.Name == "official-profile" {
			explicitOfficial = true
		}
	})
	p, err := prepareCheck("audit", target, official, claim, hasOfficialEndpoint(official, explicitOfficial), cf)
	if err != nil {
		return failSetup(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var done func()
	var onProbe func(int)
	lastSide := ""
	r := p.execute(ctx, func(side string, n int) {
		if side != lastSide {
			if done != nil {
				done()
			}
			lastSide = side
			onProbe, done = probeProgress(stderr, !cf.jsonOut || cf.verbose, side)
		}
		if onProbe != nil {
			onProbe(n)
		}
	})
	if done != nil {
		done()
	}
	return emitCheckResult(p, r, stdout, stderr)
}

func failSetup(stderr io.Writer, err error) int {
	var setup *checkSetupError
	if errors.As(err, &setup) && setup.hint {
		return failWithHint(stderr, err)
	}
	return fail(stderr, err)
}

// emitCheckResult is shared by CLI and dashboard report views.
func emitCheckResult(p *preparedCheck, r checkResult, stdout, stderr io.Writer) int {
	if r.err != nil {
		return fail(stderr, r.err)
	}
	if p.mode == "opencode" {
		return emitOpenCodeResult(p, r, stdout, stderr)
	}
	cf := p.common
	if p.mode == "uncover" {
		if r.uncover == nil {
			return fail(stderr, fmt.Errorf("check produced no result"))
		}
		res := *r.uncover
		if cf.jsonOut {
			out := uncoverOutput{"uncover", report.SanitizeURL(p.targetSpec.Endpoint), p.targetSpec.Model, p.targetSpec.Transport, res}
			if err := writeJSON(stdout, out); err != nil {
				return fail(stderr, err)
			}
		} else {
			report.Uncover(stdout, p.targetSpec.Endpoint, p.targetSpec.Model, p.targetSpec.Transport, res, cf.verbose)
		}
		if res.Status == fingerprint.StatusIdentified {
			return exitOK
		}
		return exitInconclusive
	}
	if r.audit == nil {
		return fail(stderr, fmt.Errorf("check produced no result"))
	}
	res := *r.audit
	if cf.jsonOut {
		out := auditOutput{Command: "audit", TargetEndpoint: report.SanitizeURL(p.targetSpec.Endpoint), TargetModel: p.targetSpec.Model, Result: res}
		if p.official != nil {
			out.OfficialEndpoint = report.SanitizeURL(p.officialSpec.Endpoint)
			out.OfficialModel = p.officialSpec.Model
		}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
	} else {
		officialLabel := ""
		if p.official != nil {
			officialLabel = p.officialSpec.Endpoint
		}
		report.Audit(stdout, p.targetSpec.Endpoint, officialLabel, res, cf.verbose)
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
