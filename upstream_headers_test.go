package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDiagnosticUpstreamHeaders(t *testing.T) {
	header := http.Header{
		"X-Request-Id":                 {"req-123"},
		"Openai-Processing-Ms":         {"12.5"},
		"X-Codex-Primary-Used-Percent": {"42"},
	}
	for _, name := range []string{
		"Set-Cookie", "Cookie", "Authorization", "Proxy-Authorization", "Www-Authenticate",
		"Proxy-Authenticate", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade",
		"Transfer-Encoding", "Content-Length", "Content-Encoding", "X-Api-Key", "X-Secret",
		"X-Access-Token", "X-Refresh-Token", "X-Id-Token", "Session-Id", "X-Session",
		"X-Codex-Session-State", "X-Codex-Primary-Secret", "X-Prompt", "X-Completion",
		"X-Upstream-X-Request-Id", "Location", "Content-Type", "Cache-Control",
	} {
		header.Set(name, "private-sentinel")
	}
	want := map[string]string{"x-request-id": "req-123", "openai-processing-ms": "12.5", "x-codex-primary-used-percent": "42"}
	if got := diagnosticUpstreamHeaders(header); !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered headers = %#v, want %#v", got, want)
	}
	for _, value := range []string{
		"Bearer short-secret", "bearer short-secret", "sk-short-secret", strings.Repeat("a", 90),
		strings.Repeat("a", 20) + "." + strings.Repeat("b", 20) + "." + strings.Repeat("c", 20),
		`{"token":"` + strings.Repeat("a", 90) + `"}`, "user prompt or completion text",
		"session=short-secret", "req-\x00invalid", "req-\xffinvalid",
		strings.Repeat("界", 80) + " Bearer secret-after-cutoff",
	} {
		got := diagnosticUpstreamHeaders(http.Header{"X-Request-Id": {value}})["x-request-id"]
		if got != "[redacted]" {
			t.Errorf("unsafe value was not fully redacted: %q", got)
		}
	}
	for _, value := range []string{"sk-short-secret", "NaN", "+Inf", "some text", "1.2.3"} {
		if got := diagnosticUpstreamHeaders(http.Header{"Openai-Processing-Ms": {value}})["openai-processing-ms"]; got != "[redacted]" {
			t.Errorf("invalid metric retained: %q", got)
		}
	}
	value := strings.Repeat("界", 90)
	got := diagnosticUpstreamHeaders(http.Header{"X-Request-Id": {value}})["x-request-id"]
	if !utf8.ValidString(got) || len(got) != 255 || got != strings.Repeat("界", 85) {
		t.Fatalf("UTF-8 truncation = %q (%d bytes)", got, len(got))
	}
}

func TestDiagnosticUpstreamHeadersBoundsAndHopByHop(t *testing.T) {
	for _, header := range []http.Header{
		{"connection": {"keep-alive, X-Request-ID"}, "X-Request-Id": {"req-hidden"}},
		{"X-Request-Id": {"one", "two"}},
		{"X-Request-Id": {strings.Repeat("界", 1400)}},
		{"X-Request-Id": {""}},
	} {
		if got := diagnosticUpstreamHeaders(header); len(got) != 0 {
			t.Fatalf("invalid or hop-by-hop headers retained: %#v", got)
		}
	}
	header := http.Header{"X-Request-Id": {"one"}, "x-request-id": {"two"}}
	if got := diagnosticUpstreamHeaders(header)["x-request-id"]; got != "[redacted]" {
		t.Fatalf("case duplicate = %q", got)
	}
	header = make(http.Header)
	for i := 0; i < 10000; i++ {
		header.Set(fmt.Sprintf("X-Unknown-%d", i), "private")
	}
	for name, numeric := range diagnosticHeaderNames {
		value := strings.Repeat("界", 90)
		if numeric {
			value = "42"
		}
		header.Set(name, value)
	}
	got := diagnosticUpstreamHeaders(header)
	if len(got) != len(diagnosticHeaderNames) {
		t.Fatalf("retained %d headers, want %d", len(got), len(diagnosticHeaderNames))
	}
	for name, value := range got {
		if len(value) > 256 || !utf8.ValidString(value) {
			t.Fatalf("invalid bounded value for %s", name)
		}
	}
}

