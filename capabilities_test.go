package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testOffer = "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n"

func capabilityProxy(t *testing.T, upstream *httptest.Server) *responsesProxy {
	t.Helper()
	return &responsesProxy{cfg: config{upstreamURL: upstream.URL + "/backend-api/codex/responses", apiKey: "local-key"}, client: upstream.Client(), pool: newAccountPool([]string{writeWebSocketTestAuth(t, "voice-account")}, 0, upstream.Client()), requests: newRequestLogStore(20)}
}

func callRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("sdp", testOffer)
	_ = mw.WriteField("session", `{"type":"realtime","model":"gpt-live-1-codex","instructions":"private instructions"}`)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer local-key")
	return r
}

func TestLiveCallAndSidebandPinAccountAndPreserveNativeEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-Id") != "voice-account" || r.Header.Get("OpenAI-Alpha") != "quicksilver=v2" || r.Header.Get("Authorization") == "Bearer local-key" {
			t.Error("wrong upstream identity")
		}
		if r.Method == http.MethodPost {
			calls.Add(1)
			if r.URL.Path != "/backend-api/codex/realtime/calls" || r.URL.Query().Get("architecture") != "avas" || r.URL.Query().Get("intent") != "quicksilver" {
				t.Errorf("wrong call route: %s", r.URL)
			}
			var body struct {
				SDP     string         `json:"sdp"`
				Session map[string]any `json:"session"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.SDP != testOffer || body.Session["model"] != liveModel || body.Session["type"] != nil {
				t.Error("wrong backend call body")
			}
			w.Header().Set("Location", "https://api.openai.com/v1/live/rtc_test")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, testOffer)
			return
		}
		if r.URL.Path != "/v1/live/rtc_test" {
			t.Errorf("wrong sideband URL: %s", r.URL)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Write(ctx, typ, data)
		_, _, _ = conn.Read(ctx)
	}))
	defer upstream.Close()
	p := capabilityProxy(t, upstream)
	// Rewrite only the fixed API sideband host for the test; exercise a real
	// WebSocket handshake and both broker pumps over loopback connections.
	original := p.client.Transport
	p.client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.openai.com" {
			r = r.Clone(r.Context())
			u, _ := url.Parse(upstream.URL)
			r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
		}
		return original.RoundTrip(r)
	})
	mux := newServerMux(p)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, callRequest(t, "/v1/realtime/calls"))
	if w.Code != 201 || w.Header().Get("Location") != "/v1/realtime/calls/rtc_test" || w.Body.String() != testOffer {
		t.Fatalf("call response: %d %s", w.Code, w.Body.String())
	}
	if _, ok := p.liveCalls.lookup("rtc_test", liveOwner(callRequest(t, "/"))); !ok {
		t.Fatal("call not published before returning")
	}
	// A second enabled key must not be able to join the first client's call.
	p.cfg.apiKey = "other-key"
	r := httptest.NewRequest("GET", "/v1/realtime?call_id=rtc_test", nil)
	r.Header.Set("Authorization", "Bearer other-key")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("cross-client join status = %d", w.Code)
	}
	p.cfg.apiKey = "local-key"
	broker := httptest.NewServer(mux)
	defer broker.Close()
	conn, _, err := websocket.Dial(ctx, broker.URL+"/v1/live/rtc_test", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer local-key"}}})
	if err != nil {
		t.Fatal(err)
	}
	event := `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"private test content"}]}}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
		t.Fatal(err)
	}
	_, received, err := conn.Read(ctx)
	if err != nil || string(received) != event {
		t.Fatalf("native event changed or failed: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	if calls.Load() != 1 {
		t.Fatalf("created %d calls", calls.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		finished := false
		for _, entry := range p.requests.snapshot(20).RequestLog {
			if entry.Status == 101 {
				finished = true
			}
		}
		if finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sideband handler did not finish its metadata log")
		}
		time.Sleep(time.Millisecond)
	}
	entries, _ := json.Marshal(p.requests.snapshot(20))
	if bytes.Contains(entries, []byte("private")) || bytes.Contains(entries, []byte("v=0")) || bytes.Contains(entries, []byte("rtc_test")) {
		t.Fatal("payload retained in logs")
	}
}

