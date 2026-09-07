package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// userUsageRow aggregates the request log per (user, model). User "" collects
// requests that carried no attribution (no X-Broker-User header and no
// prompt_cache_key "user:..." fallback).
type userUsageRow struct {
	User             string  `json:"user"`
	Model            string  `json:"model"`
	Requests         int     `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

type userUsageSummary struct {
	Source       string         `json:"source"`
	PersistError string         `json:"persist_error,omitempty"`
	Window       string         `json:"window"`
	Requests     int            `json:"requests"`
	Users        []userUsageRow `json:"users"`
	GeneratedAt  string         `json:"generated_at"`
}

// handleUsageByUser serves GET /dashboard/api/usage/by-user. Admin-gated like
// the rest of the dashboard API. ?window= accepts 24h, 7d, 30d, all (default),
// or any Go duration; it filters by request start time within the retained
// request log (the persisted JSONL when enabled, else the in-memory ring).
func (p *responsesProxy) handleUsageByUser(w http.ResponseWriter, r *http.Request) {
	if !p.requireDashboardAdmin(w, r) {
		return
	}
	age, label, err := parseUsageWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, err := p.requests.userUsageSummary(time.Now().UTC(), age, label)
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "usage aggregation failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// parseUsageWindow parses the ?window= query param into a max entry age.
// 0 means unbounded (the whole retained log).
func parseUsageWindow(value string) (time.Duration, string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "all" {
		return 0, "all", nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, "", fmt.Errorf("invalid window %q: use 24h, 7d, 30d, all, or a duration", value)
		}
		return time.Duration(n) * 24 * time.Hour, value, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, "", fmt.Errorf("invalid window %q: use 24h, 7d, 30d, all, or a duration", value)
	}
	return parsed, value, nil
}

// userUsageSummary aggregates per (user, model) over the retained request log:
// the persisted JSONL file when persistence is enabled (full history), else
// the in-memory ring.
func (s *requestLogStore) userUsageSummary(now time.Time, age time.Duration, window string) (userUsageSummary, error) {
	summary := userUsageSummary{
		Source:      "memory",
		Window:      window,
		GeneratedAt: now.Format(time.RFC3339Nano),
	}
	type key struct{ user, model string }
	rows := map[key]*userUsageRow{}
	consume := func(entry requestLogEntry) {
		started, err := time.Parse(time.RFC3339Nano, entry.StartedAt)
		if err != nil {
			return
		}
		if age > 0 && now.Sub(started) > age {
			return
		}
		summary.Requests++
		model := valueOr(entry.NormalizedModel, entry.Model)
		k := key{user: entry.User, model: model}
		row := rows[k]
		if row == nil {
			row = &userUsageRow{User: entry.User, Model: model}
			rows[k] = row
		}
		row.Requests++
		if entry.InputTokens != nil {
			row.InputTokens += *entry.InputTokens
		}
		if entry.OutputTokens != nil {
			row.OutputTokens += *entry.OutputTokens
		}
		if entry.CachedTokens != nil {
			row.CachedTokens += *entry.CachedTokens
		}
		if entry.CacheWriteTokens != nil {
			row.CacheWriteTokens += *entry.CacheWriteTokens
		}
		if entry.CostUSD != nil {
			row.CostUSD += *entry.CostUSD
		}
	}

	source, err := s.visitRetainedEntries(consume)
	summary.Source, summary.PersistError = source.Source, source.PersistError
	if err != nil {
		return summary, err
	}

	summary.Users = make([]userUsageRow, 0, len(rows))
	for _, row := range rows {
		summary.Users = append(summary.Users, *row)
	}
	sort.Slice(summary.Users, func(i, j int) bool {
		if summary.Users[i].User != summary.Users[j].User {
			return summary.Users[i].User < summary.Users[j].User
		}
		return summary.Users[i].Model < summary.Users[j].Model
	})
	return summary, nil
}
