package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// Catalogs belong to accounts, not model names: failover may change eligibility.
// A cold parallel burst shares one fetch; callers can cancel while waiting.
type accountTierCatalog struct {
	mu      sync.Mutex
	ready   chan struct{}
	models  []upstreamModel
	expires time.Time
	err     error
}

func (p *responsesProxy) accountSupportsTier(ctx context.Context, acct *account, model, tier string) bool {
	// Official Codex explicitly accepts Flex without catalog support.
	if tier != "priority" {
		return true
	}
	models, err := p.accountTierModels(ctx, acct)
	if err != nil {
		// Discovery outages must not change an otherwise valid API request.
		// The catalog fetch logs this unverified path once per cache refresh.
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	var selected *upstreamModel
	for i := range models {
		if strings.ToLower(models[i].Slug) == model {
			selected = &models[i]
			break
		}
	}
	if selected == nil {
		for i := range models {
			if isDatedModelSnapshot(model, strings.ToLower(models[i].Slug)) {
				selected = &models[i]
				break
			}
		}
	}
	if selected != nil {
		for _, supported := range selected.ServiceTiers {
			if supported.ID == tier {
				return true
			}
		}
	}
	log.Printf("service_tier %s is not advertised for model=%s account=%s; sending at the standard tier", tier, truncateLogField(model, 256), acct.label)
	return false
}

func (p *responsesProxy) accountTierModels(ctx context.Context, acct *account) ([]upstreamModel, error) {
	value, ok := p.tierCatalog.Load(acct)
	if !ok {
		value, _ = p.tierCatalog.LoadOrStore(acct, &accountTierCatalog{})
	}
	cache := value.(*accountTierCatalog)
	for {
		cache.mu.Lock()
		if time.Now().Before(cache.expires) {
			models, err := cache.models, cache.err
			cache.mu.Unlock()
			return models, err
		}
		if cache.ready != nil {
			ready := cache.ready
			cache.mu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		cache.ready = make(chan struct{})
		cache.mu.Unlock()

		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		models, err := p.fetchUpstreamModels(fetchCtx, acct)
		cancel()
		ttl := 5 * time.Minute
		if err != nil {
			ttl = 30 * time.Second
			log.Printf("service_tier catalog unavailable account=%s; forwarding requested tier without compatibility validation", acct.label)
		}
		if ctx.Err() != nil {
			ttl = 0 // One canceled caller must not poison another caller's lookup.
		}
		cache.mu.Lock()
		cache.models, cache.err, cache.expires = models, err, time.Now().Add(ttl)
		close(cache.ready)
		cache.ready = nil
		cache.mu.Unlock()
		return models, err
	}
}
