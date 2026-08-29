# Images API

The broker exposes an OpenAI-compatible image generation endpoint backed by
the ChatGPT Codex Responses transport:

```text
POST /v1/images/generations
```

Keep the broker on localhost or a private network such as Tailscale. Configure
a client bearer key for any non-local listener, and never expose the broker to
the public internet: it fronts the owner's personal Codex login.

## Example

```bash
curl -sS http://127.0.0.1:8317/v1/images/generations \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{
    "model": "gpt-image-2",
    "prompt": "A small yellow robot reading beside a window",
    "n": 1,
    "size": "1024x1024",
    "quality": "high",
    "output_format": "png",
    "background": "auto"
  }'
```

`dummy` is only a redacted example. If the broker was started with
`--api-key`, `--api-key-file`, or `--keys-file`, use the corresponding local
broker key. It is not an OpenAI API key and is never forwarded as Codex OAuth.

## Request fields

- `prompt` is required and must be a non-empty string.
- `model` defaults to `gpt-image-2` and selects the image-generation tool
  model.
- `n` defaults to `1` and accepts integers from `1` through `4`. The broker
  makes one upstream Responses request per image.
- `size` accepts `auto` or a `WIDTHxHEIGHT` resolution within GPT Image 2's
  official limits: each edge at most `3840px` and divisible by `16`, aspect
  ratio at most `3:1`, and total area from `655,360` through `8,294,400`
  pixels. Popular values include `1024x1024`, `1536x1024`, `1024x1536`,
  `2048x2048`, `2048x1152`, `3840x2160`, and `2160x3840`.
- `quality` accepts `auto`, `low`, `medium`, or `high`.
- `output_format` accepts `png`, `jpeg`, or `webp`.
- `background` accepts `auto`, `opaque`, or `transparent`.
- `moderation` accepts `auto` or `low`.
- `output_compression` accepts an integer from `0` through `100` and is
  forwarded only for `jpeg` or `webp` output. Transparent backgrounds require
  `png` or `webp`.
- `user` is accepted for request-shape compatibility but deliberately not
  forwarded or logged because arbitrary end-user identifiers are not known-safe
  on the private Codex transport.

Omitted optional fields are left for the upstream image tool to default. An
option can still be rejected if the selected image model does not support it.

## Response

Successful responses use the OpenAI Images API shape:

```json
{
  "created": 1787990000,
  "data": [
    {
      "b64_json": "iVBORw0KGgoAAA...",
      "revised_prompt": "A refined version of the prompt"
    }
  ],
  "usage": {
    "input_tokens": 12,
    "output_tokens": 34,
    "total_tokens": 46
  }
}
```

`revised_prompt` and `usage` are included only when the Codex backend returns
them. For `n > 1`, usage counters are summed across upstream calls. The broker
returns base64 only; URL responses are not supported.

The broker strictly requires a terminal `response.completed` event and a
canonical base64 image result. Failed, incomplete, interrupted, malformed, or
oversized upstream streams return a normalized error. Account failover and the
global upstream concurrency limit work the same way as `POST /v1/responses`;
each requested image occupies one slot for its own upstream call.

## Compatibility boundary

This route adapts the official GPT Image request shape to behavior currently
accepted by the ChatGPT Codex Responses backend. The backend may normalize
requested dimensions, so clients that require exact pixel output should inspect
the returned image rather than assume `size` was honored byte-for-byte. Codex
OAuth and `chatgpt.com/backend-api/codex/responses` are private implementation
details, not a public stability contract. Backend fields, supported image
options, and availability may change without the guarantees of the public
OpenAI API.
