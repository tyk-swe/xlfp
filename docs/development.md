# Development

Requirements: Go 1.23+. There are no Go module dependencies.

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
  tie/inconclusive rules, stable reason codes.
- `pkg/probe` — the three transports and the staged, capped probe
  runner (with an `OnProbe` progress callback).
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

## Agent skill

The shipped skill source is
[skills/xlfp/SKILL.md](../skills/xlfp/SKILL.md). Keep its license
frontmatter in sync with [LICENSE](../LICENSE) (enforced by
`TestSkillFile`), and validate with:

```bash
muse skills validate skills/xlfp --json
```
