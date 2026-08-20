package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type chatCompletionRequestInfo struct {
	Stream       bool
	IncludeUsage bool
}

func (p *responsesProxy) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	logEntry := p.beginRequestLog(r)
	defer logEntry.finish()
	if !p.authorizedClient(r) {
		logEntry.markError(http.StatusUnauthorized, "unauthorized")
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	chatBody, err := decodeRequestBody(r.Body, r.Header.Get("Content-Encoding"))
	if err != nil {
		logEntry.markError(http.StatusBadRequest, err.Error())
		writeChatCompletionsError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	body, chatInfo, err := translateChatCompletionsBody(chatBody)
	if err != nil {
		logEntry.markError(http.StatusBadRequest, err.Error())
		writeChatCompletionsError(w, http.StatusBadRequest, err.Error(), chatErrorParam(err))
		return
	}

	info := normalizeResponsesBody(body, p.cfg, r)
	logEntry.markRequest(body, info, r)
	log.Printf("chat completions request model=%s normalized=%s service_tier=%s stream=%t prompt_cache_key=%t prompt_cache_retention=%s",
		info.Model, info.NormalizedModel, info.ServiceTier, info.Stream,
		info.PromptCacheKeySet, valueOr(info.PromptCacheRetention, "none"))

	// The ChatGPT Codex endpoint streams Responses events even when the
	// downstream Chat Completions client requested one final JSON object.
	upstreamBody := copyMap(body)
	upstreamBody["stream"] = true
	encoded, err := json.Marshal(upstreamBody)
	if err != nil {
		logEntry.markError(http.StatusBadRequest, "invalid request body")
		writeChatCompletionsError(w, http.StatusBadRequest, "invalid request body", "")
		return
	}

	release, limitFail := p.acquireUpstreamSlot(r.Context())
	if limitFail != nil {
		p.writeDispatchFailure(w, logEntry, limitFail)
		return
	}
	// Held until the handler returns, so a streaming response occupies its slot
	// for the full duration of the stream.
	defer release()
	resp, fail := p.dispatchUpstream(r.Context(), encoded, info, body, r)
	if fail != nil {
		p.writeDispatchFailure(w, logEntry, fail)
		return
	}
	defer resp.Body.Close()
	logEntry.markUpstreamStatus(resp.StatusCode)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		p.relayChatCompletionsUpstreamError(w, logEntry, resp)
		return
	}

	if !chatInfo.Stream {
		finalResponse, err := aggregateResponsesSSE(resp.Body)
		if err != nil {
			message := "aggregate upstream stream failed: " + err.Error()
			logEntry.markError(http.StatusBadGateway, message)
			writeChatCompletionsError(w, http.StatusBadGateway, message, "")
			return
		}
		completion, err := chatCompletionFromResponse(finalResponse, info.NormalizedModel)
		if err != nil {
			message := "translate upstream response failed: " + err.Error()
			logEntry.markError(http.StatusBadGateway, message)
			writeChatCompletionsError(w, http.StatusBadGateway, message, "")
			return
		}
		logEntry.markUsage(logUsage(finalResponse))
		recordAppliedServiceTier(logEntry, info.ServiceTier, extractServiceTier(finalResponse))
		logEntry.markStatus(http.StatusOK)
		writeJSON(w, http.StatusOK, completion)
		return
	}

	logEntry.markStatus(resp.StatusCode)
	copyResponseHeaders(w, resp.Header, true)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	usage, appliedTier, streamErr := copyChatCompletionsStream(
		w, resp.Body, info.NormalizedModel, chatInfo.IncludeUsage,
	)
	logEntry.markUsage(usage)
	recordAppliedServiceTier(logEntry, info.ServiceTier, appliedTier)
	if streamErr != nil {
		logEntry.markStreamError("translate upstream stream failed: " + streamErr.Error())
		log.Printf("chat completions stream translation error: %v", streamErr)
	}
}

