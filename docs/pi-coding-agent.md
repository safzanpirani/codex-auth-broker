# Pi Coding Agent

Pi can use the broker through its Codex Responses adapter, including the
`websocket` and `websocket-cached` transports. The broker exposes
`/v1/codex/responses` as an alias for `/v1/responses` because Pi appends
`/codex/responses` to the configured `/v1` base URL.

## 1. Create a Pi client credential

Pi's `openai-codex-responses` adapter extracts a ChatGPT account id from its API
key before making a request. The broker replaces the client credential with the
real local Codex OAuth token, so the client credential does not need to contain
or sign any real account data. It only needs the JWT shape Pi expects.

Create a random, broker-only credential without printing it:

```bash
install -d -m 700 ~/.codex-auth-broker

header=$(printf '%s' '{"alg":"none","typ":"JWT"}' | openssl base64 -A | tr '+/' '-_' | tr -d '=')
account_id=$(uuidgen)
payload=$(printf '{"https://api.openai.com/auth":{"chatgpt_account_id":"%s"}}' "$account_id" | openssl base64 -A | tr '+/' '-_' | tr -d '=')
signature=$(openssl rand -hex 16)

(umask 077 && printf '%s.%s.%s\n' "$header" "$payload" "$signature" > ~/.codex-auth-broker/pi-client.jwt)
unset header account_id payload signature
```

This is not a Codex access, refresh, or identity token. If the broker uses
`--api-key-file`, point it at this same file so the JWT-shaped value is also the
actual client authentication key:

```bash
./codex-auth-broker serve \
  --listen 127.0.0.1:8317 \
  --api-key-file ~/.codex-auth-broker/pi-client.jwt
```

For another device on your Tailnet, bind to the Mac's Tailscale IP instead of
`127.0.0.1`. Do not expose this listener directly to the public internet.

## 2. Configure Pi's provider

Add a provider to `~/.pi/agent/models.json`. Use localhost when Pi and the
broker run on the same machine, or replace it with the broker's private
Tailscale IP:

```json
{
  "providers": {
    "codex-auth-broker": {
      "baseUrl": "http://127.0.0.1:8317/v1",
      "api": "openai-codex-responses",
      "apiKey": "!cat ~/.codex-auth-broker/pi-client.jwt",
      "authHeader": true,
      "models": [
        {
          "id": "gpt-5.5",
          "name": "GPT-5.5 (Codex Auth Broker)",
          "reasoning": true,
          "input": ["text", "image"],
          "contextWindow": 272000,
          "maxTokens": 128000,
          "cost": {
            "input": 5,
            "output": 30,
            "cacheRead": 0.5,
            "cacheWrite": 0
          }
        }
      ]
    }
  }
}
```

The important custom value is `"api": "openai-codex-responses"`. The generic
`openai-responses` adapter uses HTTP and does not activate Pi's Codex WebSocket
transport.

If `models.json` already contains other providers, merge only the
`codex-auth-broker` object instead of replacing the whole file. Add other model
ids advertised by the broker with `GET /v1/models`.

## 3. Enable cached WebSocket transport

Set these fields in `~/.pi/agent/settings.json`:

```json
{
  "defaultProvider": "codex-auth-broker",
  "defaultModel": "gpt-5.5",
  "transport": "websocket-cached"
}
```

The available transport modes are:

- `sse`: one HTTP streaming request per turn.
- `websocket`: Responses events over WebSocket.
- `websocket-cached`: reuse the connection and, after the first turn, send new
  input plus `previous_response_id` instead of the full conversation.

Cached reuse is scoped to one running Pi session. Separate `pi -p` processes
open separate connections and cannot demonstrate the second-turn delta.

`websocket-cached` is conversation-state reuse, not itself a prompt-cache TTL
setting. Pi sends a stable `prompt_cache_key`; after the first turn it can also
send only new input with `previous_response_id` on the reused socket. The
ChatGPT Codex backend applies prompt-cache retention server-side.

OpenAI documents GPT-5.5 and GPT-5.4 as supporting extended prompt retention
for up to 24 hours. GPT-5.6 uses the newer cache system: its documented minimum
TTL is 30 minutes, and entries may remain eligible longer. The ChatGPT Codex
endpoint rejects both public request controls, so the broker strips them while
preserving the cache key. `compat.supportsLongCacheRetention` can remain unset
or `false`; Pi's `openai-codex-responses` adapter does not need it to receive
server-managed caching.

Pi derives its cache key from the session id. Reuse or resume the same Pi
session to keep that cache namespace. A fresh session gets a new key, so its
first turn normally reports zero cached tokens even when its visible prompt is
similar.

Current Pi versions zstd-compress Codex SSE request bodies. Broker versions
with Pi SSE support decode `Content-Encoding: zstd` before applying request
normalization.

Restart existing Pi processes after changing the provider or environment. A
new Pi process reads `models.json` and `settings.json` immediately.

## 4. Verify it

Check the broker first:

```bash
curl -fsS http://127.0.0.1:8317/healthz
```

Run a minimal Pi request:

```bash
pi --provider codex-auth-broker \
  --model gpt-5.5 \
  --thinking off \
  --no-tools \
  --no-session \
  -p 'Reply exactly: PI_WEBSOCKET_OK'
```

The request history at `/dashboard` or `/dashboard/api/requests` should show:

```text
method: WS
path: /v1/codex/responses
status: 200
```

The broker log records connection lifecycle without prompt or token data:

```bash
tail -f ~/.codex/codex-openai-adapter.err.log
```

Look for `responses websocket connected`. To exercise cached deltas, continue
for at least two turns inside one interactive Pi session.

## Troubleshooting

### Pi still shows HTTP POST

Confirm the provider has `"api": "openai-codex-responses"`, not
`"openai-responses"`, and restart Pi. An already-running Pi process may retain
the provider definition it loaded at startup.

### `Failed to extract accountId from token`

Pi did not receive the JWT-shaped client credential. Confirm the file exists
and has three dot-separated segments without printing its contents:

```bash
awk -F. '{print "segments=" NF}' ~/.codex-auth-broker/pi-client.jwt
```

The result should be `segments=3`. Also confirm that `apiKey` uses the `!cat`
command shown above.

### WebSocket returns 404

Update the broker to a version that exposes `GET /v1/codex/responses`. The
canonical `GET /v1/responses` route and Pi compatibility alias use the same
handler.

### Connection closes after a long session

The upstream WebSocket has a hard 60-minute lifetime. Pi must reconnect after
`websocket_connection_limit_reached`; a new connection can continue normally.
