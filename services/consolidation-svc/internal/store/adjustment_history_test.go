package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
)

func TestPgStore_GetAdjustmentHistory_FullLifecycle(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")
	journalID := uuid.NewString()

	if err := s.CreateAdjustment(ctx, adj, "corr-create"); err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}
	if err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-1", "corr-approve"); err != nil {
		t.Fatalf("ApproveAdjustment: %v", err)
	}
	if err := s.MarkAdjustmentPosted(ctx, adj.ConsolidationAdjustmentID, "poster-1", journalID, "corr-post"); err != nil {
		t.Fatalf("MarkAdjustmentPosted: %v", err)
	}
	if err := s.ReverseAdjustment(ctx, adj.ConsolidationAdjustmentID, "reverser-1", "correction", nil, "corr-reverse"); err != nil {
		t.Fatalf("ReverseAdjustment: %v", err)
	}

	h, err := s.GetAdjustmentHistory(ctx, adj.ConsolidationAdjustmentID)
	if err != nil {
		t.Fatalf("GetAdjustmentHistory: %v", err)
	}
	if h.GroupLegalEntityID != "group-1" || h.FiscalPeriod != "2026-07" ||
		h.AdjustmentType != domain.AdjustmentTypeManual || h.Status != domain.AdjustmentStatusReversed {
		t.Fatalf("history metadata lost: %+v", h)
	}
	if len(h.Transitions) != 4 {
		t.Fatalf("expected four transitions, got %+v", h.Transitions)
	}

	wantFrom := []*string{nil, ptr(domain.AdjustmentStatusPendingApproval),
		ptr(domain.AdjustmentStatusApproved), ptr(domain.AdjustmentStatusPosted)}
	wantTo := []string{domain.AdjustmentStatusPendingApproval, domain.AdjustmentStatusApproved,
		domain.AdjustmentStatusPosted, domain.AdjustmentStatusReversed}
	wantActor := []string{"preparer-1", "approver-1", "poster-1", "reverser-1"}
	wantCorrelation := []string{"corr-create", "corr-approve", "corr-post", "corr-reverse"}

	for i, tr := range h.Transitions {
		if tr.Sequence != int64(i+1) {
			t.Errorf("transition %d sequence = %d", i, tr.Sequence)
		}
		if wantFrom[i] == nil {
			if tr.FromStatus != nil {
				t.Errorf("transition %d from_status = %v, want nil", i, *tr.FromStatus)
			}
		} else if tr.FromStatus == nil || *tr.FromStatus != *wantFrom[i] {
			t.Errorf("transition %d from_status = %v, want %s", i, tr.FromStatus, *wantFrom[i])
		}
		if tr.ToStatus != wantTo[i] {
			t.Errorf("transition %d to_status = %s, want %s", i, tr.ToStatus, wantTo[i])
		}
		if tr.ActorPrincipalID != wantActor[i] {
			t.Errorf("transition %d actor = %s, want %s", i, tr.ActorPrincipalID, wantActor[i])
		}
		if tr.CorrelationID != wantCorrelation[i] {
			t.Errorf("transition %d correlation = %s, want %s", i, tr.CorrelationID, wantCorrelation[i])
		}
		if tr.OccurredAt.IsZero() || tr.OccurredAt.After(time.Now().Add(time.Minute)) {
			t.Errorf("transition %d occurred_at = %v", i, tr.OccurredAt)
		}
		if i > 0 && tr.OccurredAt.Before(h.Transitions[i-1].OccurredAt) {
			t.Errorf("transition %d occurred before its predecessor", i)
		}
	}

	if got := h.Transitions[2].Details["consolidation_book_journal_id"]; got != journalID {
		t.Errorf("posted transition journal = %v, want %s", got, journalID)
	}
	if h.Transitions[3].Reason == nil || *h.Transitions[3].Reason != "correction" {
		t.Errorf("reversed transition reason = %v", h.Transitions[3].Reason)
	}
}

