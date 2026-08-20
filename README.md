# codex-auth-broker

Codex app-server powered auth bridge for Pi, Factory Droid, and tools that
talk to the OpenAI Responses or Chat Completions APIs.

The main use case is simple: log into Codex on one trusted machine, run this
broker there, and point Pi or Factory Droid at `http://127.0.0.1:8317/v1` or a
private Tailscale address. Your client gets `/v1/responses`; your real Codex
OAuth refresh token stays on the machine that owns the login.

Run it on private networks (localhost, Tailscale, a VPC subnet) with named
client keys (`--keys-file`) or at least a single `--api-key`. Even with keys
configured, never expose it on the public internet — it fronts a personal
Codex account.

## What It Does

- Reads the existing Codex CLI auth file, usually `~/.codex/auth.json`.
- Refreshes the Codex access token locally when needed.
- Calls the ChatGPT Codex Responses backend with access-only auth.
- Exposes:
  - `GET /healthz`
  - `GET /dashboard`
  - `GET /dashboard/api/usage`
  - `GET /dashboard/api/usage/by-user`
  - `GET /dashboard/api/requests`
  - `GET /dashboard/api/costs`
  - `GET /v1/models`
  - `GET /v1/responses` (Responses WebSocket upgrade)
  - `POST /v1/responses`
  - `GET` / `POST /v1/codex/responses` (Pi Codex transport alias)
  - `POST /v1/chat/completions`
- Supports Responses-over-WebSocket, HTTP SSE streaming, and non-streaming
  Responses clients.
- Translates Chat Completions messages, function tools, structured output,
  final responses, and streaming chunks over the same Responses backend.
