package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

func (p *responsesProxy) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML)
}

func (p *responsesProxy) handleDashboardRequests(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := requestLimitFromQuery(r, 250)
		writeJSON(w, http.StatusOK, p.requests.snapshot(limit))
	case http.MethodDelete:
		p.requests.clear()
		writeJSON(w, http.StatusOK, map[string]any{
			"status":       "cleared",
			"generated_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
	default:
		writeProxyError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (p *responsesProxy) handleCodexUsage(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	usage, status, err := p.fetchCodexUsage(r.Context())
	if err != nil {
		writeProxyError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (p *responsesProxy) fetchCodexUsage(ctx context.Context) (map[string]any, int, error) {
	access, err := p.auth.current(ctx)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("Codex auth failed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.usageURL, nil)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("build usage request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+access.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-auth-broker/"+valueOr(version, "dev"))
	if access.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", access.AccountID)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("usage request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("usage API returned HTTP %d%s", resp.StatusCode, dashboardErrorSuffix(body))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("usage API returned invalid JSON: %w", err)
	}
	parsed["_broker"] = map[string]any{
		"fetched_at":         time.Now().UTC().Format(time.RFC3339Nano),
		"account_id_present": access.AccountID != "",
		"source":             "chatgpt.com/backend-api/wham/usage",
	}
	return parsed, http.StatusOK, nil
}

func dashboardErrorSuffix(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err == nil {
		if message := stringField(parsed, "detail"); message != "" {
			return ": " + redactTokenLikeText(message)
		}
		if message := stringField(parsed, "message"); message != "" {
			return ": " + redactTokenLikeText(message)
		}
		if message := stringField(parsed, "error"); message != "" {
			return ": " + redactTokenLikeText(message)
		}
	}
	return ": " + truncateLogField(redactTokenLikeText(string(body)), 240)
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>codex-auth-broker</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #0d1117;
      --panel: #111820;
      --panel-2: #151f2a;
      --text: #eef4fb;
      --muted: #8a96a8;
      --line: #253140;
      --accent: #51d38a;
      --warn: #f5b84b;
      --bad: #f26d6d;
      --blue: #6da8ff;
      --mono: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
      --sans: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      min-height: 100svh;
      background: var(--bg);
      color: var(--text);
      font-family: var(--sans);
      letter-spacing: 0;
    }
    button, input {
      font: inherit;
    }
    .shell {
      display: grid;
      grid-template-columns: 260px minmax(0, 1fr);
      min-height: 100svh;
    }
    aside {
      border-right: 1px solid var(--line);
      background: #0a0f15;
      padding: 22px;
      display: flex;
      flex-direction: column;
      gap: 24px;
    }
    main {
      min-width: 0;
      padding: 22px;
      display: flex;
      flex-direction: column;
      gap: 18px;
    }
    .brand {
      display: flex;
      flex-direction: column;
      gap: 6px;
    }
    .brand h1 {
      margin: 0;
      font-size: 20px;
      font-weight: 720;
    }
    .brand p, .muted {
      margin: 0;
      color: var(--muted);
      font-size: 13px;
      line-height: 1.45;
    }
    .navstat {
      display: grid;
      gap: 10px;
    }
    .statline {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      font-family: var(--mono);
      font-size: 12px;
      color: var(--muted);
    }
    .statline strong {
      color: var(--text);
      font-weight: 650;
    }
    .keybox {
      display: grid;
      gap: 8px;
    }
    .keybox input, .toolbar input {
      width: 100%;
      min-width: 0;
      border: 1px solid var(--line);
      border-radius: 6px;
      background: #0d141c;
      color: var(--text);
      padding: 9px 10px;
      outline: none;
    }
    .keybox input:focus, .toolbar input:focus {
      border-color: var(--blue);
    }
    .row {
      display: flex;
      gap: 8px;
      align-items: center;
      flex-wrap: wrap;
    }
    .button {
      border: 1px solid var(--line);
      background: var(--panel-2);
      color: var(--text);
      border-radius: 6px;
      padding: 8px 10px;
      cursor: pointer;
      min-height: 34px;
    }
    .button:hover {
      border-color: #3b4a5d;
      background: #1a2632;
    }
    .button.danger {
      color: #ffdede;
      border-color: #553038;
      background: #241418;
    }
    .topbar {
      display: flex;
      justify-content: space-between;
      gap: 16px;
      align-items: flex-start;
    }
    .topbar h2 {
      margin: 0 0 6px;
      font-size: 22px;
      font-weight: 720;
    }
    .pill {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      min-height: 26px;
      border: 1px solid var(--line);
      border-radius: 999px;
      padding: 4px 9px;
      color: var(--muted);
      background: #0c131b;
      font-size: 12px;
      white-space: nowrap;
    }
    .dot {
      width: 7px;
      height: 7px;
      border-radius: 99px;
      background: var(--muted);
    }
    .ok .dot { background: var(--accent); }
    .warn .dot { background: var(--warn); }
    .bad .dot { background: var(--bad); }
    .grid {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 14px;
    }
    .panel {
      border: 1px solid var(--line);
      background: var(--panel);
      border-radius: 8px;
      overflow: hidden;
    }
    .panel-head {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      align-items: center;
      padding: 14px 16px;
      border-bottom: 1px solid var(--line);
    }
    .panel-head h3 {
      margin: 0;
      font-size: 14px;
      font-weight: 700;
    }
    .panel-body {
      padding: 16px;
    }
    .usage {
      display: grid;
      gap: 14px;
    }
    .usage-title {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 8px;
    }
    .usage-title strong {
      font-family: var(--mono);
      font-size: 20px;
    }
    .bar {
      position: relative;
      height: 12px;
      border-radius: 999px;
      background: #0b1219;
      border: 1px solid #1f2b38;
      overflow: hidden;
    }
    .bar span {
      display: block;
      height: 100%;
      width: 0%;
      background: var(--accent);
      transition: width 180ms ease;
    }
    .bar i {
      position: absolute;
      top: -3px;
      width: 2px;
      height: 18px;
      background: var(--text);
      opacity: .7;
      left: 0%;
      transition: left 180ms ease;
    }
    .toolbar {
      display: grid;
      grid-template-columns: minmax(180px, 1fr) auto auto auto;
      gap: 8px;
      align-items: center;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      table-layout: fixed;
      font-size: 13px;
    }
    th, td {
      padding: 10px 12px;
      border-bottom: 1px solid var(--line);
      text-align: left;
      vertical-align: top;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    th {
      color: var(--muted);
      font-size: 11px;
      text-transform: uppercase;
      font-weight: 650;
      letter-spacing: 0;
      background: #0f161f;
    }
    tr:hover td {
      background: #121b25;
    }
    .mono {
      font-family: var(--mono);
      font-size: 12px;
    }
    .status {
      display: inline-flex;
      min-width: 42px;
      justify-content: center;
      border-radius: 999px;
      padding: 3px 7px;
      font-family: var(--mono);
      font-size: 12px;
      border: 1px solid var(--line);
      background: #0c131b;
    }
    .status.s2 { color: #a5f2c2; border-color: #244d36; background: #0d1b14; }
    .status.s4, .status.s5 { color: #ffb9b9; border-color: #60313a; background: #211317; }
    .empty {
      padding: 28px 16px;
      color: var(--muted);
      text-align: center;
    }
    .detail {
      color: var(--muted);
      white-space: nowrap;
    }
    .error {
      color: #ffb9b9;
    }
    @media (max-width: 900px) {
      .shell { grid-template-columns: 1fr; }
      aside { border-right: 0; border-bottom: 1px solid var(--line); }
      .grid { grid-template-columns: 1fr; }
      .topbar { flex-direction: column; }
      .toolbar { grid-template-columns: 1fr 1fr; }
      th:nth-child(6), td:nth-child(6), th:nth-child(7), td:nth-child(7) { display: none; }
    }
  </style>
</head>
<body>
  <div class="shell">
    <aside>
      <section class="brand">
        <h1>codex-auth-broker</h1>
        <p>Local Codex app-server proxy for Factory Droid and Responses clients.</p>
      </section>
      <section class="navstat">
        <div class="statline"><span>service</span><strong id="serviceState">checking</strong></div>
        <div class="statline"><span>requests</span><strong id="requestCount">0</strong></div>
        <div class="statline"><span>retained</span><strong id="retainedCount">0</strong></div>
        <div class="statline"><span>plan</span><strong id="planType">unknown</strong></div>
      </section>
      <section class="keybox">
        <p class="muted">If the broker has an API key, enter it here. It stays in this browser session.</p>
        <input id="apiKey" type="password" autocomplete="off" placeholder="Bearer key">
        <div class="row">
          <button class="button" id="saveKey" type="button">Save key</button>
          <button class="button" id="clearKey" type="button">Clear</button>
        </div>
      </section>
      <p class="muted">No prompts, completions, access tokens, or refresh tokens are stored in request history.</p>
    </aside>
    <main>
      <header class="topbar">
        <div>
          <h2>Live console</h2>
          <p class="muted">Usage is read from ChatGPT wham usage with local Codex OAuth. Requests are in-memory and redacted.</p>
        </div>
        <div class="row">
          <span class="pill" id="healthPill"><span class="dot"></span><span>health</span></span>
          <span class="pill" id="usagePill"><span class="dot"></span><span>usage</span></span>
          <span class="pill" id="refreshPill"><span class="dot"></span><span>auto</span></span>
        </div>
      </header>

      <section class="grid">
        <div class="panel">
          <div class="panel-head">
            <h3>Codex usage</h3>
            <span class="mono" id="usageFetched">never</span>
          </div>
          <div class="panel-body usage" id="usageBody"></div>
        </div>
        <div class="panel">
          <div class="panel-head">
            <h3>Broker snapshot</h3>
            <span class="mono" id="snapshotTime">never</span>
          </div>
          <div class="panel-body">
            <div class="statline"><span>history policy</span><strong>memory only</strong></div>
            <div class="statline"><span>request limit</span><strong id="limitCount">0</strong></div>
            <div class="statline"><span>redaction</span><strong>enabled</strong></div>
            <div class="statline"><span>base url</span><strong class="mono">/v1</strong></div>
          </div>
        </div>
      </section>

      <section class="panel">
        <div class="panel-head">
          <h3>Requests</h3>
          <div class="toolbar">
            <input id="filter" type="search" placeholder="Filter model, status, request id, error">
            <button class="button" id="refreshNow" type="button">Refresh</button>
            <button class="button" id="pause" type="button">Pause</button>
            <button class="button danger" id="clearHistory" type="button">Clear</button>
          </div>
        </div>
        <div style="overflow:auto">
          <table>
            <thead>
              <tr>
                <th style="width:160px">Time</th>
                <th style="width:72px">Status</th>
                <th style="width:190px">Model</th>
                <th style="width:80px">Stream</th>
                <th style="width:90px">Duration</th>
                <th style="width:150px">Tokens</th>
                <th style="width:160px">Cache</th>
                <th>Request</th>
              </tr>
            </thead>
            <tbody id="requestRows">
              <tr><td colspan="8" class="empty">No requests yet.</td></tr>
            </tbody>
          </table>
        </div>
      </section>
    </main>
  </div>

  <script>
    const state = {
      requests: [],
      paused: false,
      key: sessionStorage.getItem("codex_auth_broker_key") || ""
    };

    const $ = (id) => document.getElementById(id);
    $("apiKey").value = state.key;

    function authHeaders() {
      const headers = { "Accept": "application/json" };
      if (state.key) headers.Authorization = "Bearer " + state.key;
      return headers;
    }

    async function fetchJSON(url, options = {}) {
      const res = await fetch(url, {
        ...options,
        headers: { ...authHeaders(), ...(options.headers || {}) }
      });
      const text = await res.text();
      let body = {};
      if (text) {
        try { body = JSON.parse(text); } catch { body = { raw: text }; }
      }
      if (!res.ok) {
        const message = body && body.error && body.error.message ? body.error.message : "HTTP " + res.status;
        throw new Error(message);
      }
      return body;
    }

    function setPill(id, kind, text) {
      const el = $(id);
      el.className = "pill " + kind;
      el.lastElementChild.textContent = text;
    }

    function fmtTime(value) {
      if (!value) return "";
      return new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(new Date(value));
    }

    function fmtMS(value) {
      if (value == null) return "";
      if (value < 1000) return value + " ms";
      return (value / 1000).toFixed(value < 10000 ? 1 : 0) + " s";
    }

    function fmtTokens(value) {
      if (value == null) return "-";
      if (value >= 1000000) return (value / 1000000).toFixed(1) + "m";
      if (value >= 1000) return (value / 1000).toFixed(1) + "k";
      return String(value);
    }

    function relTime(epochSeconds) {
      if (!epochSeconds) return "unknown";
      let seconds = Math.max(0, Math.round(epochSeconds - Date.now() / 1000));
      const days = Math.floor(seconds / 86400);
      seconds -= days * 86400;
      const hours = Math.floor(seconds / 3600);
      seconds -= hours * 3600;
      const minutes = Math.floor(seconds / 60);
      if (days) return days + "d " + hours + "h";
      if (hours) return hours + "h " + minutes + "m";
      return minutes + "m";
    }

    function usageColor(used) {
      if (used >= 90) return "var(--bad)";
      if (used >= 70) return "var(--warn)";
      return "var(--accent)";
    }

    function usageWindow(label, win) {
      if (!win) return "";
      const used = Math.max(0, Math.min(100, Number(win.used_percent || 0)));
      const totalSeconds = Number(win.limit_window_seconds || 0);
      const resetAt = Number(win.reset_at || 0);
      let elapsed = 0;
      if (totalSeconds > 0 && resetAt > 0) {
        elapsed = ((totalSeconds - Math.max(0, resetAt - Date.now() / 1000)) / totalSeconds) * 100;
      }
      elapsed = Math.max(0, Math.min(100, elapsed));
      return '<div>' +
        '<div class="usage-title"><span>' + label + '</span><strong>' + used.toFixed(1) + '%</strong></div>' +
        '<div class="bar"><span style="width:' + used + '%;background:' + usageColor(used) + '"></span><i style="left:' + elapsed + '%"></i></div>' +
        '<p class="muted">resets in ' + relTime(resetAt) + ' · time cursor ' + elapsed.toFixed(0) + '%</p>' +
      '</div>';
    }

    async function loadHealth() {
      try {
        const health = await fetchJSON("/healthz");
        $("serviceState").textContent = health.version || "ok";
        setPill("healthPill", "ok", "health ok");
      } catch (err) {
        $("serviceState").textContent = "down";
        setPill("healthPill", "bad", "health error");
      }
    }

    async function loadUsage() {
      try {
        const usage = await fetchJSON("/dashboard/api/usage");
        const rate = usage.rate_limit || {};
        $("planType").textContent = usage.plan_type || "unknown";
        $("usageFetched").textContent = fmtTime((usage._broker || {}).fetched_at);
        $("usageBody").innerHTML =
          usageWindow("Primary window", rate.primary_window) +
          usageWindow("Secondary window", rate.secondary_window);
        if (!$("usageBody").innerHTML) $("usageBody").innerHTML = '<div class="empty">No usage windows returned.</div>';
        setPill("usagePill", "ok", "usage live");
      } catch (err) {
        $("usageBody").innerHTML = '<div class="empty error">' + escapeHTML(err.message) + '</div>';
        setPill("usagePill", "bad", "usage error");
      }
    }

    async function loadRequests() {
      try {
        const snapshot = await fetchJSON("/dashboard/api/requests?limit=250");
        state.requests = snapshot.requests || [];
        $("requestCount").textContent = snapshot.total_seen || 0;
        $("retainedCount").textContent = snapshot.retained || 0;
        $("limitCount").textContent = snapshot.limit || 0;
        $("snapshotTime").textContent = fmtTime(snapshot.generated_at);
        renderRequests();
      } catch (err) {
        $("requestRows").innerHTML = '<tr><td colspan="8" class="empty error">' + escapeHTML(err.message) + '</td></tr>';
      }
    }

    function renderRequests() {
      const filter = $("filter").value.trim().toLowerCase();
      const rows = state.requests.filter((req) => {
        if (!filter) return true;
        return [
          req.status, req.model, req.normalized_model, req.reasoning_effort,
          req.request_id, req.error, req.path
        ].join(" ").toLowerCase().includes(filter);
      });
      if (!rows.length) {
        $("requestRows").innerHTML = '<tr><td colspan="8" class="empty">No matching requests.</td></tr>';
        return;
      }
      $("requestRows").innerHTML = rows.map((req) => {
        const statusClass = "s" + String(req.status || 0).slice(0, 1);
        const model = req.normalized_model && req.normalized_model !== req.model
          ? escapeHTML(req.model || "") + '<div class="detail">' + escapeHTML(req.normalized_model) + '</div>'
          : escapeHTML(req.model || req.path || "");
        const tokens = fmtTokens(req.input_tokens) + " in / " + fmtTokens(req.output_tokens) + " out";
        const cache = fmtTokens(req.cached_tokens) + " cached / " + fmtTokens(req.total_tokens) + " total";
        const request = req.error
          ? '<span class="error">' + escapeHTML(req.error) + '</span>'
          : escapeHTML(req.request_id || req.client || "");
        return '<tr title="' + escapeHTML(JSON.stringify(req)) + '">' +
          '<td class="mono">' + fmtTime(req.started_at) + '</td>' +
          '<td><span class="status ' + statusClass + '">' + (req.status || "") + '</span></td>' +
          '<td>' + model + '</td>' +
          '<td>' + (req.stream ? "yes" : "no") + '</td>' +
          '<td class="mono">' + fmtMS(req.duration_ms) + '</td>' +
          '<td class="mono">' + tokens + '</td>' +
          '<td class="mono">' + cache + '</td>' +
          '<td>' + request + '</td>' +
        '</tr>';
      }).join("");
    }

    function escapeHTML(value) {
      return String(value == null ? "" : value)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;")
        .replace(/"/g, "&quot;");
    }

    async function refreshAll() {
      await Promise.allSettled([loadHealth(), loadUsage(), loadRequests()]);
    }

    $("saveKey").addEventListener("click", () => {
      state.key = $("apiKey").value.trim();
      sessionStorage.setItem("codex_auth_broker_key", state.key);
      refreshAll();
    });
    $("clearKey").addEventListener("click", () => {
      state.key = "";
      $("apiKey").value = "";
      sessionStorage.removeItem("codex_auth_broker_key");
      refreshAll();
    });
    $("refreshNow").addEventListener("click", refreshAll);
    $("pause").addEventListener("click", () => {
      state.paused = !state.paused;
      $("pause").textContent = state.paused ? "Resume" : "Pause";
      setPill("refreshPill", state.paused ? "warn" : "ok", state.paused ? "paused" : "auto");
    });
    $("clearHistory").addEventListener("click", async () => {
      await fetchJSON("/dashboard/api/requests", { method: "DELETE" });
      await loadRequests();
    });
    $("filter").addEventListener("input", renderRequests);

    refreshAll();
    setPill("refreshPill", "ok", "auto");
    setInterval(() => { if (!state.paused) loadRequests(); }, 3000);
    setInterval(() => { if (!state.paused) loadUsage(); }, 30000);
  </script>
</body>
</html>
`