func TestPgStore_GetAdjustmentHistory_RejectedTransitionWritesNoTransition(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")

	if err := s.CreateAdjustment(ctx, adj, "corr-create"); err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}
	if err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-1", "corr-approve"); err != nil {
		t.Fatalf("ApproveAdjustment: %v", err)
	}
	if err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-2", "corr-again"); !errors.Is(err, domain.ErrInvalidAdjustmentTransition) {
		t.Fatalf("second approve: expected ErrInvalidAdjustmentTransition, got %v", err)
	}

	h, err := s.GetAdjustmentHistory(ctx, adj.ConsolidationAdjustmentID)
	if err != nil {
		t.Fatalf("GetAdjustmentHistory: %v", err)
	}
	if len(h.Transitions) != 2 {
		t.Fatalf("rejected transition must write no row: %+v", h.Transitions)
	}
}

func TestPgStore_GetAdjustmentHistory_Unknown(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)
	ctx := svcmiddleware.WithTenant(context.Background(), uuid.NewString())

	_, err := s.GetAdjustmentHistory(ctx, uuid.NewString())
	if !errors.Is(err, domain.ErrAdjustmentNotFound) {
		t.Fatalf("expected ErrAdjustmentNotFound, got %v", err)
	}
}

func TestPgStore_GetAdjustmentHistory_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantA)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	adj := newTestAdjustment(tenantA, "group-1", "preparer-1")

	if err := s.CreateAdjustment(ctxA, adj, "corr-create"); err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}
	if _, err := s.GetAdjustmentHistory(ctxB, adj.ConsolidationAdjustmentID); !errors.Is(err, domain.ErrAdjustmentNotFound) {
		t.Fatalf("another tenant must not read the history, got %v", err)
	}
}

// A row created before migration 000006 has no transition rows. The read
// must rebuild the chain from the lifecycle columns rather than answer with
// an empty list; deleting the rows here simulates exactly that old row.
func TestPgStore_GetAdjustmentHistory_SynthesizesLegacyChain(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")
	journalID := uuid.NewString()

	if err := s.CreateAdjustment(ctx, adj, "corr-create"); err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}
	if err := s.ApproveAdjustment(ctx, adj.ConsolidationAdjustmentID, "approver-1", "corr-approve"); err != nil {
		t.Fatalf("ApproveAdjustment: %v", err)
	}
	if err := s.MarkAdjustmentPosted(ctx, adj.ConsolidationAdjustmentID, "poster-1", journalID, "corr-post"); err != nil {
		t.Fatalf("MarkAdjustmentPosted: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM consolidation_adjustment_transitions WHERE tenant_id = $1 AND consolidation_adjustment_id = $2`,
		tenantID, adj.ConsolidationAdjustmentID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	h, err := s.GetAdjustmentHistory(ctx, adj.ConsolidationAdjustmentID)
	if err != nil {
		t.Fatalf("GetAdjustmentHistory: %v", err)
	}
	if len(h.Transitions) != 3 {
		t.Fatalf("expected the three lifecycle steps to be rebuilt, got %+v", h.Transitions)
	}
	want := []string{domain.AdjustmentStatusPendingApproval, domain.AdjustmentStatusApproved, domain.AdjustmentStatusPosted}
	for i, tr := range h.Transitions {
		if tr.ToStatus != want[i] || tr.Sequence != int64(i+1) {
			t.Errorf("synthesized transition %d = %+v", i, tr)
		}
	}
	if got := h.Transitions[2].Details["consolidation_book_journal_id"]; got != journalID {
		t.Errorf("synthesized journal id = %v, want %s", got, journalID)
	}
}

// History rows backfilled from pre-000006 adjustments have no correlation id
// (the old columns never stored one). Reading them must not fail on NULL.
func TestPgStore_GetAdjustmentHistory_BackfilledNullCorrelationReads(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)

	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	adj := newTestAdjustment(tenantID, "group-1", "preparer-1")
	if err := s.CreateAdjustment(ctx, adj, "corr-create"); err != nil {
		t.Fatalf("CreateAdjustment: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE consolidation_adjustment_transitions
		SET correlation_id = NULL WHERE tenant_id = $1 AND consolidation_adjustment_id = $2`,
		tenantID, adj.ConsolidationAdjustmentID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	h, err := s.GetAdjustmentHistory(ctx, adj.ConsolidationAdjustmentID)
	if err != nil {
		t.Fatalf("GetAdjustmentHistory with a NULL correlation id: %v", err)
	}
	if len(h.Transitions) != 1 || h.Transitions[0].CorrelationID != "" {
		t.Fatalf("unexpected history: %+v", h.Transitions)
	}
}

func ptr[T any](v T) *T { return &v }
