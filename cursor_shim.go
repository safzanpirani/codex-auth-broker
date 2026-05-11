package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type cursorShim struct {
	proxy     *responsesProxy
	runs      *cursorRunStore
	histories *cursorHistoryStore
}

type cursorHistoryStore struct {
	mu    sync.Mutex
	items map[string][]cursorHistoryMessage
}

type cursorHistoryMessage struct {
	Role    string
	Content string
}

type cursorRunStore struct {
	mu   sync.Mutex
	runs map[string]*cursorRun
}

type cursorRun struct {
	id      string
	mu      sync.Mutex
	queue   [][]byte
	waiters []chan []byte
}

type responsesEvent struct {
	Name string
	Data map[string]any
}

type pendingFunctionCall struct {
	Key    string
	CallID string
	Name   string
	Args   string
}

func newCursorShim(proxy *responsesProxy) *cursorShim {
	return &cursorShim{proxy: proxy, runs: &cursorRunStore{runs: map[string]*cursorRun{}}, histories: &cursorHistoryStore{items: map[string][]cursorHistoryMessage{}}}
}

func (s *cursorShim) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": "invalid_argument", "message": err.Error()})
		return
	}
	codec := cursorCodec(r.Header.Get("Content-Type"))
	switch {
	case r.URL.Path == "/auth/exchange_user_api_key":
		token := cursorFakeToken()
		writeJSON(w, http.StatusOK, map[string]any{"accessToken": token, "refreshToken": token})
	case r.URL.Path == "/agent.v1.AgentService/Run" || r.URL.Path == "/agent.v1.AgentService/RunSSE":
		s.handleRun(w, r, codec, body)
	case r.URL.Path == "/v1/traces":
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, "/agent.v1.AgentService/"):
		if response, ok := s.unaryStub(r.URL.Path, codec, body); ok {
			w.Header().Set("Content-Type", connectUnaryContentType(codec))
			w.Header().Set("Connect-Protocol-Version", "1")
			_, _ = w.Write(response)
			return
		}
		writeJSON(w, http.StatusNotImplemented, map[string]any{"code": "unimplemented", "message": "cursor shim does not implement " + r.URL.Path})
	case strings.HasSuffix(r.URL.Path, "/BidiAppend"):
		s.handleBidiAppend(w, codec, body)
	default:
		if response, ok := s.unaryStub(r.URL.Path, codec, body); ok {
			w.Header().Set("Content-Type", connectUnaryContentType(codec))
			w.Header().Set("Connect-Protocol-Version", "1")
			_, _ = w.Write(response)
			return
		}
		writeJSON(w, http.StatusNotImplemented, map[string]any{"code": "unimplemented", "message": "cursor shim does not implement " + r.URL.Path})
	}
}

func (s *cursorShim) unaryStub(path string, codec connectCodec, body []byte) ([]byte, bool) {
	if codec == connectJSON {
		var response map[string]any
		switch {
		case strings.Contains(path, "GetServerConfig"):
			base := s.baseURL()
			response = map[string]any{
				"http2_config": 1,
				"http2Config":  "HTTP2_CONFIG_FORCE_ALL_DISABLED",
				"agent_url_config": map[string]any{
					"agent_url":  base,
					"agentn_url": base,
				},
				"agentUrlConfig": map[string]any{
					"agentUrl":  base,
					"agentnUrl": base,
				},
			}
		case strings.Contains(path, "GetCliDownloadUrl"):
			response = map[string]any{"url": "", "version": version}
		case strings.Contains(path, "GetMe"):
			response = map[string]any{"auth_id": "local", "user_id": 1, "email": "local@dev", "first_name": "Local", "last_name": "Dev"}
		case strings.Contains(path, "GetUsableModels") || strings.Contains(path, "AvailableModels"):
			response = map[string]any{"models": []any{map[string]any{"id": s.defaultModel(), "model_name": s.defaultModel(), "display": "Local"}}, "model_names": []string{s.defaultModel()}}
		case strings.Contains(path, "GetDefaultModel"):
			response = map[string]any{"model": map[string]any{"id": s.defaultModel(), "model_name": s.defaultModel(), "display": "Local"}, "model_id": s.defaultModel()}
		case strings.Contains(path, "GetUserInfo"):
			response = map[string]any{"email": "local@dev", "display_name": "Local", "tier": "pro"}
		case strings.Contains(path, "CountTokens"):
			response = map[string]any{"tokens": max(1, len(body)/4)}
		case strings.Contains(path, "GetEffectiveTokenLimit"):
			response = map[string]any{"limit": 200000, "token_limit": 200000}
		case strings.Contains(path, "CheckFeatureStatus") || strings.Contains(path, "CheckFeaturesStatus"):
			response = map[string]any{"status": "ENABLED", "enabled": true}
		case strings.Contains(path, "CheckNumberConfig") || strings.Contains(path, "CheckNumberConfigs"):
			response = map[string]any{"value": 0}
		case strings.Contains(path, "NameAgent") || strings.Contains(path, "CreateTranscriptOverview"):
			response = map[string]any{"name": "Local chat", "title": "Local chat"}
		case cursorEmptyUnary(path):
			response = map[string]any{}
		default:
			return nil, false
		}
		encoded, _ := json.Marshal(response)
		return encoded, true
	}
	switch {
	case strings.Contains(path, "GetServerConfig"):
		base := s.baseURL()
		agentURLConfig := pbConcat(pbString(1, base), pbString(2, base))
		return pbConcat(pbInt(7, 1), pbMessage(27, agentURLConfig)), true
	case strings.Contains(path, "GetCliDownloadUrl"):
		currentVersion := pbStringField(body, 1)
		if currentVersion == "" {
			currentVersion = version
		}
		return pbString(2, currentVersion), true
	case strings.Contains(path, "GetMe"):
		return pbConcat(pbString(1, "local"), pbInt(2, 1), pbString(3, "local@dev"), pbString(4, "Local"), pbString(5, "Dev")), true
	case strings.Contains(path, "AvailableModels"):
		model := s.defaultModel()
		return pbConcat(pbString(1, model), pbMessage(2, pbString(1, model))), true
	case strings.Contains(path, "GetUsableModels") || strings.Contains(path, "GetDefaultModel"):
		return pbMessage(1, cursorModelDetails(s.defaultModel())), true
	case strings.Contains(path, "GetUserInfo"):
		return pbConcat(pbString(1, "local@dev"), pbString(2, "Local"), pbString(3, "pro")), true
	case strings.Contains(path, "CountTokens"):
		return pbInt(1, uint64(max(1, len(body)/4))), true
	case strings.Contains(path, "GetEffectiveTokenLimit"):
		return pbInt(1, 200000), true
	case strings.Contains(path, "CheckFeatureStatus") || strings.Contains(path, "CheckFeaturesStatus"):
		return pbInt(1, 1), true
	case strings.Contains(path, "CheckNumberConfig") || strings.Contains(path, "CheckNumberConfigs"):
		return pbInt(1, 0), true
	case strings.Contains(path, "NameAgent") || strings.Contains(path, "CreateTranscriptOverview"):
		return pbString(1, "Local chat"), true
	case strings.Contains(path, "GetUserPrivacyMode"):
		return pbInt(1, 2), true
	case cursorEmptyUnary(path):
		return nil, true
	default:
		return nil, false
	}
}

