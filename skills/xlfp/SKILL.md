---
name: xlfp
description: Identify the true model family behind an unknown or "stealth" LLM endpoint, and audit third-party providers for silent model substitution, using the xlfp CLI (tokenizer fingerprinting from reported token usage). Use when the user asks what model an endpoint really is, whether a provider swapped or substituted models, or wants to verify a claimed model family.
license: GPL-3.0-only
compatibility: Requires xlfp on PATH and network access. HTTP checks use API keys from environment variables; OpenCode checks also require the installed OpenCode v1 client and its existing login.
---

# xlfp — LLM endpoint fingerprinting

`xlfp` sends up to 12 tiny probe requests to an LLM endpoint and
matches the reported prompt-token counts against known tokenizer
families (OpenAI, Qwen, DeepSeek, Mistral, GLM, MiMo, Kimi verified;
Google/Claude scored as unverified hints). It provides these checks:

- `xlfp uncover` — which family is this endpoint, really?
- `xlfp opencode` — which family is this OpenCode stealth model, through OpenCode?
- `xlfp audit` — does this endpoint match the family the provider claims?
- `xlfp tokprobe` — which tokenizer counts for this endpoint, checked
  against the model name? Differential counts with an open-set
  verdict (`exact`|`related`|`unknown`); use it as an independent
  second tokenizer opinion next to `uncover`, especially when
  template overhead may be distorting the classic fit.
- `xlfp numbers` — behavioral ranking from first-instinct integer
  replies against a reference bank. Needs a populated bank
  (`--bank FILE`); the embedded bank ships empty, so expect
  `insufficient_bank` until the user collects samples
  (`xlfp sample` → `xlfp enroll` → `xlfp retrain`, official
  endpoints only). A numbers round costs `--count` (default 3)
  long generations per round — much more than the tiny token
  probes; say so before running it. Rankings are closed-set
  within the bank and never prove an exact model; `not_model_reply`
  means the numbers look program-generated or sorted. Offline:
  `xlfp numbers --input FILE` re-scores saved reports, answer
  arrays, batches, or pasted replies with zero requests — prefer
  this when the user already has replies.

Hard limits you must respect when reporting: results are
**family-level evidence, never proof of an exact model/checkpoint**;
audit detects family substitution only, never quantization or
degradation; Qwen2.5, MiMo-7B, and Kimi-Dev share one tokenizer and
cannot be separated; Google/Claude can never be verdicts. Missing or
implausible usage yields *inconclusive*, a valid final answer, not a
failure to retry around. Plausible fabricated counts can fool the
fingerprint; confidence measures fit and does not authenticate a
provider's reporting.

## 0. Make sure xlfp exists

```bash
xlfp version || curl -fsSL https://raw.githubusercontent.com/tyk-swe/xlfp/main/install.sh | sh
```

Windows: `irm https://raw.githubusercontent.com/tyk-swe/xlfp/main/install.ps1 | iex`.
From a clone: `go build -o xlfp .`. If installation is impossible,
say so — never invent fingerprint results.

## 1. Safety gates (before every run)

1. **Authorization.** Only probe endpoints the user owns or is
   authorized to test. If that is not explicit, ask first.
2. **Cost.** Each check sends up to `--max-requests` (default 12)
   tiny, normally billable requests per endpoint, visible to the
   provider. Tell the user before the first run; don't raise the cap
   silently.
   OpenCode checks instead use `--max-probes` (default 12) to limit CLI
   runs. OpenCode controls internal calls and retries, so provider-request
   counts cannot be capped or reported by xlfp.
3. **Keys.** Never ask the user to paste an API key into the chat,
   and never put a key in a command (xlfp has no such flag). Ask the
   user to export it in the shell environment you run in, then check
   presence without printing it: `test -n "$XLFP_API_KEY" && echo set`.
   OpenCode checks use the existing OpenCode login (`opencode auth login`)
   instead of xlfp keys or endpoint profiles.
   - Unknown/third-party endpoints: `XLFP_API_KEY`, or name any
     variable with `--api-key-env NAME`.
   - Audit sides: `XLFP_TARGET_API_KEY`, `XLFP_OFFICIAL_API_KEY`,
     `--target-api-key-env`, `--official-api-key-env`.
   - `OPENAI_API_KEY`/`ANTHROPIC_API_KEY`/`GEMINI_API_KEY` are only
     ever sent to that provider's official host, by design — don't
     try to work around this.

## 2. Run (always `--json`)

