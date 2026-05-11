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

## Development

Use Go standard library by default.

Before committing:

```bash
gofmt -w *.go
go test ./...
go build -o codex-auth-broker .
```

Do not stage the built binary.

