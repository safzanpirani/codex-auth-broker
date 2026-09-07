package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const reviewPrivateError = "private upstream request content sentinel"

type reviewFragmentReader struct {
	r    io.Reader
	size int
}

func (r reviewFragmentReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.r.Read(p)
}

type reviewErrorReader struct{}

func (reviewErrorReader) Read([]byte) (int, error) {
	return 0, errors.New(reviewPrivateError)
}

type reviewFailingWriter struct{ *httptest.ResponseRecorder }

func (reviewFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New(reviewPrivateError)
}

func reviewUpstreamProxy(t *testing.T, body io.Reader) *responsesProxy {
	t.Helper()
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(body), Request: r}, nil
	})}
	return &responsesProxy{
		cfg:      config{upstreamURL: "https://upstream.invalid/responses", alphaSearchURL: "https://upstream.invalid/search"},
		pool:     newAccountPool([]string{writeWebSocketTestAuth(t, "acct_test")}, time.Minute, client),
		client:   client,
		requests: newRequestLogStore(10),
	}
}

func reviewRequestEntry(t *testing.T, proxy *responsesProxy) requestLogEntry {
	t.Helper()
	entries := proxy.requests.snapshot(10).RequestLog
	if len(entries) != 1 {
		t.Fatalf("request entries = %d, want 1", len(entries))
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(reviewPrivateError)) {
		t.Fatal("private upstream content retained in request metadata")
	}
	return entries[0]
}

func TestReviewSSEFramingFragmentedCRLFMultilineAndEOF(t *testing.T) {
	stream := ": keepalive\r\nevent: custom\r\ndata: hello\r\ndata:  world\r\n\r\ndata:\r\n\r\ndata: eof"
	for _, chunkSize := range []int{1, 2, 7, 1024} {
		var events []string
		err := readSSE(reviewFragmentReader{strings.NewReader(stream), chunkSize}, 128, func(data []byte) error {
			events = append(events, string(data))
			return nil
		})
		if err != nil {
			t.Fatalf("chunk size %d: %v", chunkSize, err)
		}
		if !reflect.DeepEqual(events, []string{"hello\n world", "", "eof"}) {
			t.Fatalf("chunk size %d: events = %#v", chunkSize, events)
		}
	}
}

func TestReviewSSEFrameBoundsAndReset(t *testing.T) {
	for _, stream := range []string{"data: " + strings.Repeat("x", 40), strings.Repeat("data: 1234567\n", 8)} {
		if err := readSSE(reviewFragmentReader{strings.NewReader(stream), 1}, 32, func([]byte) error { return nil }); err == nil {
			t.Fatal("oversized SSE event accepted")
		}
	}
	events := 0
	if err := readSSE(strings.NewReader(strings.Repeat("data: small\n\n", 100)), 16, func([]byte) error { events++; return nil }); err != nil || events != 100 {
		t.Fatalf("separate bounded events: count=%d error=%v", events, err)
	}
}

func TestReviewAggregateSSEKeepsAuthoritativeOutput(t *testing.T) {
	stream := "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"stale\"}}\r\n\r\n" +
		"data: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"output\":[{\"id\":\"authoritative\"}],\"service_tier\":\"priority\"}}"
	response, err := aggregateResponsesSSE(reviewFragmentReader{strings.NewReader(stream), 1})
	if err != nil {
		t.Fatal(err)
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) != 1 || output[0].(map[string]any)["id"] != "authoritative" {
		t.Fatalf("final output = %#v", response["output"])
	}
	if response["service_tier"] != "priority" {
		t.Fatal("final response metadata lost")
	}
}

func TestReviewAggregateSSEErrorsDoNotRetainPrivateContent(t *testing.T) {
	for _, upstream := range []io.Reader{
		strings.NewReader(`data: {"type":"error","message":"` + reviewPrivateError + `"}` + "\n\n"),
		io.MultiReader(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"), reviewErrorReader{}),
		strings.NewReader("data: invalid " + reviewPrivateError + "\n\n"),
	} {
		proxy := reviewUpstreamProxy(t, upstream)
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"test","stream":false}`))
		response := httptest.NewRecorder()
		proxy.handleResponses(response, request)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("status=%d, want 502", response.Code)
		}
		if strings.Contains(response.Body.String(), reviewPrivateError) {
			t.Fatal("private upstream error returned to client")
		}
		if entry := reviewRequestEntry(t, proxy); entry.Error == "" {
			t.Fatal("aggregate failure not recorded")
		}
	}
}

func TestReviewResponsesStreamPreservesBytesAndRecordsUsage(t *testing.T) {
	stream := ": keepalive\r\n\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"two  spaces\"}\r\n\r\n" +
		"event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\r\n\r\n"
	proxy := reviewUpstreamProxy(t, reviewFragmentReader{strings.NewReader(stream), 1})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"test","stream":true}`))
	response := httptest.NewRecorder()
	proxy.handleResponses(response, request)
	if response.Code != http.StatusOK || response.Body.String() != stream {
		t.Fatal("successful stream bytes changed")
	}
	entry := reviewRequestEntry(t, proxy)
	if entry.Error != "" || entry.InputTokens == nil || *entry.InputTokens != 12 || entry.OutputTokens == nil || *entry.OutputTokens != 3 {
		t.Fatalf("unexpected stream metadata: status=%d error=%q usage=%v/%v", entry.Status, entry.Error, entry.InputTokens, entry.OutputTokens)
	}
}

func TestReviewResponsesStreamFailuresRecorded(t *testing.T) {
	tests := []struct {
		name       string
		body       func() io.Reader
		writeFails bool
	}{
		{"premature EOF", func() io.Reader { return strings.NewReader("data: {\"type\":\"response.created\"}\n\n") }, false},
		{"upstream read failure", func() io.Reader {
			return io.MultiReader(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"), reviewErrorReader{})
		}, false},
		{"client write failure", func() io.Reader {
			return strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
		}, true},
		{"failed response", func() io.Reader {
			return strings.NewReader(`data: {"type":"response.failed","response":{"error":{"message":"` + reviewPrivateError + `"}}}` + "\n\n")
		}, false},
		{"error event", func() io.Reader {
			return strings.NewReader(`data: {"type":"error","message":"` + reviewPrivateError + `"}` + "\n\n")
		}, false},
		{"incomplete response", func() io.Reader {
			return strings.NewReader(`data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"` + reviewPrivateError + `"}}}` + "\n\n")
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := reviewUpstreamProxy(t, tt.body())
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"test","stream":true}`))
			recorder := httptest.NewRecorder()
			var response http.ResponseWriter = recorder
			if tt.writeFails {
				response = reviewFailingWriter{recorder}
			}
			proxy.handleResponses(response, request)
			entry := reviewRequestEntry(t, proxy)
			if entry.Error == "" {
				t.Fatal("failed stream recorded as successful")
			}
			if entry.Status != http.StatusOK || entry.UpstreamStatus != http.StatusOK {
				t.Fatalf("wire status changed after stream started: %d/%d", entry.Status, entry.UpstreamStatus)
			}
		})
	}
}

func TestReviewImageSSEUsesFragmentedMultilineEOF(t *testing.T) {
	stream := "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"image_generation_call\",\"result\":\"b2xk\"}}\r\n\r\n" +
		"data: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"image_generation_call\",\"result\":\"bmV3\"}]}}"
	result, err := parseImageGenerationSSE(reviewFragmentReader{strings.NewReader(stream), 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Image.Base64 != "bmV3" {
		t.Fatal("image final output did not take precedence")
	}
}