func TestCompactPreservesOpaqueOutputAndDoesNotReplay(t *testing.T) {
	var attempts atomic.Int32
	result := `{"id":"cmp_test","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-ciphertext"}],"usage":{"input_tokens":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.URL.Path != "/backend-api/codex/responses/compact" {
			t.Error("incorrect route")
		}
		body, _ := decodeRequestBody(r.Body, "")
		if body["model"] != "gpt-6-astra" || body["stream"] != nil {
			t.Error("unexpected compaction normalization")
		}
		if r.Header.Get(codexRoutingHintHeader) != "model=gpt-6-astra" || r.Header.Get("OpenAI-Beta") != "responses=experimental" {
			t.Error("missing Codex compaction routing headers")
		}
		w.Header().Set("x-codex-turn-state", "test-state")
		_, _ = io.WriteString(w, result)
	}))
	defer upstream.Close()
	p := capabilityProxy(t, upstream)
	r := httptest.NewRequest("POST", "/v1/responses/compact", strings.NewReader(`{"model":"gpt-6-astra(high)","input":[{"role":"user","content":"private message"}]}`))
	r.Header.Set("Authorization", "Bearer local-key")
	w := httptest.NewRecorder()
	newServerMux(p).ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != result || w.Header().Get("x-codex-turn-state") != "test-state" {
		t.Fatalf("compaction response: %d %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 1 {
		t.Fatal("compaction replayed")
	}
	entries := p.requests.snapshot(10).RequestLog
	if len(entries) != 1 || entries[0].InputTokens == nil || *entries[0].InputTokens != 1 {
		t.Fatal("compaction usage was not recorded")
	}
}

func TestLiveValidationAndRegistryBounds(t *testing.T) {
	for _, session := range []string{`{"model":"gpt-realtime-2.1"}`, `{"tools":[]}`, `{"type":"transcription"}`, `null`} {
		body := `{"sdp":` + strconvQuote(testOffer) + `,"session":` + session + `}`
		r := httptest.NewRequest("POST", "/v1/live", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if _, _, err := decodeLiveCall(httptest.NewRecorder(), r); err == nil {
			t.Errorf("accepted session %s", session)
		}
	}
	r := httptest.NewRequest("POST", "/v1/live", strings.NewReader(testOffer+strings.Repeat("a", maxLiveBodyBytes)))
	r.Header.Set("Content-Type", "application/sdp")
	if _, _, err := decodeLiveCall(httptest.NewRecorder(), r); err == nil {
		t.Fatal("oversized SDP accepted")
	}
	var store liveCallStore
	for range maxLiveCalls {
		if !store.reserve() {
			t.Fatal("early registry limit")
		}
	}
	if store.reserve() {
		t.Fatal("unbounded pending calls")
	}
	store.finish("rtc_expired", liveCall{expires: time.Now().Add(-time.Second)})
	if !store.reserve() {
		t.Fatal("expired calls did not free capacity")
	}
}

func strconvQuote(value string) string { data, _ := json.Marshal(value); return string(data) }

func TestCapabilityRoutesRequireAuthenticationAndUnsupportedIsJSON(t *testing.T) {
	p := &responsesProxy{cfg: config{apiKey: "local-key"}}
	mux := newServerMux(p)
	for _, route := range []string{"GET /v1/capabilities", "POST /v1/embeddings", "POST /v1/audio/speech", "POST /v1/live", "POST /v1/responses/compact", "GET /v1/realtime"} {
		method, path, _ := strings.Cut(route, " ")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != 401 {
			t.Errorf("%s unauthenticated status = %d", route, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/v1/embeddings", nil)
	r.Header.Set("Authorization", "Bearer local-key")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 501 || !strings.Contains(w.Body.String(), `"code":"unsupported_endpoint"`) {
		t.Fatalf("unsupported response %d %s", w.Code, w.Body.String())
	}
}

func TestSubscriptionCreationRotatesOnlyOnExplicitRejection(t *testing.T) {
	for _, mode := range []string{"rate_limit", "transport", "redirect", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			var attempts atomic.Int32
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				attempt := attempts.Add(1)
				if mode == "transport" {
					return nil, errors.New("ambiguous network failure")
				}
				if r.URL.Host != "upstream.test" {
					t.Error("followed upstream redirect")
				}
				header := make(http.Header)
				status, body := http.StatusCreated, testOffer
				header.Set("Location", "/v1/live/rtc_retry")
				if attempt == 1 {
					switch mode {
					case "rate_limit":
						status = 429
						header.Set("Retry-After", "60")
						body = `{"error":{"code":"rate_limit_exceeded","message":"private diagnostic"}}`
					case "redirect":
						status = 307
						header.Set("Location", "https://different.test/capture")
					case "forbidden":
						status = 403
						body = `{"error":{"code":"forbidden","message":"private diagnostic and SDP"}}`
					}
				} else if mode == "rate_limit" && r.Header.Get("ChatGPT-Account-Id") != "second" {
					t.Error("did not rotate to second account")
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			p := &responsesProxy{cfg: config{upstreamURL: "https://upstream.test/responses", apiKey: "local-key"}, client: client, pool: newAccountPool([]string{writeWebSocketTestAuth(t, "first"), writeWebSocketTestAuth(t, "second")}, 0, client), requests: newRequestLogStore(10)}
			w := httptest.NewRecorder()
			newServerMux(p).ServeHTTP(w, callRequest(t, "/v1/live"))
			if mode == "rate_limit" {
				if attempts.Load() != 2 || w.Code != 201 {
					t.Fatalf("rate limit: attempts=%d status=%d", attempts.Load(), w.Code)
				}
				call, ok := p.liveCalls.lookup("rtc_retry", liveOwner(callRequest(t, "/")))
				if !ok || call.account != p.pool.accounts[1] {
					t.Fatal("call not pinned to successful account")
				}
			} else {
				if attempts.Load() != 1 || w.Code < 400 {
					t.Fatalf("unexpected replay or success: attempts=%d status=%d", attempts.Load(), w.Code)
				}
				if len(p.liveCalls.calls) != 0 || p.liveCalls.pending != 0 {
					t.Fatal("failed call retained registry capacity")
				}
			}
			entries, _ := json.Marshal(p.requests.snapshot(10))
			if bytes.Contains(entries, []byte("private")) || strings.Contains(w.Body.String(), "private") {
				t.Fatal("upstream diagnostics exposed")
			}
		})
	}
}
