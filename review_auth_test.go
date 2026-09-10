package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAuthManagerCanceledContextDoesNotReadAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	manager := &authManager{authFile: filepath.Join(t.TempDir(), "missing.json")}
	if _, err := manager.current(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("current error = %v, want context.Canceled", err)
	}
}

func TestCanceledRefreshDoesNotCoolAccounts(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		for _, phase := range []string{"before refresh", "during refresh"} {
			t.Run(transport+"/"+phase, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if phase == "before refresh" {
					cancel()
				}
				refreshCalls := 0
				client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					refreshCalls++
					cancel()
					return nil, r.Context().Err()
				})}
				files := make([]string, 2)
				for i := range files {
					files[i] = filepath.Join(t.TempDir(), "auth.json")
					document, err := json.Marshal(map[string]any{"tokens": map[string]any{
						"access_token":  fakeAccessToken(t, "acct_test", time.Now().Add(-time.Hour)),
						"refresh_token": "synthetic-refresh",
						"account_id":    "acct_test",
					}})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(files[i], document, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				pool := newAccountPool(files, time.Minute, client)
				proxy := &responsesProxy{pool: pool, client: client}
				if transport == "http" {
					if _, failure := proxy.dispatchUpstream(ctx, nil, requestInfo{}, nil, nil); failure == nil {
						t.Fatal("canceled dispatch succeeded")
					}
				} else {
					if _, _, _, err := proxy.dialResponsesWebSocket(ctx, nil); !errors.Is(err, context.Canceled) {
						t.Fatalf("dial error = %v, want context.Canceled", err)
					}
				}
				if _, available := pool.availability(time.Now()); available != len(files) {
					t.Fatalf("available accounts = %d, want %d", available, len(files))
				}
				wantCalls := 0
				if phase == "during refresh" {
					wantCalls = 1
				}
				if refreshCalls != wantCalls {
					t.Fatalf("refresh calls = %d, want %d", refreshCalls, wantCalls)
				}
			})
		}
	}
}

func TestResponsesWebSocketCoolingPoolReturnsRetryAfter(t *testing.T) {
	for _, preCooled := range []bool{true, false} {
		t.Run(strconv.FormatBool(preCooled), func(t *testing.T) {
			upstreamCalls := 0
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				upstreamCalls++
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Retry-After": []string{"120"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rate limited"}}`)),
					Request:    r,
				}, nil
			})}
			pool := newAccountPool([]string{writeWebSocketTestAuth(t, "acct_one"), writeWebSocketTestAuth(t, "acct_two")}, time.Minute, client)
			if preCooled {
				for _, account := range pool.accounts {
					account.cool(time.Now(), time.Now().Add(2*time.Minute), "test")
				}
			}
			proxy := &responsesProxy{
				cfg: config{upstreamURL: "https://upstream.invalid/responses"}, pool: pool, client: client,
			}
			request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
			response := httptest.NewRecorder()
			proxy.handleResponsesWebSocket(response, request)
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", response.Code)
			}
			retryAfter, err := strconv.Atoi(response.Header().Get("Retry-After"))
			if err != nil || retryAfter < 115 || retryAfter > 120 {
				t.Fatalf("Retry-After = %q, want approximately 120 seconds", response.Header().Get("Retry-After"))
			}
			wantCalls := 2
			if preCooled {
				wantCalls = 0
			}
			if upstreamCalls != wantCalls {
				t.Fatalf("upstream calls = %d, want %d", upstreamCalls, wantCalls)
			}
		})
	}
}