func (s *cursorShim) handleRun(w http.ResponseWriter, r *http.Request, codec connectCodec, body []byte) {
	w.Header().Set("Content-Type", connectStreamContentType(codec))
	w.Header().Set("Connect-Protocol-Version", "1")
	flusher, _ := w.(http.Flusher)
	open := firstCursorPayload(codec, body)
	requestID := cursorRequestID(codec, open)
	if requestID == "" {
		requestID = strings.TrimSpace(r.Header.Get("x-request-id"))
	}
	if requestID == "" {
		requestID = fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	run := s.runs.get(requestID)
	defer func() {
		_, _ = w.Write(encodeConnectFrame([]byte("{}"), 0x02))
		if flusher != nil {
			flusher.Flush()
		}
		s.runs.close(requestID)
	}()

	initial := open
	if !isCursorInitial(codec, open) {
		var err error
		initial, err = run.next(20 * time.Second)
		if err != nil {
			s.writeCursorFrame(w, flusher, codec, cursorTextFrame(codec, "Shim error: "+err.Error()))
			return
		}
	}
	if err := s.converse(r.Context(), w, flusher, codec, r, run, initial); err != nil {
		log.Printf("cursor-shim Run failed request_id=%s error=%v", requestID, err)
		s.writeCursorFrame(w, flusher, codec, cursorTextFrame(codec, "Shim error: "+err.Error()))
	}
}

func (s *cursorShim) converse(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, codec connectCodec, original *http.Request, run *cursorRun, initial []byte) error {
	prompt := cursorPrompt(codec, initial)
	model := cursorModel(codec, initial, s.defaultModel())
	s.writeCursorFrame(w, flusher, codec, cursorStartFrame(codec))
	conversationID := cursorConversationID(codec, initial)
	inputMessages := s.responsesInputForTurn(codec, initial, conversationID, prompt)
	input := any(inputMessages)
	requestHistory := append([]any(nil), inputMessages...)
	var assistant strings.Builder
	for turn := 0; turn < 4; turn++ {
		turnResult, err := s.pumpResponsesTurn(ctx, w, flusher, codec, original, model, input)
		assistant.WriteString(turnResult.Text)
		if err != nil || turnResult.Call == nil {
			if err != nil {
				return err
			}
			break
		}
		call := turnResult.Call
		call.ExecID = uint64(turn + 1)
		s.writeCursorFrame(w, flusher, codec, cursorToolCallUpdateFrame(codec, 2, call, nil))
		s.writeCursorFrame(w, flusher, codec, cursorExecFrame(codec, call))
		result, err := s.nextToolResult(codec, run, call)
		if err != nil {
			return err
		}
		if result.CallID == "" {
			result.CallID = call.CallID
		}
		functionID := call.CallID
		if !strings.HasPrefix(functionID, "fc") {
			functionID = "fc_" + strings.TrimPrefix(functionID, "call_")
		}
		s.writeCursorFrame(w, flusher, codec, cursorToolCallUpdateFrame(codec, 3, call, result.RawUIResult))
		requestHistory = append(requestHistory,
			map[string]any{"type": "function_call", "id": functionID, "call_id": call.CallID, "name": call.Name, "arguments": call.Args, "status": "completed"},
			map[string]any{"type": "function_call_output", "call_id": result.CallID, "output": result.Output},
		)
		input = requestHistory
	}
	s.histories.append(conversationID, "user", prompt)
	if text := strings.TrimSpace(assistant.String()); text != "" {
		s.histories.append(conversationID, "assistant", text)
	}
	s.writeCursorFrame(w, flusher, codec, cursorSummaryFrame(codec))
	return nil
}

func (s *cursorShim) responsesInputForTurn(codec connectCodec, initial []byte, conversationID, prompt string) []any {
	if messages := cursorHistoryFromClient(codec, initial); len(messages) > 0 {
		return cursorHistoryMessagesToResponsesInput(messages, prompt)
	}
	return s.histories.responsesInput(conversationID, prompt)
}

func (s *cursorShim) nextToolResult(codec connectCodec, run *cursorRun, call *cursorToolCall) (cursorToolResultData, error) {
	var last cursorToolResultData
	for attempts := 0; attempts < 8; attempts++ {
		resultBytes, err := run.next(120 * time.Second)
		if err != nil {
			return last, err
		}
		last = cursorToolResult(codec, resultBytes, call)
		if strings.TrimSpace(last.Output) != "" || last.CallID != "" {
			return last, nil
		}
	}
	return last, nil
}

func (s *cursorHistoryStore) responsesInput(conversationID, prompt string) []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages := append([]cursorHistoryMessage(nil), s.items[conversationID]...)
	return cursorHistoryMessagesToResponsesInput(messages, prompt)
}

