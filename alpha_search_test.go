package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewAlphaSearchPreservesSuccessfulBytes(t *testing.T) {
	body := "{\n  \"text\": \"Multiple  spaces and\\tindentation\",\n  \"url\": \"https://www.example.com/ordinary-natural-search-result\",\n  \"word\": \"" + strings.Repeat("search", 12) + "\"\n}\n"
	proxy := reviewUpstreamProxy(t, strings.NewReader(body))
	request := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"commands":{"search_query":[{"q":"test"}]}}`))
	response := httptest.NewRecorder()
	proxy.handleAlphaSearch(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), []byte(body)) {
		t.Fatal("successful search response bytes changed")
	}
	if entry := reviewRequestEntry(t, proxy); entry.Error != "" {
		t.Fatalf("successful search logged error: %s", entry.Error)
	}
}

func TestReviewAlphaSearchRejectsIncompleteOrOversizedBody(t *testing.T) {
	tests := []struct {
		name string
		body func() io.Reader
	}{
		{"read failure", func() io.Reader { return io.MultiReader(strings.NewReader(`{"partial":"`), reviewErrorReader{}) }},
		{"oversized", func() io.Reader { return io.LimitReader(reviewRepeatingReader{}, maxAlphaSearchResponseBytes+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy := reviewUpstreamProxy(t, tt.body())
			request := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{}`))
			response := httptest.NewRecorder()
			proxy.handleAlphaSearch(response, request)
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", response.Code)
			}
			if strings.Contains(response.Body.String(), reviewPrivateError) || !strings.Contains(response.Body.String(), "incomplete or too large") {
				t.Fatal("search failure response was not safely summarized")
			}
			entry := reviewRequestEntry(t, proxy)
			if entry.Status != http.StatusBadGateway || entry.Error == "" {
				t.Fatal("search failure not recorded")
			}
		})
	}
}

type reviewRepeatingReader struct{}

func (reviewRepeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
