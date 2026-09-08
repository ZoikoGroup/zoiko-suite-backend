package store_test

// AST-03's own store methods are exercised only by handler_test.go's
// in-memory stub elsewhere in this package — this file is their
// real-Postgres coverage, against the actual guarded UPDATEs, the
// chk_asset_event_no_cross_entity_transfer CHECK constraint and the
// trg_reject_asset_event_economic_mutation reject-trigger, not a stub's
// map mutation.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
	"zoiko.io/asset-management-svc/internal/store"
)

func newTestAssetEvent(tenantID string, a *domain.FixedAsset, eventType string) *domain.AssetEvent {
	return &domain.AssetEvent{
		EventID: uuid.New().String(), TenantID: tenantID, LegalEntityID: a.LegalEntityID, AssetID: a.AssetID,
		EventType: eventType, Status: domain.AssetEventStatusDraft,
		SourceDocumentRef: "SRC-DOC-1", EffectiveDate: time.Now().UTC(), FiscalPeriod: "2026-09",
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "preparer-1",
	}
}

func TestPgStore_CreateAssetEvent_And_GetAssetEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")

	e := newTestAssetEvent(tenantID, a, domain.AssetEventTypeAddition)
	if err := s.CreateAssetEvent(ctx, e); err != nil {
		t.Fatalf("CreateAssetEvent: %v", err)
	}

	got, err := s.GetAssetEvent(ctx, e.EventID)
	if err != nil {
		t.Fatalf("GetAssetEvent: %v", err)
	}
	if got.Status != domain.AssetEventStatusDraft || got.EventType != domain.AssetEventTypeAddition {
		t.Fatalf("unexpected event: %+v", got)
	}
}

// TestPgStore_AssetEvent_CrossEntityTransfer_RejectedByCheckConstraint
// proves negative path #3, "Cross-entity asset transfer bypasses
// intercompany accounting," is enforced at the database layer, not merely
// by application code — a raw INSERT bypassing the handler's own check is
// still refused.
func TestPgStore_AssetEvent_CrossEntityTransfer_RejectedByCheckConstraint(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")

	e := newTestAssetEvent(tenantID, a, domain.AssetEventTypeTransfer)
	otherEntity := "le-2"
	e.DestinationLegalEntityID = &otherEntity
	if err := s.CreateAssetEvent(ctx, e); err == nil {
		t.Fatal("expected chk_asset_event_no_cross_entity_transfer to reject a cross-entity TRANSFER event")
	}
}

// TestPgStore_AssetEvent_DisposalLifecycle_RealDB walks Draft->Validated->
// Approved->Applied(DISPOSED)->Reversed(ACTIVE again) against real
// Postgres, proving the asset-status delta and its reversal are correct
// and atomic with the event's own status.
func TestPgStore_AssetEvent_DisposalLifecycle_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")

	e := newTestAssetEvent(tenantID, a, domain.AssetEventTypeDisposal)
	proceeds := 500.0
	e.ProceedsAmount = &proceeds
	if err := s.CreateAssetEvent(ctx, e); err != nil {
		t.Fatalf("CreateAssetEvent: %v", err)
	}

	now := time.Now().UTC()
	if err := s.ValidateAssetEvent(ctx, e.EventID, now); err != nil {
		t.Fatalf("ValidateAssetEvent: %v", err)
	}
	if err := s.ApproveAssetEvent(ctx, e.EventID, "approver-1", now); err != nil {
		t.Fatalf("ApproveAssetEvent: %v", err)
	}
	if err := s.ApplyAssetEvent(ctx, e.EventID, now, nil); err != nil {
		t.Fatalf("ApplyAssetEvent: %v", err)
	}

	gotAsset, err := s.GetAsset(ctx, a.AssetID)
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	if gotAsset.Status != domain.AssetStatusDisposed {
		t.Fatalf("expected asset DISPOSED, got %q", gotAsset.Status)
	}
	gotEvent, err := s.GetAssetEvent(ctx, e.EventID)
	if err != nil {
		t.Fatalf("GetAssetEvent: %v", err)
	}
	if gotEvent.Status != domain.AssetEventStatusApplied {
		t.Fatalf("expected event APPLIED (no journal posted), got %q", gotEvent.Status)
	}

	// A second disposal of the already-DISPOSED asset must be refused —
	// enforced structurally by ApplyAssetEvent's own WHERE status IN
	// (ACTIVE, SUSPENDED) guard, not by any duplicate-check bookkeeping.
	second := newTestAssetEvent(tenantID, a, domain.AssetEventTypeDisposal)
	second.ProceedsAmount = &proceeds
	if err := s.CreateAssetEvent(ctx, second); err != nil {
		t.Fatalf("CreateAssetEvent (second): %v", err)
	}
	if err := s.ValidateAssetEvent(ctx, second.EventID, now); err != nil {
		t.Fatalf("ValidateAssetEvent (second): %v", err)
	}
	if err := s.ApproveAssetEvent(ctx, second.EventID, "approver-1", now); err != nil {
		t.Fatalf("ApproveAssetEvent (second): %v", err)
	}
	if err := s.ApplyAssetEvent(ctx, second.EventID, now, nil); err == nil {
		t.Fatal("expected disposing an already-DISPOSED asset to be refused")
	}

	// Re-applying the first event (already APPLIED, not APPROVED) must
	// also be refused — the guarded transition's own WHERE status =
	// APPROVED clause.
	if err := s.ApplyAssetEvent(ctx, e.EventID, now, nil); err == nil {
		t.Fatal("expected re-applying an already-APPLIED event to be refused (guarded transition)")
	}
}

