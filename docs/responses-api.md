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
Primary model: gpt-5.5
Reasoning: omit reasoning for off/default, or send reasoning.effort low/medium/high/xhigh (gpt-5.6 also accepts max)
Prompt cache key: use a stable project key, for example "safzan-coding-agent"

Do not use /v1/chat/completions. Use /v1/responses only.
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
Do not use /v1/chat/completions. Use /v1/responses only.
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
max    (gpt-5.6 family only; gpt-5.4 and older reject it)
ultra  (alias for max; wire-level "ultra" does not exist)
```

`ultra` is accepted for convenience but is forwarded as `max`: the Codex
Responses endpoint rejects `reasoning.effort: "ultra"` even though the model
catalog advertises it. In the official Codex CLI, ultra maps to `max` on the
wire and additionally enables proactive multi-agent task delegation — a
client-side behavior the broker does not replicate.

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

- Use `/v1/responses`, not `/v1/chat/completions`.
- `prompt_cache_key` is preserved or injected so repeated long prompts can hit
  model-side prompt caching.
- `prompt_cache_retention`, max-token aliases, `stream_options`, `user`, and
  `service_tier` are stripped before forwarding because the Codex backend
  rejects them.
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
session. If you omit it, the broker injects its configured default key, currently
`factory-droid`, but agents should send their own stable key so dashboard rows
and cache affinity are easier to reason about.

Do not send `prompt_cache_retention`. Some clients send
`prompt_cache_retention: "24h"`, but the Codex backend rejects that field, so
the broker strips it before forwarding.

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
