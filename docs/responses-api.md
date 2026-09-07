# Calling The Responses API

`codex-auth-broker` exposes one main model endpoint:

```text
POST /v1/responses
```

Use it like an OpenAI Responses-compatible provider. The broker handles Codex
OAuth locally and forwards the request to the ChatGPT Codex backend.

## Base URL

Local machine:

```text
http://127.0.0.1:8317/v1
```

Current Mac Tailscale IP:

```text
100.121.157.57
```

Remote private-network URL, if the broker is started on the Tailscale
interface:

```text
http://100.121.157.57:8317/v1
```

Keep remote use on a private network such as Tailscale. Do not expose this
broker directly to the public internet.

Current verified personal deployment, as of 2026-05-14:

```text
Responses API:       http://127.0.0.1:8317/v1
Dashboard:           http://127.0.0.1:8317/dashboard
Cursor Agent shim:   http://127.0.0.1:8318
Mac Tailscale IP:    100.121.157.57
Tailscale Responses: not currently listening on 100.121.157.57:8317
```

That means:

- If the coding agent runs on this Mac, use `http://127.0.0.1:8317/v1`.
- If the coding agent runs on another Tailnet machine, first run or configure
  the broker with `--listen 100.121.157.57:8317`, then use
  `http://100.121.157.57:8317/v1`.
- If remote access is enabled, also start the broker with `--api-key-file` and
  give the remote agent only that client key, not any Codex OAuth file.

To intentionally enable remote Tailnet access:

```bash
mkdir -p ~/.codex-auth-broker
test -s ~/.codex-auth-broker/client.key || \
  (umask 077 && openssl rand -hex 32 > ~/.codex-auth-broker/client.key)

./codex-auth-broker serve \
  --listen 100.121.157.57:8317 \
  --api-key-file ~/.codex-auth-broker/client.key
```

Remote health check from another Tailnet machine:

```bash
curl -fsS http://100.121.157.57:8317/healthz
```

Remote model check:

```bash
BROKER_KEY="paste-client-key-here"
curl -fsS http://100.121.157.57:8317/v1/models \
  -H "Authorization: Bearer $BROKER_KEY"
```

## API Key

If the broker was started without `--api-key` or `--api-key-file`, any dummy
client key works:

```text
Authorization: Bearer dummy
```

If the broker was started with a client API key, use that value as the bearer
token. This is a local broker key. It is not an OpenAI API key and is not sent
upstream as the Codex OAuth credential.

## List Models

```bash
curl -fsS http://127.0.0.1:8317/v1/models
```

Common model ids:

```text
gpt-6-astra
gpt-6-astra(max)
gpt-5.5
gpt-5.5(low)
gpt-5.5(medium)
gpt-5.5(high)
gpt-5.5(xhigh)
gpt-5.6-sol(max)
gpt-5.4
gpt-5.4-mini
gpt-5.3-codex
```

The suffix form is a convenience for clients that cannot send
`reasoning.effort` directly. For example, `gpt-5.5(high)` is forwarded as:

```json
{
  "model": "gpt-5.5",
  "reasoning": {
    "effort": "high"
  }
}
```

To run without explicit reasoning effort, use `gpt-5.5` and omit `reasoning`.

## Basic Text Call

```bash
curl -sS http://127.0.0.1:8317/v1/responses \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5",
    "input": "Reply exactly: BROKER_OK",
    "stream": false
  }'
```

Extract just the text:

```bash
curl -sS http://127.0.0.1:8317/v1/responses \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5",
    "input": "Reply exactly: BROKER_OK",
    "stream": false
  }' |
  node -e 'let s=""; process.stdin.on("data", d=>s+=d); process.stdin.on("end",()=>{const r=JSON.parse(s); console.log((r.output||[]).flatMap(o=>o.content||[]).map(c=>c.text||"").join(""))})'
```

## Copy-Paste Agent Instructions

Give this block to a coding agent running on the same Mac:

```text
Use codex-auth-broker as an OpenAI Responses-compatible provider.

Base URL: http://127.0.0.1:8317/v1
Responses endpoint: POST http://127.0.0.1:8317/v1/responses
Models endpoint: GET http://127.0.0.1:8317/v1/models
Dashboard: http://127.0.0.1:8317/dashboard
API key: dummy, unless the broker owner gives you a real local broker key
Primary model: gpt-6-astra
Reasoning: omit reasoning for off/default, or send reasoning.effort low/medium/high/xhigh (GPT-6 Astra and gpt-5.6 also accept max)
Prompt cache key: use a stable project key, for example "safzan-coding-agent"

Use /v1/responses for this provider configuration. The broker also supports
/v1/chat/completions for clients that require the Chat Completions protocol.
Do not ask for, read, copy, or store ~/.codex/auth.json.
Do not handle Codex refresh tokens. The broker owns OAuth refresh locally.
```

