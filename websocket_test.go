package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestResponsesWebSocketProxiesNormalizesAndRotatesHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var firstAttempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID := r.Header.Get("ChatGPT-Account-Id")
		if accountID == "acct_one" {
			firstAttempts.Add(1)
			w.Header().Set("Retry-After", "30")
			http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
			return
		}
		if accountID != "acct_two" {
			t.Errorf("ChatGPT-Account-Id = %q, want acct_two", accountID)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("x-codex-turn-state"); got != "turn-state-in" {
			t.Errorf("x-codex-turn-state = %q, want turn-state-in", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); !headerHasToken(got, responsesWebSocketBeta) || !headerHasToken(got, "caller-beta") {
			t.Errorf("OpenAI-Beta = %q, want caller-beta and websocket beta", got)
		}
		if got := r.Header.Get(codexRoutingHintHeader); got != "model=gpt-5.5;tier=priority" {
			t.Errorf("%s = %q, want normalized fast routing hint", codexRoutingHintHeader, got)
		}

		w.Header().Set("x-codex-turn-state", "turn-state-out")
		w.Header().Set("x-models-etag", "models-123")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept upstream websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		_, payload, err := readWebSocketMessage(ctx, conn)
		if err != nil {
			t.Errorf("read upstream event: %v", err)
			return
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Errorf("decode upstream event: %v", err)
			return
		}
		if event["model"] != "gpt-5.5" {
			t.Errorf("model = %#v, want gpt-5.5", event["model"])
		}
		if event["stream"] != true {
			t.Errorf("stream = %#v, want true", event["stream"])
		}
		if event["service_tier"] != "priority" {
			t.Errorf("service_tier = %#v, want priority", event["service_tier"])
		}
		if _, ok := event["serviceTier"]; ok {
			t.Error("serviceTier should be normalized away")
		}
		if _, ok := event["max_output_tokens"]; ok {
			t.Error("max_output_tokens should be stripped")
		}
		reasoning, _ := event["reasoning"].(map[string]any)
		if reasoning["effort"] != "high" {
			t.Errorf("reasoning = %#v, want high effort", reasoning)
		}

		completed := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","service_tier":"default","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":75}}}}`
		if err := conn.Write(ctx, websocket.MessageText, []byte(completed)); err != nil {
			t.Errorf("write upstream event: %v", err)
		}
	}))
	defer upstream.Close()

	authOne := writeWebSocketTestAuth(t, "acct_one")
	authTwo := writeWebSocketTestAuth(t, "acct_two")
	store := newRequestLogStore(10)
	proxy := &responsesProxy{
		cfg: config{
			apiKey:               "client-key",
			upstreamURL:          upstream.URL + "/v1/responses",
			upstreamOriginator:   "codex_cli_rs",
			modelsClientVersion:  "2.0.0",
			promptCacheKey:       "",
			promptCacheRetention: "",
		},
		pool:     newAccountPool([]string{authOne, authTwo}, time.Minute, upstream.Client()),
		requests: store,
		client:   upstream.Client(),
	}
	broker := httptest.NewServer(http.HandlerFunc(proxy.handleResponsesWebSocket))
	defer broker.Close()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer client-key")
	headers.Set("OpenAI-Beta", "caller-beta")
	headers.Set("x-codex-turn-state", "turn-state-in")
	headers.Set(codexRoutingHintHeader, "model=gpt-5.5(high);tier=fast")
	conn, response, err := websocket.Dial(ctx, broker.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatalf("dial broker websocket: %v", err)
	}
	defer conn.CloseNow()
	if got := response.Header.Get("x-codex-turn-state"); got != "turn-state-out" {
		t.Fatalf("downstream x-codex-turn-state = %q, want turn-state-out", got)
	}
	if got := response.Header.Get("x-models-etag"); got != "models-123" {
		t.Fatalf("downstream x-models-etag = %q, want models-123", got)
	}

	create := `{"type":"response.create","model":"gpt-5.5(high)","input":"hello","serviceTier":"fast","max_output_tokens":64}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(create)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := readWebSocketMessage(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"type":"response.completed"`) {
		t.Fatalf("unexpected server event: %s", payload)
	}

	if firstAttempts.Load() != 1 {
		t.Fatalf("first account attempts = %d, want 1", firstAttempts.Load())
	}
	snapshot := store.snapshot(10)
	if snapshot.TotalSeen != 1 || len(snapshot.RequestLog) != 1 {
		t.Fatalf("request log = %#v, want one WebSocket turn", snapshot)
	}
	entry := snapshot.RequestLog[0]
	if entry.Method != "WS" || entry.NormalizedModel != "gpt-5.5" || entry.Status != http.StatusOK {
		t.Fatalf("request entry = %#v", entry)
	}
	if entry.InputTokens == nil || *entry.InputTokens != 100 || entry.CachedTokens == nil || *entry.CachedTokens != 75 {
		t.Fatalf("request usage = %#v", entry)
	}
	if entry.ServiceTier != "priority" || entry.AppliedServiceTier != "default" {
		t.Fatalf("service tiers = requested %q applied %q, want priority/default", entry.ServiceTier, entry.AppliedServiceTier)
	}
}