func cursorHistoryMessagesToResponsesInput(messages []cursorHistoryMessage, prompt string) []any {
	if len(messages) > 16 {
		messages = messages[len(messages)-16:]
	}
	out := make([]any, 0, len(messages)+1)
	for _, msg := range messages {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		out = append(out, map[string]any{"role": msg.Role, "content": msg.Content})
	}
	out = append(out, map[string]any{"role": "user", "content": prompt})
	return out
}

func (s *cursorHistoryStore) append(conversationID, role, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	messages := append(s.items[conversationID], cursorHistoryMessage{Role: role, Content: content})
	if len(messages) > 32 {
		messages = messages[len(messages)-32:]
	}
	s.items[conversationID] = messages
}

type cursorToolCall struct {
	CallID string
	Name   string
	Args   string
	Kind   string
	Path   string
	ExecID uint64
}

type cursorTurnResult struct {
	Call *cursorToolCall
	Text string
}

func (s *cursorShim) pumpResponsesTurn(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, codec connectCodec, original *http.Request, model string, input any) (cursorTurnResult, error) {
	body := map[string]any{"model": model, "input": input, "stream": true, "tools": cursorResponseTools()}
	resp, err := s.openResponsesStream(ctx, original, body)
	if err != nil {
		return cursorTurnResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return cursorTurnResult{}, fmt.Errorf("responses upstream returned %d: %s", resp.StatusCode, redactTokenLikeText(string(data)))
	}
	calls := map[string]*pendingFunctionCall{}
	var textBuffer strings.Builder
	var writtenText strings.Builder
	flushText := func() {
		if textBuffer.Len() == 0 {
			return
		}
		text := textBuffer.String()
		s.writeCursorFrame(w, flusher, codec, cursorTextFrame(codec, text))
		writtenText.WriteString(text)
		textBuffer.Reset()
	}
	for event, err := range readResponsesEvents(resp.Body) {
		if err != nil {
			writtenText.WriteString(textBuffer.String())
			return cursorTurnResult{Text: writtenText.String()}, err
		}
		if delta := outputTextDelta(event); delta != "" {
			textBuffer.WriteString(delta)
		}
		if call := functionCallAdded(event); call != nil {
			calls[call.Key] = call
		}
		if key, delta := functionArgumentsDelta(event); key != "" {
			if call := calls[key]; call != nil {
				call.Args += delta
			}
		}
		if call := cursorToolCallDone(event, calls); call != nil {
			flushText()
			return cursorTurnResult{Text: writtenText.String(), Call: call}, nil
		}
	}
	flushText()
	return cursorTurnResult{Text: writtenText.String()}, nil
}

