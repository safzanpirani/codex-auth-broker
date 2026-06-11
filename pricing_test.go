package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func int64Ptr(v int64) *int64 { return &v }

func TestEstimateCostUSD(t *testing.T) {
	usage := tokenUsage{
		InputTokens:  int64Ptr(100_000),
		OutputTokens: int64Ptr(10_000),
		CachedTokens: int64Ptr(40_000),
	}
	cost := estimateCostUSD(defaultModelPricing, "gpt-5.4", usage)
	if cost == nil {
		t.Fatal("expected cost for gpt-5.4")
	}
	// (60k * 2.50 + 40k * 0.25 + 10k * 15.00) / 1e6
	want := (60_000*2.50 + 40_000*0.25 + 10_000*15.00) / 1e6
	if math.Abs(*cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", *cost, want)
	}
}

func TestEstimateCostUSDPrefixAndUnknown(t *testing.T) {
	usage := tokenUsage{InputTokens: int64Ptr(1000), OutputTokens: int64Ptr(1000)}
	if cost := estimateCostUSD(defaultModelPricing, "gpt-5.4-mini-2026-01-15", usage); cost == nil {
		t.Fatal("expected prefix match for dated gpt-5.4-mini id")
	}
	if cost := estimateCostUSD(defaultModelPricing, "unknown-model", usage); cost != nil {
		t.Fatalf("expected nil cost for unknown model, got %v", *cost)
	}
	if cost := estimateCostUSD(defaultModelPricing, "gpt-5.5", tokenUsage{}); cost != nil {
		t.Fatalf("expected nil cost without token counts, got %v", *cost)
	}
}

func TestLookupModelPricingPrefersLongestPrefix(t *testing.T) {
	pricing, ok := lookupModelPricing(defaultModelPricing, "gpt-5.4-mini")
	if !ok || pricing.InputPerM != 0.75 {
		t.Fatalf("expected gpt-5.4-mini pricing, got %+v ok=%t", pricing, ok)
	}
}

func TestRequestLogPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "requests.jsonl")
	persist, err := openRequestLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store := newRequestLogStore(10)
	store.persist = persist
	for i := 0; i < 3; i++ {
		store.add(requestLogEntry{Method: "POST", Path: "/v1/responses", Model: "gpt-5.5", Status: 200})
	}
	if err := persist.file.Close(); err != nil {
		t.Fatal(err)
	}

	entries, maxID, err := loadPersistedEntries(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || maxID != 3 {
		t.Fatalf("got %d entries maxID=%d, want 2 entries maxID=3", len(entries), maxID)
	}
	if entries[0].ID != 2 || entries[1].ID != 3 {
		t.Fatalf("expected last two entries, got ids %d, %d", entries[0].ID, entries[1].ID)
	}

	restored := newRequestLogStore(10)
	restored.restore(entries, maxID)
	snapshot := restored.snapshot(0)
	if snapshot.Retained != 2 || snapshot.TotalSeen != 3 {
		t.Fatalf("restored snapshot retained=%d total=%d, want 2 and 3", snapshot.Retained, snapshot.TotalSeen)
	}
}

func TestCostSummaryWindows(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cost := 0.5
	persist, err := openRequestLogFile(filepath.Join(t.TempDir(), "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	store := newRequestLogStore(10)
	store.persist = persist
	old := requestLogEntry{StartedAt: now.Add(-48 * time.Hour).Format(time.RFC3339Nano), NormalizedModel: "gpt-5.5", CostUSD: &cost, InputTokens: int64Ptr(100)}
	recent := requestLogEntry{StartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), NormalizedModel: "gpt-5.4", CostUSD: &cost, InputTokens: int64Ptr(200)}
	store.add(old)
	store.add(recent)

	summary, err := store.costSummary(now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Source != "file" {
		t.Fatalf("source = %s, want file", summary.Source)
	}
	day := summary.Windows["24h"]
	if day.Requests != 1 || math.Abs(day.CostUSD-0.5) > 1e-9 || day.InputTokens != 200 {
		t.Fatalf("24h window = %+v, want 1 request / $0.5 / 200 input tokens", day)
	}
	all := summary.Windows["all"]
	if all.Requests != 2 || math.Abs(all.CostUSD-1.0) > 1e-9 || len(all.Models) != 2 {
		t.Fatalf("all window = %+v, want 2 requests / $1.0 / 2 models", all)
	}
}

func TestLoadPersistedEntriesMissingFile(t *testing.T) {
	entries, maxID, err := loadPersistedEntries(filepath.Join(t.TempDir(), "absent.jsonl"), 5)
	if err != nil || entries != nil || maxID != 0 {
		t.Fatalf("missing file should be empty, got entries=%v maxID=%d err=%v", entries, maxID, err)
	}
}

func TestLoadModelPricingOverride(t *testing.T) {
	t.Setenv("CODEX_AUTH_BROKER_PRICING", `{"my-model":{"input":1,"cached_input":0.1,"output":2}}`)
	table, err := loadModelPricing()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := table["my-model"]; !ok {
		t.Fatal("expected override model in table")
	}
	if _, ok := table["gpt-5.5"]; !ok {
		t.Fatal("defaults should be preserved")
	}
	_ = os.Unsetenv("CODEX_AUTH_BROKER_PRICING")
}
