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
   DeepSeek, Mistral, GLM, MiMo, Kimi), and clearly-labelled community
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

## OpenCode observations

`xlfp opencode` uses the same corpus, adaptive stages, database, and ranking
through the installed OpenCode CLI. Every probe starts a fresh session in
one private workspace, with a fixed title and agent prompt and tools disabled.
This keeps session context from growing across probes. The constant context
overhead is fitted by the existing ranking.

Resolved configuration is checked with `opencode debug config` at startup
and before every probe. Overrides and added context or integrations are
rejected before submitting a prompt, including settings loaded after the
inline configuration from managed installations or organizations.

One completed `step_finish` supplies the count: `tokens.input` plus
`tokens.cache.read` and `tokens.cache.write`. Output and reasoning tokens
are excluded. Tool calls, multiple distinct steps or sessions, failed runs,
and unusable usage are rejected. Model text is never used as evidence.

`--max-probes` (default 12) limits CLI probe runs. OpenCode may retry provider
requests internally, so actual provider-call counts are unknown. Reports
use `probes_spent`; HTTP reports continue to use `requests_spent`.

## Verified vs unverified families

"Source-verified" means the counts are reproduced from the family's
official tokenizer encoding/file at a hash-pinned revision (see
[NOTICE.md](../NOTICE.md)) and pinned by regression fixtures. In DB
v1.1.0 those families are OpenAI, Qwen, DeepSeek, Mistral, GLM, MiMo,
and Kimi. The maintainer has additionally completed live testing of
OpenAI, Qwen, DeepSeek, Mistral, GLM, and MiMo against official endpoints.

Google and Claude are scored and shown as **unverified candidates**:
their server tokenizers are closed, so their database entries come
from community conversions and can never be rendered as verdicts.
Treat them as hints.

## Inherent limits

- Family-level claims only. Token counts cannot prove an exact model
  version or checkpoint.
- Qwen2.5, MiMo-7B, and Kimi-Dev ship an identical `tokenizer.json`,
  so a match to that tokenizer is reported as a three-family tie.
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

## Tokenizer probe v2 (`xlfp tokprobe`)

An independent second tokenizer method. Each probe text is sent
inside a fixed wrapper, and a wrapper-only baseline request is
subtracted, so hidden input (chat template, injected system text)
cancels exactly instead of being fitted as a constant. A Bayesian
open-set posterior over the database entries — exactly this
tokenizer, an unlisted close relative, or unrelated to every entry —
produces an `exact` / `related` / `unknown` verdict, so an unlisted
tokenizer is a first-class answer rather than a forced closest
match. Probe texts are chosen adaptively in batches by a pairwise
Bhattacharyya error bound, probing stops at 99% posterior or
negligible expected gain, and a closing baseline that disagrees
with the opening one marks the result inconclusive
(`baseline_drift`). A failed closing baseline also forces inconclusive
(`baseline_unvalidated`) and records the error. Split evidence across
listed candidates, including shared tokenizers, yields inconclusive
(`ambiguous_tokenizer`); unverified proxies remain display-only
(`unverified_tokenizer`). The result is also checked against `--model`
after at least four usable differential counts and successful baseline
validation, when the claimed family has a verified reference:
the probability mass on that model's family is reported as
consistent (≥ 0.9), inconsistent (≤ 0.1), or unclear. Exit codes:
0 for any verdict, 2 inconclusive, 1 operational error.
`--max-requests` (default 24) caps every HTTP attempt, baselines
and retries included.

## Number fingerprint (`xlfp numbers`)

A behavioral signal that needs no token counts at all. The model
writes a few hundred first-instinct integers between 1 and 355
(three randomized challenges per round, own templates in five
languages, patterns and tools forbidden). "Random" numbers from a
language model are not uniform — each model has fairly stable
preferences for values, positions, and final digits. Replies are
parsed to their longest valid integer run (at least
`max(80, ceil(0.55 × requested))` values), checked for model-like
sequence shape (lag-1 autocorrelation and small-step share both
over their limits ⇒ `not_model_reply`, which filters sorted,
shuffled, and program-generated numbers), featurized (Hellinger
value distribution plus position/final-digit blocks), and ranked
against the reference bank by an equal-weight ensemble of centroid
cosine, k-nearest-neighbour distance, and an optional LDA
discriminant fitted offline. Confidence is a temperature-scaled
softmax fitted on held-out three-answer rounds using the same
aggregation as inference. Calibration requires at least four
samples per entry and must improve on uniform chance; otherwise,
or with other than three valid answers, the result is a ranking
only. Older banks need retraining to record this calibration.
Imported LDA models also produce rankings without
calibrated confidence; leave-one-out reports exclude the externally
fitted LDA projection and evaluate centroid/kNN only.
The ranking is **closed-set within the bank**: a
model outside the bank still gets a closest candidate, and no
result proves an exact model. `--rounds` repeats detection,
`--strict` requires naturally completed answers, `--output` saves
the full evidence, and `--input` re-scores a saved report, an
answers array, a sampling batch, or a pasted reply offline with
zero requests. Failed requests are reported on stderr and return exit
code 1, including when replaying saved failures. The full report and
usable rankings are retained. Otherwise, exit code 0 means every round
ranked; exit code 2 means at least one round was inconclusive or rejected.

Scoring, retraining, and feature export use at most the requested
number of values from each reply, including in strict mode. The
full captured reply remains available in saved evidence.

Sampling records transport, streaming, reasoning effort, service
tier, and timeout in each batch and enrolled sample. Resuming
requires matching settings and endpoint host. Legacy batches
without recorded settings can be replayed but cannot be resumed.

The bank ships empty (`insufficient_bank`) until reference samples
are collected with `xlfp sample` (fixed 36-challenge set across 12
prompt environments, failures kept on record, `--resume`
supported), validated and merged with `xlfp enroll`, and fitted
with `xlfp retrain` (reply-check limits, calibration temperature,
leave-one-out report; `--export-features` plus
`tools/train_numprint.py` fit the optional LDA). Only sample
models from endpoints trusted to be the real thing — official
provider APIs with your own keys. Bank provenance rules are in
[clean-room.md](./clean-room.md).

Tokenizer and number evidence are always reported separately and
never merged into one confidence: forged usage numbers must not
contaminate behavioral evidence, and vice versa.
