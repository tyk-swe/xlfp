# Third-party notices and data provenance

`xlfp` itself is licensed under GPL-3.0-only (see
[LICENSE](./LICENSE)). Copyright (C) 2026 tyk-swe. This project
is an independent diagnostic tool and is **not affiliated with, endorsed
by, or sponsored by** OpenAI, Anthropic, Google, Alibaba (Qwen),
DeepSeek, Mistral AI, Zhipu AI (GLM), or Xiaomi (MiMo). Product and
model names are used only to identify the tokenizers/endpoints being
measured.

## Go dependencies

The terminal dashboard includes Bubble Tea, Bubbles, and their dependencies.
The module versions are pinned in [go.mod](./go.mod). Their license and
attribution texts are included in [THIRD_PARTY_LICENSES.txt](./THIRD_PARTY_LICENSES.txt)
and in release archives. `github.com/mattn/go-localereader` v0.0.1 does
not ship a separate license file; its upstream README identifies its license
as MIT and its author as Yasuhiro Matsumoto.

## Fingerprint database sources

`pkg/fingerprint/db.json` contains only integer token counts for the
project's own probe strings — no tokenizer vocabularies, merge tables,
or model weights are redistributed. Counts were computed with
`tools/gen_db.py` from the sources below. Every download is pinned to a
revision and verified by SHA-256 in the generator. Model-repository
terms may differ from code licenses; only the derived integer counts
are shipped here.

| Entry | Source | Pinned revision (short) | File SHA-256 (short) |
|---|---|---|---|
| OpenAI `o200k_base`, `cl100k_base` | `tiktoken` Python package (official encodings) | package pinned in `tools/requirements.txt` | — |
| Qwen2.5 | `Qwen/Qwen2.5-7B-Instruct` `tokenizer.json` | `a09a3545` | `c0382117` |
| DeepSeek V3 | `deepseek-ai/DeepSeek-V3` `tokenizer.json` | `e815299b` | `621ac2e3` |
| Mistral v0.3 | `mistralai/Mistral-7B-Instruct-v0.3` `tokenizer.json` | `c170c708` | `e553af6f` |
| GLM-4 | `THUDM/glm-4-9b-chat` `tokenizer.model` (tiktoken-format ranks) | `bd8234fe` | `5a493598` |
| MiMo-7B | `XiaomiMiMo/MiMo-7B-Base` `tokenizer.json` | `c72df458` | `c0382117` |
| Google (Gemma proxy, unverified) | `Xenova/gemma-tokenizer` `tokenizer.json` (community conversion) | `fbc8c39d` | `05e97791` |
| Claude (unverified) | `Xenova/claude-tokenizer` `tokenizer.json` (community conversion) | `cae68882` | `c241737d` |

Note the identical SHA-256 for the Qwen2.5 and MiMo-7B `tokenizer.json`
files at the pinned revisions: those two families cannot be separated
by token counting, and `xlfp` reports them as a shared-tokenizer tie.

Full revisions and hashes live in `tools/gen_db.py`, which fails closed
on any hash mismatch.