func (s *cursorShim) openResponsesStream(ctx context.Context, original *http.Request, body map[string]any) (*http.Response, error) {
	normalizeResponsesBody(body, s.proxy.cfg, original)
	body["stream"] = true
	delete(body, "previous_response_id")
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	access, err := s.proxy.auth.current(ctx)
	if err != nil {
		return nil, fmt.Errorf("Codex auth failed: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.proxy.cfg.upstreamURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access.AccessToken)
	req.Header.Set("chatgpt-account-id", access.AccountID)
	req.Header.Set("originator", "codex-auth-broker cursor-shim")
	req.Header.Set("User-Agent", "codex-auth-broker/"+valueOr(version, "dev"))
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return s.proxy.client.Do(req)
}

func (s *cursorShim) handleBidiAppend(w http.ResponseWriter, codec connectCodec, body []byte) {
	requestID, data := cursorBidiAppend(codec, body)
	if requestID != "" && len(data) > 0 {
		s.runs.get(requestID).push(data)
	}
	w.Header().Set("Content-Type", connectUnaryContentType(codec))
	w.Header().Set("Connect-Protocol-Version", "1")
	if codec == connectJSON {
		_, _ = w.Write([]byte("{}"))
	}
}

func (s *cursorShim) writeCursorFrame(w http.ResponseWriter, flusher http.Flusher, codec connectCodec, payload []byte) {
	_, _ = w.Write(encodeConnectFrame(payload, 0))
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *cursorShim) defaultModel() string {
	if len(s.proxy.cfg.models) > 0 {
		return s.proxy.cfg.models[0]
	}
	return "local"
}

func (s *cursorShim) baseURL() string {
	listen := strings.TrimSpace(s.proxy.cfg.cursorListen)
	if listen == "" {
		listen = defaultCursorListen
	}
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	return "http://" + listen
}

func cursorEmptyUnary(path string) bool {
	for _, part := range []string{"HealthCheck", "PrivacyCheck", "ServerTime", "BootstrapStatsig", "DashboardService", "AnalyticsService", "Report", "Track", "Submit", "UpdateConversationMetadata", "UploadConversationBlobs", "NotifyConversationClone", "GetAllowedModelIntents", "GetNewChatNudge", "ModelLabels"} {
		if strings.Contains(path, part) {
			return true
		}
	}
	return false
}

func cursorModelDetails(name string) []byte {
	return pbConcat(pbString(1, name), pbString(3, name), pbString(4, "Local"), pbString(5, "Local"))
}

func firstCursorPayload(codec connectCodec, body []byte) []byte {
	frames := decodeConnectFrames(body)
	if len(frames) > 0 {
		body = frames[0].data
	}
	return body
}

func cursorRequestID(codec connectCodec, data []byte) string {
	if codec == connectJSON {
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			return firstString(m, "requestId", "request_id")
		}
		return ""
	}
	return pbStringField(data, 1)
}

func cursorConversationID(codec connectCodec, data []byte) string {
	if codec == connectJSON {
		var value any
		if json.Unmarshal(data, &value) == nil {
			var found string
			walkJSONStrings(value, func(key, text string) {
				lk := strings.ToLower(key)
				if found == "" && (lk == "conversation_id" || lk == "conversationid") {
					found = text
				}
			})
			if found != "" {
				return found
			}
		}
		return "local"
	}
	if runRequest := firstPBBytes(data, 1); len(runRequest) > 0 {
		if id := pbStringField(runRequest, 5); id != "" {
			return id
		}
	}
	if id := pbStringField(data, 5); id != "" {
		return id
	}
	return "local"
}

func isCursorInitial(codec connectCodec, data []byte) bool {
	if codec == connectJSON {
		return strings.Contains(string(data), "stream_unified_chat_request") || strings.Contains(string(data), "streamUnifiedChatRequest")
	}
	first := firstPBBytes(data, 1)
	return len(first) > 0 && !isMostlyPrintable(first)
}

func cursorPrompt(codec connectCodec, data []byte) string {
	if codec == connectJSON {
		var value any
		if json.Unmarshal(data, &value) == nil {
			var found []string
			walkJSONStrings(value, func(key, text string) {
				if (key == "text" || key == "prompt" || key == "content" || key == "rich_text" || key == "richText") && strings.TrimSpace(text) != "" {
					found = append(found, text)
				}
			})
			if len(found) > 0 {
				return found[len(found)-1]
			}
		}
		return "hello"
	}
	if prompt := cursorPromptFromAgentClientMessage(data); prompt != "" {
		return prompt
	}
	request := firstPBBytes(data, 1)
	if len(request) == 0 {
		request = data
	}
	var turns []string
	for _, msg := range pbBytes(request, 1) {
		if text := pbStringField(msg, 1); strings.TrimSpace(text) != "" && isProtoTextCandidate(text) {
			turns = append(turns, text)
		}
	}
	if len(turns) > 0 {
		return turns[len(turns)-1]
	}
	candidates := protoStrings(data, 0)
	sort.SliceStable(candidates, func(i, j int) bool { return cursorPromptScore(candidates[i]) > cursorPromptScore(candidates[j]) })
	if len(candidates) > 0 {
		return candidates[0]
	}
	return "hello"
}

func cursorPromptFromAgentClientMessage(data []byte) string {
	if runRequest := firstPBBytes(data, 1); len(runRequest) > 0 {
		if prompt := cursorPromptFromRunRequest(runRequest); prompt != "" {
			return prompt
		}
	}
	if action := firstPBBytes(data, 4); len(action) > 0 {
		if prompt := cursorPromptFromConversationAction(action); prompt != "" {
			return prompt
		}
	}
	return cursorPromptFromRunRequest(data)
}

func cursorPromptFromRunRequest(runRequest []byte) string {
	if action := firstPBBytes(runRequest, 2); len(action) > 0 {
		if prompt := cursorPromptFromConversationAction(action); prompt != "" {
			return prompt
		}
	}
	return ""
}

func cursorPromptFromConversationAction(action []byte) string {
	userAction := firstPBBytes(action, 1)
	if len(userAction) == 0 {
		return ""
	}
	userMessage := firstPBBytes(userAction, 1)
	if len(userMessage) == 0 {
		return ""
	}
	for _, fieldNo := range []int{1, 8} {
		text := strings.TrimSpace(pbStringField(userMessage, fieldNo))
		if text != "" && isProtoTextCandidate(text) {
			return text
		}
	}
	return ""
}

func cursorHistoryFromClient(codec connectCodec, data []byte) []cursorHistoryMessage {
	if codec == connectJSON {
		return nil
	}
	var runRequest []byte
	if candidate := firstPBBytes(data, 1); len(candidate) > 0 {
		runRequest = candidate
	} else {
		runRequest = data
	}
	state := firstPBBytes(runRequest, 1)
	if len(state) == 0 {
		return nil
	}
	var out []cursorHistoryMessage
	for _, turn := range pbBytes(state, 8) {
		agentTurn := firstPBBytes(turn, 1)
		if len(agentTurn) == 0 {
			continue
		}
		if text := cursorUserMessageText(firstPBBytes(agentTurn, 1)); text != "" {
			out = append(out, cursorHistoryMessage{Role: "user", Content: text})
		}
		for _, step := range pbBytes(agentTurn, 2) {
			if assistant := firstPBBytes(step, 1); len(assistant) > 0 {
				if text := strings.TrimSpace(pbStringField(assistant, 1)); text != "" && isProtoTextCandidate(text) {
					out = append(out, cursorHistoryMessage{Role: "assistant", Content: text})
				}
			}
		}
	}
	return out
}

func cursorUserMessageText(userMessage []byte) string {
	if len(userMessage) == 0 {
		return ""
	}
	for _, fieldNo := range []int{1, 8} {
		text := strings.TrimSpace(pbStringField(userMessage, fieldNo))
		if text != "" && isProtoTextCandidate(text) {
			return text
		}
	}
	return ""
}

func cursorModel(codec connectCodec, data []byte, fallback string) string {
	if codec == connectJSON {
		var value any
		if json.Unmarshal(data, &value) == nil {
			var found string
			walkJSONStrings(value, func(key, text string) {
				if found == "" && strings.Contains(strings.ToLower(key), "model") {
					found = text
				}
			})
			if found != "" {
				return found
			}
		}
		return fallback
	}
	request := firstPBBytes(data, 1)
	if details := firstPBBytes(request, 5); len(details) > 0 {
		if model := pbStringField(details, 1); model != "" {
			return model
		}
	}
	return fallback
}

func cursorStartFrame(codec connectCodec) []byte {
	if codec == connectJSON {
		return mustJSON(map[string]any{"response": map[string]any{"case": "stream_start", "value": map[string]any{}}})
	}
	return pbMessage(1, pbMessage(13, nil))
}

func cursorTextFrame(codec connectCodec, text string) []byte {
	if codec == connectJSON {
		return mustJSON(map[string]any{"response": map[string]any{"case": "stream_unified_chat_response", "value": map[string]any{"text": text}}})
	}
	return pbMessage(1, pbMessage(1, pbString(1, text)))
}

func cursorSummaryFrame(codec connectCodec) []byte {
	if codec == connectJSON {
		return mustJSON(map[string]any{"response": map[string]any{"case": "conversation_summary", "value": map[string]any{}}})
	}
	return pbMessage(1, pbMessage(14, nil))
}

func cursorExecFrame(codec connectCodec, call *cursorToolCall) []byte {
	if codec == connectJSON {
		return mustJSON(map[string]any{"execServerMessage": map[string]any{"id": call.ExecID, "execId": call.CallID, "kind": call.Kind, "args": call.Args}})
	}
	exec := pbConcat(pbInt(1, call.ExecID), pbMessage(cursorExecArgField(call.Kind), cursorExecArgs(call)), pbString(15, call.CallID))
	return pbMessage(2, exec)
}

func cursorToolCallUpdateFrame(codec connectCodec, updateField int, call *cursorToolCall, rawResult []byte) []byte {
	if codec == connectJSON {
		name := "tool_call_started"
		if updateField == 3 {
			name = "tool_call_completed"
		}
		return mustJSON(map[string]any{"response": map[string]any{"case": name, "value": map[string]any{"call_id": call.CallID, "tool_call": map[string]any{"name": call.Name, "args": call.Args}}}})
	}
	toolCall := cursorToolCallProto(call, rawResult)
	update := pbConcat(pbString(1, call.CallID), pbMessage(2, toolCall), pbString(3, call.CallID))
	return pbMessage(1, pbMessage(updateField, update))
}

func cursorExecArgField(kind string) int {
	switch kind {
	case "shell":
		return 2
	case "write":
		return 3
	case "delete":
		return 4
	case "grep":
		return 5
	case "read":
		return 7
	case "list_dir":
		return 8
	default:
		return 8
	}
}

func cursorExecResultField(kind string) int {
	switch kind {
	case "shell":
		return 2
	case "write":
		return 3
	case "delete":
		return 4
	case "grep":
		return 5
	case "read":
		return 7
	case "list_dir":
		return 8
	default:
		return 8
	}
}

func cursorToolCallField(kind string) int {
	switch kind {
	case "shell":
		return 1
	case "delete":
		return 3
	case "grep":
		return 5
	case "read":
		return 8
	case "write":
		return 12
	case "list_dir":
		return 13
	default:
		return 13
	}
}

func cursorExecArgs(call *cursorToolCall) []byte {
	args := cursorCallArgs(call.Args)
	switch call.Kind {
	case "shell":
		command := cursorArg(args, "command")
		parsingResult := pbConcat(pbInt(1, 0), pbInt(3, 0), pbInt(4, 0))
		body := pbConcat(pbString(1, command), pbString(2, cursorArg(args, "working_directory", "cwd")), pbInt(3, uint64(cursorArgInt(args, 120000, "timeout", "timeout_ms"))), pbString(4, call.CallID), pbString(5, command), pbMessage(8, parsingResult))
		if desc := cursorArg(args, "description"); desc != "" {
			body = pbConcat(body, pbString(15, desc))
		}
		return body
	case "write":
		return pbConcat(pbString(1, call.Path), pbString(2, cursorArg(args, "content", "file_text", "text")), pbString(3, call.CallID), pbInt(4, 1))
	case "delete":
		return pbConcat(pbString(1, call.Path), pbString(2, call.CallID))
	case "grep":
		body := pbConcat(pbString(1, cursorArg(args, "pattern", "query")), pbString(14, call.CallID))
		if path := cursorArg(args, "path", "directory", "target_directory"); path != "" {
			body = pbConcat(body, pbString(2, path))
		}
		if glob := cursorArg(args, "glob", "include"); glob != "" {
			body = pbConcat(body, pbString(3, glob))
		}
		body = pbConcat(body, pbString(4, valueOr(cursorArg(args, "output_mode"), "content")))
		if limit := cursorArgInt(args, 0, "head_limit", "limit"); limit > 0 {
			body = pbConcat(body, pbInt(10, uint64(limit)))
		}
		return body
	case "read":
		body := pbConcat(pbString(1, call.Path), pbString(2, call.CallID))
		if offset := cursorArgInt(args, 0, "offset"); offset > 0 {
			body = pbConcat(body, pbInt(4, uint64(offset)))
		}
		if limit := cursorArgInt(args, 0, "limit"); limit > 0 {
			body = pbConcat(body, pbInt(5, uint64(limit)))
		}
		return body
	case "list_dir":
		return pbConcat(pbString(1, call.Path), pbString(3, call.CallID))
	default:
		return pbConcat(pbString(1, call.Path), pbString(3, call.CallID))
	}
}

func cursorToolCallProto(call *cursorToolCall, rawResult []byte) []byte {
	body := pbMessage(1, cursorToolArgsForUI(call))
	if len(rawResult) > 0 {
		body = pbConcat(body, pbMessage(2, rawResult))
	}
	return pbMessage(cursorToolCallField(call.Kind), body)
}

func cursorToolArgsForUI(call *cursorToolCall) []byte {
	if call.Kind == "write" {
		args := cursorCallArgs(call.Args)
		return pbConcat(pbString(1, call.Path), pbString(6, cursorArg(args, "content", "file_text", "text")))
	}
	if call.Kind == "read" {
		args := cursorCallArgs(call.Args)
		body := pbString(1, call.Path)
		if offset := cursorArgInt(args, 0, "offset"); offset > 0 {
			body = pbConcat(body, pbInt(2, uint64(offset)))
		}
		if limit := cursorArgInt(args, 0, "limit"); limit > 0 {
			body = pbConcat(body, pbInt(3, uint64(limit)))
		}
		return body
	}
	return cursorExecArgs(call)
}

type cursorToolResultData struct {
	CallID      string
	Output      string
	RawUIResult []byte
}

func cursorToolResult(codec connectCodec, data []byte, call *cursorToolCall) cursorToolResultData {
	if codec == connectJSON {
		var value any
		_ = json.Unmarshal(data, &value)
		var callID string
		walkJSONStrings(value, func(key, text string) {
			lk := strings.ToLower(key)
			if callID == "" && (strings.Contains(lk, "tool_call_id") || strings.Contains(lk, "toolcallid")) {
				callID = text
			}
		})
		return cursorToolResultData{CallID: callID, Output: string(data)}
	}
	execClient := firstPBBytes(data, 2)
	if len(execClient) == 0 {
		execClient = data
	}
	if call == nil {
		call = &cursorToolCall{Kind: "list_dir"}
	}
	result := firstPBBytes(execClient, cursorExecResultField(call.Kind))
	if len(result) == 0 {
		return cursorToolResultData{CallID: pbStringField(execClient, 15), Output: strings.Join(protoStrings(execClient, 0), "\n")}
	}
	parsed := cursorParseToolResult(call, result)
	parsed.CallID = valueOr(pbStringField(execClient, 15), call.CallID)
	return parsed
}

func cursorParseToolResult(call *cursorToolCall, result []byte) cursorToolResultData {
	switch call.Kind {
	case "list_dir":
		return cursorParseLsResult(result)
	case "read":
		return cursorParseReadResult(result)
	case "grep":
		return cursorToolResultData{Output: valueOr(strings.Join(protoStrings(result, 0), "\n"), "grep completed"), RawUIResult: result}
	case "shell":
		return cursorParseShellResult(result)
	case "write":
		return cursorParseWriteResult(call, result)
	case "delete":
		return cursorToolResultData{Output: valueOr(strings.Join(protoStrings(result, 0), "\n"), "delete completed"), RawUIResult: result}
	default:
		return cursorToolResultData{Output: strings.Join(protoStrings(result, 0), "\n"), RawUIResult: result}
	}
}

func cursorParseLsResult(lsResult []byte) cursorToolResultData {
	if errMsg := firstPBBytes(lsResult, 2); len(errMsg) > 0 {
		return cursorToolResultData{Output: "ERROR: " + strings.Join(protoStrings(errMsg, 0), "\n"), RawUIResult: lsResult}
	}
	success := firstPBBytes(lsResult, 1)
	if len(success) == 0 {
		return cursorToolResultData{Output: strings.Join(protoStrings(lsResult, 0), "\n"), RawUIResult: lsResult}
	}
	root := firstPBBytes(success, 1)
	var entries []string
	collectLsNode(root, "", &entries)
	if len(entries) == 0 {
		entries = protoStrings(success, 0)
	}
	return cursorToolResultData{Output: strings.Join(entries, "\n"), RawUIResult: lsResult}
}

func cursorParseReadResult(result []byte) cursorToolResultData {
	success := firstPBBytes(result, 1)
	if len(success) == 0 {
		msg := valueOr(strings.Join(protoStrings(result, 0), "\n"), "read failed")
		return cursorToolResultData{Output: msg, RawUIResult: pbMessage(2, pbString(1, msg))}
	}
	content := pbStringField(success, 2)
	if content == "" {
		if data := firstPBBytes(success, 5); len(data) > 0 && utf8.Valid(data) {
			content = string(data)
		}
	}
	ui := pbConcat(pbString(1, content), pbString(7, pbStringField(success, 1)))
	if total, ok := pbVarintField(success, 3); ok {
		ui = pbConcat(ui, pbInt(4, total))
	}
	if size, ok := pbVarintField(success, 4); ok {
		ui = pbConcat(ui, pbInt(5, size))
	}
	return cursorToolResultData{Output: content, RawUIResult: pbMessage(1, ui)}
}

func cursorParseShellResult(result []byte) cursorToolResultData {
	msg := firstPBBytes(result, 1)
	if len(msg) == 0 {
		msg = firstPBBytes(result, 2)
	}
	if len(msg) == 0 {
		return cursorToolResultData{Output: valueOr(strings.Join(protoStrings(result, 0), "\n"), "shell command completed"), RawUIResult: result}
	}
	parts := []string{}
	if out := pbStringField(msg, 10); out != "" {
		parts = append(parts, out)
	} else {
		if out := pbStringField(msg, 5); out != "" {
			parts = append(parts, out)
		}
		if errText := pbStringField(msg, 6); errText != "" {
			parts = append(parts, errText)
		}
	}
	if len(parts) == 0 {
		parts = protoStrings(msg, 0)
	}
	return cursorToolResultData{Output: strings.Join(parts, "\n"), RawUIResult: result}
}

func cursorParseWriteResult(call *cursorToolCall, result []byte) cursorToolResultData {
	success := firstPBBytes(result, 1)
	if len(success) == 0 {
		return cursorToolResultData{Output: valueOr(strings.Join(protoStrings(result, 0), "\n"), "write failed"), RawUIResult: cursorEditErrorResult(call, result)}
	}
	path := valueOr(pbStringField(success, 1), call.Path)
	content := pbStringField(success, 4)
	message := "Wrote " + path
	editSuccess := pbConcat(pbString(1, path), pbString(7, content), pbString(8, message))
	return cursorToolResultData{Output: message, RawUIResult: pbMessage(1, editSuccess)}
}

func cursorEditErrorResult(call *cursorToolCall, result []byte) []byte {
	msg := valueOr(strings.Join(protoStrings(result, 0), "\n"), "write failed")
	return pbMessage(7, pbConcat(pbString(1, call.Path), pbString(2, msg), pbString(5, msg)))
}

func collectLsNode(node []byte, prefix string, entries *[]string) {
	if len(node) == 0 {
		return
	}
	base := pbStringField(node, 1)
	if prefix == "" && base != "" {
		prefix = base
	}
	for _, dir := range pbBytes(node, 2) {
		name := pbStringField(dir, 1)
		if name != "" {
			*entries = append(*entries, "[dir] "+name)
		}
		collectLsNode(dir, name, entries)
	}
	for _, file := range pbBytes(node, 3) {
		name := pbStringField(file, 1)
		if name == "" {
			ss := protoStrings(file, 0)
			if len(ss) > 0 {
				name = ss[0]
			}
		}
		if name != "" {
			*entries = append(*entries, name)
		}
	}
}

func cursorBidiAppend(codec connectCodec, body []byte) (string, []byte) {
	if codec == connectJSON {
		var m map[string]any
		if json.Unmarshal(body, &m) != nil {
			return "", nil
		}
		requestID := firstString(m, "requestId", "request_id")
		if nested, ok := m["requestId"].(map[string]any); ok && requestID == "" {
			requestID = firstString(nested, "requestId", "request_id")
		}
		if nested, ok := m["request_id"].(map[string]any); ok && requestID == "" {
			requestID = firstString(nested, "requestId", "request_id")
		}
		switch data := m["data"].(type) {
		case string:
			decoded, err := hex.DecodeString(data)
			if err == nil {
				return requestID, decoded
			}
			return requestID, []byte(data)
		case map[string]any:
			return requestID, mustJSON(data)
		default:
			return requestID, nil
		}
	}
	requestIDMsg := firstPBBytes(body, 2)
	requestID := pbStringField(requestIDMsg, 1)
	dataValues := pbBytes(body, 1)
	if len(dataValues) == 0 {
		return requestID, nil
	}
	dataHex := string(dataValues[0])
	decoded, err := hex.DecodeString(dataHex)
	if err == nil {
		return requestID, decoded
	}
	return requestID, dataValues[0]
}

func cursorResponseTools() []any {
	return []any{
		cursorFunctionTool("list_dir", "List files and directories in a local directory.", map[string]any{"path": map[string]any{"type": "string"}}, []string{"path"}),
		cursorFunctionTool("read_file", "Read a local file, optionally with offset and line limit.", map[string]any{"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"}, "limit": map[string]any{"type": "integer"}}, []string{"path"}),
		cursorFunctionTool("write_file", "Write or replace a local file with complete content.", map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, []string{"path", "content"}),
		cursorFunctionTool("delete_file", "Delete a local file.", map[string]any{"path": map[string]any{"type": "string"}}, []string{"path"}),
		cursorFunctionTool("grep", "Search files with ripgrep-compatible pattern matching.", map[string]any{"pattern": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}, "glob": map[string]any{"type": "string"}, "head_limit": map[string]any{"type": "integer"}}, []string{"pattern"}),
		cursorFunctionTool("run_shell", "Run a shell command in the local workspace through Cursor's harness.", map[string]any{"command": map[string]any{"type": "string"}, "working_directory": map[string]any{"type": "string"}, "timeout_ms": map[string]any{"type": "integer"}, "description": map[string]any{"type": "string"}}, []string{"command"}),
	}
}

