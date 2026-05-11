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

## `GET /v1/models`

Returns configured model ids in OpenAI list format.

## `POST /v1/responses`

The primary endpoint. It accepts OpenAI Responses-shaped requests and forwards
them to the Codex Responses backend using local Codex OAuth access auth.

Compatibility normalizations:

- `gpt-5.5(low)` becomes `model: "gpt-5.5"` and
  `reasoning.effort: "low"`.
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
