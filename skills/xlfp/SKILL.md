---
name: xlfp
description: Identify the true model family behind an unknown or "stealth" LLM endpoint, and audit third-party providers for silent model substitution, using the xlfp CLI (tokenizer fingerprinting from reported token usage). Use when the user asks what model an endpoint really is, whether a provider swapped or substituted models, or wants to verify a claimed model family.
license: GPL-3.0-only
compatibility: Requires the xlfp binary on PATH (install commands in the body), network access to the endpoint being checked, and an API key provided through environment variables.
---

# xlfp — LLM endpoint fingerprinting

`xlfp` sends up to 12 tiny probe requests to an LLM endpoint and
matches the reported prompt-token counts against known tokenizer
families (OpenAI, Qwen, DeepSeek, Mistral, GLM, MiMo verified;
Google/Claude scored as unverified hints). It answers two questions:

- `xlfp uncover` — which family is this endpoint, really?
- `xlfp audit` — does this endpoint match the family the provider claims?

Hard limits you must respect when reporting: results are
**family-level evidence, never proof of an exact model/checkpoint**;
audit detects family substitution only, never quantization or
degradation; Qwen and MiMo share one tokenizer and cannot be
separated; Google/Claude can never be verdicts. Missing or implausible
usage yields *inconclusive*, a valid final answer, not a failure to retry
around. Plausible fabricated counts can fool the fingerprint; confidence
measures fit and does not authenticate a provider's reporting.

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
3. **Keys.** Never ask the user to paste an API key into the chat,
   and never put a key in a command (xlfp has no such flag). Ask the
   user to export it in the shell environment you run in, then check
   presence without printing it: `test -n "$XLFP_API_KEY" && echo set`.
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

## 3. Interpret

Exit codes: `0` identified/match, `1` operational or usage error,
`2` inconclusive, `3` audit mismatch.

`uncover` JSON: `status` (`identified`|`inconclusive`),
`top_family`, `rankings[]` (verified families with `confidence`),
`unverified_rankings[]` (hints only), `reason` (technical prose),
`reason_code` (stable cause), `requests_spent`, `observations[]`.

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
| `shared_tokenizer` | tie (e.g. Qwen vs MiMo) | report both families as inseparable |
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