func cursorFunctionTool(name, description string, properties map[string]any, required []string) map[string]any {
	return map[string]any{"type": "function", "name": name, "description": description, "parameters": map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
}

func cursorCallArgs(raw string) map[string]any {
	var parsed map[string]any
	if json.Unmarshal([]byte(raw), &parsed) == nil && parsed != nil {
		return parsed
	}
	return map[string]any{}
}

func cursorArg(args map[string]any, keys ...string) string {
	return firstString(args, keys...)
}

func cursorArgInt(args map[string]any, fallback int, keys ...string) int {
	for _, key := range keys {
		switch v := args[key].(type) {
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		case float64:
			return int(v)
		case int:
			return v
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return fallback
}

func readResponsesEvents(r io.Reader) func(func(responsesEvent, error) bool) {
	return func(yield func(responsesEvent, error) bool) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 64*1024), maxRequestBodyBytes)
		var name string
		var dataLines []string
		flush := func() bool {
			if len(dataLines) == 0 {
				name = ""
				return true
			}
			data := strings.TrimSpace(strings.Join(dataLines, "\n"))
			dataLines = nil
			eventName := name
			name = ""
			if data == "" || data == "[DONE]" {
				return true
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				return yield(responsesEvent{}, err)
			}
			if eventName == "" {
				eventName = stringField(parsed, "type")
			}
			return yield(responsesEvent{Name: eventName, Data: parsed}, nil)
		}
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			if line == "" {
				if !flush() {
					return
				}
				continue
			}
			if strings.HasPrefix(line, "event:") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err := scanner.Err(); err != nil {
			yield(responsesEvent{}, err)
			return
		}
		if !flush() {
			return
		}
	}
}