func TestResponsesWebSocketHeadersRejectsRoutingHintInjection(t *testing.T) {
	proxy := &responsesProxy{cfg: config{upstreamOriginator: "codex_cli_rs", modelsClientVersion: "2.0.0"}}
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set(codexRoutingHintHeader, "model=gpt-5.5;tier=priority;injected=yes")
	headers := proxy.responsesWebSocketHeaders(request, accessMaterial{AccessToken: "token", AccountID: "account"})
	if got := headers.Get(codexRoutingHintHeader); got != "" {
		t.Fatalf("unsafe %s forwarded as %q", codexRoutingHintHeader, got)
	}
}

func TestResponsesWebSocketRequiresUpgrade(t *testing.T) {
	proxy := &responsesProxy{cfg: config{}, pool: &accountPool{}}
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	recorder := httptest.NewRecorder()
	proxy.handleResponsesWebSocket(recorder, req)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUpgradeRequired)
	}
}

func TestWebSocketHTTPClientBoundsHandshakeWithoutLimitingConnection(t *testing.T) {
	const timeout = 50 * time.Millisecond

	t.Run("handshake timeout", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * timeout):
				conn, err := websocket.Accept(w, r, nil)
				if err == nil {
					conn.CloseNow()
				}
			}
		}))
		defer upstream.Close()

		client := upstream.Client()
		client.Timeout = timeout
		dialClient := webSocketHTTPClient(client)
		if dialClient.Timeout != timeout {
			t.Fatalf("WebSocket HTTP client timeout = %s, want %s", dialClient.Timeout, timeout)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, upstream.URL, &websocket.DialOptions{HTTPClient: dialClient})
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil {
			t.Fatal("WebSocket handshake succeeded after the HTTP client timeout")
		}
	})

	t.Run("upgraded connection", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept WebSocket: %v", err)
				return
			}
			defer conn.CloseNow()
			time.Sleep(3 * timeout)
			if err := conn.Write(ctx, websocket.MessageText, []byte("still-open")); err != nil {
				t.Errorf("write after HTTP client timeout: %v", err)
			}
		}))
		defer upstream.Close()

		client := upstream.Client()
		client.Timeout = timeout
		conn, _, err := websocket.Dial(ctx, upstream.URL, &websocket.DialOptions{
			HTTPClient: webSocketHTTPClient(client),
		})
		if err != nil {
			t.Fatalf("dial WebSocket: %v", err)
		}
		defer conn.CloseNow()

		_, payload, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read after HTTP client timeout: %v", err)
		}
		if string(payload) != "still-open" {
			t.Fatalf("payload = %q, want still-open", payload)
		}
	})
}

