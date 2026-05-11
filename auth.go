package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexTokenURL = "https://auth.openai.com/oauth/token"
)

type authManager struct {
	authFile    string
	refreshSkew time.Duration
	client      *http.Client
}

type accessMaterial struct {
	AccessToken string
	AccountID   string
	ExpiresAt   time.Time
	ExpiresIn   int64
	IssuedAt    time.Time
	Refreshed   bool
}

type authDocument struct {
	raw    map[string]any
	tokens tokenSet
}

type tokenSet struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (m *authManager) current(ctx context.Context) (accessMaterial, error) {
	unlock, err := lockFile(m.authFile + ".lock")
	if err != nil {
		return accessMaterial{}, err
	}
	defer unlock()

	doc, err := readAuthDocument(m.authFile)
	if err != nil {
		return accessMaterial{}, err
	}
	expiresAt, err := jwtExpiresAt(doc.tokens.AccessToken)
	if err != nil {
		return accessMaterial{}, err
	}
	if expiresAt.After(time.Now().Add(m.refreshSkew)) {
		return materialFromDocument(doc, expiresAt, false), nil
	}
	if strings.TrimSpace(doc.tokens.RefreshToken) == "" {
		return accessMaterial{}, errors.New("Codex auth file is missing refresh_token")
	}
	refreshed, err := m.refresh(ctx, doc.tokens.RefreshToken)
	if err != nil {
		return accessMaterial{}, err
	}
	newAccessExpiresAt, err := jwtExpiresAt(refreshed.AccessToken)
	if err != nil {
		return accessMaterial{}, err
	}
	doc.tokens.AccessToken = refreshed.AccessToken
	if strings.TrimSpace(refreshed.RefreshToken) != "" {
		doc.tokens.RefreshToken = refreshed.RefreshToken
	}
	accountID, err := accountIDFromAccessToken(refreshed.AccessToken)
	if err != nil {
		return accessMaterial{}, err
	}
	doc.tokens.AccountID = accountID
	if err := writeAuthDocument(m.authFile, doc); err != nil {
		return accessMaterial{}, err
	}
	return materialFromDocument(doc, newAccessExpiresAt, true), nil
}

func (m *authManager) refresh(ctx context.Context, refreshToken string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", codexClientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenResponse{}, fmt.Errorf("token refresh failed with HTTP %d%s", resp.StatusCode, oauthErrorSuffix(body))
	}
	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return tokenResponse{}, err
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return tokenResponse{}, errors.New("token refresh response missing access_token")
	}
	return parsed, nil
}

func readAuthDocument(path string) (authDocument, error) {
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		return authDocument{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(rawBytes, &raw); err != nil {
		return authDocument{}, err
	}
	tokensRaw, ok := raw["tokens"].(map[string]any)
	if !ok {
		return authDocument{}, errors.New("auth.json is missing tokens")
	}
	tokens := tokenSet{
		IDToken:      stringField(tokensRaw, "id_token"),
		AccessToken:  stringField(tokensRaw, "access_token"),
		RefreshToken: stringField(tokensRaw, "refresh_token"),
		AccountID:    stringField(tokensRaw, "account_id"),
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return authDocument{}, errors.New("auth.json is missing access_token")
	}
	if strings.TrimSpace(tokens.AccountID) == "" {
		accountID, err := accountIDFromAccessToken(tokens.AccessToken)
		if err != nil {
			return authDocument{}, err
		}
		tokens.AccountID = accountID
	}
	return authDocument{raw: raw, tokens: tokens}, nil
}

func writeAuthDocument(path string, doc authDocument) error {
	tokensRaw, ok := doc.raw["tokens"].(map[string]any)
	if !ok {
		tokensRaw = map[string]any{}
		doc.raw["tokens"] = tokensRaw
	}
	tokensRaw["id_token"] = doc.tokens.IDToken
	tokensRaw["access_token"] = doc.tokens.AccessToken
	tokensRaw["refresh_token"] = doc.tokens.RefreshToken
	tokensRaw["account_id"] = doc.tokens.AccountID
	doc.raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc.raw); err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".auth.json.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func materialFromDocument(doc authDocument, expiresAt time.Time, refreshed bool) accessMaterial {
	issuedAt, _ := jwtIssuedAt(doc.tokens.AccessToken)
	now := time.Now().UTC()
	return accessMaterial{
		AccessToken: doc.tokens.AccessToken,
		AccountID:   doc.tokens.AccountID,
		ExpiresAt:   expiresAt,
		ExpiresIn:   secondsUntil(expiresAt, now),
		IssuedAt:    issuedAt,
		Refreshed:   refreshed,
	}
}

func jwtExpiresAt(token string) (time.Time, error) {
	payload, err := jwtPayload(token)
	if err != nil {
		return time.Time{}, err
	}
	exp, ok := numericField(payload, "exp")
	if !ok {
		return time.Time{}, errors.New("JWT is missing exp")
	}
	return time.Unix(int64(exp), 0).UTC(), nil
}

func jwtIssuedAt(token string) (time.Time, error) {
	payload, err := jwtPayload(token)
	if err != nil {
		return time.Time{}, err
	}
	iat, ok := numericField(payload, "iat")
	if !ok {
		return time.Time{}, errors.New("JWT is missing iat")
	}
	return time.Unix(int64(iat), 0).UTC(), nil
}

func accountIDFromAccessToken(token string) (string, error) {
	payload, err := jwtPayload(token)
	if err != nil {
		return "", err
	}
	authRaw, ok := payload["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		return "", errors.New("access token is missing ChatGPT auth claim")
	}
	accountID := stringField(authRaw, "chatgpt_account_id")
	if accountID == "" {
		return "", errors.New("access token is missing chatgpt_account_id")
	}
	return accountID, nil
}

func jwtPayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func oauthErrorSuffix(body []byte) string {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	code := stringField(parsed, "error")
	description := stringField(parsed, "error_description")
	switch {
	case code == "" && description == "":
		return ""
	case code == "":
		return ": " + redactTokenLikeText(description)
	case description == "":
		return ": " + redactTokenLikeText(code)
	default:
		return ": " + redactTokenLikeText(code) + ": " + redactTokenLikeText(description)
	}
}

func secretFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])[:12]
}

func numericField(m map[string]any, key string) (float64, bool) {
	switch value := m[key].(type) {
	case float64:
		return value, true
	case int64:
		return float64(value), true
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
