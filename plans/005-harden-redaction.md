# Plan 005: Harden `redactTokenLikeText` to catch tokens embedded in JSON

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 0a50ba9..HEAD -- util.go util_test.go`
> If either changed since this plan was written, compare the "Current state"
> excerpt against the live code before proceeding; on a mismatch, treat it as a
> STOP condition.

## Status

- **Priority**: P1
- **Effort**: S
- **Risk**: MED
- **Depends on**: plans/004-auth-and-redaction-tests.md (it creates `util_test.go`, which this plan extends)
- **Category**: security
- **Planned at**: commit `0a50ba9`, 2026-06-26

## Why this matters

`redactTokenLikeText` is the **sole backstop** that keeps token-like strings out
of (a) upstream error bodies forwarded verbatim to clients (`proxy.go:147`),
(b) OAuth refresh error messages (`auth.go:293-297`), and (c) request-log error
fields and server logs. The project's entire reason for existing is to not leak
tokens, and this function is the last line of defense if one ever slips into an
error path.

The current implementation only inspects **whitespace-delimited fields**
(`strings.Fields`). That means a token embedded in a compact JSON error body —
exactly the shape an upstream API returns — is one big field with the wrong dot
count and slips through unredacted. Example: `{"access_token":"eyJ...sig"}` has
no spaces, so the whole blob is a single field and the JWT is not matched. The
function also collapses all internal whitespace via `strings.Join(fields, " ")`,
mangling multi-line error text as a side effect.

This plan replaces the field-splitting logic with two precompiled regexes that
match token-like substrings **anywhere** in the text and leave everything else
(including whitespace and JSON punctuation) intact. The risk to weigh is
over-redaction of legitimate long identifiers; the thresholds stay conservative
(a JWT must start with `eyJ`; an opaque run must be ≥ 60 base64url chars — the
same length bar the current code already uses) and the tests assert that
realistic error messages pass through untouched.

## Current state

- `util.go` — `redactTokenLikeText` (the function to rewrite) plus the import
  block.
- `util_test.go` — created by **plan 004**, contains `TestRedactTokenLikeText`
  with characterization cases that must still pass after this change.

Excerpt — `util.go:98-116` as it exists today:

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

`util.go` currently imports: `encoding/json`, `errors`, `fmt`, `net/http`,
`os`, `path/filepath`, `strings`, `time`. `strings` is still used elsewhere in
the file (e.g. `stringField`, `expandPath`), so keep it; you will **add**
`regexp`.

**Precondition**: plan 004 must already be applied — `util_test.go` exists with
`TestRedactTokenLikeText`. If it does not exist, STOP and run plan 004 first.

## Commands you will need

| Purpose     | Command                                       | Expected on success |
|-------------|-----------------------------------------------|---------------------|
| Format      | `gofmt -l util.go util_test.go`               | no output           |
| Vet         | `go vet ./...`                                 | exit 0              |
| Test (new)  | `go test ./... -run RedactTokenLikeText -v`   | PASS                |
| Test (all)  | `go test ./...`                                | `ok`, exit 0        |
| Build       | `go build -o codex-auth-broker .`             | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `util.go` — add `regexp` import, two package-level patterns, and rewrite
  `redactTokenLikeText`.
- `util_test.go` — add embedded-in-JSON and whitespace-preservation cases.

**Out of scope** (do NOT touch):
- The call sites (`proxy.go`, `auth.go`, `dashboard.go`, `requests.go`) — the
  function signature is unchanged, so callers need no edits. Do not modify them.
- The characterization cases plan 004 added — they must keep passing as-is. Do
  not weaken or delete them.
- Account-id redaction — short identifiers like `acct_...` are intentionally
  left readable (they are not the secret this defends; trying to redact them
  invites over-redaction). Do not add that here.

## Git workflow

- Branch: `advisor/005-harden-redaction`
- Commit message style matches the repo (imperative — e.g.
  `util: redact tokens embedded in JSON, not just whitespace-split fields`).
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Rewrite `redactTokenLikeText` with substring-matching regexes

In `util.go`, add `"regexp"` to the import block (alphabetical order: after
`path/filepath`, before `strings`). Add two package-level vars just above the
function, and replace the function body:

```go
var (
	// jwtPattern matches a 3-segment JWT anywhere in the text. JWTs always
	// begin with "eyJ" (base64url of '{"'); segments are base64url.
	jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	// opaqueTokenPattern matches a long opaque base64url-ish run (access /
	// refresh / client keys). 60 chars keeps ordinary words and identifiers
	// (model ids, account ids, error codes) below the bar.
	opaqueTokenPattern = regexp.MustCompile(`[A-Za-z0-9_-]{60,}`)
)

