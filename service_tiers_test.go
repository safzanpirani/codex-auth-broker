package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the real HTTP dispatch seam under a cold parallel burst, including
// account-specific discovery and failover. No live credentials or backend calls.
func TestServiceTierCatalogParallelDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, model, tier, wantTier string
		chat, failover              bool
		parallel                    int
	}{
		{name: "supported", model: "gpt-5.5(high)", tier: "fast", wantTier: "priority", parallel: 64},
		{name: "unsupported", model: "gpt-5.4", tier: "priority", parallel: 64},
		{name: "chat", model: "gpt-5.4", tier: "fast", chat: true, parallel: 16},
		{name: "flex_without_catalog_support", model: "gpt-5.4", tier: "flex", wantTier: "flex", parallel: 1},
		{name: "dated_snapshot", model: "gpt-5.5-2026-04-23", tier: "priority", wantTier: "priority", parallel: 1},
		{name: "failover_changes_eligibility", model: "gpt-5.5", tier: "priority", failover: true, parallel: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var catalogCalls, served, active, peak atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				acct := r.Header.Get("chatgpt-account-id")
				if r.Method == http.MethodGet {
					catalogCalls.Add(1)
					tiers := `[]`
					if acct == "acct_priority" {
						tiers = `[{"id":"priority","name":"Fast","description":"Priority processing"}]`
					}
					fmt.Fprintf(w, `{"models":[{"slug":"gpt-5.5","service_tiers":%s},{"slug":"gpt-5.4","service_tiers":[]}]}`, tiers)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if tc.failover && acct == "acct_priority" {
					if body["service_tier"] != "priority" {
						t.Error("first account lost its supported tier")
					}
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if got := stringField(body, "service_tier"); got != tc.wantTier {
					t.Errorf("wire tier = %q, want %q", got, tc.wantTier)
				}
				if got, want := r.Header.Get(codexRoutingHintHeader), buildCodexRoutingHint(stringField(body, "model"), tc.wantTier); got != want {
					t.Errorf("routing hint = %q, want %q", got, want)
				}
				if body["stream"] != true {
					t.Error("filtered request lost upstream streaming")
				}
				n := active.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				defer active.Add(-1)
				served.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tier\",\"model\":%q,\"status\":\"completed\",\"output\":[],\"service_tier\":%q}}\n\n", stringField(body, "model"), valueOr(tc.wantTier, "default"))
			}))
			defer upstream.Close()
			files := []string{writeWebSocketTestAuth(t, "acct_priority")}
			if tc.failover {
				files = append(files, writeWebSocketTestAuth(t, "acct_standard"))
			}
			proxy := &responsesProxy{
				cfg:  config{upstreamURL: upstream.URL + "/responses", modelsURL: upstream.URL + "/models"},
				pool: newAccountPool(files, time.Minute, upstream.Client()), client: upstream.Client(),
				requests: newRequestLogStore(tc.parallel), limiter: newConcurrencyLimiter(8, 5*time.Second),
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			began := time.Now()
			for range tc.parallel {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					payload := fmt.Sprintf(`{"model":%q,"input":"hi","service_tier":%q}`, tc.model, tc.tier)
					path := "/v1/responses"
					if tc.chat {
						path = "/v1/chat/completions"
						payload = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"serviceTier":%q}`, tc.model, tc.tier)
					}
					r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
					w := httptest.NewRecorder()
					if tc.chat {
						proxy.handleChatCompletions(w, r)
					} else {
						proxy.handleResponses(w, r)
					}
					if w.Code != http.StatusOK {
						t.Errorf("status = %d, want 200", w.Code)
					}
				}()
			}
			close(start)
			wg.Wait()
			wantFetches := int32(1)
			if tc.tier == "flex" {
				wantFetches = 0
			} else if tc.failover {
				wantFetches = 2
			}
			if got := catalogCalls.Load(); got != wantFetches {
				t.Errorf("catalog fetches = %d, want %d", got, wantFetches)
			}
			if got := served.Load(); got != int32(tc.parallel) {
				t.Errorf("served = %d, want %d", got, tc.parallel)
			}
			if peak.Load() > 8 {
				t.Errorf("upstream concurrency = %d, exceeded 8", peak.Load())
			}
			for _, entry := range proxy.requests.snapshot(tc.parallel).RequestLog {
				if entry.ServiceTier != valueOr(map[string]string{"fast": "priority"}[tc.tier], tc.tier) || entry.AppliedServiceTier != valueOr(tc.wantTier, "default") {
					t.Errorf("requested/applied tiers = %s/%s", entry.ServiceTier, entry.AppliedServiceTier)
				}
			}
			t.Logf("local fixture: %d requests in %s, peak=%d, catalog fetches=%d", tc.parallel, time.Since(began), peak.Load(), catalogCalls.Load())
		})
	}
}

func TestServiceTierCatalogCancellationAndWebSocket(t *testing.T) {
	started, unblock := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-unblock
		fmt.Fprint(w, `{"models":[{"slug":"gpt-5.5","service_tiers":[]}]}`)
	}))
	defer upstream.Close()
	proxy := &responsesProxy{
		cfg: config{modelsURL: upstream.URL}, client: upstream.Client(),
		pool: newAccountPool([]string{writeWebSocketTestAuth(t, "acct_ws")}, time.Minute, upstream.Client()),
	}
	acct := proxy.pool.preferred(time.Now())
	done := make(chan bool, 1)
	go func() { done <- proxy.accountSupportsTier(context.Background(), acct, "gpt-5.5", "priority") }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := proxy.accountTierModels(ctx, acct); err != context.Canceled {
		t.Errorf("canceled waiter error = %v", err)
	}
	close(unblock)
	if <-done {
		t.Error("unsupported tier accepted")
	}
	tracker := &webSocketTurnTracker{proxy: proxy, account: acct, request: httptest.NewRequest(http.MethodGet, "/v1/responses", nil)}
	payload, err := proxy.normalizeWebSocketClientEvent([]byte(`{"type":"response.create","model":"gpt-5.5(high)","serviceTier":"fast","input":"hi"}`), tracker)
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if _, ok := event["service_tier"]; ok {
		t.Error("WebSocket event retained unsupported priority")
	}
	if calls.Load() != 1 {
		t.Errorf("catalog fetches = %d, want 1", calls.Load())
	}
}

// A downgraded compaction must not be priced at the requested priority tier.
func TestCompactionRecordsAppliedServiceTier(t *testing.T) {
	for _, tc := range []struct{ name, catalogTiers, upstreamTier, wantWire, wantApplied string }{
		{name: "downgraded_unreported", catalogTiers: `[]`, wantApplied: "default"},
		{name: "supported_reported", catalogTiers: `[{"id":"priority"}]`, upstreamTier: `,"service_tier":"priority"`, wantWire: "priority", wantApplied: "priority"},
		{name: "supported_unreported", catalogTiers: `[{"id":"priority"}]`, wantWire: "priority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprintf(w, `{"models":[{"slug":"gpt-5.5","service_tiers":%s}]}`, tc.catalogTiers)
					return
				}
				body, _ := decodeRequestBody(r.Body, "")
				if got := stringField(body, "service_tier"); got != tc.wantWire {
					t.Errorf("wire tier = %q, want %q", got, tc.wantWire)
				}
				fmt.Fprintf(w, `{"output":[],"usage":{"input_tokens":1}%s}`, tc.upstreamTier)
			}))
			defer upstream.Close()
			p := capabilityProxy(t, upstream)
			p.cfg.modelsURL = upstream.URL + "/models"
			r := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5.5","input":[],"service_tier":"fast"}`))
			r.Header.Set("Authorization", "Bearer local-key")
			w := httptest.NewRecorder()
			newServerMux(p).ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			entries := p.requests.snapshot(1).RequestLog
			if len(entries) != 1 || entries[0].ServiceTier != "priority" || entries[0].AppliedServiceTier != tc.wantApplied {
				t.Fatalf("requested/applied tiers = %+v, want priority/%s", entries, tc.wantApplied)
			}
		})
	}
}