func (p *responsesProxy) relayChatCompletionsUpstreamError(w http.ResponseWriter, logEntry *pendingRequestLog, resp *http.Response) {
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	message := summarizeUpstreamError(responseBody, resp.StatusCode)
	logEntry.markError(resp.StatusCode, message)
	if len(strings.TrimSpace(string(responseBody))) == 0 {
		writeChatCompletionsError(w, resp.StatusCode, message, "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write([]byte(redactTokenLikeText(string(responseBody))))
}

type chatRequestError struct {
	param   string
	message string
}

func (e *chatRequestError) Error() string {
	return e.message
}

func chatError(param, format string, args ...any) error {
	return &chatRequestError{param: param, message: fmt.Sprintf(format, args...)}
}

func chatErrorParam(err error) string {
	var requestErr *chatRequestError
	if errors.As(err, &requestErr) {
		return requestErr.param
	}
	return ""
}

func writeChatCompletionsError(w http.ResponseWriter, status int, message, param string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": redactTokenLikeText(message),
			"type":    "invalid_request_error",
			"param":   valueOrAny(param, nil),
			"code":    nil,
		},
	})
}

func valueOrAny(value string, fallback any) any {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func translateChatCompletionsBody(chatBody map[string]any) (map[string]any, chatCompletionRequestInfo, error) {
	var info chatCompletionRequestInfo
	model := stringField(chatBody, "model")
	if model == "" {
		return nil, info, chatError("model", "model is required")
	}

	if n, ok := numericField(chatBody, "n"); ok && n != 1 {
		return nil, info, chatError("n", "n must be 1; the Codex Responses backend returns one choice")
	}
	if stream, ok := chatBody["stream"].(bool); ok {
		info.Stream = stream
	} else if _, exists := chatBody["stream"]; exists {
		return nil, info, chatError("stream", "stream must be a boolean")
	}
	if streamOptions, ok := chatBody["stream_options"].(map[string]any); ok {
		info.IncludeUsage, _ = streamOptions["include_usage"].(bool)
	}
	if streamOptions, ok := chatBody["streamOptions"].(map[string]any); ok {
		info.IncludeUsage, _ = streamOptions["includeUsage"].(bool)
	}

	messages, ok := chatBody["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, info, chatError("messages", "messages must be a non-empty array")
	}
	input, instructions, err := translateChatMessages(messages)
	if err != nil {
		return nil, info, err
	}

	body := map[string]any{
		"model":  model,
		"input":  input,
		"stream": info.Stream,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}

	copyChatRequestFields(chatBody, body)
	if err := translateChatReasoning(chatBody, body); err != nil {
		return nil, info, err
	}
	if err := translateChatTextOptions(chatBody, body); err != nil {
		return nil, info, err
	}
	if err := translateChatTools(chatBody, body); err != nil {
		return nil, info, err
	}
	return body, info, nil
}

func copyChatRequestFields(from, to map[string]any) {
	for _, key := range []string{
		"metadata",
		"parallel_tool_calls",
		"service_tier",
		"store",
		"temperature",
		"top_p",
		"truncation",
		"prompt_cache_key",
		"prompt_cache_retention",
		"prompt_cache_options",
		"session_id",
		"conversation_id",
		"max_tokens",
		"max_completion_tokens",
		"logprobs",
		"top_logprobs",
	} {
		if value, ok := from[key]; ok {
			to[key] = value
		}
	}
	for camel, snake := range map[string]string{
		"parallelToolCalls":    "parallel_tool_calls",
		"serviceTier":          "service_tier",
		"topP":                 "top_p",
		"promptCacheKey":       "prompt_cache_key",
		"promptCacheRetention": "prompt_cache_retention",
		"promptCacheOptions":   "prompt_cache_options",
		"sessionId":            "session_id",
		"conversationId":       "conversation_id",
		"maxTokens":            "max_tokens",
		"maxCompletionTokens":  "max_completion_tokens",
		"topLogprobs":          "top_logprobs",
	} {
		if _, exists := to[snake]; exists {
			continue
		}
		if value, ok := from[camel]; ok {
			to[snake] = value
		}
	}
}

func translateChatReasoning(chatBody, body map[string]any) error {
	reasoning := map[string]any{}
	if source, ok := chatBody["reasoning"].(map[string]any); ok {
		reasoning = copyMap(source)
	} else if _, exists := chatBody["reasoning"]; exists {
		return chatError("reasoning", "reasoning must be an object")
	}
	if effort := valueOr(stringField(chatBody, "reasoning_effort"), stringField(chatBody, "reasoningEffort")); effort != "" {
		reasoning["effort"] = effort
	}
	if len(reasoning) > 0 {
		body["reasoning"] = reasoning
	}
	return nil
}

func translateChatTextOptions(chatBody, body map[string]any) error {
	text := map[string]any{}
	if source, ok := chatBody["text"].(map[string]any); ok {
		text = copyMap(source)
	} else if _, exists := chatBody["text"]; exists {
		return chatError("text", "text must be an object")
	}
	if verbosity := stringField(chatBody, "verbosity"); verbosity != "" {
		text["verbosity"] = verbosity
	}
	if responseFormat, ok := chatBody["response_format"].(map[string]any); ok {
		format, err := translateChatResponseFormat(responseFormat)
		if err != nil {
			return err
		}
		text["format"] = format
	} else if _, exists := chatBody["response_format"]; exists {
		return chatError("response_format", "response_format must be an object")
	}
	if len(text) > 0 {
		body["text"] = text
	}
	return nil
}

func translateChatResponseFormat(responseFormat map[string]any) (map[string]any, error) {
	switch formatType := stringField(responseFormat, "type"); formatType {
	case "text", "json_object":
		return map[string]any{"type": formatType}, nil
	case "json_schema":
		schema, ok := responseFormat["json_schema"].(map[string]any)
		if !ok {
			return nil, chatError("response_format.json_schema", "response_format.json_schema must be an object")
		}
		format := map[string]any{"type": "json_schema"}
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if value, exists := schema[key]; exists {
				format[key] = value
			}
		}
		if stringField(format, "name") == "" {
			return nil, chatError("response_format.json_schema.name", "response_format.json_schema.name is required")
		}
		return format, nil
	default:
		return nil, chatError("response_format.type", "unsupported response_format type %q", formatType)
	}
}

