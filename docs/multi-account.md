# Multi-Account Failover

Pool several Codex logins so the broker rotates past an account that has hit a
rolling usage limit instead of stalling until the window resets.

Codex enforces two rolling windows — roughly a 5-hour bucket and a weekly
bucket. When either is exhausted the backend returns `429`. A single-account
broker can do nothing but wait; a pool moves the next request to a fresh
account and keeps serving.

## Setup

### 1. Log in each account into its own `CODEX_HOME`

Each account is an ordinary Codex login stored in a separate home directory, so
the logins never overwrite each other. `codex login` will not create the
directory for you — make it first.

```bash
# Account 1 — the default home
CODEX_HOME=~/.codex   codex login
CODEX_HOME=~/.codex   codex login status

# Account 2 — a different Codex account
mkdir -p ~/.codex-2
CODEX_HOME=~/.codex-2 codex login
CODEX_HOME=~/.codex-2 codex login status
```

On a headless box, use the device-code flow so no localhost callback is needed:

```bash
CODEX_HOME=~/.codex-2 codex login --device-auth
```

Confirm the accounts are actually different (a common mistake is logging the
same account in twice):

```bash
for f in ~/.codex/auth.json ~/.codex-2/auth.json; do
  python3 -c "import json;print('$f',json.load(open('$f'))['tokens']['account_id'])"
done
```

The `account_id` values must differ.

### 2. Run the broker against the pool

```bash
./codex-auth-broker serve --auth-files ~/.codex/auth.json,~/.codex-2/auth.json
```

or via the environment (equivalent):

```bash
CODEX_AUTH_FILES=~/.codex/auth.json,~/.codex-2/auth.json ./codex-auth-broker serve
```

`--auth-files` overrides `--auth-file`. With a single entry it behaves exactly
like `--auth-file`, so single-account setups need no change. Duplicate paths are
collapsed. List order is the failover order.

### Systemd `EnvironmentFile` pattern

So adding an account is one line and one restart — no unit edit, no
`daemon-reload` — keep the pool in an optional env file the unit reads:

```ini
[Service]
EnvironmentFile=-/home/USER/codex-auth-broker.env
ExecStart=/usr/local/bin/codex-auth-broker serve --listen 127.0.0.1:8317 --auth-file /home/USER/.codex/auth.json
```

The leading `-` makes the file optional, so single-account works with no env
file present. To pool accounts:

```bash
printf 'CODEX_AUTH_FILES=/home/USER/.codex/auth.json,/home/USER/.codex-2/auth.json\n' \
  > ~/codex-auth-broker.env
sudo systemctl restart codex-auth-broker
```

`CODEX_AUTH_FILES` (set in the env file) overrides the `--auth-file` in
`ExecStart`, so you never touch the unit to add or remove an account.

## How it behaves

**Sticky selection.** Requests stay on the active account so its backend prompt
cache stays warm. The broker only moves off an account when that account returns
`429`.

**Rotation.** On a `429` the account is benched until its window resets and the
same request is retried on the next available account. The client sees no error
— just a slightly slower response for the one request that triggered the
rotation.

**Cooldown deadline.** The bench time prefers an explicit reset from the `429`,
in this order:

1. `Retry-After` header (delta seconds or an HTTP date).
2. A rate-limit reset header (`x-codex-*-reset-after-seconds`,
   `x-ratelimit-reset*`).
3. A machine-readable body field whose key contains `reset` / `retry` /
   `resets_in` / `resets_at` / `cooldown` — numbers are read as epoch seconds if
   large, otherwise as a delta; strings are also parsed as RFC3339 timestamps.

If none is present, it falls back on the response wording: a body mentioning a
weekly limit → 7 days, a usage or 5-hour limit → 5 hours, anything else → 60
seconds. Every value is clamped to `[30s, 8d]`.

**All accounts cooling down.** The broker returns `429` to the client with a
`Retry-After` header pointing at the soonest reset across the pool.

**Auth errors.** If an account's token refresh fails, it is benched briefly
(2 minutes) and the pool rotates past it, so one broken login does not take the
broker down.

## Observability

`/healthz` reports the pool:

```json
{
  "status": "ok",
  "accounts_total": 2,
  "accounts_available": 1,
  "accounts": [
    {"index": 0, "label": ".codex", "available": true},
    {"index": 1, "label": ".codex-2", "available": false,
     "cooldown_until": "2026-07-09T15:30:00Z", "cooldown_seconds": 1420,
     "last_reason": "5h"}
  ]
}
```

`doctor` validates every login at once:

```bash
./codex-auth-broker doctor --auth-files ~/.codex/auth.json,~/.codex-2/auth.json
```

```json
{
  "status": "ok",
  "accounts_total": 2,
  "accounts": [
    {"auth_file": "~/.codex/auth.json",   "status": "ok", "expires_at": "..."},
    {"auth_file": "~/.codex-2/auth.json", "status": "ok", "expires_at": "..."}
  ]
}
```

Overall status is `degraded` if any account fails to load; the per-account
`status`/`error` fields say which. Tokens are never printed.

Each rotation logs one line:

```text
codex account .codex-2 hit rate limit window=5h source=retry-after cooling_until=2026-07-09T15:30:00Z; rotating (1/2)
```

`window` is a coarse label (`weekly` / `5h` / `windowed` / `short`); `source` is
where the reset came from (`retry-after`, `header:<name>`, `body:<field>`, or
`default`). The first time an account actually hits a limit, check this line: if
`source=default` the `429` carried no machine-readable reset and the fallback
guess was used — the raw body is also logged (redacted) so you can see the real
field names and tighten the parser if needed.

## Troubleshooting

- **Both accounts report the same `account_id`.** You logged the same Codex
  account into both homes. Re-run `codex login` in the second `CODEX_HOME` and
  pick a different account.
- **`CODEX_HOME points to "..." but that path does not exist`.** `codex login`
  does not create the home directory. `mkdir -p` it first.
- **An account never rotates back in.** Rotation back is automatic once the
  cooldown passes. Check `/healthz` for `cooldown_seconds`; if it looks far too
  long, the `429` likely carried a misparsed reset — see the logged `source` and
  raw body.
- **All accounts cooling down at once.** Expected when every account's window is
  exhausted. The `Retry-After` on the `429` tells you when the first one frees
  up. Add another account to widen the pool.

## Ownership note

The broker refreshes each account's token in place and is the sole owner of all
of them. Do not run a second broker (or the Codex CLI in normal use) against an
`auth.json` that a pooled broker owns, or the two will fight over refresh-token
rotation.

Pooling multiple accounts to extend usage limits is account-multiplexing that
OpenAI's terms discourage. This is an operational feature; use it within the
terms that apply to you.
