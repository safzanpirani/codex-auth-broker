package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// dashboardCookieName holds the admin key exchanged via /dashboard?key=... so
// the browser dashboard stays usable without pasting the key on every visit.
// HttpOnly; never logged (the redaction rules for keys apply to it).
const dashboardCookieName = "codex_broker_dashboard_key"

// dashboardAuthStatus classifies a dashboard request: 200 when admin access is
// granted (or no keys are configured at all, preserving the historical open
// dashboard), 401 when no valid key was presented, 403 when a valid key was
// presented but its role is not admin. Accepts Authorization: Bearer and the
// dashboard cookie.
func (p *responsesProxy) dashboardAuthStatus(r *http.Request) int {
	reg := p.registry()
	if !reg.enabled() {
		return http.StatusOK
	}
	token := bearerToken(r)
	if token == "" {
		if cookie, err := r.Cookie(dashboardCookieName); err == nil {
			token = cookie.Value
		}
	}
	if token == "" {
		return http.StatusUnauthorized
	}
	id, ok := reg.resolve(token)
	if !ok {
		return http.StatusUnauthorized
	}
	if !id.admin() {
		return http.StatusForbidden
	}
	return http.StatusOK
}

// requireDashboardAdmin gates a /dashboard* handler; it writes the error
// response and returns false when access is denied.
func (p *responsesProxy) requireDashboardAdmin(w http.ResponseWriter, r *http.Request) bool {
	switch p.dashboardAuthStatus(r) {
	case http.StatusUnauthorized:
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return false
	case http.StatusForbidden:
		writeProxyError(w, http.StatusForbidden, "admin key required")
		return false
	}
	return true
}

func (p *responsesProxy) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Browser bootstrap: /dashboard?key=<admin key> exchanges the query param
	// for an HttpOnly cookie and redirects, so the key does not stay in the
	// address bar. Bearer-based access works without the cookie.
	if key := r.URL.Query().Get("key"); key != "" && p.registry().enabled() {
		id, ok := p.registry().resolve(key)
		if !ok {
			writeProxyError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !id.admin() {
			writeProxyError(w, http.StatusForbidden, "admin key required")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     dashboardCookieName,
			Value:    key,
			Path:     "/",
			HttpOnly: true,
			Secure:   r.TLS != nil,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}
	if !p.requireDashboardAdmin(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML)
}

// Logout clears this browser's cookie even when the key has already expired.
func (p *responsesProxy) handleDashboardLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, &http.Cookie{
		Name: dashboardCookieName, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (p *responsesProxy) handleDashboardRequests(w http.ResponseWriter, r *http.Request) {
	if !p.requireDashboardAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := requestLimitFromQuery(r, 250)
		writeJSON(w, http.StatusOK, p.requests.snapshot(limit))
	case http.MethodDelete:
		if err := p.requests.clear(); err != nil {
			writeProxyError(w, http.StatusInternalServerError, "clear request history failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":       "cleared",
			"generated_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
	default:
		writeProxyError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (p *responsesProxy) handleCodexUsage(w http.ResponseWriter, r *http.Request) {
	if !p.requireDashboardAdmin(w, r) {
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
	acct := p.pool.preferred(time.Now())
	if acct == nil {
		return nil, http.StatusBadGateway, errors.New("no Codex accounts configured")
	}
	access, err := acct.mgr.current(ctx)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("Codex auth failed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.usageURL, nil)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("build usage request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+access.AccessToken)
	req.Header.Set("Accept", "application/json")
	p.setClientIdentity(req)
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

//go:embed dashboard.html
var dashboardHTML string
