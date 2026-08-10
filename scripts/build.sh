#!/usr/bin/env bash
# Build the broker with version metadata stamped in, so a running instance can
# say what it is. Without this, /healthz reports commit="" and the only way to
# tell whether a deployed binary carries a given fix is to diff file sizes.
#
#   ./scripts/build.sh                      # host platform -> ./codex-auth-broker
#   ./scripts/build.sh linux amd64 /tmp/bin # cross-compile to an explicit path
set -euo pipefail

cd "$(dirname "$0")/.."

goos="${1:-$(go env GOOS)}"
goarch="${2:-$(go env GOARCH)}"
out="${3:-./codex-auth-broker}"

version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
# Tracked changes only. Untracked files here are build artifacts and scratch
# dirs (codex-auth-broker.bak-*, plans/), and they say nothing about whether the
# compiled source differs from the commit.
if ! git diff --quiet HEAD 2>/dev/null; then
  commit="${commit}-dirty"
fi
# Plain UTC: `date -r` means "epoch seconds" on BSD/macOS but "read this file"
# on GNU, so it is not portable enough to be worth the reproducibility.
date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

pkg="github.com/safzanpirani/codex-auth-broker"
GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags "-s -w -X main.version=${version} -X main.commit=${commit} -X main.date=${date}" \
  -o "$out" .

echo "built $out ($goos/$goarch) version=${version} commit=${commit} date=${date}"
