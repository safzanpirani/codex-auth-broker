package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const responsesWebSocketBeta = "responses_websockets=2026-02-06"

var responsesWebSocketResponseHeaders = []string{
	"x-codex-turn-state",
	"x-models-etag",
	"x-reasoning-included",
	"openai-model",
}

// handleResponsesWebSocket terminates the client WebSocket and opens a second,
// authenticated WebSocket to the Codex backend. Terminating both sides lets the
// broker normalize response.create events without ever exposing the Codex OAuth
// token to the client.
func (p *responsesProxy) handleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !isWebSocketUpgrade(r) {
		writeProxyError(w, http.StatusUpgradeRequired, "use a WebSocket Upgrade request or POST /v1/responses")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream, response, acct, err := p.dialResponsesWebSocket(ctx, r)
	if err != nil {
		status := http.StatusBadGateway
		if response != nil && response.StatusCode >= 400 {
			status = response.StatusCode
		}
		writeProxyError(w, status, "upstream WebSocket handshake failed")
		return
	}
	defer upstream.CloseNow()

	for _, key := range responsesWebSocketResponseHeaders {
		if value := response.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	downstream, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		return
	}
	defer downstream.CloseNow()
	downstream.SetReadLimit(maxRequestBodyBytes)
	upstream.SetReadLimit(maxRequestBodyBytes)

	log.Printf("responses websocket connected account=%s", acct.label)
	tracker := &webSocketTurnTracker{proxy: p, request: r, account: acct}
	errorsCh := make(chan error, 2)
	go func() {
		errorsCh <- p.copyWebSocketClientToUpstream(ctx, downstream, upstream, tracker)
	}()
	go func() {
		errorsCh <- copyWebSocketUpstreamToClient(ctx, upstream, downstream, tracker)
	}()

	err = <-errorsCh
	cancel()
	tracker.finishOpen(err)
	closeWebSocketPeer(upstream, err)
	closeWebSocketPeer(downstream, err)
	log.Printf("responses websocket disconnected account=%s status=%d", acct.label, websocket.CloseStatus(err))
}