Give this block to a coding agent running on another Tailnet machine only after
the broker has been rebound to the Tailscale interface:

```text
Use codex-auth-broker as an OpenAI Responses-compatible provider over Tailscale.

Base URL: http://100.121.157.57:8317/v1
Responses endpoint: POST http://100.121.157.57:8317/v1/responses
Models endpoint: GET http://100.121.157.57:8317/v1/models
Dashboard: http://100.121.157.57:8317/dashboard
API key: use the local broker bearer key provided by the owner
Primary model: gpt-5.5
Prompt cache key: use a stable project key, for example "safzan-coding-agent"

Do not use this over the public internet.
Prefer /v1/responses for this agent. Use /v1/chat/completions only when the
client does not support Responses.
Do not ask for, read, copy, or store ~/.codex/auth.json.
```

Minimal agent request:

```bash
curl -sS http://127.0.0.1:8317/v1/responses \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5",
    "input": "Reply exactly: BROKER_OK",
    "prompt_cache_key": "safzan-coding-agent",
    "stream": false
  }'
```

## Reasoning Level

Either use model suffixes:

```json
{
  "model": "gpt-5.5(low)",
  "input": "Keep this short.",
  "stream": false
}
```

Or send native Responses reasoning:

```json
{
  "model": "gpt-5.5",
  "input": "Think carefully, then answer concisely.",
  "reasoning": {
    "effort": "medium"
  },
  "stream": false
}
```

Supported effort values are:

```text
low
medium
high
xhigh
max    (GPT-6 Astra and gpt-5.6 family only; gpt-5.4 and older reject it)
ultra  (alias for max; wire-level "ultra" does not exist)
```

`ultra` is accepted for convenience but is forwarded as `max`: the Codex
Responses endpoint rejects `reasoning.effort: "ultra"` even though the model
catalog advertises it. In the official Codex CLI, ultra maps to `max` on the
wire and additionally enables proactive multi-agent task delegation — a
client-side behavior the broker does not replicate.

## Fast Mode And Service Tiers

Send either accepted Fast mode spelling:

```json
{
  "model": "gpt-5.5",
  "input": "Keep this short.",
  "service_tier": "fast"
}
```

`fast` and `priority` are aliases. The broker canonicalizes both to the signal
used by the current official Codex client: `service_tier: "priority"` in the
request body plus `x-codex-routing-hint: model=<model>;tier=priority` on HTTP
requests. `serviceTier` is accepted as a camel-case input alias. `flex` and
`ultrafast` retain their own wire values.

Explicit `auto` and `default` are omitted on the ChatGPT Codex wire. This
matches the official client and avoids the backend rejection seen with
explicit `auto`; omit `service_tier` for normal Standard routing.

The response's `service_tier` is always the value the upstream backend reports
it actually used. The broker does not replace `default` with the requested
Fast tier. ChatGPT Fast mode depends on model/account/backend eligibility, and
the backend can decline a correctly transmitted `priority` request. The
dashboard keeps requested and applied tiers in separate fields.

For Responses WebSocket, the routing hint belongs to the opening handshake,
which happens before the first `response.create` event. The broker normalizes
the event body and safely forwards a client handshake hint such as:

```text
x-codex-routing-hint: model=gpt-5.5;tier=priority
```

Official Codex WebSocket clients send that header. A generic WebSocket client
that only puts `service_tier` in `response.create` still sends the canonical
body field, but cannot retroactively add the connection-level routing hint.

## Streaming

Set `stream: true` and keep the curl connection open:

```bash
curl -N http://127.0.0.1:8317/v1/responses \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5(low)",
    "input": "Write one sentence about local auth brokers.",
    "stream": true
  }'
```

The broker forwards Server-Sent Events from the Codex backend. The dashboard
also scans streaming responses for final usage when the upstream stream includes
it.

## Responses WebSocket

WebSocket-capable Responses clients use the same base URL. Connect to
`ws://127.0.0.1:8317/v1/responses` (or `wss://` when TLS terminates in front of
the broker), authenticate with the same bearer key, and negotiate:

```text
OpenAI-Beta: responses_websockets=2026-02-06
```

