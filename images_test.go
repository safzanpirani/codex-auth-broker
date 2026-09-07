package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func imageSSE(result, revised string, usage map[string]any) string {
	item := map[string]any{"type": "image_generation_call", "status": "completed", "result": result}
	if revised != "" {
		item["revised_prompt"] = revised
	}
	response := map[string]any{"status": "completed", "output": []any{item}}
	if usage != nil {
		response["usage"] = usage
	}
	event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	return "data: " + string(event) + "\n\n"
}

func testImageProxy(t *testing.T, upstream http.Handler) (*responsesProxy, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	payload, _ := json.Marshal(map[string]any{
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"iat":                         time.Now().Add(-time.Minute).Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-test"},
	})
	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	authFile := t.TempDir() + "/auth.json"
	if err := os.WriteFile(authFile, []byte(fmt.Sprintf(`{"tokens":{"access_token":%q,"account_id":"acct-test"}}`, token)), 0o600); err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	return &responsesProxy{
		cfg:      config{apiKey: "client-key", upstreamURL: server.URL},
		pool:     newAccountPool([]string{authFile}, time.Minute, client),
		client:   client,
		limiter:  newConcurrencyLimiter(2, time.Second),
		requests: newRequestLogStore(10),
	}, server
}

func performImageRequest(proxy *responsesProxy, body string, authorized bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorized {
		req.Header.Set("Authorization", "Bearer client-key")
	}
	recorder := httptest.NewRecorder()
	proxy.handleImageGenerations(recorder, req)
	return recorder
}

func TestImageGenerationsRouteAndAuth(t *testing.T) {
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, imageSSE("aGVsbG8=", "", nil))
	}))
	mux := newServerMux(proxy)

	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/images/generations", nil))
	if get.Code == http.StatusOK {
		t.Fatalf("GET unexpectedly succeeded: body = %s", get.Body.String())
	}

	post := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"cat"}`))
	post.Header.Set("Authorization", "Bearer client-key")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, post)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestImageGenerationsAuthAndValidation(t *testing.T) {
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called")
	}))
	if got := performImageRequest(proxy, `{"prompt":"cat"}`, false); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, body = %s", got.Code, got.Body.String())
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{"missing prompt", `{}`},
		{"blank prompt", `{"prompt":"  "}`},
		{"bad model", `{"prompt":"cat","model":7}`},
		{"low count", `{"prompt":"cat","n":0}`},
		{"high count", `{"prompt":"cat","n":5}`},
		{"fractional count", `{"prompt":"cat","n":1.5}`},
		{"bad size", `{"prompt":"cat","size":"640x480"}`},
		{"bad quality", `{"prompt":"cat","quality":"ultra"}`},
		{"bad format", `{"prompt":"cat","output_format":"gif"}`},
		{"bad background", `{"prompt":"cat","background":"none"}`},
		{"bad moderation", `{"prompt":"cat","moderation":"high"}`},
		{"bad compression", `{"prompt":"cat","output_compression":101}`},
		{"bad user", `{"prompt":"cat","user":7}`},
		{"transparent jpeg", `{"prompt":"cat","background":"transparent","output_format":"jpeg"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := performImageRequest(proxy, test.body, true)
			if got.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", got.Code, got.Body.String())
			}
		})
	}
}

func TestImageGenerationsFlexibleSizeAndPNGCompression(t *testing.T) {
	var tool map[string]any
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		tool = body["tools"].([]any)[0].(map[string]any)
		_, _ = io.WriteString(w, imageSSE("aGVsbG8=", "", nil))
	}))

	got := performImageRequest(proxy, `{"prompt":"cat","size":"1280x768","output_format":"png","output_compression":70}`, true)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", got.Code, got.Body.String())
	}
	if tool["size"] != "1280x768" {
		t.Fatalf("size = %#v", tool["size"])
	}
	if _, exists := tool["output_compression"]; exists {
		t.Fatalf("PNG output_compression was forwarded: %#v", tool)
	}
}

