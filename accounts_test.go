package main

import (
	"net/http"
	"testing"
	"time"
)

func testPool(n int) *accountPool {
	files := make([]string, n)
	for i := range files {
		files[i] = "/tmp/acct" + string(rune('a'+i)) + "/auth.json"
	}
	return newAccountPool(files, time.Minute, http.DefaultClient)
}

func TestPoolStickyThenRotate(t *testing.T) {
	pool := testPool(3)
	now := time.Now()

	// Sticky: repeated picks stay on the same (active) account.
	a1, err := pool.pick(now)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	a2, _ := pool.pick(now)
	if a1 != a2 {
		t.Fatalf("expected sticky pick, got %s then %s", a1.label, a2.label)
	}

	// Cool the active account: next pick rotates to a different one.
	a1.cool(now.Add(time.Hour), "5h")
	a3, err := pool.pick(now)
	if err != nil {
		t.Fatalf("pick after cool: %v", err)
	}
	if a3 == a1 {
		t.Fatalf("expected rotation away from cooled account %s", a1.label)
	}
}

func TestPoolAllCoolingDown(t *testing.T) {
	pool := testPool(2)
	now := time.Now()
	for _, a := range pool.accounts {
		a.cool(now.Add(2*time.Hour), "weekly")
	}
	if _, err := pool.pick(now); err != errAllCoolingDown {
		t.Fatalf("want errAllCoolingDown, got %v", err)
	}
	// After the window passes, the pool recovers.
	if _, err := pool.pick(now.Add(3 * time.Hour)); err != nil {
		t.Fatalf("expected recovery after cooldown, got %v", err)
	}
}

func TestDeriveCooldownRetryAfterSeconds(t *testing.T) {
	now := time.Now()
	resp := &http.Response{Header: http.Header{"Retry-After": {"120"}}}
	until, window, source := deriveCooldown(resp, nil, now)
	if source != "retry-after" {
		t.Fatalf("source = %q, want retry-after", source)
	}
	if d := until.Sub(now); d < 119*time.Second || d > 121*time.Second {
		t.Fatalf("cooldown = %v, want ~120s", d)
	}
	if window != "windowed" {
		t.Fatalf("window = %q, want windowed", window)
	}
}

func TestDeriveCooldownBodyResetSeconds(t *testing.T) {
	now := time.Now()
	body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":18000,"message":"You've hit your 5-hour usage limit."}}`)
	resp := &http.Response{Header: http.Header{}}
	until, window, source := deriveCooldown(resp, body, now)
	if source != "body:resets_in_seconds" {
		t.Fatalf("source = %q, want body:resets_in_seconds", source)
	}
	if d := until.Sub(now); d < 4*time.Hour || d > 6*time.Hour {
		t.Fatalf("cooldown = %v, want ~5h", d)
	}
	if window != "5h" {
		t.Fatalf("window = %q, want 5h", window)
	}
}

func TestDeriveCooldownWeeklyWording(t *testing.T) {
	now := time.Now()
	body := []byte(`{"detail":"You've reached your weekly usage limit. Try again later."}`)
	resp := &http.Response{Header: http.Header{}}
	until, window, source := deriveCooldown(resp, body, now)
	if source != "default" {
		t.Fatalf("source = %q, want default", source)
	}
	if window != "weekly" {
		t.Fatalf("window = %q, want weekly", window)
	}
	if d := until.Sub(now); d < 6*24*time.Hour {
		t.Fatalf("cooldown = %v, want ~7d", d)
	}
}

func TestDeriveCooldownEpochTimestamp(t *testing.T) {
	now := time.Now()
	reset := now.Add(3 * time.Hour).Unix()
	body := []byte(`{"resets_at":` + itoa(reset) + `}`)
	resp := &http.Response{Header: http.Header{}}
	until, _, source := deriveCooldown(resp, body, now)
	if source != "body:resets_at" {
		t.Fatalf("source = %q, want body:resets_at", source)
	}
	if d := until.Sub(now); d < 150*time.Minute || d > 200*time.Minute {
		t.Fatalf("cooldown = %v, want ~3h", d)
	}
}

func TestDeriveCooldownClampsFloor(t *testing.T) {
	now := time.Now()
	resp := &http.Response{Header: http.Header{"Retry-After": {"1"}}}
	until, _, _ := deriveCooldown(resp, nil, now)
	if d := until.Sub(now); d < minCooldown {
		t.Fatalf("cooldown = %v, want clamped to >= %v", d, minCooldown)
	}
}

func TestAccountLabelDisambiguates(t *testing.T) {
	used := map[string]bool{}
	l1 := accountLabel("/home/ubuntu/.codex/auth.json", used)
	l2 := accountLabel("/home/ubuntu/.codex-2/auth.json", used)
	l3 := accountLabel("/other/.codex/auth.json", used) // same dir base ".codex" as l1
	if l1 != ".codex" {
		t.Fatalf("l1 = %q, want .codex", l1)
	}
	if l2 != ".codex-2" {
		t.Fatalf("l2 = %q, want .codex-2", l2)
	}
	if l3 == l1 {
		t.Fatalf("expected disambiguated label, got duplicate %q", l3)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
