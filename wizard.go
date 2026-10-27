package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/tyk-swe/xlfp/pkg/config"
	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

// runWizard is the guided first-run experience for bare `xlfp` on a
// terminal. It only collects answers and then dispatches to the regular
// uncover/audit commands, so every validation and security rule of the
// flag path applies unchanged. The wizard never reads an API key: keys
// stay in the environment, and when none resolves it explains the
// export step instead of asking for a paste.
func runWizard(stdin io.Reader, stdout, stderr io.Writer) int {
	sc := bufio.NewScanner(stdin)
	ask := func(prompt string) (string, bool) {
		fmt.Fprint(stdout, prompt)
		if !sc.Scan() {
			return "", false
		}
		return strings.TrimSpace(sc.Text()), true
	}
	abort := func() int {
		fmt.Fprintln(stdout, "\nStopped. Nothing was sent.")
		return exitError
	}

	fmt.Fprintln(stdout, "xlfp — find out which AI model family is really behind an endpoint.")
	fmt.Fprintln(stdout, "Answer a few questions (Ctrl-C stops at any time; nothing is sent until you confirm).")
	fmt.Fprintln(stdout)

	choice, ok := ask("What do you want to do?\n  1) Find out which model family an endpoint really is\n  2) Check whether a provider really serves the family it claims\nChoose 1 or 2: ")
	if !ok {
		return abort()
	}
	var mode string
	switch strings.ToLower(choice) {
	case "1", "uncover", "find", "identify":
		mode = "uncover"
	case "2", "audit", "check":
		mode = "audit"
	default:
		fmt.Fprintf(stdout, "Sorry, %q is not 1 or 2.\n", choice)
		return abort()
	}

	endpoint, ok := ask("\nPaste the API address (endpoint URL), e.g. https://provider.example\nEndpoint: ")
	if !ok || endpoint == "" {
		return abort()
	}
	endpoint = withScheme(endpoint)

	model, ok := ask("\nModel name exactly as the provider calls it, e.g. deepseek-chat\nModel: ")
	if !ok || model == "" {
		return abort()
	}

	var claim, official string
	if mode == "audit" {
		db, err := fingerprint.Load()
		if err != nil {
			return fail(stderr, err)
		}
		claim, ok = ask(fmt.Sprintf("\nWhich family does the provider claim? Known families: %s.\nLeave blank to guess from the model name.\nClaimed family: ", strings.Join(db.Families(), ", ")))
		if !ok {
			return abort()
		}
		if claim != "" && db.NormalizeFamily(claim) == "" {
			fmt.Fprintf(stdout, "%q is not a family this build knows.\n", claim)
			return abort()
		}
		official, ok = ask("\nOptional: the official API address to compare against in the same run,\ne.g. https://api.deepseek.com. Leave blank to skip.\nOfficial endpoint: ")
		if !ok {
			return abort()
		}
		official = withScheme(official)
	}

	// Key check: resolve exactly as the real command will, so a missing
	// key is explained before anything is sent.
	cfg, err := config.Load("")
	if err != nil {
		return fail(stderr, err)
	}
	purpose := ""
	if mode == "audit" {
		purpose = "target"
	}
	spec, err := cfg.Resolve(config.Overrides{Endpoint: endpoint, Model: model, Purpose: purpose})
	if err != nil {
		return failWithHint(stderr, err)
	}
	if spec.APIKey == "" && !isLoopback(endpoint) {
		fmt.Fprintln(stdout, "\nOne more thing: xlfp needs your API key for that endpoint, but you should")
		fmt.Fprintln(stdout, "never paste it here. Set it in your environment instead:")
		fmt.Fprintln(stdout, "\n  export XLFP_API_KEY='your-key-for-that-endpoint'")
		fmt.Fprintln(stdout, "then run `xlfp` again. (Keys like OPENAI_API_KEY only work for that")
		fmt.Fprintln(stdout, "provider's official address, so they can't leak to unknown endpoints.)")
		ans, ok := ask("If this endpoint needs no key at all, continue anyway? [y/N]: ")
		if !ok || !isYes(ans, false) {
			fmt.Fprintf(stdout, "\nWhen you're ready, this command does the same thing:\n  %s\n", equivalentCommand(mode, endpoint, model, claim, official))
			return abort()
		}
	}
	if mode == "audit" && official != "" {
		oSpec, oErr := cfg.Resolve(config.Overrides{Endpoint: official, Model: model, Purpose: "official"})
		if oErr == nil && oSpec.APIKey == "" && !isLoopback(official) {
			fmt.Fprintln(stdout, "\nNote: no key found for the official endpoint either. Set XLFP_OFFICIAL_API_KEY,")
			fmt.Fprintln(stdout, "or the provider's own variable (e.g. DEEPSEEK_API_KEY via --official-api-key-env")
			fmt.Fprintln(stdout, "in the flag form), if it needs one. Continuing anyway; the official side may fail.")
		}
	}

	where := "that endpoint"
	per := "It"
	if official != "" {
		where = "each endpoint"
		per = "Each check"
	}
	ans, ok := ask(fmt.Sprintf("\n%s will send up to 12 tiny test requests to %s. They count as normal API\nusage (may cost a fraction of a cent) and the provider can see them.\nOnly continue for endpoints you are authorized to test.\nContinue? [Y/n]: ", per, where))
	if !ok || !isYes(ans, true) {
		return abort()
	}
	fmt.Fprintln(stdout)

	if mode == "uncover" {
		return cmdUncover([]string{"--endpoint", endpoint, "--model", model}, stdout, stderr)
	}
	args := []string{"--target-url", endpoint, "--model", model}
	if claim != "" {
		args = append(args, "--claim", claim)
	}
	if official != "" {
		args = append(args, "--official-url", official)
	}
	return cmdAudit(args, stdout, stderr)
}

func withScheme(endpoint string) string {
	if endpoint == "" || strings.Contains(endpoint, "://") {
		return endpoint
	}
	return "https://" + endpoint
}

func isLoopback(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isYes(ans string, defaultYes bool) bool {
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "":
		return defaultYes
	case "y", "yes":
		return true
	}
	return false
}

func equivalentCommand(mode, endpoint, model, claim, official string) string {
	if mode == "uncover" {
		return fmt.Sprintf("xlfp uncover --endpoint %s --model %s", endpoint, model)
	}
	cmd := fmt.Sprintf("xlfp audit --target-url %s --model %s", endpoint, model)
	if claim != "" {
		cmd += " --claim " + claim
	}
	if official != "" {
		cmd += " --official-url " + official
	}
	return cmd
}
