package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTranslateChatCompletionsBody(t *testing.T) {
	body, info, err := translateChatCompletionsBody(map[string]any{
		"model": "gpt-5.5(high)",
		"messages": []any{
			map[string]any{"role": "developer", "content": "Be precise."},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "Inspect this."},
					map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url":    "data:image/png;base64,abc",
							"detail": "low",
						},
					},
				},
			},
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "lookup",
							"arguments": `{"id":1}`,
						},
					},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": `{"ok":true}`},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "lookup",
					"description": "Look up a record.",
					"parameters":  map[string]any{"type": "object"},
					"strict":      true,
				},
			},
		},
		"tool_choice": map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "lookup"},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "answer",
				"schema": map[string]any{"type": "object"},
				"strict": true,
			},
		},
		"prompt_cache_key": "conversation-123",
		"stream":           true,
		"stream_options":   map[string]any{"include_usage": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Stream || !info.IncludeUsage {
		t.Fatalf("chat request info = %#v, want stream with usage", info)
	}
	if _, exists := body["instructions"]; exists {
		t.Fatalf("instructions = %#v, want instruction messages kept in input order", body["instructions"])
	}
	if body["prompt_cache_key"] != "conversation-123" {
		t.Fatalf("prompt_cache_key = %#v", body["prompt_cache_key"])
	}

	input := body["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input length = %d, want 4", len(input))
	}
	developer := input[0].(map[string]any)
	if developer["role"] != "developer" {
		t.Fatalf("translated developer message = %#v", developer)
	}
	developerContent := developer["content"].([]any)
	if developerContent[0].(map[string]any)["text"] != "Be precise." {
		t.Fatalf("translated developer content = %#v", developerContent)
	}
	user := input[1].(map[string]any)
	userContent := user["content"].([]any)
	image := userContent[1].(map[string]any)
	if image["type"] != "input_image" || image["detail"] != "low" {
		t.Fatalf("translated image = %#v", image)
	}
	call := input[2].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" {
		t.Fatalf("translated function call = %#v", call)
	}
	output := input[3].(map[string]any)
	if output["type"] != "function_call_output" || output["call_id"] != "call_1" {
		t.Fatalf("translated function output = %#v", output)
	}

	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "lookup" || tool["strict"] != true {
		t.Fatalf("translated tool = %#v", tool)
	}
	choice := body["tool_choice"].(map[string]any)
	if choice["name"] != "lookup" {
		t.Fatalf("translated tool choice = %#v", choice)
	}
	text := body["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "answer" {
		t.Fatalf("translated response format = %#v", format)
	}
}

func TestChatCompletionsUsesStablePromptCacheKey(t *testing.T) {
	chatBody := map[string]any{
		"model": "gpt-5.5",
		"messages": []any{
			map[string]any{"role": "user", "content": "same long prefix"},
		},
		"prompt_cache_key":       "project-session",
		"prompt_cache_retention": "24h",
	}
	body, _, err := translateChatCompletionsBody(chatBody)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := normalizeResponsesBody(body, config{promptCacheKey: "factory-droid"}, req)

	if body["prompt_cache_key"] != "project-session" {
		t.Fatalf("prompt_cache_key = %#v, want project-session", body["prompt_cache_key"])
	}
	if !info.PromptCacheKeySet || info.PromptCacheKey != "project-session" {
		t.Fatalf("cache info = %#v", info)
	}
	if !info.PromptCacheRetentionSet || info.PromptCacheRetention != "24h" {
		t.Fatalf("retention intent = %#v", info)
	}
	if _, exists := body["prompt_cache_retention"]; exists {
		t.Fatal("prompt_cache_retention should be stripped before the Codex upstream")
	}
}

