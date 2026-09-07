package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const maxAlphaSearchResponseBytes = 16 * 1024 * 1024

// handleAlphaSearch proxies POST /v1/alpha/search to the Codex standalone
// search backend (web.run without a GPT inference turn). The broker injects
// the stored ChatGPT OAuth access token and account id, exactly as it does for
// /v1/responses, so clients only need the broker API key.
//
// Protocol notes (from codex-rs/codex-api/src/search.rs):
//   - body: { id, model, commands: { search_query: [{q, recency?, domains?}], ... }, settings? }
//   - the endpoint rejects multi-action batches; send one action type per request
//   - upstream may return a Cloudflare 403 HTML challenge for non-browser
//     clients; that status is passed through for the caller to handle.
func (p *responsesProxy) handleAlphaSearch(w http.ResponseWriter, r *http.Request) {
	logEntry := p.beginRequestLog(r)
	defer logEntry.finish()
	if !p.authorizedClient(r) {
		logEntry.markError(http.StatusUnauthorized, "unauthorized")
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	release, fail := p.acquireUpstreamSlot(r.Context())
	if fail != nil {
		p.writeDispatchFailure(w, logEntry, fail)
		return
	}
	defer release()

	payload, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		logEntry.markError(http.StatusBadRequest, err.Error())
		writeProxyError(w, http.StatusBadRequest, "read request body failed: "+err.Error())
		return
	}
	if len(payload) > maxRequestBodyBytes {
		logEntry.markError(http.StatusRequestEntityTooLarge, "request body too large")
		writeProxyError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	acct := p.pool.preferred(time.Now())
	if acct == nil {
		logEntry.markError(http.StatusBadGateway, "no Codex accounts configured")
		writeProxyError(w, http.StatusBadGateway, "no Codex accounts configured")
		return
	}
	access, err := acct.mgr.current(r.Context())
	if err != nil {
		logEntry.markError(http.StatusBadGateway, err.Error())
		writeProxyError(w, http.StatusBadGateway, "Codex auth failed: "+err.Error())
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.cfg.alphaSearchURL, bytes.NewReader(payload))
	if err != nil {
		logEntry.markError(http.StatusInternalServerError, err.Error())
		writeProxyError(w, http.StatusInternalServerError, "build upstream request failed: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+access.AccessToken)
	req.Header.Set("chatgpt-account-id", access.AccountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	p.setClientIdentity(req)

	resp, err := p.client.Do(req)
	if err != nil {
		logEntry.markError(http.StatusBadGateway, err.Error())
		writeProxyError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	logEntry.markUpstreamStatus(resp.StatusCode)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAlphaSearchResponseBytes+1))
	if err != nil || len(body) > maxAlphaSearchResponseBytes {
		message := "upstream search response incomplete or too large"
		logEntry.markError(http.StatusBadGateway, message)
		writeProxyError(w, http.StatusBadGateway, message)
		return
	}
	logEntry.markStatus(resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		summary := summarizeUpstreamError(body, resp.StatusCode)
		logEntry.markError(resp.StatusCode, summary)
		log.Printf("alpha/search upstream returned %d: %s", resp.StatusCode, summary)
		body = []byte(redactTokenLikeText(string(body)))
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || strings.Contains(strings.ToLower(contentType), "html") {
		// Cloudflare challenges come back as HTML; keep the payload but make it
		// clear to JSON clients what happened via the content type.
		contentType = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
