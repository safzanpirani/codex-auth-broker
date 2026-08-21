# Plan 003: Bound the persisted request log so it can't grow (or be rescanned) without limit

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 0a50ba9..HEAD -- persist.go main.go costs.go requests.go pricing_test.go README.md`
> If any in-scope file changed since this plan was written, compare the
> "Current state" excerpts against the live code before proceeding; on a
> mismatch, treat it as a STOP condition.

## Status

- **Priority**: P1
- **Effort**: M
- **Risk**: MED
- **Depends on**: none
- **Category**: perf
- **Planned at**: commit `0a50ba9`, 2026-06-26

## Why this matters

The persistent request log (`~/.codex-auth-broker/requests.jsonl`, on by
default) is **append-only with no upper bound** — `requestLogFile.append`
(`persist.go:35`) only ever writes; nothing rotates or truncates it. The
in-memory ring is bounded by `--request-log-limit` (default 1000), but the file
on disk grows forever.

That has two compounding costs:

1. **Unbounded disk growth** — over months of use the JSONL file climbs into
   hundreds of MB.
2. **An O(file size) scan every 30 seconds** — the dashboard polls
   `/dashboard/api/costs` on a 30s timer (`dashboard.go:977`), and
   `costSummary` reads and JSON-parses the *entire* file on every call when
   persistence is enabled (`costs.go:121-129`). As the file grows, this fixed
   30s background cost grows with it, forever.

Capping the file size fixes both at once: a bounded file means a bounded scan.
This plan adds a size cap with newest-wins compaction. The tradeoff — and it is
a real behavior change worth stating — is that once the cap is reached, the
oldest entries are dropped, so the dashboard's "all"-time cost window reflects
only the retained history rather than literally all requests ever made. The cap
defaults generously (64 MiB ≈ 150k+ requests) and can be disabled with `0`.

## Current state

- `persist.go` — `requestLogFile` and its `append`/load helpers. This is where
  the cap and compaction go.
- `main.go` — `config` struct + `loadConfig` (flags/env parsing) +
  `runServe` (which calls `openRequestLogFile`). Wiring for the new option.
- `costs.go` — `costSummary` scans the file; **no change needed** here, it
  benefits automatically once the file is bounded.
- `requests.go` — `requestLogStore.add` calls `s.persist.append`; **no change
  needed**.
- `pricing_test.go` — calls `openRequestLogFile` in two tests; signature change
  means these call sites must be updated, and the new behavior gets a test here.
- `README.md` — config table + Dashboard section document the option.

Excerpt — `persist.go:14-44` as it exists today:

```go
type requestLogFile struct {
	path string
	file *os.File
}

func openRequestLogFile(path string) (*requestLogFile, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o700); err != nil {
		return nil, fmt.Errorf("create request log directory: %w", err)
	}
	file, err := os.OpenFile(expanded, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open request log file: %w", err)
	}
	return &requestLogFile{path: expanded, file: file}, nil
}

