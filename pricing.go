package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// modelPricing holds USD prices per one million tokens.
type modelPricing struct {
	InputPerM  float64 `json:"input"`
	CachedPerM float64 `json:"cached_input"`
	OutputPerM float64 `json:"output"`
}

// defaultModelPricing mirrors OpenAI API list prices (USD per 1M tokens).
// Costs are estimates of equivalent API spend; ChatGPT-plan requests are not
// actually billed per token.
var defaultModelPricing = map[string]modelPricing{
	"gpt-5.6-sol":   {InputPerM: 5.00, CachedPerM: 0.50, OutputPerM: 30.00},
	"gpt-5.6-terra": {InputPerM: 2.50, CachedPerM: 0.25, OutputPerM: 15.00},
	"gpt-5.6-luna":  {InputPerM: 1.00, CachedPerM: 0.10, OutputPerM: 6.00},
	"gpt-5.5":       {InputPerM: 5.00, CachedPerM: 0.50, OutputPerM: 30.00},
	"gpt-5.4":       {InputPerM: 2.50, CachedPerM: 0.25, OutputPerM: 15.00},
	"gpt-5.4-mini":  {InputPerM: 0.75, CachedPerM: 0.075, OutputPerM: 4.50},
	"gpt-5.3-codex": {InputPerM: 1.75, CachedPerM: 0.175, OutputPerM: 14.00},
}

// loadModelPricing returns the default table merged with any overrides from
// CODEX_AUTH_BROKER_PRICING, a JSON object like
// {"gpt-5.5":{"input":5,"cached_input":0.5,"output":30}}.
func loadModelPricing() (map[string]modelPricing, error) {
	table := make(map[string]modelPricing, len(defaultModelPricing))
	for model, pricing := range defaultModelPricing {
		table[model] = pricing
	}
	raw := strings.TrimSpace(os.Getenv("CODEX_AUTH_BROKER_PRICING"))
	if raw == "" {
		return table, nil
	}
	var overrides map[string]modelPricing
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		return nil, fmt.Errorf("invalid CODEX_AUTH_BROKER_PRICING: %w", err)
	}
	for model, pricing := range overrides {
		table[strings.TrimSpace(model)] = pricing
	}
	return table, nil
}

// lookupModelPricing matches the model exactly, then by longest configured
// prefix so dated or suffixed ids (gpt-5.4-2026-01-15) still price.
func lookupModelPricing(table map[string]modelPricing, model string) (modelPricing, bool) {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return modelPricing{}, false
	}
	if pricing, ok := table[model]; ok {
		return pricing, true
	}
	bestLen := 0
	var best modelPricing
	for candidate, pricing := range table {
		if strings.HasPrefix(model, candidate) && len(candidate) > bestLen {
			bestLen = len(candidate)
			best = pricing
		}
	}
	return best, bestLen > 0
}

// estimateCostUSD prices one request from its token usage. Cached tokens are
// a subset of input tokens and billed at the cached rate. Returns nil when no
// pricing is known or no token counts were reported.
func estimateCostUSD(table map[string]modelPricing, model string, usage tokenUsage) *float64 {
	pricing, ok := lookupModelPricing(table, model)
	if !ok || (usage.InputTokens == nil && usage.OutputTokens == nil) {
		return nil
	}
	var input, output, cached float64
	if usage.InputTokens != nil {
		input = float64(*usage.InputTokens)
	}
	if usage.OutputTokens != nil {
		output = float64(*usage.OutputTokens)
	}
	if usage.CachedTokens != nil {
		cached = float64(*usage.CachedTokens)
	}
	if cached > input {
		cached = input
	}
	cost := ((input-cached)*pricing.InputPerM + cached*pricing.CachedPerM + output*pricing.OutputPerM) / 1e6
	return &cost
}
