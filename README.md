# codex-auth-broker

Codex app-server powered auth bridge for Factory Droid and any tool that can
talk to an OpenAI-compatible Responses API.

The main use case is simple: log into Codex on one trusted machine, run this
broker there, and point Factory Droid at `http://127.0.0.1:8317/v1` or a
private Tailscale address. Factory gets `/v1/responses`; your real Codex OAuth
refresh token stays on the machine that owns the login.

This is for personal/local infrastructure. Do not expose it on the public
internet.

## What It Does

- Reads the existing Codex CLI auth file, usually `~/.codex/auth.json`.
- Refreshes the Codex access token locally when needed.
- Calls the ChatGPT Codex Responses backend with access-only auth.
- Exposes:
  - `GET /healthz`
  - `GET /dashboard`
  - `GET /dashboard/api/usage`
  - `GET /dashboard/api/requests`
  - `GET /v1/models`
  - `POST /v1/responses`
- Supports streaming and non-streaming Responses clients.
- Normalizes Factory model names like `gpt-5.5(medium)`.
- Preserves or injects `prompt_cache_key` for model-side prompt caching.
- Strips OpenAI SDK compatibility fields that the Codex backend rejects.
- Shows a local redacted dashboard with request history and live Codex usage.
- Never returns a refresh token to Factory Droid or remote clients.

## Why This Exists

Factory Droid custom models can point at an OpenAI-compatible base URL. Codex
subscriptions are not normal OpenAI API keys, though: Codex uses ChatGPT/Codex
OAuth and short-lived access tokens.

Copying `~/.codex/auth.json` to another machine is fragile because refresh
tokens can rotate. This broker keeps refresh-token ownership on one trusted
machine and exposes only the API surface Factory needs.

## Quick Start

1. Log into Codex on the machine that will run the broker:

```bash
codex login
codex login status
```

2. Build and run:

```bash
go build -o codex-auth-broker .
./codex-auth-broker serve --listen 127.0.0.1:8317
```

3. Point Factory Droid custom model base URL at:

```text
http://127.0.0.1:8317/v1
```

4. Use a Codex model in Factory:

```text
gpt-5.5(low)
gpt-5.5(medium)
gpt-5.5(high)
gpt-5.5(xhigh)
gpt-5.4
gpt-5.4-mini
gpt-5.3-codex
```

The API key can be any dummy value unless you start the broker with
`--api-key`.

5. Open the local dashboard:

```text
http://127.0.0.1:8317/dashboard
```

If you started the broker with `--api-key` or `--api-key-file`, enter the same
client key in the dashboard. The key is kept in browser session storage.

## Verify

Health:

```bash
curl -fsS http://127.0.0.1:8317/healthz
```

Models:

```bash
curl -fsS http://127.0.0.1:8317/v1/models
```

Real Responses call:

```bash
curl -sS http://127.0.0.1:8317/v1/responses \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5(low)",
    "input": "Reply exactly: CODEX_AUTH_BROKER_OK",
    "stream": false
  }'
```

Doctor:

```bash
./codex-auth-broker doctor
```

`doctor` prints redacted auth status only. It does not print tokens.

## Prompt Caching

This project does not cache generated text. It helps model-side prompt caching
work by preserving a stable `prompt_cache_key` or injecting one when the client
does not provide it.

By default:

```text
prompt_cache_key = factory-droid
```

Factory Droid `0.122` can send `prompt_cache_retention: "24h"` through the
OpenAI SDK path. The Codex backend currently rejects that request field, so the
proxy strips it and relies on `prompt_cache_key` for cache affinity.

Cache hits are visible in Responses usage as:

```json
{
  "usage": {
    "input_tokens_details": {
      "cached_tokens": 24832
    }
  }
}
```

OpenAI prompt-caching docs:

```text
https://platform.openai.com/docs/guides/prompt-caching
```

## Dashboard

The dashboard is served by the same Go process at `/dashboard`. It is intended
for local debugging while Factory Droid or another Responses client is pointed
at the broker.

It shows:

- Live Codex usage from `https://chatgpt.com/backend-api/wham/usage`.
- Primary and secondary usage windows, including reset countdowns.
- Redacted request history for `/v1/models`, `/v1/responses`, and unsupported
  `/v1/chat/completions` calls.
- Status, model normalization, reasoning effort, streaming mode, duration,
  cached tokens, and total tokens. Streaming calls are scanned as they pass
  through so final usage is captured when the upstream SSE includes it.
