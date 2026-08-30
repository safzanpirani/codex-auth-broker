# Calling The Chat Completions API

`codex-auth-broker` exposes an OpenAI-compatible Chat Completions endpoint over
the same local Codex OAuth and ChatGPT Codex Responses backend used by
`/v1/responses`.

## Base URL

```text
http://127.0.0.1:8317/v1
```

Use the client-facing bearer key configured on the broker. It can be `dummy`
when the broker was started without `--api-key` or `--api-key-file`.

## Basic Request

```bash
curl -sS http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5(low)",
    "messages": [
      {"role": "developer", "content": "Answer directly."},
      {"role": "user", "content": "Reply exactly: CHAT_OK"}
    ],
    "stream": false,
    "prompt_cache_key": "my-project"
  }'
```

The result is a normal `chat.completion` object with one choice. Factory model
suffixes such as `(low)`, `(medium)`, `(high)`, `(xhigh)`, and supported
`(max)` variants use the same normalization as `/v1/responses`.

## Fast Mode And Service Tiers

Chat Completions accepts `service_tier` or `serviceTier`. Values `fast` and
`priority` both become the current Codex Fast mode wire signal:
`service_tier: "priority"` plus the matching `x-codex-routing-hint` upstream
header. Explicit `auto` and `default` are omitted; omit the field for Standard
routing.

Both non-streaming completions and streaming chunks report `service_tier` only
when the upstream Responses event reports it, and use the upstream-applied
value. A returned `default` therefore means the backend did not apply Fast mode
even if the request asked for it.

## Streaming

```bash
curl -N http://127.0.0.1:8317/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-5.5",
    "messages": [{"role": "user", "content": "Write one short sentence."}],
    "stream": true,
    "stream_options": {"include_usage": true},
    "prompt_cache_key": "my-project"
  }'
```

The broker translates Responses events into `chat.completion.chunk` SSE
records, including assistant-role, content, refusal, function-call argument,
finish-reason, and optional final usage chunks, followed by `data: [DONE]`.

## Function Calling

Function tools use the normal Chat Completions shape:

```json
{
  "model": "gpt-5.5",
  "messages": [
    {"role": "user", "content": "Look up record 42."}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "lookup_record",
        "description": "Look up one record.",
        "parameters": {
          "type": "object",
          "properties": {
            "id": {"type": "integer"}
          },
          "required": ["id"],
          "additionalProperties": false
        },
        "strict": true
      }
    }
  ],
  "tool_choice": "auto"
}
```

On the next turn, resend the assistant `tool_calls` message and the matching
`role: "tool"` result with the same `tool_call_id`. The broker maps both into
Responses function-call items without changing the call ID.

## Prompt Caching

The broker does not cache generated answers. It keeps the upstream model-side
prompt cache effective:

- Chat history is translated deterministically, without per-request timestamps
  or generated identifiers in the upstream prompt.
- A client-provided `prompt_cache_key` is preserved. Otherwise the configured
  broker default is injected.
- Reusing the same key and resending the same long message/tool prefix allows
  matching upstream prefixes to be reused.
- A rotating request ID is never substituted for `prompt_cache_key`.

Use one stable key per project, agent session, or conversation family:

```json
{
  "prompt_cache_key": "tenant:project:agent-v1"
}
```

Actual cache reads are returned in Chat Completions usage:

```json
{
  "usage": {
    "prompt_tokens_details": {
      "cached_tokens": 12345,
      "cache_write_tokens": 0
    }
  }
}
```

`cache_write_tokens` is included only when the upstream response reports it.
For streams, request `stream_options.include_usage` to receive the final usage
chunk.

The public API's `prompt_cache_retention` and `prompt_cache_options` controls
are not forwarded. The ChatGPT Codex OAuth endpoint rejects them and manages
retention server-side. The broker records legacy retention intent as metadata
only and preserves the cache key.

Prompt caching requires matching prefixes and is only observable on eligible
prompts; a zero `cached_tokens` value on the first request is expected.

## Supported Surface

- Roles: `system`, `developer`, `user`, `assistant`, and `tool`.
- Input: text, image URL/data URL, and file content parts.
- Output: text/refusal and function tool calls.
- Tools: function tools and the legacy `functions` form.
- Structured output: `text`, `json_object`, and `json_schema`
  `response_format` values.
- Streaming usage, reasoning effort, verbosity, service tier, metadata,
  parallel tool calls, and zstd request bodies.

Current limits:

- `n` must be omitted or set to `1`.
- Audio input/output and custom tools are not supported by this translation
  layer.
- Chat-only parameters without a Codex Responses equivalent may be ignored or
  stripped. Max-token aliases are accepted but not enforced because the Codex
  backend rejects them.
- This remains a compatibility layer, not a complete clone of every OpenAI Chat
  Completions feature.

For native Responses clients, including Responses WebSocket continuation, see
[`responses-api.md`](responses-api.md).
