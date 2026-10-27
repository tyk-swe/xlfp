# xlfp (xo llm fingerprinter) — Project Specification

Status: **Final** — grilling interview complete. Decisions Q1–Q13 were
recorded as Draft during the interview and became Final upon the explicit
Q14 acceptance recorded below. This Final status does not authorize
implementation; building v1 needs a separate request.

> **Implementation addendum (post-acceptance).** Implementation was later
> requested separately and now exists in this repository (see
> [README.md](./README.md)). Two honesty notes for public readers:
> (1) "verified" in the shipped database currently means the family's
> counts are reproduced from its official tokenizer encoding/file and
> pinned by regression fixtures; the Q12 live held-out comparison
> against official endpoints is still outstanding because it needs
> provider keys, so no family is yet endpoint-certified under the full
> Q12 bar. (2) The interview history below (Q1–Q14) is the preserved
> decision record, not a user guide.

## Supplied brief (as supplied; narrowed by the decisions below)

- Type: Go CLI tool.
- `uncover`: identify the true model family behind an unknown/stealth endpoint
  via special-token and tokenizer fingerprinting plus token-counting analysis.
- `audit`: check a third-party provider against an official reference API for
  quantization, degradation, or silent model substitution.
- Families named in the brief: Qwen, MiMo, OpenAI, Google, GLM, DeepSeek, Mistral.
- Proposed packages: `pkg/fingerprint`, `pkg/probe`, `pkg/compare`.

## Scope contract

Status: **Accepted.** Accepted in Q14 when the user selected
"Accept + mark Final (Recommended)" via the questions tool in this session,
which also marked this document Final. No owning issue exists in this
empty repository, so this document is the durable acceptance record.

1. **Artifact boundary.** The deliverable of this interview is this
   specification document only. In scope: the settled decisions Q1–Q13,
   goals, non-goals, constraints, risks, validation bar, and the unresolved
   list below. Out of scope for this interview: Go source code, tests, live
   fixture collection, task or release plans, and provider account setup.
   The document describes the future v1 product (`uncover` and the narrowed
   `audit` per Q6); describing v1 does not authorize building it.
2. **Done means.** This interview is done when: (a) Q1–Q13 are recorded
   above as Draft decisions with their answer sources; (b) this scope text
   is explicitly accepted, rejected for revision, or accepted while keeping
   the document Draft, as chosen in Q14; and (c) unresolved implementation
   details are listed under "Unresolved details" instead of being silently
   assumed. No repository definition-of-done floor was found (empty repo).
3. **Deferred stages.** Implementation, fixture collection against official
   endpoints, any degradation/quantization verdict (excluded in Q6),
   active-evasion detection (excluded in Q4), and native clients beyond
   Gemini and Anthropic (Q2) are deferred proposals. Accepting this
   document does not approve any later stage; each needs its own request.
4. **No silent widening.** Later execution requests authorize only the
   accepted boundary. Work outside it needs explicit approval of a wider
   boundary or a follow-up effort of its own.

## Settled decisions

- Q1 — Probe signal contract (Draft, user answer via questions tool):
  **Add native APIs in v1.** v1 covers OpenAI-compatible endpoints plus
  native provider APIs, using each API's reported token-usage fields and
  special-token handling as the fingerprint source. The recommended narrower
  option (OpenAI-compatible only) was not chosen, so v1 scope includes the
  extra native auth, request-shape, and usage-trust work. Model free-text
  self-reports are not the primary fingerprint source.
- Q2 — Native API set (Draft, user answer via questions tool):
  **Gemini + Anthropic native.** v1 transports are OpenAI-compatible plus
  Google Gemini native and Anthropic Messages native. Qwen, DeepSeek, GLM,
  Mistral, and MiMo are reached through OpenAI-compatible APIs in v1 rather
  than separate native clients. This adds Claude to the fingerprint families
  beyond the families named in the supplied brief.
- Q3 — Result contract (Draft, user answer via questions tool):
  **Ranked families + inconclusive.** `uncover` ranks candidate model
  families with evidence and confidence and may return inconclusive. It does
  not force a single verdict and does not present a shared-tokenizer match as
  proof of an exact checkpoint.
- Q4 — Adversary assumption (Draft, user answer via questions tool):
  **Lazy substitution only.** v1 trusts self-consistent reported usage as
  derived from the backend tokenizer. Detecting a provider that actively
  fakes or normalizes token counts to evade fingerprinting is out of scope
  for v1; such cases should yield inconclusive, not a forced verdict.
