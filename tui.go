package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
	"github.com/tyk-swe/xlfp/pkg/probe"
	"github.com/tyk-swe/xlfp/pkg/report"
)

const interactiveHelp = "Usage: xlfp %s\n\nRun xlfp's %s in an interactive terminal.\n"

var tuiMenuLabels = []string{"Uncover a model family", "Audit a provider claim", "OpenCode stealth test", "Browse families", "How xlfp works", "Session results", "Quit"}

// tuiAvailable is a variable so routing can be tested without a real terminal.
var tuiAvailable = func(in io.Reader, out io.Writer) bool {
	i, iok := in.(*os.File)
	o, ook := out.(*os.File)
	return iok && ook && term.IsTerminal(i.Fd()) && term.IsTerminal(o.Fd()) && os.Getenv("TERM") != "dumb"
}

// Preserve the terminal file descriptor through the checked output writer so
// Bubble Tea can discover dimensions and enable Windows terminal processing.
type tuiTerminalWriter struct {
	file   *os.File
	writer io.Writer
}

func (w tuiTerminalWriter) Read(p []byte) (int, error)  { return w.file.Read(p) }
func (w tuiTerminalWriter) Write(p []byte) (int, error) { return w.writer.Write(p) }
func (w tuiTerminalWriter) Close() error                { return nil }
func (w tuiTerminalWriter) Fd() uintptr                 { return w.file.Fd() }

func interactiveCommand(command string, args []string, rawOut, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		label := "terminal dashboard"
		if command == "wizard" {
			label = "line-by-line wizard"
		}
		fmt.Fprintf(stdout, interactiveHelp, command, label)
		return exitOK
	}
	if len(args) != 0 {
		return fail(stderr, fmt.Errorf("%s takes no arguments; use --help", command))
	}
	if !stdinIsTTY() {
		return fail(stderr, fmt.Errorf("%s requires interactive terminal input", command))
	}
	if command == "wizard" || !tuiAvailable(stdinReader, rawOut) {
		return runWizard(stdinReader, stdout, stderr)
	}
	model := newTUI()
	output := tuiTerminalWriter{file: rawOut.(*os.File), writer: stdout}
	program := tea.NewProgram(model, tea.WithInput(stdinReader), tea.WithOutput(output), tea.WithAltScreen())
	_, err := program.Run()
	model.shutdown()
	if err != nil {
		if !model.started {
			fmt.Fprintln(stderr, "xlfp: dashboard unavailable; starting the line-by-line wizard")
			return runWizard(stdinReader, stdout, stderr)
		}
		return fail(stderr, fmt.Errorf("dashboard: %w", err))
	}
	return exitOK
}

type tuiField struct {
	name, label string
	input       textinput.Model
	endpoint    bool
}

type tuiRecord struct{ title, plain, verbose, json string }
type tuiProgress struct {
	id       int
	side     string
	requests int
}
type tuiFinished struct {
	id     int
	result checkResult
}

type tuiModel struct {
	screen, mode                        string
	width, height                       int
	menu, focus, historyIndex, viewMode int
	fields                              []tuiField
	message                             string
	prepared                            *preparedCheck
	history                             []tuiRecord
	current                             tuiRecord
	viewport                            viewport.Model
	events                              chan tea.Msg
	stopEvents                          chan struct{}
	workerDone                          chan struct{}
	cancel                              context.CancelFunc
	runID, targetSpent, officialSpent   int
	cancelling, started                 bool
}

func newTUI() *tuiModel {
	return &tuiModel{screen: "home", width: 80, height: 24, events: make(chan tea.Msg, 32), stopEvents: make(chan struct{}), viewport: viewport.New(80, 18)}
}
func (m *tuiModel) Init() tea.Cmd { return nil }

func (m *tuiModel) shutdown() {
	if m.cancel != nil {
		m.cancel()
	}
	close(m.stopEvents)
	if m.workerDone != nil {
		<-m.workerDone
	}
}

