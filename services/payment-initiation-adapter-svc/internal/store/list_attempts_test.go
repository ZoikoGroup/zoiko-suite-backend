//go:build integration

// Real-Postgres tests for SPEC_DEVIATIONS A1 (ListPaymentAttempts).
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
)

func prepareForList(t *testing.T, ctx context.Context, tenantID, legalEntityID, idempotencyKey string) *domain.PaymentInitiationAttempt {
	t.Helper()
	a, err := testStore.PrepareAttempt(ctx, tenantID, domain.PrepareAttemptRequest{
		LegalEntityID: legalEntityID, PayerAccountRef: "acct-src", PayeeRef: "acct-dst",
		Amount: "100.00", Currency: "USD", ExecutionDate: time.Now(), PayerAccountVerified: true,
		IdempotencyKey: idempotencyKey,
	}, "maker-1", idempotencyKey)
	if err != nil {
		t.Fatalf("PrepareAttempt(%s): %v", idempotencyKey, err)
	}
	return a
}

// TestPgStore_ListPaymentAttempts_ScopesByTenantAndLegalEntity proves the
// list never crosses a tenant or legal-entity boundary, even for an
// attempt that would otherwise match on status alone.
func TestPgStore_ListPaymentAttempts_ScopesByTenantAndLegalEntity(t *testing.T) {
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	entity1, entity2 := uuid.New().String(), uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	inScope := prepareForList(t, ctxA, tenantA, entity1, "idem-list-scope-1")
	prepareForList(t, ctxA, tenantA, entity2, "idem-list-scope-2")  // same tenant, other entity
	prepareForList(t, ctxB, tenantB, entity1, "idem-list-scope-3") // other tenant, same entity id value

	items, err := testStore.ListPaymentAttempts(ctxA, domain.ListPaymentAttemptsFilter{LegalEntityID: entity1})
	if err != nil {
		t.Fatalf("ListPaymentAttempts: %v", err)
	}
	if len(items) != 1 || items[0].AttemptID != inScope.AttemptID {
		t.Fatalf("expected exactly the one attempt in (tenantA, entity1), got %+v", items)
	}
}

// TestPgStore_ListPaymentAttempts_FiltersByStatus proves the optional
// status filter works, and that omitting it returns every status.
func TestPgStore_ListPaymentAttempts_FiltersByStatus(t *testing.T) {
	tenantID, entity := uuid.New().String(), uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	prepared := prepareForList(t, ctx, tenantID, entity, "idem-list-status-1")
	submitted := prepareForList(t, ctx, tenantID, entity, "idem-list-status-2")
	if _, err := testStore.MarkSubmitted(ctx, submitted.AttemptID, "prov-req-1", "prov-ref-1", "maker-1", "corr-1"); err != nil {
		t.Fatalf("MarkSubmitted: %v", err)
	}

	all, err := testStore.ListPaymentAttempts(ctx, domain.ListPaymentAttemptsFilter{LegalEntityID: entity})
	if err != nil {
		t.Fatalf("ListPaymentAttempts (all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected both attempts with no status filter, got %d", len(all))
	}

	onlyPrepared, err := testStore.ListPaymentAttempts(ctx, domain.ListPaymentAttemptsFilter{LegalEntityID: entity, Status: "PREPARED"})
	if err != nil {
		t.Fatalf("ListPaymentAttempts (PREPARED): %v", err)
	}
	if len(onlyPrepared) != 1 || onlyPrepared[0].AttemptID != prepared.AttemptID {
		t.Fatalf("expected exactly the PREPARED attempt, got %+v", onlyPrepared)
	}
}

