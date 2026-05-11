package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	if _, ok := body["max_output_tokens"]; ok {
		t.Fatal("max_output_tokens should be stripped")
	}
	for _, key := range []string{"max_completion_tokens", "maxOutputTokens", "stream_options", "user", "service_tier"} {
		if _, ok := body[key]; ok {
			t.Fatalf("%s should be stripped", key)
		}
	}
	input := body["input"].([]any)
	first := input[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("input role = %#v, want user", first["role"])
	}
	if !info.Stream || !info.PromptCacheKeySet || !info.PromptCacheRetentionSet {
		t.Fatalf("unexpected info: %#v", info)
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
				"cached_tokens": json.Number("75"),
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
