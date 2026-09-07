package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func chatTestSSE(t *testing.T, events ...map[string]any) string {
	t.Helper()
	var stream strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString("data: ")
		stream.Write(encoded)
		stream.WriteString("\n\n")
	}
	return stream.String()
}

func chatTestStreamPayload(t *testing.T, stream, field string) string {
	t.Helper()
	var result strings.Builder
	for _, event := range decodeChatSSE(t, stream) {
		choices, _ := event["choices"].([]any)
		for _, raw := range choices {
			delta, _ := raw.(map[string]any)["delta"].(map[string]any)
			if field == "arguments" {
				calls, _ := delta["tool_calls"].([]any)
				for _, call := range calls {
					function, _ := call.(map[string]any)["function"].(map[string]any)
					result.WriteString(chatPayloadString(function, "arguments"))
				}
			} else {
				result.WriteString(chatPayloadString(delta, field))
			}
		}
	}
	return result.String()
}

func TestChatPayloadWhitespacePreserved(t *testing.T) {
	const text = "  indented code\n\treturn value\n"
	parts := []any{map[string]any{"type": "text", "text": text}}
	for _, role := range []string{"user", "assistant", "developer", "tool"} {
		t.Run(role, func(t *testing.T) {
			message := map[string]any{"role": role, "content": parts, "tool_call_id": "call_1"}
			input, err := translateChatMessages([]any{message})
			if err != nil {
				t.Fatal(err)
			}
			item := input[0].(map[string]any)
			got := chatPayloadString(item, "output")
			if role != "tool" {
				got = chatPayloadString(item["content"].([]any)[0].(map[string]any), "text")
			}
			if got != text {
				t.Fatalf("content = %q, want %q", got, text)
			}
		})
	}
	completion, err := chatCompletionFromResponse(map[string]any{
		"output": []any{map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "output_text", "text": text},
			map[string]any{"type": "refusal", "refusal": " no\n"},
		}}},
	}, "gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	message := completion["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != text || message["refusal"] != " no\n" {
		t.Fatalf("response whitespace changed: %#v", message)
	}
}

func TestChatStreamPayloadWhitespacePreserved(t *testing.T) {
	for _, tt := range []struct {
		name, eventType, field string
		deltas                 []string
	}{
		{"text", "response.output_text.delta", "content", []string{"Hello", " ", "world\n", "\t", "next"}},
		{"refusal", "response.refusal.delta", "refusal", []string{"No", " ", "thanks\n"}},
		{"tool JSON", "response.function_call_arguments.delta", "arguments", []string{"{\"text\":\"a", " ", "b\"}\n"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := []map[string]any{}
			if tt.field == "arguments" {
				events = append(events, map[string]any{"type": "response.output_item.added", "output_index": 0,
					"item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "lookup", "arguments": ""}})
			}
			for _, delta := range tt.deltas {
				events = append(events, map[string]any{"type": tt.eventType, "item_id": "fc_1", "output_index": 0, "content_index": 0, "delta": delta})
			}
			events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed"}})
			w := httptest.NewRecorder()
			if _, _, err := copyChatCompletionsStream(w, strings.NewReader(chatTestSSE(t, events...)), "gpt-5.5", false); err != nil {
				t.Fatal(err)
			}
			if got, want := chatTestStreamPayload(t, w.Body.String(), tt.field), strings.Join(tt.deltas, ""); got != want {
				t.Fatalf("payload = %q, want %q", got, want)
			}
		})
	}
}

func TestChatStreamFallbackDeduplicatesEachPart(t *testing.T) {
	first := map[string]any{"id": "msg_1", "type": "message", "content": []any{
		map[string]any{"type": "output_text", "text": "first "},
		map[string]any{"type": "output_text", "text": "second "},
		map[string]any{"type": "refusal", "refusal": "no "},
		map[string]any{"type": "refusal", "refusal": "thanks\n"},
	}}
	second := map[string]any{"id": "msg_2", "type": "message", "content": []any{
		map[string]any{"type": "output_text", "text": "third\n"},
	}}
	for _, mode := range []string{"terminal only", "done items", "mixed deltas and fallback"} {
		t.Run(mode, func(t *testing.T) {
			var events []map[string]any
			if mode == "mixed deltas and fallback" {
				events = append(events,
					map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": "first "},
					map[string]any{"type": "response.refusal.delta", "output_index": 0, "content_index": 2, "delta": "no "})
			}
			if mode != "terminal only" {
				events = append(events,
					map[string]any{"type": "response.output_item.done", "output_index": 0, "item": first},
					map[string]any{"type": "response.output_item.done", "output_index": 1, "item": second})
			}
			events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{
				"status": "completed", "output": []any{first, second}}})
			w := httptest.NewRecorder()
			if _, _, err := copyChatCompletionsStream(w, strings.NewReader(chatTestSSE(t, events...)), "gpt-5.5", false); err != nil {
				t.Fatal(err)
			}
			if got := chatTestStreamPayload(t, w.Body.String(), "content"); got != "first second third\n" {
				t.Fatalf("content = %q", got)
			}
			if got := chatTestStreamPayload(t, w.Body.String(), "refusal"); got != "no thanks\n" {
				t.Fatalf("refusal = %q", got)
			}
		})
	}
}

