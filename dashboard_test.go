package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchCodexUsageUsesLocalAccessAuth(t *testing.T) {
	accessToken := fakeAccessToken(t, "acct_test", time.Now().Add(time.Hour))
	authFile := filepath.Join(t.TempDir(), "auth.json")
	rawAuth := map[string]any{
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "refresh-not-used",
			"account_id":    "acct_test",
		},
	}
	rawBytes, err := json.Marshal(rawAuth)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, rawBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Fatalf("Authorization = %q, want bearer access token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "acct_test" {
			t.Fatalf("ChatGPT-Account-Id = %q, want acct_test", got)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"plan_type": "test",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": 12.5},
			},
		})
	}))
	defer server.Close()

	proxy := &responsesProxy{
		cfg: config{usageURL: server.URL},
		auth: &authManager{
			authFile:    authFile,
			refreshSkew: time.Minute,
			client:      server.Client(),
		},
		client: server.Client(),
	}
	usage, status, err := proxy.fetchCodexUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if usage["plan_type"] != "test" {
		t.Fatalf("plan_type = %#v, want test", usage["plan_type"])
	}
	if broker, ok := usage["_broker"].(map[string]any); !ok || broker["account_id_present"] != true {
		t.Fatalf("_broker = %#v, want account_id_present true", usage["_broker"])
	}
}

func fakeAccessToken(t *testing.T, accountID string, expiresAt time.Time) string {
	t.Helper()
	header := map[string]any{"alg": "none", "typ": "JWT"}
	payload := map[string]any{
		"exp": expiresAt.Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	}
	return encodeJWTPart(t, header) + "." + encodeJWTPart(t, payload) + ".signature"
}

func encodeJWTPart(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