func TestChatCompletionFromResponse(t *testing.T) {
	response := map[string]any{
		"id":           "resp_abc",
		"created_at":   json.Number("1700000000"),
		"model":        "gpt-5.5",
		"status":       "completed",
		"service_tier": "priority",
		"output": []any{
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": "Checking."},
				},
			},
			map[string]any{
				"id":        "fc_1",
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "lookup",
				"arguments": `{"id":1}`,
			},
		},
		"usage": map[string]any{
			"input_tokens":  json.Number("1200"),
			"output_tokens": json.Number("50"),
			"total_tokens":  json.Number("1250"),
			"input_tokens_details": map[string]any{
				"cached_tokens":      json.Number("1024"),
				"cache_write_tokens": json.Number("64"),
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": json.Number("20"),
			},
		},
	}

	completion, err := chatCompletionFromResponse(response, "")
	if err != nil {
		t.Fatal(err)
	}
	if completion["id"] != "chatcmpl-abc" || completion["object"] != "chat.completion" {
		t.Fatalf("completion identity = %#v", completion)
	}
	choices := completion["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" {
		t.Fatalf("tool call = %#v", call)
	}
	usage := completion["usage"].(map[string]any)
	promptDetails := usage["prompt_tokens_details"].(map[string]any)
	if promptDetails["cached_tokens"] != int64(1024) || promptDetails["cache_write_tokens"] != int64(64) {
		t.Fatalf("prompt token details = %#v", promptDetails)
	}
	completionDetails := usage["completion_tokens_details"].(map[string]any)
	if completionDetails["reasoning_tokens"] != int64(20) {
		t.Fatalf("completion token details = %#v", completionDetails)
	}
}

func TestCopyChatCompletionsStream(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_stream","created_at":1700000000,"model":"gpt-5.5","status":"in_progress"}}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Checking."}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"id\":"}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"1}"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_stream","created_at":1700000000,"model":"gpt-5.5","status":"completed","service_tier":"default","usage":{"input_tokens":1200,"output_tokens":20,"total_tokens":1220,"input_tokens_details":{"cached_tokens":1024,"cache_write_tokens":0}}}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	usage, tier, err := copyChatCompletionsStream(recorder, strings.NewReader(stream), "gpt-5.5", true)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CachedTokens == nil || *usage.CachedTokens != 1024 {
		t.Fatalf("cached tokens = %#v", usage.CachedTokens)
	}
	if tier != "default" {
		t.Fatalf("service tier = %q", tier)
	}

	events := decodeChatSSE(t, recorder.Body.String())
	if len(events) != 7 {
		t.Fatalf("event count = %d, want 7; stream:\n%s", len(events), recorder.Body.String())
	}
	firstChoice := events[0]["choices"].([]any)[0].(map[string]any)
	firstDelta := firstChoice["delta"].(map[string]any)
	if firstDelta["role"] != "assistant" {
		t.Fatalf("first delta = %#v", firstDelta)
	}
	contentChoice := events[1]["choices"].([]any)[0].(map[string]any)
	if contentChoice["delta"].(map[string]any)["content"] != "Checking." {
		t.Fatalf("content chunk = %#v", events[1])
	}
	toolChoice := events[2]["choices"].([]any)[0].(map[string]any)
	toolDelta := toolChoice["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if toolDelta["id"] != "call_1" || toolDelta["index"] != float64(0) && toolDelta["index"] != 0 {
		t.Fatalf("tool delta = %#v", toolDelta)
	}
	finishChoice := events[5]["choices"].([]any)[0].(map[string]any)
	if finishChoice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish chunk = %#v", events[5])
	}
	if choices := events[6]["choices"].([]any); len(choices) != 0 {
		t.Fatalf("usage choices = %#v, want empty", choices)
	}
	promptDetails := events[6]["usage"].(map[string]any)["prompt_tokens_details"].(map[string]any)
	if promptDetails["cached_tokens"] != float64(1024) {
		t.Fatalf("stream usage = %#v", events[6]["usage"])
	}
	if !strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream missing [DONE]:\n%s", recorder.Body.String())
	}
}

