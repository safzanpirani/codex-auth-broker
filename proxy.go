package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

const maxRequestBodyBytes = 128 * 1024 * 1024

type responsesProxy struct {
	cfg    config
	auth   *authManager
	client *http.Client
}

type requestInfo struct {
	Model                   string
	NormalizedModel         string
	ReasoningEffort         string
	Stream                  bool
	PromptCacheKeySet       bool
	PromptCacheRetentionSet bool
}

func (p *responsesProxy) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": version,
		"commit":  commit,
		"mode":    "responses-proxy",
	})
}

func (p *responsesProxy) handleModels(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	now := time.Now().Unix()
	models := make([]map[string]any, 0, len(p.cfg.models))
	for _, model := range p.cfg.models {
		models = append(models, map[string]any{
			"id":       model,
			"object":   "model",
			"created":  now,
			"owned_by": "codex-auth-broker",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (p *responsesProxy) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	body, err := decodeRequestBody(r.Body)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, err.Error())
		return
	}
	info := normalizeResponsesBody(body, p.cfg, r)
	log.Printf("responses request model=%s normalized=%s stream=%t prompt_cache_key=%t prompt_cache_retention=%t",
		info.Model, info.NormalizedModel, info.Stream, info.PromptCacheKeySet, info.PromptCacheRetentionSet)

	upstreamBody := body
	if !info.Stream {
		upstreamBody = copyMap(body)
		upstreamBody["stream"] = true
	}
	encoded, err := json.Marshal(upstreamBody)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	access, err := p.auth.current(r.Context())
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "Codex auth failed: "+err.Error())
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.cfg.upstreamURL, bytes.NewReader(encoded))
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "build upstream request failed")
		return
	}
	req.Header.Set("Authorization", "Bearer "+access.AccessToken)
	req.Header.Set("chatgpt-account-id", access.AccountID)
	req.Header.Set("originator", "codex-auth-broker")
	req.Header.Set("User-Agent", "codex-auth-broker/"+valueOr(version, "dev"))
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("Content-Type", "application/json")
	if info.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	if requestID := requestID(r, body); requestID != "" {
		req.Header.Set("session_id", requestID)
		req.Header.Set("x-client-request-id", requestID)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		copyResponseHeaders(w, resp.Header, info.Stream)
		w.WriteHeader(resp.StatusCode)
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_, _ = w.Write([]byte(redactTokenLikeText(string(body))))
		return
	}
	if !info.Stream {
		finalResponse, err := aggregateResponsesSSE(resp.Body)
		if err != nil {
			writeProxyError(w, http.StatusBadGateway, "aggregate upstream stream failed: "+err.Error())
			return
		}
		logUsage(finalResponse)
		writeJSON(w, http.StatusOK, finalResponse)
		return
	}
	copyResponseHeaders(w, resp.Header, true)
	w.WriteHeader(resp.StatusCode)
	copyStreamingResponse(w, resp.Body)
}

func (p *responsesProxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedClient(r) {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeProxyError(w, http.StatusNotImplemented, "Factory Droid uses /v1/responses; /v1/chat/completions is not implemented yet")
}

func (p *responsesProxy) authorizedClient(r *http.Request) bool {
	want := strings.TrimSpace(p.cfg.apiKey)
	if want == "" {
		return true
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix)) == want
}

func decodeRequestBody(r io.Reader) (map[string]any, error) {
	decoder := json.NewDecoder(io.LimitReader(r, maxRequestBodyBytes))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("invalid JSON request body: %w", err)
	}
	if body == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	return body, nil
}

