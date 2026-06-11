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
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&family=JetBrains+Mono:wght@400;500;700&family=Space+Mono:wght@400;700&display=swap" rel="stylesheet">
  <style>
    /* TUI / terminal-dashboard aesthetic — matches me.safzan.dev DESIGN.md.
       Monospace everywhere · square (radius 0) · flat (no gradient/shadow) · dense. */
    :root{
      color-scheme: dark;
      --font-head:"Google Sans Code","Space Mono","Berkeley Mono","JetBrains Mono",monospace;
      --font-data:"Berkeley Mono","JetBrains Mono",ui-monospace,"SF Mono",Menlo,monospace;
      --font-label:"IBM Plex Mono","JetBrains Mono",ui-monospace,monospace;
      --mono:var(--font-data);
    }
    :root,:root[data-theme="mocha"]{--base:#1e1e2e;--mantle:#181825;--crust:#11111b;--s0:#313244;--s1:#45475a;--ov0:#6c7086;--ov1:#7f849c;--text:#cdd6f4;--sub1:#bac2de;--sub0:#a6adc8;--green:#a6e3a1;--red:#f38ba8;--yellow:#f9e2af;--teal:#94e2d5;--accent:#cba6f7;--blue:#89b4fa;}
    :root[data-theme="gruvbox"]{--base:#282828;--mantle:#1d2021;--crust:#161616;--s0:#3c3836;--s1:#504945;--ov0:#928374;--ov1:#a89984;--text:#ebdbb2;--sub1:#d5c4a1;--sub0:#bdae93;--green:#b8bb26;--red:#fb4934;--yellow:#fabd2f;--teal:#8ec07c;--accent:#d3869b;--blue:#83a598;}
    :root[data-theme="tokyo"]{--base:#1a1b26;--mantle:#16161e;--crust:#101015;--s0:#292e42;--s1:#3b4261;--ov0:#565f89;--ov1:#787c99;--text:#c0caf5;--sub1:#a9b1d6;--sub0:#9aa5ce;--green:#9ece6a;--red:#f7768e;--yellow:#e0af68;--teal:#2ac3de;--accent:#bb9af7;--blue:#7aa2f7;}
    :root[data-theme="phosphor"]{--base:#040804;--mantle:#070d07;--crust:#020402;--s0:#11260f;--s1:#1c3f1a;--ov0:#3f7a3f;--ov1:#56a356;--text:#7CFC7C;--sub1:#5fd35f;--sub0:#4caf4c;--green:#39ff14;--red:#ff5f56;--yellow:#d7ff2a;--teal:#39ff14;--accent:#7CFC7C;--blue:#39ff14;}
    :root[data-theme="amber"]{--base:#0e0900;--mantle:#120c00;--crust:#070400;--s0:#2a1f00;--s1:#3f2f00;--ov0:#7a5c12;--ov1:#a37c1a;--text:#ffb000;--sub1:#e0a020;--sub0:#c08818;--green:#ffc000;--red:#ff5f00;--yellow:#ffd000;--teal:#ffb000;--accent:#ffb000;--blue:#ffb000;}

    *{box-sizing:border-box;border-radius:0!important;}
    *:focus{outline:none;}
    *:focus-visible{outline:1px solid var(--blue);outline-offset:1px;}
    body{
      margin:0;min-height:100svh;background:var(--base);color:var(--text);
      font-family:var(--font-data);font-size:13px;line-height:1.45;
      font-variant-numeric:tabular-nums;-webkit-font-smoothing:antialiased;
    }
    .skip{position:absolute;top:-100px;left:12px;padding:8px 12px;background:var(--blue);color:var(--crust);font-weight:600;z-index:99;font-family:var(--font-label);}
    .skip:focus{top:12px;}
    button,input,select{font:inherit;color:inherit;}
    .mono{font-family:var(--font-data);font-variant-numeric:tabular-nums;}
    .small{font-size:11px;}
    .muted{color:var(--ov0);}

    .shell{display:grid;grid-template-columns:240px minmax(0,1fr);min-height:100svh;}
    aside{border-right:1px solid var(--s1);background:var(--crust);padding:16px;display:flex;flex-direction:column;gap:16px;position:sticky;top:0;max-height:100svh;overflow-y:auto;}
    main{min-width:0;padding:16px 20px 28px;display:flex;flex-direction:column;gap:12px;}

    .brand h1{margin:0 0 4px;font-size:.95rem;font-weight:700;letter-spacing:-.01em;display:flex;align-items:center;gap:8px;font-family:var(--font-head);}
    .brand h1::before{content:"";display:inline-block;width:9px;height:9px;background:var(--green);}
    .brand p{margin:0;color:var(--ov0);font-size:.7rem;font-family:var(--font-label);}

    .status-list{display:grid;gap:5px;padding:11px 0;margin:0;border-top:1px solid var(--s0);border-bottom:1px solid var(--s0);}
    .kv{display:flex;justify-content:space-between;gap:12px;font-family:var(--font-label);font-size:.68rem;}
    .kv dt{color:var(--ov0);margin:0;text-transform:uppercase;letter-spacing:.06em;}
    .kv dd{color:var(--sub1);margin:0;font-family:var(--font-data);}

    .keybox{display:grid;gap:8px;}
    .keybox label{font-size:.62rem;color:var(--ov0);text-transform:uppercase;letter-spacing:.08em;font-family:var(--font-label);}
    .keybox input,.toolbar input[type="search"]{width:100%;min-width:0;border:1px solid var(--s1);background:var(--base);color:var(--text);padding:6px 9px;font-size:.78rem;font-family:var(--font-data);transition:border-color 100ms ease;}
    .keybox input:hover,.toolbar input[type="search"]:hover{border-color:var(--ov0);}
    .keybox input:focus,.toolbar input[type="search"]:focus{border-color:var(--blue);background:var(--crust);}
    .row{display:flex;gap:6px;align-items:center;flex-wrap:wrap;}
    .button{border:1px solid var(--s1);background:var(--mantle);color:var(--sub1);padding:5px 11px;font-size:.72rem;font-family:var(--font-label);cursor:pointer;min-height:28px;transition:background 100ms ease,border-color 100ms ease;}
    .button:hover{background:var(--crust);border-color:var(--ov0);color:var(--text);}
    .button:active{background:var(--base);}
    .button.ghost{background:transparent;}
    .button.danger{color:var(--red);border-color:var(--s1);}
    .button.danger:hover{border-color:var(--red);background:var(--crust);}
    .footnote{font-size:.66rem;color:var(--ov0);line-height:1.5;margin:0;font-family:var(--font-label);}

    .topbar{display:flex;flex-direction:column;gap:10px;}
    .topbar-row{display:flex;justify-content:space-between;gap:12px;align-items:center;flex-wrap:wrap;}
    .pills{display:flex;gap:6px;flex-wrap:wrap;}
    .actions{display:flex;gap:6px;align-items:center;}
    .pill{display:inline-flex;align-items:center;gap:7px;height:26px;border:1px solid var(--s1);padding:0 10px;color:var(--sub0);background:var(--mantle);font-size:.7rem;font-family:var(--font-label);white-space:nowrap;}
    .pill .dot{width:7px;height:7px;background:var(--ov0);}
    .pill.ok{color:var(--green);} .pill.ok .dot{background:var(--green);}
    .pill.warn{color:var(--yellow);} .pill.warn .dot{background:var(--yellow);}
    .pill.bad{color:var(--red);} .pill.bad .dot{background:var(--red);}
    .pill.live .dot{animation:pulse 1.6s ease-in-out infinite;}
    @keyframes pulse{0%,100%{opacity:1;}50%{opacity:.35;}}
    @media (prefers-reduced-motion:reduce){.pill.live .dot{animation:none;}*,*::before,*::after{transition:none!important;animation-duration:0s!important;}}

    /* theme dropdown — me-dashboard pattern */
    .dd{position:relative;}
    .ddbtn{display:inline-flex;align-items:center;gap:7px;cursor:pointer;color:var(--sub1);font-family:var(--font-label);font-size:.7rem;background:var(--mantle);border:1px solid var(--s1);height:28px;padding:0 10px;}
    .ddbtn .sw{width:8px;height:8px;background:var(--accent);}
    .ddbtn .caret{color:var(--ov0);font-size:.55rem;}
    .ddmenu{position:absolute;top:calc(100% + 4px);right:0;min-width:140px;z-index:50;background:var(--mantle);border:1px solid var(--s1);padding:3px;}
    .ddmenu[hidden]{display:none;}
    .dditem{display:flex;align-items:center;gap:8px;width:100%;text-align:left;background:transparent;border:0;color:var(--sub0);font-family:var(--font-label);font-size:.72rem;padding:6px 9px;cursor:pointer;}
    .dditem .sw{width:8px;height:8px;background:transparent;}
    .dditem:hover{background:var(--crust);color:var(--text);}
    .dditem.on{color:var(--text);} .dditem.on .sw{background:var(--accent);}

    .kpi-grid{display:grid;grid-template-columns:repeat(6,minmax(0,1fr));gap:8px;}
    .kpi{border:1px solid var(--s1);background:var(--mantle);padding:10px 12px;display:grid;gap:2px;align-content:start;}
    .kpi span{color:var(--ov0);font-size:.6rem;text-transform:uppercase;letter-spacing:.08em;font-family:var(--font-label);}
    .kpi strong{font-family:var(--font-head);font-size:1.35rem;font-weight:700;color:var(--text);}
    .kpi em{font-style:normal;color:var(--ov0);font-size:.66rem;font-family:var(--font-label);}
    .kpi.ok strong{color:var(--green);}
    .kpi.warn strong{color:var(--yellow);}
    .kpi.bad strong{color:var(--red);}

    .usage-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px;}
    .panel{border:1px solid var(--s1);background:var(--mantle);overflow:hidden;}
    .panel-head{display:flex;justify-content:space-between;gap:12px;align-items:center;padding:9px 13px;background:var(--crust);border-bottom:1px solid var(--s0);}
    .panel-head h3{margin:0;font-size:.66rem;font-weight:600;letter-spacing:.1em;text-transform:uppercase;color:var(--ov1);font-family:var(--font-label);}
    .panel-body{padding:13px;}

    .usage{display:grid;gap:8px;}
    .usage-title{display:flex;align-items:baseline;justify-content:space-between;gap:12px;}
    .usage-percent{font-family:var(--font-head);font-size:1.5rem;font-weight:700;}
    .usage-reset{font-family:var(--font-label);font-size:.68rem;color:var(--ov0);}
    .bar{position:relative;height:10px;background:var(--crust);border:1px solid var(--s1);overflow:hidden;}
    .bar span{display:block;height:100%;width:0%;background:var(--green);transition:width 220ms ease;}
    .bar i{position:absolute;top:-3px;bottom:-3px;width:2px;background:var(--text);opacity:.7;left:0%;transition:left 220ms ease;}
    .usage-meta{display:flex;justify-content:space-between;gap:12px;font-size:.68rem;color:var(--ov0);font-family:var(--font-label);}

    .cost-grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:8px;}
    .cost-cell{border:1px solid var(--s0);background:var(--crust);padding:9px 11px;display:grid;gap:2px;align-content:start;}
    .cost-cell span{color:var(--ov0);font-size:.6rem;text-transform:uppercase;letter-spacing:.08em;font-family:var(--font-label);}
    .cost-cell strong{font-family:var(--font-head);font-size:1.15rem;font-weight:700;color:var(--text);}
    .cost-cell em{font-style:normal;color:var(--ov0);font-size:.64rem;font-family:var(--font-label);}
    .cost-models{margin:10px 0 0;padding:0;list-style:none;display:grid;gap:3px;}
    .cost-models li{display:flex;justify-content:space-between;gap:12px;font-size:.68rem;font-family:var(--font-label);color:var(--sub0);}
    .cost-models li b{font-weight:400;color:var(--sub1);font-family:var(--font-data);}

    .toolbar{display:grid;grid-template-columns:auto minmax(160px,280px) auto;gap:8px;align-items:center;}
    .chips{display:inline-flex;gap:0;border:1px solid var(--s1);overflow:hidden;}
    .chip{border:0;background:transparent;color:var(--ov0);padding:5px 11px;font-size:.7rem;font-family:var(--font-label);cursor:pointer;border-right:1px solid var(--s0);transition:background 100ms ease,color 100ms ease;}
    .chip:last-child{border-right:0;}
    .chip:hover{color:var(--text);background:var(--crust);}
    .chip.active{background:var(--crust);color:var(--text);}

    .tableWrap{overflow:auto;max-height:70vh;}
    table{width:100%;border-collapse:collapse;table-layout:fixed;font-size:.74rem;}
    thead th{position:sticky;top:0;z-index:2;background:var(--crust);}
    th,td{padding:6px 12px;border-bottom:1px solid var(--s0);text-align:left;vertical-align:middle;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;}
    th{color:var(--ov0);font-size:.6rem;text-transform:uppercase;font-weight:600;letter-spacing:.08em;font-family:var(--font-label);}
    tbody tr:nth-child(2n) td{background:var(--crust);}
    tbody tr:hover td,tbody tr:nth-child(2n):hover td{background:var(--s0);}
    .col-time{width:86px;} .col-status{width:64px;} .col-model{width:22%;}
    .col-effort{width:70px;} .col-stream{width:70px;} .col-dur{width:78px;}
    .col-tokens{width:130px;} .col-cost{width:84px;} .col-cache{width:140px;}

    .status{display:inline-flex;min-width:38px;justify-content:center;padding:1px 6px;font-family:var(--font-data);font-size:.66rem;font-weight:600;border:1px solid var(--s0);background:var(--crust);color:var(--sub0);}
    .status.s2{color:var(--green);border-color:var(--s1);}
    .status.s3{color:var(--teal);border-color:var(--s1);}
    .status.s4{color:var(--yellow);border-color:var(--s1);}
    .status.s5{color:var(--red);border-color:var(--s1);}

    .badge{display:inline-flex;align-items:center;gap:5px;padding:1px 7px;border:1px solid var(--s0);background:var(--crust);font-size:.62rem;font-family:var(--font-label);text-transform:lowercase;color:var(--sub0);}
    .badge.stream{color:var(--teal);border-color:var(--s1);}
    .badge.effort-low{color:var(--ov0);}
    .badge.effort-medium{color:var(--blue);border-color:var(--s1);}
    .badge.effort-high{color:var(--yellow);border-color:var(--s1);}
    .badge.effort-xhigh{color:var(--red);border-color:var(--s1);}
    .badge.tier-priority{color:var(--yellow);border-color:var(--s1);}
    .badge.cache{color:var(--green);border-color:var(--s1);}

    .detail{color:var(--ov0);font-family:var(--font-label);font-size:.62rem;}
    tbody tr.req-row{cursor:pointer;}
    tbody tr.detail-row td,tbody tr.detail-row:hover td{background:var(--crust);white-space:normal;padding:0;}
    .detail-box{padding:10px 14px;display:grid;gap:6px;border-left:2px solid var(--s1);}
    .detail-box .line{font-size:.7rem;color:var(--sub1);font-family:var(--font-label);}
    .detail-box .line b{color:var(--ov1);font-weight:600;text-transform:uppercase;font-size:.6rem;letter-spacing:.06em;margin-right:6px;}
    .detail-box pre{margin:0;font-size:.64rem;color:var(--sub0);white-space:pre-wrap;word-break:break-all;font-family:var(--font-data);}
    .empty{padding:36px 16px;color:var(--ov0);text-align:center;white-space:normal;font-family:var(--font-label);}
    .empty.error{color:var(--red);}
    .loading{padding:10px 0;color:var(--ov0);font-size:.74rem;display:flex;align-items:center;gap:8px;font-family:var(--font-label);}
    .loading::before{content:"";display:inline-block;width:8px;height:8px;background:var(--ov0);animation:pulse 1.2s ease-in-out infinite;}
    .error-text{color:var(--red);font-family:var(--font-label);font-size:.68rem;}
    .cache-cell{display:flex;align-items:center;gap:6px;}
    .cache-pct{font-family:var(--font-data);font-size:.62rem;padding:1px 6px;background:var(--crust);color:var(--green);border:1px solid var(--s1);}
    .cache-pct.cold{color:var(--ov0);background:var(--crust);border-color:var(--s0);}

    @media (max-width:1180px){
      .kpi-grid{grid-template-columns:repeat(3,minmax(0,1fr));}
      th.col-cache,td.col-cache,th.col-effort,td.col-effort{display:none;}
    }
    @media (max-width:820px){
      .shell{grid-template-columns:1fr;}
      aside{position:static;max-height:none;border-right:0;border-bottom:1px solid var(--s1);}
      main{padding:14px;}
      .usage-grid{grid-template-columns:1fr;}
      .kpi-grid{grid-template-columns:repeat(2,minmax(0,1fr));}
      .toolbar{grid-template-columns:1fr;}
      th.col-stream,td.col-stream{display:none;}
      .col-model{width:38%;}
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
        <div class="kv"><dt>log</dt><dd id="persistInfo">—</dd></div>
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
      <p class="footnote">Request history holds metadata only — in memory and in the optional JSONL log. No prompts, completions, bearer tokens, or refresh tokens are stored or shown. Costs are API-equivalent estimates.</p>
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
            <div class="dd" id="themeDD">
              <button class="ddbtn" id="themeBtn" type="button" aria-label="Theme"><span class="sw"></span><span id="themeLabel">mocha</span><span class="caret">▼</span></button>
              <div class="ddmenu" id="themeMenu" hidden></div>
            </div>
          </div>
        </div>
      </header>

      <section class="kpi-grid" aria-label="Request stats">
        <div class="kpi"><span>Visible</span><strong id="kpiVisible">0</strong><em id="kpiVisibleSub">of 0 retained</em></div>
        <div class="kpi"><span>Success</span><strong id="kpiSuccess">—</strong><em id="kpiSuccessSub">no requests</em></div>
        <div class="kpi"><span>Median</span><strong id="kpiLatency">—</strong><em id="kpiLatencySub">visible window</em></div>
        <div class="kpi"><span>Tokens</span><strong id="kpiTokens">0</strong><em id="kpiTokensSub">in + out</em></div>
        <div class="kpi"><span>Est. cost</span><strong id="kpiCost">—</strong><em id="kpiCostSub">API-equivalent</em></div>
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
          <h3>Cost totals</h3>
          <span class="mono small muted" id="costsSource">—</span>
        </div>
        <div class="panel-body">
          <div class="cost-grid" id="costGrid"><div class="loading">loading costs…</div></div>
          <ul class="cost-models" id="costModels"></ul>
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
                <th class="col-cost">Cost</th>
                <th class="col-cache">Cache</th>
                <th>Request / Error</th>
              </tr>
            </thead>
            <tbody id="requestRows">
              <tr><td colspan="10" class="empty">No requests yet. Send a request through <span class="mono">/v1/responses</span> to see it here.</td></tr>
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
        expandedId: null,
        lastRenderSig: "",
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

      function fmtCost(value) {
        if (value == null) return "—";
        if (value === 0) return "$0";
        if (value < 0.01) return "$" + value.toFixed(4);
        if (value < 1) return "$" + value.toFixed(3);
        return "$" + value.toFixed(2);
      }

      function fmtBytes(value) {
        if (value == null || value <= 0) return "0 B";
        if (value < 1024) return value + " B";
        if (value < 1024 * 1024) return (value / 1024).toFixed(1) + " KB";
        return (value / (1024 * 1024)).toFixed(1) + " MB";
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
        if (used >= 90) return "var(--red)";
        if (used >= 70) return "var(--yellow)";
        return "var(--green)";
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

      async function loadCosts() {
        try {
          const costs = await fetchJSON("/dashboard/api/costs");
          const windows = costs.windows || {};
          const order = ["24h", "7d", "30d", "all"];
          $("costGrid").innerHTML = order.map((key) => {
            const win = windows[key] || {};
            const sub = (win.requests || 0) + " req · " + fmtTokens(win.input_tokens || 0) + " in · " + fmtTokens(win.output_tokens || 0) + " out";
            return '<div class="cost-cell"><span>' + key + '</span>' +
              '<strong>' + fmtCost(win.priced ? win.cost_usd : null) + '</strong>' +
              '<em>' + escapeHTML(sub) + '</em></div>';
          }).join("");
          const models = (windows.all && windows.all.models) || [];
          $("costModels").innerHTML = models.map((m) =>
            '<li><span>' + escapeHTML(m.model || "unknown") + ' · ' + m.requests + ' req</span><b>' + fmtCost(m.cost_usd) + '</b></li>'
          ).join("");
          if (costs.source === "file") {
            $("costsSource").textContent = "from " + fmtBytes(costs.persist_bytes) + " log";
            const parts = String(costs.persist_path || "").split("/");
            $("persistInfo").textContent = parts[parts.length - 1] + " · " + fmtBytes(costs.persist_bytes);
            $("persistInfo").title = costs.persist_path || "";
          } else {
            $("costsSource").textContent = "memory only";
            $("persistInfo").textContent = "disabled";
          }
        } catch (err) {
          $("costGrid").innerHTML = '<div class="empty error">' + escapeHTML(err.message) + '</div>';
          $("costModels").innerHTML = "";
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
          state.lastRenderSig = "";
          $("requestRows").innerHTML = '<tr><td colspan="10" class="empty error">' + escapeHTML(err.message) + '</td></tr>';
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

      function renderRequests(force) {
        const rows = filteredRequests();
        // Entries are immutable once logged, so the visible ids + filters
        // fully describe the table. Skip the rebuild when nothing changed to
        // keep text selection and hover state alive across auto-refresh.
        const sig = state.quickFilter + "|" + state.textFilter + "|" + state.expandedId + "|" + rows.map((r) => r.id).join(",");
        if (!force && sig === state.lastRenderSig) return;
        state.lastRenderSig = sig;
        const counter = state.requests.length === rows.length
          ? (rows.length ? rows.length.toString() : "")
          : rows.length + " of " + state.requests.length;
        $("requestsCounter").textContent = counter;
        updateKPIs(rows);
        if (!rows.length) {
          const msg = state.requests.length
            ? 'No requests match this filter.'
            : 'No requests yet. Send a request through <span class="mono">/v1/responses</span> to see it here.';
          $("requestRows").innerHTML = '<tr><td colspan="10" class="empty">' + msg + '</td></tr>';
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
        const cost = req.cost_usd != null
          ? '<span class="mono" title="$' + Number(req.cost_usd).toFixed(6) + ' API-equivalent">' + fmtCost(req.cost_usd) + '</span>'
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
        let html = '<tr class="req-row" data-id="' + req.id + '">' +
          '<td class="col-time mono">' + fmtTime(req.started_at) + '</td>' +
          '<td class="col-status"><span class="status ' + statusClass(status) + '">' + (status || "—") + '</span>' + upstreamHint + '</td>' +
          '<td class="col-model">' + modelLine + '</td>' +
          '<td class="col-effort">' + effort + tier + '</td>' +
          '<td class="col-stream">' + stream + '</td>' +
          '<td class="col-dur mono">' + fmtMS(req.duration_ms) + '</td>' +
          '<td class="col-tokens">' + tokens + '</td>' +
          '<td class="col-cost">' + cost + '</td>' +
          '<td class="col-cache">' + cacheCell + '</td>' +
          '<td>' + detail + '</td>' +
        '</tr>';
        if (state.expandedId === req.id) html += renderDetailRow(req);
        return html;
      }

      function renderDetailRow(req) {
        const lines = [];
        if (req.input_tokens != null || req.output_tokens != null) {
          const cached = req.cached_tokens || 0;
          const input = req.input_tokens || 0;
          let tok = fmtTokens(input) + " in";
          if (cached) tok += " (" + fmtTokens(cached) + " cached, " + fmtTokens(Math.max(0, input - cached)) + " fresh)";
          tok += " · " + fmtTokens(req.output_tokens || 0) + " out";
          if (req.total_tokens != null) tok += " · " + fmtTokens(req.total_tokens) + " total";
          lines.push("<div class='line'><b>tokens</b>" + escapeHTML(tok) + "</div>");
        }
        if (req.cost_usd != null) {
          lines.push("<div class='line'><b>est. cost</b>$" + Number(req.cost_usd).toFixed(6) + " API-equivalent</div>");
        }
        const meta = {};
        ["id", "started_at", "method", "path", "status", "upstream_status", "model", "normalized_model",
         "reasoning_effort", "service_tier", "stream", "duration_ms", "input_count", "tool_count",
         "prompt_cache_key_set", "prompt_cache_retention_set", "request_id", "client", "error"
        ].forEach((key) => { if (req[key] !== undefined && req[key] !== "" && req[key] !== null) meta[key] = req[key]; });
        lines.push("<pre>" + escapeHTML(JSON.stringify(meta, null, 2)) + "</pre>");
        return '<tr class="detail-row"><td colspan="10"><div class="detail-box">' + lines.join("") + '</div></td></tr>';
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
          setKpi("kpiCost", "—", "no priced requests", null);
          setKpi("kpiCache", "—", "no input tokens", null);
          return;
        }

        let errors = 0;
        let inSum = 0, outSum = 0, cacheSum = 0, costSum = 0, priced = 0;
        const durations = [];
        for (const r of visible) {
          if (isErrorRow(r)) errors++;
          if (typeof r.duration_ms === "number") durations.push(r.duration_ms);
          if (r.input_tokens) inSum += r.input_tokens;
          if (r.output_tokens) outSum += r.output_tokens;
          if (r.cached_tokens) cacheSum += r.cached_tokens;
          if (r.cost_usd != null) { costSum += r.cost_usd; priced++; }
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

        setKpi(
          "kpiCost",
          priced ? fmtCost(costSum) : "—",
          priced ? priced + " priced · API-equivalent" : "no priced requests",
          null
        );

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
        await Promise.allSettled([loadHealth(), loadUsage(), loadRequests(), loadCosts()]);
      }

      $("requestRows").addEventListener("click", (e) => {
        if (window.getSelection && String(window.getSelection())) return;
        const row = e.target.closest("tr.req-row");
        if (!row) return;
        const id = Number(row.dataset.id);
        state.expandedId = state.expandedId === id ? null : id;
        renderRequests(true);
      });

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
        if (!confirm("Clear in-memory request history? The on-disk JSONL log is kept.")) return;
        try {
          await fetchJSON("/dashboard/api/requests", { method: "DELETE" });
          await loadRequests();
        } catch (err) {
          state.lastRenderSig = "";
          $("requestRows").innerHTML = '<tr><td colspan="10" class="empty error">' + escapeHTML(err.message) + '</td></tr>';
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
      setInterval(() => { if (!state.paused) { loadUsage(); loadCosts(); } }, 30000);
    })();
  </script>
  <script>
    (function(){
      var THEMES = ["mocha","tokyo","gruvbox","phosphor","amber"];
      var menu = document.getElementById("themeMenu");
      var btn = document.getElementById("themeBtn");
      var lbl = document.getElementById("themeLabel");
      function set(t){
        document.documentElement.setAttribute("data-theme", t);
        try { localStorage.setItem("cab-theme", t); } catch(e){}
        lbl.textContent = t;
        var items = menu.children;
        for (var i=0;i<items.length;i++){ items[i].classList.toggle("on", items[i].getAttribute("data-t") === t); }
      }
      THEMES.forEach(function(t){
        var b = document.createElement("button");
        b.className = "dditem"; b.setAttribute("data-t", t); b.type = "button";
        var sw = document.createElement("span"); sw.className = "sw";
        var nm = document.createElement("span"); nm.textContent = t;
        b.appendChild(sw); b.appendChild(nm);
        b.onclick = function(){ set(this.getAttribute("data-t")); menu.hidden = true; };
        menu.appendChild(b);
      });
      btn.onclick = function(e){ e.stopPropagation(); menu.hidden = !menu.hidden; };
      document.addEventListener("click", function(){ menu.hidden = true; });
      document.addEventListener("keydown", function(e){ if (e.key === "Escape") menu.hidden = true; });
      var saved = "mocha";
      try { saved = localStorage.getItem("cab-theme") || "mocha"; } catch(e){}
      if (THEMES.indexOf(saved) < 0) saved = "mocha";
      set(saved);
    })();
  </script>
</body>
</html>
`
