package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
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
	maxImageRequestBytes        = 128 * 1024 * 1024
	maxImageInputBytes          = 50 * 1024 * 1024
	maxImageMaskBytes           = 4 * 1024 * 1024
	maxImageTextFieldBytes      = 1024 * 1024
	maxImageReferences          = 5
	maxPartialImages            = 3
	maxWebPChunks               = 256
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
	Stream            bool
	PartialImages     int
	Action            string
	InputImages       []imageInput
	Mask              *imageInput
	InputFidelity     string
}

type imageInput struct {
	MIME   string
	Data   []byte
	Width  int
	Height int
}

type generatedImage struct {
	Base64        string `json:"b64_json"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type imageGenerationResult struct {
	Image    generatedImage
	Usage    map[string]any
	Metadata map[string]string
}

func (p *responsesProxy) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	logEntry := p.beginRequestLog(r)
	defer logEntry.finish()
	if !p.authorizedClient(r) {
		logEntry.markError(http.StatusUnauthorized, "unauthorized")
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	req, err := parseImageEditRequest(w, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errImageRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		logEntry.markError(status, err.Error())
		writeProxyError(w, status, err.Error())
		return
	}
	logEntry.markImageRequest(req.Model, req.Stream, len(req.InputImages))
	if req.Stream {
		p.streamOneImage(w, r, req, logEntry, "image_edit")
		return
	}
	p.writeImageResults(w, r, req, logEntry)
}

var errImageRequestTooLarge = errors.New("image request exceeds size limit")

func parseImageEditRequest(w http.ResponseWriter, r *http.Request) (imageGenerationRequest, error) {
	req := imageGenerationRequest{Model: defaultImageModel, Count: 1, Action: "edit", Size: "auto", Quality: "auto", Background: "auto"}
	if r.ContentLength > maxImageRequestBytes {
		return req, errImageRequestTooLarge
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return req, errors.New("Content-Type must be multipart/form-data with a boundary")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageRequestBytes)
	reader := multipart.NewReader(r.Body, params["boundary"])
	fields := map[string]string{}
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			if strings.Contains(nextErr.Error(), "request body too large") {
				return req, errImageRequestTooLarge
			}
			return req, errors.New("malformed multipart request")
		}
		name := part.FormName()
		switch name {
		case "image", "image[]", "mask":
			var input imageInput
			var readErr error
			if name == "mask" {
				input, readErr = readMultipartMask(part)
			} else {
				input, readErr = readMultipartImage(part)
			}
			part.Close()
			if readErr != nil {
				return req, fmt.Errorf("%s: %w", name, readErr)
			}
			if name == "mask" {
				if req.Mask != nil {
					return req, errors.New("mask may be provided only once")
				}
				req.Mask = &input
			} else {
				req.InputImages = append(req.InputImages, input)
				if len(req.InputImages) > maxImageReferences {
					return req, fmt.Errorf("at most %d images may be provided", maxImageReferences)
				}
			}
		default:
			value, readErr := io.ReadAll(io.LimitReader(part, maxImageTextFieldBytes+1))
			part.Close()
			var maxErr *http.MaxBytesError
			if errors.As(readErr, &maxErr) {
				return req, errImageRequestTooLarge
			}
			if readErr != nil || len(value) > maxImageTextFieldBytes {
				return req, fmt.Errorf("field %s exceeds size limit", name)
			}
			if name != "" {
				fields[name] = string(value)
			}
		}
	}
	if len(req.InputImages) == 0 {
		return req, errors.New("at least one image or image[] file is required")
	}
	if req.Mask != nil && (req.Mask.Width != req.InputImages[0].Width || req.Mask.Height != req.InputImages[0].Height) {
		return req, errors.New("mask dimensions must match the first input image")
	}
	if prompt := fields["prompt"]; strings.TrimSpace(prompt) == "" {
		return req, errors.New("prompt must be a non-empty string")
	} else {
		req.Prompt = prompt
	}
	if value := strings.TrimSpace(fields["model"]); value != "" {
		req.Model = value
	}
	if value := fields["n"]; value != "" {
		n, parseErr := parseFormInt(value, "n", 1, maxImageCount)
		if parseErr != nil {
			return req, parseErr
		}
		req.Count = n
	}
	if value := fields["size"]; value != "" {
		req.Size, err = optionalImageSize(map[string]any{"size": value})
		if err != nil {
			return req, err
		}
	}
	for key, target := range map[string]*string{"quality": &req.Quality, "output_format": &req.OutputFormat, "background": &req.Background, "moderation": &req.Moderation, "input_fidelity": &req.InputFidelity} {
		value := fields[key]
		if value == "" {
			continue
		}
		allowed := map[string][]string{"quality": {"auto", "low", "medium", "high"}, "output_format": {"png", "jpeg", "webp"}, "background": {"auto", "opaque", "transparent"}, "moderation": {"auto", "low"}, "input_fidelity": {"low", "high"}}[key]
		parsed, parseErr := optionalImageEnum(map[string]any{key: value}, key, allowed...)
		if parseErr != nil {
			return req, parseErr
		}
		*target = parsed
	}
	if value := fields["output_compression"]; value != "" {
		compression, parseErr := parseFormInt(value, "output_compression", 0, 100)
		if parseErr != nil {
			return req, parseErr
		}
		req.OutputCompression = &compression
	}
	if value := fields["stream"]; value != "" {
		stream, parseErr := strconv.ParseBool(value)
		if parseErr != nil {
			return req, errors.New("stream must be a boolean")
		}
		req.Stream = stream
	}
	if value := fields["partial_images"]; value != "" {
		partial, parseErr := parseFormInt(value, "partial_images", 0, maxPartialImages)
		if parseErr != nil {
			return req, parseErr
		}
		req.PartialImages = partial
		if !req.Stream {
			return req, errors.New("partial_images requires stream:true")
		}
	}
	if req.Stream && req.Count != 1 {
		return req, errors.New("streaming image requests require n:1")
	}
	if req.Background == "transparent" && req.OutputFormat == "jpeg" {
		return req, errors.New("transparent background requires png or webp output_format")
	}
	// user is intentionally parsed and discarded with the other fields. It is
	// never forwarded and request logs retain metadata only.
	return req, nil
}

func parseFormInt(value, name string, min, max int) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || strconv.Itoa(parsed) != value || parsed < min || parsed > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, min, max)
	}
	return parsed, nil
}

func readMultipartImage(part *multipart.Part) (imageInput, error) {
	return readMultipartRaster(part, maxImageInputBytes, false)
}

func readMultipartMask(part *multipart.Part) (imageInput, error) {
	return readMultipartRaster(part, maxImageMaskBytes, true)
}

func readMultipartRaster(part *multipart.Part, maxBytes int, pngOnly bool) (imageInput, error) {
	data, err := io.ReadAll(io.LimitReader(part, int64(maxBytes)+1))
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return imageInput{}, errImageRequestTooLarge
	}
	if err != nil {
		return imageInput{}, errors.New("could not read uploaded image")
	}
	if len(data) == 0 {
		if pngOnly {
			return imageInput{}, errors.New("mask is empty")
		}
		return imageInput{}, errors.New("image is empty")
	}
	if len(data) >= maxBytes {
		if pngOnly {
			return imageInput{}, errors.New("mask must be smaller than 4 MiB")
		}
		return imageInput{}, errors.New("image must be smaller than 50 MB")
	}
	if pngOnly && !hasPNGSignature(data) {
		return imageInput{}, errors.New("mask must be a PNG image")
	}
	mimeType, width, height, configErr := rasterImageConfig(data)
	if configErr != nil {
		if pngOnly {
			return imageInput{}, errors.New("mask must be a structurally valid PNG image")
		}
		return imageInput{}, errors.New("unsupported image data; expected PNG, JPEG, or WebP")
	}
	if pngOnly && mimeType != "image/png" {
		return imageInput{}, errors.New("mask must be a PNG image")
	}
	return imageInput{MIME: mimeType, Data: data, Width: width, Height: height}, nil
}

func hasPNGSignature(data []byte) bool {
	return len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n"))
}

func rasterImageConfig(data []byte) (string, int, int, error) {
	switch {
	case hasPNGSignature(data):
		config, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width <= 0 || config.Height <= 0 {
			return "", 0, 0, errors.New("invalid PNG configuration")
		}
		return "image/png", config.Width, config.Height, nil
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width <= 0 || config.Height <= 0 {
			return "", 0, 0, errors.New("invalid JPEG configuration")
		}
		return "image/jpeg", config.Width, config.Height, nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		width, height, err := webPConfig(data)
		if err != nil {
			return "", 0, 0, err
		}
		return "image/webp", width, height, nil
	default:
		return "", 0, 0, errors.New("unsupported raster image")
	}
}

func webPConfig(data []byte) (int, int, error) {
	if len(data) < 20 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return 0, 0, errors.New("invalid WebP header")
	}
	declaredSize := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if declaredSize != uint64(len(data)) {
		return 0, 0, errors.New("invalid WebP RIFF size")
	}
	var width, height int
	for offset, chunks := 12, 0; offset < len(data); chunks++ {
		if chunks >= maxWebPChunks || len(data)-offset < 8 {
			return 0, 0, errors.New("invalid WebP chunk table")
		}
		chunkType := string(data[offset : offset+4])
		chunkSize := uint64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		payloadStart := uint64(offset + 8)
		payloadEnd := payloadStart + chunkSize
		paddedEnd := payloadEnd + chunkSize%2
		if payloadEnd < payloadStart || paddedEnd > uint64(len(data)) {
			return 0, 0, errors.New("invalid WebP chunk size")
		}
		if chunkSize%2 != 0 && data[payloadEnd] != 0 {
			return 0, 0, errors.New("invalid WebP chunk padding")
		}
		payload := data[payloadStart:payloadEnd]
		switch chunkType {
		case "VP8X":
			if len(payload) != 10 || payload[0]&0xc1 != 0 || !bytes.Equal(payload[1:4], []byte{0, 0, 0}) {
				return 0, 0, errors.New("invalid WebP VP8X header")
			}
			width = 1 + int(payload[4]) + int(payload[5])<<8 + int(payload[6])<<16
			height = 1 + int(payload[7]) + int(payload[8])<<8 + int(payload[9])<<16
		case "VP8L":
			if len(payload) < 5 || payload[0] != 0x2f {
				return 0, 0, errors.New("invalid WebP VP8L header")
			}
			bits := binary.LittleEndian.Uint32(payload[1:5])
			if bits>>29 != 0 {
				return 0, 0, errors.New("unsupported WebP VP8L version")
			}
			if width == 0 {
				width, height = 1+int(bits&0x3fff), 1+int((bits>>14)&0x3fff)
			}
		case "VP8 ":
			if len(payload) < 10 || payload[0]&1 != 0 || !bytes.Equal(payload[3:6], []byte{0x9d, 0x01, 0x2a}) {
				return 0, 0, errors.New("invalid WebP VP8 header")
			}
			frameWidth := int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
			frameHeight := int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)
			if frameWidth == 0 || frameHeight == 0 {
				return 0, 0, errors.New("invalid WebP dimensions")
			}
			if width == 0 {
				width, height = frameWidth, frameHeight
			}
		}
		offset = int(paddedEnd)
	}
	if width > 0 && height > 0 {
		return width, height, nil
	}
	return 0, 0, errors.New("WebP image configuration not found")
}

func imageDataURL(input imageInput) string {
	return "data:" + input.MIME + ";base64," + base64.StdEncoding.EncodeToString(input.Data)
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
	logEntry.markImageRequest(req.Model, req.Stream, req.Count)
	if req.Stream {
		p.streamOneImage(w, r, req, logEntry, "image_generation")
		return
	}

	p.writeImageResults(w, r, req, logEntry)
}

func (p *responsesProxy) writeImageResults(w http.ResponseWriter, r *http.Request, req imageGenerationRequest, logEntry *pendingRequestLog) {
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
	req := imageGenerationRequest{Model: defaultImageModel, Count: 1, Action: "generate"}
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
	if value, ok := body["stream"]; ok {
		stream, valid := value.(bool)
		if !valid {
			return req, errors.New("stream must be a boolean")
		}
		req.Stream = stream
	}
	if value, ok := body["partial_images"]; ok {
		partial, valid := exactJSONInt(value)
		if !valid || partial < 0 || partial > maxPartialImages {
			return req, fmt.Errorf("partial_images must be an integer between 0 and %d", maxPartialImages)
		}
		req.PartialImages = partial
		if !req.Stream {
			return req, errors.New("partial_images requires stream:true")
		}
	}
	if req.Stream && req.Count != 1 {
		return req, errors.New("streaming image requests require n:1")
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

func exactAnyInt(value any) (int, bool) {
	if parsed, ok := exactJSONInt(value); ok {
		return parsed, true
	}
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) {
		return 0, false
	}
	return int(number), true
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

func (p *responsesProxy) imageBackingModel() string {
	return valueOr(strings.TrimSpace(p.cfg.imageResponsesModel), imageResponsesModel)
}

func (p *responsesProxy) buildImageResponsesBody(req imageGenerationRequest) map[string]any {
	tool := map[string]any{"type": "image_generation", "model": req.Model}
	if req.Action == "edit" {
		tool["action"] = "edit"
	}
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
	if req.PartialImages > 0 {
		tool["partial_images"] = req.PartialImages
	}
	// Codex's gpt-image-2 fallback treats edit inputs as high fidelity and does
	// not accept an explicit input_fidelity. Preserve compatibility for other
	// explicitly selected image models.
	if req.InputFidelity != "" && req.Model != defaultImageModel {
		tool["input_fidelity"] = req.InputFidelity
	}
	if req.Mask != nil {
		tool["input_image_mask"] = map[string]any{"image_url": imageDataURL(*req.Mask)}
	}
	content := []any{map[string]any{"type": "input_text", "text": req.Prompt}}
	for _, image := range req.InputImages {
		content = append(content, map[string]any{"type": "input_image", "image_url": imageDataURL(image)})
	}
	return map[string]any{
		"model":        p.imageBackingModel(),
		"instructions": imageGenerationInstructions,
		"input": []any{map[string]any{
			"role":    "user",
			"content": content,
		}},
		"tools":       []any{tool},
		"tool_choice": map[string]any{"type": "image_generation"},
		"stream":      true,
		"store":       false,
	}
}

func (p *responsesProxy) generateOneImage(r *http.Request, imageReq imageGenerationRequest, index int, logEntry *pendingRequestLog) (imageGenerationResult, *dispatchFailure) {
	release, fail := p.acquireUpstreamSlot(r.Context())
	if fail != nil {
		return imageGenerationResult{}, fail
	}
	defer release()
	body := p.buildImageResponsesBody(imageReq)
	encoded, err := json.Marshal(body)
	if err != nil {
		return imageGenerationResult{}, &dispatchFailure{status: http.StatusBadRequest, message: "invalid image request body"}
	}
	model := p.imageBackingModel()
	info := requestInfo{Model: model, NormalizedModel: model, Stream: true}
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
		return imageGenerationResult{}, &dispatchFailure{status: http.StatusBadGateway, message: "invalid upstream image response"}
	}
	return result, nil
}

func (p *responsesProxy) streamOneImage(w http.ResponseWriter, r *http.Request, imageReq imageGenerationRequest, logEntry *pendingRequestLog, eventPrefix string) {
	release, fail := p.acquireUpstreamSlot(r.Context())
	if fail != nil {
		p.writeDispatchFailure(w, logEntry, fail)
		return
	}
	defer release()
	body := p.buildImageResponsesBody(imageReq)
	encoded, err := json.Marshal(body)
	if err != nil {
		logEntry.markError(http.StatusBadRequest, "invalid image request body")
		writeProxyError(w, http.StatusBadRequest, "invalid image request body")
		return
	}
	model := p.imageBackingModel()
	info := requestInfo{Model: model, NormalizedModel: model, Stream: true}
	resp, fail := p.dispatchUpstream(r.Context(), encoded, info, body, imageUpstreamRequest(r, 0))
	if fail != nil {
		p.writeDispatchFailure(w, logEntry, fail)
		return
	}
	defer resp.Body.Close()
	logEntry.markUpstreamStatus(resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		p.writeDispatchFailure(w, logEntry, &dispatchFailure{status: resp.StatusCode, message: "upstream image request failed", body: responseBody})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		logEntry.markError(http.StatusInternalServerError, "streaming unsupported")
		writeProxyError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	emit := func(event map[string]any) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		encodedEvent, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encodedEvent); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	result, err := parseImageSSEStream(resp.Body, func(payload string, index int) error {
		return emit(imageAPIEvent(eventPrefix+".partial_image", payload, imageReq, index, nil, nil))
	})
	if err != nil {
		logEntry.markStreamError("invalid upstream image response")
		if r.Context().Err() == nil {
			_ = emit(imageStreamError())
		}
		return
	}
	usage := aggregateImageUsage([]map[string]any{result.Usage})
	if usage != nil {
		logEntry.markUsage(extractTokenUsage(map[string]any{"usage": usage}))
	}
	if err := emit(imageAPIEvent(eventPrefix+".completed", result.Image.Base64, imageReq, -1, usage, result.Metadata)); err != nil {
		logEntry.markStreamError("client disconnected")
		return
	}
	logEntry.markStatus(http.StatusOK)
}

func imageAPIEvent(eventType, payload string, req imageGenerationRequest, partialIndex int, usage map[string]any, metadata map[string]string) map[string]any {
	event := map[string]any{
		"type": eventType, "b64_json": payload, "created_at": time.Now().Unix(),
		"background": valueOr(req.Background, "auto"), "output_format": valueOr(req.OutputFormat, "png"),
		"quality": valueOr(req.Quality, "auto"), "size": valueOr(req.Size, "auto"),
	}
	for _, key := range []string{"background", "output_format", "quality", "size"} {
		if value := metadata[key]; value != "" {
			event[key] = value
		}
	}
	if partialIndex >= 0 {
		event["partial_image_index"] = partialIndex
	}
	if usage != nil {
		event["usage"] = usage
	}
	return event
}

func imageStreamError() map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": "image_error", "code": "upstream_image_error", "message": "image request failed"}}
}

func parseImageSSEStream(reader io.Reader, onPartial func(string, int) error) (imageGenerationResult, error) {
	limited := &io.LimitedReader{R: reader, N: maxImageSSEBytes + 1}
	var doneItems []map[string]any
	var completed map[string]any
	events := 0
	err := readSSE(limited, maxImageSSEEventBytes, func(payload []byte) error {
		data := strings.TrimSpace(string(payload))
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
		case "response.image_generation_call.partial_image":
			payload, ok := event["partial_image_b64"].(string)
			if !ok {
				return errors.New("partial image missing payload")
			}
			if err := validateImageBase64(payload); err != nil {
				return fmt.Errorf("invalid partial image: %w", err)
			}
			index, ok := exactAnyInt(event["partial_image_index"])
			if !ok || index < 0 || index >= maxPartialImages {
				return errors.New("partial image index is invalid")
			}
			if onPartial != nil {
				if err := onPartial(payload, index); err != nil {
					return err
				}
			}
		case "response.completed":
			response, ok := event["response"].(map[string]any)
			if !ok {
				return errors.New("response.completed missing response")
			}
			completed = response
			return errSSEComplete
		case "response.output_item.done":
			if item, ok := event["item"].(map[string]any); ok && stringField(item, "type") == "image_generation_call" {
				doneItems = append(doneItems, item)
			}
		}
		return nil
	})
	if err != nil {
		return imageGenerationResult{}, err
	}
	if limited.N <= 0 {
		return imageGenerationResult{}, errors.New("response exceeded size limit")
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
	return imageGenerationResult{Image: image, Usage: usage, Metadata: imageResultMetadata(items[0], completed)}, nil
}

func imageResultMetadata(item, response map[string]any) map[string]string {
	metadata := map[string]string{}
	for _, key := range []string{"background", "output_format", "quality", "size"} {
		if value := stringField(item, key); validImageMetadata(key, value) {
			metadata[key] = value
		} else if value := stringField(response, key); validImageMetadata(key, value) {
			metadata[key] = value
		}
	}
	return metadata
}

func validImageMetadata(key, value string) bool {
	switch key {
	case "background":
		return value == "auto" || value == "opaque" || value == "transparent"
	case "output_format":
		return value == "png" || value == "jpeg" || value == "webp"
	case "quality":
		return value == "auto" || value == "low" || value == "medium" || value == "high"
	case "size":
		_, err := optionalImageSize(map[string]any{"size": value})
		return err == nil
	default:
		return false
	}
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
	return parseImageSSEStream(reader, nil)
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
	if err := validateImageBase64(payload); err != nil {
		return generatedImage{}, err
	}
	image := generatedImage{Base64: payload}
	if revised, ok := item["revised_prompt"].(string); ok && revised != "" {
		image.RevisedPrompt = revised
	}
	return image, nil
}

func validateImageBase64(payload string) error {
	if len(payload) > maxImageBase64Chars {
		return errors.New("image payload exceeded size limit")
	}
	if payload == "" || payload != strings.TrimSpace(payload) {
		return errors.New("image payload is not canonical base64")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(payload)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != payload {
		return errors.New("image payload is malformed or noncanonical base64")
	}
	return nil
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
