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
mkdir -p ~/.config/systemd/user
cp packaging/systemd/codex-auth-broker.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now codex-auth-broker.service
```

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

