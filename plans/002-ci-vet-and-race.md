# Plan 002: Run `go vet` and the race detector in CI

> **Executor instructions**: Follow this plan step by step. Run every
> verification command and confirm the expected result before moving to the
> next step. If anything in the "STOP conditions" section occurs, stop and
> report — do not improvise. When done, update the status row for this plan
> in `plans/README.md` — unless a reviewer dispatched you and told you they
> maintain the index.
>
> **Drift check (run first)**: `git diff --stat 0a50ba9..HEAD -- .github/workflows/ci.yml`
> If the workflow changed since this plan was written, compare the "Current
> state" excerpt against the live file before proceeding; on a mismatch, treat
> it as a STOP condition.

## Status

- **Priority**: P2
- **Effort**: S
- **Risk**: LOW
- **Depends on**: none
- **Category**: dx
- **Planned at**: commit `0a50ba9`, 2026-06-26

## Why this matters

CI currently formats, tests, and builds, but never runs `go vet` and never runs
the test suite under the race detector. This is a concurrency-bearing codebase:
`authManager.current` takes a cross-process `flock` per request (`auth.go:61`),
and `requestLogStore` guards shared state with a `sync.Mutex` while a background
file writer appends under that lock (`requests.go`, `persist.go`). A data race
or a vet-class mistake (bad `Printf` verb, lost struct copy of a lock, etc.)
would ship undetected today. `go vet` and `go test -race` are both first-party,
zero-dependency, and cheap on a repo this size — adding them closes a real gap
in the safety net for the exact code most likely to harbor a subtle bug.

## Current state

- `.github/workflows/ci.yml` — the only CI workflow. Matrix builds on
  `ubuntu-latest` and `macos-latest`.

Excerpt as it exists today, `.github/workflows/ci.yml` (full file):

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:

jobs:
  test:
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: gofmt -w *.go && git diff --exit-code
      - run: go test ./...
      - run: go build -o codex-auth-broker .
```

Locally, both new commands already pass at this commit (verified during the
audit): `go vet ./...` exits 0, and the suite passes under `-race`.

## Commands you will need

| Purpose        | Command                  | Expected on success            |
|----------------|--------------------------|--------------------------------|
| Vet            | `go vet ./...`           | exit 0, no output              |
| Test w/ race   | `go test -race ./...`    | `ok`, exit 0, no race reports  |
| Format check   | `gofmt -l *.go`          | no output                      |
| Build          | `go build -o codex-auth-broker .` | exit 0                |

## Scope

**In scope** (the only file you should modify):
- `.github/workflows/ci.yml`

**Out of scope** (do NOT touch):
- Any `.go` source — this plan only changes CI. If `go vet` or `go test -race`
  reports a problem in the source, that is a STOP condition (see below), not a
  source edit to make here.
- Do not add third-party actions or linters (e.g. golangci-lint). Keeping CI to
  first-party Go tooling is deliberate; a separate plan can introduce a linter
  if wanted.
- Do not change the OS matrix, Go version source (`go-version-file: go.mod`), or
  the `gofmt`/build steps.

## Git workflow

- Branch: `advisor/002-ci-vet-and-race`
- Commit message style matches the repo (imperative — e.g.
  `ci: add go vet and run tests under the race detector`).
- Do NOT push or open a PR unless the operator instructed it.

## Steps

### Step 1: Confirm both commands pass locally before editing CI

Run them now so a later CI failure can't be blamed on the workflow edit:

```bash
go vet ./...
go test -race ./...
```

**Verify**: both exit 0; `go test -race ./...` prints `ok` and no
`WARNING: DATA RACE` block. If either fails, STOP (see STOP conditions).

### Step 2: Add a `go vet` step and switch the test step to `-race`

Edit `.github/workflows/ci.yml` so the `steps:` list reads:

```yaml
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: gofmt -w *.go && git diff --exit-code
      - run: go vet ./...
      - run: go test -race ./...
      - run: go build -o codex-auth-broker .
```

Changes, precisely:
1. Insert a new `- run: go vet ./...` line after the `gofmt` step and before
   the test step.
2. Change `- run: go test ./...` to `- run: go test -race ./...`.

Leave everything else (name, triggers, matrix, checkout, setup-go, gofmt,
build) byte-for-byte unchanged.

**Verify**: the file parses as YAML and contains both new commands:

```bash
grep -n "go vet ./..." .github/workflows/ci.yml      # one match
grep -n "go test -race ./..." .github/workflows/ci.yml  # one match
grep -c "go test ./..." .github/workflows/ci.yml      # 0 — the non-race form is gone
```

Expected: the first two greps each return one line; the third returns `0`.

## Test plan

There is no Go test to add — this plan changes CI only. The "test" is that the
two commands the workflow now runs both pass locally at this commit, which you
confirmed in Step 1. Re-run them once more after editing to be safe:

```bash
go vet ./... && go test -race ./...
```

Expected: exit 0, no race warnings.

## Done criteria

Machine-checkable. ALL must hold:

- [ ] `grep -c "go vet ./..." .github/workflows/ci.yml` returns `1`
- [ ] `grep -c "go test -race ./..." .github/workflows/ci.yml` returns `1`
- [ ] `grep -c "go test ./..." .github/workflows/ci.yml` returns `0` (no plain form remains)
- [ ] `go vet ./...` exits 0
- [ ] `go test -race ./...` exits 0 with no `DATA RACE` warnings
- [ ] `git status --porcelain` shows only `.github/workflows/ci.yml` modified
- [ ] `plans/README.md` status row for plan 002 updated

## STOP conditions

Stop and report back (do not improvise) if:

- `go vet ./...` reports any problem. Do **not** edit source to silence it under
  this plan — report the exact vet output so it can be triaged as its own fix.
- `go test -race ./...` reports a `DATA RACE`. This is a genuine bug worth its
  own plan; capture the full race report and stop. Do **not** disable the race
  detector or skip the test to make CI green.
- The live `ci.yml` does not match the "Current state" excerpt (it has drifted).

## Maintenance notes

- `go test -race` roughly doubles test time and memory; on this small suite that
  is negligible, but keep it in mind if the suite grows large.
- If a future plan adds `golangci-lint` or `staticcheck`, add it as a separate
  step rather than replacing `go vet` — they catch different classes of issues.
- A reviewer should confirm no `.go` files were modified in this change.
