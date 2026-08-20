package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDecodeRequestBodyZstd(t *testing.T) {
	encoded, err := json.Marshal(map[string]any{
		"model":  "gpt-5.6-sol",
		"input":  "hello",
		"stream": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(encoded, nil)
	encoder.Close()

	body, err := decodeRequestBody(bytes.NewReader(compressed), "zstd")
	if err != nil {
		t.Fatalf("decode Pi zstd request: %v", err)
	}
	if body["model"] != "gpt-5.6-sol" || body["input"] != "hello" {
		t.Fatalf("decoded body = %#v", body)
	}
}

func TestNormalizeFactoryModel(t *testing.T) {
	tests := []struct {
		input  string
		model  string
		effort string
	}{
		{input: "gpt-5.5(low)", model: "gpt-5.5", effort: "low"},
		{input: "gpt-5.5 (xhigh)", model: "gpt-5.5", effort: "xhigh"},
		{input: "openai-codex/gpt-5.4(high)", model: "gpt-5.4", effort: "high"},
		{input: "custom:GPT-5.5-medium", model: "gpt-5.5", effort: "medium"},
		{input: "gpt-5.6-sol(max)", model: "gpt-5.6-sol", effort: "max"},
		{input: "gpt-5.6-sol(ultra)", model: "gpt-5.6-sol", effort: "max"},
		{input: "gpt-5.6-terra-ultra", model: "gpt-5.6-terra", effort: "max"},
		{input: "gpt-5.6-luna-max", model: "gpt-5.6-luna", effort: "max"},
	}
	for _, test := range tests {
		model, effort := normalizeFactoryModel(test.input)
		if model != test.model || effort != test.effort {
			t.Fatalf("normalizeFactoryModel(%q) = (%q, %q), want (%q, %q)", test.input, model, effort, test.model, test.effort)
		}
	}
}

func TestNormalizeResponsesBodyFactoryDefaults(t *testing.T) {
	body := map[string]any{
		"model":                  "gpt-5.5(high)",
		"input":                  "hello",
		"stream":                 true,
		"max_output_tokens":      json.Number("32"),
		"max_completion_tokens":  json.Number("32"),
		"maxOutputTokens":        json.Number("32"),
		"stream_options":         map[string]any{"include_usage": true},
		"user":                   "factory-user",
		"service_tier":           "auto",
		"prompt_cache_retention": "24h",
		"prompt_cache_options":   map[string]any{"ttl": "30m"},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{
		promptCacheKey:       "factory-droid",
		promptCacheRetention: "24h",
	}, req)

	if body["model"] != "gpt-5.5" {
		t.Fatalf("model = %#v, want gpt-5.5", body["model"])
	}
	reasoning := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want effort high and summary auto", reasoning)
	}
	if body["prompt_cache_key"] != "factory-droid" {
		t.Fatalf("prompt_cache_key = %#v, want factory-droid", body["prompt_cache_key"])
	}
	if _, ok := body["prompt_cache_retention"]; ok {
		t.Fatal("prompt_cache_retention should be stripped")
	}
	if _, ok := body["prompt_cache_options"]; ok {
		t.Fatal("prompt_cache_options should be stripped")
	}
	if _, ok := body["max_output_tokens"]; ok {
		t.Fatal("max_output_tokens should be stripped")
	}
	for _, key := range []string{"max_completion_tokens", "maxOutputTokens", "stream_options", "user"} {
		if _, ok := body[key]; ok {
			t.Fatalf("%s should be stripped", key)
		}
	}
	if body["service_tier"] != "auto" {
		t.Fatalf("service_tier = %#v, want auto", body["service_tier"])
	}
	input := body["input"].([]any)
	first := input[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("input role = %#v, want user", first["role"])
	}
	if !info.Stream || !info.PromptCacheKeySet || !info.PromptCacheRetentionSet {
		t.Fatalf("unexpected info: %#v", info)
	}
	if info.ServiceTier != "auto" {
		t.Fatalf("ServiceTier = %#v, want auto", info.ServiceTier)
	}
}

func TestNormalizeResponsesBodyServiceTier(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := map[string]any{
		"model":       "gpt-5.5",
		"serviceTier": "priority",
	}
	info := normalizeResponsesBody(body, config{}, req)
	if info.ServiceTier != "priority" {
		t.Fatalf("ServiceTier = %#v, want priority", info.ServiceTier)
	}
	if body["service_tier"] != "priority" {
		t.Fatalf("service_tier = %#v, want priority", body["service_tier"])
	}
	if _, ok := body["serviceTier"]; ok {
		t.Fatal("serviceTier should be normalized away")
	}

	body = map[string]any{
		"model":        "gpt-5.5",
		"service_tier": "ultrafast",
	}
	info = normalizeResponsesBody(body, config{}, req)
	if info.ServiceTier != "ultrafast" {
		t.Fatalf("ServiceTier = %#v, want ultrafast", info.ServiceTier)
	}
	if body["service_tier"] != "ultrafast" {
		t.Fatalf("service_tier = %#v, want ultrafast", body["service_tier"])
	}

	body = map[string]any{
		"model":        "gpt-5.5",
		"service_tier": "expensive",
	}
	info = normalizeResponsesBody(body, config{}, req)
	if info.ServiceTier != "" {
		t.Fatalf("ServiceTier = %#v, want empty", info.ServiceTier)
	}
	if _, ok := body["service_tier"]; ok {
		t.Fatal("invalid service_tier should be stripped")
	}
}

func TestNormalizeResponsesBodyCapturesNativeReasoningEffort(t *testing.T) {
	body := map[string]any{
		"model": "gpt-5.3-codex",
		"input": "hello",
		"reasoning": map[string]any{
			"effort": "medium",
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{}, req)

	if body["model"] != "gpt-5.3-codex" {
		t.Fatalf("model = %#v, want gpt-5.3-codex", body["model"])
	}
	if info.ReasoningEffort != "medium" {
		t.Fatalf("ReasoningEffort = %#v, want medium", info.ReasoningEffort)
	}
}

func TestPromptCacheKeyPrefersStableConversationID(t *testing.T) {
	body := map[string]any{
		"model":           "gpt-5.5",
		"input":           "hello",
		"conversation_id": "conv-123",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	// A per-request id must NOT become the cache key, or it rotates every call
	// and defeats the backend's prompt cache.
	req.Header.Set("x-request-id", "req-unique-abc")
	info := normalizeResponsesBody(body, config{}, req)

	if body["prompt_cache_key"] != "conv-123" {
		t.Fatalf("prompt_cache_key = %#v, want conv-123", body["prompt_cache_key"])
	}
	if info.PromptCacheKey != "conv-123" || !info.PromptCacheKeySet {
		t.Fatalf("info cache key = %#v (set=%t), want conv-123", info.PromptCacheKey, info.PromptCacheKeySet)
	}
	// conversation_id is used to derive the key but must be stripped — the Codex
	// backend 400s on it.
	if _, ok := body["conversation_id"]; ok {
		t.Fatal("conversation_id should be stripped before forwarding")
	}
}

func TestPromptCacheKeyIgnoresRotatingRequestID(t *testing.T) {
	// No session/conversation id and no configured key: the only candidate is a
	// rotating per-request id. It must NOT be injected — a key that changes every
	// call scopes the backend cache to a single request and defeats reuse. We'd
	// rather inject nothing and let the automatic prefix cache work unscoped.
	for _, hdr := range []string{"x-request-id", "x-client-request-id"} {
		t.Run(hdr, func(t *testing.T) {
			body := map[string]any{
				"model": "gpt-5.5",
				"input": "hello",
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			req.Header.Set(hdr, "req-rotates-every-call")
			info := normalizeResponsesBody(body, config{}, req)

			if v, ok := body["prompt_cache_key"]; ok {
				t.Fatalf("prompt_cache_key = %#v, want it left unset for a rotating %s", v, hdr)
			}
			if info.PromptCacheKeySet || info.PromptCacheKey != "" {
				t.Fatalf("info cache key = %#v (set=%t), want unset", info.PromptCacheKey, info.PromptCacheKeySet)
			}
		})
	}
}

func TestPromptCacheKeyStableIDWinsOverConfigConstant(t *testing.T) {
	// The configured constant is shared by every client and every conversation.
	// Letting it outrank a per-conversation id would funnel unrelated transcripts
	// into one cache bucket where they evict each other, leaving only the common
	// prefix hot. A derived session id must win.
	body := map[string]any{
		"model":           "gpt-5.5",
		"input":           "hello",
		"conversation_id": "conv-123",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{promptCacheKey: "factory-droid"}, req)

	if body["prompt_cache_key"] != "conv-123" {
		t.Fatalf("prompt_cache_key = %#v, want conv-123", body["prompt_cache_key"])
	}
	if info.PromptCacheKey != "conv-123" {
		t.Fatalf("info.PromptCacheKey = %#v, want conv-123", info.PromptCacheKey)
	}
}

func TestPromptCacheKeyConfigUsedWhenNoStableID(t *testing.T) {
	// No session/conversation id anywhere: the configured constant is the
	// last-resort slot, for clients that expose no session identity at all.
	body := map[string]any{
		"model": "gpt-5.5",
		"input": "hello",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{promptCacheKey: "factory-droid"}, req)

	if body["prompt_cache_key"] != "factory-droid" {
		t.Fatalf("prompt_cache_key = %#v, want factory-droid", body["prompt_cache_key"])
	}
	if info.PromptCacheKey != "factory-droid" || !info.PromptCacheKeySet {
		t.Fatalf("info cache key = %#v (set=%t), want factory-droid", info.PromptCacheKey, info.PromptCacheKeySet)
	}
}

func TestPromptCacheKeyClientValueWinsOverEverything(t *testing.T) {
	body := map[string]any{
		"model":            "gpt-5.5",
		"input":            "hello",
		"conversation_id":  "conv-123",
		"prompt_cache_key": "client-chosen",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{promptCacheKey: "factory-droid"}, req)

	if body["prompt_cache_key"] != "client-chosen" {
		t.Fatalf("prompt_cache_key = %#v, want client-chosen", body["prompt_cache_key"])
	}
	if info.PromptCacheKey != "client-chosen" {
		t.Fatalf("info.PromptCacheKey = %#v, want client-chosen", info.PromptCacheKey)
	}
}

func TestAggregateResponsesSSE(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens_details":{"cached_tokens":17}}}}`,
		"",
	}, "\n")

	got, err := aggregateResponsesSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	output := got["output"].([]any)
	first := output[0].(map[string]any)
	if first["id"] != "msg_1" {
		t.Fatalf("output[0].id = %#v, want msg_1", first["id"])
	}
}

func TestExtractTokenUsage(t *testing.T) {
	response := map[string]any{
		"usage": map[string]any{
			"input_tokens":  json.Number("100"),
			"output_tokens": json.Number("25"),
			"total_tokens":  json.Number("125"),
			"input_tokens_details": map[string]any{
				"cached_tokens":      json.Number("75"),
				"cache_write_tokens": json.Number("10"),
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": json.Number("5"),
			},
		},
	}
	usage := extractTokenUsage(response)
	if usage.InputTokens == nil || *usage.InputTokens != 100 {
		t.Fatalf("input_tokens = %#v, want 100", usage.InputTokens)
	}
	if usage.OutputTokens == nil || *usage.OutputTokens != 25 {
		t.Fatalf("output_tokens = %#v, want 25", usage.OutputTokens)
	}
	if usage.CachedTokens == nil || *usage.CachedTokens != 75 {
		t.Fatalf("cached_tokens = %#v, want 75", usage.CachedTokens)
	}
	if usage.CacheWriteTokens == nil || *usage.CacheWriteTokens != 10 {
		t.Fatalf("cache_write_tokens = %#v, want 10", usage.CacheWriteTokens)
	}
	if usage.ReasoningTokens == nil || *usage.ReasoningTokens != 5 {
		t.Fatalf("reasoning_tokens = %#v, want 5", usage.ReasoningTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 125 {
		t.Fatalf("total_tokens = %#v, want 125", usage.TotalTokens)
	}
}

func TestSSEUsageTrackerCapturesStreamingFinalUsage(t *testing.T) {
	tracker := &sseUsageTracker{}
	tracker.feed([]byte("event: response.completed\n"))
	tracker.feed([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":50,"output_tokens":7,"total_tokens":57,"input_tokens_details":{"cached_tokens":31}}}}` + "\n\n"))

	usage := tracker.finish()
	if usage.InputTokens == nil || *usage.InputTokens != 50 {
		t.Fatalf("input_tokens = %#v, want 50", usage.InputTokens)
	}
	if usage.OutputTokens == nil || *usage.OutputTokens != 7 {
		t.Fatalf("output_tokens = %#v, want 7", usage.OutputTokens)
	}
	if usage.CachedTokens == nil || *usage.CachedTokens != 31 {
		t.Fatalf("cached_tokens = %#v, want 31", usage.CachedTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 57 {
		t.Fatalf("total_tokens = %#v, want 57", usage.TotalTokens)
	}
}

func TestExtractServiceTier(t *testing.T) {
	if got := extractServiceTier(map[string]any{"service_tier": "  Ultrafast "}); got != "ultrafast" {
		t.Fatalf("extractServiceTier = %q, want ultrafast", got)
	}
	if got := extractServiceTier(map[string]any{"service_tier": "default"}); got != "default" {
		t.Fatalf("extractServiceTier = %q, want default", got)
	}
	if got := extractServiceTier(map[string]any{}); got != "" {
		t.Fatalf("extractServiceTier = %q, want empty", got)
	}
	if got := extractServiceTier(nil); got != "" {
		t.Fatalf("extractServiceTier(nil) = %q, want empty", got)
	}
}

func TestSSEUsageTrackerCapturesAppliedServiceTier(t *testing.T) {
	tracker := &sseUsageTracker{}
	tracker.feed([]byte("event: response.completed\n"))
	tracker.feed([]byte(`data: {"type":"response.completed","response":{"service_tier":"default","usage":{"input_tokens":1}}}` + "\n\n"))
	tracker.finish()
	if tracker.serviceTier != "default" {
		t.Fatalf("tracker.serviceTier = %q, want default (upstream downgraded ultrafast)", tracker.serviceTier)
	}
}

func TestAggregateResponsesSSEKeepsServiceTier(t *testing.T) {
	stream := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"service_tier":"ultrafast","output":[]}}` + "\n\n"
	got, err := aggregateResponsesSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregateResponsesSSE error: %v", err)
	}
	if tier := extractServiceTier(got); tier != "ultrafast" {
		t.Fatalf("applied service_tier = %q, want ultrafast", tier)
	}
}

func TestRequestLogStoreBoundsAndOrder(t *testing.T) {
	store := newRequestLogStore(2)
	store.add(requestLogEntry{Status: 200, Model: "first"})
	store.add(requestLogEntry{Status: 200, Model: "second"})
	store.add(requestLogEntry{Status: 500, Model: "third"})

	snapshot := store.snapshot(10)
	if snapshot.Retained != 2 || snapshot.TotalSeen != 3 {
		t.Fatalf("snapshot counts = retained %d total %d, want 2 and 3", snapshot.Retained, snapshot.TotalSeen)
	}
	if len(snapshot.RequestLog) != 2 {
		t.Fatalf("len(requests) = %d, want 2", len(snapshot.RequestLog))
	}
	if snapshot.RequestLog[0].Model != "third" || snapshot.RequestLog[1].Model != "second" {
		t.Fatalf("request order = %#v, want newest first", snapshot.RequestLog)
	}
}

func TestNormalizeResponsesBodyDetectsCompactionTrigger(t *testing.T) {
	body := map[string]any{
		"model": "gpt-5.3-codex",
		"input": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"type": "compaction_trigger"},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{}, req)

	if !info.CompactionTrigger {
		t.Fatal("CompactionTrigger = false, want true")
	}
	// The trigger item must survive normalization untouched, or the backend
	// answers the turn instead of returning a checkpoint.
	items, ok := body["input"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("input = %#v, want the original 2 items", body["input"])
	}
	last, _ := items[1].(map[string]any)
	if last["type"] != "compaction_trigger" {
		t.Fatalf("last input item = %#v, want compaction_trigger", items[1])
	}
}

func TestNormalizeResponsesBodyNoCompactionTrigger(t *testing.T) {
	body := map[string]any{"model": "gpt-5.3-codex", "input": "hello"}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if info := normalizeResponsesBody(body, config{}, req); info.CompactionTrigger {
		t.Fatal("CompactionTrigger = true for an ordinary turn")
	}
}

func TestCodexBetaFeatures(t *testing.T) {
	tests := []struct {
		name       string
		client     []string
		compaction bool
		want       string
	}{
		{name: "absent", want: ""},
		{name: "forwards client value", client: []string{"some_feature"}, want: "some_feature"},
		{
			name:       "adds gate when client omitted it",
			client:     []string{"some_feature"},
			compaction: true,
			want:       "some_feature, " + remoteCompactionFeature,
		},
		{
			name:       "does not duplicate an existing gate",
			client:     []string{remoteCompactionFeature},
			compaction: true,
			want:       remoteCompactionFeature,
		},
		{
			name:       "adds gate with no client header",
			compaction: true,
			want:       remoteCompactionFeature,
		},
		{
			name:   "joins repeated headers",
			client: []string{"a", "b"},
			want:   "a, b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			for _, value := range tc.client {
				req.Header.Add(codexBetaFeaturesHeader, value)
			}
			if got := codexBetaFeatures(req, tc.compaction); got != tc.want {
				t.Fatalf("codexBetaFeatures() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAggregateResponsesSSEKeepsCompactionItem(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"compaction","encrypted_content":"gAAAA-opaque"}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[]}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	final, err := aggregateResponsesSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("aggregateResponsesSSE: %v", err)
	}
	output, ok := final["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output = %#v, want 1 item", final["output"])
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "compaction" || item["encrypted_content"] != "gAAAA-opaque" {
		t.Fatalf("output item = %#v, want the compaction checkpoint intact", output[0])
	}
}

func TestBuildUpstreamRequestForwardsCompactionGate(t *testing.T) {
	p := &responsesProxy{cfg: config{upstreamURL: "https://example.invalid/codex/responses", upstreamOriginator: "codex_cli_rs"}}
	body := map[string]any{
		"model": "gpt-5.3-codex",
		"input": []any{map[string]any{"type": "compaction_trigger"}},
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{}, r)

	req, err := p.buildUpstreamRequest(context.Background(), []byte("{}"), info, body, r, accessMaterial{AccessToken: "tok", AccountID: "acct"})
	if err != nil {
		t.Fatalf("buildUpstreamRequest: %v", err)
	}
	if got := req.Header.Get(codexBetaFeaturesHeader); got != remoteCompactionFeature {
		t.Fatalf("%s = %q, want %q", codexBetaFeaturesHeader, got, remoteCompactionFeature)
	}
}

func TestBuildUpstreamRequestOmitsBetaFeaturesByDefault(t *testing.T) {
	p := &responsesProxy{cfg: config{upstreamURL: "https://example.invalid/codex/responses", upstreamOriginator: "codex_cli_rs"}}
	body := map[string]any{"model": "gpt-5.3-codex", "input": "hello"}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := normalizeResponsesBody(body, config{}, r)

	req, err := p.buildUpstreamRequest(context.Background(), []byte("{}"), info, body, r, accessMaterial{AccessToken: "tok", AccountID: "acct"})
	if err != nil {
		t.Fatalf("buildUpstreamRequest: %v", err)
	}
	if got := req.Header.Get(codexBetaFeaturesHeader); got != "" {
		t.Fatalf("%s = %q, want it unset on an ordinary turn", codexBetaFeaturesHeader, got)
	}
}

func TestNormalizeInputDemotesSystemRole(t *testing.T) {
	body := map[string]any{"input": []any{
		map[string]any{"type": "message", "role": "system", "content": "prompt"},
		map[string]any{"type": "message", "role": "user", "content": "hi"},
	}}
	normalizeInput(body)
	items := body["input"].([]any)
	if got := items[0].(map[string]any)["role"]; got != "developer" {
		t.Fatalf("system role not demoted: got %v", got)
	}
	if got := items[1].(map[string]any)["role"]; got != "user" {
		t.Fatalf("user role altered: got %v", got)
	}
}

func TestNormalizeUpstreamErrorBodyWrapsDetail(t *testing.T) {
	out := normalizeUpstreamErrorBody([]byte(`{"detail":"System messages are not allowed"}`), 400)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	envelope, ok := parsed["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %s", out)
	}
	if envelope["message"] != "System messages are not allowed" {
		t.Fatalf("message = %#v", envelope["message"])
	}
	if parsed["detail"] != "System messages are not allowed" {
		t.Fatalf("original detail field dropped: %s", out)
	}
}

func TestNormalizeUpstreamErrorBodyPassesThroughEnvelope(t *testing.T) {
	original := `{"error":{"message":"already shaped","type":"invalid_request_error"}}`
	out := normalizeUpstreamErrorBody([]byte(original), 400)
	if string(out) != original {
		t.Fatalf("envelope rewritten: %s", out)
	}
}

func TestNormalizeUpstreamErrorBodyLeavesNonJSONAlone(t *testing.T) {
	out := normalizeUpstreamErrorBody([]byte("gateway timeout"), 504)
	if string(out) != "gateway timeout" {
		t.Fatalf("non-JSON body rewritten: %s", out)
	}
}

func TestInstructionsNotInjectedOverCallerPrompt(t *testing.T) {
	body := map[string]any{
		"model": "gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "system", "content": "you are ace"},
			map[string]any{"type": "message", "role": "user", "content": "hi"},
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	normalizeResponsesBody(body, config{}, req)
	if got := body["instructions"]; got == defaultInstructions {
		t.Fatalf("placeholder instructions injected over the caller's own prompt")
	}
}

func TestInstructionsInjectedWhenNoPromptAtAll(t *testing.T) {
	body := map[string]any{
		"model": "gpt-5.5",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	normalizeResponsesBody(body, config{}, req)
	if body["instructions"] != defaultInstructions {
		t.Fatalf("instructions = %#v, want the default placeholder", body["instructions"])
	}
}

func TestAuthorizedClient(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		header     string
		want       bool
	}{
		{name: "no key configured allows missing header", configured: "", header: "", want: true},
		{name: "no key configured allows any header", configured: "", header: "Bearer anything", want: true},
		{name: "exact match accepted", configured: "secret-key", header: "Bearer secret-key", want: true},
		{name: "surrounding whitespace trimmed", configured: "secret-key", header: "Bearer   secret-key", want: true},
		{name: "missing header rejected", configured: "secret-key", header: "", want: false},
		{name: "wrong token rejected", configured: "secret-key", header: "Bearer wrong", want: false},
		{name: "prefix of key rejected", configured: "secret-key", header: "Bearer secret-ke", want: false},
		{name: "key plus suffix rejected", configured: "secret-key", header: "Bearer secret-key2", want: false},
		{name: "wrong scheme rejected", configured: "secret-key", header: "Basic secret-key", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := &responsesProxy{cfg: config{apiKey: tt.configured}}
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			if got := proxy.authorizedClient(request); got != tt.want {
				t.Fatalf("authorizedClient = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestHandlersRequireBearerKey(t *testing.T) {
	proxy := &responsesProxy{
		cfg:      config{apiKey: "secret-key"},
		pool:     &accountPool{},
		requests: newRequestLogStore(10),
	}
	tests := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		path    string
		body    string
	}{
		{name: "responses", handler: proxy.handleResponses, method: http.MethodPost, path: "/v1/responses", body: `{"model":"gpt-5.5","input":"hi"}`},
		{name: "chat completions", handler: proxy.handleChatCompletions, method: http.MethodPost, path: "/v1/chat/completions", body: `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`},
		{name: "models", handler: proxy.handleModels, method: http.MethodGet, path: "/v1/models"},
		{name: "responses websocket upgrade", handler: proxy.handleResponsesWebSocket, method: http.MethodGet, path: "/v1/responses"},
		{name: "codex alias websocket upgrade", handler: proxy.handleResponsesWebSocket, method: http.MethodGet, path: "/v1/codex/responses"},
		{name: "dashboard requests api", handler: proxy.handleDashboardRequests, method: http.MethodGet, path: "/dashboard/api/requests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, header := range []string{"", "Bearer wrong-key"} {
				request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
				if header != "" {
					request.Header.Set("Authorization", header)
				}
				recorder := httptest.NewRecorder()
				tt.handler(recorder, request)
				if recorder.Code != http.StatusUnauthorized {
					t.Fatalf("header %q: status = %d, want 401", header, recorder.Code)
				}
				var parsed map[string]any
				if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
					t.Fatalf("header %q: 401 body is not JSON: %v", header, err)
				}
				errBody, _ := parsed["error"].(map[string]any)
				if errBody["message"] != "unauthorized" || errBody["type"] != "codex_auth_broker_error" {
					t.Fatalf("header %q: error body = %#v", header, parsed)
				}
			}

			// With the right key the request must clear auth: it may fail later
			// (no upstream configured here) but never with 401.
			request := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			request.Header.Set("Authorization", "Bearer secret-key")
			recorder := httptest.NewRecorder()
			tt.handler(recorder, request)
			if recorder.Code == http.StatusUnauthorized {
				t.Fatalf("valid key rejected with 401, body = %s", recorder.Body.String())
			}
		})
	}
}
