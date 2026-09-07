package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func startShutdownTestServer(t *testing.T, handler http.Handler, timeout time.Duration) (string, context.CancelFunc, <-chan struct{}, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop, cancel := context.WithCancel(context.Background())
	server := &http.Server{Handler: handler}
	draining := make(chan struct{})
	server.RegisterOnShutdown(func() { close(draining) })
	done := make(chan error, 1)
	go func() { done <- serveHTTP(stop, server, listener, timeout) }()
	t.Cleanup(cancel)
	return "http://" + listener.Addr().String(), cancel, draining, done
}

func awaitShutdown(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestServerShutdownDrainsHTTPStream(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	url, stop, draining, done := startShutdownTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		_, _ = io.WriteString(w, "started\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "finished\n")
		case <-r.Context().Done():
			t.Error("draining canceled an admitted request")
		}
	}), 2*time.Second)
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "started\n" {
		t.Fatalf("initial stream: %q, %v", line, err)
	}
	stop()
	<-draining
	if next, err := client.Get(url); err == nil {
		next.Body.Close()
		t.Fatal("server accepted a new connection during drain")
	}
	close(release)
	remaining, err := io.ReadAll(reader)
	if err != nil || string(remaining) != "finished\n" {
		t.Fatalf("drained stream: %q, %v", remaining, err)
	}
	awaitShutdown(t, done)
	select {
	case <-finished:
	default:
		t.Fatal("returned before handler finished")
	}
}

func TestServerShutdownCancelsHTTPAtDeadline(t *testing.T) {
	finished := make(chan struct{})
	url, stop, _, done := startShutdownTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "started\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(finished)
	}), 30*time.Millisecond)
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	stop()
	awaitShutdown(t, done)
	select {
	case <-finished:
	default:
		t.Fatal("handler context was not canceled")
	}
}

func TestServerShutdownTracksHijackedWebSocketSessions(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "deadline"}[forced], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			upstreamClosed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamClosed)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_, _, err = conn.Read(ctx)
				if err != nil {
					return
				}
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"test"}}`))
				_, _, _ = conn.Read(ctx)
			}))
			defer upstream.Close()
			proxy := &responsesProxy{cfg: config{upstreamURL: upstream.URL}, client: upstream.Client(), pool: newAccountPool([]string{writeWebSocketTestAuth(t, "test-account")}, 0, upstream.Client()), requests: newRequestLogStore(10)}
			timeout := 2 * time.Second
			if forced {
				timeout = 40 * time.Millisecond
			}
			url, stop, draining, done := startShutdownTestServer(t, newServerMux(proxy), timeout)
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-5.5","input":"test"}`)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := conn.Read(ctx); err != nil {
				t.Fatal(err)
			}
			stop()
			<-draining
			if !forced {
				select {
				case err := <-done:
					t.Fatalf("returned while WebSocket was open: %v", err)
				default:
				}
				_ = conn.CloseNow()
			}
			awaitShutdown(t, done)
			if forced {
				if _, _, err := conn.Read(ctx); err == nil {
					t.Error("WebSocket remained open after deadline")
				}
			}
			select {
			case <-upstreamClosed:
			case <-ctx.Done():
				t.Fatal("upstream WebSocket did not close")
			}
			rows := proxy.requests.snapshot(10).RequestLog
			if len(rows) != 1 || rows[0].Error == "" {
				t.Fatalf("unfinished turn was not logged before shutdown: %+v", rows)
			}
		})
	}
}

func TestServerDrainRejectsLateHandlers(t *testing.T) {
	requests := &serverRequests{done: make(chan struct{})}
	handler := requests.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("late handler was admitted") }), context.Background())
	requests.drain()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("drain rejection status %d", w.Code)
	}
}

func TestServerSignalHelper(t *testing.T) {
	if os.Getenv("BROKER_SHUTDOWN_TEST_HELPER") != "1" {
		return
	}
	if err := runServe([]string{"--listen", "127.0.0.1:0", "--request-log-file", "", "--shutdown-timeout", "100ms"}); err != nil {
		t.Fatal(err)
	}
}

func TestServerInterruptExitsCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt delivery to child processes is not supported on Windows")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServerSignalHelper$")
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "CODEX_") || strings.HasPrefix(entry, "HOME=") || strings.HasPrefix(entry, "BROKER_SHUTDOWN_TEST_HELPER=") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "HOME="+t.TempDir(), "CODEX_AUTH_FILE="+filepath.Join(t.TempDir(), "unused-auth.json"), "BROKER_SHUTDOWN_TEST_HELPER=1")
	output, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(output)
	ready := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "codex-auth-broker listening on") {
			ready = true
			break
		}
	}
	if !ready {
		_ = cmd.Wait()
		t.Fatal("child never listened")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, output)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("interrupt exit: %v", err)
	}
}
