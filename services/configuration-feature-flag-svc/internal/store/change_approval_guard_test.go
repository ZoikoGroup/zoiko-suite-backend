package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// S3-1 / R-4 and S3-2 / R-5: segregated approval, and no decision on a
// change that is no longer awaiting one.

func proposeChange(t *testing.T, s *store.PgStore, class, proposer string) *domain.ConfigChange {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.UpsertConfigEntry(ctx, domain.UpsertConfigEntryParams{
		Key: "payroll.batch_size", Value: []byte(`100`), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("baseline write: %v", err)
	}
	change, err := s.CreateChange(ctx, domain.CreateChangeParams{
		ChangeClass: class,
		Environment: "staging",
		Parts: []domain.ChangePart{{
			Kind: domain.PartKindConfig, Key: "payroll.batch_size",
			Scope: domain.ChangePartScope{Environment: "staging"}, NewValue: []byte(`300`),
		}},
		ApprovalRequired: true,
		CallerTenantID:   testCallerTenant,
		ActorPrincipalID: proposer,
	})
	if err != nil {
		t.Fatalf("create change: %v", err)
	}
	return change
}

func TestAA_Change_ProposerCannotApproveOwnC2C3(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	for _, class := range []string{domain.ChangeClassC2, domain.ChangeClassC3} {
		change := proposeChange(t, s, class, "maker-1")
		_, err := s.ApproveChange(ctx, change.ChangeID, domain.ChangeApproval{Approved: true, ByPrincipalID: "maker-1", ApprovedAt: time.Now()}, testCallerTenant)
		if !errors.Is(err, domain.ErrChangeSelfApproval) {
			t.Fatalf("%s: maker approving own change = %v, want change_self_approval", class, err)
		}
		// Rejecting one's own change only withholds, and is allowed.
		if _, err := s.ApproveChange(ctx, change.ChangeID, domain.ChangeApproval{Approved: false, ByPrincipalID: "maker-1", ApprovedAt: time.Now()}, testCallerTenant); err != nil {
			t.Fatalf("%s: maker withdrawing: %v", class, err)
		}
		appr, err := s.ApproveChange(ctx, change.ChangeID, domain.ChangeApproval{Approved: true, ByPrincipalID: "checker-2", ApprovedAt: time.Now()}, testCallerTenant)
		if err != nil || appr.Status != domain.ChangeStatusApproved {
			t.Fatalf("%s: independent approval = %+v, %v", class, appr, err)
		}
	}
}

func TestAA_Change_FinishedChangeCannotBeReDecided(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")

	change := proposeChange(t, s, domain.ChangeClassC2, "maker-1")
	if _, err := s.ApproveChange(ctx, change.ChangeID, domain.ChangeApproval{Approved: true, ByPrincipalID: "checker-2"}, testCallerTenant); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if act, err := s.ActivateChange(ctx, change.ChangeID, testCallerTenant, "operator-1"); err != nil || act.Status != domain.ChangeStatusVerified {
		t.Fatalf("activate: %+v %v", act, err)
	}
	for _, approved := range []bool{false, true} {
		_, err := s.ApproveChange(ctx, change.ChangeID, domain.ChangeApproval{Approved: approved, ByPrincipalID: "checker-3"}, testCallerTenant)
		if !errors.Is(err, domain.ErrChangeNotApprovable) {
			t.Fatalf("deciding a VERIFIED change (approved=%v) = %v, want change_not_approvable", approved, err)
		}
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM config_changes WHERE change_id = $1`, change.ChangeID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != domain.ChangeStatusVerified {
		t.Fatalf("status = %s; a refused decision must leave it VERIFIED", status)
	}
	if _, err := s.ActivateChange(ctx, change.ChangeID, testCallerTenant, "operator-1"); err == nil {
		t.Fatal("a VERIFIED change must not be activatable again")
	}
}
