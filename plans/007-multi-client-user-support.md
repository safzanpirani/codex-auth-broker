# Plan 007: Multi-client / user support (EC2-ready broker)

> **This is a DESIGN DOC, not an executor plan.** It defines the feature set
> needed before the broker can run as a shared service on an EC2 instance
> inside the InsideIIM AWS VPC — same network as the "lovableclone" builder
> backend and its sandboxes — with no Tailscale dependency. Slice it into
> individual executor plans (007a, 007b, …) before implementing.

## Status

- **Priority**: P1 (blocks moving the builder's model path off the tailnet)
- **Effort**: L (phased; phase 1 alone is M)
- **Risk**: MEDIUM (auth-surface changes; backwards compat required)
- **Depends on**: the uncommitted working-tree changes as of 2026-08-20
  (constant-time key compare, `--max-concurrent` semaphore, `/v1/alpha/search`)
- **Category**: feature / security
- **Written**: 2026-08-20, for the lovableclone builder integration
  (see `~/Development/insideiim/lovableclone/DECISIONS.md` §6, §11, §15)

## Why this matters

Today the broker is deliberately single-tenant: one shared bearer key, one
anonymous caller, tailnet-only, an unauthenticated dashboard. The builder
changes the shape of demand:

1. **Several consumers** will call it — the builder backend, every sandbox
   container, individual devs pointing Pi/Factory at it, possibly other
   InsideIIM services. One shared key means one leaked sandbox env leaks
   everything, and rotation is all-or-nothing.
2. **Per-user accounting** is a product feature: the builder shows "who used
   how much" keyed by Google-SSO email. Today the backend reconstructs this
   from Pi usage events in its own SQLite; the broker — the only component
   that sees *every* request — can't attribute anything.
3. **EC2 placement** removes the tailnet crutch. Inside a VPC security group
   the network is semi-trusted, but the broker fronts a personal Codex
   account: named keys, an authenticated dashboard, and quota guardrails are
   the difference between "service" and "open proxy".

## Feature set

### F1 — Named API keys (clients)

Replace the single `--api-key` with a key registry while keeping the old flag
working (it becomes the implicit `default` client).

- Config: `--keys-file keys.json` (reloaded on SIGHUP or mtime change):
  ```json
  [
    { "name": "buildr-backend",  "key": "…", "role": "client" },
    { "name": "buildr-sandbox",  "key": "…", "role": "client" },
    { "name": "safzan-dev",      "key": "…", "role": "admin" },
    { "name": "old-shared",      "key": "…", "role": "client", "disabled": true }
  ]
  ```
- Lookup must stay timing-safe: iterate all entries with
  `subtle.ConstantTimeCompare`, never map-by-key-prefix alone.
- Request log gains a `client` field; dashboard filters by it.
- Per-client overrides (optional, later): `max_concurrent`, `models` allowlist
  (e.g. a search-only client that may only hit `/v1/alpha/search`).
- Rotation story: add new key, flip consumers, disable old — no restart.

### F2 — Per-user attribution

The builder backend knows the human (Google SSO email); the broker should
record it.

- Accept `X-Broker-User: <email>` on `/v1/*` requests. Trusted only because
  the request already carried a valid client key — document that clients own
  the truthfulness of this header.
- Store per-request: client, user, model, input/output/cached tokens, cost.
- New endpoint `GET /dashboard/api/usage/by-user?window=7d` → aggregates per
  (user, model). The builder's Settings page can then read broker-truth
  instead of reconstructing from Pi events (its SQLite copy stays as a
  fallback/cross-check).
- Wire-through for the builder: backend chat relay already injects the session
  email into prompt commands; it additionally sets the header on… nothing —
  the *sandbox* calls the broker, not the backend. So the agent-host must
  forward it: pass `BROKER_USER` env into the sandbox per-session? No — a
  sandbox is shared across users per project. Correct plumbing: the agent-host
  learns the user per-prompt (already in the WS `prompt` frame) and sets
  `X-Broker-User` per model request. Requires Pi custom-provider header
  support (check `models.json` `headers` field; if absent, a tiny header on
  the generated provider config or a Pi PR). Fallback: reuse
  `prompt_cache_key` convention `user:<email>` — the broker already preserves
  that field; parsing it is zero-client-change.

### F3 — Quotas / guardrails (phase 2)

- Optional per-client and per-user token budgets per rolling window
  (`--quota buildr-sandbox=5M/day`), enforced with the structured 429 +
  `Retry-After` shape the semaphore already uses.
- A global "reserve" so interactive dev keys keep working when the builder
  burns a window (e.g. sandboxes capped at 80% of the 5-h window).
- Soft mode first (log + dashboard warning), hard mode behind a flag.

### F4 — Dashboard & admin auth

- Once off the tailnet, the static dashboard HTML must not ship to anonymous
  callers. Gate `/dashboard*` behind `role: admin` keys (query param or
  cookie exchange), keep `/healthz` open.
- Admin-only mutating endpoints if/when needed (clear request log — already
  DELETE — plus future key management).

### F5 — EC2 deployment posture (ops, mostly not code)

- **Placement**: small instance (t4g.micro is plenty; it's an I/O proxy) in a
  private subnet of the builder's VPC; security group allows only the backend
  ECS tasks / sandbox subnet + an admin SSH path. No public IP. Consumers use
  the private DNS name; the tailnet address stays as a secondary for dev
  machines (broker binds 0.0.0.0, both routes work).
- **TLS**: terminate on an internal ALB/NLB with an ACM cert, or accept
  plain HTTP inside the SG boundary for v1 (documented decision; VPC traffic
  is not the internet, but the bearer keys ride on it).
- **Secrets**: `auth.json` (Codex OAuth refresh token) is the crown jewel.
  Options in order of preference: SSM Parameter Store (SecureString) fetched
  at boot with instance-role, synced back on refresh; or EBS + strict file
  perms. Never bake into an AMI. The broker already rotates/refreshes —
  needs a `--auth-sync` hook (post-refresh command) so the refreshed token is
  pushed back to SSM; otherwise an instance replacement loses the session.
- **Login bootstrap**: `codex login` is a browser flow; document the ritual:
  login on a laptop → push tokens to SSM → instance pulls. Same for each
  pooled account.
- **Service**: systemd unit + restart policy, CloudWatch (or existing fleet
  dashboard) shipping the structured log, alarm on `/healthz` failure and on
  auth-refresh failure (F6).
- Remove/update the README "do not expose on the public internet" caveat to
  the accurate post-auth guidance ("private networks with named keys; still
  never the public internet").

### F6 — Operational alerts (phase 2)

- Auth refresh failure, account window exhaustion (all pool accounts limited),
  and quota trips should notify (webhook URL flag; Safzan can point it at
  telegram via `tg`). Today these are only visible in logs/dashboard.

## What the builder needs first (suggested slicing)

1. **007a — F1 named keys** (S/M): registry + compat + request-log client
   field. Unblocks giving backend and sandboxes separate rotatable keys.
2. **007b — F2 attribution** (M): header + `prompt_cache_key` fallback +
   by-user usage endpoint. Unblocks broker-truth usage in the builder UI.
3. **007c — F4 dashboard auth** (S): required before EC2.
4. **007d — F5 ops** (M, mostly infra): SSM auth sync + systemd + SG design;
   coordinate with the lovableclone senior-engineer review (Fargate-vs-EC2
   decision may co-locate the broker on the same box).
5. F3 quotas and F6 alerts after real usage data exists.

## Compatibility invariants

- `CODEX_AUTH_BROKER_API_KEY` alone must keep working unchanged (implicit
  default client, role `admin` to preserve current dashboard access —
  or `client` + a printed warning; decide in 007a).
- No change to `/v1/*` request/response shapes.
- Existing tests must pass untouched except where they assert the single-key
  behavior; extend, don't rewrite.

## STOP conditions for implementers

- Any change that would write a plaintext key or token into the request log,
  dashboard JSON, or an error message (the redaction layer must cover new
  fields — see plan 005).
- Any auth comparison that is not constant-time.
- Breaking the single-key compat path.