func normalizeResponsesBody(body map[string]any, cfg config, r *http.Request) requestInfo {
	info := requestInfo{}
	if model := stringField(body, "model"); model != "" {
		info.Model = model
		normalized, effort := normalizeFactoryModel(model)
		body["model"] = normalized
		info.NormalizedModel = normalized
		info.ReasoningEffort = effort
		if effort != "" {
			ensureReasoningEffort(body, effort)
		}
	}
	if _, ok := body["store"]; !ok {
		body["store"] = false
	}
	removeUnsupportedParams(body)
	normalizeInput(body)
	if stringField(body, "instructions") == "" {
		body["instructions"] = defaultInstructions
	}
	if _, ok := body["include"]; !ok {
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	if _, ok := body["text"]; !ok {
		body["text"] = map[string]any{"verbosity": "low"}
	}
	if _, ok := body["tools"]; ok {
		if _, hasToolChoice := body["tool_choice"]; !hasToolChoice {
			body["tool_choice"] = "auto"
		}
		if _, hasParallel := body["parallel_tool_calls"]; !hasParallel {
			body["parallel_tool_calls"] = true
		}
	}
	if current := stringField(body, "prompt_cache_key"); current != "" {
		info.PromptCacheKeySet = true
	} else if key := strings.TrimSpace(cfg.promptCacheKey); key != "" {
		body["prompt_cache_key"] = key
		info.PromptCacheKeySet = true
	}
	if current := stringField(body, "prompt_cache_retention"); current != "" {
		info.PromptCacheRetentionSet = true
	} else if retention := strings.TrimSpace(cfg.promptCacheRetention); retention != "" {
		body["prompt_cache_retention"] = retention
		info.PromptCacheRetentionSet = true
	}
	if stream, ok := body["stream"].(bool); ok {
		info.Stream = stream
	}
	if requestID := requestID(r, body); requestID != "" && !info.PromptCacheKeySet && strings.TrimSpace(cfg.promptCacheKey) == "" {
		body["prompt_cache_key"] = requestID
		info.PromptCacheKeySet = true
	}
	return info
}

func normalizeFactoryModel(raw string) (string, string) {
	model := strings.TrimSpace(raw)
	model = strings.TrimPrefix(model, "openai-codex/")
	if strings.HasPrefix(strings.ToLower(model), "custom:") {
		model = strings.TrimSpace(model[len("custom:"):])
	}
	effort := ""
	if open := strings.LastIndex(model, "("); open >= 0 && strings.HasSuffix(model, ")") {
		candidate := strings.ToLower(strings.TrimSpace(model[open+1 : len(model)-1]))
		if normalized := normalizeReasoningEffort(candidate); normalized != "" || candidate == "off" || candidate == "none" {
			effort = normalized
			model = strings.TrimSpace(model[:open])
		}
	}
	lower := strings.ToLower(model)
	for _, suffix := range []string{"-xhigh", "-high", "-medium", "-low"} {
		if strings.HasSuffix(lower, suffix) {
			effort = strings.TrimPrefix(suffix, "-")
			model = strings.TrimSpace(model[:len(model)-len(suffix)])
			break
		}
	}
	if strings.HasPrefix(model, "GPT-") {
		model = "gpt-" + strings.TrimPrefix(model, "GPT-")
	}
	return model, effort
}

func normalizeReasoningEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "minimal":
		return "low"
	case "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(value))
	case "max":
		return "xhigh"
	default:
		return ""
	}
}

func ensureReasoningEffort(body map[string]any, effort string) {
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
		body["reasoning"] = reasoning
	}
	if stringField(reasoning, "effort") == "" {
		reasoning["effort"] = effort
	}
	if stringField(reasoning, "summary") == "" {
		reasoning["summary"] = "auto"
	}
}

func normalizeInput(body map[string]any) {
	switch input := body["input"].(type) {
	case string:
		text := strings.TrimSpace(input)
		if text == "" {
			body["input"] = []any{}
			return
		}
		body["input"] = []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": text},
				},
			},
		}
	}
}

func removeUnsupportedParams(body map[string]any) {
	delete(body, "max_output_tokens")
	delete(body, "max_completion_tokens")
}

func requestID(r *http.Request, body map[string]any) string {
	for _, key := range []string{"x-client-request-id", "x-request-id", "session_id"} {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			return value
		}
	}
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId", "prompt_cache_key"} {
		if value := stringField(body, key); value != "" {
			return value
		}
	}
	return ""
}

type indexedItem struct {
	index int
	item  any
}

func aggregateResponsesSSE(r io.Reader) (map[string]any, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxRequestBodyBytes)
	var dataLines []string
	var final map[string]any
	var items []indexedItem
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if data == "" || data == "[DONE]" {
			return nil
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		switch event["type"] {
		case "response.output_item.done":
			if item, ok := event["item"].(map[string]any); ok {
				index := 0
				if value, ok := numericField(event, "output_index"); ok {
					index = int(value)
				}
				items = append(items, indexedItem{index: index, item: item})
			}
		case "response.completed", "response.done", "response.incomplete":
			if response, ok := event["response"].(map[string]any); ok {
				final = response
			}
		case "response.failed":
			if response, ok := event["response"].(map[string]any); ok {
				final = response
			}
		case "error":
			return fmt.Errorf("%v", event)
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if final == nil {
		return nil, errors.New("upstream stream did not include a final response")
	}
	if len(items) > 0 {
		sort.SliceStable(items, func(i, j int) bool { return items[i].index < items[j].index })
		output := make([]any, 0, len(items))
		for _, item := range items {
			output = append(output, item.item)
		}
		final["output"] = output
	}
	return final, nil
}

func logUsage(response map[string]any) {
	usage, _ := response["usage"].(map[string]any)
	if usage == nil {
		return
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details == nil {
		details, _ = usage["prompt_tokens_details"].(map[string]any)
	}
	cached, _ := numericField(details, "cached_tokens")
	input, _ := numericField(usage, "input_tokens")
	if input == 0 {
		input, _ = numericField(usage, "prompt_tokens")
	}
	total, _ := numericField(usage, "total_tokens")
	log.Printf("response usage input_tokens=%.0f cached_tokens=%.0f total_tokens=%.0f", input, cached, total)
}

func copyStreamingResponse(w http.ResponseWriter, r io.Reader) {
	buf := make([]byte, 32*1024)
	flusher, _ := w.(http.Flusher)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func copyResponseHeaders(w http.ResponseWriter, header http.Header, stream bool) {
	for _, key := range []string{"Content-Type", "Cache-Control"} {
		if value := header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
	}
}

func writeProxyError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": redactTokenLikeText(message),
			"type":    "codex_auth_broker_error",
		},
	})
}

func copyMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
