package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newKeyedProxy() *responsesProxy {
	return &responsesProxy{
		requests: newRequestLogStore(10),
		keys: &keyRegistry{
			entries: []clientKey{
				{Name: "dev", Key: "admin-key", Role: roleAdmin},
				{Name: "backend", Key: "client-key", Role: roleClient},
			},
			statInterval: keyRegistryStatInterval,
		},
	}
}

func TestDashboardGating(t *testing.T) {
	tests := []struct {
		name       string
		proxy      *responsesProxy
		target     string
		bearer     string
		cookie     string
		wantStatus int
	}{
		{name: "open when no keys configured", proxy: &responsesProxy{}, target: "/dashboard", wantStatus: http.StatusOK},
		{name: "no credentials rejected", proxy: newKeyedProxy(), target: "/dashboard", wantStatus: http.StatusUnauthorized},
		{name: "bearer admin accepted", proxy: newKeyedProxy(), target: "/dashboard", bearer: "admin-key", wantStatus: http.StatusOK},
		{name: "bearer client role forbidden", proxy: newKeyedProxy(), target: "/dashboard", bearer: "client-key", wantStatus: http.StatusForbidden},
		{name: "bearer wrong key rejected", proxy: newKeyedProxy(), target: "/dashboard", bearer: "wrong", wantStatus: http.StatusUnauthorized},
		{name: "cookie admin accepted", proxy: newKeyedProxy(), target: "/dashboard", cookie: "admin-key", wantStatus: http.StatusOK},
		{name: "cookie client role forbidden", proxy: newKeyedProxy(), target: "/dashboard", cookie: "client-key", wantStatus: http.StatusForbidden},
		{name: "cookie wrong key rejected", proxy: newKeyedProxy(), target: "/dashboard", cookie: "stale-key", wantStatus: http.StatusUnauthorized},
		{name: "root path gated too", proxy: newKeyedProxy(), target: "/", wantStatus: http.StatusUnauthorized},
		{name: "query key admin exchanged for cookie", proxy: newKeyedProxy(), target: "/dashboard?key=admin-key", wantStatus: http.StatusSeeOther},
		{name: "query key client role forbidden", proxy: newKeyedProxy(), target: "/dashboard?key=client-key", wantStatus: http.StatusForbidden},
		{name: "query key wrong rejected", proxy: newKeyedProxy(), target: "/dashboard?key=wrong", wantStatus: http.StatusUnauthorized},
		{name: "single legacy key keeps dashboard access", proxy: &responsesProxy{cfg: config{apiKey: "legacy"}}, target: "/dashboard", bearer: "legacy", wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, tt.target, nil)
			if tt.bearer != "" {
				request.Header.Set("Authorization", "Bearer "+tt.bearer)
			}
			if tt.cookie != "" {
				request.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: tt.cookie})
			}
			recorder := httptest.NewRecorder()
			tt.proxy.handleDashboard(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantStatus == http.StatusOK && !strings.Contains(recorder.Body.String(), "<!doctype html>") {
				t.Fatal("200 response did not serve the dashboard HTML")
			}
			if body := recorder.Body.String(); strings.Contains(body, "admin-key") || strings.Contains(body, "client-key") {
				t.Fatal("response body echoes a configured key")
			}
		})
	}
}

// TestDashboardCookieExchangeFlow walks the browser flow end to end: visiting
// /dashboard?key=<admin key> sets an HttpOnly cookie and redirects to
// /dashboard, and the cookie then authenticates both the HTML page and the
// dashboard data APIs.
func TestDashboardCookieExchangeFlow(t *testing.T) {
	proxy := newKeyedProxy()

	request := httptest.NewRequest(http.MethodGet, "/dashboard?key=admin-key", nil)
	recorder := httptest.NewRecorder()
	proxy.handleDashboard(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("exchange status = %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/dashboard" {
		t.Fatalf("redirect location = %q, want /dashboard (no key in the URL)", location)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies set = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != dashboardCookieName || !cookie.HttpOnly {
		t.Fatalf("cookie = %+v, want HttpOnly %s", cookie, dashboardCookieName)
	}

	// The redirect target renders with only the cookie.
	request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	request.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	proxy.handleDashboard(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("post-exchange dashboard status = %d, want 200", recorder.Code)
	}

	// The dashboard data APIs accept the same cookie (the page's fetches carry
	// it automatically).
	for _, api := range []struct {
		name    string
		handler http.HandlerFunc
		path    string
	}{
		{name: "requests", handler: proxy.handleDashboardRequests, path: "/dashboard/api/requests"},
		{name: "costs", handler: proxy.handleDashboardCosts, path: "/dashboard/api/costs"},
		{name: "by-user", handler: proxy.handleUsageByUser, path: "/dashboard/api/usage/by-user"},
	} {
		t.Run(api.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, api.path, nil)
			request.AddCookie(cookie)
			recorder := httptest.NewRecorder()
			api.handler(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s with cookie = %d, want 200: %s", api.path, recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDashboardAPIsRequireAdminRole(t *testing.T) {
	proxy := newKeyedProxy()
	for _, api := range []struct {
		name    string
		handler http.HandlerFunc
		path    string
	}{
		{name: "requests", handler: proxy.handleDashboardRequests, path: "/dashboard/api/requests"},
		{name: "costs", handler: proxy.handleDashboardCosts, path: "/dashboard/api/costs"},
		{name: "by-user", handler: proxy.handleUsageByUser, path: "/dashboard/api/usage/by-user"},
	} {
		t.Run(api.name, func(t *testing.T) {
			// A valid client-role key can call /v1/* but not the dashboard API.
			request := httptest.NewRequest(http.MethodGet, api.path, nil)
			request.Header.Set("Authorization", "Bearer client-key")
			recorder := httptest.NewRecorder()
			api.handler(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("client-role key on %s = %d, want 403", api.path, recorder.Code)
			}
			// No key at all is 401.
			recorder = httptest.NewRecorder()
			api.handler(recorder, httptest.NewRequest(http.MethodGet, api.path, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous on %s = %d, want 401", api.path, recorder.Code)
			}
		})
	}

	// /v1/* accepts the client-role key as before (any enabled key).
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer client-key")
	if !proxy.authorizedClient(request) {
		t.Fatal("client-role key rejected on /v1/*")
	}
}
