package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadConfigIgnoresOpenAIAPIKey(t *testing.T) {
	t.Setenv("CODEX_AUTH_BROKER_API_KEY", "")
	t.Setenv("CODEX_AUTH_BROKER_API_KEY_FILE", "")
	t.Setenv("OPENAI_API_KEY", "unrelated-openai-key")
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.apiKey != "" {
		t.Fatalf("api key = %q, want unrelated OPENAI_API_KEY ignored", cfg.apiKey)
	}
}

func TestLoadConfigUsesBrokerAPIKey(t *testing.T) {
	t.Setenv("CODEX_AUTH_BROKER_API_KEY", "broker-key")
	cfg, err := loadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.apiKey != "broker-key" {
		t.Fatalf("api key = %q, want broker-key", cfg.apiKey)
	}
}

func TestLoadConfigExplicitAPIKeyFileOverridesEnvironmentKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.key")
	if err := os.WriteFile(path, []byte("file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_AUTH_BROKER_API_KEY", "environment-key")
	cfg, err := loadConfig([]string{"--api-key-file", path})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.apiKey != "file-key" {
		t.Fatalf("api key = %q, want explicit file key", cfg.apiKey)
	}
}

func TestLoadConfigRejectsUnexpectedArgumentsAndNegativeDurations(t *testing.T) {
	for _, args := range [][]string{
		{"accidental", "--listen", "127.0.0.1:9999"},
		{"--refresh-skew", "-1s"},
		{"--timeout", "-1s"},
	} {
		if _, err := loadConfig(args); err == nil {
			t.Fatalf("loadConfig(%q) succeeded", args)
		}
	}
}

func TestLoadConfigRejectsPermissiveAPIKeyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes do not represent ACL privacy")
	}
	path := filepath.Join(t.TempDir(), "client.key")
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_AUTH_BROKER_API_KEY", "")
	t.Setenv("CODEX_AUTH_BROKER_API_KEY_FILE", "")
	if _, err := loadConfig([]string{"--api-key-file", path}); err == nil {
		t.Fatal("API key file with group or world permissions was accepted")
	}
}

func TestLoadConfigRejectsAuthFileAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available on Windows")
	}
	directory := t.TempDir()
	realPath := filepath.Join(directory, "auth.json")
	aliasPath := filepath.Join(directory, "alias.json")
	if err := os.WriteFile(realPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_AUTH_FILES", "")
	if _, err := loadConfig([]string{"--auth-files", realPath + "," + aliasPath}); err == nil {
		t.Fatal("filesystem-identical auth files were accepted")
	}
}

func TestSystemdInstallerUsesSelectedBinaryAndScriptRelativeAssets(t *testing.T) {
	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin with spaces and 100%")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "codex-auth-broker")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	systemctlLog := filepath.Join(tempDir, "systemctl.log")
	systemctl := filepath.Join(tempDir, "systemctl")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SYSTEMCTL_LOG\"\n"
	if err := os.WriteFile(systemctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configHome := filepath.Join(tempDir, "config")
	stateDir := filepath.Join(tempDir, ".codex-auth-broker")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	clientKey := filepath.Join(stateDir, "client.key")
	if err := os.WriteFile(clientKey, []byte("existing-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", filepath.Join(workDir, "scripts", "install-systemd-user.sh"))
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(),
		"BIN="+binary,
		"HOME="+tempDir,
		"XDG_CONFIG_HOME="+configHome,
		"SYSTEMCTL_LOG="+systemctlLog,
		"PATH="+tempDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("installer failed: %v\n%s", err, output)
	}
	unit, err := os.ReadFile(filepath.Join(configHome, "systemd", "user", "codex-auth-broker.service"))
	if err != nil {
		t.Fatal(err)
	}
	systemdBinary := strings.ReplaceAll(binary, "%", "%%")
	if !strings.Contains(string(unit), `ExecStart="`+systemdBinary+`" serve`) {
		t.Fatalf("unit did not use selected binary:\n%s", unit)
	}
	if strings.Contains(string(unit), "CODEX_AUTH_BROKER_PROMPT_CACHE_KEY") {
		t.Fatalf("unit forces a shared prompt cache key:\n%s", unit)
	}
	stat, err := os.Stat(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := stat.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("existing client key mode = %04o, want 0600", permissions)
	}
}
