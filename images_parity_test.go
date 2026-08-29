package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func multipartEditRequest(t *testing.T, fields map[string]string, files map[string][][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, values := range files {
		for index, value := range values {
			part, err := writer.CreateFormFile(name, fmt.Sprintf("image-%d.bin", index))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(value); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}

func pngBytes(_ string) []byte { return pngWithDimensions(1, 1) }

func pngWithDimensions(width, height int) []byte {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		panic(err)
	}
	return data.Bytes()
}

func jpegBytes(width, height int) []byte {
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewNRGBA(image.Rect(0, 0, width, height)), &jpeg.Options{Quality: 75}); err != nil {
		panic(err)
	}
	return data.Bytes()
}

func webPVP8XBytes(width, height int) []byte {
	data := make([]byte, 30)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	binary.LittleEndian.PutUint32(data[16:20], 10)
	width--
	height--
	data[24], data[25], data[26] = byte(width), byte(width>>8), byte(width>>16)
	data[27], data[28], data[29] = byte(height), byte(height>>8), byte(height>>16)
	return data
}

func performEditRequest(proxy *responsesProxy, body io.Reader, contentType string, authorized bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	req.Header.Set("Content-Type", contentType)
	if authorized {
		req.Header.Set("Authorization", "Bearer client-key")
	}
	recorder := httptest.NewRecorder()
	proxy.handleImageEdits(recorder, req)
	return recorder
}

func partialImageSSE(partial, final string, usage map[string]any) string {
	partialEvent, _ := json.Marshal(map[string]any{"type": "response.image_generation_call.partial_image", "partial_image_b64": partial, "partial_image_index": 0})
	return "event: response.image_generation_call.partial_image\ndata: " + string(partialEvent) + "\n\n" + imageSSE(final, "", usage)
}

func TestImageEditsRouteAuthAndContentType(t *testing.T) {
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("unexpected upstream call") }))
	mux := newServerMux(proxy)
	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/images/edits", nil))
	if get.Code == http.StatusOK {
		t.Fatal("GET unexpectedly succeeded")
	}
	body, contentType := multipartEditRequest(t, map[string]string{"prompt": "edit"}, map[string][][]byte{"image": {pngBytes("one")}})
	if got := performEditRequest(proxy, body, contentType, false); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", got.Code)
	}
	if got := performEditRequest(proxy, strings.NewReader("{}"), "application/json", true); got.Code != http.StatusBadRequest {
		t.Fatalf("content type status = %d", got.Code)
	}
}

func TestImageEditsMultipartDefaultsOptionsAndPrivacy(t *testing.T) {
	var upstream map[string]any
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&upstream); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, imageSSE("aGVsbG8=", "private revision", map[string]any{"total_tokens": 4, "unsafe": "drop"}))
	}))
	fields := map[string]string{"prompt": "private edit prompt", "n": "1", "output_format": "webp", "output_compression": "55", "moderation": "low", "input_fidelity": "high", "user": "private-user"}
	body, contentType := multipartEditRequest(t, fields, map[string][][]byte{"image": {pngBytes("one")}, "image[]": {pngBytes("two")}, "mask": {pngBytes("mask")}})
	got := performEditRequest(proxy, body, contentType, true)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", got.Code, got.Body.String())
	}
	tool := upstream["tools"].([]any)[0].(map[string]any)
	for key, want := range map[string]any{"type": "image_generation", "model": defaultImageModel, "action": "edit", "size": "auto", "quality": "auto", "background": "auto", "output_format": "webp", "output_compression": float64(55), "moderation": "low"} {
		if tool[key] != want {
			t.Fatalf("tool[%s]=%#v want %#v; tool=%#v", key, tool[key], want, tool)
		}
	}
	if _, ok := tool["input_fidelity"]; ok {
		t.Fatalf("gpt-image-2 input_fidelity forwarded: %#v", tool)
	}
	mask := tool["input_image_mask"].(map[string]any)["image_url"].(string)
	if !strings.HasPrefix(mask, "data:image/png;base64,") {
		t.Fatalf("mask = %q", mask)
	}
	content := upstream["input"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 3 || content[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("content = %#v", content)
	}
	encodedLog, _ := json.Marshal(proxy.requests.snapshot(10))
	for _, forbidden := range []string{"private edit prompt", "private-user", "private revision", "aGVsbG8=", base64.StdEncoding.EncodeToString(pngBytes("mask"))} {
		if bytes.Contains(encodedLog, []byte(forbidden)) {
			t.Fatalf("request log leaked %q: %s", forbidden, encodedLog)
		}
	}
	entry := proxy.requests.snapshot(10).RequestLog[0]
	if entry.Path != "/v1/images/edits" || entry.InputCount != 2 || entry.ToolCount != 1 {
		t.Fatalf("metadata = %#v", entry)
	}
}