func TestUpstreamHeaderMirroringPreservesBrokerHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-Upstream-X-Request-Id", "broker-id")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "30")
	captureUpstreamHeaders(w, nil, http.Header{
		"X-Request-Id": {"upstream-id"}, "Content-Type": {"text/event-stream"},
		"Retry-After": {"999"}, "X-Upstream-X-Request-Id": {"injected"},
	})
	for key, want := range map[string]string{
		"X-Upstream-X-Request-Id": "broker-id", "Content-Type": "application/json", "Retry-After": "30",
	} {
		if got := w.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

const headerTestSSE = "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_test","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

func headerTestProxy(t *testing.T, upstream *httptest.Server, accounts ...string) *responsesProxy {
	t.Helper()
	if len(accounts) == 0 {
		accounts = []string{"acct_one"}
	}
	var authFiles []string
	for _, account := range accounts {
		authFiles = append(authFiles, writeWebSocketTestAuth(t, account))
	}
	store := newRequestLogStore(2)
	store.persist = reviewLog(t, 8192)
	return &responsesProxy{
		cfg:      config{upstreamURL: upstream.URL + "/responses", alphaSearchURL: upstream.URL + "/alpha/search"},
		pool:     newAccountPool(authFiles, time.Minute, upstream.Client()),
		requests: store, client: upstream.Client(),
	}
}

func TestUpstreamHeadersHTTPAndPersistence(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/alpha/search", "/v1/responses/compact"} {
		for _, stream := range []bool{false, true} {
			if stream && path != "/v1/responses" && path != "/v1/chat/completions" {
				continue
			}
			for _, status := range []int{200, 400, 502} {
				t.Run(fmt.Sprintf("%s/stream=%t/status=%d", path, stream, status), func(t *testing.T) {
					secret := "upstream-private-sentinel"
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("X-Request-Id", "req-final")
						w.Header().Set("Openai-Processing-Ms", "12.5")
						w.Header().Set("X-Codex-Primary-Used-Percent", "42")
						w.Header().Set("Openai-Request-Id", "Bearer "+secret)
						for _, name := range []string{"Set-Cookie", "X-Api-Key", "X-Auth-Token", "X-Codex-Turn-State", "X-Prompt", "X-Completion"} {
							w.Header().Set(name, secret)
						}
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(status)
						switch {
						case status == 502: // Empty upstream error body.
						case status == 400:
							fmt.Fprint(w, `{"error":{"message":"invalid request"}}`)
						case path == "/v1/alpha/search":
							fmt.Fprint(w, `{"results":[]}`)
						case path == "/v1/responses/compact":
							fmt.Fprint(w, `{"output":[]}`)
						default:
							fmt.Fprint(w, headerTestSSE)
						}
					}))
					defer upstream.Close()
					proxy := headerTestProxy(t, upstream)
					body := fmt.Sprintf(`{"model":"gpt-6-astra","input":[],"stream":%t,"messages":[{"role":"user","content":"private prompt sentinel"}]}`, stream)
					r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					w := httptest.NewRecorder()
					switch path {
					case "/v1/responses":
						proxy.handleResponses(w, r)
					case "/v1/chat/completions":
						proxy.handleChatCompletions(w, r)
					case "/v1/alpha/search":
						proxy.handleAlphaSearch(w, r)
					case "/v1/responses/compact":
						proxy.handleCompact(w, r)
					}
					if w.Code != status {
						t.Fatalf("status %d, want %d; body %s", w.Code, status, w.Body.String())
					}
					want := map[string]string{"x-request-id": "req-final", "openai-processing-ms": "12.5", "x-codex-primary-used-percent": "42", "openai-request-id": "[redacted]"}
					mirrored := make(map[string]string)
					for name, values := range w.Result().Header {
						if strings.HasPrefix(strings.ToLower(name), "x-upstream-") {
							mirrored[strings.TrimPrefix(strings.ToLower(name), "x-upstream-")] = strings.Join(values, ",")
						}
					}
					if !reflect.DeepEqual(mirrored, want) {
						t.Fatalf("client diagnostics = %#v, want %#v", mirrored, want)
					}
					entry := reviewRequestEntry(t, proxy)
					if !reflect.DeepEqual(entry.UpstreamHeaders, want) {
						t.Fatalf("logged diagnostics = %#v", entry.UpstreamHeaders)
					}
					raw, err := os.ReadFile(proxy.requests.persist.path)
					if err != nil {
						t.Fatal(err)
					}
					clientHeaders := w.Result().Header.Clone()
					if path == "/v1/responses/compact" {
						// Compaction intentionally relays turn state to the client.
						clientHeaders.Del("X-Codex-Turn-State")
					}
					for _, private := range []string{secret, "private prompt sentinel"} {
						if strings.Contains(string(raw), private) || strings.Contains(fmt.Sprint(clientHeaders), private) {
							t.Fatal("private upstream value escaped filtering")
						}
					}
					var persisted requestLogEntry
					if err := json.Unmarshal(raw, &persisted); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(persisted.UpstreamHeaders, want) {
						t.Fatalf("persisted diagnostics = %#v", persisted.UpstreamHeaders)
					}
					if status == 200 && (path == "/v1/responses" || path == "/v1/chat/completions") {
						wantType := "application/json"
						if stream {
							wantType = "text/event-stream"
						}
						if !strings.HasPrefix(w.Result().Header.Get("Content-Type"), wantType) {
							t.Fatalf("content type = %q, want %s", w.Result().Header.Get("Content-Type"), wantType)
						}
					}
				})
			}
		}
	}
}

