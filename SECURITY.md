# Security policy

`xlfp` sends API keys to endpoints its user chooses to probe, so key
handling defects are treated as security bugs.

## Reporting a vulnerability

Please report security issues privately through GitHub's "Report a
vulnerability" flow (Security advisories) on this repository instead of
opening a public issue. Include the affected version (`xlfp version`),
the transport involved, and a reproduction against a local or mock
endpoint. Never include a real API key in a report.

## Scope notes

- Provider-standard ambient keys (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
  `GEMINI_API_KEY`) are only sent to official provider hosts by design.
  A report that they reached any other host is in scope and urgent.
- Fingerprinting accuracy limits (family-level claims, shared
  tokenizers, fabricated provider usage) are documented product limits,
  not vulnerabilities; please use regular issues for those.