func TestImageEditsValidation(t *testing.T) {
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("unexpected upstream call") }))
	tests := []struct {
		name   string
		fields map[string]string
		files  map[string][][]byte
	}{
		{"missing image", map[string]string{"prompt": "x"}, nil},
		{"missing prompt", nil, map[string][][]byte{"image": {pngBytes("x")}}},
		{"bad magic", map[string]string{"prompt": "x"}, map[string][][]byte{"image": {[]byte("not an image")}}},
		{"truncated PNG magic only", map[string]string{"prompt": "x"}, map[string][][]byte{"image": {[]byte("\x89PNG\r\n\x1a\n")}}},
		{"truncated JPEG magic only", map[string]string{"prompt": "x"}, map[string][][]byte{"image": {{0xff, 0xd8, 0xff}}}},
		{"truncated WebP magic only", map[string]string{"prompt": "x"}, map[string][][]byte{"image": {[]byte("RIFF\x04\x00\x00\x00WEBP")}}},
		{"non-PNG mask", map[string]string{"prompt": "x"}, map[string][][]byte{"image": {pngBytes("x")}, "mask": {jpegBytes(1, 1)}}},
		{"mask dimension mismatch", map[string]string{"prompt": "x"}, map[string][][]byte{"image": {pngWithDimensions(2, 1)}, "mask": {pngWithDimensions(1, 1)}}},
		{"too many", map[string]string{"prompt": "x"}, map[string][][]byte{"image[]": {pngBytes("1"), pngBytes("2"), pngBytes("3"), pngBytes("4"), pngBytes("5"), pngBytes("6")}}},
		{"partial without stream", map[string]string{"prompt": "x", "partial_images": "1"}, map[string][][]byte{"image": {pngBytes("x")}}},
		{"stream n two", map[string]string{"prompt": "x", "stream": "true", "n": "2"}, map[string][][]byte{"image": {pngBytes("x")}}},
		{"bad partial", map[string]string{"prompt": "x", "stream": "true", "partial_images": "4"}, map[string][][]byte{"image": {pngBytes("x")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, contentType := multipartEditRequest(t, test.fields, test.files)
			if got := performEditRequest(proxy, body, contentType, true); got.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
}

func TestImageEditsBodyAndPerFileLimits(t *testing.T) {
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("unexpected upstream call") }))
	body, contentType := multipartEditRequest(t, map[string]string{"prompt": "x"}, map[string][][]byte{"image": {pngBytes("x")}})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = maxImageRequestBytes + 1
	recorder := httptest.NewRecorder()
	proxy.handleImageEdits(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	oversized := make([]byte, maxImageInputBytes)
	copy(oversized, pngBytes(""))
	body, contentType = multipartEditRequest(t, map[string]string{"prompt": "x"}, map[string][][]byte{"image": {oversized}})
	if got := performEditRequest(proxy, body, contentType, true); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "smaller than 50 MB") {
		t.Fatalf("file limit status=%d body=%s", got.Code, got.Body.String())
	}

	maskAtLimit := make([]byte, maxImageMaskBytes)
	copy(maskAtLimit, pngBytes("mask"))
	body, contentType = multipartEditRequest(t, map[string]string{"prompt": "x"}, map[string][][]byte{"image": {pngBytes("x")}, "mask": {maskAtLimit}})
	if got := performEditRequest(proxy, body, contentType, true); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "smaller than 4 MiB") {
		t.Fatalf("mask limit status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestRasterImageConfig(t *testing.T) {
	for _, test := range []struct {
		name          string
		data          []byte
		wantMIME      string
		width, height int
	}{
		{"PNG", pngWithDimensions(2, 3), "image/png", 2, 3},
		{"JPEG", jpegBytes(4, 5), "image/jpeg", 4, 5},
		{"WebP VP8X", webPVP8XBytes(6, 7), "image/webp", 6, 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			mimeType, width, height, err := rasterImageConfig(test.data)
			if err != nil || mimeType != test.wantMIME || width != test.width || height != test.height {
				t.Fatalf("config = %q %dx%d err=%v, want %q %dx%d", mimeType, width, height, err, test.wantMIME, test.width, test.height)
			}
		})
	}
	for _, data := range [][]byte{
		[]byte("\x89PNG\r\n\x1a\n"),
		{0xff, 0xd8, 0xff},
		[]byte("RIFF\x04\x00\x00\x00WEBP"),
		[]byte("GIF89a"),
	} {
		if _, _, _, err := rasterImageConfig(data); err == nil {
			t.Fatalf("truncated/unsupported image accepted: %x", data)
		}
	}
}

