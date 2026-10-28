# Changelog

## 0.1.6

- Add an interactive terminal dashboard for checks, progress, cancellation,
  and session history, with the line-by-line wizard retained as a fallback.
- Add `xlfp opencode` and guided OpenCode stealth testing through the
  installed OpenCode v1 client, using fresh sessions and structured usage.
- Report OpenCode probe runs separately from HTTP request counts, and
  block direct HTTP probing of OpenCode-hosted endpoints.
- Verify resolved OpenCode configuration before probing, rejecting managed
  or organization overrides that change the isolated probe settings.

## 0.1.5

This release strengthens the evidence and credential handling behind
`xlfp uncover` and `xlfp audit`.

- Bind config-sourced credentials to their endpoint origin, restrict
  provider-standard keys to official HTTPS hosts on the default port,
  and fail closed when an explicitly named key variable is empty.
- Redact API keys in provider errors, including non-JSON responses, and
  tighten configuration and endpoint validation.
- Reject invalid token usage and failed observations as ranking evidence;
  account for Anthropic cache input tokens and improve ranking stability
  and shared-tokenizer tie handling.
- Harden installer behavior and preserve useful diagnostics and request
  accounting in more failure cases.

**Known limitation:** An official audit baseline that fails after
collecting enough usable counts can still influence a verdict. Inspect
the official result using `--json` or `--verbose` and rerun if probing
stopped early. [PR #3](https://github.com/tyk-swe/xlfp/pull/3) contains
a pending fix.

For earlier versions, see [GitHub Releases](https://github.com/tyk-swe/xlfp/releases).