Send Responses client events such as `response.create`; the broker returns
Responses server events on the same socket. It preserves `previous_response_id`
and forwards Codex turn-state headers/events, so compatible clients can send
only newly added input items on later turns. The broker normalizes each
`response.create` just like an HTTP request.

Pi's `openai-codex-responses` adapter may instead connect to
`ws://127.0.0.1:8317/v1/codex/responses`; that path is an equivalent alias.

The upstream limits a WebSocket connection to 60 minutes. Reconnect after
`websocket_connection_limit_reached`, and reconnect after a rate-limit event so
multi-account failover can select another account.

Streaming agent request:

```bash
curl -N http://127.0.0.1:8317/v1/responses \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5(low)",
    "input": "Write one short status update.",
    "prompt_cache_key": "safzan-coding-agent",
    "stream": true
  }'
```

## Image Input

Clients that support Responses-style multimodal input can send image content
through the same endpoint:

```json
{
  "model": "gpt-5.5",
  "input": [
    {
      "role": "user",
      "content": [
        {
          "type": "input_text",
          "text": "Describe this image briefly."
        },
        {
          "type": "input_image",
          "image_url": "https://example.com/image.png"
        }
      ]
    }
  ],
  "stream": false
}
```

The broker does not rewrite structured `input`; it forwards it after applying
the same auth and compatibility normalization.

## Provider Configuration

For tools that accept a custom Responses provider:

```json
{
  "baseUrl": "http://127.0.0.1:8317/v1",
  "api": "openai-responses",
  "apiKey": "dummy",
  "model": "gpt-5.5",
  "promptCacheKey": "safzan-coding-agent"
}
```

If the client has a separate option for adding an authorization header, enable
it. If the broker was started with `--api-key-file`, set `apiKey` to that file's
contents.

## Compatibility Notes

- Prefer `/v1/responses` for native reasoning items and Responses WebSocket
  continuation. `/v1/chat/completions` is available for Chat-only clients.
- `prompt_cache_key` is preserved or injected so repeated long prompts can hit
  model-side prompt caching.
- `prompt_cache_retention`, max-token aliases, `stream_options`, and `user` are
  stripped before forwarding because the Codex backend rejects them.
- `prompt_cache_options` passes through for GPT-6 Astra and is stripped for
  older models.
- The broker never returns or exposes the Codex refresh token.

## Prompt Caching For Agents

The broker does not cache responses itself. It helps the upstream model-side
prompt cache work by keeping `prompt_cache_key` stable.

Recommended agent behavior:

```json
{
  "model": "gpt-5.5",
  "input": "your request here",
  "prompt_cache_key": "safzan-coding-agent",
  "stream": false
}
```

Use the same `prompt_cache_key` for the same project or long-running agent
session, and a different one per session. The key scopes the backend's cache
routing affinity, so reusing one value across unrelated conversations puts them
in a single bucket where they evict each other. If you omit it the broker falls
back to a session id derived from the request, then to its configured constant
(unset by default); sending your own stable key is still preferred, since it
also makes dashboard rows easier to reason about.

Do not send `prompt_cache_retention`. For GPT-6 Astra, send
`prompt_cache_options` when you need its supported cache TTL. The broker strips
that object for older models and preserves `prompt_cache_key` for all models.
OpenAI documents GPT-5.5 and GPT-5.4 as extended-retention models with entries
retained for up to 24 hours. GPT-5.6 uses a newer policy with a 30-minute
minimum lifetime and possible longer retention.

The response does not report the chosen retention policy. It reports only
actual cache reads through `usage.input_tokens_details.cached_tokens`, so a
zero on the first eligible request is expected and does not prove that cache
retention is disabled.

How to verify prompt caching:

1. Send one long request with a stable `prompt_cache_key`.
2. Send a second request with the same long prefix and the same
   `prompt_cache_key`.
3. Inspect the response usage or dashboard row.

The cached-token signal is:

```json
{
  "usage": {
    "input_tokens_details": {
      "cached_tokens": 12345
    }
  }
}
```

The dashboard also shows `cached_tokens` and cache percentage for each request.

## Debugging

Health:

```bash
curl -fsS http://127.0.0.1:8317/healthz
```

Dashboard:

```text
http://127.0.0.1:8317/dashboard
```

The dashboard shows request status, model normalization, reasoning effort,
streaming mode, token usage, and cached input tokens. It does not store prompt
text, completion text, bearer tokens, access tokens, or refresh tokens.