// append writes one entry as a JSON line. Called with the store mutex held.
func (f *requestLogFile) append(entry requestLogEntry) {
	if f == nil || f.file == nil {
		return
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = f.file.Write(append(encoded, '\n'))
}
```

Excerpt — the single production call site, `main.go:101-113`:

```go
	if path := strings.TrimSpace(cfg.requestLogFile); path != "" {
		restored, maxID, err := loadPersistedEntries(path, cfg.requestLogLimit)
		if err != nil {
			return fmt.Errorf("load persisted request log: %w", err)
		}
		requests.restore(restored, maxID)
		persist, err := openRequestLogFile(path)
		if err != nil {
			return err
		}
		requests.persist = persist
		log.Printf("persisting request metadata (no prompts or tokens) to %s", persist.path)
	}
```

Excerpt — the two test call sites, `pricing_test.go:53` and `pricing_test.go:87`:

```go
	persist, err := openRequestLogFile(path)
```
```go
	persist, err := openRequestLogFile(filepath.Join(t.TempDir(), "requests.jsonl"))
```

Existing config-parsing conventions to match (from `main.go`):
- Integer env vars are parsed in `loadConfig` with `strconv` and return a
  wrapped error on failure (see the `CODEX_AUTH_BROKER_REQUEST_LOG_LIMIT`
  block, `main.go:197-203`).
- Each option has a default const at the top of `main.go` (e.g.
  `defaultRequestLogLimit = 1000`, `main.go:24`).
- Flags are registered with `fs.IntVar`/`fs.StringVar` and mirror the env
  default (see `main.go:209-221`).
- Validation lives at the end of `loadConfig` (e.g.
  `if cfg.requestLogLimit < 0 { ... }`, `main.go:256-258`).
- `strconv` and `fmt` are already imported in `main.go`. `persist.go` imports
  `bufio`, `encoding/json`, `fmt`, `os`, `path/filepath` — you will add
  `bytes`.

## Commands you will need

| Purpose     | Command                                          | Expected on success |
|-------------|--------------------------------------------------|---------------------|
| Format      | `gofmt -l persist.go main.go pricing_test.go`    | no output           |
| Vet         | `go vet ./...`                                    | exit 0              |
| Test (new)  | `go test ./... -run RequestLogFile -v`           | PASS                |
| Test (all)  | `go test ./...`                                   | `ok`, exit 0        |
| Build       | `go build -o codex-auth-broker .`                | exit 0              |

## Scope

**In scope** (the only files you should modify):
- `persist.go` — add `maxBytes`/`size` fields, size tracking, and `compact`.
- `main.go` — add the `requestLogMaxBytes` config field, default const, env +
  flag parsing, validation, and pass it to `openRequestLogFile`.
- `pricing_test.go` — update the two `openRequestLogFile` call sites and add the
  compaction test.
- `README.md` — document the new flag/env and the retention tradeoff.

**Out of scope** (do NOT touch, even though they look related):
- `costs.go` — `costSummary` is intentionally left as a full scan; it is now
  bounded by the file cap and needs no change. Do not add caching here in this
  plan.
- `requests.go` — `add` already routes through `persist.append`; no change.
- `dashboard.go` — the 30s poll cadence stays; the fix is the bounded file.
- The atomic-write pattern in `auth.go` (`writeAuthDocument`) — do not refactor
  it, but you may mirror its temp-file + rename + dir-sync shape in `compact`.

## Git workflow

- Branch: `advisor/003-bound-request-log`
- Commit message style matches the repo (imperative — e.g.
  `persist: cap the request log file with newest-wins compaction`).
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Add the size cap and compaction to `persist.go`

Add `"bytes"` to the import block (alphabetically first). Replace the
`requestLogFile` struct, `openRequestLogFile`, and `append` with the versions
below, and add the new `compact` method directly after `append`:

```go
type requestLogFile struct {
	path     string
	file     *os.File
	maxBytes int64
	size     int64
}

func openRequestLogFile(path string, maxBytes int64) (*requestLogFile, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o700); err != nil {
		return nil, fmt.Errorf("create request log directory: %w", err)
	}
	file, err := os.OpenFile(expanded, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open request log file: %w", err)
	}
	f := &requestLogFile{path: expanded, file: file, maxBytes: maxBytes}
	if stat, statErr := file.Stat(); statErr == nil {
		f.size = stat.Size()
	}
	return f, nil
}

