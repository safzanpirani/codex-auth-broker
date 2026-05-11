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
  "stream": false,
  "status": 200,
  "upstream_status": 200,
  "prompt_cache_key_set": true,
  "prompt_cache_retention_set": false,
  "input_count": 1,
  "tool_count": 4,
  "input_tokens": 26000,
  "output_tokens": 400,
  "cached_tokens": 23000,
  "total_tokens": 26400
}
```

The request log deliberately does not store prompt text, completion text,
request bodies, access tokens, refresh tokens, or bearer keys. Set
`--request-log-limit 0` to disable it.

For streaming `/v1/responses`, the broker forwards bytes to the client while
also scanning SSE `data:` frames for the final response usage object. If the
client disconnects early or the upstream stream does not include usage, token
fields are omitted for that row.

## `DELETE /dashboard/api/requests`

Clears retained request history. The lifetime `total_seen` counter is not
reset.

## `GET /v1/models`

Returns configured model ids in OpenAI list format.

## `POST /v1/responses`

The primary endpoint. It accepts OpenAI Responses-shaped requests and forwards
them to the Codex Responses backend using local Codex OAuth access auth.

Compatibility normalizations:

- `gpt-5.5(low)` becomes `model: "gpt-5.5"` and
  `reasoning.effort: "low"`.
- `gpt-5.4-mini(high)` becomes `model: "gpt-5.4-mini"` and
  `reasoning.effort: "high"`.
- `gpt-5.3-codex` is advertised by `/v1/models` and forwards as
  `model: "gpt-5.3-codex"`.
- Native client reasoning, such as Pi sending `reasoning.effort`, is preserved
  and shown in dashboard request history.
- String `input` becomes a Responses input item list.
- Missing `instructions` gets a compact default.
- Missing `store` becomes `false`.
- Missing `include` becomes `["reasoning.encrypted_content"]`.
- `max_output_tokens`, `max_completion_tokens`, `maxOutputTokens`,
  `prompt_cache_retention`, `stream_options`, `user`, `service_tier`, and
  related OpenAI SDK compatibility fields are stripped because the Codex
  backend rejects them.
- Non-streaming requests are implemented by forcing upstream streaming and
  aggregating the final SSE response.

## `POST /v1/chat/completions`

Returns HTTP 501. Factory Droid uses `/v1/responses`; Chat Completions support
should only be added when a real client requires it.
