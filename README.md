# xlfp — xo llm fingerprinter

**Find out which AI model family is really behind an API endpoint.**

Some endpoints hide which model they run, and some providers quietly
serve a different (often cheaper) model than the one they advertise.
`xlfp` sends a few tiny test messages, looks at how the endpoint
counts tokens, and matches that pattern against known model families
(DeepSeek, Qwen, OpenAI, Mistral, GLM, MiMo, …). No knowledge of
tokenizers needed: the output tells you the likely family, what it
means, and what to do next — or honestly says "inconclusive."

What it can tell you:

- Which model **family** an endpoint most likely is, with evidence.
- Whether a provider's endpoint matches the family it claims.

What it can't:

- The exact model version. Token counts can't prove that.
- Whether a same-family model was secretly shrunk or degraded.
- Authenticate provider-reported token counts. Missing or implausible
  counts can be rejected, but deliberately forged counts that match a
  known tokenizer can fool the fingerprint, including its confidence.

## Install

One line (Linux/macOS):

```bash
curl -fsSL https://raw.githubusercontent.com/tyk-swe/xlfp/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/tyk-swe/xlfp/main/install.ps1 | iex
```

This downloads the right binary for your machine from the latest
GitHub Release, verifies its SHA-256 checksum, and installs it to
`~/.local/bin` (Windows: `%LOCALAPPDATA%\xlfp`). Set `XLFP_VERSION`
to pin a release (e.g. `v0.1.4`) or `XLFP_INSTALL_DIR` to change the
destination.

With Go 1.23+ you can also install or build from source:

```bash
go install github.com/tyk-swe/xlfp@latest
# or, in a clone:
go build -o xlfp .
```

There are no Go module dependencies. Check it works:

```bash
xlfp version
xlfp families        # the families this build can recognize
xlfp explain         # two-minute plain-words guide
```

Hostnames such as `provider.example` below are placeholders — replace
them with real endpoints you are authorized to probe.

## Your first check (2 minutes)

**1. Give xlfp your API key — via the environment, never a flag.**

```bash
export XLFP_API_KEY='the key for that endpoint'
```

`XLFP_API_KEY` is your explicit "yes, probe this endpoint" key. Keys
like `OPENAI_API_KEY` are only ever sent to that provider's official
address, so checking a stranger's endpoint can never leak them.

**2. Run it.** In a terminal, just run `xlfp` and answer the
questions. Or use the direct form:

```bash
xlfp uncover --endpoint https://provider.example --model stealth-model-x
```

**3. Read the answer.** Sample output (values vary by endpoint):

```text
Endpoint: https://provider.example  Model: stealth-model-x  Transport: openai
Sent 8 tiny test requests and compared how the endpoint counts tokens.

Result: IDENTIFIED — this endpoint behaves like the DeepSeek (deepseek) family.
Confidence: very high (98%)

What this means: the way this endpoint splits text into tokens matches that
family. That is strong evidence about the model family, but token counts
cannot prove an exact model version or checkpoint.
```

If the evidence isn't trustworthy enough, you get `INCONCLUSIVE` plus
a plain-English reason and concrete things to try. Add `--verbose`
for the technical evidence, or `--json` for scripts and agents.

### Check a provider's claim (audit)

```bash
# "You say this is DeepSeek. Is it?"
xlfp audit --target-url https://provider.example --model deepseek-chat --claim deepseek

# Stronger: probe the official API in the same run as a live baseline.
export XLFP_TARGET_API_KEY='provider key'
export XLFP_OFFICIAL_API_KEY='official key'
xlfp audit \
  --target-url https://provider.example \
  --official-url https://api.deepseek.com \
  --model deepseek-chat --claim deepseek
```

Verdicts are `MATCH`, `MISMATCH`, or `INCONCLUSIVE`. `--claim` is
guessed from the model name when possible (`deepseek-chat` →
`deepseek`). A `MISMATCH` is strong evidence of a different family —
not, on its own, proof of fraud. Claims for not-yet-verifiable
families (`google`, `claude`) return inconclusive immediately,
without spending requests; use `uncover` to see their scored hints.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | `uncover`: identified · `audit`: match |
| 1 | operational or usage error |
| 2 | inconclusive |
| 3 | `audit`: mismatch |

## Use xlfp from your AI agent

This repo ships an agent skill ([skills/xlfp/SKILL.md](./skills/xlfp/SKILL.md))
that teaches coding agents to run `xlfp` correctly: when to use
`uncover` vs `audit`, always `--json`, exit-code meanings, key safety
(never paste keys into chat), and the honesty rules above.

From a clone, install it into your agent's skill directory:

```bash
scripts/install-skill.sh                 # every detected harness
scripts/install-skill.sh --target hermes # or codex / Muse / muse
scripts/install-skill.sh --dry-run       # show what would happen
```

Hermes can also install straight from the URL:
`hermes skills install https://raw.githubusercontent.com/tyk-swe/xlfp/main/skills/xlfp/SKILL.md`.
For Muse Code the script prints the `muse skills install …`
command instead of copying files, because that store owns its writes.
When run outside a checkout, downloaded Muse skill sources are kept in
`${XDG_CACHE_HOME:-~/.cache}/xlfp/skills/xlfp` for that command to use.

## Documentation

- [How it works](./docs/how-it-works.md) — probing, ranking,
  verified vs unverified families, and inherent limits.
- [Configuration](./docs/configuration.md) — transports, endpoint
  forms, API-key rules, environment variables, config profiles.
- [Development](./docs/development.md) — build/test commands,
  release process, database regeneration, package layout.

## Responsible use

Only probe endpoints you own or are authorized to test. Every run
sends real, potentially billable API requests (capped by
`--max-requests`) including literal special-token strings, which
some providers may log or prohibit. A `MISMATCH` verdict is
diagnostic tokenizer evidence about the model family — it is not, on
its own, proof of fraud, quantization, or quality degradation.
Verify before publishing claims about a provider.

## Contact

- Twitter/X: [@xodud_rkd](https://x.com/xodud_rkd)
- Discord: `@xodud`

## License and attribution

GPL-3.0-only — Copyright (C) 2026 tyk-swe. See
[LICENSE](./LICENSE). Tokenizer-data provenance and third-party
notices are in [NOTICE.md](./NOTICE.md). This is an independent
project, not affiliated with or endorsed by OpenAI, Anthropic,
Google, Alibaba, DeepSeek, Mistral AI, Zhipu AI, or Xiaomi; their
names identify the tokenizers and endpoints being measured.
