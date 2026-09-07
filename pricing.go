package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const longContextThresholdTokens int64 = 272_000

// modelPricing holds USD prices per one million tokens.
type modelPricing struct {
	InputPerM                   float64 `json:"input"`
	CachedPerM                  float64 `json:"cached_input"`
	CacheWritePerM              float64 `json:"cache_write"`
	OutputPerM                  float64 `json:"output"`
	LongContextThreshold        int64   `json:"-"`
	LongContextInputMultiplier  float64 `json:"-"`
	LongContextOutputMultiplier float64 `json:"-"`
}

type modelPricingOverride struct {
	InputPerM      *float64 `json:"input"`
	CachedPerM     *float64 `json:"cached_input"`
	CacheWritePerM *float64 `json:"cache_write"`
	OutputPerM     *float64 `json:"output"`
}

func longContextModelPricing(input, cached, cacheWrite, output float64) modelPricing {
	return modelPricing{
		InputPerM:                   input,
		CachedPerM:                  cached,
		CacheWritePerM:              cacheWrite,
		OutputPerM:                  output,
		LongContextThreshold:        longContextThresholdTokens,
		LongContextInputMultiplier:  2,
		LongContextOutputMultiplier: 1.5,
	}
}

// defaultModelPricing mirrors OpenAI API list prices (USD per 1M tokens).
// Costs are estimates of equivalent API spend; ChatGPT-plan requests are not
// actually billed per token.
var defaultModelPricing = map[string]modelPricing{
	"gpt-6-astra":   {InputPerM: 10.00, CachedPerM: 1.00, CacheWritePerM: 10.00, OutputPerM: 50.00},
	"gpt-5.6":       longContextModelPricing(4.00, 0.40, 4.00, 20.00),
	"gpt-5.6-sol":   longContextModelPricing(4.00, 0.40, 4.00, 20.00),
	"gpt-5.6-terra": longContextModelPricing(2.00, 0.20, 2.00, 12.00),
	"gpt-5.6-luna":  longContextModelPricing(0.20, 0.02, 0.20, 1.20),
	"gpt-5.5":       longContextModelPricing(5.00, 0.50, 5.00, 30.00),
	"gpt-5.4":       longContextModelPricing(2.50, 0.25, 2.50, 15.00),
	"gpt-5.4-mini":  {InputPerM: 0.75, CachedPerM: 0.075, CacheWritePerM: 0.75, OutputPerM: 4.50},
	"gpt-5.3-codex": {InputPerM: 1.75, CachedPerM: 0.175, CacheWritePerM: 1.75, OutputPerM: 14.00},
}

// loadModelPricing returns the default table merged with any overrides from
// CODEX_AUTH_BROKER_PRICING, a JSON object like
// {"gpt-5.5":{"input":5,"cached_input":0.5,"cache_write":5,"output":30}}.
func loadModelPricing() (map[string]modelPricing, error) {
	table := make(map[string]modelPricing, len(defaultModelPricing))
	for model, pricing := range defaultModelPricing {
		table[model] = pricing
	}
	raw := strings.TrimSpace(os.Getenv("CODEX_AUTH_BROKER_PRICING"))
	if raw == "" {
		return table, nil
	}
	var overrides map[string]modelPricingOverride
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		return nil, fmt.Errorf("invalid CODEX_AUTH_BROKER_PRICING: %w", err)
	}
	normalized := make(map[string]modelPricingOverride, len(overrides))
	for model, override := range overrides {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			return nil, fmt.Errorf("invalid CODEX_AUTH_BROKER_PRICING: model id must not be empty")
		}
		if _, exists := normalized[model]; exists {
			return nil, fmt.Errorf("invalid CODEX_AUTH_BROKER_PRICING: duplicate model id %q after normalization", model)
		}
		normalized[model] = override
	}
	for model, override := range normalized {
		pricing := table[model]
		if override.InputPerM != nil {
			pricing.InputPerM = *override.InputPerM
		}
		if override.CachedPerM != nil {
			pricing.CachedPerM = *override.CachedPerM
		}
		if override.CacheWritePerM != nil {
			pricing.CacheWritePerM = *override.CacheWritePerM
		} else if _, exists := table[model]; !exists {
			pricing.CacheWritePerM = pricing.InputPerM
		}
		if override.OutputPerM != nil {
			pricing.OutputPerM = *override.OutputPerM
		}
		if pricing.InputPerM < 0 || pricing.CachedPerM < 0 || pricing.CacheWritePerM < 0 || pricing.OutputPerM < 0 {
			return nil, fmt.Errorf("invalid CODEX_AUTH_BROKER_PRICING for %q: prices must be nonnegative", model)
		}
		table[model] = pricing
	}
	return table, nil
}

