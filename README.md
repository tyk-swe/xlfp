# xlfp — xo llm fingerprinter

`xlfp` is a diagnostic Go CLI that identifies the model **family** behind
an LLM endpoint from its reported token usage, and audits whether a
third-party provider's claimed family matches its fingerprint.

> **Status: pre-release (v0.1).** The fingerprint database is reproduced
> from official tokenizer encodings/files, but has not yet been validated
> against live official endpoints (see "How it works"). Treat results as
> diagnostic evidence, and expect breaking changes before v1.0.

It implements the specification in [PROJECT.md](./PROJECT.md).
Two limits from that spec are load-bearing:

- Results are **ranked families with evidence, or inconclusive**. Token
  counts cannot prove an exact checkpoint, and some families share a
  tokenizer byte-for-byte (Qwen2.5 and MiMo-7B ship an identical
  `tokenizer.json`), so ties between them are reported, not guessed.
- `audit` detects **family substitution**. It does not claim proof of
  quantization or degradation of a same-family backend, and a provider
  that actively fakes token counts yields *inconclusive*, not a verdict.

## Build

```bash
go build -o xlfp .
./xlfp version
./xlfp families        # inspect the embedded fingerprint database
```

Or install onto your `PATH`:

```bash
go install github.com/tyk-swe/xlfp@latest
```

Requires Go 1.23+. There are no Go module dependencies. Examples below
write `xlfp` and assume it is on your `PATH` (use `./xlfp` after a local
build). Hostnames such as `provider.example` are placeholders — replace
them with real endpoints you are authorized to probe.

## Usage

### Uncover a stealth/unknown endpoint

```bash
# XLFP_API_KEY is the explicit opt-in key for probing arbitrary endpoints.
# (OPENAI_API_KEY and friends are only ever sent to official hosts.)
export XLFP_API_KEY=sk-...
xlfp uncover --endpoint https://provider.example --model stealth-model-x
xlfp uncover --endpoint https://provider.example/v1 --model stealth-model-x --json
```

Sample output (values vary by endpoint):

```text
Endpoint: https://provider.example  Model: stealth-model-x  Transport: openai
Database: v1.0.0  Requests spent: 8
Result: IDENTIFIED as family "deepseek" (entry deepseek-v3, tokenizer DeepSeek V3 BPE, confidence 0.984)
Note: family-level identification from tokenizer evidence; not proof of an exact checkpoint.
Ranked source-verified families:
  1. deepseek   confidence 0.984  MAE 0.00  overhead 6.0  entry deepseek-v3 (DeepSeek V3 BPE)
```

### Audit a third-party provider

```bash
# Claimed family checked against the fingerprint database:
xlfp audit --target-url https://provider.example --model deepseek-chat --claim deepseek

# With a live official baseline probed in the same run:
xlfp audit \
  --target-url https://provider.example --target-api-key-env PROVIDER_KEY \
  --official-url https://api.deepseek.com --official-api-key-env DEEPSEEK_KEY \
  --model deepseek-chat --claim deepseek
```

`--claim` is inferred from the model name when possible (for example
`deepseek-chat` implies the `deepseek` family). Claims for unverified
families (`google`, `claude`) return inconclusive immediately, without
spending probe requests; use `uncover` to see their scored evidence.

### Transports

Three transports are supported. The official Anthropic and Gemini hosts
are auto-detected; everything else defaults to `openai`, so pass
`--transport anthropic` or `--transport gemini` for native APIs behind
other hostnames or gateways:

| Transport | Request shape | Usage field read |
|---|---|---|
| `openai` (compatible) | `POST /v1/chat/completions` | `usage.prompt_tokens` |
| `anthropic` | `POST /v1/messages` | `usage.input_tokens` |
| `gemini` | `POST /v1beta/models/<model>:generateContent` | `usageMetadata.promptTokenCount` |

The endpoint value may be a bare host, a versioned base (`/v1`, `/v2`,
`/v1beta`), any other prefix base such as an Azure deployment URL or
Gemini's `/v1beta/openai`, or a full request URL. A bare host gains the
default version prefix; any non-empty path is treated as a complete
prefix. Query parameters such as Azure's `api-version` are preserved,
while credential query parameters and URL userinfo are rejected. Qwen,
DeepSeek, GLM, Mistral, MiMo and OpenAI endpoints are normally reached
through the `openai` transport. Azure OpenAI hosts receive the `api-key`
header instead of `Authorization`.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | `uncover`: identified · `audit`: match |
| 1 | operational or usage error |
| 2 | inconclusive |
| 3 | `audit`: mismatch |

## How it works

1. A fixed corpus of 12 probe strings (English, CJK, numbers, emoji ZWJ
   sequences, code, whitespace runs, special-token literals, …) is sent
   to the endpoint in three adaptive stages. Probing stops early when
   the ranking is confidently settled, never exceeding `--max-requests`
   (default 12) per endpoint. Completed and inconclusive runs report
   requests spent (retries count); a run that fails before verdict-grade
   evidence exits with an operational error instead.
