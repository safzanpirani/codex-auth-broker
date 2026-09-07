package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const liveModel = "gpt-live-1-codex"
const maxLiveBodyBytes = 1024 * 1024
const maxLiveCalls = 256
const liveCallTTL = 70 * time.Minute

type liveCall struct {
	account *account
	owner   [32]byte
	expires time.Time
}

// Only routing metadata is retained. No SDP, session instructions, media,
// event payloads or bearer credentials belong in the registry or request log.
type liveCallStore struct {
	mu      sync.Mutex
	calls   map[string]liveCall
	pending int
}

func (s *liveCallStore) reserve() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = make(map[string]liveCall)
	}
	for id, call := range s.calls {
		if time.Now().After(call.expires) {
			delete(s.calls, id)
		}
	}
	if len(s.calls)+s.pending >= maxLiveCalls {
		return false
	}
	s.pending++
	return true
}

func (s *liveCallStore) finish(id string, call liveCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending--
	if id != "" {
		s.calls[id] = call
	}
}

func (s *liveCallStore) lookup(id string, owner [32]byte) (liveCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call, ok := s.calls[id]
	if ok && time.Now().After(call.expires) {
		delete(s.calls, id)
		ok = false
	}
	return call, ok && call.owner == owner
}

func liveOwner(r *http.Request) [32]byte {
	return sha256.Sum256([]byte(bearerToken(r)))
}

func decodeLiveCall(w http.ResponseWriter, r *http.Request) (string, map[string]any, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLiveBodyBytes)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return "", nil, errors.New("expected multipart/form-data, application/sdp, or application/json")
	}
	var sdp string
	session := make(map[string]any)
	switch mediaType {
	case "multipart/form-data":
		reader, err := r.MultipartReader()
		if err != nil {
			return "", nil, errors.New("invalid multipart request")
		}
		seen := make(map[string]bool)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", nil, errors.New("invalid or oversized multipart request")
			}
			name := part.FormName()
			if (name != "sdp" && name != "session") || seen[name] {
				return "", nil, errors.New("expected one sdp part and at most one session part")
			}
			seen[name] = true
			data, err := io.ReadAll(part)
			part.Close()
			if err != nil {
				return "", nil, errors.New("invalid or oversized multipart field")
			}
			if name == "sdp" {
				sdp = string(data)
			} else {
				if json.Unmarshal(data, &session) != nil || session == nil {
					return "", nil, errors.New("session must be a JSON object")
				}
			}
		}
	case "application/json":
		body, err := decodeRequestBody(r.Body, "")
		if err != nil {
			return "", nil, errors.New("invalid or oversized call JSON")
		}
		sdp, _ = body["sdp"].(string)
		if value, exists := body["session"]; exists {
			var ok bool
			session, ok = value.(map[string]any)
			if !ok {
				return "", nil, errors.New("session must be a JSON object")
			}
		}
		for key := range body {
			if key != "sdp" && key != "session" {
				return "", nil, errors.New("unknown call field")
			}
		}
	case "application/sdp":
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return "", nil, errors.New("invalid or oversized SDP")
		}
		sdp = string(data)
	default:
		return "", nil, errors.New("expected multipart/form-data, application/sdp, or application/json")
	}
	if !strings.HasPrefix(sdp, "v=0") || !strings.Contains(sdp, "\nm=") {
		return "", nil, errors.New("sdp must contain a WebRTC offer with a media section")
	}
	// GPT-Live has a different event protocol from GA Realtime. Accept the
	// shared GA fields, but reject unsupported fields instead of ignoring them.
	for key := range session {
		switch key {
		case "type", "model", "instructions", "audio", "delegation", "initial_items":
		default:
			return "", nil, errors.New("unsupported GPT-Live session field; see docs/capabilities.md")
		}
	}
	if value, exists := session["type"]; exists && value != "realtime" {
		return "", nil, errors.New("session.type must be realtime")
	}
	delete(session, "type")
	if value, exists := session["model"]; exists && value != liveModel {
		return "", nil, errors.New("subscription voice currently supports gpt-live-1-codex")
	}
	session["model"] = liveModel
	if value, exists := session["instructions"]; exists {
		if _, ok := value.(string); !ok {
			return "", nil, errors.New("instructions must be a string")
		}
	} else {
		session["instructions"] = ""
	}
	if _, exists := session["audio"]; !exists {
		session["audio"] = map[string]any{"output": map[string]any{"voice": "marin"}}
	}
	if _, exists := session["delegation"]; !exists {
		session["delegation"] = map[string]any{"type": "client"}
	}
	if err := validateLiveSessionOptions(session); err != nil {
		return "", nil, err
	}
	return sdp, session, nil
}

