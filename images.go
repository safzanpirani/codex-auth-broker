package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultImageModel           = "gpt-image-2"
	imageResponsesModel         = "gpt-5.6-sol"
	imageGenerationInstructions = "You are an image generation assistant."
	maxImageCount               = 4
	maxImageSSEBytes            = 64 * 1024 * 1024
	maxImageSSEEventBytes       = 64 * 1024 * 1024
	maxImageSSEEvents           = 512
	maxImageBase64Chars         = 64 * 1024 * 1024
)

type imageGenerationRequest struct {
	Model             string
	Prompt            string
	Count             int
	Size              string
	Quality           string
	OutputFormat      string
	Background        string
	Moderation        string
	OutputCompression *int
}

type generatedImage struct {
	Base64        string `json:"b64_json"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type imageGenerationResult struct {
	Image generatedImage
	Usage map[string]any
}

func (p *responsesProxy) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	logEntry := p.beginRequestLog(r)
	defer logEntry.finish()
	if !p.authorizedClient(r) {
		logEntry.markError(http.StatusUnauthorized, "unauthorized")
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	body, err := decodeRequestBody(r.Body, r.Header.Get("Content-Encoding"))
	if err != nil {
		logEntry.markError(http.StatusBadRequest, err.Error())
		writeProxyError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := parseImageGenerationRequest(body)
	if err != nil {
		logEntry.markError(http.StatusBadRequest, err.Error())
		writeProxyError(w, http.StatusBadRequest, err.Error())
		return
	}
	logEntry.markImageRequest(req.Model, req.Count)

	results := make([]imageGenerationResult, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		result, fail := p.generateOneImage(r, req, i, logEntry)
		if fail != nil {
			p.writeDispatchFailure(w, logEntry, fail)
			return
		}
		results = append(results, result)
	}

	data := make([]generatedImage, 0, len(results))
	usages := make([]map[string]any, 0, len(results))
	for _, result := range results {
		data = append(data, result.Image)
		if result.Usage != nil {
			usages = append(usages, result.Usage)
		}
	}
	response := map[string]any{"created": time.Now().Unix(), "data": data}
	if usage := aggregateImageUsage(usages); usage != nil {
		response["usage"] = usage
		logEntry.markUsage(extractTokenUsage(map[string]any{"usage": usage}))
	}
	logEntry.markStatus(http.StatusOK)
	writeJSON(w, http.StatusOK, response)
}

func parseImageGenerationRequest(body map[string]any) (imageGenerationRequest, error) {
	req := imageGenerationRequest{Model: defaultImageModel, Count: 1}
	if value, ok := body["model"]; ok {
		model, ok := value.(string)
		if !ok || strings.TrimSpace(model) == "" {
			return req, errors.New("model must be a non-empty string")
		}
		req.Model = strings.TrimSpace(model)
	}
	prompt, ok := body["prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return req, errors.New("prompt must be a non-empty string")
	}
	req.Prompt = prompt
	if value, ok := body["n"]; ok {
		n, ok := exactJSONInt(value)
		if !ok || n < 1 || n > maxImageCount {
			return req, fmt.Errorf("n must be an integer between 1 and %d", maxImageCount)
		}
		req.Count = n
	}
	var err error
	if req.Size, err = optionalImageSize(body); err != nil {
		return req, err
	}
	if req.Quality, err = optionalImageEnum(body, "quality", "auto", "low", "medium", "high"); err != nil {
		return req, err
	}
	if req.OutputFormat, err = optionalImageEnum(body, "output_format", "png", "jpeg", "webp"); err != nil {
		return req, err
	}
	if req.Background, err = optionalImageEnum(body, "background", "auto", "opaque", "transparent"); err != nil {
		return req, err
	}
	if req.Moderation, err = optionalImageEnum(body, "moderation", "auto", "low"); err != nil {
		return req, err
	}
	if value, ok := body["output_compression"]; ok {
		compression, valid := exactJSONInt(value)
		if !valid || compression < 0 || compression > 100 {
			return req, errors.New("output_compression must be an integer between 0 and 100")
		}
		req.OutputCompression = &compression
	}
	if value, ok := body["user"]; ok {
		if _, valid := value.(string); !valid {
			return req, errors.New("user must be a string")
		}
	}
	if req.Background == "transparent" && req.OutputFormat == "jpeg" {
		return req, errors.New("transparent background requires png or webp output_format")
	}
	// The public Images API accepts user, but this broker deliberately does not
	// forward it: arbitrary end-user identifiers are not known-safe on the
	// private ChatGPT Codex transport. It remains absent from request logs too.
	return req, nil
}

func optionalImageSize(body map[string]any) (string, error) {
	value, exists := body["size"]
	if !exists {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", errors.New("size must be a string")
	}
	if text == "auto" {
		return text, nil
	}
	parts := strings.Split(text, "x")
	if len(parts) != 2 {
		return "", fmt.Errorf("unsupported size %q", text)
	}
	width, widthErr := strconv.Atoi(parts[0])
	height, heightErr := strconv.Atoi(parts[1])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 ||
		width > 3840 || height > 3840 || width%16 != 0 || height%16 != 0 {
		return "", fmt.Errorf("unsupported size %q", text)
	}
	shortEdge, longEdge := width, height
	if shortEdge > longEdge {
		shortEdge, longEdge = longEdge, shortEdge
	}
	pixels := int64(width) * int64(height)
	if longEdge > 3*shortEdge || pixels < 655_360 || pixels > 8_294_400 {
		return "", fmt.Errorf("unsupported size %q", text)
	}
	return text, nil
}

func exactJSONInt(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	if err != nil || int64(int(parsed)) != parsed {
		return 0, false
	}
	return int(parsed), true
}

func optionalImageEnum(body map[string]any, key string, allowed ...string) (string, error) {
	value, exists := body[key]
	if !exists {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	for _, candidate := range allowed {
		if text == candidate {
			return text, nil
		}
	}
	return "", fmt.Errorf("unsupported %s %q", key, text)
}

func buildImageResponsesBody(req imageGenerationRequest) map[string]any {
	tool := map[string]any{"type": "image_generation", "model": req.Model}
	for key, value := range map[string]string{
		"size": req.Size, "quality": req.Quality, "output_format": req.OutputFormat,
		"background": req.Background, "moderation": req.Moderation,
	} {
		if value != "" {
			tool[key] = value
		}
	}
	if req.OutputCompression != nil && (req.OutputFormat == "jpeg" || req.OutputFormat == "webp") {
		tool["output_compression"] = *req.OutputCompression
	}
	return map[string]any{
		"model":        imageResponsesModel,
		"instructions": imageGenerationInstructions,
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": req.Prompt}},
		}},
		"tools":       []any{tool},
		"tool_choice": map[string]any{"type": "image_generation"},
		"stream":      true,
		"store":       false,
	}
}

func (p *responsesProxy) generateOneImage(r *http.Request, imageReq imageGenerationRequest, index int, logEntry *pendingRequestLog) (imageGenerationResult, *dispatchFailure) {
	body := buildImageResponsesBody(imageReq)
	encoded, err := json.Marshal(body)
	if err != nil {
		return imageGenerationResult{}, &dispatchFailure{status: http.StatusBadRequest, message: "invalid image request body"}
	}
	release, fail := p.acquireUpstreamSlot(r.Context())
	if fail != nil {
		return imageGenerationResult{}, fail
	}
	defer release()
	info := requestInfo{Model: imageResponsesModel, NormalizedModel: imageResponsesModel, Stream: true}
	upstreamRequest := imageUpstreamRequest(r, index)
	resp, fail := p.dispatchUpstream(r.Context(), encoded, info, body, upstreamRequest)
	if fail != nil {
		return imageGenerationResult{}, fail
	}
	defer resp.Body.Close()
	logEntry.markUpstreamStatus(resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return imageGenerationResult{}, &dispatchFailure{status: resp.StatusCode, message: "upstream image generation failed", body: responseBody}
	}
	result, err := parseImageGenerationSSE(resp.Body)
	if err != nil {
		return imageGenerationResult{}, &dispatchFailure{status: http.StatusBadGateway, message: "invalid upstream image response: " + err.Error()}
	}
	return result, nil
}

func imageUpstreamRequest(r *http.Request, index int) *http.Request {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	requestID := ""
	for _, key := range []string{"x-client-request-id", "x-request-id", "session_id"} {
		if requestID == "" {
			requestID = strings.TrimSpace(clone.Header.Get(key))
		}
		clone.Header.Del(key)
	}
	if requestID != "" {
		clone.Header.Set("x-client-request-id", fmt.Sprintf("%s-image-%d", requestID, index+1))
	}
	return clone
}

func parseImageGenerationSSE(reader io.Reader) (imageGenerationResult, error) {
	limited := &io.LimitedReader{R: reader, N: maxImageSSEBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64*1024), maxImageSSEEventBytes)
	var dataLines []string
	var doneItems []map[string]any
	var completed map[string]any
	events := 0
	terminal := false
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if data == "" || data == "[DONE]" {
			return nil
		}
		events++
		if events > maxImageSSEEvents {
			return errors.New("response exceeded event limit")
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return errors.New("malformed SSE event")
		}
		typeName, ok := event["type"].(string)
		if !ok || typeName == "" {
			return errors.New("SSE event missing type")
		}
		switch typeName {
		case "response.failed", "error":
			return errors.New(imageGenerationEventError(event, "image generation failed"))
		case "response.incomplete":
			reason := nestedString(event, "response", "incomplete_details", "reason")
			if reason == "" {
				reason = "unknown"
			}
			return fmt.Errorf("image generation response incomplete: %s", reason)
		case "response.completed":
			response, ok := event["response"].(map[string]any)
			if !ok {
				return errors.New("response.completed missing response")
			}
			completed = response
			terminal = true
		case "response.output_item.done":
			if item, ok := event["item"].(map[string]any); ok && stringField(item, "type") == "image_generation_call" {
				doneItems = append(doneItems, item)
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return imageGenerationResult{}, err
			}
			if terminal {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return imageGenerationResult{}, err
	}
	if limited.N <= 0 {
		return imageGenerationResult{}, errors.New("response exceeded size limit")
	}
	if !terminal {
		if err := flush(); err != nil {
			return imageGenerationResult{}, err
		}
	}
	if completed == nil {
		return imageGenerationResult{}, errors.New("stream closed before response.completed")
	}
	if status := stringField(completed, "status"); status != "" && status != "completed" {
		return imageGenerationResult{}, errors.New(imageGenerationResponseError(completed, fmt.Sprintf("completed response has status %q", status)))
	}
	items := imageOutputItems(completed["output"])
	if len(items) == 0 {
		items = doneItems
	}
	if len(items) == 0 {
		return imageGenerationResult{}, errors.New("completed response did not contain an image")
	}
	image, err := decodeImageItem(items[0])
	if err != nil {
		return imageGenerationResult{}, err
	}
	usage, _ := completed["usage"].(map[string]any)
	return imageGenerationResult{Image: image, Usage: usage}, nil
}

func imageGenerationEventError(event map[string]any, fallback string) string {
	if message := nestedString(event, "response", "error", "message"); message != "" {
		return message
	}
	if message := nestedString(event, "error", "message"); message != "" {
		return message
	}
	if message := stringField(event, "message"); message != "" {
		return message
	}
	return fallback
}

func imageGenerationResponseError(response map[string]any, fallback string) string {
	if message := nestedString(response, "error", "message"); message != "" {
		return message
	}
	return fallback
}

func nestedString(value map[string]any, path ...string) string {
	current := value
	for index, key := range path {
		next, ok := current[key]
		if !ok {
			return ""
		}
		if index == len(path)-1 {
			text, _ := next.(string)
			return strings.TrimSpace(text)
		}
		current, ok = next.(map[string]any)
		if !ok {
			return ""
		}
	}
	return ""
}

func imageOutputItems(value any) []map[string]any {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	items := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if item, ok := value.(map[string]any); ok && stringField(item, "type") == "image_generation_call" {
			items = append(items, item)
		}
	}
	return items
}

func decodeImageItem(item map[string]any) (generatedImage, error) {
	if status := stringField(item, "status"); status != "" && status != "completed" {
		return generatedImage{}, fmt.Errorf("image generation call has status %q", status)
	}
	payload, ok := item["result"].(string)
	if !ok || payload == "" {
		return generatedImage{}, errors.New("image generation call missing result")
	}
	if len(payload) > maxImageBase64Chars {
		return generatedImage{}, errors.New("image payload exceeded size limit")
	}
	if payload != strings.TrimSpace(payload) {
		return generatedImage{}, errors.New("image payload is not canonical base64")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(payload)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != payload {
		return generatedImage{}, errors.New("image payload is malformed or noncanonical base64")
	}
	image := generatedImage{Base64: payload}
	if revised, ok := item["revised_prompt"].(string); ok && revised != "" {
		image.RevisedPrompt = revised
	}
	return image, nil
}

func aggregateImageUsage(usages []map[string]any) map[string]any {
	if len(usages) == 0 {
		return nil
	}
	result := map[string]any{}
	for _, usage := range usages {
		mergeNumericUsage(result, usage)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func mergeNumericUsage(dst, src map[string]any) {
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens"} {
		mergeNumericUsageField(dst, src, key)
	}
	for _, key := range []string{"input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details"} {
		typed, ok := src[key].(map[string]any)
		if !ok {
			continue
		}
		nested, _ := dst[key].(map[string]any)
		if nested == nil {
			nested = map[string]any{}
			dst[key] = nested
		}
		for _, detail := range []string{"cached_tokens", "cache_write_tokens", "reasoning_tokens", "image_tokens", "text_tokens"} {
			mergeNumericUsageField(nested, typed, detail)
		}
		if len(nested) == 0 {
			delete(dst, key)
		}
	}
}

func mergeNumericUsageField(dst, src map[string]any, key string) {
	switch typed := src[key].(type) {
	case json.Number:
		if n, err := typed.Int64(); err == nil && n >= 0 {
			current, _ := dst[key].(int64)
			dst[key] = current + n
		}
	case float64:
		if typed >= 0 && typed == float64(int64(typed)) {
			current, _ := dst[key].(int64)
			dst[key] = current + int64(typed)
		}
	}
}
