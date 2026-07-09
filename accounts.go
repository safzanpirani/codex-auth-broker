package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cooldown bounds and defaults. Codex enforces rolling usage windows (a ~5-hour
// bucket and a weekly bucket); when one is exhausted the backend returns 429. We
// prefer an explicit reset from the response, and fall back to these when the
// response carries no machine-readable reset.
const (
	minCooldown           = 30 * time.Second
	maxCooldownDur        = 8 * 24 * time.Hour
	defaultShortCooldown  = 60 * time.Second
	defaultUsageCooldown  = 5 * time.Hour
	defaultWeeklyCooldown = 7 * 24 * time.Hour
	// authErrorCooldown briefly benches an account whose token refresh fails so
	// the pool rotates past it instead of hammering a broken credential.
	authErrorCooldown = 2 * time.Minute
)

var errAllCoolingDown = errors.New("all Codex accounts are cooling down")

// account wraps a single Codex credential (one auth.json) with its own refresh
// state plus a cooldown deadline set when that account hits a rate-limit window.
type account struct {
	index int
	label string
	mgr   *authManager

	mu            sync.Mutex
	cooldownUntil time.Time
	lastReason    string
	lastAccountID string
}

func (a *account) available(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.cooldownUntil.After(now)
}

func (a *account) cool(until time.Time, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if until.After(a.cooldownUntil) {
		a.cooldownUntil = until
	}
	a.lastReason = reason
}

func (a *account) noteAccountID(id string) {
	if id == "" {
		return
	}
	a.mu.Lock()
	a.lastAccountID = id
	a.mu.Unlock()
}

func (a *account) snapshot(now time.Time) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]any{
		"index":     a.index,
		"label":     a.label,
		"available": !a.cooldownUntil.After(now),
	}
	if !a.cooldownUntil.IsZero() {
		out["cooldown_until"] = a.cooldownUntil.UTC().Format(time.RFC3339)
		if secs := int(time.Until(a.cooldownUntil).Seconds()); secs > 0 {
			out["cooldown_seconds"] = secs
		}
	}
	if a.lastReason != "" {
		out["last_reason"] = a.lastReason
	}
	if a.lastAccountID != "" {
		out["account_id"] = a.lastAccountID
	}
	return out
}

// accountPool holds the ordered set of Codex accounts. Selection is sticky:
// requests stay on the active account (keeping the backend prompt cache warm)
// until it hits a rate limit, then rotate to the next available account.
type accountPool struct {
	mu       sync.Mutex
	accounts []*account
	active   int
}

func newAccountPool(files []string, refreshSkew time.Duration, client *http.Client) *accountPool {
	pool := &accountPool{}
	used := map[string]bool{}
	for i, f := range files {
		pool.accounts = append(pool.accounts, &account{
			index: i,
			label: accountLabel(f, used),
			mgr:   &authManager{authFile: f, refreshSkew: refreshSkew, client: client},
		})
	}
	return pool
}

func (p *accountPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// pick returns the next available account, starting from the active one (sticky)
// and advancing round-robin past any that are cooling down.
func (p *accountPool) pick(now time.Time) (*account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.accounts)
	for i := 0; i < n; i++ {
		idx := (p.active + i) % n
		if p.accounts[idx].available(now) {
			p.active = idx
			return p.accounts[idx], nil
		}
	}
	return nil, errAllCoolingDown
}

// preferred returns an available account, or the active one as a best-effort
// fallback when every account is cooling down (used by non-billable calls like
// the model-list fetch where trying anyway is harmless).
func (p *accountPool) preferred(now time.Time) *account {
	if a, err := p.pick(now); err == nil {
		return a
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accounts) == 0 {
		return nil
	}
	return p.accounts[p.active]
}

func (p *accountPool) soonestReset(now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	var soonest time.Time
	for _, a := range p.accounts {
		a.mu.Lock()
		until := a.cooldownUntil
		a.mu.Unlock()
		if until.IsZero() {
			continue
		}
		if soonest.IsZero() || until.Before(soonest) {
			soonest = until
		}
	}
	return soonest
}

func (p *accountPool) statuses(now time.Time) []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, a.snapshot(now))
	}
	return out
}

// accountLabel derives a short, secret-free log label for an auth file. Since
// files are usually all named auth.json in different homes, it prefers the
// parent directory name, and disambiguates any collisions with an index.
func accountLabel(path string, used map[string]bool) string {
	base := filepath.Base(path)
	label := strings.TrimSuffix(base, ".json")
	if label == "auth" || label == "" {
		if dir := filepath.Base(filepath.Dir(path)); dir != "" && dir != "." && dir != "/" {
			label = dir
		}
	}
	if label == "" {
		label = "account"
	}
	if used != nil {
		orig := label
		for i := 2; used[label]; i++ {
			label = orig + "-" + strconv.Itoa(i)
		}
		used[label] = true
	}
	return label
}

