# Plan 004: Put tests under the auth/refresh path and the redaction backstop

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 0a50ba9..HEAD -- auth.go util.go dashboard_test.go`
> If any of these changed since this plan was written, compare the "Current
> state" excerpts against the live code before proceeding; on a mismatch, treat
> it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: LOW
- **Depends on**: none
- **Category**: tests
- **Planned at**: commit `0a50ba9`, 2026-06-26

## Why this matters

The two most security-critical pieces of this codebase have **zero direct
tests**:

1. **The auth/refresh path** (`auth.go`) — reading `auth.json`, deciding when to
   refresh, calling the OAuth token endpoint, persisting the rotated refresh
   token, and writing the file back atomically. A regression here either breaks
   login for everyone or mishandles the refresh token this whole project exists
   to protect.
2. **`redactTokenLikeText`** (`util.go:98`) — the single backstop that strips
   token-like strings out of error bodies and logs before they reach a client
   or disk. It has no test at all.

This plan adds characterization + unit tests around both, with no behavior
change to production logic except one tiny, safe test seam: making the OAuth
token URL injectable on `authManager` (defaulting to the existing constant) so
the refresh path can be exercised against a local test server. Landing these
tests first makes the redaction-hardening work in **plan 005** safe — it will
extend the redaction test file this plan creates.

## Current state

- `auth.go` — `authManager` and its `current`/`refresh` methods, the
  `read`/`writeAuthDocument` helpers, and the JWT claim helpers.
- `util.go` — `redactTokenLikeText`.
- `dashboard_test.go` — already defines two reusable helpers in `package main`:
  `fakeAccessToken(t, accountID, expiresAt)` and `encodeJWTPart(t, value)`.
  **Reuse them — do not redefine them.**

Excerpt — `authManager` struct and `refresh`, `auth.go:27-31` and `104-114`:

```go
type authManager struct {
	authFile    string
	refreshSkew time.Duration
	client      *http.Client
}
```

```go
func (m *authManager) refresh(ctx context.Context, refreshToken string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", codexClientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(form.Encode()))
```

Excerpt — the redaction function, `util.go:98-116`:

```go
func redactTokenLikeText(value string) string {
	fields := strings.Fields(value)
	for i, field := range fields {
		trimmed := strings.Trim(field, `"'.,;:()[]{}<>`)
		if strings.Count(trimmed, ".") == 2 && len(trimmed) > 40 {
			fields[i] = strings.Replace(field, trimmed, "[redacted-jwt]", 1)
			continue
		}
		if len(trimmed) > 60 && strings.IndexFunc(trimmed, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' || r == '~' || r == '+' || r == '/' || r == '=' ||
				(r >= '0' && r <= '9') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= 'a' && r <= 'z'))
		}) == -1 {
			fields[i] = strings.Replace(field, trimmed, "[redacted-token]", 1)
		}
	}
	return strings.Join(fields, " ")
}
```

Repo conventions to follow:
- Tests are `package main`, table-driven where natural, use `t.Fatalf` with a
  `got, want` message, and use `t.TempDir()` for filesystem tests. See
  `dashboard_test.go` (`TestFetchCodexUsageUsesLocalAccessAuth`) for the exact
  httptest + temp-auth-file shape to mirror.
- Standard library only.
- `writeJSON` (`util.go:14`) is the in-repo helper for writing a JSON HTTP
  response in tests' fake servers.

## Commands you will need

| Purpose      | Command                                        | Expected on success |
|--------------|------------------------------------------------|---------------------|
| Format       | `gofmt -l auth.go auth_test.go util_test.go`   | no output           |
| Vet          | `go vet ./...`                                  | exit 0              |
| Test (new)   | `go test ./... -run 'Auth|RedactTokenLikeText|JWT' -v` | PASS        |
| Test (all)   | `go test ./...`                                 | `ok`, exit 0        |
| Build        | `go build -o codex-auth-broker .`              | exit 0              |

## Scope

**In scope** (the only files you should modify or create):
- `auth.go` — add an injectable `tokenURL` field used by `refresh` (test seam).
- `auth_test.go` — **create**; tests for the JWT helpers, document round-trip,
  and `authManager.current` fast path + refresh path.
- `util_test.go` — **create**; characterization tests for `redactTokenLikeText`.

**Out of scope** (do NOT touch):
- The logic of `redactTokenLikeText` — this plan only *tests* it; plan 005
  changes its behavior.
- `dashboard_test.go` — reuse its helpers; do not edit it.
- Any other production behavior in `auth.go` beyond adding the `tokenURL` field
  and using it in `refresh`. Do not change refresh timing, error handling, or
  the file-write logic.
- `main.go` / `doctor.go` — they construct `authManager` without `tokenURL`;
  the zero value must keep working (falls back to the constant). Do not touch.

## Git workflow

- Branch: `advisor/004-auth-and-redaction-tests`
- Commit message style matches the repo (imperative — e.g.
  `auth: add refresh/round-trip tests behind an injectable token URL`).
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add the injectable token URL seam to `auth.go`

Add a `tokenURL` field to `authManager`:

```go
type authManager struct {
	authFile    string
	refreshSkew time.Duration
	client      *http.Client
	tokenURL    string // defaults to codexTokenURL when empty (override in tests)
}
```

In `refresh`, select the URL before building the request:

```go
	tokenURL := strings.TrimSpace(m.tokenURL)
	if tokenURL == "" {
		tokenURL = codexTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
```

`strings` is already imported in `auth.go`. This is additive: every existing
construction of `authManager` leaves `tokenURL` zero, so production keeps using
`codexTokenURL`.

**Verify**: `go build -o codex-auth-broker .` → exit 0; `go test ./...` → still
`ok` (no test references the field yet).

### Step 2: Create `util_test.go` with redaction characterization tests

These lock in the **current** behavior (whitespace-isolated tokens get
redacted; ordinary single-spaced text is preserved). Plan 005 will extend this
file with embedded-in-JSON cases.

```go
package main

import (
	"strings"
	"testing"
)

func TestRedactTokenLikeText(t *testing.T) {
	jwt := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJicm9rZXItdGVzdCIsImV4cCI6OTk5OTk5OTk5OX0.c2lnbmF0dXJlLXBsYWNlaG9sZGVyLXZhbHVl"
	opaque := strings.Repeat("A", 64)

	tests := []struct {
		name        string
		in          string
		wantContain string // must appear in output ("" to skip)
		wantAbsent  string // must NOT appear in output ("" to skip)
		wantEqual   string // exact expected output ("" to skip)
	}{
		{
			name:        "whitespace-isolated jwt is redacted",
			in:          "upstream error: " + jwt,
			wantContain: "[redacted-jwt]",
			wantAbsent:  "c2lnbmF0dXJl",
		},
		{
			name:        "long opaque token is redacted",
			in:          "Authorization Bearer " + opaque,
			wantContain: "[redacted-token]",
			wantAbsent:  opaque,
		},
		{
			name:      "ordinary error text is preserved",
			in:        "invalid_request_error: model not found",
			wantEqual: "invalid_request_error: model not found",
		},
		{
			name:      "short identifiers are not redacted",
			in:        "account acct_12345 model gpt-5.5",
			wantEqual: "account acct_12345 model gpt-5.5",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := redactTokenLikeText(test.in)
			if test.wantContain != "" && !strings.Contains(got, test.wantContain) {
				t.Fatalf("redactTokenLikeText(%q) = %q, want it to contain %q", test.in, got, test.wantContain)
			}
			if test.wantAbsent != "" && strings.Contains(got, test.wantAbsent) {
				t.Fatalf("redactTokenLikeText(%q) = %q, must not contain %q", test.in, got, test.wantAbsent)
			}
			if test.wantEqual != "" && got != test.wantEqual {
				t.Fatalf("redactTokenLikeText(%q) = %q, want %q", test.in, got, test.wantEqual)
			}
		})
	}
}
```

**Verify**: `go test ./... -run TestRedactTokenLikeText -v` → all subtests PASS.
If "ordinary error text is preserved" fails, STOP — the live function differs
from the excerpt.

### Step 3: Create `auth_test.go` with JWT-helper and document round-trip tests

```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestAuthFile(t *testing.T, path, access, refresh, accountID string) {
	t.Helper()
	raw := map[string]any{
		"tokens": map[string]any{
			"id_token":      "id-token-value",
			"access_token":  access,
			"refresh_token": refresh,
			"account_id":    accountID,
		},
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestJWTClaimHelpers(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	tok := fakeAccessToken(t, "acct_claims", exp)

	gotExp, err := jwtExpiresAt(tok)
	if err != nil {
		t.Fatal(err)
	}
	if gotExp.Unix() != exp.Unix() {
		t.Fatalf("jwtExpiresAt = %d, want %d", gotExp.Unix(), exp.Unix())
	}
	if _, err := jwtIssuedAt(tok); err != nil {
		t.Fatalf("jwtIssuedAt error: %v", err)
	}
	acct, err := accountIDFromAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if acct != "acct_claims" {
		t.Fatalf("accountIDFromAccessToken = %q, want acct_claims", acct)
	}
	if _, err := jwtPayload("not-a-jwt"); err == nil {
		t.Fatal("jwtPayload should reject a non-JWT string")
	}
}

func TestReadWriteAuthDocumentRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	access := fakeAccessToken(t, "acct_round", time.Now().Add(time.Hour))
	raw := map[string]any{
		"openai_api_key": "preserve-me",
		"tokens": map[string]any{
			"id_token":      "id-token-value",
			"access_token":  access,
			"refresh_token": "refresh-token-value",
			"account_id":    "acct_round",
		},
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := readAuthDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.tokens.AccessToken != access || doc.tokens.RefreshToken != "refresh-token-value" || doc.tokens.AccountID != "acct_round" {
		t.Fatalf("read tokens = %+v, want the values written", doc.tokens)
	}

	doc.tokens.AccessToken = fakeAccessToken(t, "acct_round", time.Now().Add(2*time.Hour))
	if err := writeAuthDocument(path, doc); err != nil {
		t.Fatal(err)
	}

	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := stat.Mode().Perm(); perm != 0o600 {
		t.Fatalf("auth file perm = %v, want 0600", perm)
	}

	reread, err := readAuthDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if reread.tokens.AccessToken != doc.tokens.AccessToken {
		t.Fatal("rewritten access token not persisted")
	}
	if reread.raw["openai_api_key"] != "preserve-me" {
		t.Fatal("unrelated fields in auth.json must be preserved on write-back")
	}
	if _, ok := reread.raw["last_refresh"]; !ok {
		t.Fatal("writeAuthDocument should stamp last_refresh")
	}
}
```

**Verify**: `go test ./... -run 'JWT|ReadWriteAuthDocument' -v` → PASS.

### Step 4: Add the `current` fast-path and refresh-path tests to `auth_test.go`

```go
func TestAuthManagerCurrentFastPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	access := fakeAccessToken(t, "acct_fast", time.Now().Add(time.Hour))
	writeTestAuthFile(t, path, access, "refresh-unused", "acct_fast")

	m := &authManager{
		authFile:    path,
		refreshSkew: time.Minute, // token has ~1h left, well beyond the skew
		client:      &http.Client{},
		tokenURL:    "http://127.0.0.1:1/must-not-be-called",
	}
	mat, err := m.current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mat.Refreshed {
		t.Fatal("fast path must not refresh a still-valid token")
	}
	if mat.AccessToken != access {
		t.Fatal("fast path should return the existing access token")
	}
	if mat.AccountID != "acct_fast" {
		t.Fatalf("AccountID = %q, want acct_fast", mat.AccountID)
	}
}

func TestAuthManagerCurrentRefreshesExpiredToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	expired := fakeAccessToken(t, "acct_refresh", time.Now().Add(-time.Hour))
	writeTestAuthFile(t, path, expired, "refresh-token-original", "acct_refresh")
	newAccess := fakeAccessToken(t, "acct_refresh", time.Now().Add(time.Hour))

	var gotGrant, gotRefresh string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotGrant = r.Form.Get("grant_type")
		gotRefresh = r.Form.Get("refresh_token")
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  newAccess,
			"refresh_token": "refresh-token-rotated",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	m := &authManager{
		authFile:    path,
		refreshSkew: 10 * time.Minute,
		client:      server.Client(),
		tokenURL:    server.URL,
	}
	mat, err := m.current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !mat.Refreshed {
		t.Fatal("expired token should have triggered a refresh")
	}
	if mat.AccessToken != newAccess {
		t.Fatal("current should return the refreshed access token")
	}
	if gotGrant != "refresh_token" {
		t.Fatalf("grant_type = %q, want refresh_token", gotGrant)
	}
	if gotRefresh != "refresh-token-original" {
		t.Fatalf("refresh_token sent = %q, want refresh-token-original", gotRefresh)
	}

	doc, err := readAuthDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.tokens.RefreshToken != "refresh-token-rotated" {
		t.Fatal("a rotated refresh token must be persisted to auth.json")
	}
	if doc.tokens.AccessToken != newAccess {
		t.Fatal("the new access token must be persisted to auth.json")
	}
}
```

**Verify**:
```bash
go test ./... -run Auth -v   # fast-path + refresh tests PASS
go test ./...                # ok
go vet ./...                 # exit 0
gofmt -l auth.go auth_test.go util_test.go   # no output
go build -o codex-auth-broker .              # exit 0
```

## Test plan

New tests, all `package main`:
- `util_test.go`: `TestRedactTokenLikeText` — jwt redaction, opaque-token
  redaction, ordinary text preserved, short identifiers preserved.
- `auth_test.go`: `TestJWTClaimHelpers`, `TestReadWriteAuthDocumentRoundTrip`,
  `TestAuthManagerCurrentFastPath`, `TestAuthManagerCurrentRefreshesExpiredToken`.
- Reuse `fakeAccessToken` / `encodeJWTPart` from `dashboard_test.go`.
- Structural pattern: `TestFetchCodexUsageUsesLocalAccessAuth` in
  `dashboard_test.go` (temp auth file + httptest server).
- Verification: `go test ./...` → `ok`, with the five new tests passing.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `auth_test.go` and `util_test.go` exist
- [ ] `grep -n "tokenURL" auth.go` returns ≥ 2 matches (field + use in `refresh`)
- [ ] `grep -c "func fakeAccessToken" *.go` returns `1` (helper not duplicated)
- [ ] `go test ./... -run 'Auth|RedactTokenLikeText|JWT|ReadWriteAuthDocument'` passes
- [ ] `go test ./...` exits 0
- [ ] `go vet ./...` exits 0
- [ ] `gofmt -l auth.go auth_test.go util_test.go` prints nothing
- [ ] `go build -o codex-auth-broker .` exits 0
- [ ] `git status --porcelain` shows only `auth.go` modified and `auth_test.go` + `util_test.go` added
- [ ] `plans/README.md` status row for plan 004 updated

## STOP conditions

Stop and report back (do not improvise) if:

- Any "Current state" excerpt does not match the live code (drift) — especially
  if `redactTokenLikeText` already differs (it may have been hardened by plan
  005 already; if so, the characterization assertions here may need to match the
  new behavior — stop and report rather than guess).
- A refresh test hangs — that usually means the `tokenURL` seam was not applied
  and `refresh` is hitting the real `auth.openai.com`. Fix the seam (Step 1) or
  stop.
- `TestReadWriteAuthDocumentRoundTrip` fails on the permission assertion on a
  filesystem that can't represent 0600 (rare; not the CI matrix) — report it,
  do not delete the assertion.
- Adding these tests appears to require changing production code beyond the
  `tokenURL` field — stop and report.

## Maintenance notes

- The `tokenURL` field exists for testability; production must never set it.
  A reviewer should confirm `main.go`/`doctor.go` still construct `authManager`
  without it.
- **Plan 005 depends on this plan**: it extends `util_test.go` with
  embedded-in-JSON redaction cases and changes `redactTokenLikeText`. The
  characterization assertions here (jwt redacted, opaque redacted, ordinary text
  preserved) are written to remain true after that hardening; if plan 005 must
  change one of them, that is a signal to re-review the redaction behavior.
- Follow-up deferred: an end-to-end `handleResponses` test against an httptest
  upstream returning SSE would round out proxy coverage; it is out of scope here
  to keep this plan focused on the auth + redaction gaps.
