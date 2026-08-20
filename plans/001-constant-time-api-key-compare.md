# Plan 001: Compare the client API key in constant time

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 0a50ba9..HEAD -- proxy.go proxy_test.go`
> If either in-scope file changed since this plan was written, compare the
> "Current state" excerpt against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: security
- **Planned at**: commit `0a50ba9`, 2026-06-26

## Why this matters

The client-facing bearer key is the single secret that gates access to your
Codex account: anyone who presents it can spend your Codex quota and use your
account through `/v1/responses`. The current check compares the presented key
against the configured key with Go's `==` string operator, which short-circuits
on the first differing byte. That is a textbook timing side-channel — an
attacker who can reach the listen address (a Tailscale peer, a process on the
same host, anything sharing the private interface) can in principle recover the
key byte-by-byte from response-time differences. The fix is to compare with
`crypto/subtle.ConstantTimeCompare`, the standard remedy. It is a few lines,
carries essentially no behavioral risk, and removes the side-channel from the
one comparison this whole project exists to protect.

## Current state

- `proxy.go` — contains `authorizedClient`, the single gate used by every
  authenticated handler (`handleModels`, `handleResponses`,
  `handleChatCompletions`, `handleDashboardRequests`, `handleDashboardCosts`,
  `handleCodexUsage`).

Excerpt as it exists today, `proxy.go:180-191`:

```go
func (p *responsesProxy) authorizedClient(r *http.Request) bool {
	want := strings.TrimSpace(p.cfg.apiKey)
	if want == "" {
		return true
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix)) == want
}
```

The `want == ""` branch (no key configured → allow all) is intentional: the
broker runs open on localhost by default. Keep that branch exactly as-is.

Repo conventions to follow:
- Go standard library only — `crypto/subtle` is stdlib, so no new dependency.
  Do not add any third-party package.
- Imports in this file are a single grouped `import (...)` block at the top of
  `proxy.go` (currently `bufio`, `bytes`, `encoding/json`, `errors`, `fmt`,
  `io`, `log`, `net/http`, `sort`, `strings`, `time`). Add `crypto/subtle` in
  alphabetical position (before `encoding/json`).
- Tests are table-driven and use `net/http/httptest`. See the existing
  `proxy_test.go` (e.g. `TestNormalizeFactoryModel`) for the exact style to
  match: a slice of anonymous structs, one `t.Fatalf` per failing case.

## Commands you will need

| Purpose   | Command                                   | Expected on success |
|-----------|-------------------------------------------|---------------------|
| Format    | `gofmt -l proxy.go proxy_test.go`         | no output           |
| Vet       | `go vet ./...`                            | exit 0, no output   |
| Test (new)| `go test ./... -run TestAuthorizedClient -v` | PASS, exit 0    |
| Test (all)| `go test ./...`                           | `ok`, exit 0        |
| Build     | `go build -o codex-auth-broker .`         | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `proxy.go` — change the comparison in `authorizedClient`.
- `proxy_test.go` — add `TestAuthorizedClient`.

**Out of scope** (do NOT touch, even though they look related):
- The `want == ""` open-by-default branch — keep it.
- The `Bearer ` prefix handling / `strings.TrimSpace` of the header — keep the
  same parsing; only the final equality comparison changes.
- `main.go` config loading, `doctor.go` fingerprinting — unrelated.
- Do not change the `Authorization` header name or make the prefix
  case-insensitive; that is a separate compatibility decision.

## Git workflow

- Branch: `advisor/001-constant-time-api-key-compare`
- Commit message style matches the repo (imperative, optional `area:` prefix —
  e.g. `security: compare client API key in constant time`).
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add the `crypto/subtle` import

In `proxy.go`, add `"crypto/subtle"` to the import block in alphabetical order
(it sorts before `"encoding/json"`).

**Verify**: `go build -o codex-auth-broker .` → exit 0 (the import is now used
after Step 2; if you build between steps it may report "imported and not used"
— that is expected until Step 2 lands, so run this verify after Step 2).

### Step 2: Replace the final comparison with a constant-time compare

Change the last line of `authorizedClient` from:

```go
	return strings.TrimSpace(strings.TrimPrefix(header, prefix)) == want
