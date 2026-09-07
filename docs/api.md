# API

`codex-auth-broker` exposes a deliberately small OpenAI-compatible surface.

## `GET /healthz`

Returns process health:

```json
{
  "status": "ok",
  "version": "dev",
  "commit": "",
  "mode": "responses-proxy"
}
```

## `GET /dashboard`

Serves the local browser dashboard. The HTML itself is not protected so a
browser can load it before an API key is entered. The dashboard API calls below
use the same bearer key as `/v1/responses` when `--api-key` is configured.

## `GET /dashboard/api/usage`

Fetches live Codex usage from ChatGPT's wham usage endpoint using the local
Codex OAuth access token.

The upstream response is returned mostly as-is, with a `_broker` object added:

```json
{
  "plan_type": "team",
  "rate_limit": {
    "primary_window": {
      "used_percent": 22.4,
      "limit_window_seconds": 18000,
      "reset_at": 1777891740
    },
    "secondary_window": {
      "used_percent": 46.8,
      "limit_window_seconds": 604800,
      "reset_at": 1778057520
    }
  },
  "_broker": {
    "fetched_at": "2026-05-11T14:20:00Z",
    "account_id_present": true,
    "source": "chatgpt.com/backend-api/wham/usage"
  }
}
```

`reset_at` is Unix seconds. HTTP 401 or 403 from this endpoint usually means
the local Codex login is stale or the account id is no longer accepted by the
usage API.

## `GET /usage`

Alias for `GET /dashboard/api/usage`.

## `GET /dashboard/api/requests`

Returns redacted in-memory request history. Query parameter:

```text
limit=250
```

The newest request is returned first.

Example entry:

```json
{
  "id": 12,
  "started_at": "2026-05-11T14:20:00.123Z",
  "duration_ms": 1842,
  "method": "POST",
  "path": "/v1/responses",
  "client": "127.0.0.1",
  "request_id": "optional-client-id",
  "model": "gpt-5.5(medium)",
  "normalized_model": "gpt-5.5",
  "reasoning_effort": "medium",
  "service_tier": "priority",
  "applied_service_tier": "default",
  "stream": false,
  "status": 200,
  "upstream_status": 200,
  "prompt_cache_key_set": true,
  "prompt_cache_key": "sha256:9ec8d7df5522",
  "prompt_cache_retention_set": false,
  "input_count": 1,
  "tool_count": 4,
  "input_tokens": 26000,
  "output_tokens": 400,
  "cached_tokens": 23000,
  "total_tokens": 26400
}
```

The request log stores a short SHA-256 fingerprint instead of the raw prompt
cache key. It deliberately does not store prompt text, completion text,
request bodies, access tokens, refresh tokens, or bearer keys. Set
`--request-log-limit 0` to disable it.

For streaming `/v1/responses`, the broker forwards bytes to the client while
also scanning SSE `data:` frames for the final response usage object. If the
client disconnects early or the upstream stream does not include usage, token
fields are omitted for that row.

Responses WebSocket turns are recorded the same way with `method: "WS"`. The
broker inspects only `response.create` metadata and final usage/tier/error
events; prompt text, completion text, full frames, and bearer tokens are not
retained.

## `DELETE /dashboard/api/requests`

Clears retained request history. The lifetime `total_seen` counter is not
reset.

## `GET /v1/models`

Returns configured model ids in OpenAI list format.

## `POST /v1/responses`

The primary endpoint. It accepts OpenAI Responses-shaped requests and forwards
them to the Codex Responses backend using local Codex OAuth access auth.

Compatibility normalizations:

- `gpt-6-astra(max)` becomes `model: "gpt-6-astra"` and
  `reasoning.effort: "max"`.
- `gpt-5.5(low)` becomes `model: "gpt-5.5"` and
  `reasoning.effort: "low"`.
- `gpt-5.4-mini(high)` becomes `model: "gpt-5.4-mini"` and
  `reasoning.effort: "high"`.
- `gpt-5.6-sol(max)` becomes `model: "gpt-5.6-sol"` and
  `reasoning.effort: "max"` (GPT-6 Astra and the gpt-5.6 family support `max`;
  older models reject it).
- `gpt-5.6-sol(ultra)` is forwarded as `reasoning.effort: "max"` — the Codex
  backend rejects wire-level `ultra`; in the official CLI it means max effort
  plus client-side proactive multi-agent delegation.
- `gpt-5.3-codex` is advertised by `/v1/models` and forwards as
  `model: "gpt-5.3-codex"`.