func (p *responsesProxy) dialResponsesWebSocket(ctx context.Context, r *http.Request) (*websocket.Conn, *http.Response, *account, error) {
	n := p.pool.size()
	if n == 0 {
		return nil, nil, nil, errors.New("no Codex accounts configured")
	}
	var lastResponse *http.Response
	var lastErr error
	for attempt := 0; attempt < n; attempt++ {
		acct, err := p.pool.pick(time.Now())
		if err != nil {
			break
		}
		access, err := acct.mgr.current(ctx)
		if err != nil {
			acct.cool(time.Now().Add(authErrorCooldown), "auth error: "+err.Error())
			lastErr = err
			continue
		}
		acct.noteAccountID(access.AccountID)
		headers := p.responsesWebSocketHeaders(r, access)
		conn, response, err := websocket.Dial(ctx, p.cfg.upstreamURL, &websocket.DialOptions{
			HTTPClient:      webSocketHTTPClient(p.client),
			HTTPHeader:      headers,
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err == nil {
			return conn, response, acct, nil
		}
		lastResponse, lastErr = response, err
		if response == nil || response.StatusCode != http.StatusTooManyRequests {
			break
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		until, window, source := deriveCooldown(response, body, time.Now())
		acct.cool(until, window)
		log.Printf("codex account %s websocket handshake rate-limited window=%s source=%s; rotating (%d/%d)", acct.label, window, source, attempt+1, n)
	}
	return nil, lastResponse, nil, lastErr
}

func (p *responsesProxy) responsesWebSocketHeaders(r *http.Request, access accessMaterial) http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+access.AccessToken)
	headers.Set("chatgpt-account-id", access.AccountID)
	headers.Set("originator", valueOr(strings.TrimSpace(p.cfg.upstreamOriginator), "codex-auth-broker"))
	if headers.Get("originator") == "codex-auth-broker" {
		headers.Set("User-Agent", "codex-auth-broker/"+valueOr(version, "dev"))
	} else {
		headers.Set("User-Agent", headers.Get("originator")+"/"+p.cfg.modelsClientVersion)
	}
	for _, key := range []string{
		"x-codex-turn-state",
		"x-codex-turn-metadata",
		"x-codex-parent-thread-id",
		"x-codex-window-id",
		"x-codex-installation-id",
		"x-responsesapi-include-timing-metrics",
		"x-openai-internal-codex-responses-lite",
		"x-openai-subagent",
		"x-openai-memgen-request",
		"session_id",
		"x-client-request-id",
		"traceparent",
		"tracestate",
	} {
		if values := r.Header.Values(key); len(values) > 0 {
			headers[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
		}
	}
	headers.Set("OpenAI-Beta", mergeHeaderToken(r.Header.Get("OpenAI-Beta"), responsesWebSocketBeta))
	return headers
}

func (p *responsesProxy) copyWebSocketClientToUpstream(ctx context.Context, downstream, upstream *websocket.Conn, tracker *webSocketTurnTracker) error {
	for {
		typ, payload, err := readWebSocketMessage(ctx, downstream)
		if err != nil {
			return err
		}
		if typ == websocket.MessageText {
			payload, err = p.normalizeWebSocketClientEvent(payload, tracker)
			if err != nil {
				return fmt.Errorf("invalid response.create event: %w", err)
			}
		}
		if err := upstream.Write(ctx, typ, payload); err != nil {
			return err
		}
	}
}

func copyWebSocketUpstreamToClient(ctx context.Context, upstream, downstream *websocket.Conn, tracker *webSocketTurnTracker) error {
	for {
		typ, payload, err := readWebSocketMessage(ctx, upstream)
		if err != nil {
			return err
		}
		shouldReconnect := false
		if typ == websocket.MessageText {
			shouldReconnect = tracker.observeServerEvent(payload)
		}
		if err := downstream.Write(ctx, typ, payload); err != nil {
			return err
		}
		if shouldReconnect {
			return errors.New("upstream account rate-limited; reconnect to fail over")
		}
	}
}

func (p *responsesProxy) normalizeWebSocketClientEvent(payload []byte, tracker *webSocketTurnTracker) ([]byte, error) {
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	if stringField(event, "type") != "response.create" {
		return payload, nil
	}
	event["stream"] = true
	info := normalizeResponsesBody(event, p.cfg, tracker.request)
	tracker.begin(event, info)
	return json.Marshal(event)
}

func readWebSocketMessage(ctx context.Context, conn *websocket.Conn) (websocket.MessageType, []byte, error) {
	typ, reader, err := conn.Reader(ctx)
	if err != nil {
		return 0, nil, err
	}
	payload, err := io.ReadAll(io.LimitReader(reader, maxRequestBodyBytes+1))
	if err != nil {
		return 0, nil, err
	}
	if len(payload) > maxRequestBodyBytes {
		return 0, nil, errors.New("WebSocket message too large")
	}
	return typ, payload, nil
}

type webSocketTurnTracker struct {
	mu      sync.Mutex
	proxy   *responsesProxy
	request *http.Request
	account *account
	pending *pendingRequestLog
}

func (t *webSocketTurnTracker) begin(body map[string]any, info requestInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending != nil {
		t.pending.markError(http.StatusBadGateway, "another response.create arrived before the previous response completed")
		t.pending.finish()
	}
	t.pending = t.proxy.beginRequestLog(t.request)
	t.pending.markRequest(body, info, t.request)
	if t.pending != nil {
		t.pending.Entry.Method = "WS"
		t.pending.Entry.Status = http.StatusOK
		t.pending.Entry.UpstreamStatus = http.StatusSwitchingProtocols
	}
}

func (t *webSocketTurnTracker) observeServerEvent(payload []byte) bool {
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	kind := stringField(event, "type")
	if kind != "response.completed" && kind != "response.done" && kind != "response.incomplete" && kind != "response.failed" && kind != "error" {
		return false
	}
	rateLimited := kind == "error" && webSocketEventStatus(event) == http.StatusTooManyRequests
	if rateLimited && t.account != nil {
		response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: webSocketEventHeaders(event)}
		until, window, source := deriveCooldown(response, payload, time.Now())
		t.account.cool(until, window)
		log.Printf("codex account %s websocket rate-limited window=%s source=%s; reconnect required", t.account.label, window, source)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		return rateLimited
	}
	if response, ok := event["response"].(map[string]any); ok {
		t.pending.markUsage(extractTokenUsage(response))
	}
	if kind == "response.failed" || kind == "error" {
		t.pending.Entry.Error = webSocketEventError(event)
	}
	t.pending.finish()
	t.pending = nil
	return rateLimited
}

func (t *webSocketTurnTracker) finishOpen(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		return
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusNormalClosure && status != websocket.StatusGoingAway {
		t.pending.markError(http.StatusBadGateway, "WebSocket closed before response completion")
	}
	t.pending.finish()
	t.pending = nil
}

func webSocketEventError(event map[string]any) string {
	if errBody, ok := event["error"].(map[string]any); ok {
		if message := stringField(errBody, "message"); message != "" {
			return message
		}
	}
	return stringField(event, "type")
}

func webSocketEventStatus(event map[string]any) int {
	if value, ok := numericField(event, "status"); ok {
		return int(value)
	}
	if errBody, ok := event["error"].(map[string]any); ok {
		if value, ok := numericField(errBody, "status"); ok {
			return int(value)
		}
	}
	return 0
}

func webSocketEventHeaders(event map[string]any) http.Header {
	headers := make(http.Header)
	raw, _ := event["headers"].(map[string]any)
	for key, value := range raw {
		if text, ok := value.(string); ok {
			headers.Set(key, text)
		}
	}
	return headers
}

func webSocketHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{}
	}
	clone := *client
	clone.Timeout = 0
	return &clone
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") &&
		headerHasToken(r.Header.Get("Connection"), "upgrade")
}

func mergeHeaderToken(value, required string) string {
	if headerHasToken(value, required) {
		return value
	}
	if strings.TrimSpace(value) == "" {
		return required
	}
	return value + ", " + required
}

func headerHasToken(value, wanted string) bool {
	for _, token := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), wanted) {
			return true
		}
	}
	return false
}

func closeWebSocketPeer(conn *websocket.Conn, err error) {
	status := websocket.CloseStatus(err)
	// RFC 6455 reserves these values for local reporting; they cannot appear in
	// a Close frame.
	if status < 0 || status == websocket.StatusNoStatusRcvd || status == websocket.StatusAbnormalClosure || status == websocket.StatusTLSHandshake {
		status = websocket.StatusInternalError
	}
	reason := "peer closed"
	if status == websocket.StatusInternalError {
		reason = "proxy connection closed"
	}
	_ = conn.Close(status, reason)
}