func (m *tuiModel) addField(name, label, value string, endpoint bool) {
	in := textinput.New()
	in.SetValue(value)
	in.CharLimit = 2048
	m.fields = append(m.fields, tuiField{name: name, label: label, input: in, endpoint: endpoint})
}
func (m *tuiModel) newForm(mode string) {
	m.mode, m.screen, m.focus, m.message = mode, "form", 0, ""
	m.fields = nil
	if mode == "opencode" {
		m.addField("target_model", "OpenCode model (opencode/<model-id>)", "", false)
		m.addField("opencode_bin", "OpenCode executable", "opencode", false)
	} else if mode == "uncover" {
		m.addField("target_url", "Endpoint URL (blank inherits config/environment)", "", true)
		m.addField("target_model", "Model", "", false)
		m.addField("target_transport", "Transport (auto, openai, anthropic, gemini)", "", false)
		m.addField("target_profile", "Profile", "", false)
		m.addField("target_key", "API key environment variable name", "", false)
	} else {
		m.addField("target_url", "Target endpoint URL", "", true)
		m.addField("both_model", "Model for both endpoints", "", false)
		m.addField("target_model", "Target model override", "", false)
		m.addField("target_transport", "Target transport override", "", false)
		m.addField("target_profile", "Target profile", "", false)
		m.addField("target_key", "Target API key environment variable name", "", false)
		m.addField("official_enabled", "Use an official baseline? (yes/no)", "no", false)
		m.addField("official_url", "Official endpoint URL", "", true)
		m.addField("official_model", "Official model override", "", false)
		m.addField("official_transport", "Official transport override", "", false)
		m.addField("official_profile", "Official profile", "", false)
		m.addField("official_key", "Official API key environment variable name", "", false)
		m.addField("both_transport", "Transport for both endpoints", "", false)
		m.addField("both_key", "API key environment variable name for both", "", false)
		m.addField("claim", "Claimed family (blank infers from target model)", "", false)
	}
	if mode == "opencode" {
		m.addField("max", "Maximum probe runs (OpenCode controls internal retries)", "12", false)
		m.addField("timeout", "Per-probe timeout (for example 60s)", "60s", false)
	} else {
		m.addField("config", "Config file path", "", false)
		m.addField("max", "Maximum requests per endpoint", "12", false)
		m.addField("timeout", "Per-request timeout (for example 30s)", "30s", false)
	}
	m.addField("confidence", "Minimum confidence (0 uses the default)", "0", false)
	m.fields[0].input.Focus()
}
func (m *tuiModel) value(name string) string {
	for _, f := range m.fields {
		if f.name == name {
			return strings.TrimSpace(f.input.Value())
		}
	}
	return ""
}
func (m *tuiModel) changeFocus(delta int) {
	m.fields[m.focus].input.Blur()
	m.focus = (m.focus + delta + len(m.fields)) % len(m.fields)
	m.fields[m.focus].input.Focus()
}
func (m *tuiModel) prepare() {
	m.message = ""
	max, err := strconv.Atoi(m.value("max"))
	if err != nil {
		m.message = "The probe limit must be a whole number."
		return
	}
	timeout, err := time.ParseDuration(m.value("timeout"))
	if err != nil {
		m.message = "Enter a timeout such as 30s or 2m."
		return
	}
	confidence, err := strconv.ParseFloat(m.value("confidence"), 64)
	if err != nil {
		m.message = "Minimum confidence must be a number between 0 and 1."
		return
	}
	common := commonFlags{configPath: m.value("config"), maxRequests: max, timeout: timeout, minConfidence: confidence}
	if m.mode == "opencode" {
		p, err := prepareOpenCode(m.value("opencode_bin"), m.value("target_model"), common)
		if err != nil {
			m.message = cleanUI(err.Error())
			return
		}
		m.prepared = p
		m.showReview()
		return
	}
	target := endpointFlags{endpoint: withScheme(m.value("target_url")), model: m.value("target_model"), transport: m.value("target_transport"), profile: m.value("target_profile"), apiKeyEnv: m.value("target_key")}
	official := endpointFlags{endpoint: withScheme(m.value("official_url")), model: m.value("official_model"), transport: m.value("official_transport"), profile: m.value("official_profile"), apiKeyEnv: m.value("official_key")}
	enabled := false
	if m.mode == "audit" {
		switch strings.ToLower(m.value("official_enabled")) {
		case "yes", "y":
			enabled = true
		case "no", "n", "":
		default:
			m.message = "Use yes or no for the official baseline."
			return
		}
		for _, ef := range []*endpointFlags{&target, &official} {
			if ef.model == "" {
				ef.model = m.value("both_model")
			}
			if ef.transport == "" {
				ef.transport = m.value("both_transport")
			}
			if ef.apiKeyEnv == "" {
				ef.apiKeyEnv = m.value("both_key")
			}
		}
	}
	p, err := prepareCheck(m.mode, target, official, m.value("claim"), enabled, common)
	if err != nil {
		m.message = cleanUI(err.Error())
		if h := hintFor(err); h != "" {
			m.message += "\n" + h
		}
		return
	}
	m.prepared = p
	m.showReview()
}

