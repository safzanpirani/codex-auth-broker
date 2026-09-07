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
	ClientName              string   `json:"client_name,omitempty"`
	User                    string   `json:"user,omitempty"`
	RequestID               string   `json:"request_id,omitempty"`
	Model                   string   `json:"model,omitempty"`
	NormalizedModel         string   `json:"normalized_model,omitempty"`
	ReasoningEffort         string   `json:"reasoning_effort,omitempty"`
	ServiceTier             string   `json:"service_tier,omitempty"`
	AppliedServiceTier      string   `json:"applied_service_tier,omitempty"`
	Stream                  bool     `json:"stream"`
	Status                  int      `json:"status"`
	UpstreamStatus          int      `json:"upstream_status,omitempty"`
	Error                   string   `json:"error,omitempty"`
	PromptCacheKeySet       bool     `json:"prompt_cache_key_set"`
	PromptCacheKey          string   `json:"prompt_cache_key,omitempty"`
	PromptCacheRetentionSet bool     `json:"prompt_cache_retention_set"`
	PromptCacheRetention    string   `json:"prompt_cache_retention,omitempty"`
	InputCount              int      `json:"input_count,omitempty"`
	ToolCount               int      `json:"tool_count,omitempty"`
	InputTokens             *int64   `json:"input_tokens,omitempty"`
	OutputTokens            *int64   `json:"output_tokens,omitempty"`
	CachedTokens            *int64   `json:"cached_tokens,omitempty"`
	CacheWriteTokens        *int64   `json:"cache_write_tokens,omitempty"`
	TotalTokens             *int64   `json:"total_tokens,omitempty"`
	CostUSD                 *float64 `json:"cost_usd,omitempty"`
	APICostUSD              *float64 `json:"api_cost_usd,omitempty"`
}

type pendingRequestLog struct {
	store   *requestLogStore
	started time.Time
	Entry   requestLogEntry
}

type requestLogSnapshot struct {
	Limit         int               `json:"limit"`
	PersistPath   string            `json:"persist_path,omitempty"`
	PersistError  string            `json:"persist_error,omitempty"`
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
	entry = sanitizeRequestLogEntry(entry)
	entry = s.priceEntry(entry)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	entry.ID = s.nextID
	_ = s.persist.append(entry)
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
	// Recompute both views from stored token counts so pricing changes apply to
	// historical dashboard rows.
	for i := range entries {
		entries[i] = s.priceEntry(entries[i])
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
	persistError := ""
	if s.persist != nil {
		s.persist.mu.RLock()
		persistPath = s.persist.path
		persistError = s.persist.lastError
		s.persist.mu.RUnlock()
	}
	return requestLogSnapshot{
		Limit:         s.limit,
		PersistPath:   persistPath,
		PersistError:  persistError,
		Retained:      len(s.entries),
		TotalSeen:     s.nextID,
		RequestLog:    requests,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		RedactionNote: requestLogRedactionNote,
	}
}

func (s *requestLogStore) clear() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persist.clear(); err != nil {
		return err
	}
	s.entries = nil
	return nil
}

