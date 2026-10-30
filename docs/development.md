# Development

Requirements: Go 1.23+. The terminal dashboard uses pinned Go modules;
the CLI and dashboard share the same check preparation and execution code.
Release archives include the bundled third-party license notices.

```bash
go build -o xlfp .
gofmt -l .
go vet ./...
go test -race -count=1 ./...
sh -n install.sh && sh -n scripts/install-skill.sh
```

Releases are cut by tagging `v*`, which runs the GoReleaser workflow
(`.goreleaser.yaml`) and publishes per-OS/arch archives plus SHA-256
checksums to a GitHub Release. Validate the release configuration
without publishing with `goreleaser check` and
`goreleaser release --snapshot --clean`.

The `pkg/` packages are importable under the project's GPL-3.0-only
license, but before v1.0 there is **no Go API stability guarantee**;
the CLI, its exit codes, and its JSON fields are the supported
surface. Package layout:

- `pkg/fingerprint` — embedded DB (`db.json`), overhead-fit ranking,
  tie/inconclusive rules, stable reason codes, plus the differential
  tokenizer probe (`tokprobe.go`: Bayesian open-set posterior,
  adaptive batch selection, claim check).
- `pkg/numprint` — number fingerprint: challenge generation, reply
  parsing and sequence check, features, bank ranking/calibration,
  sampling set, batch enrollment, and retraining. The embedded
  `bank.json` ships empty on purpose (see docs/clean-room.md).
- `pkg/probe` — the four transports (OpenAI chat, Anthropic,
  Gemini, OpenAI Responses), the staged capped probe runner, the
  tokenizer-probe runner, streaming/non-streaming reply capture
  (`Chat`), and the multi-round numbers runner.
- `pkg/opencode` — isolated OpenCode CLI sessions, effective-configuration
  checks, and structured usage parsing for stealth model testing.
- `pkg/compare` — audit verdicts and overhead-normalized divergence.
- `pkg/config` — flag/env/config resolution (environment wins for
  keys).
- `pkg/report` — plain-English rendering, `--verbose` technical
  detail (JSON is the result structs).

## Regenerating the fingerprint database

After changing the probe corpus or tokenizer sources, use the project
Python environment:

```bash
python3 -m venv .venv
.venv/bin/pip install -r tools/requirements.txt
.venv/bin/python tools/gen_db.py
```

Downloads are pinned to recorded revisions and verified by SHA-256;
the generator fails closed on any mismatch. Provenance for every
entry is recorded in [NOTICE.md](../NOTICE.md).

## Building the number reference bank

```bash
# 1. Collect from an official endpoint trusted to be the named model
#    (36 billable requests; failures are recorded, --resume continues).
export XLFP_API_KEY='official key'
xlfp sample --endpoint https://api.example.com --model some-model \
  --family somefamily --out batch-some-model.json

# 2. Enroll into a bank file, then refit its parameters.
xlfp enroll batch-some-model.json --bank numprint-bank.json
xlfp retrain --bank numprint-bank.json

# 3. Optional LDA discriminant (Python env with numpy from
#    tools/requirements.txt):
xlfp retrain --bank numprint-bank.json --export-features features.json
.venv/bin/python tools/train_numprint.py --features features.json --out lda.json
xlfp retrain --bank numprint-bank.json --lda lda.json
```

With imported LDA parameters, the leave-one-out report evaluates only
centroid/kNN scorers. The LDA projection and class means include all
exported samples, so they cannot be used to validate held-out samples.
LDA rankings remain available, with confidence marked uncalibrated.

Use the bank with `xlfp numbers --bank numprint-bank.json`. Repeat
per model. Never import reference data collected by other projects;
see [clean-room.md](./clean-room.md).

## Agent skill

The shipped skill source is
[skills/xlfp/SKILL.md](../skills/xlfp/SKILL.md). Keep its license
frontmatter in sync with [LICENSE](../LICENSE) (enforced by
`TestSkillFile`), and validate with:

```bash
muse skills validate skills/xlfp --json
```