// TestPgStore_ListPaymentAttempts_KeysetPaginationIsStableUnderConcurrentInsert
// is the real proof the cursor is a (created_at, attempt_id) row-value
// comparison, not plain offset pagination: five attempts are created in a
// known order (a small real sleep between each, since created_at is
// protected by migration 000002's own immutability trigger — it cannot be
// backdated, even by a raw UPDATE, so distinguishing insertion order needs
// an actual time gap), paged two at a time, and a new attempt inserted
// between pages must not shift page 2's contents or duplicate/skip a row —
// the actual failure mode offset pagination has and keyset pagination does
// not.
func TestPgStore_ListPaymentAttempts_KeysetPaginationIsStableUnderConcurrentInsert(t *testing.T) {
	tenantID, entity := uuid.New().String(), uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	var ids []string
	for i := 0; i < 5; i++ {
		a := prepareForList(t, ctx, tenantID, entity, "idem-list-page-"+uuid.New().String())
		ids = append(ids, a.AttemptID)
		time.Sleep(5 * time.Millisecond)
	}
	// Oldest first in `ids`; descending list order is therefore the reverse.
	wantOrder := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}

	page1, err := testStore.ListPaymentAttempts(ctx, domain.ListPaymentAttemptsFilter{LegalEntityID: entity, Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 || page1[0].AttemptID != wantOrder[0] || page1[1].AttemptID != wantOrder[1] {
		t.Fatalf("page1: expected %v, got %+v", wantOrder[:2], page1)
	}

	// Insert a brand-new, NEWEST attempt between page1 and page2 — under
	// offset pagination this would shift every later row back by one and
	// either duplicate page1's last row or skip one; under keyset
	// pagination (cursor = page1's last row) it must not appear on page2
	// at all, and no existing row is skipped or repeated.
	prepareForList(t, ctx, tenantID, entity, "idem-list-page-inserted-between")

	page2, err := testStore.ListPaymentAttempts(ctx, domain.ListPaymentAttemptsFilter{
		LegalEntityID: entity, Limit: 2, AfterID: page1[len(page1)-1].AttemptID})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 2 || page2[0].AttemptID != wantOrder[2] || page2[1].AttemptID != wantOrder[3] {
		t.Fatalf("page2: expected %v (unaffected by the concurrent insert), got %+v", wantOrder[2:4], page2)
	}

	page3, err := testStore.ListPaymentAttempts(ctx, domain.ListPaymentAttemptsFilter{
		LegalEntityID: entity, Limit: 2, AfterID: page2[len(page2)-1].AttemptID})
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3) != 1 || page3[0].AttemptID != wantOrder[4] {
		t.Fatalf("page3: expected the one remaining original attempt %v, got %+v", wantOrder[4:], page3)
	}
}

// TestPgStore_FindAttempt_FindByIdempotencyKey_ListEvents_ScopeByTenant is
// the regression proof for SPEC_DEVIATIONS A11: these three reads had no
// explicit tenant_id predicate and relied on RLS alone, which a
// table-owning (superuser) connection — exactly how this test's own
// embedded Postgres instance, and this project's real dev/test
// environments, connect — bypasses entirely regardless of FORCE ROW LEVEL
// SECURITY. Discovered via a real cross-tenant leak surfaced while testing
// ListPaymentAttempts, not assumed.
func TestPgStore_FindAttempt_FindByIdempotencyKey_ListEvents_ScopeByTenant(t *testing.T) {
	tenantA, tenantB := uuid.New().String(), uuid.New().String()
	ctxA := middleware.WithTenant(context.Background(), tenantA)
	ctxB := middleware.WithTenant(context.Background(), tenantB)

	a := prepareForList(t, ctxB, tenantB, uuid.New().String(), "idem-scope-isolation-1")

	if _, err := testStore.FindAttempt(ctxA, a.AttemptID); err != domain.ErrAttemptNotFound {
		t.Fatalf("FindAttempt: expected ErrAttemptNotFound for another tenant's attempt, got %v", err)
	}
	if _, err := testStore.FindByIdempotencyKey(ctxA, "idem-scope-isolation-1"); err != domain.ErrAttemptNotFound {
		t.Fatalf("FindByIdempotencyKey: expected ErrAttemptNotFound for another tenant's key, got %v", err)
	}
	events, err := testStore.ListEvents(ctxA, a.AttemptID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ListEvents: expected no events visible for another tenant's attempt, got %d", len(events))
	}

	// Same tenant: all three must still work.
	if found, err := testStore.FindAttempt(ctxB, a.AttemptID); err != nil || found.AttemptID != a.AttemptID {
		t.Fatalf("FindAttempt: expected to find the attempt in its own tenant, got %+v, %v", found, err)
	}
	if found, err := testStore.FindByIdempotencyKey(ctxB, "idem-scope-isolation-1"); err != nil || found.AttemptID != a.AttemptID {
		t.Fatalf("FindByIdempotencyKey: expected to find the attempt in its own tenant, got %+v, %v", found, err)
	}
	if events, err := testStore.ListEvents(ctxB, a.AttemptID); err != nil || len(events) == 0 {
		t.Fatalf("ListEvents: expected at least the PREPARED event in its own tenant, got %+v, %v", events, err)
	}
}

// TestPgStore_ListPaymentAttempts_EmptyLegalEntityReturnsNothing guards
// against a filter mistake silently turning into "every entity."
func TestPgStore_ListPaymentAttempts_EmptyLegalEntityReturnsNothing(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)
	prepareForList(t, ctx, tenantID, uuid.New().String(), "idem-list-empty-entity")

	items, err := testStore.ListPaymentAttempts(ctx, domain.ListPaymentAttemptsFilter{LegalEntityID: ""})
	if err != nil {
		t.Fatalf("ListPaymentAttempts: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected no rows for an empty legal_entity_id, got %d", len(items))
	}
}
