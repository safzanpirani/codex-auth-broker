const http = require("http");

const endpoint = process.env.CODEX_AUTH_BROKER_URL || "http://127.0.0.1:8317/v1/responses";
const apiKey = process.env.CODEX_AUTH_BROKER_API_KEY || "dummy";
const prefix = Array.from({ length: 1800 }, (_, i) =>
  `CACHEPREFIX${String(i).padStart(4, "0")}: keep this exact static sentence for prompt caching.`
).join("\n");

const body = {
  model: process.env.CODEX_AUTH_BROKER_MODEL || "gpt-5.5(low)",
  instructions: "Reply with exactly OK.",
  input: `${prefix}\n\nFinal user task: Reply exactly OK.`,
  stream: false,
  prompt_cache_key: "factory-cache-check",
};

function post(payload) {
  return new Promise((resolve, reject) => {
    const raw = JSON.stringify(payload);
    const url = new URL(endpoint);
    const req = http.request(
      {
        hostname: url.hostname,
        port: url.port || 80,
        path: url.pathname + url.search,
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${apiKey}`,
          "Content-Length": Buffer.byteLength(raw),
        },
      },
      (res) => {
        let data = "";
        res.setEncoding("utf8");
        res.on("data", (chunk) => (data += chunk));
        res.on("end", () => {
          try {
            resolve({ status: res.statusCode, json: JSON.parse(data), raw: data });
          } catch (error) {
            reject(new Error(`invalid JSON response: ${error.message}\n${data.slice(0, 500)}`));
          }
        });
      }
    );
    req.on("error", reject);
    req.write(raw);
    req.end();
  });
}

function summarize(label, out) {
  const usage = out.json.usage || {};
  const details = usage.input_tokens_details || usage.prompt_tokens_details || {};
  const text = (out.json.output || [])
    .flatMap((item) => item.content || [])
    .map((part) => part.text || "")
    .join("");
  console.log(
    JSON.stringify({
      label,
      http_status: out.status,
      response_status: out.json.status,
      text,
      input_tokens: usage.input_tokens || usage.prompt_tokens || null,
      total_tokens: usage.total_tokens || null,
      cached_tokens: details.cached_tokens ?? null,
      response_id: out.json.id || null,
    })
  );
}

(async () => {
  const first = await post(body);
  summarize("first", first);
  await new Promise((resolve) => setTimeout(resolve, 1500));
  const second = await post(body);
  summarize("second", second);
})();
