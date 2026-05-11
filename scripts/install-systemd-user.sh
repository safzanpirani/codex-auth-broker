#!/usr/bin/env bash
set -euo pipefail

BIN="${BIN:-/usr/local/bin/codex-auth-broker}"
SERVICE_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
STATE_DIR="$HOME/.codex-auth-broker"
UNIT="$SERVICE_DIR/codex-auth-broker.service"

if [[ ! -x "$BIN" ]]; then
  echo "missing executable: $BIN" >&2
  echo "build and install it first, for example:" >&2
  echo "  go build -o codex-auth-broker ." >&2
  echo "  sudo install -m 0755 codex-auth-broker $BIN" >&2
  exit 1
fi

mkdir -p "$SERVICE_DIR" "$STATE_DIR"
chmod 700 "$STATE_DIR"

if [[ ! -f "$STATE_DIR/client.key" ]]; then
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32 > "$STATE_DIR/client.key"
  else
    dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n' > "$STATE_DIR/client.key"
    printf '\n' >> "$STATE_DIR/client.key"
  fi
  chmod 600 "$STATE_DIR/client.key"
fi

cp packaging/systemd/codex-auth-broker.service "$UNIT"
systemctl --user daemon-reload
systemctl --user enable --now codex-auth-broker.service
systemctl --user status codex-auth-broker.service --no-pager