- Normalizes Factory model names like `gpt-5.5(medium)`.
- Preserves or injects `prompt_cache_key` for model-side prompt caching.
- Strips OpenAI SDK compatibility fields that the Codex backend rejects.
- Shows a local redacted dashboard with request history and live Codex usage.
- Optionally pools several Codex accounts and fails over when one hits a rolling
  usage limit (the ~5-hour or weekly window). See [Multi-Account Failover](#multi-account-failover).
- Never returns a refresh token to Pi, Factory Droid, or remote clients.

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

If you started the broker with `--api-key`, `--api-key-file`, or `--keys-file`,
the dashboard requires an admin key: open
`http://127.0.0.1:8317/dashboard?key=<admin key>` once (the broker exchanges it
for an HttpOnly cookie and redirects), or enter the key in the dashboard's key
field (kept in browser session storage and sent as a bearer token).

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

Chat Completions call:

```bash
curl -sS http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5(low)",
    "messages": [{"role": "user", "content": "Reply exactly: CHAT_OK"}],
    "stream": false,
    "prompt_cache_key": "my-project"
  }'
```

See [`docs/chat-completions.md`](docs/chat-completions.md) for streaming,
function calling, cache behavior, and current compatibility boundaries.

Native Codex compaction passes through: send a `compaction_trigger` input item
and the broker forwards the `remote_compaction_v2` gate and relays the encrypted
checkpoint back untouched. The client owns checkpoint storage and replay — see
[`docs/compaction.md`](docs/compaction.md).

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

The key is resolved in this order:

1. `prompt_cache_key` sent by the client.
2. A conversation-stable id derived from the request — `session_id`,
   `conversation_id` (either casing) in the body, or a `session_id` header.
   Per-request ids such as `x-request-id` are never used: they rotate every call,
   which scopes the cache to a single request and gives zero reuse.
3. The configured constant, unset by default.

Step 2 outranks step 3 on purpose. `prompt_cache_key` drives the backend's cache
routing affinity, so one constant shared by every client and every conversation
puts them all in a single bucket where unrelated long transcripts evict each
other and only the common system+tools prefix stays hot. The constant is the
last-resort slot for clients that expose no session identity at all, and it is
unset by default: with no key the backend hashes the prefix unscoped, which is
strictly better than a colliding one. Set `--prompt-cache-key <value>` only when
a fleet of otherwise-anonymous clients really should share one cache bucket.

The public OpenAI Responses API exposes cache-retention controls. The ChatGPT
Codex OAuth endpoint used by this broker applies its cache policy server-side
and rejects both the legacy `prompt_cache_retention` field and the newer
`prompt_cache_options` object. The broker therefore strips those controls and
preserves `prompt_cache_key`.

This does not disable extended caching. OpenAI documents GPT-5.5 and GPT-5.4 as
supporting extended prompt retention for up to 24 hours; GPT-5.6 instead uses a
30-minute minimum lifetime and may retain entries longer. The exact retention
policy of ChatGPT-plan Codex traffic is not exposed in the response, so verify
actual reuse with `usage.input_tokens_details.cached_tokens` on Responses or
`usage.prompt_tokens_details.cached_tokens` on Chat Completions.

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

Chat Completions returns the equivalent signal under
`usage.prompt_tokens_details`. When upstream reports GPT-5.6 cache writes, the
broker also preserves `cache_write_tokens` in that object and in redacted
request metadata.

OpenAI prompt-caching docs:

```text
https://platform.openai.com/docs/guides/prompt-caching
```

## Dashboard

The dashboard is served by the same Go process at `/dashboard`. It is intended
for local debugging while Pi, Factory Droid, or another Responses client is
pointed at the broker.

It shows:

- Live Codex usage from `https://chatgpt.com/backend-api/wham/usage`.
- Primary and secondary usage windows, including reset countdowns.
- Redacted request history for `/v1/models`, `/v1/responses`, and
  `/v1/chat/completions` calls.
- Status, model normalization, reasoning effort, streaming mode, duration,
  cached tokens, and total tokens. Streaming calls are scanned as they pass
  through so final usage is captured when the upstream SSE includes it.
- A per-request estimated cost column plus an aggregate cost KPI. Costs are
  API-equivalent estimates from the built-in pricing table (cached input is
  priced at the discounted rate); ChatGPT-plan traffic is not actually billed
  per token. Override prices with `CODEX_AUTH_BROKER_PRICING`, for example
  `{"gpt-5.5":{"input":5,"cached_input":0.5,"cache_write":5,"output":30}}`
  (USD per 1M tokens). GPT-5.6 defaults price reported cache writes at 1.25x
  uncached input, matching the public API-equivalent rate.
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
GET    /dashboard/api/usage/by-user?window=7d
GET    /dashboard/api/requests?limit=250
DELETE /dashboard/api/requests
GET    /dashboard/api/costs
```

When any client key is configured (`--api-key`, `--api-key-file`, or
`--keys-file`), every `/dashboard*` route — the HTML page and the data APIs —
requires a key with role `admin` (the legacy single `--api-key` counts as
admin). Two ways in:

- `Authorization: Bearer <admin key>` on each request (what the dashboard's
  key field and curl use).
- Visit `/dashboard?key=<admin key>` in a browser once: the broker validates
  the key, sets it as an HttpOnly cookie, and redirects to `/dashboard`, so
  the key does not linger in the address bar and the page's own API fetches
  are authenticated by the cookie.

With no keys configured at all the dashboard stays open, as before. `/healthz`
is always unauthenticated so process supervisors can probe it.

`GET /dashboard/api/usage/by-user` aggregates the retained request log per
`(user, model)`: request count, input/output/cached/cache-write tokens, and
estimated cost. `?window=` accepts `24h`, `7d`, `30d`, `all` (default), or any
Go duration, filtering by request start time. It reads the persisted JSONL log
when `--request-log-file` is enabled (full history), else the in-memory ring.

## Named Client Keys

`--keys-file` (or `CODEX_AUTH_BROKER_KEYS_FILE`) points at a JSON array of
named bearer keys, so each consumer gets its own rotatable credential:

```json
[
  { "name": "buildr-backend", "key": "<random>", "role": "client" },
  { "name": "buildr-sandbox", "key": "<random>", "role": "client" },
  { "name": "safzan-dev",     "key": "<random>", "role": "admin" },
  { "name": "old-shared",     "key": "<random>", "role": "client", "disabled": true }
]
```

- `role` is `"admin"` (full access including `/dashboard*`) or `"client"`
  (`/v1/*` only); it defaults to `client` when omitted. Names must be unique.
- The file is reloaded automatically when its mtime or size changes (checked
  at most every couple of seconds), so rotation is: add the new key, flip the
  consumer, mark the old entry `"disabled": true` — no restart. An invalid
  rewrite is rejected with a log line and the previous key set stays active.
- Every request is checked against all enabled keys with a constant-time
  compare per entry; on match the request is attributed to the entry's `name`,
  which appears as `client_name` in the request log and dashboard.
- The legacy single `--api-key` keeps working alongside (or instead of) the
  keys file as an implicit client named `default` with role `admin`, so
  existing single-key setups keep their dashboard access unchanged.

## Per-User Attribution

Clients may attribute each `/v1/*` request to an end user by sending
`X-Broker-User: <identity>` (e.g. a Google-SSO email). Fallback for clients
that cannot set headers: a `prompt_cache_key` of the form `user:<identity>` is
parsed the same way (the header wins when both are present). The value is
trimmed, stripped of control characters, and capped at 128 characters; the
broker trusts it only because the request already carried a valid client key —
clients own its truthfulness. The user lands in the request log (`user` field)
next to the token usage and estimated cost, and is aggregated by
`GET /dashboard/api/usage/by-user`.

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
| `--keys-file` | `CODEX_AUTH_BROKER_KEYS_FILE` | empty; JSON array of named keys, reloaded on change (see [Named Client Keys](#named-client-keys)) |
| `--prompt-cache-key` | `CODEX_AUTH_BROKER_PROMPT_CACHE_KEY` | _(unset)_ |
| `--prompt-cache-retention` | `CODEX_AUTH_BROKER_PROMPT_CACHE_RETENTION` | records legacy client intent for compatibility; never forwarded |
| `--usage-url` | `CODEX_AUTH_BROKER_USAGE_URL` | ChatGPT wham usage endpoint |
| `--models-url` | `CODEX_AUTH_BROKER_MODELS_URL` | ChatGPT Codex models endpoint |
| n/a | `CODEX_AUTH_BROKER_MODELS_CLIENT_VERSION` | `2.0.0` (`client_version` sent to the Codex models endpoint) |
| `--max-concurrent` | `CODEX_AUTH_BROKER_MAX_CONCURRENT` | `8`; cap on simultaneous upstream Codex calls, `0` = unlimited (see [Concurrency Cap](#concurrency-cap)) |
| `--request-log-limit` | `CODEX_AUTH_BROKER_REQUEST_LOG_LIMIT` | `1000` |
| `--request-log-file` | `CODEX_AUTH_BROKER_REQUEST_LOG_FILE` | `~/.codex-auth-broker/requests.jsonl` (empty disables) |
| n/a | `CODEX_AUTH_BROKER_PRICING` | built-in per-model USD/1M-token table |
| `--models` | `CODEX_AUTH_BROKER_MODELS` | empty; proxies the live Codex model list |
| `--refresh-skew` | `CODEX_AUTH_BROKER_REFRESH_SKEW` | `10m` |
| `--timeout` | none | `10m` |

## Concurrency Cap

The broker holds a global semaphore around upstream Codex calls: HTTP
Responses and Chat Completions dispatches, and Responses WebSocket sessions.
`--max-concurrent` / `CODEX_AUTH_BROKER_MAX_CONCURRENT` sets the cap
(default `8`; `0` disables it).

- A slot is held for the full duration of the upstream work — a streaming
  response occupies its slot until the stream ends, and a WebSocket session
  occupies one slot from handshake to close.
- When every slot is busy, new requests queue for up to 120 seconds rather
  than failing immediately. Queued requests respect client disconnects.
- If the queue wait expires the broker returns `429` with a `Retry-After`
  header and the standard error envelope.
- `GET /healthz` reports the live state under `concurrency`:
  `{"max_concurrent": 8, "in_flight": 2, "queued": 0}`.

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
only model responses from `/v1/responses` or `/v1/chat/completions`; they do
not receive access tokens, refresh tokens, or the auth file.

If you bind to anything other than localhost, configure keys — preferably
per-consumer named keys via `--keys-file`, or at least a single `--api-key` /
`--api-key-file` — and keep the broker on a private network (Tailscale, a VPC
security group). Named keys make a leak recoverable: disable the one leaked
entry instead of rotating a shared secret everywhere. Even so, never expose
the broker on the public internet.

When client keys are configured, every `/v1/*` endpoint — including the
`/v1/codex/responses` aliases and Responses WebSocket upgrades — requires
`Authorization: Bearer <key>` matching any enabled key, and every
`/dashboard*` route (HTML page and data APIs, plus `/usage`) requires a key
with role `admin` via bearer or the dashboard cookie. Each presented key is
checked against every configured entry with a constant-time compare; keys are
never logged or persisted, and the request log stores only the matching
entry's name. `/healthz` stays unauthenticated so process supervisors can
probe it.

## Limitations

- Chat Completions currently supports one choice (`n: 1`), text/image/file
  input, function tools, and text output. Audio and custom tools are rejected.
- Chat max-token aliases are accepted for SDK compatibility but stripped
  because the Codex backend rejects them.
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

### Standalone Search Passthrough

`POST /v1/alpha/search` proxies the Codex standalone search backend (`web.run`
without a GPT inference turn) at `https://chatgpt.com/backend-api/codex/alpha/search`,
injecting the stored OAuth access token and account id. Gated by the same API key
and concurrency semaphore as `/v1/responses`. Override the upstream with
`--alpha-search-url` / `CODEX_AUTH_BROKER_ALPHA_SEARCH_URL`. One command action
type per request; upstream Cloudflare 403s are passed through.
