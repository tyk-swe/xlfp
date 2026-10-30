package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/numprint"
	"github.com/tyk-swe/xlfp/pkg/probe"
	"github.com/tyk-swe/xlfp/pkg/report"
)

func addEndpointFlags(fs *flag.FlagSet, ef *endpointFlags, cf *commonFlags, timeoutDefault time.Duration) {
	fs.StringVar(&ef.endpoint, "endpoint", "", "endpoint base URL (host, /v1 base, or full request URL)")
	fs.StringVar(&ef.endpoint, "url", "", "alias for --endpoint")
	fs.StringVar(&ef.model, "model", "", "model name to probe")
	fs.StringVar(&ef.transport, "transport", "", "openai | anthropic | gemini | responses (default: auto-detect from URL)")
	fs.StringVar(&ef.profile, "profile", "", "config profile name")
	fs.StringVar(&ef.apiKeyEnv, "api-key-env", "", "name of env var holding the API key (never the key itself)")
	fs.StringVar(&cf.configPath, "config", "", "config file path")
	fs.BoolVar(&cf.jsonOut, "json", false, "emit JSON")
	fs.BoolVar(&cf.verbose, "verbose", false, "also print technical evidence")
	fs.DurationVar(&cf.timeout, "timeout", timeoutDefault, "per-request timeout")
}

func resolveEndpointClient(ef endpointFlags, cf commonFlags) (*probe.Client, config.EndpointSpec, error) {
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		return nil, config.EndpointSpec{}, err
	}
	return resolveClient(cfg, ef, "", cf.timeout)
}

