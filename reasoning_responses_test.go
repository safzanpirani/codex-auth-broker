package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestHandleResponsesForwardsSol61Reasoning(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"native", "suffix", "explicit effort overrides suffix"} {
			t.Run(fmt.Sprintf("%s/stream=%t", mode, stream), func(t *testing.T) {
				reasoningItem := map[string]any{
					"id": "rs_test", "type": "reasoning",
					"summary":           []any{map[string]any{"type": "summary_text", "text": "Checked the arithmetic."}},
					"encrypted_content": "opaque-test-reasoning",
				}
				response := map[string]any{
					"id": "resp_test", "model": "gpt-6.1-sol", "status": "completed",
					"reasoning": map[string]any{"effort": "high", "summary": "auto"},
					"output":    []any{reasoningItem},
					"usage":     map[string]any{"output_tokens_details": map[string]any{"reasoning_tokens": 42}},
				}
				var sse bytes.Buffer
				for _, event := range []map[string]any{
					{"type": "response.reasoning_summary_text.delta", "item_id": "rs_test", "output_index": 0, "summary_index": 0, "delta": "Checked the arithmetic."},
					{"type": "response.output_item.done", "output_index": 0, "item": reasoningItem},
					{"type": "response.completed", "response": response},
				} {
					encoded, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					fmt.Fprintf(&sse, "data: %s\n\n", encoded)
				}
				upstreamRequest := make(chan map[string]any, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode upstream request: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					upstreamRequest <- body
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, sse.String())
				}))
				defer upstream.Close()
				proxy := &responsesProxy{
					cfg:    config{apiKey: "client-key", upstreamURL: upstream.URL},
					pool:   newAccountPool([]string{writeWebSocketTestAuth(t, "acct_reasoning")}, time.Minute, upstream.Client()),
					client: upstream.Client(),
				}
				body := map[string]any{
					"model": "gpt-6.1-sol", "stream": stream,
					"input":     []any{reasoningItem, map[string]any{"role": "user", "content": "Check the result."}},
					"reasoning": map[string]any{"effort": "high", "summary": "auto"},
				}
				switch mode {
				case "suffix":
					body["model"] = "gpt-6.1-sol(high)"
					delete(body, "reasoning")
				case "explicit effort overrides suffix":
					body["model"] = "gpt-6.1-sol(low)"
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(encoded))
				request.Header.Set("Authorization", "Bearer client-key")
				recorder := httptest.NewRecorder()
				newServerMux(proxy).ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
				}
				select {
				case wire := <-upstreamRequest:
					if wire["model"] != "gpt-6.1-sol" || wire["stream"] != true {
						t.Fatalf("upstream model/stream = %v/%v", wire["model"], wire["stream"])
					}
					if !reflect.DeepEqual(wire["reasoning"], response["reasoning"]) {
						t.Fatalf("upstream reasoning = %#v", wire["reasoning"])
					}
					if !reflect.DeepEqual(wire["input"].([]any)[0], reasoningItem) {
						t.Fatal("reasoning continuation item changed")
					}
					if !reflect.DeepEqual(wire["include"], []any{"reasoning.encrypted_content"}) {
						t.Fatalf("upstream include = %#v", wire["include"])
					}
				default:
					t.Fatal("broker did not forward the request")
				}
				if stream {
					if recorder.Body.String() != sse.String() {
						t.Fatal("broker changed reasoning SSE events")
					}
				} else {
					var got map[string]any
					if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got["output"], response["output"]) || !reflect.DeepEqual(got["reasoning"], response["reasoning"]) {
						t.Fatal("broker changed reasoning response fields")
					}
					if usage := extractTokenUsage(got); usage.ReasoningTokens == nil || *usage.ReasoningTokens != 42 {
						t.Fatal("broker dropped reasoning token usage")
					}
				}
			})
		}
	}
}
