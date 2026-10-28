# Contributing

## Development

```bash
go vet ./...
go test -race -count=1 ./...
go build -o xlfp .
```

The terminal dashboard uses pinned Go module dependencies. Keep new
dependencies focused and justified, and update the bundled third-party
notices when dependencies change.

## Ground rules

- Behavior and claim limits are documented in
  [docs/how-it-works.md](./docs/how-it-works.md). Changes that widen
  a claim (exact checkpoints, degradation proof, defeating actively
  fabricated usage) need maintainer agreement first.
- Fingerprint verdicts are only rendered for verified families. Adding
  or promoting a family requires its generating tokenizer source in
  `tools/gen_db.py`, regenerated `pkg/fingerprint/db.json`, and updated
  regression fixtures in `pkg/fingerprint/rank_test.go`.
- Never add a way to pass a literal API key on the command line, and
  never let a key reach logs, output, URLs, or errors. Provider-standard
  ambient keys stay restricted to official provider hosts.
- New behavior needs a failing-then-passing test in the owning package,
  preferably against local mock endpoints. Tests must not require
  network access or real API keys.

## Regenerating the fingerprint database

```bash
python3 -m venv .venv
.venv/bin/pip install -r tools/requirements.txt
.venv/bin/python tools/gen_db.py
```