// lookupModelPricing matches the model exactly, then accepts only a dated
// snapshot suffix such as gpt-5.4-2026-03-05. Arbitrary prefix matching would
// misprice distinct families such as gpt-5.4-pro and gpt-5.4-nano.
func lookupModelPricing(table map[string]modelPricing, model string) (modelPricing, bool) {
	_, pricing, ok := resolveModelPricing(table, model)
	return pricing, ok
}

func resolveModelPricing(table map[string]modelPricing, model string) (string, modelPricing, bool) {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return "", modelPricing{}, false
	}
	if pricing, ok := table[model]; ok {
		return model, pricing, true
	}
	bestName := ""
	var best modelPricing
	for candidate, pricing := range table {
		if isDatedModelSnapshot(model, candidate) && len(candidate) > len(bestName) {
			bestName = candidate
			best = pricing
		}
	}
	return bestName, best, bestName != ""
}

func isDatedModelSnapshot(model, candidate string) bool {
	prefix := candidate + "-"
	if !strings.HasPrefix(model, prefix) {
		return false
	}
	date := strings.TrimPrefix(model, prefix)
	if len(date) != len("2006-01-02") {
		return false
	}
	_, err := time.Parse("2006-01-02", date)
	return err == nil
}

// estimateCostUSD prices one request from its token usage. Cached tokens are
// a subset of input tokens and billed at the cached rate. Returns nil when no
// pricing is known or no token counts were reported.
func estimateCostUSD(table map[string]modelPricing, model string, usage tokenUsage) *float64 {
	return estimateCostUSDForTier(table, model, "", usage)
}

// estimateCostUSDForTier applies the public API-equivalent service-tier rate.
// Fast requests use the "priority" wire value and cost twice the standard rate.
func estimateCostUSDForTier(table map[string]modelPricing, model, serviceTier string, usage tokenUsage) *float64 {
	pricing, ok := lookupModelPricing(table, model)
	if !ok {
		return nil
	}
	return estimatePricedUsage(pricing, serviceTier, usage)
}

func estimatePricedUsage(pricing modelPricing, serviceTier string, usage tokenUsage) *float64 {
	if usage.InputTokens == nil && usage.OutputTokens == nil {
		return nil
	}
	var input, output, cached, cacheWrite float64
	if usage.InputTokens != nil {
		input = float64(*usage.InputTokens)
	}
	if usage.OutputTokens != nil {
		output = float64(*usage.OutputTokens)
	}
	if usage.CachedTokens != nil {
		cached = float64(*usage.CachedTokens)
	}
	if usage.CacheWriteTokens != nil {
		cacheWrite = float64(*usage.CacheWriteTokens)
	}
	if cached > input {
		cached = input
	}
	if cacheWrite > input-cached {
		cacheWrite = input - cached
	}
	inputMultiplier := 1.0
	outputMultiplier := 1.0
	if usage.InputTokens != nil && pricing.LongContextThreshold > 0 && *usage.InputTokens > pricing.LongContextThreshold {
		inputMultiplier = pricing.LongContextInputMultiplier
		outputMultiplier = pricing.LongContextOutputMultiplier
	}
	cost := ((input-cached-cacheWrite)*pricing.InputPerM +
		cached*pricing.CachedPerM +
		cacheWrite*pricing.CacheWritePerM) * inputMultiplier
	cost += output * pricing.OutputPerM * outputMultiplier
	cost /= 1e6
	if strings.EqualFold(strings.TrimSpace(serviceTier), "priority") || strings.EqualFold(strings.TrimSpace(serviceTier), "fast") {
		cost *= 2
	}
	return &cost
}

// estimateAPICostUSDForTier restores public API cache-write and long-context
// rates where Codex plan usage has pricing exceptions.
func estimateAPICostUSDForTier(table map[string]modelPricing, model, serviceTier string, usage tokenUsage) *float64 {
	name, pricing, ok := resolveModelPricing(table, model)
	if !ok {
		return nil
	}
	if name == "gpt-6-astra" || name == "gpt-5.6" || strings.HasPrefix(name, "gpt-5.6-") {
		pricing.CacheWritePerM = pricing.InputPerM * 1.25
	}
	if name == "gpt-6-astra" {
		pricing.LongContextThreshold = longContextThresholdTokens
		pricing.LongContextInputMultiplier = 2
		pricing.LongContextOutputMultiplier = 1.5
	}
	return estimatePricedUsage(pricing, serviceTier, usage)
}