// deriveCooldown decides when a rate-limited account should be retried, plus a
// short window label ("weekly" / "5h" / ...) and the source of the decision, for
// logging. It prefers an explicit reset (Retry-After, reset headers, or a
// machine-readable body field) and falls back to a wording-based default.
func deriveCooldown(resp *http.Response, body []byte, now time.Time) (time.Time, string, string) {
	if resp != nil {
		if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
			if until, ok := parseResetValue(v, now); ok {
				return until, classifyWindow(until.Sub(now), body), "retry-after"
			}
		}
		for _, key := range []string{
			"x-codex-primary-reset-after-seconds", "x-codex-reset-after-seconds",
			"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "x-ratelimit-reset",
		} {
			if v := strings.TrimSpace(resp.Header.Get(key)); v != "" {
				if until, ok := parseResetValue(v, now); ok {
					return until, classifyWindow(until.Sub(now), body), "header:" + key
				}
			}
		}
	}
	if until, source, ok := resetFromBody(body, now); ok {
		return until, classifyWindow(until.Sub(now), body), source
	}
	d := defaultCooldownFromBody(body)
	return now.Add(d), classifyWindow(d, body), "default"
}

// resetFromBody scans a 429 JSON body for any reset/retry-style field and
// returns the earliest future reset it finds (conservative: never over-cools).
func resetFromBody(body []byte, now time.Time) (time.Time, string, bool) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return time.Time{}, "", false
	}
	var best time.Time
	var bestKey string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "reset") || strings.Contains(lk, "retry") ||
					strings.Contains(lk, "seconds_until") || strings.Contains(lk, "cooldown") ||
					strings.Contains(lk, "resets_in") || strings.Contains(lk, "resets_at") {
					if until, ok := coerceReset(val, now); ok {
						if best.IsZero() || until.Before(best) {
							best, bestKey = until, "body:"+k
						}
					}
				}
				walk(val)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(parsed)
	if best.IsZero() {
		return time.Time{}, "", false
	}
	return best, bestKey, true
}

func coerceReset(v any, now time.Time) (time.Time, bool) {
	switch t := v.(type) {
	case float64:
		return numericReset(t, now)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return numericReset(f, now)
		}
	case string:
		return parseResetValue(t, now)
	}
	return time.Time{}, false
}

// parseResetValue interprets a string reset value: an integer/float (epoch
// seconds if large, else a delta in seconds) or an RFC3339 timestamp.
func parseResetValue(v string, now time.Time) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return numericReset(f, now)
	}
	if t, err := http.ParseTime(v); err == nil {
		return clampUntil(now, t), true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return clampUntil(now, t), true
	}
	return time.Time{}, false
}

func numericReset(n float64, now time.Time) (time.Time, bool) {
	if n <= 0 {
		return time.Time{}, false
	}
	// Values large enough to be a Unix timestamp are treated as absolute; smaller
	// ones as a delta in seconds from now.
	if n >= 1e9 {
		return clampUntil(now, time.Unix(int64(n), 0)), true
	}
	return now.Add(clampCooldown(time.Duration(n) * time.Second)), true
}

func classifyWindow(d time.Duration, body []byte) string {
	text := strings.ToLower(string(body))
	switch {
	case strings.Contains(text, "week") || d >= 48*time.Hour:
		return "weekly"
	case d >= 90*time.Minute || strings.Contains(text, "5h") ||
		strings.Contains(text, "5-hour") || strings.Contains(text, "5 hour"):
		return "5h"
	case d >= 2*time.Minute:
		return "windowed"
	default:
		return "short"
	}
}

func defaultCooldownFromBody(body []byte) time.Duration {
	text := strings.ToLower(string(body))
	switch {
	case strings.Contains(text, "week"):
		return defaultWeeklyCooldown
	case strings.Contains(text, "usage limit"), strings.Contains(text, "quota"),
		strings.Contains(text, "5 hour"), strings.Contains(text, "5-hour"):
		return defaultUsageCooldown
	default:
		return defaultShortCooldown
	}
}

func clampCooldown(d time.Duration) time.Duration {
	if d < minCooldown {
		return minCooldown
	}
	if d > maxCooldownDur {
		return maxCooldownDur
	}
	return d
}

func clampUntil(now, t time.Time) time.Time {
	return now.Add(clampCooldown(t.Sub(now)))
}