func translateChatTools(chatBody, body map[string]any) error {
	rawTools, hasTools := chatBody["tools"]
	if !hasTools {
		rawTools, hasTools = chatBody["functions"]
	}
	if hasTools {
		tools, ok := rawTools.([]any)
		if !ok {
			return chatError("tools", "tools must be an array")
		}
		translated := make([]any, 0, len(tools))
		for index, rawTool := range tools {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				return chatError(fmt.Sprintf("tools.%d", index), "tool %d must be an object", index)
			}
			translatedTool, err := translateChatTool(tool, !hasKey(chatBody, "tools"))
			if err != nil {
				return err
			}
			translated = append(translated, translatedTool)
		}
		body["tools"] = translated
	}

	if choice, ok := chatBody["tool_choice"]; ok {
		translated, err := translateChatToolChoice(choice)
		if err != nil {
			return err
		}
		body["tool_choice"] = translated
	} else if choice, ok := chatBody["function_call"]; ok {
		translated, err := translateLegacyFunctionChoice(choice)
		if err != nil {
			return err
		}
		body["tool_choice"] = translated
	}
	return nil
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func translateChatTool(tool map[string]any, legacy bool) (map[string]any, error) {
	definition := tool
	if !legacy {
		if toolType := stringField(tool, "type"); toolType != "function" {
			return nil, chatError("tools", "unsupported tool type %q; only function tools are supported", toolType)
		}
		var ok bool
		definition, ok = tool["function"].(map[string]any)
		if !ok {
			return nil, chatError("tools.function", "function tool definition must be an object")
		}
	}
	name := stringField(definition, "name")
	if name == "" {
		return nil, chatError("tools.function.name", "function tool name is required")
	}
	translated := map[string]any{"type": "function", "name": name}
	for _, key := range []string{"description", "parameters", "strict"} {
		if value, ok := definition[key]; ok {
			translated[key] = value
		}
	}
	return translated, nil
}

