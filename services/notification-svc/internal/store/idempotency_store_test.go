package store_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/store"
)

// Migration 000012: purpose-scoped dedup on (tenant_id, idempotency_key), and
// the old (tenant_id, correlation_id) dedup kept for sends that name no purpose.

func TestIdempotency_SameCorrelationDifferentKeys_BothCreated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	a := newNotification("tenant-a", "entity-1", "recipient-1", "payroll-run-7")
	a.PurposeContext, a.IdempotencyKey = "payslip", "psk-a"
	b := newNotification("tenant-a", "entity-1", "recipient-1", "payroll-run-7")
	b.PurposeContext, b.IdempotencyKey = "tax_form", "psk-b"

	if created, err := s.CreateNotification(ctx, a); err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if created, err := s.CreateNotification(ctx, b); err != nil || !created {
		t.Fatalf("second (same correlation, other purpose): created=%v err=%v — purpose scoping failed", created, err)
	}
}

func TestIdempotency_SameKeyReplaysTheOriginal(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	orig := newNotification("tenant-a", "entity-1", "recipient-1", "corr-k")
	orig.PurposeContext, orig.IdempotencyKey = "payslip", "psk-same"
	if _, err := s.CreateNotification(ctx, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	dup := newNotification("tenant-a", "entity-1", "recipient-1", "corr-k-retry")
	dup.PurposeContext, dup.IdempotencyKey = "payslip", "psk-same"
	created, err := s.CreateNotification(ctx, dup)
	if err != nil || created {
		t.Fatalf("replay: created=%v err=%v, want false/nil", created, err)
	}
	if dup.NotificationID != orig.NotificationID {
		t.Fatalf("replay returned %s, want the original %s", dup.NotificationID, orig.NotificationID)
	}
}

// Unkeyed sends keep the pre-000012 behaviour: one row per correlation id.
func TestIdempotency_UnkeyedStillDedupesOnCorrelation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	first := newNotification("tenant-a", "entity-1", "recipient-1", "corr-legacy")
	second := newNotification("tenant-a", "entity-1", "recipient-2", "corr-legacy")
	if _, err := s.CreateNotification(ctx, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	created, err := s.CreateNotification(ctx, second)
	if err != nil || created || second.NotificationID != first.NotificationID {
		t.Fatalf("legacy dedup broke: created=%v err=%v id=%s", created, err, second.NotificationID)
	}
}

// The dedup is decided by the database, so concurrent sends of one
// communication produce exactly one row — the property a check-then-insert
// does not have.
func TestIdempotency_ConcurrentSameKey_ExactlyOneCreated(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := tenantCtx("tenant-a")

	const racers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	createdCount := 0
	ids := map[string]bool{}
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := newNotification("tenant-a", "entity-1", "recipient-1", "corr-race-"+uuid.NewString())
			n.PurposeContext, n.IdempotencyKey = "payslip", "psk-race"
			created, err := s.CreateNotification(ctx, n)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("racer: %v", err)
				return
			}
			if created {
				createdCount++
			}
			ids[n.NotificationID] = true
		}()
	}
	wg.Wait()
	if createdCount != 1 || len(ids) != 1 {
		t.Fatalf("created %d rows across %d distinct ids, want exactly 1 of each", createdCount, len(ids))
	}
}
