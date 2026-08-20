package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConcurrencyLimiterAcquire(t *testing.T) {
	tests := []struct {
		name      string
		max       int
		held      int
		queueWait time.Duration
		cancelCtx bool
		wantErr   error
	}{
		{name: "slot free acquires immediately", max: 2, held: 0, queueWait: time.Second},
		{name: "zero max means unlimited", max: 0, held: 0, queueWait: time.Second},
		{name: "saturated queue times out", max: 1, held: 1, queueWait: 25 * time.Millisecond, wantErr: errQueueWaitExceeded},
		{name: "saturated queue respects context cancel", max: 1, held: 1, queueWait: 10 * time.Second, cancelCtx: true, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := newConcurrencyLimiter(tt.max, tt.queueWait)
			if tt.max <= 0 && limiter != nil {
				t.Fatalf("limiter = %#v, want nil for unlimited", limiter)
			}
			for i := 0; i < tt.held; i++ {
				if err := limiter.acquire(context.Background()); err != nil {
					t.Fatalf("pre-hold slot %d: %v", i, err)
				}
			}
			ctx := context.Background()
			if tt.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := limiter.acquire(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("acquire error = %v, want %v", err, tt.wantErr)
			}
			if err == nil {
				limiter.release()
			}
		})
	}
}

func TestConcurrencyLimiterQueuesUntilRelease(t *testing.T) {
	limiter := newConcurrencyLimiter(1, 5*time.Second)
	if err := limiter.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() {
		acquired <- limiter.acquire(context.Background())
	}()

	deadline := time.Now().Add(2 * time.Second)
	for limiter.stats()["queued"].(int64) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("stats = %#v, want one queued waiter", limiter.stats())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-acquired:
		t.Fatalf("second acquire returned %v before release", err)
	default:
	}

	limiter.release()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("queued acquire = %v, want nil after release", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued acquire did not proceed after release")
	}
	stats := limiter.stats()
	if stats["in_flight"] != 1 || stats["max_concurrent"] != 1 || stats["queued"].(int64) != 0 {
		t.Fatalf("stats after handoff = %#v", stats)
	}
	limiter.release()
}

func TestResponsesConcurrencyLimitReturns429AfterQueueWait(t *testing.T) {
	proxy := &responsesProxy{
		cfg:      config{},
		pool:     &accountPool{},
		requests: newRequestLogStore(10),
		limiter:  newConcurrencyLimiter(1, 25*time.Millisecond),
	}
	if err := proxy.limiter.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer proxy.limiter.release()

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":false}`))
	recorder := httptest.NewRecorder()
	proxy.handleResponses(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("Retry-After header missing, headers = %#v", recorder.Header())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"codex_auth_broker_error"`) || !strings.Contains(body, "concurrency limit") {
		t.Fatalf("error body = %s, want broker error shape mentioning the concurrency limit", body)
	}
}

func TestChatCompletionsConcurrencyLimitReturns429AfterQueueWait(t *testing.T) {
	proxy := &responsesProxy{
		cfg:      config{},
		pool:     &accountPool{},
		requests: newRequestLogStore(10),
		limiter:  newConcurrencyLimiter(1, 25*time.Millisecond),
	}
	if err := proxy.limiter.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer proxy.limiter.release()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	proxy.handleChatCompletions(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("Retry-After header missing, headers = %#v", recorder.Header())
	}
}

func TestResponsesConcurrencyLimitQueuesThenServes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: response.completed",
			`data: {"type":"response.completed","response":{"id":"resp_q","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}`,
			"",
		}, "\n")))
	}))
	defer upstream.Close()

	authFile := writeWebSocketTestAuth(t, "acct_queue")
	proxy := &responsesProxy{
		cfg: config{
			upstreamURL:         upstream.URL,
			upstreamOriginator:  "codex_cli_rs",
			modelsClientVersion: "2.0.0",
		},
		pool:     newAccountPool([]string{authFile}, time.Minute, upstream.Client()),
		requests: newRequestLogStore(10),
		client:   upstream.Client(),
		limiter:  newConcurrencyLimiter(1, 5*time.Second),
	}
	// Saturate the single slot so the request has to queue.
	if err := proxy.limiter.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"hello","stream":false}`))
		recorder := httptest.NewRecorder()
		proxy.handleResponses(recorder, request)
		done <- recorder
	}()

	deadline := time.Now().Add(2 * time.Second)
	for proxy.limiter.stats()["queued"].(int64) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("request never queued, stats = %#v", proxy.limiter.stats())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case recorder := <-done:
		t.Fatalf("request completed with %d before slot release", recorder.Code)
	default:
	}

	proxy.limiter.release()
	select {
	case recorder := <-done:
		if recorder.Code != http.StatusOK {
			t.Fatalf("queued request status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued request did not complete after release")
	}
	stats := proxy.limiter.stats()
	if stats["in_flight"] != 0 || stats["queued"].(int64) != 0 {
		t.Fatalf("stats after completion = %#v, want released slot", stats)
	}
}

func TestHealthReportsConcurrencyStats(t *testing.T) {
	proxy := &responsesProxy{
		cfg:      config{},
		pool:     &accountPool{},
		requests: newRequestLogStore(10),
		limiter:  newConcurrencyLimiter(4, time.Second),
	}
	if err := proxy.limiter.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer proxy.limiter.release()

	recorder := httptest.NewRecorder()
	proxy.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"max_concurrent":4`, `"in_flight":1`, `"queued":0`} {
		if !strings.Contains(body, want) {
			t.Fatalf("healthz body = %s, want %s", body, want)
		}
	}
}