func outputTextDelta(event responsesEvent) string {
	if event.Name == "response.output_text.delta" {
		if delta, ok := event.Data["delta"].(string); ok {
			return delta
		}
	}
	return ""
}

func functionCallAdded(event responsesEvent) *pendingFunctionCall {
	if event.Name != "response.output_item.added" {
		return nil
	}
	item, _ := event.Data["item"].(map[string]any)
	if stringField(item, "type") != "function_call" {
		return nil
	}
	key := firstString(item, "id", "call_id")
	if key == "" {
		key = fmt.Sprint(event.Data["output_index"])
	}
	return &pendingFunctionCall{Key: key, CallID: firstString(item, "call_id", "id"), Name: stringField(item, "name"), Args: stringField(item, "arguments")}
}

func functionArgumentsDelta(event responsesEvent) (string, string) {
	if event.Name != "response.function_call_arguments.delta" && event.Name != "response.function_call.arguments.delta" {
		return "", ""
	}
	key := firstString(event.Data, "item_id", "output_index")
	return key, stringField(event.Data, "delta")
}

func functionCallDone(event responsesEvent, calls map[string]*pendingFunctionCall) *pendingFunctionCall {
	if event.Name == "response.output_item.done" {
		item, _ := event.Data["item"].(map[string]any)
		if stringField(item, "type") == "function_call" {
			return &pendingFunctionCall{CallID: firstString(item, "call_id", "id"), Name: stringField(item, "name"), Args: stringField(item, "arguments")}
		}
	}
	if event.Name == "response.function_call_arguments.done" || event.Name == "response.function_call.arguments.done" {
		key := firstString(event.Data, "item_id", "output_index")
		if call := calls[key]; call != nil {
			if args := stringField(event.Data, "arguments"); args != "" {
				call.Args = args
			}
			return call
		}
	}
	return nil
}

