package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/filing-tracker-svc/internal/domain"
	"zoiko.io/filing-tracker-svc/internal/middleware"
	"zoiko.io/filing-tracker-svc/internal/store"
)

func newReq(period, due string) *domain.FilingRequirement {
	return &domain.FilingRequirement{LegalEntityID: "le", JurisdictionID: "GB", FilingAuthority: "HMRC",
		FilingType: "VAT_RETURN", PeriodKey: period, DueDate: due, CreatedBy: "p1"}
}

// Confirm used to accept a SCHEDULED filing that was never submitted.
func TestPgStore_Confirm_RequiresSubmitted(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-ft")
	s := store.NewPgStore(pool)
	f := newReq("2026Q3", "2026-12-31")
	if err := s.Create(ctx, f); err != nil {
		t.Fatalf("create: %v", err)
	}
	conf := &domain.ConfirmFilingRequest{ConfirmationReference: "ACK"}
	if _, err := s.Confirm(ctx, f.FilingID, conf); !errors.Is(err, domain.ErrNotSubmitted) {
		t.Fatalf("confirm SCHEDULED: expected ErrNotSubmitted, got %v", err)
	}
	if _, err := s.Submit(ctx, f.FilingID, &domain.SubmitFilingRequest{SubmissionReference: "S", SubmittedBy: "p2"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got, err := s.Confirm(ctx, f.FilingID, conf); err != nil || got.Status != domain.StatusConfirmed {
		t.Fatalf("confirm SUBMITTED: %v %v", err, got)
	}
	if _, err := s.Confirm(ctx, f.FilingID, conf); !errors.Is(err, domain.ErrAlreadyConfirmed) {
		t.Fatalf("re-confirm: expected ErrAlreadyConfirmed, got %v", err)
	}
}

func TestPgStore_Create_DuplicatePeriodIsErrDuplicateRequirement(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-ft")
	s := store.NewPgStore(pool)
	if err := s.Create(ctx, newReq("2026Q1", "2026-04-30")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Create(ctx, newReq("2026Q1", "2026-04-30")); !errors.Is(err, domain.ErrDuplicateRequirement) {
		t.Fatalf("expected ErrDuplicateRequirement, got %v", err)
	}
}

func TestPgStore_MarkOverdue_TwiceIsErrAlreadyOverdue(t *testing.T) {
	pool := freshTestDB(t)
	ctx := middleware.WithTenant(context.Background(), "tenant-ft")
	s := store.NewPgStore(pool)
	f := newReq("2026Q2", "2026-01-31")
	if err := s.Create(ctx, f); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := s.MarkOverdue(ctx, f.FilingID, "2026-10-23"); err != nil || got.Status != domain.StatusOverdue {
		t.Fatalf("mark overdue: %v %v", err, got)
	}
	if _, err := s.MarkOverdue(ctx, f.FilingID, "2026-10-23"); !errors.Is(err, domain.ErrAlreadyOverdue) {
		t.Fatalf("expected ErrAlreadyOverdue, got %v", err)
	}
}
