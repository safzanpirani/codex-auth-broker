# Plan 006: Broker-managed native Codex compaction

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat d9420c2..HEAD -- proxy.go requests.go pricing.go`
> If `normalizeResponsesBody`, `buildUpstreamRequest`, `sseUsageTracker`, or
> `stablePromptCacheKey` changed since this plan was written, re-read them
> before proceeding; on a structural mismatch, treat it as a STOP condition.

## Status

- **Priority**: P3
- **Effort**: L
- **Risk**: HIGH
- **Depends on**: `d9420c2` (compaction passthrough) — landed
- **Category**: feature
- **Planned at**: commit `d9420c2`, 2026-07-27

## Why this matters

Passthrough (`d9420c2`, `docs/compaction.md`) only helps clients that implement
compaction themselves — today that is essentially Codex CLI and the
`pi-codex-compaction` Pi extension. Every other client on this broker (Factory
Droid, OpenClaw, anything on the Chat Completions shim) has no concept of an
encrypted checkpoint, so long sessions still walk into context-limit errors and
pay full input-token cost on every turn.

Broker-managed compaction moves that logic behind the proxy: the broker watches
context growth per conversation, fires its own compaction request when a session
crosses a threshold, and transparently splices the resulting checkpoint into
subsequent requests. Dumb clients get long-run behaviour for free.

This is materially harder than passthrough because the broker is, by design,
nearly stateless. The plan below is mostly about earning the right to be
stateful safely.

## Current state

- `proxy.go:406 normalizeResponsesBody` — single funnel for every Responses
  request; already computes `stablePromptCacheKey`, strips unsupported params,
  and (since `d9420c2`) sets `info.CompactionTrigger`.
- `proxy.go:322 buildUpstreamRequest` — sets `x-codex-beta-features` when the
  gate is needed.
- `proxy.go hasCompactionTrigger` / `codexBetaFeatures` — the passthrough
  helpers this plan builds on.
- `proxy.go aggregateResponsesSSE` — collects `response.output_item.done` items
  regardless of type; compaction items already survive.
- `sseUsageTracker` (`proxy.go`, tested in `proxy_test.go:307`) — already
  observes streaming final usage, including `input_tokens`. This is the trigger
  signal; it needs no new upstream call.
- `pricing.go` — per-model metadata. **Verify whether it carries a context
  window.** If not, step 2 must add one.
- No persistence layer for per-conversation state exists. `persist.go` handles
  the request log only.

## Design

### Conversation identity

Key checkpoints on `(conversationKey, model)` where `conversationKey` is the
value `stablePromptCacheKey` already derives (client `prompt_cache_key`, else
`conversation_id`/`session_id`, else configured key). **Rotating per-request ids
are deliberately excluded there** — reuse that exclusion; a per-request key would
make every checkpoint a single-use orphan.

If no stable key can be derived, broker-managed compaction is **off** for that
request. Do not invent a key from client IP or history hashing.

### The splice problem

The broker never sees "the conversation" — it sees one `input` array per turn.
To replace a prefix with a checkpoint it must know that the array it is looking
at today actually starts with the array it compacted yesterday. Solution:

When storing a checkpoint, also store a **prefix fingerprint** of the input that
was compacted: the item count `n` plus a SHA-256 over the canonical JSON of
those `n` items. On a later request, recompute the fingerprint over the first
`n` items of the incoming `input`. On match, splice:

```
input = [retained recent user messages…] + [compaction item] + input[n:]
```

On mismatch (client trimmed history itself, forked the conversation, replayed an
edited turn), **discard the checkpoint and pass the request through unmodified**.
A stale splice would silently corrupt the conversation; a discarded checkpoint
only costs tokens.

Clients using `previous_response_id` instead of sending full history are out of
scope — detect and skip.

### Fail-open, not fail-closed

`pi-codex-compaction` is fail-closed: a failed compaction aborts the pending
model request, because the user asked for compaction. The broker's situation is
inverted — the user asked for *an answer* and never mentioned compaction. So:
**any failure in the compaction path must fall back to forwarding the original
request untouched**, logged but not surfaced as an error. This is the single
most important invariant in this plan.

### Timing

Compact **after** a turn completes, out of band, not before. The trigger signal
(`input_tokens` from the completed turn's usage) is only available at that point,
and doing it inline would add checkpoint latency to a user-visible request.
Consequence: the checkpoint applies from the *next* turn onward, so the threshold
must leave headroom for one more full turn (default 80%, not 90%).

### Prompt-cache interaction

Splicing rewrites the prefix, so the compaction turn is a guaranteed upstream
cache miss; turns after it re-stabilise. Keep `prompt_cache_key` unchanged across
the splice — do not derive a new key from the checkpoint, or every compaction
permanently orphans the cache.

## Steps

### Step 1 — Config surface, default off

Add to `config` in `main.go`, all env + flag like their neighbours:

- `CODEX_AUTH_BROKER_COMPACTION` / `--compaction` (bool, **default false**)
- `CODEX_AUTH_BROKER_COMPACTION_THRESHOLD` / `--compaction-threshold` (float,
  default `0.80`, clamp to `[0.5, 0.95]`)
- `CODEX_AUTH_BROKER_COMPACTION_TTL` / `--compaction-ttl` (duration, default
  `6h`)

Verify: `go build ./... && ./codex-auth-broker --help | grep compaction`

### Step 2 — Model context windows

Confirm whether `pricing.go` model metadata already carries a context window.
If not, add a `contextWindow` field with entries for the Codex model families
this broker serves, plus a conservative fallback for unknown models. An unknown
window means **compaction disabled for that model** — never guess a window, since
guessing low burns tokens on needless compactions and guessing high defeats the
feature.

Verify: unit test asserting a known model resolves a window and an invented model
id resolves zero/disabled.

### Step 3 — Checkpoint store

New file `compaction.go`. In-memory, mutex-guarded, bounded:

```go
type checkpointKey struct{ conversation, model string }

