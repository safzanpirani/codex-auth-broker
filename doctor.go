package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func printDoctor(cfg config, access accessMaterial) {
	response := map[string]any{
		"status":                  "ok",
		"version":                 version,
		"commit":                  commit,
		"listen":                  cfg.listen,
		"auth_file":               cfg.authFile,
		"auth_file_warning":       authFilePermissionWarning(cfg.authFile),
		"account_id_present":      access.AccountID != "",
		"access_token_present":    access.AccessToken != "",
		"expires_at":              access.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"expires_in":              access.ExpiresIn,
		"issued_at":               access.IssuedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"refreshed_during_doctor": access.Refreshed,
		"client_api_key_enabled":  cfg.apiKey != "",
		"api_key_fingerprint":     optionalFingerprint(cfg.apiKey),
		"models":                  cfg.models,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(response); err != nil {
		fmt.Fprintf(os.Stderr, "encode doctor response: %v\n", err)
	}
}

func optionalFingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	return secretFingerprint(secret)
}