func (p *responsesProxy) beginRequestLog(r *http.Request) *pendingRequestLog {
	if p.requests == nil || p.requests.limit == 0 {
		return nil
	}
	started := time.Now().UTC()
	entry := requestLogEntry{
		StartedAt: started.Format(time.RFC3339Nano),
		Method:    r.Method,
		Path:      r.URL.Path,
		Client:    clientAddress(r.RemoteAddr),
		RequestID: requestIDFromHeaders(r),
	}
	// Attribute the entry using the authentication snapshot installed by API
	// middleware, so authorization and accounting use the same key identity.
	// Direct handler callers fall back to resolution; unauthorized requests keep
	// an empty client name.
	if id, ok := p.authenticate(r); ok {
		entry.ClientName = id.Name
		entry.User = sanitizeBrokerUser(r.Header.Get(brokerUserHeader))
	}
	return &pendingRequestLog{
		store:   p.requests,
		started: started,
		Entry:   entry,
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
	l.store.add(l.Entry)
}

// priceEntry uses today's configured rates everywhere history is presented.
// Stores without a pricing table preserve supplied estimates (useful for
// metadata-only consumers); production always installs a table at startup.
func (s *requestLogStore) priceEntry(entry requestLogEntry) requestLogEntry {
	if s == nil || s.pricing == nil {
		return entry
	}
	model := valueOr(entry.NormalizedModel, entry.Model)
	tier := valueOr(entry.AppliedServiceTier, entry.ServiceTier)
	usage := tokenUsage{
		InputTokens:      entry.InputTokens,
		OutputTokens:     entry.OutputTokens,
		CachedTokens:     entry.CachedTokens,
		CacheWriteTokens: entry.CacheWriteTokens,
		TotalTokens:      entry.TotalTokens,
	}
	entry.CostUSD = estimateCostUSDForTier(s.pricing, model, tier, usage)
	entry.APICostUSD = estimateAPICostUSDForTier(s.pricing, model, tier, usage)
	return entry
}

func (l *pendingRequestLog) markError(status int, message string) {
	if l == nil {
		return
	}
	l.Entry.Status = status
	l.Entry.Error = message
}

func (l *pendingRequestLog) markStatus(status int) {
	if l == nil {
		return
	}
	l.Entry.Status = status
}

func (l *pendingRequestLog) markUpstreamStatus(status int) {
	if l == nil {
		return
	}
	l.Entry.UpstreamStatus = status
}

func (l *pendingRequestLog) markStreamError(message string) {
	if l == nil {
		return
	}
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
	if info.PromptCacheKey != "" {
		l.Entry.PromptCacheKey = "sha256:" + secretFingerprint(info.PromptCacheKey)
	}
	l.Entry.PromptCacheRetentionSet = info.PromptCacheRetentionSet
	l.Entry.PromptCacheRetention = info.PromptCacheRetention
	if user := extractBrokerUser(r, info.PromptCacheKey); user != "" {
		l.Entry.User = user
	}
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

func (l *pendingRequestLog) markImageRequest(model string, stream bool, inputCount int) {
	if l == nil {
		return
	}
	l.Entry.Model = model
	l.Entry.NormalizedModel = model
	l.Entry.Stream = stream
	l.Entry.InputCount = inputCount
	l.Entry.ToolCount = 1
}

func (l *pendingRequestLog) markAppliedServiceTier(tier string) {
	if l == nil {
		return
	}
	l.Entry.AppliedServiceTier = tier
}

func (l *pendingRequestLog) markUsage(usage tokenUsage) {
	if l == nil {
		return
	}
	l.Entry.InputTokens = usage.InputTokens
	l.Entry.OutputTokens = usage.OutputTokens
	l.Entry.CachedTokens = usage.CachedTokens
	l.Entry.CacheWriteTokens = usage.CacheWriteTokens
	l.Entry.TotalTokens = usage.TotalTokens
}

// brokerUserHeader carries the end-user identity a client attributes its
// request to (e.g. a Google SSO email). Trusted only because the request
// already carried a valid client key: clients own the truthfulness of it.
const brokerUserHeader = "X-Broker-User"

// brokerUserCacheKeyPrefix is the zero-client-change fallback: a client that
// cannot set headers may send prompt_cache_key "user:<identity>" instead. The
// header wins when both are present.
const brokerUserCacheKeyPrefix = "user:"

// maxBrokerUserLen caps the stored user identity.
const maxBrokerUserLen = 128

// extractBrokerUser resolves the per-request user: the X-Broker-User header
// first, then the prompt_cache_key "user:<identity>" convention.
func extractBrokerUser(r *http.Request, promptCacheKey string) string {
	if user := sanitizeBrokerUser(r.Header.Get(brokerUserHeader)); user != "" {
		return user
	}
	if rest, ok := strings.CutPrefix(promptCacheKey, brokerUserCacheKeyPrefix); ok {
		return sanitizeBrokerUser(rest)
	}
	return ""
}

// sanitizeBrokerUser strips control characters, trims whitespace, caps length,
// and redacts token-like values so a pasted secret never lands in the log.
func sanitizeBrokerUser(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if len(value) > maxBrokerUserLen {
		value = value[:maxBrokerUserLen]
	}
	return redactTokenLikeText(value)
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
	if max <= 3 {
		return strings.Repeat(".", max)
	}
	return value[:max-3] + "..."
}

func sanitizeRequestLogEntry(entry requestLogEntry) requestLogEntry {
	clean := func(value string, max int) string {
		return truncateLogField(redactTokenLikeText(value), max)
	}
	entry.Method = clean(entry.Method, 16)
	entry.Path = clean(entry.Path, 256)
	entry.Client = clean(entry.Client, 128)
	entry.ClientName = clean(entry.ClientName, 128)
	entry.User = clean(entry.User, maxBrokerUserLen)
	entry.RequestID = clean(entry.RequestID, 256)
	entry.Model = clean(entry.Model, 256)
	entry.NormalizedModel = clean(entry.NormalizedModel, 256)
	entry.ReasoningEffort = clean(entry.ReasoningEffort, 64)
	entry.ServiceTier = clean(entry.ServiceTier, 64)
	entry.AppliedServiceTier = clean(entry.AppliedServiceTier, 64)
	entry.Error = clean(entry.Error, 300)
	if entry.PromptCacheKey != "" && !isRequestLogFingerprint(entry.PromptCacheKey) {
		entry.PromptCacheKey = "sha256:" + secretFingerprint(entry.PromptCacheKey)
	}
	entry.PromptCacheKey = clean(entry.PromptCacheKey, 64)
	entry.PromptCacheRetention = clean(entry.PromptCacheRetention, 64)
	return entry
}

func isRequestLogFingerprint(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+12 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, char := range value[len(prefix):] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
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