func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func slugify(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func joinInts(seq []int) string {
	parts := make([]string, len(seq))
	for i, v := range seq {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, " ")
}

type tokprobeOutput struct {
	Command   string `json:"command"`
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model"`
	Transport string `json:"transport"`
	fingerprint.TokResult
}

func cmdTokprobe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tokprobe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "xlfp tokprobe — identify the upstream tokenizer with differential token counts\n\nSends a wrapper-only baseline plus adaptively chosen wrapped probes and\ncomputes a Bayesian open-set verdict (exact / related / unknown) with a\nclaim check against --model. A tokenizer match never proves an exact\nmodel version. Add --verbose for candidate probabilities, --json for scripts.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	var ef endpointFlags
	var cf commonFlags
	addEndpointFlags(fs, &ef, &cf, 30*time.Second)
	fs.IntVar(&cf.maxRequests, "max-requests", 24, "hard cap on probe requests (baselines included)")
	maxProbes := fs.Int("max-probes", 0, "cap on probe texts (0 = all)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if cf.maxRequests <= 0 {
		return fail(stderr, fmt.Errorf("--max-requests must be positive"))
	}
	if *maxProbes < 0 || (*maxProbes > 0 && *maxProbes < fingerprint.MinTokProbes) {
		return fail(stderr, fmt.Errorf("--max-probes must be 0 or at least %d", fingerprint.MinTokProbes))
	}
	db, err := fingerprint.Load()
	if err != nil {
		return fail(stderr, err)
	}
	client, spec, err := resolveEndpointClient(ef, cf)
	if err != nil {
		return failWithHint(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	onProbe, done := probeProgress(stderr, !cf.jsonOut || cf.verbose, "endpoint")
	opts := probe.TokOptions{MaxRequests: cf.maxRequests, MaxProbes: *maxProbes}
	if onProbe != nil {
		opts.OnProbe = onProbe
	}
	res, err := probe.TokenizerProbe(ctx, client, db, spec.Model, opts)
	done()
	if err != nil && len(res.Observations) < fingerprint.MinTokProbes {
		return fail(stderr, err)
	}
	if err != nil && res.ProbeError == "" {
		res.ProbeError = err.Error()
	}
	if cf.jsonOut {
		out := tokprobeOutput{Command: "tokprobe", Endpoint: report.SanitizeURL(spec.Endpoint), Model: spec.Model, Transport: spec.Transport, TokResult: res}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
	} else {
		report.TokProbe(stdout, report.SanitizeURL(spec.Endpoint), spec.Model, spec.Transport, res, cf.verbose)
	}
	if res.Verdict == fingerprint.TokVerdictInconclusive {
		return exitInconclusive
	}
	return exitOK
}

type numbersOutput struct {
	Command     string `json:"command"`
	Endpoint    string `json:"endpoint,omitempty"`
	Model       string `json:"model,omitempty"`
	Transport   string `json:"transport,omitempty"`
	BankVersion string `json:"bank_version"`
	probe.NumRunResult
}

// offlineRounds parses a saved numbers report, an answers array, a
// sampling batch, or a plain-text reply into scoreable rounds.
func offlineRounds(raw []byte) ([]probe.NumRound, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("input is empty")
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		// Manual mode: a pasted reply as plain text.
		return []probe.NumRound{{Round: 1, Answers: []numprint.Answer{{Text: trimmed, Completed: true}}}}, nil
	}
	if trimmed[0] == '[' {
		var answers []numprint.Answer
		if err := json.Unmarshal(raw, &answers); err != nil {
			return nil, fmt.Errorf("input: %w", err)
		}
		for i := range answers {
			if !answers[i].Truncated {
				answers[i].Completed = true
			}
		}
		return []probe.NumRound{{Round: 1, Answers: answers}}, nil
	}
	var probeDoc struct {
		Rounds  []probe.NumRound       `json:"rounds"`
		Answers []numprint.Answer      `json:"answers"`
		Outputs []numprint.Answer      `json:"outputs"`
		Samples []numprint.BatchSample `json:"samples"`
	}
	if err := json.Unmarshal(raw, &probeDoc); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	switch {
	case len(probeDoc.Rounds) > 0:
		return probeDoc.Rounds, nil
	case len(probeDoc.Answers) > 0:
		return []probe.NumRound{{Round: 1, Answers: probeDoc.Answers}}, nil
	case len(probeDoc.Outputs) > 0:
		return []probe.NumRound{{Round: 1, Answers: probeDoc.Outputs}}, nil
	case len(probeDoc.Samples) > 0:
		// A sampling batch: chunk samples into rounds of three.
		var rounds []probe.NumRound
		for i := 0; i < len(probeDoc.Samples); i += 3 {
			var answers []numprint.Answer
			for _, s := range probeDoc.Samples[i:min(i+3, len(probeDoc.Samples))] {
				text := s.Text
				if text == "" {
					text = joinInts(s.Sequence)
				}
				answers = append(answers, numprint.Answer{ChallengeID: s.ID, ExpectedCount: s.ExpectedCount, Text: text, Completed: true})
			}
			rounds = append(rounds, probe.NumRound{Round: len(rounds) + 1, Answers: answers})
		}
		return rounds, nil
	}
	return nil, fmt.Errorf("input: no rounds, answers, outputs, or samples found")
}

func cmdNumbers(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("numbers", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "xlfp numbers — rank the likely model from first-instinct number replies\n\nLive: sends --count number-writing challenges per round (default 3) and\nranks the value preferences against the reference bank. Offline:\n--input FILE re-scores a saved report, an answers array, a sampling\nbatch, or a pasted reply (\"-\" for stdin) without any requests.\nThe ranking is closed-set within the bank and never proves identity.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	var ef endpointFlags
	var cf commonFlags
	addEndpointFlags(fs, &ef, &cf, 120*time.Second)
	rounds := fs.Int("rounds", 1, "sequential detection rounds")
	count := fs.Int("count", 3, "samples per round (1-3; fewer than 3 give a ranking only)")
	parallel := fs.Int("parallel", 0, "concurrent samples per round (0 = --count)")
	strict := fs.Bool("strict", false, "require complete, non-truncated answers")
	noStream := fs.Bool("no-stream", false, "use plain JSON responses instead of streaming")
	effort := fs.String("effort", "", "reasoning effort passthrough (openai/responses)")
	serviceTier := fs.String("service-tier", "", "service tier passthrough (openai/responses)")
	bankPath := fs.String("bank", "", "custom number reference bank JSON (default: embedded)")
	challengesPath := fs.String("challenges", "", "reuse saved challenges JSON every round (length must equal --count)")
	inputPath := fs.String("input", "", "score saved answers offline instead of probing (\"-\" = stdin)")
	outputPath := fs.String("output", "", "also save the full report (challenges, replies, results) as JSON")
	maxRequests := fs.Int("max-requests", 0, "hard cap on requests (0 = exactly --rounds * --count)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if *rounds < 1 || *rounds > 20 {
		return fail(stderr, fmt.Errorf("--rounds must be between 1 and 20"))
	}
	if *count < 1 || *count > 3 {
		return fail(stderr, fmt.Errorf("--count must be between 1 and 3"))
	}
	if *parallel < 0 || *parallel > 3 {
		return fail(stderr, fmt.Errorf("--parallel must be between 0 and 3"))
	}
	bank, err := numprint.LoadBankFile(*bankPath)
	if err != nil {
		return fail(stderr, err)
	}
	out := numbersOutput{Command: "numbers", BankVersion: bank.Version}
	if *inputPath != "" {
		var raw []byte
		var err error
		if *inputPath == "-" {
			raw, err = io.ReadAll(stdinReader)
		} else {
			raw, err = os.ReadFile(*inputPath)
		}
		if err != nil {
			return fail(stderr, err)
		}
		saved, err := offlineRounds(raw)
		if err != nil {
			return fail(stderr, err)
		}
		wins := map[string]int{}
		for i := range saved {
			saved[i].Round = i + 1
			saved[i].Result = bank.Score(saved[i].Answers, numprint.ScoreOptions{Strict: *strict})
			if saved[i].Result.Status == numprint.StatusRanked {
				wins[saved[i].Result.TopEntryID]++
			}
		}
		out.Rounds = saved
		best, bestN := "", 0
		for id, n := range wins {
			if n > bestN || (n == bestN && (best == "" || id < best)) {
				best, bestN = id, n
			}
		}
		out.MostFrequent = best
	} else {
		if *maxRequests > 0 && *rounds**count > *maxRequests {
			return fail(stderr, fmt.Errorf("--rounds * --count = %d exceeds --max-requests %d", *rounds**count, *maxRequests))
		}
		var fixed []numprint.Challenge
		if *challengesPath != "" {
			raw, err := os.ReadFile(*challengesPath)
			if err != nil {
				return fail(stderr, err)
			}
			if err := json.Unmarshal(raw, &fixed); err != nil {
				return fail(stderr, fmt.Errorf("challenges: %w", err))
			}
			if len(fixed) != *count {
				return fail(stderr, fmt.Errorf("challenges file has %d challenges, --count is %d", len(fixed), *count))
			}
		}
		client, spec, err := resolveEndpointClient(ef, cf)
		if err != nil {
			return failWithHint(stderr, err)
		}
		out.Endpoint, out.Model, out.Transport = report.SanitizeURL(spec.Endpoint), spec.Model, spec.Transport
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		onProbe, done := probeProgress(stderr, !cf.jsonOut || cf.verbose, "endpoint")
		opts := probe.NumOptions{
			Rounds: *rounds, Count: *count, Parallel: *parallel, Strict: *strict,
			Stream: !*noStream, Challenges: fixed, Effort: *effort, ServiceTier: *serviceTier,
		}
		total := 0
		if onProbe != nil {
			opts.OnSample = func(int, int) { total++; onProbe(total) }
		}
		out.NumRunResult = probe.RunNumbers(ctx, client, bank, opts)
		done()
		if ctx.Err() != nil && len(out.Rounds) == 0 {
			return fail(stderr, ctx.Err())
		}
	}
	if *outputPath != "" {
		raw, err := json.MarshalIndent(out, "", "  ")
		if err == nil {
			err = os.WriteFile(*outputPath, append(raw, '\n'), 0o644)
		}
		if err != nil {
			return fail(stderr, err)
		}
	}
	if cf.jsonOut {
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
	} else {
		if out.Endpoint != "" {
			report.NumbersHeader(stdout, out.Endpoint, out.Model, out.Transport, out.RequestsSpent)
		}
		for _, r := range out.Rounds {
			report.NumRound(stdout, r.Round, r.Result, cf.verbose)
		}
		if len(out.Rounds) > 1 && out.MostFrequent != "" {
			fmt.Fprintf(stdout, "\nMost frequent round winner: %s (a count of round wins, not a combined probability).\n", out.MostFrequent)
		}
		report.NumbersFooter(stdout)
	}
	for _, r := range out.Rounds {
		if r.Result.Status != numprint.StatusRanked {
			return exitInconclusive
		}
	}
	if len(out.Rounds) == 0 {
		return exitInconclusive
	}
	return exitOK
}

func cmdSample(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "xlfp sample — collect a reference batch for the number bank\n\nSends the fixed 36-challenge sampling set (12 prompt environments × 3)\nto ONE endpoint you trust to be the named model — use official provider\nAPIs. Results and failures are saved after every sample; --resume\ncontinues an existing batch file. Then: xlfp enroll, xlfp retrain.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	var ef endpointFlags
	var cf commonFlags
	addEndpointFlags(fs, &ef, &cf, 120*time.Second)
	family := fs.String("family", "", "claimed family of the sampled model (required)")
	entryID := fs.String("entry-id", "", "bank entry id (default: slug of --model)")
	display := fs.String("display", "", "display name (default: --model)")
	outPath := fs.String("out", "", "batch JSON file to write (required)")
	resume := fs.Bool("resume", false, "continue an existing batch file, skipping collected samples")
	noStream := fs.Bool("no-stream", false, "use plain JSON responses instead of streaming")
	effort := fs.String("effort", "", "reasoning effort passthrough (openai/responses)")
	serviceTier := fs.String("service-tier", "", "service tier passthrough (openai/responses)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if *family == "" {
		return fail(stderr, fmt.Errorf("--family is required (the family this endpoint is trusted to be)"))
	}
	if *outPath == "" {
		return fail(stderr, fmt.Errorf("--out is required"))
	}
	client, spec, err := resolveEndpointClient(ef, cf)
	if err != nil {
		return failWithHint(stderr, err)
	}
	id := *entryID
	if id == "" {
		id = slugify(spec.Model)
	}
	batch := &numprint.Batch{
		Version: "1", EntryID: id, Family: strings.ToLower(*family), Display: *display,
		Model: spec.Model, EndpointHost: endpointHost(spec.Endpoint),
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}
	doneIDs := map[string]bool{}
	if *resume {
		if existing, err := numprint.LoadBatchFile(*outPath); err == nil {
			if existing.EntryID != batch.EntryID || existing.Model != batch.Model || existing.Family != batch.Family {
				return fail(stderr, fmt.Errorf("--resume: existing batch is for %s/%s (%s), flags say %s/%s (%s)", existing.EntryID, existing.Model, existing.Family, batch.EntryID, batch.Model, batch.Family))
			}
			batch = existing
			for _, s := range batch.Samples {
				doneIDs[s.ID] = true
			}
		} else if !os.IsNotExist(err) {
			return fail(stderr, err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	set := numprint.SamplingSet()
	for i, sc := range set {
		if doneIDs[sc.ID] {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		fmt.Fprintf(stderr, "sampling %d/%d (%s)...\n", i+1, len(set), sc.ID)
		req := probe.ChatRequest{System: sc.System, User: sc.Prompt(), Stream: !*noStream, Effort: *effort, ServiceTier: *serviceTier}
		if !*noStream {
			expected := sc.Challenge.ExpectedCount
			req.StopWhen = func(text string) bool { return numprint.CountIntegers(text) >= expected }
		} else {
			req.Stream = false
		}
		res, err := client.Chat(ctx, req)
		switch {
		case err != nil:
			batch.Failures = append(batch.Failures, numprint.BatchFailure{ID: sc.ID, Error: err.Error()})
		case res.Error != "":
			batch.Failures = append(batch.Failures, numprint.BatchFailure{ID: sc.ID, Error: res.Error})
		default:
			seq := numprint.ParseSequence(res.Text)
			if verr := numprint.ValidateSequence(seq, sc.Challenge.ExpectedCount); verr != nil {
				batch.Failures = append(batch.Failures, numprint.BatchFailure{ID: sc.ID, Error: verr.Error()})
			} else {
				batch.Samples = append(batch.Samples, numprint.BatchSample{
					ID: sc.ID, Environment: sc.Environment, ExpectedCount: sc.Challenge.ExpectedCount,
					Text: res.Text, Sequence: seq,
				})
			}
		}
		if err := numprint.SaveBatchFile(*outPath, batch); err != nil {
			return fail(stderr, err)
		}
	}
	fmt.Fprintf(stdout, "Batch %s: %d samples, %d failures recorded in %s\n", batch.EntryID, len(batch.Samples), len(batch.Failures), *outPath)
	if len(batch.Samples) == 0 {
		return exitError
	}
	return exitOK
}

func cmdEnroll(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "xlfp enroll BATCH — validate a sampling batch and add it to a number bank\n\nUsage: xlfp enroll batch.json [--bank numprint-bank.json]\nA missing bank file starts from the embedded bank. Sample ids are\nnever overwritten. Run xlfp retrain afterwards.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	bankPath := fs.String("bank", "numprint-bank.json", "number bank JSON file to update")
	// Accept flags before or after the positional BATCH argument.
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if err == flag.ErrHelp {
				return exitOK
			}
			return exitError
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
		if len(rest) == 0 {
			break
		}
	}
	if len(positional) != 1 {
		return fail(stderr, fmt.Errorf("usage: xlfp enroll BATCH [--bank FILE]"))
	}
	batch, err := numprint.LoadBatchFile(positional[0])
	if err != nil {
		return fail(stderr, err)
	}
	bank, err := numprint.LoadBankFile(*bankPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fail(stderr, err)
		}
		bank, err = numprint.Load()
		if err != nil {
			return fail(stderr, err)
		}
	}
	added, err := bank.Enroll(batch)
	if err != nil {
		return fail(stderr, err)
	}
	if err := numprint.SaveBankFile(*bankPath, bank); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Enrolled %d samples into entry %s (%s). %d failures stayed on record in the batch.\n", added, batch.EntryID, *bankPath, len(batch.Failures))
	return exitOK
}

func cmdRetrain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("retrain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "xlfp retrain — refit number-bank parameters from its own samples\n\nFits the reply-check limits and the confidence temperature by\nleave-one-out evaluation, writes them back into the bank, and\nprints the evaluation report. --export-features writes labeled\nfeature vectors for the offline LDA trainer; --lda loads its output.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	bankPath := fs.String("bank", "numprint-bank.json", "number bank JSON file to refit in place")
	exportPath := fs.String("export-features", "", "write labeled feature vectors JSON for tools/train_numprint.py")
	ldaPath := fs.String("lda", "", "load LDA parameters JSON produced by tools/train_numprint.py")
	jsonOut := fs.Bool("json", false, "emit the evaluation report as JSON only")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitError
	}
	if fs.NArg() > 0 {
		return fail(stderr, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	bank, err := numprint.LoadBankFile(*bankPath)
	if err != nil {
		return fail(stderr, err)
	}
	if *exportPath != "" {
		raw, err := json.MarshalIndent(bank.ExportFeatures(), "", "  ")
		if err == nil {
			err = os.WriteFile(*exportPath, append(raw, '\n'), 0o644)
		}
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "features exported to %s\n", *exportPath)
	}
	if *ldaPath != "" {
		raw, err := os.ReadFile(*ldaPath)
		if err != nil {
			return fail(stderr, err)
		}
		var lda numprint.LDAParams
		if err := json.Unmarshal(raw, &lda); err != nil {
			return fail(stderr, fmt.Errorf("lda params: %w", err))
		}
		bank.Params.LDA = &lda
	}
	rep, err := numprint.Retrain(bank)
	if err != nil {
		return fail(stderr, err)
	}
	if err := numprint.SaveBankFile(*bankPath, bank); err != nil {
		return fail(stderr, err)
	}
	if *jsonOut {
		if err := writeJSON(stdout, rep); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "Retrained %s: %d samples, leave-one-out accuracy %.1f%% (%d/%d), temperature %.2f\n", *bankPath, rep.Samples, rep.Accuracy*100, rep.Top1, rep.Samples, rep.Temperature)
	fmt.Fprintf(stdout, "Reply-check limits: autocorrelation > %.3f and small-step share > %.3f mark a non-model reply (fitted: %v)\n", rep.SeqLimits.Autocorr, rep.SeqLimits.SmallStep, rep.SeqLimits.Fitted)
	for id, acc := range rep.PerEntry {
		fmt.Fprintf(stdout, "  %-24s %.1f%%\n", id, acc*100)
	}
	return exitOK
}
