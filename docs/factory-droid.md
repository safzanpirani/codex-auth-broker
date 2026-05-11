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
```

The suffix is converted to `reasoning.effort` before the request is sent to
Codex.

## Prompt Cache Check

Use a long repeated prompt. The first call usually has `cached_tokens: 0`; a
second identical-prefix call should show a large cached-token count.

```bash
node scripts/cache-check.js
```