func cursorToolCallDone(event responsesEvent, calls map[string]*pendingFunctionCall) *cursorToolCall {
	call := functionCallDone(event, calls)
	if call == nil {
		return nil
	}
	tool := &cursorToolCall{CallID: call.CallID, Name: call.Name, Args: call.Args}
	args := cursorCallArgs(call.Args)
	switch call.Name {
	case "list_dir":
		tool.Kind = "list_dir"
		tool.Path = valueOr(cursorArg(args, "path", "directory_path", "target_directory"), ".")
	case "read_file":
		tool.Kind = "read"
		tool.Path = cursorArg(args, "path", "file_path")
	case "write_file":
		tool.Kind = "write"
		tool.Path = cursorArg(args, "path", "file_path")
	case "delete_file":
		tool.Kind = "delete"
		tool.Path = cursorArg(args, "path", "file_path")
	case "grep":
		tool.Kind = "grep"
		tool.Path = cursorArg(args, "path", "directory", "target_directory")
	case "run_shell":
		tool.Kind = "shell"
		tool.Path = cursorArg(args, "working_directory", "cwd")
	default:
		return nil
	}
	return tool
}

func (s *cursorRunStore) get(id string) *cursorRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[id]
	if run == nil {
		run = &cursorRun{id: id}
		s.runs[id] = run
	}
	return run
}