func TestWebSocketTurnTrackerRecordsFailedTerminalStatus(t *testing.T) {
	tests := []struct {
		name          string
		event         string
		wantStatus    int
		wantError     string
		wantReconnect bool
	}{
		{
			name:          "rate limit",
			event:         `{"type":"error","error":{"message":"private-prompt-sentinel","status":429,"type":"rate_limit_error","code":"rate_limit"}}`,
			wantStatus:    http.StatusTooManyRequests,
			wantError:     "upstream WebSocket error status=429 (type=rate_limit_error code=rate_limit)",
			wantReconnect: true,
		},
		{
			name:       "response failed",
			event:      `{"type":"response.failed","error":{"message":"model failed","status":500},"response":{"status":"failed"}}`,
			wantStatus: http.StatusBadGateway,
			wantError:  "upstream WebSocket error status=502",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newRequestLogStore(10)
			proxy := &responsesProxy{requests: store}
			request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			tracker := &webSocketTurnTracker{proxy: proxy, request: request}
			tracker.begin(map[string]any{"model": "gpt-5.5", "input": []any{}}, requestInfo{NormalizedModel: "gpt-5.5", Stream: true})

			if reconnect := tracker.observeServerEvent([]byte(tt.event)); reconnect != tt.wantReconnect {
				t.Fatalf("reconnect = %t, want %t", reconnect, tt.wantReconnect)
			}
			snapshot := store.snapshot(10)
			if len(snapshot.RequestLog) != 1 {
				t.Fatalf("request log length = %d, want 1", len(snapshot.RequestLog))
			}
			entry := snapshot.RequestLog[0]
			if entry.Status != tt.wantStatus || entry.Error != tt.wantError {
				t.Fatalf("terminal entry status/error = %d/%q, want %d/%q", entry.Status, entry.Error, tt.wantStatus, tt.wantError)
			}
			if strings.Contains(entry.Error, "private-prompt-sentinel") {
				t.Fatalf("terminal entry retained upstream error text: %q", entry.Error)
			}
		})
	}
}

func TestWebSocketTurnTrackerRecordsNormalCloseBeforeTerminalAsFailure(t *testing.T) {
	store := newRequestLogStore(10)
	proxy := &responsesProxy{requests: store}
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	tracker := &webSocketTurnTracker{proxy: proxy, request: request}
	tracker.begin(map[string]any{"model": "gpt-5.5", "input": []any{}}, requestInfo{NormalizedModel: "gpt-5.5", Stream: true})

	tracker.finishOpen(websocket.CloseError{Code: websocket.StatusNormalClosure})
	entry := store.snapshot(10).RequestLog[0]
	if entry.Status != http.StatusBadGateway {
		t.Fatalf("normal close before terminal status = %d, want 502", entry.Status)
	}
}

func writeWebSocketTestAuth(t *testing.T, accountID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	document := map[string]any{
		"tokens": map[string]any{
			"access_token":  fakeAccessToken(t, accountID, time.Now().Add(time.Hour)),
			"refresh_token": "refresh-not-used",
			"account_id":    accountID,
		},
	}
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResponsesWebSocketRejectsBadBearerKey(t *testing.T) {
	proxy := &responsesProxy{
		cfg:      config{apiKey: "client-key"},
		pool:     &accountPool{},
		requests: newRequestLogStore(10),
	}
	broker := httptest.NewServer(http.HandlerFunc(proxy.handleResponsesWebSocket))
	defer broker.Close()

	tests := []struct {
		name   string
		header string
	}{
		{name: "missing key", header: ""},
		{name: "wrong key", header: "Bearer wrong-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			headers := http.Header{}
			if tt.header != "" {
				headers.Set("Authorization", tt.header)
			}
			conn, response, err := websocket.Dial(ctx, broker.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: headers})
			if err == nil {
				conn.CloseNow()
				t.Fatal("websocket dial succeeded, want 401 rejection")
			}
			if response == nil || response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("handshake response = %#v, want 401", response)
			}
		})
	}
}