func validateLiveSessionOptions(session map[string]any) error {
	audio, ok := session["audio"].(map[string]any)
	if !ok || len(audio) != 1 {
		return errors.New("GPT-Live audio supports only output.voice")
	}
	output, ok := audio["output"].(map[string]any)
	if !ok || len(output) != 1 {
		return errors.New("GPT-Live audio supports only output.voice")
	}
	voice, ok := output["voice"].(string)
	if !ok || strings.TrimSpace(voice) == "" || len(voice) > 64 {
		return errors.New("audio.output.voice must be a non-empty voice name")
	}
	delegation, ok := session["delegation"].(map[string]any)
	if !ok || delegation["type"] != "client" {
		return errors.New("GPT-Live delegation.type must be client")
	}
	for key, value := range delegation {
		if key == "type" {
			continue
		}
		if key != "ack_filler" {
			return errors.New("unsupported GPT-Live delegation field")
		}
		if _, ok := value.(bool); !ok {
			return errors.New("delegation.ack_filler must be boolean")
		}
	}
	if items, exists := session["initial_items"]; exists {
		if _, ok := items.([]any); !ok {
			return errors.New("initial_items must be an array")
		}
	}
	return nil
}

func (p *responsesProxy) handleLiveCall(w http.ResponseWriter, r *http.Request) {
	entry := p.beginRequestLog(r)
	defer entry.finish()
	if !p.authorizedClient(r) {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusUnauthorized, message: "unauthorized"})
		return
	}
	sdp, session, err := decodeLiveCall(w, r)
	if err != nil {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusBadRequest, message: err.Error()})
		return
	}
	if entry != nil {
		entry.Entry.Model, entry.Entry.NormalizedModel = liveModel, liveModel
	}
	endpoint, err := p.codexEndpoint("realtime/calls")
	if err != nil {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusBadGateway, message: err.Error()})
		return
	}
	if !p.liveCalls.reserve() {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusServiceUnavailable, message: "voice call registry is full; retry after existing calls expire"})
		return
	}
	published := false
	defer func() {
		if !published {
			p.liveCalls.finish("", liveCall{})
		}
	}()
	release, fail := p.acquireUpstreamSlot(r.Context())
	if fail != nil {
		p.writeDispatchFailure(w, entry, fail)
		return
	}
	defer release()
	payload, _ := json.Marshal(map[string]any{"sdp": sdp, "session": session})
	resp, acct, fail := p.postSubscription(r.Context(), endpoint+"?intent=quicksilver&architecture=avas", payload, http.Header{"Openai-Alpha": {"quicksilver=v2"}})
	if fail != nil {
		p.writeDispatchFailure(w, entry, fail)
		return
	}
	entry.markUpstreamStatus(resp.StatusCode)
	answer, fail := readCapabilityResponse(resp, maxLiveBodyBytes)
	if fail != nil {
		p.writeDispatchFailure(w, entry, fail)
		return
	}
	id := liveCallID(resp.Header.Get("Location"))
	if id == "" || !strings.HasPrefix(string(answer), "v=0") {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusBadGateway, message: "upstream returned an invalid call answer"})
		return
	}
	// Publish routing metadata before the client can use the returned Location.
	p.liveCalls.finish(id, liveCall{account: acct, owner: liveOwner(r), expires: time.Now().Add(liveCallTTL)})
	published = true
	w.Header().Set("Location", "/v1/realtime/calls/"+id)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Broker-Event-Protocol", "gpt-live")
	entry.markStatus(http.StatusCreated)
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(answer)
}