func (s *cursorRunStore) close(id string) {
	s.mu.Lock()
	run := s.runs[id]
	delete(s.runs, id)
	s.mu.Unlock()
	if run != nil {
		run.close()
	}
}

func (r *cursorRun) push(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.waiters) > 0 {
		ch := r.waiters[0]
		r.waiters = r.waiters[1:]
		ch <- data
		return
	}
	r.queue = append(r.queue, data)
}

func (r *cursorRun) next(timeout time.Duration) ([]byte, error) {
	r.mu.Lock()
	if len(r.queue) > 0 {
		data := r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()
		return data, nil
	}
	ch := make(chan []byte, 1)
	r.waiters = append(r.waiters, ch)
	r.mu.Unlock()
	select {
	case data, ok := <-ch:
		if !ok {
			return nil, errors.New("run closed")
		}
		return data, nil
	case <-time.After(timeout):
		return nil, errors.New("timed out waiting for BidiAppend")
	}
}

func (r *cursorRun) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, waiter := range r.waiters {
		close(waiter)
	}
	r.waiters = nil
}

func firstPBBytes(data []byte, no int) []byte {
	values := pbBytes(data, no)
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		switch v := m[key].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case json.Number:
			return v.String()
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}

func walkJSONStrings(value any, visit func(key, text string), key ...string) {
	current := ""
	if len(key) > 0 {
		current = key[0]
	}
	switch v := value.(type) {
	case string:
		visit(current, v)
	case []any:
		for _, item := range v {
			walkJSONStrings(item, visit, current)
		}
	case map[string]any:
		for k, item := range v {
			walkJSONStrings(item, visit, k)
		}
	}
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func mustJSONString(value any) string {
	return string(mustJSON(value))
}

func cursorFakeToken() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := mustJSON(map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix()})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func isMostlyPrintable(data []byte) bool {
	if len(data) == 0 || !utf8.Valid(data) {
		return false
	}
	printable := 0
	for _, r := range string(data) {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 0x20 && r < 0x7f) {
			printable++
		}
	}
	return printable*100/len([]rune(string(data))) > 90
}

func cursorPromptScore(s string) int {
	score := len(s)
	if strings.Contains(s, " ") {
		score += 100
	}
	if strings.Contains(s, "/") {
		score -= 30
	}
	return score
}

func stringsContains(s, substr string) bool { return strings.Contains(s, substr) }
func stringsTrimSpace(s string) string      { return strings.TrimSpace(s) }
func hasPromptChar(s string) bool {
	return strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
}
