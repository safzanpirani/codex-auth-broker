package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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

func canonicalAuthPath(path string) (string, error) {
	expanded, err := expandPath(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	if expanded == "" {
		return "", errors.New("auth file path must not be empty")
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return absolute, nil
	}
	return "", err
}

func readSecretFile(path string) (string, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return "", err
	}
	raw, _, err := readPrivateFile(expanded)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return "", fmt.Errorf("secret file %s is empty", expanded)
	}
	return secret, nil
}

func readPrivateFile(path string) ([]byte, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("secret file %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" {
		if permissions := stat.Mode().Perm(); permissions&0o077 != 0 {
			return nil, nil, fmt.Errorf("secret file %s has permissions %04o; use 0600", path, permissions)
		}
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	return raw, stat, nil
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