type checkpoint struct {
    encryptedContent string
    retained         []any // recent user messages kept verbatim
    prefixCount      int
    prefixHash       string
    createdAt        time.Time
}
```

- Bounded by entry count (reuse the `requestLogStore` bounding idiom from
  `requests.go:75`) with LRU eviction, plus TTL expiry on read.
- **In-memory only in this phase.** Do not add disk persistence — checkpoints are
  short-lived and a corrupt on-disk checkpoint is worse than a cold start. Revisit
  only if a measured need appears.
- Redaction: `encryptedContent` must never reach the request log or dashboard.
  Cross-check `requests.go markRequest` and the redaction helpers from plan 005.

Verify: table tests for store/get, TTL expiry, LRU eviction, and key isolation by
model (a checkpoint stored for model A must not be returned for model B).

### Step 4 — Splice on the request path

In `normalizeResponsesBody` (or a helper it calls, to keep that function
readable), after `normalizeInput`:

1. Skip entirely if: feature off, no stable key, `info.CompactionTrigger` is true
   (client is driving compaction itself — never fight it), `previous_response_id`
   present, or `input` is not an array.
2. Look up the checkpoint. On hit, verify the prefix fingerprint. On match,
   splice per the design above and set `info.CompactionApplied = true`.
3. On any mismatch or error, delete the checkpoint and leave the body untouched.

Because the spliced body now contains a `compaction` item, confirm the existing
`codexBetaFeatures` path sends `remote_compaction_v2` for replay requests too —
`hasCompactionTrigger` only matches `compaction_trigger`. Extend it (or add a
sibling) so a `compaction` item also sets the gate.

Verify: tests for hit-and-splice, fingerprint mismatch → passthrough + eviction,
client-driven trigger → untouched, `previous_response_id` → untouched.

### Step 5 — Trigger on the response path

Where `sseUsageTracker` finalises usage, compare `input_tokens` against
`window * threshold`. On exceed, and if the feature is on and a stable key
exists, enqueue a background compaction job carrying the *pre-normalization*
input that was just sent upstream.

- Single-flight per `checkpointKey` — never two concurrent compactions for one
  conversation.
- Bounded worker pool and a queue that **drops** rather than blocks when full.
- Detached context with its own timeout; it must not inherit the client request's
  context, which is cancelled the moment the client disconnects.

Verify: test that a usage report above threshold enqueues exactly one job, that a
second report for the same key while one is in flight does not enqueue, and that
a report below threshold enqueues nothing.

### Step 6 — The compaction request

Build the body per `docs/compaction.md` / the `pi-codex-compaction` reference:
same model, `store: false`, `stream: true`, `include:
["reasoning.encrypted_content"]`, `text.verbosity: "low"`, the same
`prompt_cache_key`, tools as sent on the originating turn, and `input` = sent
input + `{"type":"compaction_trigger"}`.

Send it through the **existing** account-pool dispatch path so it inherits
failover, rate-limit cooldown, and the `x-codex-beta-features` gate. Do not open
a bespoke HTTP client.

Parse the stream expecting exactly one `compaction` output item; anything else
(zero items, two items, `response.failed`, `response.incomplete`) is a failure →
log, store nothing, move on.

On success, compute `retained` via a port of `retainRecentUserMessages` (recent
user messages, ~token-budgeted, middle-truncated) and store the checkpoint with
the prefix fingerprint of the input that was compacted.

Verify: httptest upstream returning a canned compaction SSE stream → checkpoint
stored with correct fingerprint. Separate tests for the malformed/failed/zero-item
cases asserting nothing is stored.

### Step 7 — Observability

- Dashboard: compaction count, active checkpoint count, last error per
  conversation (redacted).
- Request log: a boolean "compaction applied" on spliced turns, and a distinct
  entry kind for broker-issued compaction calls so their token cost is not
  mistaken for user traffic in `costs.go`.
- Structured log lines on: threshold crossed, checkpoint stored, splice applied,
  fingerprint mismatch, compaction failed.

Verify: dashboard test asserting the counters move; cost test asserting
broker-issued compaction tokens are attributed separately.

### Step 8 — Docs

Extend `docs/compaction.md` with a "Broker-managed mode" section: what it does,
the config flags, the fail-open guarantee, the identity requirement (clients
*must* send a stable `prompt_cache_key` or `conversation_id` to benefit), and the
explicit non-goals (no `previous_response_id` support, no cross-model reuse, no
persistence across restart). Update the README paragraph added in `d9420c2`.

## Verification

```bash
gofmt -l .
go vet ./...
go test -race ./...
```

Then a live soak against a real account, feature on, threshold lowered so it
fires quickly:

1. Run a long multi-turn conversation through `/v1/responses` with a fixed
   `prompt_cache_key`; confirm from the logs that a checkpoint is stored and a
   later turn is spliced, and that answers stay coherent across the splice.
2. Repeat with the feature off and confirm byte-identical upstream bodies to the
   pre-change build.
3. Force a failure (point compaction at an unreachable upstream) and confirm user
   turns still succeed.

## STOP conditions

- Any code path where a compaction failure can fail a user's turn. This breaks
  the core invariant — stop and redesign.
- Needing to persist checkpoints to disk to make the feature useful. That means
  the TTL/threshold model is wrong; stop and re-plan.
- `pricing.go` has no reliable context-window source and one cannot be added
  without hardcoding guesses for models the broker discovers dynamically.
- The splice fingerprint proves unreliable in the soak (mismatches on
  well-behaved clients). Without a trustworthy prefix check the feature is not
  safe to ship.
- Scope pressure to also support the Chat Completions shim or WebSocket transport
  in this phase. Land HTTP `/v1/responses` first.

## Out of scope

- Client-driven compaction (already shipped, `d9420c2`).
- Disk persistence of checkpoints.
- `previous_response_id` conversations.
- Cross-model checkpoint reuse.
