package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Windows at or below this length are the short (5-hour) bucket; longer ones
// are the weekly bucket. wham/usage reports 18000 and 604800 today, in either
// the primary or secondary slot depending on the plan.
const shortUsageWindowMaxSeconds = 24 * 60 * 60

// istZone renders reset times for the text format only; JSON stays UTC.
var istZone = time.FixedZone("IST", 5*60*60+30*60)

type usageWindowSummary struct {
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
	ResetAt          string  `json:"reset_at,omitempty"`
	ResetsInSeconds  int64   `json:"resets_in_seconds"`
	LimitReached     bool    `json:"limit_reached"`
	resetAt          time.Time
}

type usageCreditsSummary struct {
	Balance   any  `json:"balance"`
	Unlimited bool `json:"unlimited"`
}

type usageSummary struct {
	Plan                  string               `json:"plan"`
	Email                 string               `json:"email,omitempty"`
	FetchedAt             string               `json:"fetched_at"`
	Weekly                *usageWindowSummary  `json:"weekly"`
	FiveHour              *usageWindowSummary  `json:"five_hour"`
	LimitReached          bool                 `json:"limit_reached"`
	ResetCreditsAvailable int                  `json:"reset_credits_available"`
	Credits               *usageCreditsSummary `json:"credits"`
	Models                map[string]bool      `json:"models"`
}

// handleUsageSummary serves GET /v1/usage: a compact view of the live
// wham/usage payload, as JSON or (?format=text) one human-readable line.
func (p *responsesProxy) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format != "" && format != "json" && format != "text" {
		writeProxyError(w, http.StatusBadRequest, "format must be json or text")
		return
	}
	raw, _, err := p.fetchCodexUsage(r.Context())
	if err != nil {
		writeUsageFetchError(w, err)
		return
	}
	now := time.Now()
	summary := summarizeCodexUsage(raw, now)
	w.Header().Set("Cache-Control", "no-store")
	if format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, summary.textLine())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// writeUsageFetchError maps every usage fetch failure to 502, carrying the
// upstream HTTP status and error code when the usage API answered.
func writeUsageFetchError(w http.ResponseWriter, err error) {
	body := map[string]any{
		"message": redactTokenLikeText(err.Error()),
		"type":    "codex_auth_broker_error",
	}
	if upstream, ok := errors.AsType[*usageUpstreamError](err); ok {
		body["upstream_status"] = upstream.status
		if upstream.code != "" {
			body["upstream_code"] = upstream.code
		}
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": body})
}

func summarizeCodexUsage(raw map[string]any, now time.Time) usageSummary {
	summary := usageSummary{
		Plan:      stringField(raw, "plan_type"),
		Email:     stringField(raw, "email"),
		FetchedAt: now.UTC().Format(time.RFC3339),
		Models:    map[string]bool{},
	}
	rateLimit, _ := raw["rate_limit"].(map[string]any)
	if limitReached, ok := rateLimit["limit_reached"].(bool); ok {
		summary.LimitReached = limitReached
	}
	for _, slot := range []string{"primary_window", "secondary_window"} {
		window, ok := rateLimit[slot].(map[string]any)
		if !ok {
			continue
		}
		seconds, ok := numericField(window, "limit_window_seconds")
		short := slot == "primary_window" // pre-classification fallback
		if ok && seconds > 0 {
			short = seconds <= shortUsageWindowMaxSeconds
		}
		target := &summary.Weekly
		if short {
			target = &summary.FiveHour
		}
		if *target != nil {
			continue
		}
		*target = summarizeUsageWindow(window, now)
		summary.LimitReached = summary.LimitReached || (*target).LimitReached
	}
	if resetCredits, ok := raw["rate_limit_reset_credits"].(map[string]any); ok {
		summary.ResetCreditsAvailable = intField(resetCredits, "available_count")
	}
	if credits, ok := raw["credits"].(map[string]any); ok {
		unlimited, _ := credits["unlimited"].(bool)
		summary.Credits = &usageCreditsSummary{Balance: credits["balance"], Unlimited: unlimited}
	}
	if models, ok := raw["model_usage"].(map[string]any); ok {
		for id, value := range models {
			entry, _ := value.(map[string]any)
			available, _ := entry["available"].(bool)
			summary.Models[id] = available
		}
	}
	return summary
}

func summarizeUsageWindow(window map[string]any, now time.Time) *usageWindowSummary {
	used, _ := numericField(window, "used_percent")
	out := &usageWindowSummary{
		UsedPercent:      used,
		RemainingPercent: math.Max(0, math.Min(100, 100-used)),
		LimitReached:     used >= 100,
	}
	if resetAt, ok := numericField(window, "reset_at"); ok && resetAt > 0 {
		out.resetAt = time.Unix(int64(resetAt), 0).UTC()
	} else if after, ok := numericField(window, "reset_after_seconds"); ok && after >= 0 {
		out.resetAt = now.Add(time.Duration(after) * time.Second).UTC().Truncate(time.Second)
	}
	if !out.resetAt.IsZero() {
		out.ResetAt = out.resetAt.Format(time.RFC3339)
		out.ResetsInSeconds = secondsUntil(out.resetAt, now)
	}
	return out
}

// textLine renders e.g.
// "prolite · weekly 5% used (95% left), resets in 6d17h (2026-11-01 05:37 IST) · 5h n/a · 2 reset credits".
func (s usageSummary) textLine() string {
	parts := []string{valueOr(s.Plan, "unknown"), "weekly " + s.Weekly.text(), "5h " + s.FiveHour.text()}
	credits := strconv.Itoa(s.ResetCreditsAvailable) + " reset credits"
	if s.ResetCreditsAvailable == 1 {
		credits = "1 reset credit"
	}
	parts = append(parts, credits)
	if s.LimitReached && !(s.Weekly != nil && s.Weekly.LimitReached) && !(s.FiveHour != nil && s.FiveHour.LimitReached) {
		parts = append(parts, "limit reached")
	}
	return strings.Join(parts, " · ")
}

func (w *usageWindowSummary) text() string {
	if w == nil {
		return "n/a"
	}
	text := fmt.Sprintf("%s%% used (%s%% left)", formatUsagePercent(w.UsedPercent), formatUsagePercent(w.RemainingPercent))
	if !w.resetAt.IsZero() {
		text += fmt.Sprintf(", resets in %s (%s)", formatUsageDuration(w.ResetsInSeconds), w.resetAt.In(istZone).Format("2006-01-02 15:04 MST"))
	}
	if w.LimitReached {
		text += ", limit reached"
	}
	return text
}

func formatUsagePercent(value float64) string {
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64)
}

func formatUsageDuration(seconds int64) string {
	days, hours, minutes := seconds/86400, seconds%86400/3600, seconds%3600/60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