func TestImageGenerationAndEditStreamingTranslation(t *testing.T) {
	for _, test := range []struct {
		name, prefix string
		edit         bool
	}{{"generation", "image_generation", false}, {"edit", "image_edit", true}} {
		t.Run(test.name, func(t *testing.T) {
			proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, partialImageSSE("cGFydGlhbA==", "ZmluYWw=", map[string]any{"input_tokens": 2, "total_tokens": 5, "secret": "drop"}))
			}))
			var got *httptest.ResponseRecorder
			if test.edit {
				body, ct := multipartEditRequest(t, map[string]string{"prompt": "edit", "stream": "true", "partial_images": "1", "quality": "high"}, map[string][][]byte{"image": {pngBytes("x")}})
				got = performEditRequest(proxy, body, ct, true)
			} else {
				got = performImageRequest(proxy, `{"prompt":"cat","stream":true,"partial_images":1,"quality":"high"}`, true)
			}
			if got.Code != http.StatusOK || got.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("status=%d headers=%#v body=%s", got.Code, got.Header(), got.Body.String())
			}
			text := got.Body.String()
			for _, want := range []string{"event: " + test.prefix + ".partial_image", `"partial_image_index":0`, `"b64_json":"cGFydGlhbA=="`, "event: " + test.prefix + ".completed", `"b64_json":"ZmluYWw="`, `"quality":"high"`, `"total_tokens":5`} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in %s", want, text)
				}
			}
			for _, forbidden := range []string{"response.image_generation_call", "response.completed", "[DONE]", `"secret"`} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("leaked %q in %s", forbidden, text)
				}
			}
		})
	}
}

func TestImageStreamingValidationAndErrorEvent(t *testing.T) {
	proxy, _ := testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "data: {malformed}\n\n") }))
	for _, body := range []string{`{"prompt":"x","partial_images":1}`, `{"prompt":"x","stream":true,"n":2}`, `{"prompt":"x","stream":"yes"}`} {
		if got := performImageRequest(proxy, body, true); got.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d", body, got.Code)
		}
	}
	got := performImageRequest(proxy, `{"prompt":"x","stream":true}`, true)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "event: error") || !strings.Contains(got.Body.String(), `"type":"error"`) {
		t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
	}

	privateError := "private-backend detail must not escape"
	proxy, _ = testImageProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":%q}}}\n\n", privateError)
	}))
	got = performImageRequest(proxy, `{"prompt":"x","stream":true}`, true)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"message":"image request failed"`) || strings.Contains(got.Body.String(), privateError) {
		t.Fatalf("private upstream SSE error escaped: status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestParseImageSSEStreamBoundsAndCompletedAuthoritative(t *testing.T) {
	var partials []string
	result, err := parseImageSSEStream(strings.NewReader(partialImageSSE("cGFydGlhbA==", "ZmluYWw=", nil)), func(payload string, index int) error { partials = append(partials, payload); return nil })
	if err != nil || result.Image.Base64 != "ZmluYWw=" || len(partials) != 1 {
		t.Fatalf("result=%#v partials=%#v err=%v", result, partials, err)
	}
	if _, err := parseImageSSEStream(strings.NewReader(partialImageSSE("Zh==", "ZmluYWw=", nil)), nil); err == nil {
		t.Fatal("noncanonical partial accepted")
	}
	done := `data: {"type":"response.output_item.done","item":{"type":"image_generation_call","status":"completed","result":"Z29vZA=="}}` + "\n\n"
	completedBad := `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"image_generation_call","result":"%%%"}]}}` + "\n\n"
	if _, err := parseImageSSEStream(strings.NewReader(done+completedBad), nil); err == nil {
		t.Fatal("done item overrode malformed completed output")
	}
	completedEmpty := `data: {"type":"response.completed","response":{"status":"completed","output":[],"usage":{"total_tokens":7}}}` + "\n\n"
	result, err = parseImageSSEStream(strings.NewReader(done+completedEmpty), nil)
	if err != nil || result.Image.Base64 != "Z29vZA==" {
		t.Fatalf("empty completed output fallback result=%#v err=%v", result, err)
	}
}

func TestImageCompletedEventUsesBoundedUpstreamMetadata(t *testing.T) {
	req := imageGenerationRequest{Background: "auto", OutputFormat: "png", Quality: "low", Size: "1024x1024"}
	event := imageAPIEvent("image_generation.completed", "ZmluYWw=", req, -1, nil, map[string]string{"background": "opaque", "quality": "high", "size": "1536x1024"})
	if event["background"] != "opaque" || event["quality"] != "high" || event["size"] != "1536x1024" {
		t.Fatalf("event metadata = %#v", event)
	}
}
