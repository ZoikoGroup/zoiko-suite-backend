package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/filing-preparation-svc/internal/domain"
	"zoiko.io/filing-preparation-svc/internal/middleware"
	"zoiko.io/filing-preparation-svc/internal/store"
)

func newDraft(period string) *domain.FilingDraft {
	return &domain.FilingDraft{LegalEntityID: "le", JurisdictionID: "GB", FilingType: "VAT_RETURN",
		PeriodKey: period, DueDate: "2026-11-07", PayloadData: "{}", CreatedBy: "p1"}
}

// Finalize used to accept a never-validated DRAFT, and Update used to rewrite a
// draft already handed over as READY_FOR_SUBMISSION.
func TestPgStore_Finalize_RequiresPrepared_ThenDraftIsFrozen(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-fp")
	s := store.NewPgStore(pool)
	d := newDraft("2026Q3")
	if err := s.Create(ctx, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	fin := &domain.FinalizeDraftRequest{FinalizedBy: "p2"}
	if _, err := s.Finalize(ctx, d.DraftID, fin); !errors.Is(err, domain.ErrDraftNotPrepared) {
		t.Fatalf("finalize DRAFT: expected ErrDraftNotPrepared, got %v", err)
	}
	if _, err := s.Validate(ctx, d.DraftID, &domain.ValidateDraftRequest{}, true, ""); err != nil {
		t.Fatalf("validate: %v", err)
	}
	got, err := s.Finalize(ctx, d.DraftID, fin)
	if err != nil || got.ValidationStatus != domain.StatusReadyForSubmission {
		t.Fatalf("finalize PREPARED: %v %v", err, got)
	}
	got.PayloadData = `{"changed":true}`
	if err := s.Update(ctx, got); !errors.Is(err, domain.ErrDraftAlreadyFinal) {
		t.Fatalf("update finalized: expected ErrDraftAlreadyFinal, got %v", err)
	}
	if _, err := s.Finalize(ctx, d.DraftID, fin); !errors.Is(err, domain.ErrDraftAlreadyFinal) {
		t.Fatalf("re-finalize: expected ErrDraftAlreadyFinal, got %v", err)
	}
}

func TestPgStore_Create_DuplicatePeriodIsErrDuplicateDraft(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-fp")
	s := store.NewPgStore(pool)
	if err := s.Create(ctx, newDraft("2026Q1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(ctx, newDraft("2026Q1")); !errors.Is(err, domain.ErrDuplicateDraft) {
		t.Fatalf("expected ErrDuplicateDraft, got %v", err)
	}
}