func translateChatToolChoice(choice any) (any, error) {
	if value, ok := choice.(string); ok {
		switch value {
		case "none", "auto", "required":
			return value, nil
		default:
			return nil, chatError("tool_choice", "unsupported tool_choice %q", value)
		}
	}
	choiceMap, ok := choice.(map[string]any)
	if !ok || stringField(choiceMap, "type") != "function" {
		return nil, chatError("tool_choice", "tool_choice must be none, auto, required, or a function choice")
	}
	function, ok := choiceMap["function"].(map[string]any)
	if !ok || stringField(function, "name") == "" {
		return nil, chatError("tool_choice.function.name", "tool_choice.function.name is required")
	}
	return map[string]any{"type": "function", "name": stringField(function, "name")}, nil
}

func translateLegacyFunctionChoice(choice any) (any, error) {
	if value, ok := choice.(string); ok {
		if value == "none" || value == "auto" {
			return value, nil
		}
		return nil, chatError("function_call", "unsupported function_call %q", value)
	}
	choiceMap, ok := choice.(map[string]any)
	if !ok || stringField(choiceMap, "name") == "" {
		return nil, chatError("function_call.name", "function_call.name is required")
	}
	return map[string]any{"type": "function", "name": stringField(choiceMap, "name")}, nil
}

func translateChatMessages(messages []any) ([]any, string, error) {
	input := make([]any, 0, len(messages))
	for index, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, "", chatError(fmt.Sprintf("messages.%d", index), "message %d must be an object", index)
		}
		role := stringField(message, "role")
		switch role {
		case "system", "developer":
			content, err := translateChatMessageContent(message["content"], "input_text", false)
			if err != nil {
				return nil, "", chatError(fmt.Sprintf("messages.%d.content", index), "%v", err)
			}
			input = append(input, map[string]any{"role": "developer", "content": content})
		case "user":
			content, err := translateChatMessageContent(message["content"], "input_text", true)
			if err != nil {
				return nil, "", chatError(fmt.Sprintf("messages.%d.content", index), "%v", err)
			}
			input = append(input, map[string]any{"role": "user", "content": content})
		case "assistant":
			items, err := translateChatAssistantMessage(message)
			if err != nil {
				return nil, "", chatError(fmt.Sprintf("messages.%d", index), "%v", err)
			}
			input = append(input, items...)
		case "tool":
			callID := stringField(message, "tool_call_id")
			if callID == "" {
				return nil, "", chatError(fmt.Sprintf("messages.%d.tool_call_id", index), "tool_call_id is required")
			}
			output, err := chatTextOnlyContent(message["content"])
			if err != nil {
				return nil, "", chatError(fmt.Sprintf("messages.%d.content", index), "%v", err)
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  output,
			})
		default:
			return nil, "", chatError(fmt.Sprintf("messages.%d.role", index), "unsupported message role %q", role)
		}
	}
	return input, "", nil
}

func translateChatAssistantMessage(message map[string]any) ([]any, error) {
	var items []any
	if contentValue, exists := message["content"]; exists && contentValue != nil {
		content, err := translateChatMessageContent(contentValue, "output_text", false)
		if err != nil {
			return nil, err
		}
		if len(content) > 0 {
			items = append(items, map[string]any{"role": "assistant", "content": content})
		}
	}

	if rawCalls, exists := message["tool_calls"]; exists {
		calls, ok := rawCalls.([]any)
		if !ok {
			return nil, errors.New("tool_calls must be an array")
		}
		for index, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tool_calls.%d must be an object", index)
			}
			if callType := stringField(call, "type"); callType != "" && callType != "function" {
				return nil, fmt.Errorf("unsupported tool call type %q", callType)
			}
			function, ok := call["function"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tool_calls.%d.function must be an object", index)
			}
			callID := stringField(call, "id")
			name := stringField(function, "name")
			if callID == "" || name == "" {
				return nil, fmt.Errorf("tool_calls.%d requires id and function.name", index)
			}
			items = append(items, map[string]any{
				"type":      "function_call",
				"call_id":   callID,
				"name":      name,
				"arguments": valueOr(stringField(function, "arguments"), "{}"),
			})
		}
	}

	if functionCall, ok := message["function_call"].(map[string]any); ok {
		name := stringField(functionCall, "name")
		if name == "" {
			return nil, errors.New("function_call.name is required")
		}
		items = append(items, map[string]any{
			"type":      "function_call",
			"call_id":   "call_" + name,
			"name":      name,
			"arguments": valueOr(stringField(functionCall, "arguments"), "{}"),
		})
	}
	if len(items) == 0 {
		return nil, errors.New("assistant message requires content or tool_calls")
	}
	return items, nil
}