```bash
# Unknown / stealth endpoint:
xlfp uncover --endpoint https://provider.example --model stealth-model-x --json

# OpenCode stealth model (requires the real installed OpenCode client):
xlfp opencode --model opencode/big-pickle --max-probes 12 --json

# Provider claims a family:
xlfp audit --target-url https://provider.example --model deepseek-chat --claim deepseek --json

# Stronger audit with a live official baseline (preferred when available):
xlfp audit --target-url https://provider.example --official-url https://api.deepseek.com \
  --model deepseek-chat --claim deepseek --json
```

Notes: `--claim` is inferred from the model name when omitted.
Native Anthropic/Gemini APIs behind non-official hosts need
`--transport anthropic|gemini`. Provenance of the database:
`xlfp families --json`.

OpenCode free-tier endpoints require OpenCode. Never use `uncover` or
`audit` against `opencode.ai` or its subdomains, or impersonate OpenCode
headers. xlfp blocks these hosts for paid models too. Use `xlfp opencode`
with an available selector from `opencode models opencode`; model names
rotate. It supports family identification only, not audits/baselines.
The v1 JSON event contract is required (reference v1.19.4); if unsupported,
report the setup issue without falling back to HTTP. `--opencode-bin PATH`
selects the executable, and `--timeout` defaults to 60s per probe.

## 3. Interpret

Exit codes: `0` identified/match, `1` operational or usage error,
`2` inconclusive, `3` audit mismatch. For `tokprobe`, `0` means any
verdict (`exact`, `related`, or `unknown` = unlisted tokenizer);
the claim check (`claim.status`: consistent|inconsistent|unclear|
unchecked) is reference information and does not change the exit
code. For `numbers`, `0` means every round produced a ranking
(candidate `confidence` exists only when `calibrated` is true —
never quote a confidence from an uncalibrated ranking), `2` covers
inconclusive and `not_model_reply` rounds. `numbers` reason codes:
`insufficient_bank` (collect samples first; do not guess),
`insufficient_answers`, `not_model_reply`.

`uncover` JSON: `status` (`identified`|`inconclusive`),
`top_family`, `rankings[]` (verified families with `confidence`),
`unverified_rankings[]` (hints only), `reason` (technical prose),
`reason_code` (stable cause), `requests_spent`, `observations[]`.

`opencode` JSON has the same evidence fields, plus `model`,
`opencode_version`, and `probes_spent`. It omits `requests_spent` because
provider-call counts are unknown. Exit codes match `uncover`.

`audit` JSON: `verdict` (`match`|`mismatch`|`inconclusive`),
`claimed_family`, `reason_code`, nested `target`/`official`
fingerprints, and `divergence` when both sides identified
(overhead-normalized count differences — a signal only, never a
quality or quantization claim).

Common `reason_code` values:

| Code | Meaning | Next step |
|---|---|---|
| `insufficient_counts` | endpoint returned too little/no usage | check model/URL/key; try `--transport` for native gateways |
| `unreliable_counts`, `scaled_counts` | usage looks estimated/fabricated | report as inconclusive-by-design; don't retry-hammer |
| `poor_fit` | no known family matches | may be an unlisted family; show `rankings` as hints |
| `shared_tokenizer` | tie (e.g. Qwen, MiMo, and Kimi-Dev) | report all tied families as inseparable |
| `unverified_best` | closest match is Google/Claude | hint only, never a verdict |
| `low_confidence` | evidence too close | one re-run is reasonable; audit with official baseline is stronger |
| `unverified_claim` (audit) | claimed family can't be judged | use `uncover` for hints; no requests were spent |
| `official_in_doubt`, `official_failed` (audit) | baseline unusable | check official URL/model/key |

Exit `1` errors print a `Fix:` hint on stderr (missing endpoint,
model, or empty key variable) — follow it or relay it to the user.

## 4. Report to the user

- State the family and confidence as evidence: "fingerprints as
  DeepSeek family (98% confidence)", never "it is DeepSeek-V3".
- For `MISMATCH`: say it is strong tokenizer evidence of a different
  family and explicitly **not proof of fraud**; recommend verifying
  (ideally with `--official-url`) before any accusation.
- For inconclusive: translate `reason_code` into one plain sentence
  and give the concrete next step from the table. Do not soften an
  inconclusive into a guess, and do not present
  `unverified_rankings` or a tied family as the answer.
- Mention `requests_spent` so the user knows what the check cost.
- Never print API keys; xlfp output never contains them — keep it
  that way when quoting output.