func TestImageGenerationsDefaultsAndOptionForwarding(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("upstream headers = %#v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, imageSSE("aGVsbG8=", "a refined prompt", map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}))
	}))

	got := performImageRequest(proxy, `{"prompt":"private prompt","n":2,"size":"1536x1024","quality":"high","output_format":"webp","background":"transparent","moderation":"low","output_compression":77,"user":"must-not-forward"}`, true)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", got.Code, got.Body.String())
	}
	var response struct {
		Data  []generatedImage `json:"data"`
		Usage map[string]int64 `json:"usage"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 2 || response.Data[0].RevisedPrompt != "a refined prompt" {
		t.Fatalf("response = %#v", response)
	}
	if response.Usage["total_tokens"] != 10 || len(bodies) != 2 {
		t.Fatalf("usage = %#v, upstream calls = %d", response.Usage, len(bodies))
	}
	for _, body := range bodies {
		if body["model"] != imageResponsesModel || body["instructions"] != imageGenerationInstructions || body["stream"] != true || body["store"] != false {
			t.Fatalf("upstream envelope = %#v", body)
		}
		if _, exists := body["user"]; exists {
			t.Fatal("user was forwarded at top level")
		}
		input := body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		if input["text"] != "private prompt" {
			t.Fatalf("input = %#v", input)
		}
		tool := body["tools"].([]any)[0].(map[string]any)
		for key, want := range map[string]any{"type": "image_generation", "model": defaultImageModel, "size": "1536x1024", "quality": "high", "output_format": "webp", "background": "transparent", "moderation": "low", "output_compression": float64(77)} {
			if tool[key] != want {
				t.Fatalf("tool[%s] = %#v, want %#v; tool=%#v", key, tool[key], want, tool)
			}
		}
		if _, exists := tool["user"]; exists {
			t.Fatal("user was forwarded in tool")
		}
	}
}

func TestImageBackingModelOverride(t *testing.T) {
	for _, stream := range []bool{false, true} {
		proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body["model"] != "gpt-5.4" || r.Header.Get(codexRoutingHintHeader) != "model=gpt-5.4" {
				t.Error("backing model and routing hint must use the configured model")
			}
			tool := body["tools"].([]any)[0].(map[string]any)
			if tool["model"] != defaultImageModel {
				t.Error("image model changed with backing model")
			}
			_, _ = io.WriteString(w, imageSSE("aGVsbG8=", "", nil))
		}))
		proxy.cfg.imageResponsesModel = "gpt-5.4"
		payload, _ := json.Marshal(map[string]any{"prompt": "synthetic test", "stream": stream})
		got := performImageRequest(proxy, string(payload), true)
		if got.Code != http.StatusOK {
			t.Fatalf("stream=%v status=%d", stream, got.Code)
		}
	}
}

func TestImageGenerationsRequestLogIsMetadataOnly(t *testing.T) {
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, imageSSE("aGVsbG8=", "secret revision", nil))
	}))
	prompt := "prompt-that-must-not-be-logged"
	got := performImageRequest(proxy, `{"model":"gpt-image-2","prompt":"`+prompt+`","user":"private-user"}`, true)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", got.Code, got.Body.String())
	}
	snapshot := proxy.requests.snapshot(10)
	if len(snapshot.RequestLog) != 1 {
		t.Fatalf("request log = %#v", snapshot)
	}
	entry := snapshot.RequestLog[0]
	if entry.Path != "/v1/images/generations" || entry.Model != defaultImageModel || entry.InputCount != 1 || entry.ToolCount != 1 {
		t.Fatalf("metadata = %#v", entry)
	}
	encoded, _ := json.Marshal(snapshot)
	for _, forbidden := range []string{prompt, "private-user", "aGVsbG8=", "secret revision"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("request log leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestImageGenerationsUsesUniqueUpstreamRequestIDs(t *testing.T) {
	var ids []string
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("x-client-request-id"))
		_, _ = io.WriteString(w, imageSSE("aGVsbG8=", "", nil))
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"prompt":"cat","n":2}`))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("x-request-id", "client-turn")
	recorder := httptest.NewRecorder()
	proxy.handleImageGenerations(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	want := []string{"client-turn-image-1", "client-turn-image-2"}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("upstream request ids = %#v, want %#v", ids, want)
	}
}

func TestParseImageGenerationSSETerminalFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"failed", `data: {"type":"response.failed","response":{"error":{"message":"no"}}}` + "\n\n"},
		{"incomplete", `data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"timeout"}}}` + "\n\n"},
		{"interrupted", `data: {"type":"response.output_item.done","item":{"type":"image_generation_call","result":"aGVsbG8="}}` + "\n\n"},
		{"malformed", "data: {not-json}\n\n"},
		{"missing image", `data: {"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n"},
		{"bad completed status", `data: {"type":"response.completed","response":{"status":"in_progress","output":[]}}` + "\n\n"},
		{"malformed base64", `data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"%%%"}]}}` + "\n\n"},
		{"noncanonical base64", `data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"Zh=="}]}}` + "\n\n"},
		{"unfinished image", `data: {"type":"response.completed","response":{"output":[{"type":"image_generation_call","status":"in_progress","result":"aGVsbG8="}]}}` + "\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseImageGenerationSSE(strings.NewReader(test.body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParseImageGenerationSSESurfacesProviderFailure(t *testing.T) {
	body := `data: {"type":"response.failed","response":{"error":{"message":"quota was exhausted"}}}` + "\n\n"
	if _, err := parseImageGenerationSSE(strings.NewReader(body)); err == nil || err.Error() != "quota was exhausted" {
		t.Fatalf("error = %v", err)
	}
	incomplete := `data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"timeout"}}}` + "\n\n"
	if _, err := parseImageGenerationSSE(strings.NewReader(incomplete)); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("incomplete error = %v", err)
	}
}

func TestParseImageGenerationSSECompletedIsAuthoritativeWithDoneFallback(t *testing.T) {
	done := `data: {"type":"response.output_item.done","item":{"type":"image_generation_call","status":"completed","result":"aGVsbG8=","revised_prompt":"done"}}` + "\n\n"
	completedWithoutOutput := `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
	result, err := parseImageGenerationSSE(strings.NewReader(done + completedWithoutOutput))
	if err != nil || result.Image.RevisedPrompt != "done" {
		t.Fatalf("fallback result = %#v, err = %v", result, err)
	}
	completedBadOutput := `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"image_generation_call","result":"%%%"}]}}` + "\n\n"
	if _, err := parseImageGenerationSSE(strings.NewReader(done + completedBadOutput)); err == nil {
		t.Fatal("done fallback must not override malformed completed output")
	}
}

func TestImageGenerationBounds(t *testing.T) {
	if _, err := decodeImageItem(map[string]any{"type": "image_generation_call", "result": strings.Repeat("A", maxImageBase64Chars+1)}); err == nil {
		t.Fatal("expected oversized payload error")
	}
	var stream strings.Builder
	for i := 0; i <= maxImageSSEEvents; i++ {
		stream.WriteString(`data: {"type":"response.created"}` + "\n\n")
	}
	if _, err := parseImageGenerationSSE(strings.NewReader(stream.String())); err == nil {
		t.Fatal("expected event limit error")
	}
}

func TestImageGenerationsRotatesAccountAfter429(t *testing.T) {
	var calls int
	proxy, server := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"error":"rate limit"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, imageSSE("aGVsbG8=", "", nil))
	}))
	payload, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-2"}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	authFile := t.TempDir() + "/auth.json"
	if err := os.WriteFile(authFile, []byte(fmt.Sprintf(`{"tokens":{"access_token":%q,"account_id":"acct-2"}}`, token)), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy.pool = newAccountPool([]string{proxy.pool.accounts[0].mgr.authFile, authFile}, time.Minute, server.Client())
	got := performImageRequest(proxy, `{"prompt":"cat"}`, true)
	if got.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status = %d, calls = %d, body = %s", got.Code, calls, got.Body.String())
	}
}

func TestNormalizeResponsesBodyPreservesNativeImageGenerationTool(t *testing.T) {
	tool := map[string]any{"type": "image_generation", "model": "gpt-image-2", "size": "1024x1024"}
	body := map[string]any{"model": "gpt-5.6-sol", "input": "draw", "tools": []any{tool}, "stream": true}
	normalizeResponsesBody(body, config{}, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	got := body["tools"].([]any)[0].(map[string]any)
	if got["type"] != tool["type"] || got["model"] != tool["model"] || got["size"] != tool["size"] {
		t.Fatalf("native image tool changed: %#v", got)
	}
}