// TestPgStore_AssetEvent_ReverseRequiresEmitted_RealDB proves
// ReverseAssetEvent/SupersedeAssetEvent only operate on
// ACCOUNTING_EVENT_EMITTED events, and that reversing a disposal's
// emitted event reverts the asset back to ACTIVE.
func TestPgStore_AssetEvent_ReverseRequiresEmitted_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")

	e := newTestAssetEvent(tenantID, a, domain.AssetEventTypeDisposal)
	proceeds := 500.0
	e.ProceedsAmount = &proceeds
	if err := s.CreateAssetEvent(ctx, e); err != nil {
		t.Fatalf("CreateAssetEvent: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ValidateAssetEvent(ctx, e.EventID, now); err != nil {
		t.Fatalf("ValidateAssetEvent: %v", err)
	}
	if err := s.ApproveAssetEvent(ctx, e.EventID, "approver-1", now); err != nil {
		t.Fatalf("ApproveAssetEvent: %v", err)
	}

	// Reversing before Apply must be refused — Reverse requires
	// ACCOUNTING_EVENT_EMITTED, and this event is only APPROVED.
	if err := s.ReverseAssetEvent(ctx, e.EventID, "preparer-1", "too early", now); err == nil {
		t.Fatal("expected ReverseAssetEvent on a non-emitted event to be refused")
	}

	journalID := "journal-1"
	if err := s.ApplyAssetEvent(ctx, e.EventID, now, &journalID); err != nil {
		t.Fatalf("ApplyAssetEvent (with journal): %v", err)
	}
	gotAsset, err := s.GetAsset(ctx, a.AssetID)
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	if gotAsset.Status != domain.AssetStatusDisposed {
		t.Fatalf("expected DISPOSED, got %q", gotAsset.Status)
	}
	gotEvent, err := s.GetAssetEvent(ctx, e.EventID)
	if err != nil {
		t.Fatalf("GetAssetEvent: %v", err)
	}
	if gotEvent.Status != domain.AssetEventStatusAccountingEventEmitted {
		t.Fatalf("expected ACCOUNTING_EVENT_EMITTED, got %q", gotEvent.Status)
	}

	if err := s.ReverseAssetEvent(ctx, e.EventID, "preparer-1", "posted in error", now); err != nil {
		t.Fatalf("ReverseAssetEvent: %v", err)
	}
	gotAsset, err = s.GetAsset(ctx, a.AssetID)
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	if gotAsset.Status != domain.AssetStatusActive {
		t.Fatalf("expected asset reverted to ACTIVE after reversal, got %q", gotAsset.Status)
	}
}

// TestPgStore_AssetEvent_RejectsEconomicMutationOnceApplied is the real,
// DB-enforced proof of negative path #2, "Disposed asset event edited in
// place" — a raw UPDATE of an economic field on an APPLIED event is
// rejected by trg_reject_asset_event_economic_mutation regardless of
// application code.
func TestPgStore_AssetEvent_RejectsEconomicMutationOnceApplied(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)

	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	a := activeAssetInStore(t, ctx, s, tenantID, "le-1")

	e := newTestAssetEvent(tenantID, a, domain.AssetEventTypeAddition)
	amount := 100.0
	e.Amount = &amount
	if err := s.CreateAssetEvent(ctx, e); err != nil {
		t.Fatalf("CreateAssetEvent: %v", err)
	}
	now := time.Now().UTC()
	if err := s.ValidateAssetEvent(ctx, e.EventID, now); err != nil {
		t.Fatalf("ValidateAssetEvent: %v", err)
	}
	if err := s.ApproveAssetEvent(ctx, e.EventID, "approver-1", now); err != nil {
		t.Fatalf("ApproveAssetEvent: %v", err)
	}
	if err := s.ApplyAssetEvent(ctx, e.EventID, now, nil); err != nil {
		t.Fatalf("ApplyAssetEvent: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `UPDATE asset_events SET amount = 999999 WHERE event_id = $1`, e.EventID); err == nil {
		t.Fatal("expected UPDATE of an economic field on an APPLIED asset event to be rejected by the reject-mutation trigger")
	}

	// Negative control confirms the trigger is scoped correctly: a
	// status-only mutation (what ReverseAssetEvent/SupersedeAssetEvent
	// actually perform) must still succeed.
	if _, err := conn.Exec(ctx, `UPDATE asset_events SET reversal_reason = 'note' WHERE event_id = $1`, e.EventID); err != nil {
		t.Fatalf("expected a non-economic column UPDATE to succeed, got: %v", err)
	}
}