- Filtering, pause/resume, manual refresh, and clear-history controls.

The request log is in memory only and bounded by `--request-log-limit`.
It never stores prompt bodies, completion text, bearer tokens, access tokens, or
refresh tokens.

Dashboard endpoints:

```text
GET    /dashboard
GET    /dashboard/api/usage
GET    /dashboard/api/requests?limit=250
DELETE /dashboard/api/requests
```

When `--api-key` is configured, the dashboard API endpoints require the same
`Authorization: Bearer ...` key as `/v1/responses`. `/dashboard` itself serves
static HTML so the browser can load the page before you enter the key.

## Pi Coding Agent

Pi can use this as a custom `openai-responses` provider. Configure the provider
with `baseUrl: "http://127.0.0.1:8317/v1"`, `api: "openai-responses"`, and
`authHeader: true`.

Recommended model ids:

```text
gpt-5.5
gpt-5.4
gpt-5.4-mini
gpt-5.3-codex
```

Declare `reasoning: true` so Pi's thinking-level control maps to
`reasoning.effort`, and declare `input: ["text", "image"]` for multimodal
requests. Cost metadata should use the equivalent OpenAI API per-million-token
prices for reporting, even though traffic through this broker uses Codex OAuth
instead of an OpenAI API billing key.

## Factory Droid Over Tailscale

Run the broker on the machine that owns the Codex login:

```bash
./codex-auth-broker serve \
  --listen 100.x.y.z:8317 \
  --api-key-file ~/.codex-auth-broker/client.key
```

Factory Droid on another device:

```text
base_url: http://100.x.y.z:8317/v1
api_key: contents of ~/.codex-auth-broker/client.key
```

Use a private network such as Tailscale. Avoid public binds.

## Linux Systemd

Install the binary somewhere stable:

```bash
sudo install -m 0755 codex-auth-broker /usr/local/bin/codex-auth-broker
```

Create a client key:

```bash
mkdir -p ~/.codex-auth-broker
openssl rand -hex 32 > ~/.codex-auth-broker/client.key
chmod 600 ~/.codex-auth-broker/client.key
```

Install the user service:

```bash
mkdir -p ~/.config/systemd/user
cp packaging/systemd/codex-auth-broker.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now codex-auth-broker.service
```

More detail: `docs/linux-systemd.md`.

## Configuration

Flags and equivalent environment variables:

| Flag | Environment | Default |
| --- | --- | --- |
| `--listen` | `CODEX_AUTH_BROKER_LISTEN` | `127.0.0.1:8317` |
| `--auth-file` | `CODEX_AUTH_FILE` | `~/.codex/auth.json` |
| `--api-key` | `CODEX_AUTH_BROKER_API_KEY` | empty |
| `--api-key-file` | `CODEX_AUTH_BROKER_API_KEY_FILE` | empty |
| `--prompt-cache-key` | `CODEX_AUTH_BROKER_PROMPT_CACHE_KEY` | `factory-droid` |
| `--prompt-cache-retention` | `CODEX_AUTH_BROKER_PROMPT_CACHE_RETENTION` | accepted for compatibility but not forwarded |
| `--usage-url` | `CODEX_AUTH_BROKER_USAGE_URL` | ChatGPT wham usage endpoint |
| `--request-log-limit` | `CODEX_AUTH_BROKER_REQUEST_LOG_LIMIT` | `1000` |
| `--models` | `CODEX_AUTH_BROKER_MODELS` | built-in GPT list |
| `--refresh-skew` | `CODEX_AUTH_BROKER_REFRESH_SKEW` | `10m` |
| `--timeout` | none | `10m` |

## Security Model

The key invariant:

```text
Remote clients must not receive the Codex OAuth refresh token.
```

The broker reads and refreshes `~/.codex/auth.json` locally. Clients receive
only model responses from `/v1/responses`; they do not receive access tokens,
refresh tokens, or the auth file.

If you bind to anything other than localhost, set `--api-key` or
`--api-key-file` and use a private network.

## Limitations

- `/v1/chat/completions` intentionally returns HTTP 501 for now.
- This is not a full OpenAI API clone.
- The Codex backend is not a public stability contract; compatibility can
  change when Codex changes.
- This project is not affiliated with OpenAI.

## Development

```bash
gofmt -w *.go
go test ./...
go build -o codex-auth-broker .
```
