# Changelog

## Unreleased

- Add `xlfp tokprobe`: differential (wrapper-minus-baseline) token
  counts with a Bayesian open-set verdict (exact / related /
  unknown), adaptive probe selection, baseline-drift detection, and
  a claim check against the requested model.
- Add `xlfp numbers`: behavioral fingerprinting from first-instinct
  integer replies — randomized challenges in five languages,
  model-reply sequence check, bank ranking with optional calibrated
  confidence, multi-round runs, and offline scoring via `--input`
  (saved reports, answer arrays, sampling batches, pasted text).
  The embedded number bank ships empty; verdicts stay
  `insufficient_bank` until samples are collected.
- Add the bank pipeline `xlfp sample` / `xlfp enroll` /
  `xlfp retrain` (fixed 36-challenge sampling set, validated
  enrollment, leave-one-out fitted limits and calibration), plus
  `tools/train_numprint.py` for optional LDA parameters.
- Add streaming (SSE) reply capture and the OpenAI Responses
  transport (`--transport responses`) to the probe client, with
  reasoning-effort and service-tier passthrough for the new
  commands. Key-safety rules are unchanged.
- The two new signals are an independent clean-room implementation
  from published concepts; see docs/clean-room.md.

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