func TestUpstreamHeadersFailoverRecordsFinalResponse(t *testing.T) {
	for _, finalStatus := range []int{200, 400, 429} {
		t.Run(fmt.Sprint(finalStatus), func(t *testing.T) {
			attempts := make(chan string, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				account := r.Header.Get("ChatGPT-Account-Id")
				attempts <- account
				w.Header().Set("X-Request-Id", "req-"+account)
				status := finalStatus
				if account == "acct_one" {
					status = 429
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(status)
				if status == 200 {
					fmt.Fprint(w, headerTestSSE)
				} else {
					fmt.Fprint(w, `{"error":{"message":"unavailable"}}`)
				}
			}))
			defer upstream.Close()
			proxy := headerTestProxy(t, upstream, "acct_one", "acct_two")
			w := httptest.NewRecorder()
			proxy.handleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[],"stream":false}`)))
			if w.Code != finalStatus || len(attempts) != 2 {
				t.Fatalf("status %d, attempts %d", w.Code, len(attempts))
			}
			if first, second := <-attempts, <-attempts; first != "acct_one" || second != "acct_two" {
				t.Fatalf("attempts %s then %s", first, second)
			}
			entry := reviewRequestEntry(t, proxy)
			if entry.UpstreamStatus != finalStatus || entry.UpstreamHeaders["x-request-id"] != "req-acct_two" || w.Result().Header.Get("X-Upstream-X-Request-Id") != "req-acct_two" {
				t.Fatalf("wrong final diagnostics: %#v", entry.UpstreamHeaders)
			}
		})
	}
}

func TestUpstreamHeadersPersistenceBoundariesAndRotation(t *testing.T) {
	store := newRequestLogStore(2)
	store.persist = reviewLog(t, 2048)
	rawHeaders := map[string]string{"x-request-id": "req-safe", "set-cookie": "private-cookie", "x-prompt": "private-prompt", "openai-request-id": "sk-private-key"}
	want := map[string]string{"x-request-id": "req-safe", "openai-request-id": "[redacted]"}
	for i := int64(1); i <= 30; i++ {
		entry := reviewEntry(i)
		entry.UpstreamHeaders = rawHeaders
		store.add(entry)
	}
	rawHeaders["x-request-id"] = "mutated"
	snapshot := store.snapshot(10)
	if snapshot.Retained != 2 || snapshot.TotalSeen != 30 || !reflect.DeepEqual(snapshot.RequestLog[0].UpstreamHeaders, want) {
		t.Fatalf("history bounds or map ownership failed: %#v", snapshot)
	}
	stat, err := os.Stat(store.persist.path)
	if err != nil || stat.Size() > 2048 {
		t.Fatalf("rotation stat = %v, err %v", stat, err)
	}
	raw, err := os.ReadFile(store.persist.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-") {
		t.Fatal("raw secret persisted")
	}
	entries, maxID, err := loadPersistedEntries(store.persist.path, 100)
	if err != nil || len(entries) == 0 || maxID != 30 {
		t.Fatalf("load entries=%d maxID=%d err=%v", len(entries), maxID, err)
	}
	for _, entry := range entries {
		if !reflect.DeepEqual(entry.UpstreamHeaders, want) {
			t.Fatalf("rotation changed diagnostics: %#v", entry.UpstreamHeaders)
		}
	}
	legacy := reviewEntry(31)
	legacy.UpstreamHeaders = map[string]string{"set-cookie": "private-cookie", "x-request-id": "req-safe"}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := scanRequestLogReader(strings.NewReader(string(encoded)+"\n"), func(entry requestLogEntry) {
		if len(entry.UpstreamHeaders) != 1 || entry.UpstreamHeaders["x-request-id"] != "req-safe" {
			t.Fatal("legacy headers bypassed sanitization")
		}
	}); err != nil {
		t.Fatal(err)
	}
	restored := newRequestLogStore(1)
	restored.restore([]requestLogEntry{legacy}, 31)
	if len(restored.snapshot(1).RequestLog[0].UpstreamHeaders) != 1 {
		t.Fatal("restore retained raw headers")
	}
}

func TestModelsClientVersionDefault(t *testing.T) {
	if defaultModelsClientVersion != "0.158.0" {
		t.Fatalf("default version = %q", defaultModelsClientVersion)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("client_version"); got != "0.158.0" {
			t.Errorf("client_version = %q", got)
		}
		fmt.Fprint(w, `{"models":[{"slug":"gpt-6-astra","visibility":"list","supported_in_api":true}]}`)
	}))
	defer upstream.Close()
	proxy := headerTestProxy(t, upstream)
	proxy.cfg.modelsURL = upstream.URL + "/models"
	proxy.cfg.modelsClientVersion = defaultModelsClientVersion
	ids, err := proxy.fetchUpstreamModelIDs(context.Background())
	if err != nil || !reflect.DeepEqual(ids, []string{"gpt-6-astra"}) {
		t.Fatalf("models = %v, err %v", ids, err)
	}
}
