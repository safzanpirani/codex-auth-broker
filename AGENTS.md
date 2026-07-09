# Agent Notes

## Purpose

This is the public `codex-auth-broker` repo.

The first-class feature is Factory Droid support: expose an OpenAI-compatible
`/v1/responses` endpoint backed by the user's local Codex app-server/Codex
OAuth login, while keeping the real refresh token local.

## Safety Rules

- Never commit `~/.codex/auth.json` or any pooled auth file (`~/.codex-2/...` etc.).
- Multi-account failover pools several Codex logins via `--auth-files` /
  `CODEX_AUTH_FILES`; single `--auth-file` stays fully backward compatible. The
  broker is the sole owner of every pooled token's refresh — never run a second
  refresher (another broker, or the Codex CLI in normal use) against a pooled
  `auth.json`. Keep the README "Multi-Account Failover" section and
  `docs/multi-account.md` in sync with `accounts.go`.
- Never print or commit access tokens, refresh tokens, id tokens, API keys, or
  generated client keys.
- Do not add public-internet bind examples without strong warnings.
- Keep Factory Droid as the primary README flow.
- Keep Linux support first-class: systemd user service docs and examples should
  stay current.
- Factory Droid 0.122 sends extra OpenAI SDK fields; keep
  `prompt_cache_retention`, `stream_options`, `user`, and max-token aliases
  stripped before forwarding to the Codex backend.
- The dashboard is intentionally served by this Go binary. Keep it dependency
  free unless there is a strong reason to add a frontend build step.
- Dashboard request history may be persisted as metadata-only JSONL (see
  `--request-log-file`). Never persist or log prompt text, completion text,
  request bodies, bearer keys, access tokens, or refresh tokens — in memory or
  on disk. Anything written to the persistent log must be a `requestLogEntry`.
- Per-request cost is an API-equivalent estimate from the pricing table in
  `pricing.go` (overridable via `CODEX_AUTH_BROKER_PRICING`); ChatGPT-plan
  traffic is not actually billed per token. Keep the table in sync with the
  advertised model set.
- Live Codex usage comes from `GET https://chatgpt.com/backend-api/wham/usage`
  using the local access token and `ChatGPT-Account-Id` header when present.
- Keep the advertised model set, README model examples, and Factory/Pi docs in
  sync. The current primary set is `gpt-5.5`, `gpt-5.4`, `gpt-5.4-mini`, and
  `gpt-5.3-codex`, with reasoning-effort suffixes where useful.

## Development

Use Go standard library by default.

Before committing:

```bash
gofmt -w *.go
go test ./...
go build -o codex-auth-broker .
```

Do not stage the built binary.
