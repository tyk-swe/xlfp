# xlfp — xo llm fingerprinter

**Find out which AI model family is really behind an API endpoint.**

Some endpoints hide which model they run, and some providers quietly
serve a different (often cheaper) model than the one they advertise.
`xlfp` sends a few tiny test messages, looks at how the endpoint
counts tokens, and matches that pattern against known model families
(DeepSeek, Qwen, OpenAI, Mistral, GLM, MiMo, …). No knowledge of
tokenizers needed: the output tells you the likely family, what it
means, and what to do next — or honestly says "inconclusive."

> **Status: pre-release (v0.1).** The fingerprint database is
> reproduced from official tokenizer files, but has not yet been
> validated against live official endpoints (see "How it works").
> Treat results as strong diagnostic evidence, not proof, and expect
> breaking changes before v1.0. Design decisions live in
> [PROJECT.md](./PROJECT.md).

What it can tell you:

- Which model **family** an endpoint most likely is, with evidence.
- Whether a provider's endpoint matches the family it claims.

What it can't:

- The exact model version. Token counts can't prove that.
- Whether a same-family model was secretly shrunk or degraded.
- Anything at all if a provider fakes its token counts — that comes
  back *inconclusive*, by design, never as a guess.

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
to pin a release (e.g. `v0.1.0`) or `XLFP_INSTALL_DIR` to change the
destination. *While this repository is private, the script uses the
GitHub CLI (`gh`, after `gh auth login`) or a `GH_TOKEN` to read the
release; anonymous download starts working when the repo goes public.*

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
scripts/install-skill.sh --target hermes # or codex / claude / muse
scripts/install-skill.sh --dry-run       # show what would happen
```

Hermes can also install straight from the URL (once the repo is
public): `hermes skills install https://raw.githubusercontent.com/tyk-swe/xlfp/main/skills/xlfp/SKILL.md`.
For Muse Code the script prints the `muse skills install …`
command instead of copying files, because that store owns its writes.

## Reference

### Transports and endpoint forms

Three API shapes are supported. The official Anthropic and Gemini
hosts are auto-detected; everything else defaults to `openai`, so pass
`--transport anthropic` or `--transport gemini` for native APIs behind
other hostnames or gateways:

| Transport | Request shape | Usage field read |
|---|---|---|
| `openai` (compatible) | `POST /v1/chat/completions` | `usage.prompt_tokens` |
| `anthropic` | `POST /v1/messages` | `usage.input_tokens` |
| `gemini` | `POST /v1beta/models/<model>:generateContent` | `usageMetadata.promptTokenCount` |

The endpoint value may be a bare host, a versioned base (`/v1`, `/v2`,
`/v1beta`), any other prefix base such as an Azure deployment URL or
Gemini's `/v1beta/openai`, or a full request URL. A bare host gains
the default version prefix; any non-empty path is treated as a
complete prefix. Query parameters such as Azure's `api-version` are
preserved, while credential query parameters and URL userinfo are
rejected. Qwen, DeepSeek, GLM, Mistral, MiMo and OpenAI endpoints are
normally reached through the `openai` transport. Azure OpenAI hosts
receive the `api-key` header instead of `Authorization`.

### How it works

1. A fixed corpus of 12 probe strings (English, CJK, numbers, emoji
   ZWJ sequences, code, whitespace runs, special-token literals, …)
   is sent to the endpoint in three adaptive stages. Probing stops
   early when the ranking is confidently settled, never exceeding
   `--max-requests` (default 12) per endpoint. On a terminal, a
   one-line progress counter shows requests as they are sent.
2. The reported prompt-token counts are compared against an embedded,
   versioned database of expected counts per tokenizer, generated
   from real tokenizer files by [tools/gen_db.py](./tools/gen_db.py):
   official tiktoken encodings (OpenAI), official Hugging Face
   tokenizer files (Qwen, DeepSeek, Mistral, GLM, MiMo), and
   clearly-labelled community conversions for the closed
   Google/Claude server tokenizers.
3. Chat APIs count template tokens around the user text, so ranking
   fits a constant per-endpoint overhead (median of observed −
   expected) and scores the mean absolute residual after removing it.
   Confidence accumulates per probe over one global softmax. Counts
   that barely vary across probes are declared unreliable rather than
   identified. Special-token probes contribute acceptance/rejection
   evidence only (a 400/422 refusal); their counts never enter the
   fit. Every inconclusive result carries a machine-stable
   `reason_code` (JSON) alongside the technical `reason`.
4. Only **source-verified** families can be rendered as verdicts, and
   an unverified family that fits clearly better than every
   source-verified one forces inconclusive rather than a false
   verdict. "Source-verified" means the counts are reproduced from
   the family's official tokenizer encoding/file at a hash-pinned
   revision (see [NOTICE.md](./NOTICE.md)) and pinned by regression
   fixtures. In DB v1.0.0 those families are OpenAI, Qwen, DeepSeek,
   Mistral, GLM and MiMo. Collecting a held-out fixture corpus
   against live official endpoints (the second half of the spec's
   verification bar) requires provider keys and is the outstanding
   validation step; until then, treat positive identifications as
   strong tokenizer-file evidence, not endpoint-certified proof.
   Google and Claude are scored and shown as unverified candidates.
   Some families share a tokenizer byte-for-byte (Qwen2.5 and MiMo-7B
   ship an identical `tokenizer.json`), so ties between them are
   reported, not guessed.