type chatTestReadError struct{}

func (chatTestReadError) Read([]byte) (int, error) {
	return 0, errors.New("private read-error sentinel")
}

func TestChatStreamFailuresReachClientWithoutLeakingDetailsToLogs(t *testing.T) {
	for _, tt := range []struct {
		name   string
		reader io.Reader
	}{
		{"empty EOF", strings.NewReader("")},
		{"premature DONE", strings.NewReader("data: [DONE]\n\n")},
		{"malformed JSON", strings.NewReader("data: {private malformed-event sentinel\n\n")},
		{"read failure", chatTestReadError{}},
		{"upstream error", strings.NewReader("data: {\"type\":\"error\",\"message\":\"private upstream-message sentinel\"}\n\n")},
		{"missing response", strings.NewReader("data: {\"type\":\"response.completed\"}\n\n")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			_, _, err := copyChatCompletionsStream(w, tt.reader, "gpt-5.5", false)
			if err == nil {
				t.Fatal("expected stream error")
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "sentinel") {
				t.Fatal("returned log error contains upstream details")
			}
			events := decodeChatSSE(t, w.Body.String())
			if len(events) != 1 || events[0]["error"] == nil || strings.Count(w.Body.String(), "data: [DONE]") != 1 {
				t.Fatalf("expected one error event and one DONE, got %s", w.Body.String())
			}
		})
	}
}

func TestChatStreamStopsAtTerminalEvent(t *testing.T) {
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	w := httptest.NewRecorder()
	_, _, err := copyChatCompletionsStream(w, io.MultiReader(strings.NewReader(terminal), chatTestReadError{}), "gpt-5.5", false)
	if err != nil {
		t.Fatalf("read past terminal response: %v", err)
	}
	if strings.Count(w.Body.String(), "data: [DONE]") != 1 {
		t.Fatal("terminal response did not end stream exactly once")
	}
}

func TestChatStreamFallbackKeepsChunkIdentity(t *testing.T) {
	stream := chatTestSSE(t,
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{
			"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}}},
		map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_late", "created_at": 1, "model": "upstream-model", "status": "completed"}})
	w := httptest.NewRecorder()
	if _, _, err := copyChatCompletionsStream(w, strings.NewReader(stream), "gpt-5.5", false); err != nil {
		t.Fatal(err)
	}
	events := decodeChatSSE(t, w.Body.String())
	if len(events) != 3 {
		t.Fatalf("expected role, content, and finish chunks; got %d", len(events))
	}
	for _, field := range []string{"id", "created", "model"} {
		for _, event := range events[1:] {
			if event[field] != events[0][field] {
				t.Fatalf("chunk %s changed from %v to %v", field, events[0][field], event[field])
			}
		}
	}
}

type chatTestWriteError struct {
	http.ResponseWriter
	writes int
}

func (w *chatTestWriteError) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("private write-error sentinel")
}

func TestChatStreamStopsAfterWriteFailure(t *testing.T) {
	w := &chatTestWriteError{ResponseWriter: httptest.NewRecorder()}
	_, _, err := copyChatCompletionsStream(w, strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"), "gpt-5.5", false)
	if err == nil || strings.Contains(err.Error(), "private") || w.writes != 1 {
		t.Fatalf("write failure handling: error=%v writes=%d", err, w.writes)
	}
}

func TestChatUpstreamErrorEnvelope(t *testing.T) {
	p := &responsesProxy{}
	w := httptest.NewRecorder()
	p.relayChatCompletionsUpstreamError(w, nil, &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"detail":"invalid parameter"}`)),
	})
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusBadRequest || body["error"].(map[string]any)["message"] != "invalid parameter" {
		t.Fatalf("unexpected error envelope: %s", w.Body.String())
	}
}

func TestChatHandlerDoesNotRecordUpstreamErrorText(t *testing.T) {
	const sentinel = "private upstream prompt sentinel"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"message\":\""+sentinel+"\"}\n\n")
	}))
	defer upstream.Close()
	authFile := writeWebSocketTestAuth(t, "acct_chat_error")
	var processLog bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&processLog)
	defer log.SetOutput(previousLog)
	for _, stream := range []bool{false, true} {
		store := newRequestLogStore(10)
		p := &responsesProxy{
			cfg: config{upstreamURL: upstream.URL}, requests: store, client: upstream.Client(),
			pool: newAccountPool([]string{authFile}, time.Minute, upstream.Client()),
		}
		body, err := json.Marshal(map[string]any{"model": "gpt-5.5", "stream": stream,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}}})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		p.handleChatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
		snapshot := store.snapshot(10)
		if len(snapshot.RequestLog) != 1 || snapshot.RequestLog[0].Error == "" {
			t.Fatal("failed stream missing from request history")
		}
		if strings.Contains(snapshot.RequestLog[0].Error, sentinel) || strings.Contains(processLog.String(), sentinel) {
			t.Fatal("upstream prompt text reached history or process logs")
		}
	}
}
