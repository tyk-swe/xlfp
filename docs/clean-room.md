# Clean-room provenance for the two-signal detector

xlfp's number fingerprint (`pkg/numprint`) and tokenizer probe v2
(`pkg/fingerprint/tokprobe.go`) were implemented independently,
from published *concepts* only, so that no third-party code, data,
or text is redistributed and no additional license notice is owed
for them. This file records what was and was not consulted.

## Sources consulted

- The public documentation concepts of the "LM Fingerpoint
  Detector" project (Ikaleio/lm-detector): that a first-instinct
  integer sequence in a fixed range carries model-specific value
  preferences; that differential (wrapper-minus-baseline) token
  counts cancel hidden chat-template input; and that tokenizer
  evidence should be reported separately from behavioral evidence.
  Documentation was read at the concept level (README and the
  "how it works" pages) during planning. Its source code was not
  read, copied, or used as an implementation reference.
- Standard published methods, re-derived here from their usual
  definitions: Hellinger embedding of count distributions,
  lag-1 autocorrelation, nearest-centroid and k-nearest-neighbour
  scoring, linear discriminant analysis, temperature-scaled
  softmax calibration, discrete Laplace (two-sided geometric)
  noise models, Bayesian open-set decisions, and greedy batch
  selection under a pairwise Bhattacharyya error bound
  (Golovin/Krause-style adaptive submodularity).

## Explicitly not used

- No upstream source code in any language.
- No upstream reference bank, detector parameters, tokenizer bank,
  probe texts, challenge templates, or model-mapping tables.
- No upstream fitted constants (sequence-check limits, ensemble
  weights, calibration temperature, posterior hyper-parameters).
  Every constant in `pkg/numprint` and the tokenizer probe is
  either a structural method choice (e.g. the 1–355 integer
  range, three answers per round) or fitted by `xlfp retrain`
  from reference data xlfp collected itself. Provisional defaults
  used before a bank exists are derived from first principles
  (e.g. the small-step share of an i.i.d. uniform sequence) and
  are labelled provisional in the code.

## Data provenance rules going forward

- Reference number samples enter the bank only through
  `xlfp sample` against endpoints the maintainer is authorized
  to probe (official provider APIs with the maintainer's own
  keys), and `xlfp enroll`, which records endpoint host, model
  id, parameters, and collection date for every batch.
- Tokenizer expected counts keep following the existing rule in
  [NOTICE.md](../NOTICE.md): computed by `tools/` generators
  from hash-pinned official tokenizer files; only derived
  integer counts ship.
- Challenge prompt templates in `pkg/numprint` were written for
  this project in five languages; they share only the task
  semantics (write N first-instinct integers in range, avoid
  patterns, no tools or code) with any other implementation.
