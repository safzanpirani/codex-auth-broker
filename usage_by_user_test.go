package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractBrokerUser(t *testing.T) {
	tests := []struct {
		name           string
		header         string
		promptCacheKey string
		want           string
	}{
		{name: "header present", header: "alice@example.com", want: "alice@example.com"},
		{name: "header wins over cache key", header: "alice@example.com", promptCacheKey: "user:bob@example.com", want: "alice@example.com"},
		{name: "cache key fallback", promptCacheKey: "user:bob@example.com", want: "bob@example.com"},
		{name: "cache key without prefix ignored", promptCacheKey: "session-1234", want: ""},
		{name: "neither present", want: ""},
		{name: "control characters stripped", header: "ali\x00ce\r\n@example.com", want: "alice@example.com"},
		{name: "whitespace trimmed", header: "  alice@example.com  ", want: "alice@example.com"},
		{name: "long value truncated", header: strings.Repeat("ab ", 60), want: strings.TrimSpace(strings.Repeat("ab ", 60))[:maxBrokerUserLen]},
		{name: "empty cache key user ignored", promptCacheKey: "user:", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tt.header != "" {
				request.Header.Set(brokerUserHeader, tt.header)
			}
			if got := extractBrokerUser(request, tt.promptCacheKey); got != tt.want {
				t.Fatalf("extractBrokerUser = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeBrokerUserRedactsTokenLikeValues(t *testing.T) {
	leaked := strings.Repeat("A", 80) // token-like: long single-charset run
	if got := sanitizeBrokerUser(leaked); strings.Contains(got, leaked) {
		t.Fatalf("token-like user stored verbatim: %q", got)
	}
}

func TestMarkRequestRecordsUser(t *testing.T) {
	proxy := &responsesProxy{requests: newRequestLogStore(10)}
	body := map[string]any{
		"model":            "gpt-5.5",
		"input":            "hi",
		"prompt_cache_key": "user:bob@example.com",
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{}, request)
	entry := proxy.beginRequestLog(request)
	entry.markRequest(body, info, request)
	if entry.Entry.User != "bob@example.com" {
		t.Fatalf("user = %q, want bob@example.com", entry.Entry.User)
	}

	// Header wins over the prompt_cache_key convention.
	request.Header.Set(brokerUserHeader, "alice@example.com")
	entry = proxy.beginRequestLog(request)
	entry.markRequest(body, info, request)
	if entry.Entry.User != "alice@example.com" {
		t.Fatalf("user = %q, want alice@example.com", entry.Entry.User)
	}
}

func TestParseUsageWindow(t *testing.T) {
	tests := []struct {
		value    string
		wantAge  time.Duration
		wantName string
		wantErr  bool
	}{
		{value: "", wantAge: 0, wantName: "all"},
		{value: "all", wantAge: 0, wantName: "all"},
		{value: "24h", wantAge: 24 * time.Hour, wantName: "24h"},
		{value: "7d", wantAge: 7 * 24 * time.Hour, wantName: "7d"},
		{value: "30d", wantAge: 30 * 24 * time.Hour, wantName: "30d"},
		{value: "90m", wantAge: 90 * time.Minute, wantName: "90m"},
		{value: "0d", wantErr: true},
		{value: "-24h", wantErr: true},
		{value: "banana", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			age, name, err := parseUsageWindow(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseUsageWindow(%q) succeeded, want error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUsageWindow(%q) failed: %v", tt.value, err)
			}
			if age != tt.wantAge || name != tt.wantName {
				t.Fatalf("parseUsageWindow(%q) = %s, %q; want %s, %q", tt.value, age, name, tt.wantAge, tt.wantName)
			}
		})
	}
}

func TestUserUsageSummaryAggregates(t *testing.T) {
	now := time.Now().UTC()
	store := newRequestLogStore(100)
	seed := func(user, model string, age time.Duration, input, output, cached int64, cost float64) {
		store.add(requestLogEntry{
			StartedAt:       now.Add(-age).Format(time.RFC3339Nano),
			User:            user,
			NormalizedModel: model,
			InputTokens:     &input,
			OutputTokens:    &output,
			CachedTokens:    &cached,
			CostUSD:         &cost,
		})
	}
	seed("alice@example.com", "gpt-5.5", time.Hour, 100, 10, 50, 0.5)
	seed("alice@example.com", "gpt-5.5", time.Hour, 200, 20, 100, 1.0)
	seed("alice@example.com", "gpt-5.3-codex", time.Hour, 10, 1, 0, 0.1)
	seed("bob@example.com", "gpt-5.5", 3*24*time.Hour, 400, 40, 0, 2.0)
	seed("", "gpt-5.5", time.Hour, 5, 5, 0, 0.05) // unattributed

	summary, err := store.userUsageSummary(now, 0, "all")
	if err != nil {
		t.Fatalf("userUsageSummary failed: %v", err)
	}
	if summary.Requests != 5 {
		t.Fatalf("requests = %d, want 5", summary.Requests)
	}
	if len(summary.Users) != 4 {
		t.Fatalf("rows = %d, want 4 (user,model pairs)", len(summary.Users))
	}
	byKey := map[string]userUsageRow{}
	for _, row := range summary.Users {
		byKey[row.User+"|"+row.Model] = row
	}
	alice := byKey["alice@example.com|gpt-5.5"]
	if alice.Requests != 2 || alice.InputTokens != 300 || alice.OutputTokens != 30 || alice.CachedTokens != 150 || alice.CostUSD != 1.5 {
		t.Fatalf("alice gpt-5.5 row = %+v", alice)
	}
	if anon := byKey["|gpt-5.5"]; anon.Requests != 1 {
		t.Fatalf("unattributed row = %+v", anon)
	}

	// Window filter: bob's 3-day-old request drops out of a 24h window.
	windowed, err := store.userUsageSummary(now, 24*time.Hour, "24h")
	if err != nil {
		t.Fatalf("userUsageSummary(24h) failed: %v", err)
	}
	if windowed.Requests != 4 {
		t.Fatalf("windowed requests = %d, want 4", windowed.Requests)
	}
	for _, row := range windowed.Users {
		if row.User == "bob@example.com" {
			t.Fatalf("bob's old request survived the 24h window: %+v", row)
		}
	}
}

func TestUsageByUserEndpoint(t *testing.T) {
	store := newRequestLogStore(100)
	input := int64(100)
	cost := 0.5
	store.add(requestLogEntry{
		StartedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		User:            "alice@example.com",
		NormalizedModel: "gpt-5.5",
		InputTokens:     &input,
		CostUSD:         &cost,
	})
	proxy := &responsesProxy{cfg: config{apiKey: "admin-key"}, requests: store}

	request := httptest.NewRequest(http.MethodGet, "/dashboard/api/usage/by-user", nil)
	request.Header.Set("Authorization", "Bearer admin-key")
	recorder := httptest.NewRecorder()
	proxy.handleUsageByUser(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var summary userUsageSummary
	if err := json.Unmarshal(recorder.Body.Bytes(), &summary); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(summary.Users) != 1 || summary.Users[0].User != "alice@example.com" || summary.Users[0].InputTokens != 100 {
		t.Fatalf("summary = %+v", summary)
	}

	// Admin-gated: no key → 401; bad window → 400.
	recorder = httptest.NewRecorder()
	proxy.handleUsageByUser(recorder, httptest.NewRequest(http.MethodGet, "/dashboard/api/usage/by-user", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/dashboard/api/usage/by-user?window=banana", nil)
	request.Header.Set("Authorization", "Bearer admin-key")
	recorder = httptest.NewRecorder()
	proxy.handleUsageByUser(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad window status = %d, want 400", recorder.Code)
	}
}