func translateChatMessageContent(value any, textType string, allowMedia bool) ([]any, error) {
	switch content := value.(type) {
	case string:
		return []any{map[string]any{"type": textType, "text": content}}, nil
	case []any:
		parts := make([]any, 0, len(content))
		for index, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content part %d must be an object", index)
			}
			switch partType := stringField(part, "type"); partType {
			case "text":
				parts = append(parts, map[string]any{"type": textType, "text": stringField(part, "text")})
			case "refusal":
				parts = append(parts, map[string]any{"type": textType, "text": stringField(part, "refusal")})
			case "image_url":
				if !allowMedia {
					return nil, errors.New("image content is only supported on user messages")
				}
				image, ok := part["image_url"].(map[string]any)
				if !ok || stringField(image, "url") == "" {
					return nil, fmt.Errorf("content part %d image_url.url is required", index)
				}
				translated := map[string]any{
					"type":      "input_image",
					"image_url": stringField(image, "url"),
				}
				if detail := stringField(image, "detail"); detail != "" {
					translated["detail"] = detail
				}
				parts = append(parts, translated)
			case "file":
				if !allowMedia {
					return nil, errors.New("file content is only supported on user messages")
				}
				file, ok := part["file"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("content part %d file must be an object", index)
				}
				translated := map[string]any{"type": "input_file"}
				for _, key := range []string{"file_data", "file_id", "filename"} {
					if fileValue, exists := file[key]; exists {
						translated[key] = fileValue
					}
				}
				parts = append(parts, translated)
			case "input_audio":
				return nil, errors.New("audio input is not supported by the Codex Responses backend")
			default:
				return nil, fmt.Errorf("unsupported content part type %q", partType)
			}
		}
		return parts, nil
	case nil:
		return nil, nil
	default:
		return nil, errors.New("content must be a string or an array")
	}
}

func chatTextOnlyContent(value any) (string, error) {
	switch content := value.(type) {
	case string:
		return content, nil
	case []any:
		var parts []string
		for index, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok || stringField(part, "type") != "text" {
				return "", fmt.Errorf("content part %d must be text", index)
			}
			parts = append(parts, stringField(part, "text"))
		}
		return strings.Join(parts, ""), nil
	case nil:
		return "", nil
	default:
		return "", errors.New("content must be text")
	}
}

func chatCompletionFromResponse(response map[string]any, fallbackModel string) (map[string]any, error) {
	if status := stringField(response, "status"); status == "failed" || status == "cancelled" {
		return nil, fmt.Errorf("upstream response status is %s", status)
	}

	var textParts []string
	var refusalParts []string
	var toolCalls []any
	if output, ok := response["output"].([]any); ok {
		for _, rawItem := range output {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			switch stringField(item, "type") {
			case "message":
				content, _ := item["content"].([]any)
				for _, rawPart := range content {
					part, ok := rawPart.(map[string]any)
					if !ok {
						continue
					}
					switch stringField(part, "type") {
					case "output_text", "text":
						textParts = append(textParts, stringField(part, "text"))
					case "refusal":
						refusalParts = append(refusalParts, stringField(part, "refusal"))
					}
				}
			case "function_call":
				callID := valueOr(stringField(item, "call_id"), stringField(item, "id"))
				toolCalls = append(toolCalls, map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      stringField(item, "name"),
						"arguments": valueOr(stringField(item, "arguments"), "{}"),
					},
				})
			}
		}
	}

	content := any(strings.Join(textParts, ""))
	if len(textParts) == 0 && len(toolCalls) > 0 {
		content = nil
	}
	message := map[string]any{
		"role":        "assistant",
		"content":     content,
		"refusal":     nil,
		"annotations": []any{},
	}
	if len(refusalParts) > 0 {
		message["refusal"] = strings.Join(refusalParts, "")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	model := valueOr(stringField(response, "model"), fallbackModel)
	result := map[string]any{
		"id":      chatCompletionID(stringField(response, "id")),
		"object":  "chat.completion",
		"created": chatCompletionCreated(response),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"logprobs":      nil,
				"finish_reason": chatFinishReason(response, len(toolCalls) > 0),
			},
		},
	}
	usage := extractTokenUsage(response)
	if usage.hasAny() {
		result["usage"] = chatUsageObject(usage)
	}
	if tier := extractServiceTier(response); tier != "" {
		result["service_tier"] = tier
	}
	return result, nil
}

