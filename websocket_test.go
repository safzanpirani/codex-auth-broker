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
		if _, ok := event["max_output_tokens"]; ok {
			t.Error("max_output_tokens should be stripped")
		}
		reasoning, _ := event["reasoning"].(map[string]any)
		if reasoning["effort"] != "high" {
			t.Errorf("reasoning = %#v, want high effort", reasoning)
		}

		completed := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":75}}}}`
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

	create := `{"type":"response.create","model":"gpt-5.5(high)","input":"hello","max_output_tokens":64}`
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
