package main

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type requestLogStore struct {
	mu      sync.Mutex
	limit   int
	nextID  int64
	entries []requestLogEntry
	pricing map[string]modelPricing
	persist *requestLogFile
}

type requestLogEntry struct {
	ID                      int64    `json:"id"`
	StartedAt               string   `json:"started_at"`
	DurationMS              int64    `json:"duration_ms"`
	Method                  string   `json:"method"`
	Path                    string   `json:"path"`
	Client                  string   `json:"client,omitempty"`
	RequestID               string   `json:"request_id,omitempty"`
	Model                   string   `json:"model,omitempty"`
	NormalizedModel         string   `json:"normalized_model,omitempty"`
	ReasoningEffort         string   `json:"reasoning_effort,omitempty"`
	ServiceTier             string   `json:"service_tier,omitempty"`
	Stream                  bool     `json:"stream"`
	Status                  int      `json:"status"`
	UpstreamStatus          int      `json:"upstream_status,omitempty"`
	Error                   string   `json:"error,omitempty"`
	PromptCacheKeySet       bool     `json:"prompt_cache_key_set"`
	PromptCacheRetentionSet bool     `json:"prompt_cache_retention_set"`
	InputCount              int      `json:"input_count,omitempty"`
	ToolCount               int      `json:"tool_count,omitempty"`
	InputTokens             *int64   `json:"input_tokens,omitempty"`
	OutputTokens            *int64   `json:"output_tokens,omitempty"`
	CachedTokens            *int64   `json:"cached_tokens,omitempty"`
	TotalTokens             *int64   `json:"total_tokens,omitempty"`
	CostUSD                 *float64 `json:"cost_usd,omitempty"`
}

type pendingRequestLog struct {
	store   *requestLogStore
	started time.Time
	Entry   requestLogEntry
}

type requestLogSnapshot struct {
	Limit         int               `json:"limit"`
	PersistPath   string            `json:"persist_path,omitempty"`
	Retained      int               `json:"retained"`
	TotalSeen     int64             `json:"total_seen"`
	RequestLog    []requestLogEntry `json:"requests"`
	GeneratedAt   string            `json:"generated_at"`
	RedactionNote string            `json:"redaction_note"`
}

func newRequestLogStore(limit int) *requestLogStore {
	if limit < 0 {
		limit = 0
	}
	return &requestLogStore{limit: limit}
}

func (s *requestLogStore) add(entry requestLogEntry) {
	if s == nil || s.limit == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	entry.ID = s.nextID
	s.persist.append(entry)
	s.entries = append(s.entries, entry)
	if extra := len(s.entries) - s.limit; extra > 0 {
		copy(s.entries, s.entries[extra:])
		s.entries = s.entries[:s.limit]
	}
}

// restore seeds the store from a persisted request log so the dashboard
// survives broker restarts.
func (s *requestLogStore) restore(entries []requestLogEntry, maxID int64) {
	if s == nil || s.limit == 0 || len(entries) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if extra := len(entries) - s.limit; extra > 0 {
		entries = entries[extra:]
	}
	s.entries = append(s.entries, entries...)
	if maxID > s.nextID {
		s.nextID = maxID
	}
}

func (s *requestLogStore) snapshot(limit int) requestLogSnapshot {
	if s == nil {
		return requestLogSnapshot{
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
			RedactionNote: requestLogRedactionNote,
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.entries) {
		limit = len(s.entries)
	}
	requests := make([]requestLogEntry, 0, limit)
	for i := len(s.entries) - 1; i >= 0 && len(requests) < limit; i-- {
		requests = append(requests, s.entries[i])
	}
	persistPath := ""
	if s.persist != nil {
		persistPath = s.persist.path
	}
	return requestLogSnapshot{
		Limit:         s.limit,
		PersistPath:   persistPath,
		Retained:      len(s.entries),
		TotalSeen:     s.nextID,
		RequestLog:    requests,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		RedactionNote: requestLogRedactionNote,
	}
}

func (s *requestLogStore) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = nil
}

func (p *responsesProxy) beginRequestLog(r *http.Request) *pendingRequestLog {
	if p.requests == nil || p.requests.limit == 0 {
		return nil
	}
	started := time.Now().UTC()
	return &pendingRequestLog{
		store:   p.requests,
		started: started,
		Entry: requestLogEntry{
			StartedAt: started.Format(time.RFC3339Nano),
			Method:    r.Method,
			Path:      r.URL.Path,
			Client:    clientAddress(r.RemoteAddr),
			RequestID: requestIDFromHeaders(r),
		},
	}
}

func (l *pendingRequestLog) finish() {
	if l == nil || l.store == nil {
		return
	}
	if l.Entry.Status == 0 {
		l.Entry.Status = http.StatusOK
	}
	l.Entry.DurationMS = time.Since(l.started).Milliseconds()
	l.Entry.Error = truncateLogField(redactTokenLikeText(l.Entry.Error), 300)
	model := valueOr(l.Entry.NormalizedModel, l.Entry.Model)
	l.Entry.CostUSD = estimateCostUSD(l.store.pricing, model, tokenUsage{
		InputTokens:  l.Entry.InputTokens,
		OutputTokens: l.Entry.OutputTokens,
		CachedTokens: l.Entry.CachedTokens,
		TotalTokens:  l.Entry.TotalTokens,
	})
	l.store.add(l.Entry)
}

func (l *pendingRequestLog) markError(status int, message string) {
	if l == nil {
		return
	}
	l.Entry.Status = status
	l.Entry.Error = message
}

func (l *pendingRequestLog) markRequest(body map[string]any, info requestInfo, r *http.Request) {
	if l == nil {
		return
	}
	l.Entry.Model = info.Model
	l.Entry.NormalizedModel = info.NormalizedModel
	l.Entry.ReasoningEffort = info.ReasoningEffort
	l.Entry.ServiceTier = info.ServiceTier
	l.Entry.Stream = info.Stream
	l.Entry.PromptCacheKeySet = info.PromptCacheKeySet
	l.Entry.PromptCacheRetentionSet = info.PromptCacheRetentionSet
	if requestID := requestID(r, body); requestID != "" {
		l.Entry.RequestID = requestID
	}
	if input, ok := body["input"].([]any); ok {
		l.Entry.InputCount = len(input)
	}
	if tools, ok := body["tools"].([]any); ok {
		l.Entry.ToolCount = len(tools)
	}
}

func (l *pendingRequestLog) markUsage(usage tokenUsage) {
	if l == nil {
		return
	}
	l.Entry.InputTokens = usage.InputTokens
	l.Entry.OutputTokens = usage.OutputTokens
	l.Entry.CachedTokens = usage.CachedTokens
	l.Entry.TotalTokens = usage.TotalTokens
}

func requestIDFromHeaders(r *http.Request) string {
	for _, key := range []string{"x-client-request-id", "x-request-id", "session_id"} {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func clientAddress(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		return host
	}
	return remote
}

func truncateLogField(value string, max int) string {
	value = strings.TrimSpace(value)
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[:max-1] + "..."
}

func requestLimitFromQuery(r *http.Request, fallback int) int {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

const requestLogRedactionNote = "request bodies, prompt text, response text, bearer tokens, and refresh tokens are never stored, in memory or on disk"