func chatCompletionID(upstreamID string) string {
	switch {
	case strings.HasPrefix(upstreamID, "chatcmpl-"):
		return upstreamID
	case strings.HasPrefix(upstreamID, "resp_"):
		return "chatcmpl-" + strings.TrimPrefix(upstreamID, "resp_")
	case upstreamID != "":
		return "chatcmpl-" + upstreamID
	default:
		return "chatcmpl-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
}

func chatCompletionCreated(response map[string]any) int64 {
	if created, ok := numericField(response, "created_at"); ok {
		return int64(created)
	}
	if created, ok := numericField(response, "created"); ok {
		return int64(created)
	}
	return time.Now().Unix()
}

func chatFinishReason(response map[string]any, hasToolCalls bool) string {
	if stringField(response, "status") == "incomplete" {
		return "length"
	}
	if hasToolCalls {
		return "tool_calls"
	}
	return "stop"
}

func chatUsageObject(usage tokenUsage) map[string]any {
	total := int64Value(usage.TotalTokens)
	if usage.TotalTokens == nil {
		total = int64Value(usage.InputTokens) + int64Value(usage.OutputTokens)
	}
	result := map[string]any{
		"prompt_tokens":     int64Value(usage.InputTokens),
		"completion_tokens": int64Value(usage.OutputTokens),
		"total_tokens":      total,
	}
	if usage.CachedTokens != nil || usage.CacheWriteTokens != nil {
		details := map[string]any{}
		if usage.CachedTokens != nil {
			details["cached_tokens"] = *usage.CachedTokens
		}
		if usage.CacheWriteTokens != nil {
			details["cache_write_tokens"] = *usage.CacheWriteTokens
		}
		result["prompt_tokens_details"] = details
	}
	if usage.ReasoningTokens != nil {
		result["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": *usage.ReasoningTokens,
		}
	}
	return result
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

type chatStreamTool struct {
	index            int
	argumentsEmitted bool
}

type chatStreamTranslator struct {
	w            http.ResponseWriter
	flusher      http.Flusher
	fallback     string
	includeUsage bool

	id          string
	created     int64
	model       string
	serviceTier string
	roleSent    bool
	textSeen    bool
	refusalSeen bool
	toolSeen    bool
	terminal    bool
	usage       tokenUsage
	tools       map[string]*chatStreamTool
}

func copyChatCompletionsStream(w http.ResponseWriter, r io.Reader, fallbackModel string, includeUsage bool) (tokenUsage, string, error) {
	translator := &chatStreamTranslator{
		w:            w,
		fallback:     fallbackModel,
		includeUsage: includeUsage,
		model:        fallbackModel,
		created:      time.Now().Unix(),
		tools:        map[string]*chatStreamTool{},
	}
	translator.flusher, _ = w.(http.Flusher)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxRequestBodyBytes)
	var dataLines []string
	flushEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			if !translator.terminal {
				if err := translator.finish(nil); err != nil {
					return err
				}
			}
			return nil
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		return translator.consume(event)
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if err := flushEvent(); err != nil {
				return translator.usage, translator.serviceTier, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return translator.usage, translator.serviceTier, err
	}
	if err := flushEvent(); err != nil {
		return translator.usage, translator.serviceTier, err
	}
	if !translator.terminal {
		_ = translator.writeDone()
		return translator.usage, translator.serviceTier, errors.New("upstream stream ended without a terminal response event")
	}
	return translator.usage, translator.serviceTier, nil
}

func (t *chatStreamTranslator) consume(event map[string]any) error {
	switch stringField(event, "type") {
	case "response.created", "response.in_progress":
		if response, ok := event["response"].(map[string]any); ok {
			t.captureResponse(response)
		}
		return t.ensureRole()
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if item == nil {
			return nil
		}
		if stringField(item, "type") == "function_call" {
			_, err := t.ensureTool(item, intField(event, "output_index"))
			return err
		}
		if stringField(item, "type") == "message" {
			return t.ensureRole()
		}
	case "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		if item == nil {
			return nil
		}
		if stringField(item, "type") == "message" {
			return t.emitMessageFallback(item)
		}
		if stringField(item, "type") != "function_call" {
			return nil
		}
		tool, err := t.ensureTool(item, intField(event, "output_index"))
		if err != nil {
			return err
		}
		arguments := stringField(item, "arguments")
		if arguments != "" && !tool.argumentsEmitted {
			tool.argumentsEmitted = true
			return t.writeToolArguments(tool.index, arguments)
		}
	case "response.output_text.delta":
		if err := t.ensureRole(); err != nil {
			return err
		}
		t.textSeen = true
		return t.writeChoice(map[string]any{"content": stringField(event, "delta")}, nil)
	case "response.refusal.delta":
		if err := t.ensureRole(); err != nil {
			return err
		}
		t.refusalSeen = true
		return t.writeChoice(map[string]any{"refusal": stringField(event, "delta")}, nil)
	case "response.function_call_arguments.delta":
		key := chatStreamToolKey(stringField(event, "item_id"), intField(event, "output_index"))
		tool := t.tools[key]
		if tool == nil {
			var err error
			tool, err = t.ensureTool(map[string]any{
				"id":      stringField(event, "item_id"),
				"call_id": stringField(event, "item_id"),
				"type":    "function_call",
			}, intField(event, "output_index"))
			if err != nil {
				return err
			}
		}
		tool.argumentsEmitted = true
		return t.writeToolArguments(tool.index, stringField(event, "delta"))
	case "response.completed", "response.done", "response.incomplete":
		response, _ := event["response"].(map[string]any)
		return t.finish(response)
	case "response.failed":
		response, _ := event["response"].(map[string]any)
		if response != nil {
			t.captureResponse(response)
		}
		return t.fail("upstream response failed")
	case "error":
		message := "upstream streaming error"
		if upstreamError, ok := event["error"].(map[string]any); ok {
			message = valueOr(stringField(upstreamError, "message"), message)
		} else if eventMessage := stringField(event, "message"); eventMessage != "" {
			message = eventMessage
		}
		return t.fail(message)
	}
	return nil
}

