package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestImageBackingModelConfiguration(t *testing.T) {
	t.Setenv("CODEX_AUTH_BROKER_IMAGE_RESPONSES_MODEL", "gpt-5.4")
	cfg, err := loadConfig(nil)
	if err != nil || cfg.imageResponsesModel != "gpt-5.4" {
		t.Fatalf("environment config: %v", err)
	}
	cfg, err = loadConfig([]string{"--image-responses-model", "gpt-5.5"})
	if err != nil || cfg.imageResponsesModel != "gpt-5.5" {
		t.Fatalf("flag config: %v", err)
	}
	if _, err := loadConfig([]string{"--image-responses-model", " "}); err == nil {
		t.Fatal("blank backing model accepted")
	}
}

func TestShutdownTimeoutConfiguration(t *testing.T) {
	for _, test := range []struct {
		name    string
		env     string
		args    []string
		want    time.Duration
		invalid bool
	}{
		{name: "default", want: 30 * time.Second},
		{name: "environment", env: "1m", want: time.Minute},
		{name: "flag precedence", env: "1m", args: []string{"--shutdown-timeout", "2s"}, want: 2 * time.Second},
		{name: "immediate", args: []string{"--shutdown-timeout", "0"}},
		{name: "negative", env: "-1s", invalid: true},
		{name: "malformed", env: "later", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CODEX_AUTH_BROKER_SHUTDOWN_TIMEOUT", test.env)
			cfg, err := loadConfig(test.args)
			if test.invalid {
				if err == nil {
					t.Fatal("invalid shutdown timeout accepted")
				}
				return
			}
			if err != nil || cfg.shutdownTimeout != test.want {
				t.Fatalf("timeout %s, want %s; error=%v", cfg.shutdownTimeout, test.want, err)
			}
		})
	}
}

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

func TestRequestLogEnvironmentAndFlagPrecedence(t *testing.T) {
	t.Setenv("CODEX_AUTH_BROKER_REQUEST_LOG_FILE", "saved-original")
	if err := os.Unsetenv("CODEX_AUTH_BROKER_REQUEST_LOG_FILE"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(nil)
	if err != nil || cfg.requestLogFile != defaultRequestLogFile() {
		t.Fatalf("unset log setting did not use default: %v", err)
	}
	for _, test := range []struct {
		env  string
		args []string
		want string
	}{
		{env: "", want: ""},
		{env: "custom.jsonl", want: "custom.jsonl"},
		{env: "custom.jsonl", args: []string{"--request-log-file", ""}, want: ""},
		{env: "", args: []string{"--request-log-file", "flag.jsonl"}, want: "flag.jsonl"},
	} {
		t.Setenv("CODEX_AUTH_BROKER_REQUEST_LOG_FILE", test.env)
		cfg, err := loadConfig(test.args)
		if err != nil || cfg.requestLogFile != test.want {
			t.Fatalf("log path = %q, want %q; error=%v", cfg.requestLogFile, test.want, err)
		}
	}
}

func TestRequestLogByteLimitConfiguration(t *testing.T) {
	t.Setenv("CODEX_AUTH_BROKER_REQUEST_LOG_MAX_BYTES", "")
	cfg, err := loadConfig(nil)
	if err != nil || cfg.requestLogMaxBytes != defaultRequestLogMaxBytes {
		t.Fatalf("default log cap = %d, error=%v", cfg.requestLogMaxBytes, err)
	}
	t.Setenv("CODEX_AUTH_BROKER_REQUEST_LOG_MAX_BYTES", "4096")
	cfg, err = loadConfig(nil)
	if err != nil || cfg.requestLogMaxBytes != 4096 {
		t.Fatalf("environment log cap = %d, error=%v", cfg.requestLogMaxBytes, err)
	}
	cfg, err = loadConfig([]string{"--request-log-max-bytes", "0"})
	if err != nil || cfg.requestLogMaxBytes != 0 {
		t.Fatalf("explicit unlimited log cap = %d, error=%v", cfg.requestLogMaxBytes, err)
	}
	for _, value := range []string{"-1", "not-an-integer"} {
		t.Setenv("CODEX_AUTH_BROKER_REQUEST_LOG_MAX_BYTES", value)
		if _, err := loadConfig(nil); err == nil {
			t.Fatalf("accepted invalid log cap %q", value)
		}
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
	if runtime.GOOS == "windows" {
		t.Skip("systemd and Unix permission modes are not available on Windows")
	}
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