// redactTokenLikeText replaces token-like substrings (JWTs and long opaque
// keys) anywhere in value, including inside compact JSON, while preserving the
// surrounding text and whitespace. It is the last-resort backstop before error
// text reaches a client, a log line, or the request log.
func redactTokenLikeText(value string) string {
	value = jwtPattern.ReplaceAllString(value, "[redacted-jwt]")
	value = opaqueTokenPattern.ReplaceAllString(value, "[redacted-token]")
	return value
}
```

Order matters: replace JWTs first so a whole `eyJ...` becomes `[redacted-jwt]`
before the opaque pass runs (the placeholder itself can't re-match — its longest
base64url run is `redacted`, well under 60).

**Verify**: `go build -o codex-auth-broker .` → exit 0; `go vet ./...` → exit 0.

### Step 2: Re-run plan 004's characterization tests (must still pass)

```bash
go test ./... -run TestRedactTokenLikeText -v
```

All of plan 004's subtests must still PASS unchanged:
- whitespace-isolated jwt → `[redacted-jwt]`
- long opaque token → `[redacted-token]`
- ordinary error text preserved
- short identifiers preserved

If any now fail, STOP — the rewrite changed behavior the characterization tests
locked in.

### Step 3: Add hardened cases to `util_test.go`

Append a new test function (do not edit the existing one):

```go
func TestRedactTokenLikeTextHardened(t *testing.T) {
	jwt := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJicm9rZXItdGVzdCIsImV4cCI6OTk5OTk5OTk5OX0.c2lnbmF0dXJlLXBsYWNlaG9sZGVyLXZhbHVl"
	opaque := strings.Repeat("R", 72)

	t.Run("jwt embedded in compact json is redacted", func(t *testing.T) {
		in := `{"access_token":"` + jwt + `","expires_in":3600}`
		got := redactTokenLikeText(in)
		if strings.Contains(got, "c2lnbmF0dXJl") {
			t.Fatalf("jwt leaked through JSON redaction: %q", got)
		}
		if !strings.Contains(got, "[redacted-jwt]") {
			t.Fatalf("expected [redacted-jwt] in %q", got)
		}
		if !strings.Contains(got, `"expires_in":3600`) {
			t.Fatalf("non-token JSON structure should be preserved, got %q", got)
		}
	})

	t.Run("opaque key embedded in json is redacted", func(t *testing.T) {
		in := `{"refresh_token":"` + opaque + `"}`
		got := redactTokenLikeText(in)
		if strings.Contains(got, opaque) {
			t.Fatalf("opaque token leaked: %q", got)
		}
		if !strings.Contains(got, "[redacted-token]") {
			t.Fatalf("expected [redacted-token] in %q", got)
		}
	})

	t.Run("newlines are preserved", func(t *testing.T) {
		in := "line one\nline two"
		if got := redactTokenLikeText(in); got != in {
			t.Fatalf("redactTokenLikeText collapsed whitespace: %q, want %q", got, in)
		}
	})

	t.Run("realistic error json is not over-redacted", func(t *testing.T) {
		in := `{"error":{"message":"Unsupported parameter: conversation_id","type":"invalid_request_error"}}`
		if got := redactTokenLikeText(in); got != in {
			t.Fatalf("ordinary error JSON should pass through unchanged, got %q", got)
		}
	})
}
```

`util_test.go` already imports `strings` and `testing` (from plan 004), so no
new imports are needed.

**Verify**:
```bash
go test ./... -run RedactTokenLikeText -v   # both functions PASS
go test ./...                               # ok
go vet ./...                                # exit 0
gofmt -l util.go util_test.go               # no output
go build -o codex-auth-broker .             # exit 0
```

## Test plan

- Existing (plan 004) `TestRedactTokenLikeText` must keep passing unchanged —
  this proves the rewrite preserves the established behavior.
- New `TestRedactTokenLikeTextHardened` covers the regression this plan fixes:
  JWT and opaque tokens embedded in compact JSON, newline preservation, and a
  realistic error body that must NOT be over-redacted.
- Structural pattern: the existing `TestRedactTokenLikeText` in `util_test.go`.
- Verification: `go test ./...` → `ok`, with both redaction tests passing.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "regexp.MustCompile" util.go` returns exactly two matches
- [ ] `grep -n "strings.Fields" util.go` returns no matches (old approach gone)
- [ ] `grep -n "func TestRedactTokenLikeTextHardened" util_test.go` returns one match
- [ ] `go test ./... -run RedactTokenLikeText` passes (both the plan-004 and new functions)
- [ ] `go test ./...` exits 0
- [ ] `go vet ./...` exits 0
- [ ] `gofmt -l util.go util_test.go` prints nothing
- [ ] `go build -o codex-auth-broker .` exits 0
- [ ] `git status --porcelain` shows only `util.go` and `util_test.go` modified
- [ ] `plans/README.md` status row for plan 005 updated

## STOP conditions

Stop and report back (do not improvise) if:

- `util_test.go` does not exist (plan 004 was not applied) — run plan 004 first.
- The live `redactTokenLikeText` does not match the "Current state" excerpt
  (drift, or already hardened).
- Plan 004's `TestRedactTokenLikeText` fails after the rewrite — the new logic
  changed locked-in behavior; report which case and how, do not edit plan 004's
  assertions to make it pass.
- The "realistic error json is not over-redacted" case fails — that means the
  thresholds are too aggressive. Report it rather than loosening the test; the
  point of this plan is to redact tokens *without* mangling normal errors.

## Maintenance notes

- A reviewer should sanity-check the two regexes against a sample of real
  upstream error bodies if any are available, watching specifically for
  over-redaction (legitimate 60+ char identifiers turning into
  `[redacted-token]`). If that ever happens in practice, the fix is to tighten
  `opaqueTokenPattern` (e.g. require a known prefix), not to revert to the
  whitespace approach.
- The function is intentionally still a best-effort backstop, not a guarantee.
  The primary protection remains that the broker never *puts* tokens into
  responses (it extracts specific fields). This hardening reduces the blast
  radius if that primary protection is ever bypassed.
- Follow-up deferred: redacting account identifiers (`acct_...`,
  `ChatGPT-Account-Id` values) is out of scope here because the over-redaction
  risk is high and they are lower-sensitivity than tokens; revisit only if
  there is evidence they leak somewhere that matters.