func (t *chatStreamTranslator) captureResponse(response map[string]any) {
	if upstreamID := stringField(response, "id"); upstreamID != "" {
		t.id = chatCompletionID(upstreamID)
	}
	if model := stringField(response, "model"); model != "" {
		t.model = model
	}
	t.created = chatCompletionCreated(response)
	if usage := extractTokenUsage(response); usage.hasAny() {
		t.usage = usage
	}
	if tier := extractServiceTier(response); tier != "" {
		t.serviceTier = tier
	}
}

func (t *chatStreamTranslator) ensureRole() error {
	if t.roleSent {
		return nil
	}
	t.roleSent = true
	return t.writeChoice(map[string]any{"role": "assistant", "content": ""}, nil)
}

func (t *chatStreamTranslator) ensureTool(item map[string]any, outputIndex int) (*chatStreamTool, error) {
	if err := t.ensureRole(); err != nil {
		return nil, err
	}
	key := chatStreamToolKey(stringField(item, "id"), outputIndex)
	if existing := t.tools[key]; existing != nil {
		return existing, nil
	}
	tool := &chatStreamTool{index: len(t.tools)}
	t.tools[key] = tool
	t.toolSeen = true
	callID := valueOr(stringField(item, "call_id"), stringField(item, "id"))
	arguments := stringField(item, "arguments")
	if arguments != "" {
		tool.argumentsEmitted = true
	}
	err := t.writeChoice(map[string]any{
		"tool_calls": []any{
			map[string]any{
				"index": tool.index,
				"id":    callID,
				"type":  "function",
				"function": map[string]any{
					"name":      stringField(item, "name"),
					"arguments": arguments,
				},
			},
		},
	}, nil)
	return tool, err
}

