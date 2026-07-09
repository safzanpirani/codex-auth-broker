package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func printDoctor(cfg config, overall string, accounts []map[string]any) {
	response := map[string]any{
		"status":                 overall,
		"version":                version,
		"commit":                 commit,
		"listen":                 cfg.listen,
		"accounts":               accounts,
		"accounts_total":         len(accounts),
		"client_api_key_enabled": cfg.apiKey != "",
		"api_key_fingerprint":    optionalFingerprint(cfg.apiKey),
		"models":                 cfg.models,
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
