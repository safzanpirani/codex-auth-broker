# Agent Notes

## Purpose

This is the public `codex-auth-broker` repo.

The first-class feature is Factory Droid support: expose an OpenAI-compatible
`/v1/responses` endpoint backed by the user's local Codex app-server/Codex
OAuth login, while keeping the real refresh token local.

## Safety Rules

- Never commit `~/.codex/auth.json`.
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
- Dashboard request history must stay in memory only. Do not persist prompt
  text, completion text, request bodies, bearer keys, access tokens, or refresh
  tokens.
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
