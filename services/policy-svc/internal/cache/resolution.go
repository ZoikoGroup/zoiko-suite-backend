// Package cache keeps policy resolution off the database on the hot path —
// Doc 03 §8.1 "cache-accelerated", §3.9 local decision cache — while the
// authoritative rule source stays in Postgres ("policy decisions may be
// cached ... but authoritative rule source remains centralized").
//
// What is cached is the candidate set, not a decision: package resolve still
// runs on every request with that request's as_of and known_at, so a cached
// set never answers for the wrong time. An entry is reused only while the
// database's resolution watermark is unchanged; any lifecycle change on any
// instance (activation, suspension, retirement, scheduled promotion) bumps
// it, so a suspended policy stops deciding on the very next request — GCP
// domain negative path #30, "stale policy cache after policy suspension".
// If the watermark cannot be read, the cache is bypassed, never trusted.
package cache

import (
	"context"
	"sync"

	"zoiko.io/policy-svc/internal/domain"
)

// Source is the authoritative candidate store.
type Source interface {
	ResolutionCandidates(ctx context.Context, policyType string, tenantID, legalEntityID *string) ([]*domain.ApplicablePolicyVersion, error)
	ResolutionWatermark(ctx context.Context) (int64, error)
}

// maxEntries bounds memory: past it the cache is cleared and refills on
// demand. Keys are (type, tenant, entity), so the bound is reached only by a
// very large estate.
const maxEntries = 10000

type entry struct {
	watermark  int64
	candidates []*domain.ApplicablePolicyVersion
}

// Resolution caches candidate sets by (policy type, tenant, legal entity).
type Resolution struct {
	src Source

	mu      sync.Mutex
	entries map[string]entry
}

// New returns a cache over src.
func New(src Source) *Resolution {
	return &Resolution{src: src, entries: map[string]entry{}}
}

func key(policyType string, tenantID, legalEntityID *string) string {
	k := policyType + "|"
	if tenantID != nil {
		k += *tenantID
	}
	k += "|"
	if legalEntityID != nil {
		k += *legalEntityID
	}
	return k
}

// Candidates returns the candidate set for a request, from the cache when
// the watermark shows nothing has changed since it was loaded. The returned
// slice is shared and must not be modified.
func (c *Resolution) Candidates(ctx context.Context, policyType string, tenantID, legalEntityID *string) ([]*domain.ApplicablePolicyVersion, error) {
	wm, err := c.src.ResolutionWatermark(ctx)
	if err != nil {
		return c.src.ResolutionCandidates(ctx, policyType, tenantID, legalEntityID)
	}
	k := key(policyType, tenantID, legalEntityID)

	c.mu.Lock()
	e, ok := c.entries[k]
	c.mu.Unlock()
	if ok && e.watermark == wm {
		return e.candidates, nil
	}

	cands, err := c.src.ResolutionCandidates(ctx, policyType, tenantID, legalEntityID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.entries) >= maxEntries {
		c.entries = map[string]entry{}
	}
	c.entries[k] = entry{watermark: wm, candidates: cands}
	c.mu.Unlock()
	return cands, nil
}

// InvalidateTenant drops every entry for one tenant. The consumed events
// (entity.created, role.updated, authority.delegated) call it: none of them
// changes a policy version today, so the watermark would already keep the
// cache correct, but a tenant's context changing is the signal Doc 03 §8.1
// names, and re-reading is cheap.
func (c *Resolution) InvalidateTenant(tenantID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		// key layout: type|tenant|entity
		if tenantOf(k) == tenantID {
			delete(c.entries, k)
		}
	}
}

func tenantOf(k string) string {
	first := -1
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			if first < 0 {
				first = i
				continue
			}
			return k[first+1 : i]
		}
	}
	return ""
}

// Len reports the number of cached entries (for tests and metrics).
func (c *Resolution) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
