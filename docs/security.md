# Security

The broker's core invariant is:

```text
Do not give remote clients the Codex OAuth refresh token.
```

The broker runs on the machine where Codex is already logged in. It reads and
refreshes the local Codex auth file, then forwards model requests to the Codex
Responses backend with short-lived access auth.

Clients only talk to the OpenAI-compatible `/v1/responses` surface. They do not
receive the access token or refresh token.

## Recommendations

- Bind to `127.0.0.1` by default.
- Use Tailscale or another private network for remote Factory Droid.
- Set `--api-key-file` when binding to anything other than localhost.
- Keep `~/.codex/auth.json` mode `0600`.
- Treat the client API key as sensitive.

## Do Not

- Do not expose this directly on the public internet.
- Do not commit `~/.codex/auth.json`.
- Do not paste access-token or refresh-token values in issues.
- Do not run this for other people's accounts as a hosted service.

