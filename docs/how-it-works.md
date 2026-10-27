# How xlfp works

`xlfp` identifies the model **family** behind an LLM endpoint from
the prompt-token counts the endpoint reports. It never asks the model
what it is (self-reports are unreliable), and it never claims an
exact checkpoint.

## The pipeline

1. A fixed corpus of 12 probe strings (English, CJK, numbers, emoji
   ZWJ sequences, code, whitespace runs, special-token literals, …)
   is sent to the endpoint in three adaptive stages. Probing stops
   early when the ranking is confidently settled, never exceeding
   `--max-requests` (default 12) per endpoint. On a terminal, a
   one-line progress counter shows requests as they are sent.
2. The reported prompt-token counts are compared against an embedded,
   versioned database of expected counts per tokenizer, generated
   from real tokenizer files by
   [tools/gen_db.py](../tools/gen_db.py): official tiktoken
   encodings (OpenAI), official Hugging Face tokenizer files (Qwen,
   DeepSeek, Mistral, GLM, MiMo), and clearly-labelled community
   conversions for the closed Google/Claude server tokenizers.
   Provenance and pinned hashes are recorded in
   [NOTICE.md](../NOTICE.md).
3. Chat APIs count template tokens around the user text, so ranking
   fits a constant per-endpoint overhead (median of observed −
   expected) and scores the mean absolute residual after removing it.
   Confidence accumulates per probe over one global softmax. Counts
   that barely vary across probes are declared unreliable rather than
   identified. Special-token probes contribute acceptance/rejection
   evidence only (a 400/422 refusal); their counts never enter the
   fit. Failed observations do not contribute counts to the fit; malformed,
   fractional, negative, or overflowing usage values are not usable.
   Every inconclusive result carries a machine-stable
   `reason_code` (JSON) alongside the technical `reason`.
4. Only **source-verified** families can be rendered as verdicts, and
   an unverified family that fits clearly better than every
   source-verified one forces inconclusive rather than a false
   verdict.

## Verified vs unverified families

"Source-verified" means the counts are reproduced from the family's
official tokenizer encoding/file at a hash-pinned revision (see
[NOTICE.md](../NOTICE.md)) and pinned by regression fixtures. In DB
v1.0.0 those families are OpenAI, Qwen, DeepSeek, Mistral, GLM and
MiMo. The maintainer has additionally completed live testing of these
families against official endpoints.

Google and Claude are scored and shown as **unverified candidates**:
their server tokenizers are closed, so their database entries come
from community conversions and can never be rendered as verdicts.
Treat them as hints.

## Inherent limits

- Family-level claims only. Token counts cannot prove an exact model
  version or checkpoint.
- Some families share a tokenizer byte-for-byte (Qwen2.5 and MiMo-7B
  ship an identical `tokenizer.json`), so ties between them are
  reported, not guessed.
- `audit` detects **family substitution**. It does not claim proof
  of quantization or degradation of a same-family backend; the
  optional target-vs-official divergence number is a signal only.
- Missing or implausible usage produces inconclusive results. These
  plausibility checks cannot authenticate provider-reported counts:
  an endpoint can fabricate a known tokenizer's counts and receive an
  identified result. Confidence measures fit, not provider honesty.
- A `MISMATCH` verdict is strong tokenizer evidence about the model
  family — it is not, on its own, proof of fraud. Verify before
  publishing claims about a provider.
- In v0.1.5, an official audit baseline that fails after collecting
  enough counts can still influence a verdict. Inspect the official
  result with `--json` or `--verbose` and rerun if probing stopped early;
  a failed baseline does not corroborate a verdict. See
  [PR #3](https://github.com/tyk-swe/xlfp/pull/3) for the pending fix.