func liveCallID(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	id := path.Base(u.Path)
	if len(id) > 200 {
		return ""
	}
	if !strings.HasPrefix(id, "rtc_") && len(id) != 36 {
		return ""
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ""
		}
	}
	if id == "rtc_" {
		return ""
	}
	return id
}

func (p *responsesProxy) handleLiveWebSocket(w http.ResponseWriter, r *http.Request) {
	// Do not put the call ID from the URL in persisted request history.
	logRequest := r.Clone(r.Context())
	logRequest.URL.Path = "/v1/live/{call_id}"
	entry := p.beginRequestLog(logRequest)
	defer entry.finish()
	if !p.authorizedClient(r) {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusUnauthorized, message: "unauthorized"})
		return
	}
	id := r.PathValue("call_id")
	if id == "" {
		id = r.URL.Query().Get("call_id")
	}
	if id == "" {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusBadRequest, message: "create a WebRTC call with POST /v1/realtime/calls first, then connect with call_id; standalone subscription WebSockets are unavailable"})
		return
	}
	call, ok := p.liveCalls.lookup(id, liveOwner(r))
	if !ok {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusNotFound, message: "call not found for this client or expired"})
		return
	}
	if !isWebSocketUpgrade(r) {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusUpgradeRequired, message: "WebSocket Upgrade required"})
		return
	}
	release, fail := p.acquireUpstreamSlot(r.Context())
	if fail != nil {
		p.writeDispatchFailure(w, entry, fail)
		return
	}
	defer release()
	access, err := call.account.mgr.current(r.Context())
	if err != nil {
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: http.StatusBadGateway, message: "Codex authentication failed"})
		return
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+access.AccessToken)
	headers.Set("ChatGPT-Account-Id", access.AccountID)
	headers.Set("OpenAI-Alpha", "quicksilver=v2")
	identityReq := &http.Request{Header: headers}
	p.setClientIdentity(identityReq)
	client := webSocketHTTPClient(p.client)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	upstream, response, err := websocket.Dial(r.Context(), "wss://api.openai.com/v1/live/"+url.PathEscape(id), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	if err != nil {
		status := http.StatusBadGateway
		if response != nil && response.StatusCode >= 400 {
			status = response.StatusCode
		}
		p.writeDispatchFailure(w, entry, &dispatchFailure{status: status, message: "GPT-Live sideband handshake failed"})
		return
	}
	defer upstream.CloseNow()
	downstream, err := websocket.Accept(w, r, nil)
	if err != nil {
		entry.markError(http.StatusBadRequest, "client WebSocket handshake failed")
		return
	}
	defer downstream.CloseNow()
	ctx, cancel := webSocketSessionContext(r)
	defer cancel()
	ctx, expire := context.WithDeadline(ctx, call.expires)
	defer expire()
	upstream.SetReadLimit(maxLiveBodyBytes)
	downstream.SetReadLimit(maxLiveBodyBytes)
	entry.markStatus(http.StatusSwitchingProtocols)
	if entry != nil {
		entry.Entry.Model, entry.Entry.NormalizedModel = liveModel, liveModel
	}
	finished := make(chan error, 2)
	pump := func(from, to *websocket.Conn) {
		for {
			typ, payload, err := from.Read(ctx)
			if err != nil {
				finished <- err
				return
			}
			if err = to.Write(ctx, typ, payload); err != nil {
				finished <- err
				return
			}
		}
	}
	go pump(upstream, downstream)
	go pump(downstream, upstream)
	<-finished
	cancel()
	_ = upstream.CloseNow()
	_ = downstream.CloseNow()
	<-finished
}
