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
  - `GET /v1/responses` (Responses WebSocket upgrade)
  - `POST /v1/responses`
  - `GET` / `POST /v1/codex/responses` (Pi Codex transport alias)
- Supports Responses-over-WebSocket, HTTP SSE streaming, and non-streaming
  Responses clients.
- Normalizes Factory model names like `gpt-5.5(medium)`.
- Preserves or injects `prompt_cache_key` for model-side prompt caching.
- Strips OpenAI SDK compatibility fields that the Codex backend rejects.
- Shows a local redacted dashboard with request history and live Codex usage.
- Optionally pools several Codex accounts and fails over when one hits a rolling
  usage limit (the ~5-hour or weekly window). See [Multi-Account Failover](#multi-account-failover).
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
gpt-5.6-sol(max)
gpt-5.4
gpt-5.4-mini
gpt-5.3-codex
```

Effort suffixes accept `low`/`medium`/`high`/`xhigh`, plus `max` on the
gpt-5.6 family (`ultra` is accepted as an alias and forwarded as `max`).

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

By default `/v1/models` proxies the live Codex model catalog (each usable slug
plus its `slug(effort)` reasoning variants) fetched with the broker's stored
auth, so new models appear automatically as the Codex backend adds them. It
returns `502` if that upstream fetch fails. Set `--models` /
`CODEX_AUTH_BROKER_MODELS` to serve a fixed list instead.

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

For copy-paste examples covering model ids, reasoning levels, streaming, image
input, and custom provider configuration, see
[`docs/responses-api.md`](docs/responses-api.md).

Responses WebSocket clients can use the same base URL and bearer key. The
broker implements the `responses_websockets=2026-02-06` protocol, forwards
Codex turn-state/model handshake headers, and applies the same model and request
normalization as the HTTP endpoint. See [`docs/api.md`](docs/api.md#responses-websocket).

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

The public OpenAI Responses API exposes cache-retention controls. The ChatGPT
Codex OAuth endpoint used by this broker is a different backend and currently
rejects both the legacy `prompt_cache_retention` field and the newer
`prompt_cache_options` object. The broker therefore strips those controls and
relies on `prompt_cache_key` for ordinary cache affinity. It cannot force a
24-hour cache lifetime for ChatGPT-plan traffic.

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
- A per-request estimated cost column plus an aggregate cost KPI. Costs are
  API-equivalent estimates from the built-in pricing table (cached input is
  priced at the discounted rate); ChatGPT-plan traffic is not actually billed
  per token. Override prices with `CODEX_AUTH_BROKER_PRICING`, for example
  `{"gpt-5.5":{"input":5,"cached_input":0.5,"output":30}}` (USD per 1M tokens).
- Filtering, pause/resume, manual refresh, and clear-history controls.

The in-memory request log is bounded by `--request-log-limit`. Request
metadata is also appended as JSONL to `--request-log-file`
(default `~/.codex-auth-broker/requests.jsonl`, mode 0600; pass an empty value
to disable). On startup the broker reloads the tail of that file so dashboard
history survives restarts; the clear-history button only clears memory.
Neither store ever contains prompt bodies, completion text, bearer tokens,
access tokens, or refresh tokens.

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

Pi can use HTTP, WebSocket, or cached WebSocket transport. For WebSocket support,
configure the provider with `api: "openai-codex-responses"`; Pi then uses the
broker's `/v1/codex/responses` compatibility alias. Set `transport` to
`"websocket-cached"` in `~/.pi/agent/settings.json` to reuse a socket and send
only newly added conversation items after the first turn.

Pi's Codex adapter requires its client credential to look like a JWT, even when
the broker has client authentication disabled. See the complete configuration,
credential-generation, Tailscale, verification, and troubleshooting guide in
[`docs/pi-coding-agent.md`](docs/pi-coding-agent.md).

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
| `--auth-files` | `CODEX_AUTH_FILES` | empty; comma-separated pool for [multi-account failover](#multi-account-failover) (overrides `--auth-file`) |
| `--api-key` | `CODEX_AUTH_BROKER_API_KEY` | empty |
| `--api-key-file` | `CODEX_AUTH_BROKER_API_KEY_FILE` | empty |
| `--prompt-cache-key` | `CODEX_AUTH_BROKER_PROMPT_CACHE_KEY` | `factory-droid` |
| `--prompt-cache-retention` | `CODEX_AUTH_BROKER_PROMPT_CACHE_RETENTION` | records legacy client intent for compatibility; never forwarded |
| `--usage-url` | `CODEX_AUTH_BROKER_USAGE_URL` | ChatGPT wham usage endpoint |
| `--models-url` | `CODEX_AUTH_BROKER_MODELS_URL` | ChatGPT Codex models endpoint |
| n/a | `CODEX_AUTH_BROKER_MODELS_CLIENT_VERSION` | `2.0.0` (`client_version` sent to the Codex models endpoint) |
| `--request-log-limit` | `CODEX_AUTH_BROKER_REQUEST_LOG_LIMIT` | `1000` |
| `--request-log-file` | `CODEX_AUTH_BROKER_REQUEST_LOG_FILE` | `~/.codex-auth-broker/requests.jsonl` (empty disables) |
| n/a | `CODEX_AUTH_BROKER_PRICING` | built-in per-model USD/1M-token table |
| `--models` | `CODEX_AUTH_BROKER_MODELS` | empty; proxies the live Codex model list |
| `--refresh-skew` | `CODEX_AUTH_BROKER_REFRESH_SKEW` | `10m` |
| `--timeout` | none | `10m` |

## Multi-Account Failover

Codex enforces rolling usage windows (roughly a 5-hour bucket and a weekly
bucket). When one is exhausted the backend returns `429`. With a single account
that stalls the broker until the window resets. Point the broker at several
Codex logins and it rotates past a rate-limited account automatically.

Enable it by listing more than one auth file — a login the broker owns and
refreshes, exactly like the single-account case, just more than one:

```bash
./codex-auth-broker serve --auth-files ~/.codex/auth.json,~/.codex-2/auth.json
# or: CODEX_AUTH_FILES=~/.codex/auth.json,~/.codex-2/auth.json ./codex-auth-broker serve
```

Each account is a separate Codex login in its own `CODEX_HOME`:

```bash
CODEX_HOME=~/.codex   codex login   # account 1 (the default home)
mkdir -p ~/.codex-2
CODEX_HOME=~/.codex-2 codex login   # account 2, a different Codex account
```

`--auth-files` overrides `--auth-file`; with a single entry it behaves exactly
like `--auth-file`, so existing setups need no change.

**How it picks an account.** Selection is *sticky*: requests stay on the active
account (so its backend prompt cache stays warm) until that account returns a
`429`. The broker then benches it until its window resets and rotates to the
next available account, retrying the same request transparently — the client
sees no error. Order in the list is the failover order.

**When it comes back.** The bench deadline prefers an explicit reset from the
`429` (a `Retry-After` header, a rate-limit reset header, or a machine-readable
body field such as `resets_in_seconds` / `resets_at`). If none is present it
falls back on the response wording — "weekly" → 7 days, a usage/5-hour limit →
5 hours, otherwise 60 seconds — clamped to `[30s, 8d]`.

**When every account is cooling down.** The broker returns `429` with a
`Retry-After` header pointing at the soonest reset across the pool.

For WebSockets, account selection is pinned for the life of a connection. A
`429` during the opening handshake rotates transparently. A `429` event after
the socket is established is forwarded to the client, the account is cooled,
and the socket is closed so a reconnect can select the next account; an
in-flight turn is never replayed automatically across accounts.

**Observability.** `/healthz` lists each account with its availability and
cooldown; `doctor --auth-files ...` validates every login; and each rotation
logs a line like:

```text
codex account .codex-2 hit rate limit window=5h source=retry-after cooling_until=2026-07-09T15:30:00Z; rotating (1/2)
```

Full runbook, systemd `EnvironmentFile` pattern, and troubleshooting:
[`docs/multi-account.md`](docs/multi-account.md).

> Pooling multiple accounts to extend usage limits is account-multiplexing that
> OpenAI's terms discourage. This is an operational feature; use it within the
> terms that apply to you.

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
- Responses WebSocket connections inherit the upstream 60-minute connection
  limit. Clients must reconnect after `websocket_connection_limit_reached`.
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
