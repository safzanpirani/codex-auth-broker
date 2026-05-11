# Factory Droid

Factory Droid is the main target for this project.

Use:

```text
base_url: http://127.0.0.1:8317/v1
api_key: dummy
model: gpt-5.5(medium)
provider: openai
```

The proxy implements `/v1/responses`, which is the path Factory uses for the
OpenAI custom-provider flow.

Factory Droid `0.122` sends a few OpenAI SDK fields that the Codex backend
does not accept directly, including `prompt_cache_retention`. The proxy strips
those fields while preserving `prompt_cache_key`, so BYOK requests keep working
and still get model-side prompt-cache affinity.

## Recommended Models

```text
gpt-5.5(low)
gpt-5.5(medium)
gpt-5.5(high)
gpt-5.5(xhigh)
gpt-5.4
gpt-5.4-mini
gpt-5.3-codex
```

The suffix is converted to `reasoning.effort` before the request is sent to
Codex.

## Prompt Cache Check

Use a long repeated prompt. The first call usually has `cached_tokens: 0`; a
second identical-prefix call should show a large cached-token count.

```bash
node scripts/cache-check.js
```

You can also watch cache behavior in the broker dashboard:

```text
http://127.0.0.1:8317/dashboard
```

The request table shows `cached_tokens` and `total_tokens` when the upstream
final response includes usage. This works for non-streaming calls and for
normal streaming calls whose SSE stream reaches `response.completed`.

## Live Codex Usage

The dashboard reads Codex account usage from ChatGPT's wham usage endpoint with
the local Codex OAuth access token. This is the same usage family used by the
Pi Codex usage indicator: a primary short window and a secondary weekly window,
each with `used_percent`, `limit_window_seconds`, and `reset_at`.

If Factory Droid starts failing and `/dashboard/api/usage` returns 401 or 403,
run:

```bash
codex login status
codex login
```

on the broker machine, then restart the broker if needed.
