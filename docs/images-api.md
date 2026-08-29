# Images API

The broker exposes OpenAI-compatible image generation and edit routes over the
ChatGPT Codex Responses transport:

```text
POST /v1/images/generations   application/json
POST /v1/images/edits         multipart/form-data
```

Keep the broker on localhost or a private network such as Tailscale. A broker
client key is not an OpenAI API key and is never forwarded as Codex OAuth.

## Generation

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

The established non-stream generation behavior is unchanged: the broker makes
one upstream call per requested image and returns the Images JSON shape.

## Multipart edit and mask

Use `image` or repeated `image[]` parts. They may be mixed, up to five total.
A mask is optional.

```bash
curl -sS http://127.0.0.1:8317/v1/images/edits \
  -H 'Authorization: Bearer dummy' \
  -F 'model=gpt-image-2' \
  -F 'prompt=Replace the sky with a soft sunset' \
  -F 'image[]=@foreground.png' \
  -F 'image[]=@reference.webp' \
  -F 'mask=@mask.png' \
  -F 'quality=high' \
  -F 'output_format=png'
```

The broker structurally validates PNG, JPEG, and WebP image configuration rather
than trusting names, part content types, or file magic alone. Input images must
each be smaller than 50 MB. A mask must be a structurally valid PNG, smaller
than 4 MiB, and have the same dimensions as the first input image. The whole
multipart body is limited to 128 MB. Files are read in memory and are not
written to temporary storage.

## Streaming partial images

Set `stream:true` and optionally `partial_images` from `0` through `3`.
Streaming requires `n:1`; supplying `partial_images` requires streaming.

Generation:

```bash
curl -N http://127.0.0.1:8317/v1/images/generations \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer dummy' \
  -d '{"prompt":"A yellow robot in watercolor","stream":true,"partial_images":2}'
```

Edit:

```bash
curl -N http://127.0.0.1:8317/v1/images/edits \
  -H 'Authorization: Bearer dummy' \
  -F 'prompt=Make this look like a watercolor' \
  -F 'image=@input.png' \
  -F 'stream=true' \
  -F 'partial_images=2'
```

Each SSE frame includes both the official event name and JSON payload:

```text
event: image_generation.partial_image
data: {"type":"image_generation.partial_image","b64_json":"...","background":"auto","created_at":1787990000,"output_format":"png","quality":"auto","size":"auto","partial_image_index":0}

event: image_generation.completed
data: {"type":"image_generation.completed","b64_json":"...","background":"auto","created_at":1787990001,"output_format":"png","quality":"auto","size":"auto","usage":{"input_tokens":12,"output_tokens":34,"total_tokens":46}}
```

Edits use `image_edit.partial_image` and `image_edit.completed`. The broker does
not expose raw Responses events and does not append `[DONE]`; the typed completed
event is terminal. If an upstream failure occurs after SSE headers were sent,
the broker emits one bounded `event: error` frame and closes. Disconnects cancel
the upstream request, and the global concurrency slot remains held for the
entire stream. SSE errors use a stable generic downstream message; private
backend error details are never relayed.

The number of partial events may be lower than requested. In particular, the
private Codex backend currently may emit no partial events and return only the
final completed image; the broker never fabricates intermediate images.

## Defaults and limits

| Field | Generation | Edit | Limit / values |
| --- | --- | --- | --- |
| `model` | `gpt-image-2` | `gpt-image-2` | selected image tool model |
| `n` | `1` | `1` | 1–4 non-stream; exactly 1 streaming |
| image references | n/a | required | 1–5 via `image` / `image[]` |
| input image file | n/a | required | structurally valid PNG/JPEG/WebP; smaller than 50 MB each |
| mask file | n/a | optional | structurally valid PNG; smaller than 4 MiB; first-image dimensions |
| `size` | upstream default when omitted | `auto` | `auto` or validated flexible dimensions |
| `quality` | upstream default when omitted | `auto` | `auto`, `low`, `medium`, `high` |
| `background` | upstream default when omitted | `auto` | `auto`, `opaque`, `transparent` |
| `output_format` | upstream default (`png`) | upstream default (`png`) | `png`, `jpeg`, `webp` |
| `moderation` | upstream default | upstream default | `auto`, `low` |
| `output_compression` | optional | optional | 0–100; forwarded for JPEG/WebP |
| `input_fidelity` | n/a | optional | `low`, `high`; omitted upstream for `gpt-image-2` |
| `partial_images` | optional | optional | 0–3; requires `stream:true` when supplied |

Flexible sizes must have edges no larger than 3840 and divisible by 16, aspect
ratio at most 3:1, and total area from 655,360 through 8,294,400 pixels.
Transparent background with JPEG is rejected. Transparent output remains
private-backend dependent; the broker preserves validation rather than silently
switching image models.

## Non-stream response and privacy

Both routes return base64-only Images JSON when `stream` is false:

```json
{"created":1787990000,"data":[{"b64_json":"iVBORw0KGgo..."}],"usage":{"total_tokens":46}}
```

The broker requires canonical bounded base64 and an authoritative terminal
`response.completed`. Failed, incomplete, interrupted, malformed, oversized,
or noncanonical upstream streams are rejected. Completed output overrides any
earlier `response.output_item.done` fallback.

`user` is accepted but never forwarded or logged. Prompts, input bytes, masks,
revised prompts, output base64, bearer credentials, and request bodies are not
stored in request history. Logs contain metadata only.

## Private Codex backend boundary

These routes adapt public Images shapes to behavior accepted by the private
`chatgpt.com/backend-api/codex/responses` transport. Account rotation, 429
failover, auth refresh, and the global semaphore are shared with `/v1/responses`;
each upstream image call owns one slot for its full lifetime. The private
backend may normalize dimensions, vary transparent-background support, or
change accepted fields without the stability guarantees of the public OpenAI
API. The `n` maximum of four and edit maximum of five image references are
intentional compatibility constraints of this private Codex backend, not the
limits of the public OpenAI Images API. Clients needing exact dimensions should
inspect decoded output.