func waitTUIEvent(events <-chan tea.Msg, stop <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-events:
			return msg
		case <-stop:
			return nil
		}
	}
}
func (m *tuiModel) start() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.cancelling, m.started = cancel, false, true
	m.runID++
	id, p, events, stop := m.runID, m.prepared, m.events, m.stopEvents
	m.targetSpent, m.officialSpent, m.screen = 0, 0, "running"
	done := make(chan struct{})
	m.workerDone = done
	go func() {
		defer close(done)
		r := p.execute(ctx, func(side string, count int) {
			select {
			case events <- tuiProgress{id, side, count}:
			default:
			}
		})
		select {
		case events <- tuiFinished{id, r}:
		case <-stop:
		}
	}()
	return waitTUIEvent(events, stop)
}

func (m *tuiModel) showText(screen, content string) {
	m.screen = screen
	m.viewport.Width = maxInt(1, m.width-2)
	m.viewport.Height = maxInt(1, m.height-5)
	m.viewport.SetContent(wrapUI(cleanUI(content), m.viewport.Width))
	m.viewport.GotoTop()
}
func (m *tuiModel) showRecord() {
	content := m.current.plain
	if m.viewMode == 1 {
		content = m.current.verbose
	}
	if m.viewMode == 2 {
		content = m.current.json
	}
	m.showText("result", content)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = maxInt(1, msg.Width), maxInt(1, msg.Height)
		switch m.screen {
		case "result":
			m.showRecord()
		case "explain":
			m.showText("explain", explainText)
		case "families":
			m.showFamilies()
		case "review":
			m.showReview()
		}
		return m, nil
	case tuiProgress:
		if m.screen != "running" || msg.id != m.runID {
			return m, nil
		}
		if msg.side == "target" {
			m.targetSpent = msg.requests
		} else {
			m.officialSpent = msg.requests
		}
		return m, waitTUIEvent(m.events, m.stopEvents)
	case tuiFinished:
		if m.screen != "running" || msg.id != m.runID {
			return m, nil
		}
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		msg.result.cancelled = msg.result.cancelled || m.cancelling
		m.current = makeTUIRecord(m.prepared, msg.result)
		m.history = append(m.history, m.current)
		m.prepared = nil
		m.viewMode = 0
		m.showRecord()
		return m, nil
	case tea.KeyMsg:
		key := msg.String()
		if m.width < 40 || m.height < 10 {
			if m.screen == "running" && (key == "esc" || key == "ctrl+c") && !m.cancelling {
				m.cancelling = true
				m.cancel()
			} else if m.screen != "running" && key == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.screen == "running" {
			if (key == "esc" || key == "ctrl+c") && !m.cancelling {
				m.cancelling = true
				m.cancel()
			}
			return m, nil
		}
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		switch m.screen {
		case "home":
			switch key {
			case "up", "k":
				m.menu = (m.menu + len(tuiMenuLabels) - 1) % len(tuiMenuLabels)
			case "down", "j":
				m.menu = (m.menu + 1) % len(tuiMenuLabels)
			case "q", "esc":
				return m, tea.Quit
			case "enter":
				switch m.menu {
				case 0:
					m.newForm("uncover")
				case 1:
					m.newForm("audit")
				case 2:
					m.newForm("opencode")
				case 3:
					m.viewMode = 0
					m.showFamilies()
				case 4:
					m.showText("explain", explainText)
				case 5:
					m.screen, m.historyIndex = "history", maxInt(0, len(m.history)-1)
				case 6:
					return m, tea.Quit
				}
			}
		case "form":
			switch key {
			case "esc":
				m.screen, m.message = "home", ""
			case "tab", "down":
				m.changeFocus(1)
			case "shift+tab", "up":
				m.changeFocus(-1)
			case "ctrl+s":
				m.prepare()
			case "enter":
				if m.focus == len(m.fields)-1 {
					m.prepare()
				} else {
					m.changeFocus(1)
				}
			default:
				var cmd tea.Cmd
				m.fields[m.focus].input, cmd = m.fields[m.focus].input.Update(msg)
				return m, cmd
			}
		case "review":
			switch key {
			case "esc", "n":
				m.prepared = nil
				m.screen = "form"
			case "y", "enter":
				return m, m.start()
			default:
				var cmd tea.Cmd
				m.viewport, cmd = m.viewport.Update(msg)
				return m, cmd
			}
		case "history":
			switch key {
			case "esc", "q":
				m.screen = "home"
			case "up", "k":
				if m.historyIndex > 0 {
					m.historyIndex--
				}
			case "down", "j":
				if m.historyIndex+1 < len(m.history) {
					m.historyIndex++
				}
			case "enter":
				if len(m.history) > 0 {
					m.current = m.history[m.historyIndex]
					m.viewMode = 0
					m.showRecord()
				}
			}
		case "result":
			switch key {
			case "esc", "q":
				m.screen = "home"
			case "v":
				m.viewMode = (m.viewMode + 1) % 3
				m.showRecord()
			case "j":
				m.viewMode = 2
				m.showRecord()
			default:
				var cmd tea.Cmd
				m.viewport, cmd = m.viewport.Update(msg)
				return m, cmd
			}
		case "families":
			switch key {
			case "esc", "q":
				m.screen = "home"
			case "j":
				m.viewMode = 1 - m.viewMode
				m.showFamilies()
			default:
				var cmd tea.Cmd
				m.viewport, cmd = m.viewport.Update(msg)
				return m, cmd
			}
		case "explain":
			if key == "esc" || key == "q" {
				m.screen = "home"
			} else {
				var cmd tea.Cmd
				m.viewport, cmd = m.viewport.Update(msg)
				return m, cmd
			}
		}
	default:
		if m.screen == "form" {
			var cmd tea.Cmd
			m.fields[m.focus].input, cmd = m.fields[m.focus].input.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m *tuiModel) showReview() {
	p := m.prepared
	var b strings.Builder
	if p.mode == "opencode" {
		fmt.Fprintf(&b, "OpenCode stealth test\nModel: %s\nUses the installed OpenCode CLI and your existing OpenCode login.\n", p.targetSpec.Model)
		fmt.Fprintf(&b, "\nUp to %d probe runs; timeout %s per probe.\n", p.common.maxRequests, p.common.timeout)
		fmt.Fprintln(&b, "OpenCode controls internal provider requests and retries; this is not a request cap.")
		fmt.Fprintln(&b, "Fresh sessions use private configuration with tools disabled.")
		fmt.Fprintln(&b, "Only continue if you are authorized to test this model; usage may be billable.")
		m.showText("review", b.String())
		return
	}
	if !p.targetHasKey && !isLoopback(p.targetSpec.Endpoint) {
		fmt.Fprintln(&b, "WARNING: target has no API key; continuing sends unauthenticated requests.")
	}
	if p.official != nil && !p.officialHasKey && !isLoopback(p.officialSpec.Endpoint) {
		fmt.Fprintln(&b, "WARNING: official endpoint has no API key; continuing sends unauthenticated requests.")
	}
	fmt.Fprintf(&b, "Target: %s\nModel: %s  Transport: %s\n", report.SanitizeURL(p.targetSpec.Endpoint), p.targetSpec.Model, p.targetSpec.Transport)
	if p.mode == "audit" {
		fmt.Fprintf(&b, "Claim: %s\n", p.claimed)
	}
	if p.official != nil {
		fmt.Fprintf(&b, "Official: %s\nModel: %s  Transport: %s\n", report.SanitizeURL(p.officialSpec.Endpoint), p.officialSpec.Model, p.officialSpec.Transport)
	}
	if p.mode == "audit" && !p.db.IsVerified(p.claimed) {
		fmt.Fprintln(&b, "This family is unverified; no requests will be sent.")
	} else {
		fmt.Fprintf(&b, "\nUp to %d billable requests per endpoint; timeout %s.\n", p.common.maxRequests, p.common.timeout)
	}
	fmt.Fprintln(&b, "Only continue if you are authorized to probe these endpoints.")
	m.showText("review", redactUI(b.String(), p))
}

func (m *tuiModel) showFamilies() {
	db, err := fingerprint.Load()
	if err != nil {
		m.showText("families", "Unable to load families: "+err.Error())
		return
	}
	var b bytes.Buffer
	if m.viewMode == 1 {
		_ = writeJSON(&b, db)
	} else {
		report.Families(&b, db)
	}
	m.showText("families", b.String())
}

func cleanUI(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
func redactUI(s string, p *preparedCheck) string {
	for _, c := range []*string{keyOf(p.target), keyOf(p.official)} {
		if c != nil && *c != "" {
			s = strings.ReplaceAll(s, *c, "[redacted]")
		}
	}
	return cleanUI(s)
}
func keyOf(c *probe.Client) *string {
	if c != nil {
		return &c.Spec.APIKey
	}
	return nil
}

// Redact JSON values after decoding so escaping in an API key cannot break the
// JSON document or bypass redaction. Field names are fixed by xlfp.
func redactJSON(s string, p *preparedCheck) string {
	var data any
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return `{"state":"error","error":"could not render JSON"}`
	}
	var clean func(any) any
	clean = func(v any) any {
		switch v := v.(type) {
		case string:
			return redactUI(v, p)
		case []any:
			for i := range v {
				v[i] = clean(v[i])
			}
			return v
		case map[string]any:
			for k := range v {
				v[k] = clean(v[k])
			}
			return v
		default:
			return v
		}
	}
	b, err := json.MarshalIndent(clean(data), "", "  ")
	if err != nil {
		return `{"state":"error","error":"could not render JSON"}`
	}
	return string(b) + "\n"
}

func makeTUIRecord(p *preparedCheck, r checkResult) tuiRecord {
	state := "completed"
	if r.cancelled {
		state = "cancelled"
	} else if r.err != nil {
		state = "failed"
	}
	title := fmt.Sprintf("%s — %s — %s", strings.ToUpper(p.mode), report.SanitizeURL(p.targetSpec.Endpoint), state)
	if p.mode == "opencode" {
		title = fmt.Sprintf("OPENCODE — %s — %s", p.targetSpec.Model, state)
	}
	var plain, verbose, jsonText string
	if r.cancelled && p.mode == "opencode" {
		partial := map[string]any{"state": "cancelled", "model": p.targetSpec.Model,
			"opencode_version": p.opencode.Version(), "probes_spent": r.targetSpent}
		if r.uncover != nil {
			partial["target_evidence"] = evidenceFor(*r.uncover)
		}
		if r.err != nil {
			partial["error"] = r.err.Error()
		}
		var b bytes.Buffer
		_ = writeJSON(&b, partial)
		jsonText = b.String()
		plain = fmt.Sprintf("CANCELLED — this check did not finish.\nProbe runs: %d\n\nAvailable partial evidence (not a completed verdict):\n%s", r.targetSpent, jsonText)
		verbose = plain
	} else if r.cancelled {
		var target, official *fingerprint.Result
		if r.uncover != nil {
			target = r.uncover
		}
		if r.audit != nil {
			target, official = &r.audit.Target, r.audit.Official
		}
		partial := struct {
			State            string              `json:"state"`
			TargetRequests   int                 `json:"target_requests"`
			OfficialRequests int                 `json:"official_requests"`
			TargetEvidence   *fingerprint.Result `json:"target_evidence,omitempty"`
			OfficialEvidence *fingerprint.Result `json:"official_evidence,omitempty"`
			Error            string              `json:"error,omitempty"`
		}{State: "cancelled", TargetRequests: r.targetSpent, OfficialRequests: r.officialSpent, TargetEvidence: target, OfficialEvidence: official}
		if r.err != nil {
			partial.Error = r.err.Error()
		}
		var b bytes.Buffer
		_ = writeJSON(&b, partial)
		jsonText = redactJSON(b.String(), p)
		plain = fmt.Sprintf("CANCELLED — this check did not finish.\nTarget requests: %d\nOfficial requests: %d\n\nAvailable partial evidence (not a completed verdict):\n%s", r.targetSpent, r.officialSpent, jsonText)
		verbose = plain
	} else {
		render := func(jsonOut, technical bool) string {
			copy := *p
			copy.common.jsonOut, copy.common.verbose = jsonOut, technical
			var out, errors bytes.Buffer
			_ = emitCheckResult(&copy, r, &out, &errors)
			if errors.Len() > 0 {
				if jsonOut {
					b, _ := json.MarshalIndent(map[string]string{"state": "error", "error": strings.TrimSpace(errors.String())}, "", "  ")
					return string(b)
				}
				return errors.String()
			}
			return out.String()
		}
		plain, verbose, jsonText = render(false, false), render(false, true), render(true, false)
	}
	return tuiRecord{redactUI(title, p), redactUI(plain, p), redactUI(verbose, p), redactJSON(jsonText, p)}
}

func wrapUI(text string, width int) string {
	return ansi.Hardwrap(text, maxInt(1, width), true)
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func fitUI(s string, width, height int) string {
	lines := strings.Split(wrapUI(cleanUI(s), maxInt(1, width)), "\n")
	if len(lines) > maxInt(1, height) {
		lines = lines[:maxInt(1, height)]
	}
	return strings.Join(lines, "\n")
}
func fieldValue(f tuiField, selected bool, width int) string {
	value := cleanUI(f.input.Value())
	hidden := false
	if f.endpoint && value != "" && (!selected || strings.ContainsAny(value, "@?#")) {
		hidden = strings.ContainsAny(value, "@?#")
		value = report.SanitizeURL(withScheme(value))
		if hidden {
			value += " [sensitive parts hidden]"
		}
	}
	runes := []rune(value)
	cursor := f.input.Position()
	if hidden {
		cursor = len(runes)
	}
	if cursor > len(runes) {
		cursor = len(runes)
	}
	if selected {
		runes = append(runes[:cursor], append([]rune("|"), runes[cursor:]...)...)
	}
	available := maxInt(5, width-4)
	start := 0
	if selected && cursor >= available {
		start = cursor - available + 2
	}
	if start > len(runes) {
		start = len(runes)
	}
	end := start + available
	if end > len(runes) {
		end = len(runes)
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(runes) {
		suffix = "…"
	}
	return prefix + string(runes[start:end]) + suffix
}

func (m *tuiModel) View() string {
	if m.width < 40 || m.height < 10 {
		message := "Resize the terminal to at least 40 × 10.\nCtrl+C quits."
		if m.screen == "running" {
			message = "Resize to at least 40 × 10.\nEsc or Ctrl+C cancels the check."
		}
		return fitUI(message, m.width, m.height)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "xlfp  |  model family diagnostics")
	fmt.Fprintln(&b, strings.Repeat("─", maxInt(1, minInt(m.width, 60))))
	switch m.screen {
	case "home":
		fmt.Fprintln(&b, "Choose a task:")
		visible := minInt(len(tuiMenuLabels), m.height-6)
		start := maxInt(0, m.menu-visible+1)
		for i := start; i < minInt(start+visible, len(tuiMenuLabels)); i++ {
			marker := "  "
			if i == m.menu {
				marker = "> "
			}
			fmt.Fprintln(&b, marker+tuiMenuLabels[i])
		}
		fmt.Fprintln(&b, "\n↑/↓ choose  •  Enter open  •  q quit")
	case "form":
		fmt.Fprintf(&b, "%s  |  field %d/%d\n", strings.ToUpper(m.mode), m.focus+1, len(m.fields))
		var errors []string
		if m.message != "" {
			errors = strings.Split(wrapUI("Error: "+cleanUI(m.message), m.width), "\n")
			limit := maxInt(1, m.height-7)
			if len(errors) > limit {
				errors = errors[:limit]
				errors[limit-1] = ansi.Truncate(errors[limit-1], m.width, "…")
			}
			for _, line := range errors {
				fmt.Fprintln(&b, line)
			}
		}
		visible := maxInt(1, (m.height-5-len(errors))/2)
		if visible > len(m.fields) {
			visible = len(m.fields)
		}
		start := m.focus - visible + 1
		if start < 0 {
			start = 0
		}
		if start+visible > len(m.fields) {
			start = len(m.fields) - visible
		}
		for i := start; i < start+visible; i++ {
			marker := "  "
			if i == m.focus {
				marker = "> "
			}
			fmt.Fprintf(&b, "%s%s\n  %s\n", marker, ansi.Truncate(m.fields[i].label, m.width-2, "…"), ansi.Truncate(fieldValue(m.fields[i], i == m.focus, m.width), m.width-2, "…"))
		}
		fmt.Fprintln(&b, "Tab/↑/↓ fields • Ctrl+S review • Esc")
		if m.mode == "opencode" {
			fmt.Fprintln(&b, "Uses your existing OpenCode login.")
		} else {
			fmt.Fprintln(&b, "Use environment/config for API keys.")
		}
	case "review":
		fmt.Fprintf(&b, "Review %s\n", m.prepared.mode)
		fmt.Fprintln(&b, m.viewport.View())
		fmt.Fprintln(&b, "↑/↓ scroll • Enter confirm • Esc edit")
	case "running":
		unit := "Target requests"
		if m.mode == "opencode" {
			unit = "Probe runs"
		}
		fmt.Fprintf(&b, "Running %s…\n\n%s: %d / %d\n", m.mode, unit, m.targetSpent, m.prepared.common.maxRequests)
		if m.prepared.official != nil {
			fmt.Fprintf(&b, "Official requests: %d / %d\n", m.officialSpent, m.prepared.common.maxRequests)
		}
		if m.cancelling {
			fmt.Fprintln(&b, "\nCancelling; waiting for the current request…")
		} else {
			fmt.Fprintln(&b, "\nEsc or Ctrl+C: cancel the check")
		}
	case "history":
		fmt.Fprintln(&b, "Session results:")
		if len(m.history) == 0 {
			fmt.Fprintln(&b, "  No checks yet.")
		}
		visible := maxInt(1, m.height-6)
		start := m.historyIndex - visible + 1
		if start < 0 {
			start = 0
		}
		end := start + visible
		if end > len(m.history) {
			end = len(m.history)
		}
		for i := start; i < end; i++ {
			marker := "  "
			if i == m.historyIndex {
				marker = "> "
			}
			fmt.Fprintln(&b, marker+ansi.Truncate(m.history[i].title, m.width-2, "…"))
		}
		fmt.Fprintln(&b, "\n↑/↓ choose  •  Enter open  •  Esc back")
	case "result", "families", "explain":
		if m.screen == "result" {
			fmt.Fprintln(&b, ansi.Truncate(m.current.title+"  |  "+[]string{"plain", "technical", "JSON"}[m.viewMode], m.width, "…"))
		}
		if m.screen == "families" {
			fmt.Fprintf(&b, "Families  |  %s\n", []string{"plain", "JSON"}[m.viewMode])
		}
		if m.screen == "explain" {
			fmt.Fprintln(&b, "How xlfp works")
		}
		fmt.Fprintln(&b, m.viewport.View())
		if m.screen == "result" {
			fmt.Fprintln(&b, "↑/↓ scroll • v views • j JSON • Esc")
		} else if m.screen == "families" {
			fmt.Fprintln(&b, "↑/↓ scroll • j JSON • Esc home")
		} else {
			fmt.Fprintln(&b, "↑/↓ scroll  •  Esc home")
		}
	}
	return fitUI(b.String(), m.width, m.height)
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