2. The reported prompt-token counts are compared against an embedded,
   versioned database of expected counts per tokenizer. The database is
   generated from real tokenizer files by
   [tools/gen_db.py](./tools/gen_db.py): official tiktoken encodings
   (OpenAI), official Hugging Face tokenizer files (Qwen, DeepSeek,
   Mistral, GLM, MiMo), and clearly-labelled community conversions for
   the closed Google/Claude server tokenizers.
3. Chat APIs count template tokens around the user text, so ranking fits
   a constant per-endpoint overhead (median of observed − expected) and
   scores the mean absolute residual after removing it. Confidence
   accumulates per probe over one global softmax. Counts that barely
   vary across probes, however well they fit after the overhead shift,
   are declared unreliable rather than identified. Special-token probes
   contribute acceptance/rejection evidence only (a 400/422 refusal);
   their counts never enter the fit, because servers differ on whether
   literal special-token text is parsed as specials.
4. Only **source-verified** families can be rendered as verdicts, and
   an unverified family that fits clearly better than every
   source-verified one forces inconclusive rather than a false verdict.
   "Source-verified" means the counts are reproduced from the family's
   official tokenizer encoding/file at a hash-pinned revision (see
   [NOTICE.md](./NOTICE.md)) and pinned by regression fixtures. In DB
   v1.0.0 those families are OpenAI, Qwen, DeepSeek, Mistral, GLM and
   MiMo. Collecting a held-out fixture corpus against live official
   endpoints (the second half of the spec's verification bar) requires
   provider keys and is the outstanding validation step; until then,
   treat positive identifications as strong tokenizer-file evidence,
   not endpoint-certified proof. Google and Claude are scored and shown
   as unverified candidates (their server tokenizers are closed).

## API keys and config

Keys come from the **environment or a config file only** — there is no
flag that takes a literal key. On conflict the environment wins.

Environment variables:

- `XLFP_API_KEY`, plus `XLFP_TARGET_API_KEY` / `XLFP_OFFICIAL_API_KEY`
  per audit side. These tool-specific variables are the explicit opt-in
  for probing untrusted endpoints.
- Provider-standard variables (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
  `GEMINI_API_KEY` / `GOOGLE_API_KEY`) are honored **only when the
  endpoint host is that provider's official API host**. `xlfp` exists
  to probe unknown endpoints, so an ambient official key is never sent
  anywhere else. Redirects are never followed, for the same reason.
- `--api-key-env NAME` names any variable to read the key from.
- Non-secret overrides: `XLFP_ENDPOINT`, `XLFP_MODEL`, `XLFP_TRANSPORT`,
  plus per-side audit forms such as `XLFP_TARGET_ENDPOINT` and
  `XLFP_OFFICIAL_MODEL` (same pattern for `ENDPOINT`/`MODEL`/`TRANSPORT`).
- API keys are never sent over cleartext `http` except to loopback
  hosts, and a key stored in a profile is bound to that profile's
  endpoint host (retargeting it requires an explicitly named key).

Config file: `--config PATH`, else `$XLFP_CONFIG`, else
`xlfp/config.json` inside the operating system's user config directory
(Linux: `${XDG_CONFIG_HOME:-~/.config}`, macOS: `~/Library/Application
Support`, Windows: `%AppData%`). A config in the current directory is
never auto-loaded; pass it explicitly with `--config`. Top-level fields
are defaults; named profiles override them:

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

## Development

```bash
go vet ./...
go test -race -count=1 ./...
```

The `pkg/` packages are importable, but before v1.0 there is **no Go API
stability guarantee**; the CLI, its exit codes, and its JSON fields are
the supported surface. Package layout mirrors the spec:

- `pkg/fingerprint` — embedded DB (`db.json`), overhead-fit ranking,
  tie/inconclusive rules.
- `pkg/probe` — the three transports and the staged, capped probe runner.
- `pkg/compare` — audit verdicts and overhead-normalized divergence.
- `pkg/config` — flag/env/config resolution (environment wins for keys).
- `pkg/report` — human-readable rendering (JSON is the result structs).

To regenerate the fingerprint database after changing the probe corpus
or tokenizer sources, use the project Python environment:

```bash
python3 -m venv .venv
.venv/bin/pip install -r tools/requirements.txt
.venv/bin/python tools/gen_db.py
```

Downloads are pinned to recorded revisions and verified by SHA-256.

Known v1 limits, restated: family-level claims only; Qwen/MiMo cannot be
separated by token counts; Google/Claude verdicts wait on a verifiable
server-tokenizer source; endpoints that omit or fabricate usage produce
inconclusive results by design.

## Responsible use

Only probe endpoints you own or are authorized to test. Every run sends
real, potentially billable API requests (capped by `--max-requests`)
including literal special-token strings, which some providers may log
or prohibit. A `MISMATCH` verdict is diagnostic tokenizer evidence
about the model family — it is not, on its own, proof of fraud,
quantization, or quality degradation. Verify before publishing claims
about a provider.

## License and attribution

MIT — see [LICENSE](./LICENSE). Tokenizer-data provenance and
third-party notices are in [NOTICE.md](./NOTICE.md). This is an
independent project, not affiliated with or endorsed by OpenAI,
Anthropic, Google, Alibaba, DeepSeek, Mistral AI, Zhipu AI, or Xiaomi;
their names identify the tokenizers and endpoints being measured.