- Native client reasoning, such as Pi sending `reasoning.effort`, is preserved
  and shown in dashboard request history.
- GPT-6 Astra `configuration_update` input items and `async: true` function or
  custom tools pass through unchanged. Astra `prompt_cache_options` also pass
  through so clients can use the model's cache TTL controls.
- WebSocket clients can send `response.steer` during an Astra response. The
  broker forwards steering acknowledgements, continuations, and terminal events
  without interpreting their payloads.
- `service_tier: "fast"` and `"priority"` both become the official Codex Fast
  mode wire value `"priority"` and the matching `x-codex-routing-hint` header.
  Explicit `auto` and `default` are omitted for the ChatGPT Codex backend.
- String `input` becomes a Responses input item list.
- Missing `instructions` gets a compact default.
- Missing `store` becomes `false`.
- Missing `include` becomes `["reasoning.encrypted_content"]`.
- `max_tokens`, `max_output_tokens`, `max_completion_tokens`, `maxOutputTokens`,
  `prompt_cache_retention`, `stream_options`, `user`, and
  related OpenAI SDK compatibility fields are stripped because the Codex
  backend rejects them.
- Non-streaming requests are implemented by forcing upstream streaming and
  aggregating the final SSE response.
- Request bodies with `Content-Encoding: zstd` are decompressed before
  normalization. Pi's Codex adapter uses this encoding for SSE requests.

## Responses WebSocket

`GET /v1/responses` with a standard WebSocket Upgrade uses the Responses
WebSocket protocol. `/v1/codex/responses` is an equivalent GET/POST alias for
clients such as Pi that append `/codex/responses` to their configured `/v1`
base URL. Use the same client-facing bearer key as the HTTP endpoint and
include:

```text
OpenAI-Beta: responses_websockets=2026-02-06
```

The broker also adds that beta token when it is absent. It accepts JSON
`response.create` client events and forwards Responses server events. Every
`response.create` receives the same model-name, reasoning, input, default, and
unsupported-field normalization as `POST /v1/responses`.

The official Codex client also sends a connection-level
`x-codex-routing-hint` during the opening handshake. The broker validates and
normalizes that client header before forwarding it. Because the handshake
precedes the first `response.create`, a service tier supplied only inside the
event cannot add the connection-level hint retroactively.

The following Codex protocol headers are passed through the opening handshake:

- `x-codex-turn-state`
- `x-models-etag`
- `x-reasoning-included`
- `openai-model`

Turn state included in later server events is forwarded unchanged. This allows
compatible clients to reuse a connection and send incremental input with
`previous_response_id` rather than resending the complete conversation.

WebSocket connections are pinned to the account selected at handshake time. A
handshake `429` rotates to the next account before the downstream upgrade
completes. A `429` server event on an established connection is delivered to
the client, then the broker closes the socket and cools that account; reconnect
to select the next account. The broker does not replay an in-flight turn.

The upstream currently enforces a 60-minute connection limit and sends
`websocket_connection_limit_reached`; clients must reconnect. WebSocket clients
should also retain enough logical conversation state to retry without
`previous_response_id` if a new account cannot resolve the old response ID.

## `POST /v1/chat/completions`

Accepts OpenAI Chat Completions-shaped requests and translates them through the
same Codex Responses backend, auth pool, model normalization, cache affinity,
request log, and usage accounting as `/v1/responses`.

Supported compatibility surface:

- Non-streaming `chat.completion` objects and streaming
  `chat.completion.chunk` SSE with `[DONE]`.
- `system`, `developer`, `user`, `assistant`, and `tool` messages.
- Text, image URL/data URL, and file input content.
- Function tools, forced function choices, parallel tool calls, and tool
  result history.
- `response_format` text, JSON object, and JSON schema output.
- Reasoning effort, verbosity, service tier, metadata, and Factory model
  effort suffixes.
- `stream_options.include_usage`, including cached reads and cache writes when
  upstream reports them.
- Stable `prompt_cache_key` preservation/injection. The translation is
  deterministic so resending the same conversation prefix remains cacheable.

Current boundaries:

- Only `n: 1` is supported.
- Audio input/output and custom tools are rejected.
- Sampling fields accepted by the Responses backend are forwarded. Chat-only
  controls with no Codex Responses equivalent, including max-token aliases,
  are ignored or stripped.
- `prompt_cache_retention` is recorded as intent and stripped. GPT-6 Astra
  receives `prompt_cache_options`; older models do not because the ChatGPT
  Codex endpoint rejects the object for them.

See [`chat-completions.md`](chat-completions.md) for examples.
