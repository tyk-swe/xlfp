// Package opencode runs tokenizer probes through the genuine OpenCode CLI.
// It never calls OpenCode provider endpoints or reads authentication files.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

var modelPattern = regexp.MustCompile(`^opencode/[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
var versionPattern = regexp.MustCompile(`^(?:v)?1\.[0-9]+\.[0-9]+(?:[-+][a-zA-Z0-9.-]+)?$`)

// Client owns one isolated workspace shared by fresh, independent sessions.
// It is used sequentially and must be closed after Init, including on failure.
type Client struct {
	model, binary string
	timeout       time.Duration
	dir           string
	env           []string
	config        []byte
	version       string
	attempts      int
}

// New validates setup without starting a process or making network requests.
// The model is explicit: xlfp never guesses which rotating free model to test.
func New(binary, model string, timeout time.Duration) (*Client, error) {
	if !modelPattern.MatchString(model) {
		return nil, errors.New("OpenCode requires --model opencode/<model-id>; list available models with `opencode models opencode`")
	}
	if binary == "" {
		binary = "opencode"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, errors.New("OpenCode executable not found; install it from https://opencode.ai/docs or set --opencode-bin PATH")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, errors.New("cannot resolve OpenCode executable path")
	}
	if timeout <= 0 {
		return nil, errors.New("--timeout must be positive")
	}
	return &Client{model: model, binary: path, timeout: timeout}, nil
}

func (c *Client) Model() string    { return c.model }
func (c *Client) Version() string  { return c.version }
func (c *Client) Attempts() int    { return c.attempts }
func (c *Client) String() string   { return "OpenCode client (credentials hidden)" }
func (c *Client) GoString() string { return c.String() }

// Init is called only after confirmation. Authentication remains in OpenCode's
// normal data location. Effective configuration must match the private probe
// settings, including after OpenCode applies managed/organization overrides.
func (c *Client) Init(ctx context.Context) error {
	if c.dir != "" {
		return errors.New("OpenCode runner already initialized")
	}
	dir, err := os.MkdirTemp("", "xlfp-opencode-")
	if err != nil {
		return errors.New("cannot create private OpenCode workspace")
	}
	c.dir = dir
	configDir := filepath.Join(dir, "config", "opencode")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return errors.New("cannot create private OpenCode configuration")
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   c.model, "enabled_providers": []string{"opencode"},
		"autoupdate": false, "share": "disabled", "snapshot": false,
		"permission": map[string]string{"*": "deny"},
		"compaction": map[string]bool{"auto": false, "prune": false},
		"agent": map[string]any{"xlfp": map[string]any{
			"mode": "primary", "description": "Tokenizer probe",
			"prompt": "Reply with exactly OK. Do not call tools.", "steps": 1,
			"permission": map[string]string{"*": "deny"},
			"options":    map[string]any{},
		}},
	}
	raw, _ := json.Marshal(config)
	c.config = raw
	overrides := map[string]string{
		"PWD": dir, "XDG_CONFIG_HOME": filepath.Join(dir, "config"),
		// OpenCode scans ~/.opencode even with project config disabled.
		// Its home override isolates that scan without moving XDG auth data.
		"OPENCODE_TEST_HOME": dir,
		"OPENCODE_CONFIG":    "", "OPENCODE_CONFIG_DIR": configDir,
		"OPENCODE_CONFIG_CONTENT": string(raw), "OPENCODE_PERMISSION": `{"*":"deny"}`,
		"OPENCODE_DISABLE_PROJECT_CONFIG": "1", "OPENCODE_DISABLE_AUTOUPDATE": "1",
		"OPENCODE_DISABLE_DEFAULT_PLUGINS": "1", "OPENCODE_DISABLE_LSP_DOWNLOAD": "1",
		"OPENCODE_DISABLE_AUTOCOMPACT": "1", "OPENCODE_DISABLE_CLAUDE_CODE": "1",
		"OPENCODE_DISABLE_CLAUDE_CODE_PROMPT": "1", "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS": "1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS": "1", "OPENCODE_PURE": "1",
		"OPENCODE_AUTO_SHARE": "false",
	}
	c.env = childEnvironment(os.Environ(), overrides)
	stdout, err := c.run(ctx, []string{"--version"}, "", 4096, false)
	if err != nil {
		return fmt.Errorf("checking OpenCode version: %w", err)
	}
	version := strings.TrimSpace(string(stdout))
	if !versionPattern.MatchString(version) {
		return errors.New("unsupported OpenCode version; this runner requires the OpenCode v1 JSON event format (reference v1.19.4)")
	}
	if err := c.checkConfiguration(ctx); err != nil {
		return err
	}
	c.version = version
	return nil
}

// Close removes only the temporary workspace owned by this runner.
func (c *Client) Close() error {
	if c.dir == "" {
		return nil
	}
	err := os.RemoveAll(c.dir)
	c.dir, c.env, c.config = "", nil, nil
	if err != nil {
		return errors.New("cannot remove private OpenCode workspace")
	}
	return nil
}

func childEnvironment(parent []string, overrides map[string]string) []string {
	out := make([]string, 0, len(parent)+len(overrides))
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		// Environment names are case insensitive on Windows.
		if _, replaced := overrides[strings.ToUpper(name)]; !replaced {
			out = append(out, entry)
		}
	}
	for name, value := range overrides {
		out = append(out, name+"="+value)
	}
	return out
}

// SendProbe launches one fresh session. Remaining limits CLI runs, not the
// provider calls or retries made inside OpenCode. No xlfp retry is performed.
func (c *Client) SendProbe(ctx context.Context, p fingerprint.Probe, remaining int) (fingerprint.Observation, error) {
	o := fingerprint.Observation{ProbeID: p.ID}
	if remaining <= 0 {
		return o, errors.New("OpenCode probe budget exhausted")
	}
	if c.dir == "" || c.version == "" {
		return o, errors.New("OpenCode runner is not initialized")
	}
	// Each CLI run reloads configuration, so recheck immediately before every
	// probe as well as during Init. Config inspection never consumes the budget.
	if err := c.checkConfiguration(ctx); err != nil {
		return o, err
	}
	args := []string{"run", "--format", "json", "--model", c.model,
		"--agent", "xlfp", "--title", "xlfp tokenizer probe", "--dir", c.dir}
	stdout, err := c.run(ctx, args, p.Text, 4<<20, true)
	if err != nil {
		return o, err
	}
	tokens, err := parseEvents(stdout)
	if err != nil {
		return o, err
	}
	o.PromptTokens, o.HasUsage = tokens, true
	return o, nil
}

// boundedOutput cancels a child that exceeds its output limit. Child output is
// never echoed: even a structured provider error can contain credentials.
type boundedOutput struct {
	// Keep this named: embedding bytes.Buffer would promote ReadFrom and let
	// io.Copy bypass Write, defeating the limit for subprocess pipe output.
	buffer   bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		b.overflow = true
		b.cancel()
		return len(p), nil
	}
	return b.buffer.Write(p)
}

func (c *Client) run(ctx context.Context, args []string, input string, limit int, probe bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, c.binary, args...)
	cmd.Dir, cmd.Env = c.dir, c.env
	cmd.Stdin = strings.NewReader(input)
	stdout := &boundedOutput{limit: limit, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, io.Discard
	cmd.WaitDelay = time.Second
	afterStart, cleanup, err := configureProcess(cmd)
	if err != nil {
		return nil, errors.New("cannot configure OpenCode process isolation")
	}
	defer cleanup()
	if err := cmd.Start(); err != nil {
		return nil, errors.New("cannot start OpenCode; check --opencode-bin and executable permissions")
	}
	if probe {
		c.attempts++
	}
	if err := afterStart(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.New("cannot isolate OpenCode subprocess tree")
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stdout.overflow {
		return nil, errors.New("OpenCode output exceeded the size limit; no token evidence accepted")
	}
	if runCtx.Err() != nil {
		return nil, errors.New("OpenCode timed out; increase --timeout if needed")
	}
	if err != nil {
		return nil, errors.New("OpenCode failed; check `opencode auth login`, model availability with `opencode models opencode`, and v1 JSON support")
	}
	return stdout.buffer.Bytes(), nil
}