- Q5 — Family coverage (Draft, user answer via questions tool):
  **Verified tokenizers only.** v1 may render ranked verdicts only for
  families whose token counting is locally reproducible and checked against
  official endpoints. Other families among Qwen, MiMo, OpenAI, Google, GLM,
  DeepSeek, Mistral, and Claude return unsupported or inconclusive until
  they meet that bar.
- Q6 — Audit claim scope (Draft, user answer via questions tool):
  **Substitution check only.** v1 `audit` verifies whether the target's
  fingerprinted family matches the provider's claim and may flag behavioral
  divergence from the official API as a signal. It does not claim proof of
  quantization, degradation, or "nerfing" of a same-family backend.
- Q7 — Audit reference source (Draft, user answer via questions tool):
  **Official baseline optional.** `audit` runs from the claimed family plus
  the local fingerprint database. When the user supplies an official
  endpoint, audit additionally probes it live in the same run and compares;
  no pre-recorded golden profiles are the v1 reference of record.
- Q8 — Probe budget (Draft, user answer via questions tool):
  **Adaptive with hard cap.** Probing proceeds in stages and stops at a
  confidence threshold or a conservative default request cap, whichever
  comes first. Flags may raise the cap, and each run reports requests spent.
  The exact default numbers remain a tuning detail under this rule.

- Q9 — API-key handling (Draft, user free-text answer via questions tool):
  **Env or config.** API keys may be supplied either through environment
  variables or through local config-file profiles; neither source alone is
  mandatory. This puts config-file secret handling (restrictive file
  permissions, no keys in logs or output) in scope. Precedence when both
  sources provide a key is settled in Q13.

- Q10 — Fingerprint database packaging (Draft, user answer via questions
  tool): **Embedded in binary.** A versioned fingerprint database is embedded
  in the Go binary and updated only through binary releases. Runs need no
  database network fetch and are reproducible for a given binary version.

- Q11 — Output and exit codes (Draft, user answer via questions tool):
  **Text + JSON, distinct exits.** Default output is a human-readable
  evidence report (probe counts, ranked families, requests spent); a stable
  JSON mode serves automation. Exit codes distinguish at least match,
  mismatch, inconclusive, and operational error; exact code numbers are a
  spec detail under this rule.

- Q12 — Verification bar (Draft, user answer via questions tool):
  **Official-endpoint match.** A family counts as verified under Q5 only when
  local count predictions match official endpoint counts on a held-out probe
  corpus, with chat-template and special-token overhead explicitly modeled
  rather than ignored, and regression fixtures stored for reruns.

- Q13 — Key-source precedence (Draft, user answer via questions tool):
  **Environment wins.** When an environment variable and a config profile
  both supply a key for the same endpoint, the environment value overrides
  the config value.
- Q14 — Scope acceptance (user answer via questions tool):
  **Accept + mark Final.** The scope contract above is accepted as written
  and this document is Final. Acceptance approves the spec only; it does
  not authorize implementation or any deferred stage.

## Open decisions

None. Implementation-stage details remain under "Unresolved details".

## Risks noted before Q1

- Token-count fingerprinting only works if the endpoint reports token usage
  truthfully; a provider can normalize, estimate, or omit it.
- Model self-reports of identity or token counts are unreliable and spoofable.

## Contract summary (derivation of Q1–Q13)

- **Goals.** Rank the likely model family of an unknown endpoint with
  evidence (or inconclusive), and check a provider's claimed family against
  its fingerprint, with optional live official comparison.
- **Non-goals.** Exact-checkpoint proof from a shared tokenizer, proof of
  quantization or degradation, defeating active usage-faking, verdicts for
  unverified families, and fingerprinting from model self-reports.
- **Constraints.** OpenAI-compatible plus Gemini and Anthropic native
  transports; usage-based signals; adaptive capped probing; keys from env
  (winning) or config; embedded versioned database; text plus JSON output
  with distinct exit codes.
- **Validation.** A family is verified only by official-endpoint count
  matches on a held-out corpus with template overhead modeled (Q12).

## Unresolved details

Left to the implementation stage; not settled by this interview: exact
default request cap and confidence thresholds (Q8), exact exit-code
numbers (Q11), config file format/path and environment variable names
(Q9/Q13), the per-family and per-generation database entry list (Q5/Q10),
probe corpus contents, and per-endpoint template-overhead parameters (Q12).
