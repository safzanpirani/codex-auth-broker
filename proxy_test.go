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
		"model":                 "gpt-5.5(high)",
		"input":                 "hello",
		"stream":                true,
		"max_output_tokens":     json.Number("32"),
		"max_completion_tokens": json.Number("32"),
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
	if body["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt_cache_retention = %#v, want 24h", body["prompt_cache_retention"])
	}
	if _, ok := body["max_output_tokens"]; ok {
		t.Fatal("max_output_tokens should be stripped")
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