```

to:

```go
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
```

`ConstantTimeCompare` returns `1` when the byte slices are equal and `0`
otherwise; it returns `0` immediately when the lengths differ. Leaking the
key's *length* this way is the accepted standard tradeoff and is not a concern
for a high-entropy key.

**Verify**: `go vet ./...` → exit 0, and `go build -o codex-auth-broker .` →
exit 0.

### Step 3: Add `TestAuthorizedClient` to `proxy_test.go`

Add a table-driven test that builds a `responsesProxy` with a configured key
and exercises the gate. `authorizedClient` only reads `p.cfg.apiKey`, so you can
construct the proxy directly without auth/upstream wiring. Use
`httptest.NewRequest` to set the header. Cover these cases:

- configured key, header `Bearer <correct>` → `true`
- configured key, header `Bearer <wrong>` → `false`
- configured key, header `Bearer <correct><extra>` (length mismatch) → `false`
- configured key, no `Authorization` header → `false`
- configured key, header without the `Bearer ` prefix → `false`
- **no key configured** (`apiKey: ""`), no header → `true` (open-by-default)

Target shape (match the repo's table-driven style):

```go
func TestAuthorizedClient(t *testing.T) {
	const key = "s3cr3t-key-value"
	tests := []struct {
		name   string
		apiKey string
		header string
		want   bool
	}{
		{name: "correct key", apiKey: key, header: "Bearer " + key, want: true},
		{name: "wrong key", apiKey: key, header: "Bearer nope", want: false},
		{name: "length mismatch", apiKey: key, header: "Bearer " + key + "x", want: false},
		{name: "missing header", apiKey: key, header: "", want: false},
		{name: "no bearer prefix", apiKey: key, header: key, want: false},
		{name: "open by default", apiKey: "", header: "", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := &responsesProxy{cfg: config{apiKey: test.apiKey}}
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if test.header != "" {
				req.Header.Set("Authorization", test.header)
			}
			if got := p.authorizedClient(req); got != test.want {
				t.Fatalf("authorizedClient(header=%q, apiKey set=%t) = %t, want %t",
					test.header, test.apiKey != "", got, test.want)
			}
		})
	}
}
```

`proxy_test.go` already imports `net/http` and `net/http/httptest`, so no new
imports are needed.

**Verify**: `go test ./... -run TestAuthorizedClient -v` → all subtests PASS.

## Test plan

- New test: `TestAuthorizedClient` in `proxy_test.go`, covering the six cases
  listed in Step 3 (correct key, wrong key, length mismatch, missing header,
  missing prefix, open-by-default).
- Structural pattern to follow: `TestNormalizeFactoryModel` in the same file.
- Verification: `go test ./...` → `ok`, including the new test.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "subtle.ConstantTimeCompare" proxy.go` returns exactly one match
- [ ] `grep -n "== want" proxy.go` returns no matches (the `==` comparison is gone)
- [ ] `go vet ./...` exits 0
- [ ] `go test ./...` exits 0; `TestAuthorizedClient` exists and passes
- [ ] `gofmt -l proxy.go proxy_test.go` prints nothing
- [ ] `go build -o codex-auth-broker .` exits 0
- [ ] `git status --porcelain` shows only `proxy.go` and `proxy_test.go` modified
- [ ] `plans/README.md` status row for plan 001 updated

## STOP conditions

Stop and report back (do not improvise) if:

- The live `authorizedClient` does not match the "Current state" excerpt (the
  code has drifted since this plan was written).
- `go test ./...` fails for a reason unrelated to your change (a pre-existing
  failure) — report it rather than working around it.
- Adding the constant-time compare appears to require touching any file outside
  the in-scope list.

## Maintenance notes

- If a future change makes the bearer prefix case-insensitive or accepts the
  key from a query parameter, the constant-time comparison must be preserved on
  whatever the final extracted candidate is.
- A reviewer should confirm the open-by-default (`want == ""`) branch is
  untouched and that no logging of the presented key was added.
- `ConstantTimeCompare` returning early on length mismatch is intentional and
  acceptable; do not try to "fix" the length leak by padding — that adds
  complexity for no real gain on a high-entropy key.