func chatStreamToolKey(itemID string, outputIndex int) string {
	if itemID != "" {
		return itemID
	}
	return "output:" + strconv.Itoa(outputIndex)
}

func (t *chatStreamTranslator) writeToolArguments(index int, arguments string) error {
	return t.writeChoice(map[string]any{
		"tool_calls": []any{
			map[string]any{
				"index": index,
				"function": map[string]any{
					"arguments": arguments,
				},
			},
		},
	}, nil)
}

func (t *chatStreamTranslator) finish(response map[string]any) error {
	if t.terminal {
		return nil
	}
	if response != nil {
		t.captureResponse(response)
		if err := t.emitResponseOutputFallback(response); err != nil {
			return err
		}
	}
	if err := t.ensureRole(); err != nil {
		return err
	}
	reason := any("stop")
	if response != nil && stringField(response, "status") == "incomplete" {
		reason = "length"
	} else if t.toolSeen {
		reason = "tool_calls"
	}
	if err := t.writeChoice(map[string]any{}, reason); err != nil {
		return err
	}
	if t.includeUsage && t.usage.hasAny() {
		chunk := t.baseChunk()
		chunk["choices"] = []any{}
		chunk["usage"] = chatUsageObject(t.usage)
		if err := t.writeObject(chunk); err != nil {
			return err
		}
	}
	t.terminal = true
	return t.writeDone()
}

func (t *chatStreamTranslator) emitResponseOutputFallback(response map[string]any) error {
	output, _ := response["output"].([]any)
	for index, rawItem := range output {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(item, "type") {
		case "message":
			if err := t.emitMessageFallback(item); err != nil {
				return err
			}
		case "function_call":
			tool, err := t.ensureTool(item, index)
			if err != nil {
				return err
			}
			arguments := stringField(item, "arguments")
			if arguments != "" && !tool.argumentsEmitted {
				tool.argumentsEmitted = true
				if err := t.writeToolArguments(tool.index, arguments); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (t *chatStreamTranslator) emitMessageFallback(item map[string]any) error {
	content, _ := item["content"].([]any)
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(part, "type") {
		case "output_text", "text":
			if t.textSeen {
				continue
			}
			t.textSeen = true
			if err := t.ensureRole(); err != nil {
				return err
			}
			if err := t.writeChoice(map[string]any{"content": stringField(part, "text")}, nil); err != nil {
				return err
			}
		case "refusal":
			if t.refusalSeen {
				continue
			}
			t.refusalSeen = true
			if err := t.ensureRole(); err != nil {
				return err
			}
			if err := t.writeChoice(map[string]any{"refusal": stringField(part, "refusal")}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *chatStreamTranslator) fail(message string) error {
	if t.terminal {
		return nil
	}
	t.terminal = true
	if err := t.writeObject(map[string]any{
		"error": map[string]any{
			"message": redactTokenLikeText(message),
			"type":    "upstream_error",
			"code":    nil,
		},
	}); err != nil {
		return err
	}
	if err := t.writeDone(); err != nil {
		return err
	}
	return errors.New(redactTokenLikeText(message))
}

func (t *chatStreamTranslator) writeChoice(delta map[string]any, finishReason any) error {
	chunk := t.baseChunk()
	chunk["choices"] = []any{
		map[string]any{
			"index":         0,
			"delta":         delta,
			"logprobs":      nil,
			"finish_reason": finishReason,
		},
	}
	if t.includeUsage {
		chunk["usage"] = nil
	}
	return t.writeObject(chunk)
}

func (t *chatStreamTranslator) baseChunk() map[string]any {
	if t.id == "" {
		t.id = chatCompletionID("")
	}
	if t.model == "" {
		t.model = t.fallback
	}
	chunk := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
	}
	if t.serviceTier != "" {
		chunk["service_tier"] = t.serviceTier
	}
	return chunk
}

func (t *chatStreamTranslator) writeObject(value map[string]any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(t.w, "data: %s\n\n", encoded); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

func (t *chatStreamTranslator) writeDone() error {
	if _, err := io.WriteString(t.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

func intField(m map[string]any, key string) int {
	value, _ := numericField(m, key)
	return int(value)
}
