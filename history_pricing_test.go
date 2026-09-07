package main

import (
	"math"
	"testing"
	"time"
)

func TestReviewHistoricalPricingAgreesAcrossRestoredRowsAndSummaries(t *testing.T) {
	f := reviewLog(t, 4096)
	old := newRequestLogStore(10)
	old.persist = f
	old.pricing = map[string]modelPricing{"gpt-6-astra": {InputPerM: 1, CachedPerM: 0.1, CacheWritePerM: 1, OutputPerM: 2}}
	entry := reviewEntry(1)
	entry.User = "test-user"
	entry.InputTokens, entry.OutputTokens = int64Ptr(400_000), int64Ptr(10_000)
	entry.CachedTokens, entry.CacheWriteTokens = int64Ptr(50_000), int64Ptr(20_000)
	entry.ServiceTier, entry.AppliedServiceTier = "priority", "default"
	old.add(entry)
	oldCost := *old.snapshot(1).RequestLog[0].CostUSD

	restored, maxID, err := loadPersistedEntries(f.path, 10)
	if err != nil {
		t.Fatal(err)
	}
	current := newRequestLogStore(10)
	current.pricing = defaultModelPricing
	current.persist = f
	current.restore(restored, maxID)
	row := current.snapshot(1).RequestLog[0]
	if row.CostUSD == nil || row.APICostUSD == nil || *row.CostUSD == oldCost || *row.APICostUSD <= *row.CostUSD {
		t.Fatal("restored row did not apply current Codex and API pricing")
	}
	for _, persisted := range []bool{true, false} {
		if !persisted {
			current.persist = nil
		}
		for _, apiPricing := range []bool{false, true} {
			summary, err := current.costSummary(time.Now().UTC(), apiPricing)
			if err != nil {
				t.Fatal(err)
			}
			want := row.CostUSD
			if apiPricing {
				want = row.APICostUSD
			}
			for _, window := range costWindows {
				got := summary.Windows[window.key]
				if got.Requests != 1 || got.Priced != 1 || math.Abs(got.CostUSD-*want) > 1e-9 {
					t.Fatalf("persist=%v api=%v window=%s cost=%g want=%g", persisted, apiPricing, window.key, got.CostUSD, *want)
				}
			}
		}
		users, err := current.userUsageSummary(time.Now().UTC(), 0, "all")
		if err != nil || len(users.Users) != 1 || math.Abs(users.Users[0].CostUSD-*row.CostUSD) > 1e-9 {
			t.Fatalf("per-user pricing disagreed for persist=%v, error=%v", persisted, err)
		}
	}
}

func TestReviewHistoricalPricingRemovesStaleEstimateForUnknownModel(t *testing.T) {
	staleCost := 123.0
	entry := reviewEntry(1)
	entry.Model = "unpriced-model"
	entry.CostUSD, entry.APICostUSD = &staleCost, &staleCost
	entry.InputTokens = int64Ptr(1000)
	f := reviewLog(t, 4096)
	if err := f.append(entry); err != nil {
		t.Fatal(err)
	}
	store := newRequestLogStore(10)
	store.persist, store.pricing = f, defaultModelPricing
	store.restore([]requestLogEntry{entry}, 1)
	row := store.snapshot(1).RequestLog[0]
	if row.CostUSD != nil || row.APICostUSD != nil {
		t.Fatal("unknown model retained historical estimate")
	}
	summary, err := store.costSummary(time.Now(), false)
	if err != nil || summary.Windows["all"].Priced != 0 || summary.Windows["all"].CostUSD != 0 {
		t.Fatalf("unknown model counted as priced, error=%v", err)
	}
}

func TestReviewHistoryWithoutPricingTablePreservesSuppliedEstimates(t *testing.T) {
	cost, apiCost := 12.0, 24.0
	entry := reviewEntry(1)
	entry.CostUSD, entry.APICostUSD = &cost, &apiCost
	store := newRequestLogStore(10)
	store.restore([]requestLogEntry{entry}, 1)
	if got := store.snapshot(1).RequestLog[0]; got.CostUSD == nil || *got.CostUSD != cost || *got.APICostUSD != apiCost {
		t.Fatal("metadata-only store changed supplied estimates")
	}
}

func TestReviewAPIPricingSnapshotPolicyDoesNotMutateTable(t *testing.T) {
	table := map[string]modelPricing{"gpt-6-astra": defaultModelPricing["gpt-6-astra"]}
	before := table["gpt-6-astra"]
	usage := tokenUsage{InputTokens: int64Ptr(400_000), OutputTokens: int64Ptr(10_000), CacheWriteTokens: int64Ptr(20_000)}
	base := estimateAPICostUSDForTier(table, "gpt-6-astra", "priority", usage)
	snapshot := estimateAPICostUSDForTier(table, "gpt-6-astra-2026-09-08", "priority", usage)
	if base == nil || snapshot == nil || *base != *snapshot {
		t.Fatal("dated snapshot lost API pricing policy")
	}
	if table["gpt-6-astra"] != before {
		t.Fatal("API estimate changed shared pricing table")
	}
	if unknown := estimateAPICostUSDForTier(table, "gpt-6-astra-pro", "priority", usage); unknown != nil {
		t.Fatal("unknown model inherited unrelated pricing")
	}
}
