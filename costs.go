package main

import (
	"net/http"
	"os"
	"sort"
	"time"
)

type costModelTotals struct {
	Model        string  `json:"model"`
	Requests     int     `json:"requests"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
}

type costWindowTotals struct {
	Requests     int               `json:"requests"`
	Priced       int               `json:"priced"`
	CostUSD      float64           `json:"cost_usd"`
	InputTokens  int64             `json:"input_tokens"`
	OutputTokens int64             `json:"output_tokens"`
	CachedTokens int64             `json:"cached_tokens"`
	Models       []costModelTotals `json:"models,omitempty"`
}

type costSummary struct {
	Source       string                      `json:"source"`
	PersistPath  string                      `json:"persist_path,omitempty"`
	PersistBytes int64                       `json:"persist_bytes,omitempty"`
	Windows      map[string]costWindowTotals `json:"windows"`
	GeneratedAt  string                      `json:"generated_at"`
}

var costWindows = []struct {
	key string
	age time.Duration
}{
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
	{"all", 0},
}

func (p *responsesProxy) handleDashboardCosts(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	summary, err := p.requests.costSummary(time.Now().UTC())
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "cost aggregation failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// costSummary aggregates request totals over fixed windows. It scans the
// persisted JSONL file when persistence is enabled (full history), and falls
// back to the in-memory ring otherwise.
func (s *requestLogStore) costSummary(now time.Time) (costSummary, error) {
	summary := costSummary{
		Source:      "memory",
		GeneratedAt: now.Format(time.RFC3339Nano),
	}
	type accumulator struct {
		totals costWindowTotals
		models map[string]*costModelTotals
	}
	accs := make(map[string]*accumulator, len(costWindows))
	for _, win := range costWindows {
		accs[win.key] = &accumulator{models: map[string]*costModelTotals{}}
	}
	consume := func(entry requestLogEntry) {
		started, err := time.Parse(time.RFC3339Nano, entry.StartedAt)
		if err != nil {
			return
		}
		for _, win := range costWindows {
			if win.age > 0 && now.Sub(started) > win.age {
				continue
			}
			acc := accs[win.key]
			acc.totals.Requests++
			if entry.InputTokens != nil {
				acc.totals.InputTokens += *entry.InputTokens
			}
			if entry.OutputTokens != nil {
				acc.totals.OutputTokens += *entry.OutputTokens
			}
			if entry.CachedTokens != nil {
				acc.totals.CachedTokens += *entry.CachedTokens
			}
			if entry.CostUSD == nil {
				continue
			}
			acc.totals.Priced++
			acc.totals.CostUSD += *entry.CostUSD
			model := valueOr(entry.NormalizedModel, entry.Model)
			byModel := acc.models[model]
			if byModel == nil {
				byModel = &costModelTotals{Model: model}
				acc.models[model] = byModel
			}
			byModel.Requests++
			byModel.CostUSD += *entry.CostUSD
			if entry.InputTokens != nil {
				byModel.InputTokens += *entry.InputTokens
			}
			if entry.OutputTokens != nil {
				byModel.OutputTokens += *entry.OutputTokens
			}
			if entry.CachedTokens != nil {
				byModel.CachedTokens += *entry.CachedTokens
			}
		}
	}

	if s != nil && s.persist != nil {
		summary.Source = "file"
		summary.PersistPath = s.persist.path
		if stat, err := os.Stat(s.persist.path); err == nil {
			summary.PersistBytes = stat.Size()
		}
		if err := scanPersistedEntries(s.persist.path, consume); err != nil {
			return summary, err
		}
	} else if s != nil {
		s.mu.Lock()
		entries := make([]requestLogEntry, len(s.entries))
		copy(entries, s.entries)
		s.mu.Unlock()
		for _, entry := range entries {
			consume(entry)
		}
	}

	summary.Windows = make(map[string]costWindowTotals, len(costWindows))
	for _, win := range costWindows {
		acc := accs[win.key]
		models := make([]costModelTotals, 0, len(acc.models))
		for _, byModel := range acc.models {
			models = append(models, *byModel)
		}
		sort.Slice(models, func(i, j int) bool { return models[i].CostUSD > models[j].CostUSD })
		acc.totals.Models = models
		summary.Windows[win.key] = acc.totals
	}
	return summary, nil
}