func TestCopyChatCompletionsStreamUsesDoneItemWhenNoDeltasArrive(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback"}]}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_fallback","model":"gpt-5.5","status":"completed","output":[]}}`,
		"",
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, _, err := copyChatCompletionsStream(recorder, strings.NewReader(stream), "gpt-5.5", false); err != nil {
		t.Fatal(err)
	}
	events := decodeChatSSE(t, recorder.Body.String())
	if len(events) != 3 {
		t.Fatalf("event count = %d, want role/content/finish; stream:\n%s", len(events), recorder.Body.String())
	}
	content := events[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"]
	if content != "fallback" {
		t.Fatalf("fallback content = %#v", content)
	}
}

func TestTranslateChatCompletionsRejectsMultipleChoices(t *testing.T) {
	_, _, err := translateChatCompletionsBody(map[string]any{
		"model": "gpt-5.5",
		"n":     json.Number("2"),
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	if err == nil || chatErrorParam(err) != "n" {
		t.Fatalf("error = %#v, want n validation error", err)
	}
}

func TestHandleChatCompletionsPreservesInstructionOrder(t *testing.T) {
	upstreamRequests := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstreamRequests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: response.output_item.done",
			`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"ORDER_OK"}]}}`,
			"",
			"event: response.completed",
			`data: {"type":"response.completed","response":{"id":"resp_order","created_at":1700000000,"model":"gpt-5.5","status":"completed","output":[]}}`,
			"",
		}, "\n")))
	}))
	defer upstream.Close()

	authFile := writeWebSocketTestAuth(t, "acct_chat_order")
	proxy := &responsesProxy{
		cfg: config{
			apiKey:              "client-key",
			upstreamURL:         upstream.URL,
			upstreamOriginator:  "codex_cli_rs",
			modelsClientVersion: "2.0.0",
			promptCacheKey:      "factory-droid",
		},
		pool:     newAccountPool([]string{authFile}, time.Minute, upstream.Client()),
		requests: newRequestLogStore(0),
		client:   upstream.Client(),
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[
			{"role":"system","content":"Policy A"},
			{"role":"user","content":"Question 1"},
			{"role":"assistant","content":"Answer 1"},
			{"role":"developer","content":"Policy B"},
			{"role":"user","content":"Question 2"}
		],
		"stream":false
	}`))
	request.Header.Set("Authorization", "Bearer client-key")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	proxy.handleChatCompletions(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	upstreamBody := <-upstreamRequests
	input, ok := upstreamBody["input"].([]any)
	if !ok {
		t.Fatalf("upstream input = %#v, want array", upstreamBody["input"])
	}
	if len(input) != 5 {
		t.Fatalf("upstream input length = %d, want 5; input = %#v", len(input), input)
	}

	wantRoles := []string{"developer", "user", "assistant", "developer", "user"}
	wantText := []string{"Policy A", "Question 1", "Answer 1", "Policy B", "Question 2"}
	for index := range input {
		message, ok := input[index].(map[string]any)
		if !ok {
			t.Fatalf("upstream input[%d] = %#v, want message", index, input[index])
		}
		if message["role"] != wantRoles[index] {
			t.Fatalf("upstream input[%d] role = %#v, want %q", index, message["role"], wantRoles[index])
		}
		content, ok := message["content"].([]any)
		if !ok || len(content) != 1 {
			t.Fatalf("upstream input[%d] content = %#v, want one part", index, message["content"])
		}
		part, ok := content[0].(map[string]any)
		if !ok || part["text"] != wantText[index] {
			t.Fatalf("upstream input[%d] content = %#v, want %q", index, content[0], wantText[index])
		}
	}
}

