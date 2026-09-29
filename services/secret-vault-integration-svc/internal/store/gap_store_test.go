package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
	"zoiko.io/secret-vault-integration-svc/internal/store"
)

// The tenant predicate was an unparenthesised OR joined with AND, so every
// GLOBAL exception came back whatever its status or path — and emergency
// retrieval took a revoked exception for another secret as authority.
func TestPgStore_ListSharedSecretExceptions_FiltersApplyToGlobalRows(t *testing.T) {
	ctx := context.Background()
	s := store.New(openTestPool(t), zap.NewNop())

	other, _, err := s.CreateSharedSecretException(ctx, domain.SharedSecretException{
		ExceptionID: uuid.NewString(), SecretPath: "kv/other", Reason: "r", EvidenceReference: "INC-1",
		ApprovedByPrincipalID: "ops", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RevokeSharedSecretException(ctx, other.ExceptionID, "", "ops"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSharedSecretExceptions(ctx, domain.ListSharedSecretExceptionsFilter{SecretPath: "kv/db", Status: "ACTIVE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ACTIVE kv/db must not return a REVOKED kv/other global exception, got %+v", got[0])
	}

	mine, _, err := s.CreateSharedSecretException(ctx, domain.SharedSecretException{
		ExceptionID: uuid.NewString(), SecretPath: "kv/db", Reason: "r", EvidenceReference: "INC-2",
		ApprovedByPrincipalID: "ops", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListSharedSecretExceptions(ctx, domain.ListSharedSecretExceptionsFilter{SecretPath: "kv/db", Status: "ACTIVE"})
	if len(got) != 1 || got[0].ExceptionID != mine.ExceptionID {
		t.Fatalf("expected exactly the ACTIVE kv/db exception, got %d rows", len(got))
	}
}

// Two sweepers listing the same due slot: exactly one claim succeeds.
func TestPgStore_ClaimDueRotation_OneClaimPerDueSlot(t *testing.T) {
	ctx := context.Background()
	s := store.New(openTestPool(t), zap.NewNop())
	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/rot")
	v, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "admin-1",
		RotationIntervalSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	if err := s.UpsertRotationSchedule(ctx, v.SecretPolicyVersionID, 3600, due); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListDueRotations(ctx, time.Now())
	if err != nil || len(list) != 1 {
		t.Fatalf("expected one due rotation, got %d (%v)", len(list), err)
	}
	listed := list[0].NextRotationAt

	a, err := s.ClaimDueRotation(ctx, v.SecretPolicyVersionID, listed, time.Now().Add(10*time.Minute))
	if err != nil || !a {
		t.Fatalf("first claim must succeed: %v %v", a, err)
	}
	b, err := s.ClaimDueRotation(ctx, v.SecretPolicyVersionID, listed, time.Now().Add(10*time.Minute))
	if err != nil || b {
		t.Fatalf("second claim on the same listed slot must fail: %v %v", b, err)
	}
	if again, _ := s.ListDueRotations(ctx, time.Now()); len(again) != 0 {
		t.Fatal("a claimed rotation must not be listed as due until its claim lapses")
	}
}
