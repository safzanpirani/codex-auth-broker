package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func usageWindowPayload(windowSeconds int, usedPercent float64, resetAt time.Time) map[string]any {
	return map[string]any{
		"limit_window_seconds": windowSeconds,
		"used_percent":         usedPercent,
		"reset_at":             resetAt.Unix(),
		"reset_after_seconds":  int(time.Until(resetAt).Seconds()),
	}
}

// usagePayload builds a wham/usage body decoded the way fetchCodexUsage sees it
// (JSON numbers as float64).
func usagePayload(primary, secondary any) map[string]any {
	payload := map[string]any{
		"plan_type":  "prolite",
		"email":      "user@example.com",
		"account_id": "acct_test",
		"rate_limit": map[string]any{
			"allowed":          true,
			"limit_reached":    false,
			"primary_window":   primary,
			"secondary_window": secondary,
		},
		"rate_limit_reached_type": nil,
		"credits": map[string]any{
			"balance":   "62309.5080560000",
			"unlimited": false,
		},
		"rate_limit_reset_credits": map[string]any{"available_count": 2, "applicable_available_count": 0},
		"model_usage": map[string]any{
			"gpt-6-astra": map[string]any{"available": true, "available_at": nil},
			"gpt-6-sol":   map[string]any{"available": false, "available_at": 1793520448},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		panic(err)
	}
	return decoded
}

// newUsageTestProxy serves usage from upstream through the real mux.
func newUsageTestProxy(t *testing.T, apiKey string, upstream http.HandlerFunc) http.Handler {
	t.Helper()
	accessToken := fakeAccessToken(t, "acct_test", time.Now().Add(time.Hour))
	authFile := filepath.Join(t.TempDir(), "auth.json")
	rawBytes, err := json.Marshal(map[string]any{"tokens": map[string]any{
		"access_token":  accessToken,
		"refresh_token": "refresh-not-used",
		"account_id":    "acct_test",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, rawBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	proxy := &responsesProxy{
		cfg:    config{usageURL: server.URL, apiKey: apiKey},
		pool:   newAccountPool([]string{authFile}, time.Minute, server.Client()),
		client: server.Client(),
	}
	return newServerMux(proxy)
}

func TestSummarizeCodexUsageClassifiesWindowsBySeconds(t *testing.T) {
	now := time.Date(2026, 10, 25, 14, 47, 15, 0, time.UTC)
	weeklyReset := now.Add(6*24*time.Hour + 17*time.Hour)
	shortReset := now.Add(3*time.Hour + 12*time.Minute)
	weekly := usageWindowPayload(604800, 5, weeklyReset)
	short := usageWindowPayload(18000, 42.5, shortReset)
	tests := []struct {
		name               string
		primary, secondary any
		wantFiveHour       bool
	}{
		{name: "weekly primary, null secondary", primary: weekly, secondary: nil},
		{name: "five-hour primary, weekly secondary", primary: short, secondary: weekly, wantFiveHour: true},
		{name: "weekly primary, five-hour secondary", primary: weekly, secondary: short, wantFiveHour: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := summarizeCodexUsage(usagePayload(tt.primary, tt.secondary), now)
			if summary.Weekly == nil {
				t.Fatal("weekly = nil, want weekly window")
			}
			if summary.Weekly.UsedPercent != 5 || summary.Weekly.RemainingPercent != 95 {
				t.Fatalf("weekly = %+v, want 5%% used / 95%% left", summary.Weekly)
			}
			if summary.Weekly.ResetAt != "2026-11-01T07:47:15Z" || summary.Weekly.ResetsInSeconds != int64(6*86400+17*3600) {
				t.Fatalf("weekly reset = %q in %d", summary.Weekly.ResetAt, summary.Weekly.ResetsInSeconds)
			}
			if !tt.wantFiveHour {
				if summary.FiveHour != nil {
					t.Fatalf("five_hour = %+v, want nil", summary.FiveHour)
				}
				return
			}
			if summary.FiveHour == nil || summary.FiveHour.UsedPercent != 42.5 || summary.FiveHour.ResetsInSeconds != 3*3600+12*60 {
				t.Fatalf("five_hour = %+v, want 42.5%% resetting in 3h12m", summary.FiveHour)
			}
		})
	}
}

func TestSummarizeCodexUsageCompactFields(t *testing.T) {
	now := time.Date(2026, 10, 25, 14, 47, 15, 0, time.UTC)
	exhausted := usageWindowPayload(18000, 100, now.Add(time.Hour))
	summary := summarizeCodexUsage(usagePayload(exhausted, nil), now)
	if summary.Plan != "prolite" || summary.Email != "user@example.com" || summary.FetchedAt != "2026-10-25T14:47:15Z" {
		t.Fatalf("summary identity = %q %q %q", summary.Plan, summary.Email, summary.FetchedAt)
	}
	if summary.Weekly != nil || summary.FiveHour == nil || !summary.FiveHour.LimitReached || !summary.LimitReached {
		t.Fatalf("windows = weekly %+v five_hour %+v limit_reached %t", summary.Weekly, summary.FiveHour, summary.LimitReached)
	}
	if summary.ResetCreditsAvailable != 2 {
		t.Fatalf("reset credits = %d, want 2", summary.ResetCreditsAvailable)
	}
	if summary.Credits == nil || summary.Credits.Balance != "62309.5080560000" || summary.Credits.Unlimited {
		t.Fatalf("credits = %+v", summary.Credits)
	}
	if len(summary.Models) != 2 || !summary.Models["gpt-6-astra"] || summary.Models["gpt-6-sol"] {
		t.Fatalf("models = %#v", summary.Models)
	}
}

func TestUsageSummaryTextLine(t *testing.T) {
	now := time.Date(2026, 10, 25, 14, 47, 15, 0, time.UTC)
	weekly := usageWindowPayload(604800, 5, now.Add(6*24*time.Hour+17*time.Hour))
	got := summarizeCodexUsage(usagePayload(weekly, nil), now).textLine()
	want := "prolite · weekly 5% used (95% left), resets in 6d17h (2026-11-01 13:17 IST) · 5h n/a · 2 reset credits"
	if got != want {
		t.Fatalf("text line =\n%q\nwant\n%q", got, want)
	}

	short := usageWindowPayload(18000, 100, now.Add(42*time.Minute))
	got = summarizeCodexUsage(usagePayload(weekly, short), now).textLine()
	if !strings.Contains(got, "· 5h 100% used (0% left), resets in 42m (2026-10-25 20:59 IST), limit reached ·") {
		t.Fatalf("text line = %q, want exhausted 5h segment", got)
	}
}

func TestUsageEndpointServesJSONAndText(t *testing.T) {
	handler := newUsageTestProxy(t, "", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, usagePayload(usageWindowPayload(604800, 5, time.Now().Add(48*time.Hour+30*time.Minute)), nil))
	})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["plan"] != "prolite" || body["five_hour"] != nil {
		t.Fatalf("body = %#v", body)
	}
	if weekly, ok := body["weekly"].(map[string]any); !ok || !strings.HasSuffix(weekly["reset_at"].(string), "Z") {
		t.Fatalf("weekly = %#v, want UTC reset_at", body["weekly"])
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/usage?format=text", nil))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("text status = %d content-type %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	if got := recorder.Body.String(); !strings.HasPrefix(got, "prolite · weekly 5% used (95% left), resets in 2d0h") || !strings.HasSuffix(got, "IST) · 5h n/a · 2 reset credits\n") {
		t.Fatalf("text body = %q", got)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/usage?format=xml", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown format status = %d, want 400", recorder.Code)
	}
}

func TestUsageEndpointRequiresClientKey(t *testing.T) {
	handler := newUsageTestProxy(t, "secret-key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, usagePayload(nil, nil))
	})
	for _, tt := range []struct {
		header string
		want   int
	}{
		{header: "", want: http.StatusUnauthorized},
		{header: "Bearer wrong", want: http.StatusUnauthorized},
		{header: "Bearer secret-key", want: http.StatusOK},
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
		if tt.header != "" {
			request.Header.Set("Authorization", tt.header)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != tt.want {
			t.Fatalf("Authorization %q: status = %d, want %d", tt.header, recorder.Code, tt.want)
		}
	}
}

func TestUsageEndpointMapsUpstreamErrors(t *testing.T) {
	var leaked string
	handler := newUsageTestProxy(t, "", func(w http.ResponseWriter, r *http.Request) {
		leaked = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		writeJSON(w, http.StatusForbidden, map[string]any{
			"detail": map[string]any{"code": "token_expired", "message": "token expired"},
		})
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/usage?format=text", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
	if leaked == "" || strings.Contains(recorder.Body.String(), leaked) {
		t.Fatal("error body echoed the access token")
	}
	var body struct {
		Error struct {
			Message        string `json:"message"`
			UpstreamStatus int    `json:"upstream_status"`
			UpstreamCode   string `json:"upstream_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.UpstreamStatus != http.StatusForbidden || body.Error.UpstreamCode != "token_expired" || !strings.Contains(body.Error.Message, "HTTP 403") {
		t.Fatalf("error = %+v", body.Error)
	}
}
