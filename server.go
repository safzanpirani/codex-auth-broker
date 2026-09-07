package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

const defaultShutdownTimeout = 30 * time.Second
const shutdownCleanupTimeout = 5 * time.Second

type serverWorkContextKey struct{}

// Track handlers as well as net/http connections: Shutdown does not wait for
// hijacked WebSockets. Admission and draining share a lock to avoid late adds.
type serverRequests struct {
	mu       sync.Mutex
	draining bool
	active   int
	done     chan struct{}
}

func (s *serverRequests) wrap(next http.Handler, work context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.draining {
			s.mu.Unlock()
			w.Header().Set("Connection", "close")
			w.Header().Set("Retry-After", "1")
			writeProxyError(w, http.StatusServiceUnavailable, "broker is shutting down")
			return
		}
		s.active++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active--
			if s.draining && s.active == 0 {
				close(s.done)
			}
		}()
		ctx := context.WithValue(r.Context(), serverWorkContextKey{}, work)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *serverRequests) drain() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.draining = true
		if s.active == 0 {
			close(s.done)
		}
	}
	return s.done
}

// A hijacked connection must not use the incoming request's cancellation
// context. Keep it tied to server shutdown through a separate lifetime.
func webSocketSessionContext(r *http.Request) (context.Context, context.CancelFunc) {
	work, _ := r.Context().Value(serverWorkContextKey{}).(context.Context)
	if work == nil {
		work = context.Background()
	}
	return context.WithCancel(work)
}

// serveHTTP owns the listener and waits for drain/cleanup before returning, so
// callers can close shared resources after handlers have recorded their logs.
func serveHTTP(stop context.Context, server *http.Server, listener net.Listener, timeout time.Duration) error {
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	requests := &serverRequests{done: make(chan struct{})}
	server.Handler = requests.wrap(server.Handler, work)
	server.BaseContext = func(net.Listener) context.Context { return work }
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	var serveErr error
	var alreadyStopped bool
	select {
	case <-stop.Done():
		log.Printf("shutdown requested; draining requests and WebSocket sessions for up to %s", timeout)
	case serveErr = <-served:
		alreadyStopped = true
	}
	done := requests.drain()
	drain, cancelDrain := context.WithTimeout(context.Background(), timeout)
	defer cancelDrain()
	shutdownErr := server.Shutdown(drain)
	if shutdownErr == nil {
		select {
		case <-done:
		case <-drain.Done():
			shutdownErr = drain.Err()
		}
	}
	if shutdownErr != nil {
		log.Printf("shutdown drain ended; canceling remaining requests and closing connections")
		cancelWork()
		_ = server.Close()
		cleanup := time.NewTimer(shutdownCleanupTimeout)
		defer cleanup.Stop()
		select {
		case <-done:
		case <-cleanup.C:
			return errors.New("shutdown cleanup timed out with unfinished handlers")
		}
	}
	if !alreadyStopped {
		serveErr = <-served
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		return shutdownErr
	}
	return nil
}
