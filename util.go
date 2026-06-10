package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func stringField(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return strings.TrimSpace(value)
}

func secondsUntil(t, now time.Time) int64 {
	seconds := int64(t.Sub(now).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func defaultAuthFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".codex", "auth.json")
	}
	return filepath.Join(home, ".codex", "auth.json")
}

func defaultRequestLogFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".codex-auth-broker", "requests.jsonl")
	}
	return filepath.Join(home, ".codex-auth-broker", "requests.jsonl")
}

func expandPath(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func readSecretFile(path string) (string, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(expanded)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", fmt.Errorf("secret file %s is empty", expanded)
	}
	return secret, nil
}

func redactTokenLikeText(value string) string {
	fields := strings.Fields(value)
	for i, field := range fields {
		trimmed := strings.Trim(field, `"'.,;:()[]{}<>`)
		if strings.Count(trimmed, ".") == 2 && len(trimmed) > 40 {
			fields[i] = strings.Replace(field, trimmed, "[redacted-jwt]", 1)
			continue
		}
		if len(trimmed) > 60 && strings.IndexFunc(trimmed, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' || r == '~' || r == '+' || r == '/' || r == '=' ||
				(r >= '0' && r <= '9') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= 'a' && r <= 'z'))
		}) == -1 {
			fields[i] = strings.Replace(field, trimmed, "[redacted-token]", 1)
		}
	}
	return strings.Join(fields, " ")
}

func authFilePermissionWarning(path string) string {
	stat, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mode := stat.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Sprintf("auth file %s is accessible by group or other users: mode %s", path, mode.String())
	}
	return ""
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		if errors.Is(err, os.ErrInvalid) {
			return nil
		}
		return err
	}
	return nil
}
