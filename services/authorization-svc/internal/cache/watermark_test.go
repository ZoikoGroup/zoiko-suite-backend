package cache_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// ZS-IAM-001 §19: the assignment and policy versions are in the key, so a
// write on another replica (which this process's generation counters never
// see) retires the cached entry as soon as the watermark moves.

func TestCache_WatermarkChangeRetiresEntries(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}, basis: "rbac:role=X"}
	c := newCache(inner, time.Minute)
	c.SetWatermark(7, 40, time.Now())

	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", inner.grantCalls)
	}
	// An assignment revoked through another replica: only the watermark moves.
	c.SetWatermark(7, 41, time.Now())
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 2 {
		t.Fatalf("calls = %d, want 2 — a newer assignment version must not be served the old grant set", inner.grantCalls)
	}
	// A policy change does the same.
	c.SetWatermark(8, 41, time.Now())
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 3 {
		t.Fatalf("calls = %d, want 3 after a policy version change", inner.grantCalls)
	}
}

// A watermark that can no longer be read must not leave stale entries in use.
func TestCache_StaleWatermarkBypassesTheCache(t *testing.T) {
	ctx := context.Background()
	inner := &countingStore{actions: []string{"PAYMENT_APPROVE"}}
	c := newCache(inner, time.Minute)
	c.SetWatermark(1, 1, time.Now().Add(-time.Minute))

	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 2 {
		t.Fatalf("calls = %d, want 2 — a stale watermark must bypass the cache", inner.grantCalls)
	}
}

func TestCache_RunWatermarkReadsAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := &countingStore{actions: []string{"A"}}
	c := newCache(inner, time.Minute)
	reads := make(chan struct{}, 10)
	var fail atomic.Bool
	fail.Store(true)
	go c.RunWatermark(ctx, func(context.Context) (int64, int64, error) {
		defer func() { reads <- struct{}{} }()
		if fail.Load() {
			return 0, 0, errors.New("db down")
		}
		return 3, 4, nil
	}, 20*time.Millisecond)
	<-reads
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 2 {
		t.Fatalf("calls = %d, want 2 — no watermark yet, so nothing is cached", inner.grantCalls)
	}
	fail.Store(false)
	<-reads
	<-reads
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	_, _, _ = c.FindGrantedActions(ctx, "p-1", "e-1", tenantA)
	if inner.grantCalls != 3 {
		t.Fatalf("calls = %d, want 3 — once the watermark reads, the cache serves again", inner.grantCalls)
	}
}
