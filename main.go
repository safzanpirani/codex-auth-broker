package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListen          = "127.0.0.1:8317"
	defaultCursorListen    = "127.0.0.1:8318"
	defaultRefreshSkew     = 10 * time.Minute
	defaultHTTPTimeout     = 10 * time.Minute
	defaultPromptKey       = "factory-droid"
	defaultUpstreamURL     = "https://chatgpt.com/backend-api/codex/responses"
	defaultUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	defaultInstructions    = "You are a helpful coding assistant."
	defaultRequestLogLimit = 1000
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

type config struct {
	listen               string
	cursorListen         string
	authFile             string
	apiKey               string
	apiKeyFile           string
	promptCacheKey       string
	promptCacheRetention string
	refreshSkew          time.Duration
	upstreamURL          string
	usageURL             string
	models               []string
	timeout              time.Duration
	requestLogLimit      int
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "serve":
		if err := runServe(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			log.Fatal(err)
		}
	case "doctor":
		if err := runDoctor(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			log.Fatal(err)
		}
	case "version":
		fmt.Printf("codex-auth-broker %s commit=%s date=%s\n", valueOr(version, "dev"), valueOr(commit, "unknown"), valueOr(date, "unknown"))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func runServe(args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}
	auth := &authManager{
		authFile:    cfg.authFile,
		refreshSkew: cfg.refreshSkew,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	proxy := &responsesProxy{
		cfg:      cfg,
		auth:     auth,
		requests: newRequestLogStore(cfg.requestLogLimit),
		client: &http.Client{
			Timeout: cfg.timeout,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", proxy.handleDashboard)
	mux.HandleFunc("GET /dashboard", proxy.handleDashboard)
	mux.HandleFunc("GET /dashboard/api/requests", proxy.handleDashboardRequests)
	mux.HandleFunc("DELETE /dashboard/api/requests", proxy.handleDashboardRequests)
	mux.HandleFunc("GET /dashboard/api/usage", proxy.handleCodexUsage)
	mux.HandleFunc("GET /usage", proxy.handleCodexUsage)
	mux.HandleFunc("GET /healthz", proxy.handleHealth)
	mux.HandleFunc("GET /v1/models", proxy.handleModels)
	mux.HandleFunc("POST /v1/responses", proxy.handleResponses)
	mux.HandleFunc("POST /v1/chat/completions", proxy.handleChatCompletions)
	cursorMux := http.NewServeMux()
	cursorShim := newCursorShim(proxy)
	cursorMux.HandleFunc("/", cursorShim.handle)

	log.Printf("codex-auth-broker listening on %s", cfg.listen)
	log.Printf("using Codex auth file %s", cfg.authFile)
	log.Printf("upstream responses endpoint %s", cfg.upstreamURL)
	log.Printf("dashboard available at http://%s/dashboard", cfg.listen)
	if strings.TrimSpace(cfg.apiKey) == "" {
		log.Printf("client API key disabled; bind to localhost or a private interface only")
	}
	if strings.TrimSpace(cfg.cursorListen) != "" {
		log.Printf("cursor-agent shim listening on %s", cfg.cursorListen)
	}
	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	if strings.TrimSpace(cfg.cursorListen) == "" {
		return server.ListenAndServe()
	}
	cursorServer := &http.Server{
		Addr:              cfg.cursorListen,
		Handler:           cursorMux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	errs := make(chan error, 2)
	go func() { errs <- cursorServer.ListenAndServe() }()
	go func() { errs <- server.ListenAndServe() }()
	return <-errs
}

func runDoctor(args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}
	auth := &authManager{
		authFile:    cfg.authFile,
		refreshSkew: cfg.refreshSkew,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	access, err := auth.current(context.Background())
	if err != nil {
		return err
	}
	printDoctor(cfg, access)
	return nil
}

func loadConfig(args []string) (config, error) {
	cursorListen := defaultCursorListen
	if value, ok := os.LookupEnv("CODEX_AUTH_BROKER_CURSOR_LISTEN"); ok {
		cursorListen = strings.TrimSpace(value)
	}
	cfg := config{
		listen:               envOr("CODEX_AUTH_BROKER_LISTEN", defaultListen),
		cursorListen:         cursorListen,
		authFile:             envOr("CODEX_AUTH_FILE", defaultAuthFile()),
		apiKey:               firstNonEmptyEnv("CODEX_AUTH_BROKER_API_KEY", "OPENAI_API_KEY"),
		apiKeyFile:           envOr("CODEX_AUTH_BROKER_API_KEY_FILE", ""),
		promptCacheKey:       envOr("CODEX_AUTH_BROKER_PROMPT_CACHE_KEY", defaultPromptKey),
		promptCacheRetention: envOr("CODEX_AUTH_BROKER_PROMPT_CACHE_RETENTION", ""),
		upstreamURL:          envOr("CODEX_AUTH_BROKER_UPSTREAM_RESPONSES_URL", defaultUpstreamURL),
		usageURL:             envOr("CODEX_AUTH_BROKER_USAGE_URL", defaultUsageURL),
		refreshSkew:          defaultRefreshSkew,
		models:               defaultModels(),
		timeout:              defaultHTTPTimeout,
		requestLogLimit:      defaultRequestLogLimit,
	}
	if value := strings.TrimSpace(os.Getenv("CODEX_AUTH_BROKER_REFRESH_SKEW")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return cfg, fmt.Errorf("invalid CODEX_AUTH_BROKER_REFRESH_SKEW: %w", err)
		}
		cfg.refreshSkew = parsed
	}
	if value := strings.TrimSpace(os.Getenv("CODEX_AUTH_BROKER_MODELS")); value != "" {
		cfg.models = splitCSV(value)
	}
	if value := strings.TrimSpace(os.Getenv("CODEX_AUTH_BROKER_REQUEST_LOG_LIMIT")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return cfg, fmt.Errorf("invalid CODEX_AUTH_BROKER_REQUEST_LOG_LIMIT: %w", err)
		}
		cfg.requestLogLimit = parsed
	}

	fs := flag.NewFlagSet("codex-auth-broker", flag.ContinueOnError)
	skewValue := cfg.refreshSkew.String()
	timeoutValue := cfg.timeout.String()
	modelsValue := strings.Join(cfg.models, ",")
	fs.StringVar(&cfg.listen, "listen", cfg.listen, "listen address, for example 127.0.0.1:8317 or a Tailscale IP")
	fs.StringVar(&cfg.cursorListen, "cursor-listen", cfg.cursorListen, "optional cursor-agent Connect-RPC shim listen address; empty disables it")
	fs.StringVar(&cfg.authFile, "auth-file", cfg.authFile, "Codex auth.json path")
	fs.StringVar(&cfg.apiKey, "api-key", cfg.apiKey, "optional client-facing bearer key")
	fs.StringVar(&cfg.apiKeyFile, "api-key-file", cfg.apiKeyFile, "optional file containing client-facing bearer key")
	fs.StringVar(&cfg.promptCacheKey, "prompt-cache-key", cfg.promptCacheKey, "prompt_cache_key to inject when absent; empty disables injection")
	fs.StringVar(&cfg.promptCacheRetention, "prompt-cache-retention", cfg.promptCacheRetention, "prompt_cache_retention to inject when absent: in_memory or 24h")
	fs.StringVar(&cfg.upstreamURL, "upstream-responses-url", cfg.upstreamURL, "ChatGPT Codex Responses endpoint")
	fs.StringVar(&cfg.usageURL, "usage-url", cfg.usageURL, "ChatGPT Codex usage endpoint")
	fs.StringVar(&modelsValue, "models", modelsValue, "comma-separated model ids to return from /v1/models")
	fs.StringVar(&skewValue, "refresh-skew", skewValue, "refresh access token when it expires within this duration")
	fs.StringVar(&timeoutValue, "timeout", timeoutValue, "upstream request timeout")
	fs.IntVar(&cfg.requestLogLimit, "request-log-limit", cfg.requestLogLimit, "maximum in-memory dashboard request entries")
	fs.Usage = func() { usage(fs.Output()) }
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	expanded, err := expandPath(cfg.authFile)
	if err != nil {
		return cfg, err
	}
	cfg.authFile = expanded
	if strings.TrimSpace(cfg.apiKey) == "" && strings.TrimSpace(cfg.apiKeyFile) != "" {
		secret, err := readSecretFile(cfg.apiKeyFile)
		if err != nil {
			return cfg, err
		}
		cfg.apiKey = secret
	}
	cfg.models = splitCSV(modelsValue)
	if len(cfg.models) == 0 {
		return cfg, errors.New("at least one model must be configured")
	}
	parsedSkew, err := time.ParseDuration(skewValue)
	if err != nil {
		return cfg, fmt.Errorf("invalid refresh-skew: %w", err)
	}
	cfg.refreshSkew = parsedSkew
	parsedTimeout, err := time.ParseDuration(timeoutValue)
	if err != nil {
		return cfg, fmt.Errorf("invalid timeout: %w", err)
	}
	cfg.timeout = parsedTimeout
	if retention := strings.TrimSpace(cfg.promptCacheRetention); retention != "" && retention != "in_memory" && retention != "24h" {
		return cfg, errors.New("prompt-cache-retention must be empty, in_memory, or 24h")
	}
	if cfg.requestLogLimit < 0 {
		return cfg, errors.New("request-log-limit must be zero or greater")
	}
	return cfg, nil
}

func usage(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprint(w, `codex-auth-broker

Codex app-server powered auth bridge for Factory Droid and any client that
speaks the OpenAI Responses API.

Factory Droid first:
  codex-auth-broker serve --listen 127.0.0.1:8317

Commands:
  serve     Run the OpenAI-compatible /v1/responses proxy
  doctor    Validate local Codex auth and print redacted status
  version   Print build metadata

Common flags:
  --listen                 Listen address
  --cursor-listen          Cursor Agent shim listen address; empty disables it
  --auth-file              Codex auth.json path
  --api-key                Optional client-facing bearer key
  --api-key-file           Optional file containing client-facing bearer key
  --prompt-cache-key       Inject prompt_cache_key when clients omit it
  --prompt-cache-retention Inject prompt_cache_retention when clients omit it
  --request-log-limit      In-memory dashboard request history size
`)
}

func defaultModels() []string {
	return []string{
		"gpt-5.5",
		"gpt-5.5(low)",
		"gpt-5.5(medium)",
		"gpt-5.5(high)",
		"gpt-5.5(xhigh)",
		"gpt-5.4",
		"gpt-5.4(low)",
		"gpt-5.4(medium)",
		"gpt-5.4(high)",
		"gpt-5.4(xhigh)",
		"gpt-5.4-mini",
		"gpt-5.4-mini(low)",
		"gpt-5.4-mini(medium)",
		"gpt-5.4-mini(high)",
		"gpt-5.4-mini(xhigh)",
		"gpt-5.3-codex",
		"gpt-5.3-codex(low)",
		"gpt-5.3-codex(medium)",
		"gpt-5.3-codex(high)",
		"gpt-5.3-codex(xhigh)",
	}
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