// append writes one entry as a JSON line. Called with the store mutex held.
// When maxBytes is set and the file grows past it, the oldest entries are
// dropped so the file (and the cost scan that reads it) stays bounded.
func (f *requestLogFile) append(entry requestLogEntry) {
	if f == nil || f.file == nil {
		return
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	n, err := f.file.Write(append(encoded, '\n'))
	if err != nil {
		return
	}
	f.size += int64(n)
	if f.maxBytes > 0 && f.size > f.maxBytes {
		f.compact()
	}
}

// compact rewrites the log keeping only the newest entries that fit within
// ~3/4 of maxBytes, leaving headroom before the next compaction. Called with
// the store mutex held (via append). On any error it leaves the existing file
// in place and returns: an over-cap file is preferable to a lost or corrupt
// one. The newest entries are always retained, so restart IDs stay monotonic.
func (f *requestLogFile) compact() {
	if f == nil || f.maxBytes <= 0 {
		return
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return
	}
	var kept [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			kept = append(kept, line)
		}
	}
	target := f.maxBytes * 3 / 4
	var total int64
	start := len(kept)
	for i := len(kept) - 1; i >= 0; i-- {
		total += int64(len(kept[i])) + 1 // +1 for the newline
		if total > target && start != len(kept) {
			break
		}
		start = i
		if total > target {
			break
		}
	}
	kept = kept[start:]

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".requests.jsonl.*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	var written int64
	for _, line := range kept {
		if _, err := tmp.Write(line); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return
		}
		if _, err := tmp.Write([]byte{'\n'}); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return
		}
		written += int64(len(line)) + 1
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	_ = f.file.Close()
	if err := os.Rename(tmpName, f.path); err != nil {
		os.Remove(tmpName)
		if reopened, rerr := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); rerr == nil {
			f.file = reopened
		} else {
			f.file = nil
		}
		return
	}
	reopened, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		f.file = nil // disable further persistence rather than write to a closed handle
		return
	}
	f.file = reopened
	f.size = written
	_ = syncDir(dir)
}
```

Note: the `start != len(kept)` guard in the retention loop guarantees at least
one entry is always kept even if a single line is larger than `target`.
`syncDir` already exists in `util.go:130`.

**Verify**: `go build -o codex-auth-broker .` → will fail with
"not enough arguments in call to openRequestLogFile" until Steps 2–3 update the
call sites. That is expected; proceed.

### Step 2: Wire the option through `main.go`

1. Add a default const next to the others near the top of `main.go` (after
   `defaultRequestLogLimit = 1000`):

   ```go
   defaultRequestLogMaxBytes = 64 * 1024 * 1024
   ```

2. Add the field to the `config` struct (after `requestLogFile string`):

   ```go
   requestLogMaxBytes int64
   ```

3. In `loadConfig`'s initial `cfg := config{...}` literal, set the default:

   ```go
   requestLogMaxBytes: defaultRequestLogMaxBytes,
   ```

4. Add env parsing alongside the existing
   `CODEX_AUTH_BROKER_REQUEST_LOG_LIMIT` block (mirror its shape, using
   `strconv.ParseInt(value, 10, 64)`):

   ```go
   if value := strings.TrimSpace(os.Getenv("CODEX_AUTH_BROKER_REQUEST_LOG_MAX_BYTES")); value != "" {
       parsed, err := strconv.ParseInt(value, 10, 64)
       if err != nil {
           return cfg, fmt.Errorf("invalid CODEX_AUTH_BROKER_REQUEST_LOG_MAX_BYTES: %w", err)
       }
       cfg.requestLogMaxBytes = parsed
   }
   ```

5. Register the flag next to `--request-log-file`:

   ```go
   fs.Int64Var(&cfg.requestLogMaxBytes, "request-log-max-bytes", cfg.requestLogMaxBytes, "max size of the persistent request log in bytes before oldest entries are dropped; 0 disables the cap")
   ```

6. Add validation near the `requestLogLimit < 0` check at the end of
   `loadConfig`:

   ```go
   if cfg.requestLogMaxBytes < 0 {
       return cfg, errors.New("request-log-max-bytes must be zero or greater")
   }
   ```

   (`errors` is already imported in `main.go`.)

7. Update the `openRequestLogFile` call in `runServe` (`main.go:107`):

   ```go
   persist, err := openRequestLogFile(path, cfg.requestLogMaxBytes)
   ```

**Verify**: `go build -o codex-auth-broker .` → still fails only on the two
test call sites (fixed in Step 3). Run `go vet ./...` against non-test issues
mentally; the real gate is after Step 3.

### Step 3: Update the two test call sites and add a compaction test

In `pricing_test.go`, update both `openRequestLogFile` calls to pass `0`
(unlimited — preserves the existing tests' behavior exactly):

- `pricing_test.go:53` → `persist, err := openRequestLogFile(path, 0)`
- `pricing_test.go:87` →
  `persist, err := openRequestLogFile(filepath.Join(t.TempDir(), "requests.jsonl"), 0)`

Then add this test (it uses `os`, `path/filepath`, `testing`, all already
imported in `pricing_test.go`):

```go
func TestRequestLogFileRotatesWhenOverMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	const maxBytes = 4 * 1024
	persist, err := openRequestLogFile(path, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	// Large in-memory limit so the on-disk cap, not the ring, is the bound.
	store := newRequestLogStore(1_000_000)
	store.persist = persist
	const total = 3000
	for i := 0; i < total; i++ {
		store.add(requestLogEntry{Method: "POST", Path: "/v1/responses", Model: "gpt-5.5", Status: 200})
	}
	if err := persist.file.Sync(); err != nil {
		t.Fatal(err)
	}

	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() > maxBytes {
		t.Fatalf("log file size %d exceeds cap %d after compaction", stat.Size(), maxBytes)
	}

	entries, maxID, err := loadPersistedEntries(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected retained entries after compaction, got none")
	}
	if maxID != total {
		t.Fatalf("maxID = %d, want %d (newest id must survive compaction)", maxID, total)
	}
	if last := entries[len(entries)-1]; last.ID != total {
		t.Fatalf("last retained entry id = %d, want %d", last.ID, total)
	}
	if first := entries[0]; first.ID <= 1 {
		t.Fatalf("oldest entries should have been dropped; first id = %d", first.ID)
	}
}
```

**Verify**:
```bash
go test ./... -run RequestLogFile -v   # new + existing persistence tests PASS
go test ./...                          # ok
go vet ./...                           # exit 0
go build -o codex-auth-broker .        # exit 0
```

### Step 4: Document the option in `README.md`

1. In the Configuration flag/env table (`README.md:276-291`), add a row after
   the `--request-log-file` row:

   ```
   | `--request-log-max-bytes` | `CODEX_AUTH_BROKER_REQUEST_LOG_MAX_BYTES` | `67108864` (64 MiB; `0` disables the cap) |
   ```

2. In the Dashboard section, update the paragraph describing the JSONL log
   (`README.md:184-191`) to note that the file is capped by
   `--request-log-max-bytes` and that, once the cap is hit, the oldest entries
   are dropped — so the dashboard's "all"-time cost window reflects retained
   history, not literally every request ever made. Keep the existing sentence
   that neither store ever contains prompts/completions/tokens.

**Verify**: `grep -n "request-log-max-bytes" README.md` → at least 2 matches
(table + prose).

## Test plan

- New test: `TestRequestLogFileRotatesWhenOverMaxBytes` in `pricing_test.go`
  — writes many entries past a tiny 4 KiB cap and asserts (a) the file stays
  ≤ cap, (b) the newest entry id survives, (c) the oldest entries were dropped.
- Regression: the existing `TestRequestLogPersistenceRoundTrip` and
  `TestCostSummaryWindows` must still pass with the `, 0` (unlimited) argument
  — confirming the no-cap path is unchanged.
- Structural pattern to follow: the existing `TestRequestLogPersistenceRoundTrip`
  in `pricing_test.go`.
- Verification: `go test ./...` → `ok`, including the new test.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -n "func (f \*requestLogFile) compact" persist.go` returns one match
- [ ] `grep -n "maxBytes int64" persist.go` returns one match
- [ ] `grep -rn "openRequestLogFile(" *.go | grep -v "func openRequestLogFile"` shows every call passing two arguments
- [ ] `grep -n "request-log-max-bytes" main.go` returns one match (flag)
- [ ] `grep -n "CODEX_AUTH_BROKER_REQUEST_LOG_MAX_BYTES" main.go` returns one match (env)
- [ ] `grep -n "request-log-max-bytes" README.md` returns ≥ 2 matches
- [ ] `go vet ./...` exits 0
- [ ] `go test ./...` exits 0; `TestRequestLogFileRotatesWhenOverMaxBytes` passes
- [ ] `gofmt -l persist.go main.go pricing_test.go` prints nothing
- [ ] `go build -o codex-auth-broker .` exits 0
- [ ] `git status --porcelain` shows only `persist.go`, `main.go`, `pricing_test.go`, `README.md` modified
- [ ] `plans/README.md` status row for plan 003 updated