func TestHandleChatCompletionsEndToEnd(t *testing.T) {
	upstreamRequests := make(chan struct {
		body        map[string]any
		routingHint string
	}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstreamRequests <- struct {
			body        map[string]any
			routingHint string
		}{body: body, routingHint: r.Header.Get(codexRoutingHintHeader)}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: response.output_item.done",
			`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"CHAT_OK"}]}}`,
			"",
			"event: response.completed",
			`data: {"type":"response.completed","response":{"id":"resp_e2e","created_at":1700000000,"model":"gpt-5.5","status":"completed","service_tier":"default","output":[],"usage":{"input_tokens":1200,"output_tokens":2,"total_tokens":1202,"input_tokens_details":{"cached_tokens":1024}}}}`,
			"",
		}, "\n")))
	}))
	defer upstream.Close()

	authFile := writeWebSocketTestAuth(t, "acct_chat")
	store := newRequestLogStore(10)
	proxy := &responsesProxy{
		cfg: config{
			apiKey:              "client-key",
			upstreamURL:         upstream.URL,
			upstreamOriginator:  "codex_cli_rs",
			modelsClientVersion: "2.0.0",
			promptCacheKey:      "factory-droid",
		},
		pool:     newAccountPool([]string{authFile}, time.Minute, upstream.Client()),
		requests: store,
		client:   upstream.Client(),
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"Reply exactly: CHAT_OK"}],
		"serviceTier":"fast",
		"prompt_cache_key":"chat-session",
		"stream":false
	}`))
	request.Header.Set("Authorization", "Bearer client-key")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	proxy.handleChatCompletions(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var completion map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &completion); err != nil {
		t.Fatal(err)
	}
	choices := completion["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "CHAT_OK" {
		t.Fatalf("completion = %#v", completion)
	}
	if completion["service_tier"] != "default" {
		t.Fatalf("downstream service_tier = %#v, want upstream-applied default", completion["service_tier"])
	}

	upstreamRequest := <-upstreamRequests
	upstreamBody := upstreamRequest.body
	if upstreamBody["stream"] != true {
		t.Fatalf("upstream stream = %#v, want true", upstreamBody["stream"])
	}
	if upstreamBody["prompt_cache_key"] != "chat-session" {
		t.Fatalf("upstream prompt_cache_key = %#v", upstreamBody["prompt_cache_key"])
	}
	if upstreamBody["service_tier"] != "priority" {
		t.Fatalf("upstream service_tier = %#v, want priority", upstreamBody["service_tier"])
	}
	if upstreamRequest.routingHint != "model=gpt-5.5;tier=priority" {
		t.Fatalf("%s = %q, want model=gpt-5.5;tier=priority", codexRoutingHintHeader, upstreamRequest.routingHint)
	}
	if _, exists := upstreamBody["messages"]; exists {
		t.Fatal("Chat messages must be translated before upstream dispatch")
	}

	snapshot := store.snapshot(10)
	if len(snapshot.RequestLog) != 1 {
		t.Fatalf("request log length = %d", len(snapshot.RequestLog))
	}
	entry := snapshot.RequestLog[0]
	if entry.Path != "/v1/chat/completions" || entry.CachedTokens == nil || *entry.CachedTokens != 1024 {
		t.Fatalf("request log entry = %#v", entry)
	}
	if entry.ServiceTier != "priority" || entry.AppliedServiceTier != "default" {
		t.Fatalf("service tiers = requested %q applied %q, want priority/default", entry.ServiceTier, entry.AppliedServiceTier)
	}

	// Disabling request history is a supported configuration and must not make
	// either successful endpoint dereference a nil pending log entry.
	proxy.requests = newRequestLogStore(0)
	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"Reply exactly: CHAT_OK"}],
		"stream":false
	}`))
	request.Header.Set("Authorization", "Bearer client-key")
	recorder = httptest.NewRecorder()
	proxy.handleChatCompletions(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status with request log disabled = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	<-upstreamRequests
}

func decodeChatSSE(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, block := range strings.Split(stream, "\n\n") {
		line := strings.TrimSpace(block)
		if line == "" || line == "data: [DONE]" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("invalid SSE block %q", line)
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode SSE block: %v", err)
		}
		events = append(events, event)
	}
	return events
}
