# Subscription capabilities

The broker uses the local Codex OAuth login. `GET /v1/capabilities` lists the
implemented routes, transport, default media models, and known gaps. It does
not probe account entitlements. All API routes use the same broker bearer
key and named-client authentication as `/v1/responses`.

| Capability | Client endpoint | Support |
| --- | --- | --- |
| Text, reasoning, vision input, tools | `POST /v1/responses`, Responses WebSocket, `POST /v1/chat/completions` | Existing Codex Responses transport |
| Context compaction | `POST /v1/responses/compact` | Experimental native Codex compaction; opaque output preserved |
| Image generation | `POST /v1/images/generations` | GPT Image through the Responses image-generation tool |
| Image editing | `POST /v1/images/edits` | Multipart or JSON image inputs and masks |
| GPT-Live voice | `POST /v1/realtime/calls` or `POST /v1/live` | Experimental subscription WebRTC adapter |
| GPT-Live control socket | `GET /v1/live/{call_id}` or `GET /v1/realtime?call_id=...` | Native GPT-Live sideband for a call created by this broker |
| Standalone web search | `POST /v1/alpha/search` | Experimental Codex search route |
| Embeddings | `POST /v1/embeddings` | No verified subscription route; returns 501 |
| File transcription, translation, TTS | `/v1/audio/transcriptions`, `/v1/audio/translations`, `/v1/audio/speech` | No verified subscription route; returns 501 |
| Realtime ephemeral credentials | `/v1/realtime/client_secrets`, `/v1/realtime/sessions` | Unavailable; use the broker key to create a WebRTC call |

Unavailable routes return an OpenAI-shaped error with
`error.code: "unsupported_endpoint"`. The broker does not fall back to paid
OpenAI API credentials. ChatGPT feature availability does not establish that
an equivalent API endpoint accepts a Codex subscription.

## GPT-Live

Open `/voice` on the broker for a small microphone demo. Use localhost or
HTTPS because browsers require a secure context for microphone access. The
demo keeps the broker key in the page's memory and sends audio directly over
WebRTC. It displays connection state and event types, without retaining
transcripts or recordings.

The call endpoint follows the [GA Realtime WebRTC call-creation shape](https://developers.openai.com/api/docs/guides/realtime-webrtc):

- Send multipart fields `sdp` (a WebRTC offer) and `session` (JSON).
- Receive HTTP 201, `Content-Type: application/sdp`, and an SDP answer.
- Read the call ID from `Location: /v1/realtime/calls/{call_id}`.
- Apply the answer with `RTCPeerConnection.setRemoteDescription`.

Example session:

```json
{
  "type": "realtime",
  "model": "gpt-live-1-codex",
  "instructions": "Respond briefly.",
  "audio": { "output": { "voice": "marin" } }
}
```

`type: "realtime"` is accepted for client compatibility and removed before
forwarding. The default model is `gpt-live-1-codex`; the default voice is
`marin`. Native `delegation` and `initial_items` fields are also accepted.
The broker defaults `delegation` to `{"type":"client"}`. Unsupported
top-level session fields are rejected rather than silently ignored.

`application/sdp` requests use the default session. JSON requests with
`{"sdp":"...","session":{...}}` are a broker convenience extension.
Call requests and WebSocket messages are limited to 1 MiB.

GPT-Live uses its native continuous conversation protocol. This adapter does
not translate it into the GA Realtime `response.create` protocol. The model
is distinct from both GA `gpt-realtime` models and `gpt-live-transcribe`.
Code written against those protocols needs adaptation. The response includes
`X-Broker-Event-Protocol: gpt-live` to make that distinction explicit.

Server-side clients can attach a control WebSocket with the same broker
Authorization header used to create the call. Browser clients can use the
WebRTC data channel. Standalone model WebSockets without an existing call
are unavailable through subscription auth.

Calls are pinned to their original Codex account and creating bearer key.
A different key cannot join the call, including an admin key. Key rotation
requires creating a new call. The broker keeps at most 256 call records,
including pending creations, for up to 70 minutes. Records contain routing
metadata only and disappear on restart. The global upstream concurrency
limit covers call creation and attached control WebSockets; peer-to-peer
WebRTC media continues outside that semaphore.

Close the peer connection to end media. A sideband disconnect alone does
not terminate the peer connection. The broker does not implement the GA
hangup endpoint because an equivalent subscription route has not been
verified. Broker shutdown drains/cancels control sockets, but cannot drain
media that travels directly between the browser and OpenAI.

Explicit HTTP 429 rejections rotate to the next available account before a
call exists. Upstream transport failures are not automatically retried: a failed response
can leave an already-created call. HTTP rejections include safe diagnostic
codes where available. HTML access challenges remain errors; the broker
does not bypass them. No call SDP, instructions, media, event payloads, or
upstream error bodies are written to request history.

Implementation references: the Codex client uses the backend
`realtime/calls?intent=quicksilver&architecture=avas` JSON route with
`OpenAI-Alpha: quicksilver=v2`, then attaches to
`wss://api.openai.com/v1/live/{call_id}` using the same OAuth account.
See [Codex call creation](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/realtime_call.rs)
and [Codex realtime client](https://github.com/openai/codex/blob/main/codex-rs/core/src/realtime_conversation.rs).
These subscription interfaces are experimental and can change independently
of the public API.

## Images

Image generation and editing already use the subscription-backed Responses
image tool. `gpt-image-2` is the default image model. Existing options include
`n`, size, quality, background, output format/compression, streaming partial
images, and editing masks. Results use the Images API JSON/SSE shape. See
the README image examples for supported limits and parameters.

The backing Responses model defaults to `gpt-5.6-sol`. If an account rejects
that model, set `--image-responses-model gpt-5.4` or
`CODEX_AUTH_BROKER_IMAGE_RESPONSES_MODEL=gpt-5.4`. This changes the model that
invokes the tool; the requested image model remains `gpt-image-2`.
`/v1/capabilities` reports the configured backing model as `responses_model`.

Media models are described by `/v1/capabilities`. `/v1/models` continues to
return the Codex text/reasoning catalog for Factory Droid and Pi, so those
clients do not accidentally select an image or voice model for text calls.

## Compaction

Send the normal [Responses compaction request](https://developers.openai.com/api/docs/guides/compaction)
with `model` and `input` to `POST /v1/responses/compact`. Optional fields are
passed through. Factory model suffixes are removed from the model ID.
Compaction does not force streaming, inject text-generation defaults, or
rewrite encrypted items. The upstream JSON response and output ordering are
preserved; use the complete returned output in subsequent Responses input.
Only request metadata is logged. A backend 404 means this native route is
unavailable for the request; the broker does not substitute a text summary.

## Verification

Local tests exercise multipart translation, native event forwarding over real
loopback WebSockets, account/client isolation, registry bounds, authentication,
oversized payloads, and compaction output preservation. Subscription access
depends on the account and service rollout; local protocol tests do not prove
that a particular account can create a live voice call.

A live check on September 8, 2026 generated one image successfully (HTTP 200).
The tested account received a structured `403 forbidden` for GPT-Live call
creation and HTTP 404 for native compaction. Successful voice media and live
compaction have therefore not been verified. Browser checks confirmed that
connection failure and cancellation release microphone tracks and allow retry.
