# Linux Systemd

This project supports Linux as a first-class runtime.

## User Service

Install the binary:

```bash
sudo install -m 0755 codex-auth-broker /usr/local/bin/codex-auth-broker
```

Create a client key:

```bash
mkdir -p ~/.codex-auth-broker
openssl rand -hex 32 > ~/.codex-auth-broker/client.key
chmod 600 ~/.codex-auth-broker/client.key
```

Install the service:

```bash
./scripts/install-systemd-user.sh
```

The installer resolves its assets relative to the script and writes the
selected binary path into the unit. Set `BIN=/path/to/codex-auth-broker` when
the binary does not live at `/usr/local/bin/codex-auth-broker`.

Check it:

```bash
systemctl --user status codex-auth-broker.service
journalctl --user -u codex-auth-broker.service -f
curl -fsS http://127.0.0.1:8317/healthz
```

## Remote Access

Prefer Tailscale:

```ini
Environment=CODEX_AUTH_BROKER_LISTEN=100.x.y.z:8317
```

Keep `CODEX_AUTH_BROKER_API_KEY_FILE` enabled if binding to a private network
interface.

For a shared deployment, prefer `CODEX_AUTH_BROKER_KEYS_FILE` with named
client/admin keys as documented in the README. Set
`CODEX_AUTH_BROKER_MAX_CONCURRENT` to cap simultaneous upstream calls when
multiple clients share the broker.
