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
  <link rel="icon" href="data:,">
  <style>
    :root {
      color-scheme: dark;
      --bg: #0a0e14;
      --panel: #10161f;
      --panel-2: #151c27;
      --panel-3: #1a2331;
      --line: #1f2a3a;
      --line-2: #2a374a;
      --text: #e7eef7;
      --text-dim: #b6c0cf;
      --muted: #6e7c92;
      --accent: #4ade80;
      --warn: #fbbf24;
      --bad: #f87171;
      --info: #60a5fa;
      --cyan: #67e8f9;
      --mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Monaco, Consolas, monospace;
      --sans: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
      --radius-sm: 4px;
      --radius: 7px;
    }
    * { box-sizing: border-box; }
    *:focus { outline: none; }
    *:focus-visible {
      outline: 2px solid var(--info);
      outline-offset: 2px;
      border-radius: 4px;
    }
    body {
      margin: 0;
      min-height: 100svh;
      background: var(--bg);
      color: var(--text);
      font-family: var(--sans);
      font-size: 14px;
      line-height: 1.45;
      -webkit-font-smoothing: antialiased;
    }
    .skip {
      position: absolute;
      top: -100px;
      left: 12px;
      padding: 8px 12px;
      background: var(--info);
      color: #03101f;
      border-radius: var(--radius);
      font-weight: 600;
      z-index: 99;
    }
    .skip:focus { top: 12px; }
    button, input, select { font: inherit; color: inherit; }
    .mono { font-family: var(--mono); font-variant-numeric: tabular-nums; }
    .small { font-size: 12px; }
    .muted { color: var(--muted); }

    .shell {
      display: grid;
      grid-template-columns: 240px minmax(0, 1fr);
      min-height: 100svh;
    }
    aside {
      border-right: 1px solid var(--line);
      background: #06080c;
      padding: 18px;
      display: flex;
      flex-direction: column;
      gap: 18px;
      position: sticky;
      top: 0;
      max-height: 100svh;
      overflow-y: auto;
    }
    main {
      min-width: 0;
      padding: 18px 22px 28px;
      display: flex;
      flex-direction: column;
      gap: 14px;
    }
    .brand h1 {
      margin: 0 0 4px;
      font-size: 14px;
      font-weight: 700;
      letter-spacing: 0;
      display: flex;
      align-items: center;
      gap: 8px;
      text-transform: lowercase;
    }
    .brand h1::before {
      content: "";
      display: inline-block;
      width: 10px;
      height: 10px;
      border-radius: 2px;
      background: linear-gradient(135deg, var(--accent), var(--info));
    }
    .brand p { margin: 0; color: var(--muted); font-size: 12px; }

    .status-list {
      display: grid;
      gap: 6px;
      padding: 12px 0;
      margin: 0;
      border-top: 1px solid var(--line);
      border-bottom: 1px solid var(--line);
    }
    .kv {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      font-family: var(--mono);
      font-size: 11.5px;
      letter-spacing: 0.02em;
    }
    .kv dt { color: var(--muted); margin: 0; text-transform: uppercase; }
    .kv dd { color: var(--text); margin: 0; font-variant-numeric: tabular-nums; }

    .keybox { display: grid; gap: 8px; }
    .keybox label {
      font-size: 11px;
      color: var(--muted);
      text-transform: uppercase;
      letter-spacing: 0.06em;
    }
    .keybox input, .toolbar input[type="search"] {
      width: 100%;
      min-width: 0;
      border: 1px solid var(--line);
      border-radius: var(--radius);
      background: var(--bg);
      color: var(--text);
      padding: 7px 10px;
      font-size: 13px;
      transition: border-color 100ms ease, background 100ms ease;
    }
    .keybox input:hover, .toolbar input[type="search"]:hover { border-color: var(--line-2); }
    .keybox input:focus, .toolbar input[type="search"]:focus {
      border-color: var(--info);
      background: #0c1118;
    }
    .row { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; }
    .button {
      border: 1px solid var(--line);
      background: var(--panel-2);
      color: var(--text);
      border-radius: var(--radius);
      padding: 6px 11px;
      font-size: 12.5px;
      font-weight: 500;
      cursor: pointer;
      min-height: 30px;
      transition: background 100ms ease, border-color 100ms ease;
    }
    .button:hover { background: var(--panel-3); border-color: var(--line-2); }
    .button:active { background: var(--panel); }
    .button.ghost { background: transparent; }
    .button.danger { color: #ffd8d8; border-color: #4a2530; background: #1b1014; }
    .button.danger:hover { background: #251218; }
    .footnote { font-size: 11.5px; color: var(--muted); line-height: 1.5; margin: 0; }

    .topbar { display: flex; flex-direction: column; gap: 10px; }
    .topbar-row {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      align-items: center;
      flex-wrap: wrap;
    }
    .pills { display: flex; gap: 6px; flex-wrap: wrap; }
    .actions { display: flex; gap: 6px; }
    .pill {
      display: inline-flex;
      align-items: center;
      gap: 7px;
      height: 26px;
      border: 1px solid var(--line);
      border-radius: 999px;
      padding: 0 10px;
      color: var(--text-dim);
      background: var(--panel);
      font-size: 11.5px;
      font-family: var(--mono);
      white-space: nowrap;
    }
    .pill .dot { width: 6px; height: 6px; border-radius: 99px; background: var(--muted); }
    .pill.ok { border-color: #1f4031; color: #c4f0d2; background: #0e1a14; }
    .pill.ok .dot { background: var(--accent); }
    .pill.warn { border-color: #4a3a18; color: #fde2a6; background: #1a1408; }
    .pill.warn .dot { background: var(--warn); }
    .pill.bad { border-color: #4d2126; color: #ffc4c4; background: #1a0c0e; }
    .pill.bad .dot { background: var(--bad); }
    .pill.live .dot { animation: pulse 1.6s ease-in-out infinite; }
    @keyframes pulse {
      0%, 100% { opacity: 1; transform: scale(1); }
      50% { opacity: .45; transform: scale(.8); }
    }
    @media (prefers-reduced-motion: reduce) {
      .pill.live .dot { animation: none; }
      *, *::before, *::after { transition: none !important; animation-duration: 0s !important; }
    }

    .kpi-grid {
      display: grid;
      grid-template-columns: repeat(5, minmax(0, 1fr));
      gap: 8px;
    }
    .kpi {
      border: 1px solid var(--line);
      background: var(--panel);
      border-radius: var(--radius);
      padding: 10px 12px;
      display: grid;
      gap: 1px;
      align-content: start;
    }
    .kpi span {
      color: var(--muted);
      font-size: 10.5px;
      text-transform: uppercase;
      letter-spacing: 0.07em;
    }
    .kpi strong {
      font-family: var(--mono);
      font-size: 18px;
      font-weight: 600;
      font-variant-numeric: tabular-nums;
      color: var(--text);
    }
    .kpi em {
      font-style: normal;
      color: var(--muted);
      font-size: 11px;
      font-family: var(--mono);
      font-variant-numeric: tabular-nums;
    }
    .kpi.ok strong { color: var(--accent); }
    .kpi.warn strong { color: var(--warn); }
    .kpi.bad strong { color: var(--bad); }

    .usage-grid {
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 8px;
    }
    .panel {
      border: 1px solid var(--line);
      background: var(--panel);
      border-radius: var(--radius);
      overflow: hidden;
    }
    .panel-head {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      align-items: center;
      padding: 9px 14px;
      background: linear-gradient(to bottom, var(--panel-2), var(--panel));
      border-bottom: 1px solid var(--line);
    }
    .panel-head h3 {
      margin: 0;
      font-size: 11.5px;
      font-weight: 600;
      letter-spacing: 0.07em;
      text-transform: uppercase;
      color: var(--text-dim);
    }
    .panel-body { padding: 14px; }

    .usage { display: grid; gap: 8px; }
    .usage-title {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 12px;
    }
    .usage-percent {
      font-family: var(--mono);
      font-size: 22px;
      font-weight: 600;
      font-variant-numeric: tabular-nums;
    }
    .usage-reset {
      font-family: var(--mono);
      font-size: 11.5px;
      color: var(--muted);
    }
    .bar {
      position: relative;
      height: 10px;
      border-radius: 99px;
      background: #050709;
      border: 1px solid var(--line);
      overflow: hidden;
    }
    .bar span {
      display: block;
      height: 100%;
      width: 0%;
      background: var(--accent);
      transition: width 220ms ease;
    }
    .bar i {
      position: absolute;
      top: -3px;
      bottom: -3px;
      width: 2px;
      background: var(--text);
      opacity: .6;
      left: 0%;
      transition: left 220ms ease;
    }
    .usage-meta {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      font-size: 11.5px;
      color: var(--muted);
      font-family: var(--mono);
    }

    .toolbar {
      display: grid;
      grid-template-columns: auto minmax(160px, 280px) auto;
      gap: 8px;
      align-items: center;
    }
    .chips {
      display: inline-flex;
      gap: 0;
      border: 1px solid var(--line);
      border-radius: var(--radius);
      overflow: hidden;
    }
    .chip {
      border: 0;
      background: transparent;
      color: var(--muted);
      padding: 5px 11px;
      font-size: 12px;
      cursor: pointer;
      border-right: 1px solid var(--line);
      transition: background 100ms ease, color 100ms ease;
    }
    .chip:last-child { border-right: 0; }
    .chip:hover { color: var(--text); background: var(--panel-2); }
    .chip.active { background: var(--panel-3); color: var(--text); }

    .tableWrap { overflow: auto; max-height: 70vh; }
    table {
      width: 100%;
      border-collapse: collapse;
      table-layout: fixed;
      font-size: 12.5px;
    }
    thead th {
      position: sticky;
      top: 0;
      z-index: 2;
      background: var(--panel-2);
    }
    th, td {
      padding: 7px 12px;
      border-bottom: 1px solid var(--line);
      text-align: left;
      vertical-align: middle;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    th {
      color: var(--muted);
      font-size: 10.5px;
      text-transform: uppercase;
      font-weight: 600;
      letter-spacing: 0.07em;
    }
    tbody tr:nth-child(2n) td { background: #0d131b; }
    tbody tr:hover td, tbody tr:nth-child(2n):hover td { background: #141d28; }
    .col-time { width: 86px; }
    .col-status { width: 64px; }
    .col-model { width: 22%; }
    .col-effort { width: 70px; }
    .col-stream { width: 70px; }
    .col-dur { width: 78px; }
    .col-tokens { width: 130px; }
    .col-cache { width: 140px; }

    .status {
      display: inline-flex;
      min-width: 38px;
      justify-content: center;
      border-radius: 4px;
      padding: 1px 6px;
      font-family: var(--mono);
      font-size: 11px;
      font-weight: 600;
      font-variant-numeric: tabular-nums;
      border: 1px solid var(--line);
      background: var(--panel-2);
    }
    .status.s2 { color: #b6f5cb; border-color: #1f4031; background: #0e1a14; }
    .status.s3 { color: #b8e9ff; border-color: #1d3a52; background: #0c1721; }
    .status.s4 { color: #fde2a6; border-color: #4a3a18; background: #1a1408; }
    .status.s5 { color: #ffc4c4; border-color: #4d2126; background: #1a0c0e; }

    .badge {
      display: inline-flex;
      align-items: center;
      gap: 5px;
      padding: 1px 7px;
      border: 1px solid var(--line);
      border-radius: 99px;
      background: var(--panel-2);
      font-size: 10.5px;
      font-family: var(--mono);
      text-transform: lowercase;
      color: var(--text-dim);
    }
    .badge.stream { color: var(--cyan); border-color: #1c4250; background: #0a1820; }
    .badge.effort-low { color: var(--muted); }
    .badge.effort-medium { color: var(--info); border-color: #1d3556; background: #0a1322; }
    .badge.effort-high { color: var(--warn); border-color: #4a3a18; background: #1a1408; }
    .badge.effort-xhigh { color: var(--bad); border-color: #4d2126; background: #1a0c0e; }
    .badge.tier-priority { color: var(--warn); border-color: #4a3a18; background: #1a1408; }
    .badge.cache { color: var(--accent); border-color: #1f4031; background: #0e1a14; }

    .detail {
      color: var(--muted);
      font-family: var(--mono);
      font-size: 10.5px;
    }
    .empty {
      padding: 36px 16px;
      color: var(--muted);
      text-align: center;
      white-space: normal;
    }
    .empty.error { color: #ffb9b9; }
    .loading {
      padding: 10px 0;
      color: var(--muted);
      font-size: 12.5px;
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .loading::before {
      content: "";
      display: inline-block;
      width: 8px;
      height: 8px;
      border-radius: 99px;
      background: var(--muted);
      animation: pulse 1.2s ease-in-out infinite;
    }
    .error-text {
      color: #ffb9b9;
      font-family: var(--mono);
      font-size: 11.5px;
    }
    .cache-cell { display: flex; align-items: center; gap: 6px; }
    .cache-pct {
      font-family: var(--mono);
      font-size: 10.5px;
      padding: 1px 6px;
      border-radius: 4px;
      background: #0e1a14;
      color: var(--accent);
      border: 1px solid #1f4031;
      font-variant-numeric: tabular-nums;
    }
    .cache-pct.cold { color: var(--muted); background: var(--panel-2); border-color: var(--line); }

    @media (max-width: 1180px) {
      .kpi-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); }
      th.col-cache, td.col-cache, th.col-effort, td.col-effort { display: none; }
    }
    @media (max-width: 820px) {
      .shell { grid-template-columns: 1fr; }
      aside { position: static; max-height: none; border-right: 0; border-bottom: 1px solid var(--line); }
      main { padding: 14px; }
      .usage-grid { grid-template-columns: 1fr; }
      .kpi-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
      .toolbar { grid-template-columns: 1fr; }
      th.col-stream, td.col-stream { display: none; }
      .col-model { width: 38%; }
    }
  </style>
</head>
<body>
  <a class="skip" href="#main-content">Skip to content</a>
  <div class="shell">
    <aside>
      <section class="brand">
        <h1>codex-auth-broker</h1>
        <p>Local Codex proxy for Factory Droid + Pi clients.</p>
      </section>
      <dl class="status-list" aria-label="Broker state">
        <div class="kv"><dt>service</dt><dd id="serviceState">—</dd></div>
        <div class="kv"><dt>plan</dt><dd id="planType">—</dd></div>
        <div class="kv"><dt>account</dt><dd id="accountState">—</dd></div>
        <div class="kv"><dt>seen</dt><dd id="requestCount">0</dd></div>
        <div class="kv"><dt>retained</dt><dd id="retainedCount">0 / 0</dd></div>
        <div class="kv"><dt>updated</dt><dd id="snapshotTime">—</dd></div>
      </dl>
      <form class="keybox" id="keyForm">
        <label for="apiKey">Broker API key</label>
        <input id="apiKey" type="password" autocomplete="off" spellcheck="false" placeholder="Bearer key (if set)">
        <div class="row">
          <button class="button" id="saveKey" type="button">Save</button>
          <button class="button ghost" id="clearKey" type="button">Clear</button>
        </div>
        <p class="footnote">Stored in this browser session only — never sent upstream.</p>
      </form>
      <p class="footnote">Request history holds metadata only. No prompts, completions, bearer tokens, or refresh tokens are logged or shown.</p>
    </aside>

    <main id="main-content">
      <header class="topbar">
        <div class="topbar-row">
          <div class="pills" role="status" aria-live="polite">
            <span class="pill" id="healthPill"><span class="dot"></span><span>health · …</span></span>
            <span class="pill" id="usagePill"><span class="dot"></span><span>usage · …</span></span>
            <span class="pill live" id="refreshPill"><span class="dot"></span><span>auto · 3s</span></span>
          </div>
          <div class="actions">
            <button class="button" id="pause" type="button" aria-pressed="false">Pause</button>
            <button class="button" id="refreshNow" type="button">Refresh</button>
          </div>
        </div>
      </header>

      <section class="kpi-grid" aria-label="Request stats">
        <div class="kpi"><span>Visible</span><strong id="kpiVisible">0</strong><em id="kpiVisibleSub">of 0 retained</em></div>
        <div class="kpi"><span>Success</span><strong id="kpiSuccess">—</strong><em id="kpiSuccessSub">no requests</em></div>
        <div class="kpi"><span>Median</span><strong id="kpiLatency">—</strong><em id="kpiLatencySub">visible window</em></div>
        <div class="kpi"><span>Tokens</span><strong id="kpiTokens">0</strong><em id="kpiTokensSub">in + out</em></div>
        <div class="kpi"><span>Cache hit</span><strong id="kpiCache">—</strong><em id="kpiCacheSub">of input tokens</em></div>
      </section>

      <section class="usage-grid" aria-label="Codex rate limits">
        <div class="panel">
          <div class="panel-head">
            <h3>Primary window</h3>
            <span class="mono small muted" id="primaryFetched">—</span>
          </div>
          <div class="panel-body" id="primaryBody">
            <div class="loading">loading usage…</div>
          </div>
        </div>
        <div class="panel">
          <div class="panel-head">
            <h3>Secondary window</h3>
            <span class="mono small muted" id="secondaryFetched">—</span>
          </div>
          <div class="panel-body" id="secondaryBody">
            <div class="loading">loading usage…</div>
          </div>
        </div>
      </section>

      <section class="panel">
        <div class="panel-head">
          <h3>Requests <span class="muted small mono" id="requestsCounter"></span></h3>
          <div class="toolbar" role="toolbar" aria-label="Filter requests">
            <div class="chips" role="group" aria-label="Quick filter">
              <button class="chip active" data-filter="all" type="button">All</button>
              <button class="chip" data-filter="errors" type="button">Errors</button>
              <button class="chip" data-filter="streaming" type="button">Stream</button>
              <button class="chip" data-filter="cached" type="button">Cached</button>
            </div>
            <input id="filter" type="search" placeholder="Filter model, id, error  ·  press /" aria-label="Filter requests">
            <button class="button danger" id="clearHistory" type="button">Clear history</button>
          </div>
        </div>
        <div class="tableWrap">
          <table>
            <thead>
              <tr>
                <th class="col-time">Time</th>
                <th class="col-status">Status</th>
                <th class="col-model">Model</th>
                <th class="col-effort">Effort</th>
                <th class="col-stream">Stream</th>
                <th class="col-dur">Duration</th>
                <th class="col-tokens">Tokens (in / out)</th>
                <th class="col-cache">Cache</th>
                <th>Request / Error</th>
              </tr>
            </thead>
            <tbody id="requestRows">
              <tr><td colspan="9" class="empty">No requests yet. Send a request through <span class="mono">/v1/responses</span> to see it here.</td></tr>
            </tbody>
          </table>
        </div>
      </section>
    </main>
  </div>

  <script>
    (function() {
      const state = {
        requests: [],
        snapshot: null,
        paused: false,
        textFilter: "",
        quickFilter: "all",
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
          const message = body && body.error && body.error.message ? body.error.message : ("HTTP " + res.status);
          const error = new Error(message);
          error.status = res.status;
          throw error;
        }
        return body;
      }

      function setPill(id, kind, text) {
        const el = $(id);
        let className = "pill";
        if (kind) className += " " + kind;
        if (id === "refreshPill" && !state.paused) className += " live";
        el.className = className;
        el.lastElementChild.textContent = text;
      }

      function fmtTime(value) {
        if (!value) return "—";
        return new Intl.DateTimeFormat(undefined, {
          hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23"
        }).format(new Date(value));
      }

      function fmtMS(value) {
        if (value == null) return "—";
        if (value < 1000) return value + " ms";
        if (value < 60000) return (value / 1000).toFixed(value < 10000 ? 2 : 1) + " s";
        return Math.round(value / 1000) + " s";
      }

      function fmtTokens(value) {
        if (value == null) return "—";
        if (value >= 1000000) return (value / 1000000).toFixed(2) + "m";
        if (value >= 10000) return Math.round(value / 1000) + "k";
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
        seconds -= minutes * 60;
        if (days) return days + "d " + hours + "h";
        if (hours) return hours + "h " + minutes + "m";
        if (minutes) return minutes + "m " + seconds + "s";
        return seconds + "s";
      }

      function humanWindowLabel(seconds) {
        if (seconds <= 0) return "";
        if (seconds % 86400 === 0) return Math.round(seconds / 86400) + "d";
        if (seconds % 3600 === 0) return Math.round(seconds / 3600) + "h";
        if (seconds % 60 === 0) return Math.round(seconds / 60) + "m";
        return seconds + "s";
      }

      function usageColor(used) {
        if (used >= 90) return "var(--bad)";
        if (used >= 70) return "var(--warn)";
        return "var(--accent)";
      }

      function usageKind(used) {
        if (used >= 90) return "bad";
        if (used >= 70) return "warn";
        return "ok";
      }

      function renderUsageWindow(bodyID, fetchedID, win, fetchedAt) {
        const root = $(bodyID);
        const fetched = $(fetchedID);
        if (fetched) fetched.textContent = fetchedAt ? fmtTime(fetchedAt) : "—";
        if (!win) {
          root.innerHTML = '<div class="empty">No window data.</div>';
          return null;
        }
        const used = Math.max(0, Math.min(100, Number(win.used_percent || 0)));
        const totalSeconds = Number(win.limit_window_seconds || 0);
        const resetAt = Number(win.reset_at || 0);
        let cursor = 0;
        if (totalSeconds > 0 && resetAt > 0) {
          cursor = ((totalSeconds - Math.max(0, resetAt - Date.now() / 1000)) / totalSeconds) * 100;
        }
        cursor = Math.max(0, Math.min(100, cursor));
        const windowLabel = totalSeconds > 0 ? humanWindowLabel(totalSeconds) : "";
        root.innerHTML =
          '<div class="usage">' +
            '<div class="usage-title">' +
              '<span class="usage-percent" style="color:' + usageColor(used) + '">' + used.toFixed(1) + '%</span>' +
              '<span class="usage-reset">resets in ' + escapeHTML(relTime(resetAt)) + '</span>' +
            '</div>' +
            '<div class="bar"><span style="width:' + used + '%;background:' + usageColor(used) + '"></span><i style="left:' + cursor + '%"></i></div>' +
            '<div class="usage-meta">' +
              '<span>' + (windowLabel ? escapeHTML(windowLabel) + ' window' : '&nbsp;') + '</span>' +
              '<span>elapsed ' + cursor.toFixed(0) + '%</span>' +
            '</div>' +
          '</div>';
        return usageKind(used);
      }

      async function loadHealth() {
        try {
          const health = await fetchJSON("/healthz");
          $("serviceState").textContent = health.version || "ok";
          setPill("healthPill", "ok", "health · ok");
        } catch (err) {
          $("serviceState").textContent = "down";
          setPill("healthPill", "bad", "health · " + (err.status || "down"));
        }
      }

      async function loadUsage() {
        try {
          const usage = await fetchJSON("/dashboard/api/usage");
          const rate = usage.rate_limit || {};
          $("planType").textContent = usage.plan_type || "—";
          $("accountState").textContent = (usage._broker && usage._broker.account_id_present) ? "linked" : "missing";
          const fetched = usage._broker && usage._broker.fetched_at;
          const k1 = renderUsageWindow("primaryBody", "primaryFetched", rate.primary_window, fetched);
          const k2 = renderUsageWindow("secondaryBody", "secondaryFetched", rate.secondary_window, fetched);
          const worst = [k1, k2].includes("bad") ? "bad" : [k1, k2].includes("warn") ? "warn" : "ok";
          setPill("usagePill", worst, "usage · " + (usage.plan_type || "live"));
        } catch (err) {
          $("planType").textContent = "—";
          $("accountState").textContent = err.status === 401 ? "auth" : "error";
          const msg = '<div class="empty error">' + escapeHTML(err.message) + '</div>';
          $("primaryBody").innerHTML = msg;
          $("secondaryBody").innerHTML = msg;
          setPill("usagePill", "bad", "usage · " + (err.status === 401 ? "auth" : "error"));
        }
      }

      async function loadRequests() {
        try {
          const snapshot = await fetchJSON("/dashboard/api/requests?limit=250");
          state.snapshot = snapshot;
          state.requests = snapshot.requests || [];
          $("requestCount").textContent = (snapshot.total_seen || 0).toLocaleString();
          $("retainedCount").textContent = (snapshot.retained || 0) + " / " + (snapshot.limit || 0);
          $("snapshotTime").textContent = fmtTime(snapshot.generated_at);
          renderRequests();
        } catch (err) {
          $("requestRows").innerHTML = '<tr><td colspan="9" class="empty error">' + escapeHTML(err.message) + '</td></tr>';
        }
      }

      function isErrorRow(req) {
        return Boolean(req.error) || (req.status && req.status >= 400);
      }

      function filteredRequests() {
        const text = state.textFilter;
        const quick = state.quickFilter;
        return state.requests.filter((req) => {
          if (quick === "errors" && !isErrorRow(req)) return false;
          if (quick === "streaming" && !req.stream) return false;
          if (quick === "cached" && !(req.cached_tokens && req.cached_tokens > 0)) return false;
          if (!text) return true;
          const blob = [
            req.status, req.model, req.normalized_model, req.reasoning_effort,
            req.request_id, req.error, req.path, req.client
          ].join(" ").toLowerCase();
          return blob.includes(text);
        });
      }

      function renderRequests() {
        const rows = filteredRequests();
        const counter = state.requests.length === rows.length
          ? (rows.length ? rows.length.toString() : "")
          : rows.length + " of " + state.requests.length;
        $("requestsCounter").textContent = counter;
        updateKPIs(rows);
        if (!rows.length) {
          const msg = state.requests.length
            ? 'No requests match this filter.'
            : 'No requests yet. Send a request through <span class="mono">/v1/responses</span> to see it here.';
          $("requestRows").innerHTML = '<tr><td colspan="9" class="empty">' + msg + '</td></tr>';
          return;
        }
        $("requestRows").innerHTML = rows.map(renderRow).join("");
      }

      function statusClass(status) {
        const s = Number(status || 0);
        if (s >= 500) return "s5";
        if (s >= 400) return "s4";
        if (s >= 300) return "s3";
        if (s >= 200) return "s2";
        return "";
      }

      function renderRow(req) {
        const status = req.status || 0;
        const upstreamHint = req.upstream_status && req.upstream_status !== req.status
          ? '<div class="detail">up ' + req.upstream_status + '</div>'
          : '';
        const modelLine = req.model
          ? escapeHTML(req.model) + (req.normalized_model && req.normalized_model !== req.model
            ? '<div class="detail">' + escapeHTML(req.normalized_model) + '</div>' : '')
          : '<span class="muted">' + escapeHTML(req.path || "—") + '</span>';
        const effort = req.reasoning_effort
          ? '<span class="badge effort-' + escapeHTML(req.reasoning_effort) + '">' + escapeHTML(req.reasoning_effort) + '</span>'
          : '<span class="muted">—</span>';
        const tier = req.service_tier
          ? '<div class="detail"><span class="badge tier-' + escapeHTML(req.service_tier) + '">' + escapeHTML(req.service_tier) + '</span></div>'
          : '';
        const stream = req.stream
          ? '<span class="badge stream">stream</span>'
          : '<span class="muted">—</span>';
        const tokens = (req.input_tokens != null || req.output_tokens != null)
          ? '<span class="mono">' + fmtTokens(req.input_tokens) + ' / ' + fmtTokens(req.output_tokens) + '</span>'
          : '<span class="muted">—</span>';
        const cacheCell = renderCache(req);
        let detail;
        if (req.error) {
          const full = String(req.error);
          const short = full.length > 90 ? full.slice(0, 87) + "…" : full;
          detail = '<span class="error-text" title="' + escapeHTML(full) + '">' + escapeHTML(short) + '</span>';
        } else {
          const id = req.request_id || req.client;
          detail = id
            ? '<span class="mono detail" title="' + escapeHTML(id) + '">' + escapeHTML(id) + '</span>'
            : '<span class="muted">—</span>';
        }
        const tipObj = {
          started: req.started_at,
          method: req.method,
          path: req.path,
          status: req.status,
          upstream_status: req.upstream_status,
          model: req.model,
          normalized: req.normalized_model,
          effort: req.reasoning_effort,
          stream: req.stream,
          input_count: req.input_count,
          tool_count: req.tool_count,
          prompt_cache_key_set: req.prompt_cache_key_set,
          prompt_cache_retention_set: req.prompt_cache_retention_set,
          service_tier: req.service_tier,
          request_id: req.request_id,
          client: req.client
        };
        const tip = escapeHTML(JSON.stringify(tipObj, null, 2));
        return '<tr title="' + tip + '">' +
          '<td class="col-time mono">' + fmtTime(req.started_at) + '</td>' +
          '<td class="col-status"><span class="status ' + statusClass(status) + '">' + (status || "—") + '</span>' + upstreamHint + '</td>' +
          '<td class="col-model">' + modelLine + '</td>' +
          '<td class="col-effort">' + effort + tier + '</td>' +
          '<td class="col-stream">' + stream + '</td>' +
          '<td class="col-dur mono">' + fmtMS(req.duration_ms) + '</td>' +
          '<td class="col-tokens">' + tokens + '</td>' +
          '<td class="col-cache">' + cacheCell + '</td>' +
          '<td>' + detail + '</td>' +
        '</tr>';
      }

      function renderCache(req) {
        const cached = req.cached_tokens;
        const input = req.input_tokens;
        const flag = req.prompt_cache_key_set ? '<span class="badge cache">key</span>' : '';
        if (cached == null && !req.prompt_cache_key_set) return '<span class="muted">—</span>';
        if (cached == null || cached === 0) {
          return '<div class="cache-cell">' + flag + '<span class="cache-pct cold">0% cached</span></div>';
        }
        let label;
        let pctClass = "";
        if (input && input > 0) {
          const ratio = Math.round((cached / input) * 100);
          label = ratio + "% cached";
          pctClass = ratio >= 30 ? "" : "cold";
        } else {
          label = fmtTokens(cached) + " cached";
        }
        return '<div class="cache-cell">' + flag + '<span class="cache-pct ' + pctClass + '">' + escapeHTML(label) + '</span></div>';
      }

      function updateKPIs(visible) {
        const total = state.requests.length;
        $("kpiVisible").textContent = visible.length.toLocaleString();
        $("kpiVisibleSub").textContent = "of " + (state.snapshot ? (state.snapshot.retained || 0) : 0) + " retained";

        if (!visible.length) {
          setKpi("kpiSuccess", "—", total ? "no matches" : "no requests", null);
          setKpi("kpiLatency", "—", "no data", null);
          $("kpiTokens").textContent = "0";
          $("kpiTokensSub").textContent = "in + out";
          setKpiKind("kpiTokens", null);
          setKpi("kpiCache", "—", "no input tokens", null);
          return;
        }

        let errors = 0;
        let inSum = 0, outSum = 0, cacheSum = 0;
        const durations = [];
        for (const r of visible) {
          if (isErrorRow(r)) errors++;
          if (typeof r.duration_ms === "number") durations.push(r.duration_ms);
          if (r.input_tokens) inSum += r.input_tokens;
          if (r.output_tokens) outSum += r.output_tokens;
          if (r.cached_tokens) cacheSum += r.cached_tokens;
        }
        const ok = visible.length - errors;
        const rate = (ok / visible.length) * 100;
        setKpi(
          "kpiSuccess",
          rate.toFixed(rate === 100 ? 0 : 1) + "%",
          errors === 0 ? "no errors" : (errors + (errors === 1 ? " error" : " errors")),
          errors === 0 ? "ok" : (rate >= 90 ? "warn" : "bad")
        );

        durations.sort((a, b) => a - b);
        const median = durations.length ? durations[Math.floor((durations.length - 1) / 2)] : null;
        const p95 = durations.length ? durations[Math.floor((durations.length - 1) * 0.95)] : null;
        setKpi(
          "kpiLatency",
          median != null ? fmtMS(median) : "—",
          p95 != null ? "p95 " + fmtMS(p95) : "no data",
          null
        );

        $("kpiTokens").textContent = fmtTokens(inSum + outSum);
        $("kpiTokensSub").textContent = fmtTokens(inSum) + " in · " + fmtTokens(outSum) + " out";
        setKpiKind("kpiTokens", null);

        if (inSum > 0) {
          const pct = (cacheSum / inSum) * 100;
          setKpi(
            "kpiCache",
            pct.toFixed(pct >= 99.95 ? 0 : 1) + "%",
            fmtTokens(cacheSum) + " cached / " + fmtTokens(inSum) + " in",
            pct >= 50 ? "ok" : (pct >= 20 ? "warn" : null)
          );
        } else {
          setKpi("kpiCache", "—", "no input tokens", null);
        }
      }

      function setKpi(id, value, sub, kind) {
        $(id).textContent = value;
        $(id + "Sub").textContent = sub;
        setKpiKind(id, kind);
      }

      function setKpiKind(id, kind) {
        const el = $(id).parentElement;
        el.classList.remove("ok", "warn", "bad");
        if (kind) el.classList.add(kind);
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
      $("apiKey").addEventListener("keydown", (e) => {
        if (e.key === "Enter") { e.preventDefault(); $("saveKey").click(); }
      });
      $("keyForm").addEventListener("submit", (e) => {
        e.preventDefault();
        $("saveKey").click();
      });

      $("refreshNow").addEventListener("click", refreshAll);
      $("pause").addEventListener("click", () => {
        state.paused = !state.paused;
        $("pause").textContent = state.paused ? "Resume" : "Pause";
        $("pause").setAttribute("aria-pressed", String(state.paused));
        setPill("refreshPill", state.paused ? "warn" : "ok", state.paused ? "paused" : "auto · 3s");
      });
      $("clearHistory").addEventListener("click", async () => {
        if (!confirm("Clear in-memory request history?")) return;
        try {
          await fetchJSON("/dashboard/api/requests", { method: "DELETE" });
          await loadRequests();
        } catch (err) {
          $("requestRows").innerHTML = '<tr><td colspan="9" class="empty error">' + escapeHTML(err.message) + '</td></tr>';
        }
      });
      $("filter").addEventListener("input", (e) => {
        state.textFilter = e.target.value.trim().toLowerCase();
        renderRequests();
      });
      document.querySelectorAll(".chip").forEach((chip) => {
        chip.addEventListener("click", () => {
          state.quickFilter = chip.dataset.filter;
          document.querySelectorAll(".chip").forEach((c) => c.classList.toggle("active", c === chip));
          renderRequests();
        });
      });

      document.addEventListener("keydown", (e) => {
        if (e.target.matches("input, textarea")) return;
        if (e.key === "/") { e.preventDefault(); $("filter").focus(); }
        else if (e.key === "r" && !e.metaKey && !e.ctrlKey) { e.preventDefault(); refreshAll(); }
        else if (e.key === "p" && !e.metaKey && !e.ctrlKey) { e.preventDefault(); $("pause").click(); }
      });

      setPill("refreshPill", "ok", "auto · 3s");
      refreshAll();
      setInterval(() => { if (!state.paused) loadRequests(); }, 3000);
      setInterval(() => { if (!state.paused) loadUsage(); }, 30000);
    })();
  </script>
</body>
</html>
`