### API keys and config

Keys come from the **environment or a config file only** — there is
no flag that takes a literal key. On conflict the environment wins.

Environment variables:

- `XLFP_API_KEY`, plus `XLFP_TARGET_API_KEY` / `XLFP_OFFICIAL_API_KEY`
  per audit side. These tool-specific variables are the explicit
  opt-in for probing untrusted endpoints.
- Provider-standard variables (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
  `GEMINI_API_KEY` / `GOOGLE_API_KEY`) are honored **only when the
  endpoint host is that provider's official API host**. `xlfp` exists
  to probe unknown endpoints, so an ambient official key is never
  sent anywhere else. Redirects are never followed, for the same
  reason.
- `--api-key-env NAME` names any variable to read the key from.
- Non-secret overrides: `XLFP_ENDPOINT`, `XLFP_MODEL`,
  `XLFP_TRANSPORT`, plus per-side audit forms such as
  `XLFP_TARGET_ENDPOINT` and `XLFP_OFFICIAL_MODEL` (same pattern for
  `ENDPOINT`/`MODEL`/`TRANSPORT`).
- API keys are never sent over cleartext `http` except to loopback
  hosts, and a key stored in a profile is bound to that profile's
  endpoint host (retargeting it requires an explicitly named key).

Config file: `--config PATH`, else `$XLFP_CONFIG`, else
`xlfp/config.json` inside the operating system's user config
directory (Linux: `${XDG_CONFIG_HOME:-~/.config}`, macOS:
`~/Library/Application Support`, Windows: `%AppData%`). A config in
the current directory is never auto-loaded; pass it explicitly with
`--config`. Top-level fields are defaults; named profiles override
them:

```json
{
  "profiles": {
    "provider-x": {
      "endpoint": "https://provider.example",
      "model": "deepseek-chat",
      "transport": "openai",
      "api_key_env": "PROVIDER_X_KEY"
    }
  }
}
```

Use a profile with `--profile provider-x` (uncover) or
`--target-profile` / `--official-profile` (audit). A config file that
stores literal `api_key` values must not be group/world readable —
`xlfp` refuses it until you run `chmod 600`. Keys are never printed,
logged, or included in either output format; endpoints are displayed
with userinfo and query strings stripped.

### Development

```bash
go vet ./...
go test -race -count=1 ./...
```

Releases are cut by tagging `v*`, which runs the GoReleaser workflow
(`.goreleaser.yaml`) and publishes per-OS/arch archives plus SHA-256
checksums to a GitHub Release.

The `pkg/` packages are importable, but before v1.0 there is **no Go
API stability guarantee**; the CLI, its exit codes, and its JSON
fields are the supported surface. Package layout mirrors the spec:

- `pkg/fingerprint` — embedded DB (`db.json`), overhead-fit ranking,
  tie/inconclusive rules, stable reason codes.
- `pkg/probe` — the three transports and the staged, capped probe
  runner (with an `OnProbe` progress callback).
- `pkg/compare` — audit verdicts and overhead-normalized divergence.
- `pkg/config` — flag/env/config resolution (environment wins for
  keys).
- `pkg/report` — plain-English rendering, `--verbose` technical
  detail (JSON is the result structs).

To regenerate the fingerprint database after changing the probe
corpus or tokenizer sources, use the project Python environment:

```bash
python3 -m venv .venv
.venv/bin/pip install -r tools/requirements.txt
.venv/bin/python tools/gen_db.py
```

Downloads are pinned to recorded revisions and verified by SHA-256.

Known v1 limits, restated: family-level claims only; Qwen/MiMo
cannot be separated by token counts; Google/Claude verdicts wait on
a verifiable server-tokenizer source; endpoints that omit or
fabricate usage produce inconclusive results by design.

## Responsible use

Only probe endpoints you own or are authorized to test. Every run
sends real, potentially billable API requests (capped by
`--max-requests`) including literal special-token strings, which
some providers may log or prohibit. A `MISMATCH` verdict is
diagnostic tokenizer evidence about the model family — it is not, on
its own, proof of fraud, quantization, or quality degradation.
Verify before publishing claims about a provider.

## License and attribution

MIT — see [LICENSE](./LICENSE). Tokenizer-data provenance and
third-party notices are in [NOTICE.md](./NOTICE.md). This is an
independent project, not affiliated with or endorsed by OpenAI,
Anthropic, Google, Alibaba, DeepSeek, Mistral AI, Zhipu AI, or
Xiaomi; their names identify the tokenizers and endpoints being
measured.
