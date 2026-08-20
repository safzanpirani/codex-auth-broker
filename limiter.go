package main

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	// defaultQueueWait bounds how long a request waits for a free upstream slot
	// before the broker gives up and returns 429. Queueing (rather than
	// rejecting immediately) keeps short bursts transparent to clients.
	defaultQueueWait = 120 * time.Second
	// queueTimeoutRetryAfter is the Retry-After hint returned with the 429 when
	// the queue wait cap is exceeded.
	queueTimeoutRetryAfter = 30 * time.Second
)

var errQueueWaitExceeded = errors.New("broker concurrency limit reached and queue wait exceeded")

// concurrencyLimiter is a global semaphore around upstream Codex calls. A nil
// *concurrencyLimiter means unlimited; every method is nil-safe so existing
// call sites and tests that construct responsesProxy directly keep working.
type concurrencyLimiter struct {
	slots     chan struct{}
	queueWait time.Duration
	queued    atomic.Int64
}

// newConcurrencyLimiter returns nil (unlimited) when max <= 0.
func newConcurrencyLimiter(max int, queueWait time.Duration) *concurrencyLimiter {
	if max <= 0 {
		return nil
	}
	if queueWait <= 0 {
		queueWait = defaultQueueWait
	}
	return &concurrencyLimiter{
		slots:     make(chan struct{}, max),
		queueWait: queueWait,
	}
}

// acquire blocks until a slot is free, the request context is done, or the
// queue wait cap expires. It returns nil once a slot is held; the caller must
// release() exactly once afterwards.
func (l *concurrencyLimiter) acquire(ctx context.Context) error {
	if l == nil {
		return nil
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	default:
	}
	l.queued.Add(1)
	defer l.queued.Add(-1)
	timer := time.NewTimer(l.queueWait)
	defer timer.Stop()
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errQueueWaitExceeded
	}
}

func (l *concurrencyLimiter) release() {
	if l == nil {
		return
	}
	<-l.slots
}

// stats reports the limiter state for /healthz. max_concurrent 0 means the
// limiter is disabled (unlimited).
func (l *concurrencyLimiter) stats() map[string]any {
	if l == nil {
		return map[string]any{"max_concurrent": 0, "in_flight": 0, "queued": 0}
	}
	return map[string]any{
		"max_concurrent": cap(l.slots),
		"in_flight":      len(l.slots),
		"queued":         l.queued.Load(),
	}
}

// acquireUpstreamSlot gates one upstream Codex call (HTTP dispatch or a
// WebSocket session). On success it returns the release func the caller must
// defer; the slot is then held for the full duration of the upstream work, so
// streaming responses and open WebSocket sessions count against the limit for
// as long as they run. On failure it returns a dispatchFailure in the broker's
// standard error shape: 429 + Retry-After when the queue wait cap expired, 408
// when the client's own context ended while queued.
func (p *responsesProxy) acquireUpstreamSlot(ctx context.Context) (func(), *dispatchFailure) {
	err := p.limiter.acquire(ctx)
	if err == nil {
		return p.limiter.release, nil
	}
	if errors.Is(err, errQueueWaitExceeded) {
		return nil, &dispatchFailure{
			status:     http.StatusTooManyRequests,
			message:    errQueueWaitExceeded.Error() + "; retry later",
			retryAfter: time.Now().Add(queueTimeoutRetryAfter),
		}
	}
	// The request context ended while queued: the client gave up (or the
	// server-side deadline fired), so any response is best-effort.
	return nil, &dispatchFailure{
		status:  http.StatusRequestTimeout,
		message: "request canceled while waiting for an upstream slot: " + err.Error(),
	}
}
