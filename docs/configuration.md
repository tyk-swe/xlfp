# Configuration: endpoints, transports, and API keys

## Transports and endpoint forms

Three API shapes are supported. The official Anthropic and Gemini
hosts are auto-detected; everything else defaults to `openai`, so pass
`--transport anthropic` or `--transport gemini` for native APIs behind
other hostnames or gateways:

| Transport | Request shape | Usage field read |
|---|---|---|
| `openai` (compatible) | `POST /v1/chat/completions` | `usage.prompt_tokens` |
| `anthropic` | `POST /v1/messages` | `usage.input_tokens` |
| `gemini` | `POST /v1beta/models/<model>:generateContent` | `usageMetadata.promptTokenCount` |

The endpoint value must include `http://` or `https://` and may be a host URL, a versioned base (`/v1`, `/v2`,
`/v1beta`), any other prefix base such as an Azure deployment URL or
Gemini's `/v1beta/openai`, or a full request URL. A host URL without a path gains
the default version prefix; any non-empty path is treated as a
complete prefix. Query parameters such as Azure's `api-version` are
preserved, while credential query parameters and URL userinfo are
rejected. Qwen, DeepSeek, GLM, Mistral, MiMo and OpenAI endpoints are
normally reached through the `openai` transport. Azure OpenAI hosts
receive the `api-key` header instead of `Authorization`.

## API keys

Keys come from the **environment or a config file only** — there is
no flag that takes a literal key. On conflict the environment wins.

Environment variables:

- `XLFP_API_KEY`, plus `XLFP_TARGET_API_KEY` / `XLFP_OFFICIAL_API_KEY`
  per audit side. These tool-specific variables are the explicit
  opt-in for probing untrusted endpoints.
- Provider-standard variables (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`,
  `GEMINI_API_KEY` / `GOOGLE_API_KEY`) are honored **only when the
  endpoint is HTTPS on that provider's official API host and default port**. `xlfp` exists
  to probe unknown endpoints, so an ambient official key is never
  sent anywhere else. Redirects are never followed, for the same
  reason.
- `--api-key-env NAME` names any variable to read the key from.
  An unset or blank named variable, including a config `api_key_env`,
  fails closed rather than falling back to an unrelated credential.
- Non-secret overrides: `XLFP_ENDPOINT`, `XLFP_MODEL`,
  `XLFP_TRANSPORT`, plus per-side audit forms such as
  `XLFP_TARGET_ENDPOINT` and `XLFP_OFFICIAL_MODEL` (same pattern for
  `ENDPOINT`/`MODEL`/`TRANSPORT`).
- API keys are never sent over cleartext `http` except to exact
  `localhost` or loopback IP addresses. Subdomains such as
  `dev.localhost` require HTTPS when a key is set because HTTP proxies
  can forward them off-machine.
- A literal key or `api_key_env` reference in config is bound to the
  endpoint origin (scheme, host, and effective port) where that credential
  is configured. Profile keys may inherit the default endpoint; inherited
  default keys retain the default origin binding. Configure that endpoint explicitly, or use
  `--api-key-env` / `XLFP_API_KEY` to opt in to another host.

## Config file and profiles

Config file: `--config PATH`, else `$XLFP_CONFIG`, else
`xlfp/config.json` inside the operating system's user config
directory (Linux: `${XDG_CONFIG_HOME:-~/.config}`, macOS:
`~/Library/Application Support`, Windows: `%AppData%`). A config in
the current directory is never auto-loaded; pass it explicitly with
`--config`. Top-level fields are defaults; named profiles override
them:

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
`--target-profile` / `--official-profile` (audit). Pass `--official-url ''`
to disable an `XLFP_OFFICIAL_ENDPOINT` default; leaving the wizard's official
endpoint blank does this automatically. A config file that
stores literal `api_key` values must not be group/world readable —
on Linux and macOS `xlfp` refuses it until you run `chmod 600`
(Windows has no such permission bits, so the check does not apply
there). Keys are never printed,
logged, or included in either output format; endpoints are displayed
with userinfo and query strings stripped.
