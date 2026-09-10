package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSummarizeUpstreamErrorDoesNotRetainBodyText(t *testing.T) {
	const sentinel = "private-prompt-sentinel"
	body := []byte(`{"error":{"message":"` + sentinel + `","type":"invalid_request_error","code":"bad_input"}}`)
	summary := summarizeUpstreamError(body, http.StatusBadRequest)
	if strings.Contains(summary, sentinel) {
		t.Fatalf("summary retained upstream body text: %q", summary)
	}
	for _, want := range []string{"upstream returned 400", "type=invalid_request_error", "code=bad_input"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary = %q, want %q", summary, want)
		}
	}
}

func TestRequestShapeContainsNoClientValues(t *testing.T) {
	const sentinel = "private-prompt-sentinel"
	shape := requestShape(map[string]any{
		"model":            sentinel,
		"prompt_cache_key": sentinel,
		"input":            []any{map[string]any{"role": "user", "content": sentinel}},
		"tools":            []any{map[string]any{"name": sentinel}},
	})
	if strings.Contains(shape, sentinel) {
		t.Fatalf("request shape retained a client value: %s", shape)
	}
	for _, want := range []string{`"has_model":true`, `"has_input":true`, `"input_len":1`, `"tools_len":1`} {
		if !strings.Contains(shape, want) {
			t.Fatalf("request shape = %s, want %s", shape, want)
		}
	}
}

func TestRequestLogBoundsAndFingerprintsClientMetadata(t *testing.T) {
	store := newRequestLogStore(1)
	cacheKey := strings.Repeat("cache-key", 200_000)
	store.add(requestLogEntry{
		Method:         strings.Repeat("M", 100),
		RequestID:      strings.Repeat("R", 2*1024*1024),
		Model:          strings.Repeat("model", 300_000),
		PromptCacheKey: cacheKey,
		Status:         http.StatusOK,
	})
	entry := store.snapshot(1).RequestLog[0]
	if len(entry.Method) > 16 || len(entry.RequestID) > 256 || len(entry.Model) > 256 {
		t.Fatalf("request metadata was not bounded: method=%d request_id=%d model=%d", len(entry.Method), len(entry.RequestID), len(entry.Model))
	}
	if entry.PromptCacheKey == cacheKey || !strings.HasPrefix(entry.PromptCacheKey, "sha256:") {
		t.Fatalf("prompt cache key was not fingerprinted: %q", entry.PromptCacheKey)
	}
}

func TestPersistedLogSkipsOversizedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	valid, err := json.Marshal(requestLogEntry{ID: 7, Method: "POST", Status: http.StatusOK})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"model":"` + strings.Repeat("x", 1024*1024) + `"}`)
	payload = append(payload, '\n')
	payload = append(payload, valid...)
	payload = append(payload, '\n')
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, maxID, err := loadPersistedEntries(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || maxID != 7 || entries[0].Method != "POST" {
		t.Fatalf("loaded entries=%#v maxID=%d, want one valid entry with id 7", entries, maxID)
	}
}

func TestPersistedLogRehashesLegacyShaPrefixedCacheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	legacy := requestLogEntry{ID: 1, PromptCacheKey: "sha256:private-value"}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, _, err := loadPersistedEntries(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].PromptCacheKey == legacy.PromptCacheKey || !isRequestLogFingerprint(entries[0].PromptCacheKey) {
		t.Fatalf("legacy prompt cache key was not rehashed: %#v", entries)
	}
}

func TestRequestLogClearTruncatesPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	persist, err := openRequestLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer persist.file.Close()
	store := newRequestLogStore(10)
	store.persist = persist
	store.add(requestLogEntry{Method: "POST", Status: http.StatusOK})
	if err := store.clear(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() != 0 || store.snapshot(10).Retained != 0 {
		t.Fatalf("clear left file size=%d retained=%d", stat.Size(), store.snapshot(10).Retained)
	}
}

func TestOpenRequestLogFileTightensPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission modes are not available on Windows")
	}
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	persist, err := openRequestLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer persist.file.Close()
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := stat.Mode().Perm(); got != 0o600 {
		t.Fatalf("request log mode = %o, want 600", got)
	}
}

func TestLoadPersistedEntriesDisabledDoesNotReadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2*1024*1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, maxID, err := loadPersistedEntries(path, 0)
	if err != nil || entries != nil || maxID != 0 {
		t.Fatalf("disabled load returned entries=%v maxID=%d err=%v", entries, maxID, err)
	}
}

func TestHealthOmitsAccountMetadata(t *testing.T) {
	pool := newAccountPool([]string{"/private/sentinel-account/auth.json"}, time.Minute, http.DefaultClient)
	pool.accounts[0].cool(time.Now(), time.Now().Add(time.Minute), "sentinel-cooldown-reason")
	proxy := &responsesProxy{pool: pool}
	recorder := httptest.NewRecorder()
	proxy.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	body := recorder.Body.String()
	for _, secret := range []string{"sentinel-account", "sentinel-cooldown-reason", `"accounts":`} {
		if strings.Contains(body, secret) {
			t.Fatalf("health response exposed %q: %s", secret, body)
		}
	}
	for _, want := range []string{`"accounts_total":1`, `"accounts_available":0`} {
		if !strings.Contains(body, want) {
			t.Fatalf("health response = %s, want %s", body, want)
		}
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAuthManagerSerializesConcurrentRefresh(t *testing.T) {
	expired := fakeAccessToken(t, "acct_test", time.Now().Add(-time.Minute))
	refreshed := fakeAccessToken(t, "acct_test", time.Now().Add(time.Hour))
	authFile := filepath.Join(t.TempDir(), "auth.json")
	document := map[string]any{"tokens": map[string]any{
		"access_token": expired, "refresh_token": "refresh-old", "account_id": "acct_test",
	}}
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	var refreshCalls atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		refreshCalls.Add(1)
		body := `{"access_token":"` + refreshed + `","refresh_token":"refresh-new","expires_in":3600}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	manager := &authManager{authFile: authFile, refreshSkew: time.Minute, client: client}

	const workers = 12
	start := make(chan struct{})
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			material, err := manager.current(context.Background())
			if err == nil && material.AccessToken != refreshed {
				err = &unexpectedAccessTokenError{}
			}
			errs <- err
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

type unexpectedAccessTokenError struct{}

func (*unexpectedAccessTokenError) Error() string { return "current returned the wrong access token" }