## STOP conditions

Stop and report back (do not improvise) if:

- Any "Current state" excerpt does not match the live code (drift).
- The compaction test fails because the file size still exceeds the cap after
  the loop — do **not** loosen the assertion; the compaction logic is wrong and
  needs fixing or reporting.
- `go test ./...` shows a failure in `TestRequestLogPersistenceRoundTrip` or
  `TestCostSummaryWindows` after you add the `, 0` argument — that means the
  unlimited path changed behavior, which it must not.
- Implementing the cap appears to require changing `costs.go`, `requests.go`,
  or `dashboard.go` — it should not; stop and report what forced it.
- You discover the assumption "persistence is enabled by default" is false in
  the live code (it is controlled by `--request-log-file`, default non-empty).

## Maintenance notes

- **Follow-up explicitly deferred**: the 30s cost scan is now *bounded* but
  still O(file size). If usage grows enough that even a 64 MiB scan every 30s
  matters, the next step is incremental rolling aggregates (update window totals
  as entries are added; scan the file only once at startup) and/or a short-TTL
  in-memory cache of `costSummary`. That is a larger change and out of scope
  here.
- A reviewer should scrutinize `compact` for partial-write safety: it must never
  leave `f.path` truncated or empty on error. Confirm the temp-file + rename
  path and that every error branch either keeps the original file or reopens it.
- If `--request-log-limit` (in-memory ring) is ever raised far above what the
  byte cap can hold, the dashboard's retained count will be limited by the byte
  cap on restart — that interaction is expected.
- The default (64 MiB) was chosen to hold 150k+ entries; if entries gain many
  more fields, revisit the default so it still covers a reasonable history.
