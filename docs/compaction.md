# Native Codex compaction

The Codex backend can fold a long conversation into an opaque, encrypted
checkpoint instead of answering a turn. The client then replays that checkpoint
as the head of its history, dropping everything it replaced. This keeps a long
agent run inside the context window without a client-side "summarize the
conversation" prompt.

The broker **passes this through**. It does not decide when to compact, does not
store checkpoints, and does not rewrite history. The client owns that state; the
broker's job is to not get in the way.

## What the client sends

Append a `compaction_trigger` item as the last element of `input`:

```jsonc
{
  "model": "gpt-5.3-codex",
  "stream": true,
  "store": false,
  "include": ["reasoning.encrypted_content"],
  "prompt_cache_key": "<stable conversation id>",
  "input": [
    /* … the full conversation so far … */
    { "type": "compaction_trigger" }
  ]
}
```

The request must be gated by the `remote_compaction_v2` beta feature. Send it
yourself:

```
x-codex-beta-features: remote_compaction_v2
```

The broker forwards `x-codex-beta-features` verbatim (joining repeated headers,
de-duplicating tokens). As a convenience, when the body contains a
`compaction_trigger` item and the client omitted the header, the broker adds
`remote_compaction_v2` so the request is not rejected upstream. Any other
feature tokens you send are preserved alongside it.

## What comes back

The response streams exactly one output item of type `compaction`:

```jsonc
{
  "type": "response.output_item.done",
  "output_index": 0,
  "item": { "type": "compaction", "encrypted_content": "…" }
}
```

`encrypted_content` is opaque — do not parse, truncate, or re-encode it. Store
it as-is. If you request a non-streaming response, the broker aggregates the
stream and the compaction item appears in `response.output`.

## Replaying a checkpoint

On subsequent turns, send the checkpoint item as the input prefix, followed by
the messages that came after it:

```jsonc
"input": [
  /* optionally, a few recent user messages retained verbatim */
  { "type": "compaction", "encrypted_content": "…" },
  /* … turns since the checkpoint … */
]
```

Client-side rules worth honouring:

- A checkpoint is bound to the model that produced it. Switching models
  invalidates it — compact again rather than replaying across models.
- Treat compaction as fail-closed. If the compaction request errors, abort the
  pending model request instead of continuing with an over-long context.
- Keep `prompt_cache_key` stable across the conversation so upstream prefix
  caching survives the checkpoint swap.

## Scope and limits

- Responses API only, over both HTTP and the WebSocket transport
  (`x-codex-beta-features` is in the WebSocket header allowlist).
- Not exposed through the Chat Completions shim — that surface has no way to
  represent an opaque checkpoint item. See
  [`chat-completions.md`](chat-completions.md).
- The broker does not track context usage or trigger compaction on its own.

A working client-side implementation to model yours on is
[`pi-codex-compaction`](https://github.com/ogulcancelik/pi-extensions/tree/main/packages/pi-codex-compaction).
