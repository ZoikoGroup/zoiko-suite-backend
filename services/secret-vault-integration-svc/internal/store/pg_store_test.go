package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
	"zoiko.io/secret-vault-integration-svc/internal/store"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS secret_access_audit_log, secret_leases, secret_policy_versions, secret_policies CASCADE;`)

	for _, migFile := range []string{"000001_initial_schema.up.sql", "000002_add_data_classification.up.sql"} {
		sql, err := os.ReadFile("../../deployments/migrations/" + migFile)
		if err != nil {
			t.Fatalf("failed to read migration file %s: %v", migFile, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to execute migration %s: %v", migFile, err)
		}
	}
	return pool
}

func strPtr(s string) *string { return &s }

func createTestPolicy(t *testing.T, ctx context.Context, s *store.PgStore, secretClass, secretPath string) *domain.SecretPolicy {
	t.Helper()
	p, _, err := s.CreateSecretPolicy(ctx, domain.CreateSecretPolicyParams{
		SecretClass: secretClass, SecretPath: secretPath, CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}
	return p
}

// ── secret_policies ──────────────────────────────────────────────────────────

func TestPgStore_CreateSecretPolicy_IdempotencyAndConflict(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	params := domain.CreateSecretPolicyParams{SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/payroll/db", CreatedByPrincipalID: "admin-1"}

	p1, created, err := s.CreateSecretPolicy(ctx, params)
	if err != nil || !created {
		t.Fatalf("expected created=true, err=nil; got created=%v err=%v", created, err)
	}

	p2, created, err := s.CreateSecretPolicy(ctx, params)
	if err != nil || created {
		t.Fatalf("expected created=false on identical retry, got created=%v err=%v", created, err)
	}
	if p2.SecretPolicyID != p1.SecretPolicyID {
		t.Errorf("expected same secret_policy_id on retry")
	}

	conflictParams := params
	conflictParams.SecretClass = "API_SIGNING_SECRET"
	if _, _, err := s.CreateSecretPolicy(ctx, conflictParams); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict on differing secret_class for same secret_path, got %v", err)
	}
}

// ── secret_policy_versions ───────────────────────────────────────────────────

func TestPgStore_CreateSecretPolicyVersion_PolicyNotFound(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	_, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: "00000000-0000-0000-0000-000000000099", AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC(), CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrSecretPolicyNotFound) {
		t.Fatalf("expected ErrSecretPolicyNotFound, got %v", err)
	}
}

func TestPgStore_ActivateVersion_SupersedesPreviousActiveAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")

	v1, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create v1: %v", err)
	}

	activated1, superseded1, transitioned1, err := s.ActivateVersion(ctx, v1.SecretPolicyVersionID, "admin-1")
	if err != nil || !transitioned1 || len(superseded1) != 0 {
		t.Fatalf("unexpected first activation result: activated=%+v superseded=%d transitioned=%v err=%v", activated1, len(superseded1), transitioned1, err)
	}

	retry, _, retryTransitioned, err := s.ActivateVersion(ctx, v1.SecretPolicyVersionID, "admin-1")
	if err != nil || retryTransitioned || retry.VersionStatus != "ACTIVE" {
		t.Fatalf("expected idempotent no-op on retry, got transitioned=%v err=%v", retryTransitioned, err)
	}

	v2, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-a","svc-b"]`),
		MaxLeaseDurationSeconds: 600, EffectiveFrom: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create v2: %v", err)
	}

	_, superseded2, transitioned2, err := s.ActivateVersion(ctx, v2.SecretPolicyVersionID, "admin-1")
	if err != nil || !transitioned2 || len(superseded2) != 1 || superseded2[0].SecretPolicyVersionID != v1.SecretPolicyVersionID {
		t.Fatalf("expected v1 superseded when activating v2, got superseded=%+v transitioned=%v err=%v", superseded2, transitioned2, err)
	}

	v1After, err := s.FindSecretPolicyVersionByID(ctx, v1.SecretPolicyVersionID)
	if err != nil || v1After.VersionStatus != "SUPERSEDED" {
		t.Fatalf("expected v1 SUPERSEDED (not deleted), got %+v err=%v", v1After, err)
	}

	if _, _, _, err := s.ActivateVersion(ctx, v1.SecretPolicyVersionID, "admin-1"); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition activating a SUPERSEDED version, got %v", err)
	}
}

func TestPgStore_FindApplicableVersionByPath_ScopePrecedenceAndIsolation(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")
	tenantA := strPtr("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	tenantB := strPtr("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	global, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-global"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create global version: %v", err)
	}
	if _, _, _, err := s.ActivateVersion(ctx, global.SecretPolicyVersionID, "admin-1"); err != nil {
		t.Fatalf("failed to activate global version: %v", err)
	}

	tenantSpecific, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, TenantID: tenantA, AllowedWorkloadIDs: []byte(`["svc-tenant-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create tenant-specific version: %v", err)
	}
	if _, _, _, err := s.ActivateVersion(ctx, tenantSpecific.SecretPolicyVersionID, "admin-1"); err != nil {
		t.Fatalf("failed to activate tenant-specific version: %v", err)
	}

	// Tenant A sees the tenant-specific override (most specific).
	gotA, err := s.FindApplicableVersionByPath(ctx, "kv/payroll/db", tenantA, nil)
	if err != nil {
		t.Fatalf("unexpected error for tenant A: %v", err)
	}
	if gotA.SecretPolicyVersionID != tenantSpecific.SecretPolicyVersionID {
		t.Errorf("expected tenant-specific version for tenant A, got %s", gotA.SecretPolicyVersionID)
	}

	// Tenant B must NOT see tenant A's override — only the global fallback.
	gotB, err := s.FindApplicableVersionByPath(ctx, "kv/payroll/db", tenantB, nil)
	if err != nil {
		t.Fatalf("unexpected error for tenant B: %v", err)
	}
	if gotB.SecretPolicyVersionID != global.SecretPolicyVersionID {
		t.Errorf("expected global fallback for tenant B, got %s", gotB.SecretPolicyVersionID)
	}

	// Unregistered path → ErrSecretPolicyNotFound, not a generic error.
	if _, err := s.FindApplicableVersionByPath(ctx, "kv/does/not/exist", nil, nil); !errors.Is(err, domain.ErrSecretPolicyNotFound) {
		t.Fatalf("expected ErrSecretPolicyNotFound for unregistered path, got %v", err)
	}
}

// ── secret_leases ────────────────────────────────────────────────────────────

func TestPgStore_CreateLease_IdempotentOnRequestID(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")
	v, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create version: %v", err)
	}

	params := domain.CreateLeaseParams{
		RequestID: "req-1", SecretPolicyVersionID: v.SecretPolicyVersionID,
		SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/payroll/db",
		RequestedByPrincipalID: "svc-a", ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
	}

	l1, created1, err := s.CreateLease(ctx, params)
	if err != nil || !created1 {
		t.Fatalf("expected created=true on first write, got created=%v err=%v", created1, err)
	}

	l2, created2, err := s.CreateLease(ctx, params)
	if err != nil || created2 {
		t.Fatalf("expected created=false on retry, got created=%v err=%v", created2, err)
	}
	if l2.LeaseID != l1.LeaseID {
		t.Errorf("expected same lease_id on idempotent retry")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM secret_leases WHERE request_id = $1`, "req-1").Scan(&count); err != nil {
		t.Fatalf("failed to count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 lease row after retry, got %d", count)
	}
}

func TestPgStore_RevokeLease_TransitionAndIdempotency(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")
	v, _, _ := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	lease, _, err := s.CreateLease(ctx, domain.CreateLeaseParams{
		RequestID: "req-1", SecretPolicyVersionID: v.SecretPolicyVersionID,
		SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/payroll/db",
		RequestedByPrincipalID: "svc-a", ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
	})
	if err != nil {
		t.Fatalf("failed to create lease: %v", err)
	}

	revoked, transitioned, err := s.RevokeLease(ctx, lease.LeaseID)
	if err != nil || !transitioned || revoked.Status != "REVOKED" {
		t.Fatalf("expected revoke to succeed, got status=%v transitioned=%v err=%v", revoked, transitioned, err)
	}

	retryRevoked, retryTransitioned, err := s.RevokeLease(ctx, lease.LeaseID)
	if err != nil || retryTransitioned || retryRevoked.Status != "REVOKED" {
		t.Fatalf("expected idempotent no-op re-revoking, got transitioned=%v err=%v", retryTransitioned, err)
	}
}

func TestPgStore_LeaseStatus_ExpiredIsComputedNotStored(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")
	v, _, _ := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})

	lease, _, err := s.CreateLease(ctx, domain.CreateLeaseParams{
		RequestID: "req-expired", SecretPolicyVersionID: v.SecretPolicyVersionID,
		SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/payroll/db",
		RequestedByPrincipalID: "svc-a", ExpiresAt: time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond),
	})
	if err != nil {
		t.Fatalf("failed to create lease: %v", err)
	}

	// The stored column is still 'GRANTED' — EXPIRED is a read-time
	// computation, context.md §7.1, never a background job.
	var storedStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM secret_leases WHERE lease_id = $1`, lease.LeaseID).Scan(&storedStatus); err != nil {
		t.Fatalf("failed to read raw stored status: %v", err)
	}
	if storedStatus != "GRANTED" {
		t.Fatalf("expected the stored column to remain GRANTED, got %q", storedStatus)
	}

	got, err := s.FindLeaseByID(ctx, lease.LeaseID)
	if err != nil || got.Status != "EXPIRED" {
		t.Fatalf("expected FindLeaseByID to report computed status EXPIRED, got %+v err=%v", got, err)
	}

	listed, err := s.ListLeases(ctx, store.LeaseListFilter{})
	if err != nil || len(listed) != 1 || listed[0].Status != "EXPIRED" {
		t.Fatalf("expected ListLeases to report computed status EXPIRED, got %+v err=%v", listed, err)
	}

	// Revoking an already-expired lease is not a valid transition — there
	// is nothing left to revoke.
	if _, _, err := s.RevokeLease(ctx, lease.LeaseID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition revoking an EXPIRED lease, got %v", err)
	}
}

func TestPgStore_RevokeLeasesBySecretPath_MassRevokeForRotation(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")
	v, _, _ := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-a","svc-b"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})

	lease1, _, _ := s.CreateLease(ctx, domain.CreateLeaseParams{
		RequestID: "req-1", SecretPolicyVersionID: v.SecretPolicyVersionID, SecretClass: "DATABASE_CREDENTIAL",
		SecretPath: "kv/payroll/db", RequestedByPrincipalID: "svc-a", ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
	})
	lease2, _, _ := s.CreateLease(ctx, domain.CreateLeaseParams{
		RequestID: "req-2", SecretPolicyVersionID: v.SecretPolicyVersionID, SecretClass: "DATABASE_CREDENTIAL",
		SecretPath: "kv/payroll/db", RequestedByPrincipalID: "svc-b", ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
	})

	revoked, err := s.RevokeLeasesBySecretPath(ctx, "kv/payroll/db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(revoked) != 2 {
		t.Fatalf("expected 2 leases revoked, got %d", len(revoked))
	}

	for _, id := range []string{lease1.LeaseID, lease2.LeaseID} {
		l, err := s.FindLeaseByID(ctx, id)
		if err != nil || l.Status != "REVOKED" {
			t.Errorf("expected lease %s to be REVOKED, got %+v err=%v", id, l, err)
		}
	}

	// A second call finds nothing left to revoke — not an error, just empty.
	revokedAgain, err := s.RevokeLeasesBySecretPath(ctx, "kv/payroll/db")
	if err != nil || len(revokedAgain) != 0 {
		t.Fatalf("expected 0 leases on second rotation call, got %d err=%v", len(revokedAgain), err)
	}
}

// ── secret_access_audit_log ──────────────────────────────────────────────────

func TestPgStore_RecordAuditEntry_AndListFilters(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	entries := []domain.RecordAuditEntryParams{
		{EventType: "REQUESTED", SecretPath: "kv/a", RequestedByPrincipalID: "svc-a"},
		{EventType: "GRANTED", SecretPath: "kv/a", RequestedByPrincipalID: "svc-a"},
		{EventType: "DENIED", SecretPath: "kv/b", RequestedByPrincipalID: "svc-c"},
	}
	for _, e := range entries {
		if _, err := s.RecordAuditEntry(ctx, e); err != nil {
			t.Fatalf("failed to record audit entry: %v", err)
		}
	}

	all, err := s.ListAuditLog(ctx, store.AuditListFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("expected 3 audit entries, got %d err=%v", len(all), err)
	}

	byPrincipal, err := s.ListAuditLog(ctx, store.AuditListFilter{RequestedByPrincipalID: "svc-a"})
	if err != nil || len(byPrincipal) != 2 {
		t.Fatalf("expected 2 entries for svc-a, got %d err=%v", len(byPrincipal), err)
	}

	byEventType, err := s.ListAuditLog(ctx, store.AuditListFilter{EventType: "DENIED"})
	if err != nil || len(byEventType) != 1 {
		t.Fatalf("expected 1 DENIED entry, got %d err=%v", len(byEventType), err)
	}
}

func TestPgStore_RotationAuditDedup(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	requestID := "rot-1"

	// Nothing recorded yet.
	found, err := s.FindAuditEntryByRotationRequestID(ctx, requestID)
	if err != nil || found != nil {
		t.Fatalf("expected no prior rotation entry, got %+v err=%v", found, err)
	}

	rid := requestID
	entry, err := s.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType: "ROTATED", SecretPath: "kv/payroll/db", RequestedByPrincipalID: "admin-1", RequestID: &rid,
	})
	if err != nil {
		t.Fatalf("failed to record ROTATED entry: %v", err)
	}

	found, err = s.FindAuditEntryByRotationRequestID(ctx, requestID)
	if err != nil || found == nil || found.AuditLogID != entry.AuditLogID {
		t.Fatalf("expected to find the just-recorded ROTATED entry, got %+v err=%v", found, err)
	}

	// A second INSERT with the same request_id and event_type=ROTATED
	// must violate the partial unique index — proving the DB-level
	// backstop is real, not just the handler's pre-check.
	if _, err := s.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType: "ROTATED", SecretPath: "kv/payroll/db", RequestedByPrincipalID: "admin-1", RequestID: &rid,
	}); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Fatalf("expected the unique constraint violation to surface as ErrStoreUnavailable, got %v", err)
	}
}

// ── error wrapping ───────────────────────────────────────────────────────────

func TestPgStore_ErrorsWrapErrStoreUnavailable(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	if _, err := pool.Exec(ctx, `DROP TABLE secret_access_audit_log, secret_leases, secret_policy_versions, secret_policies CASCADE;`); err != nil {
		t.Fatalf("failed to drop tables for test setup: %v", err)
	}

	if _, _, err := s.CreateSecretPolicy(ctx, domain.CreateSecretPolicyParams{SecretClass: "X", SecretPath: "kv/x", CreatedByPrincipalID: "admin-1"}); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Errorf("CreateSecretPolicy: expected ErrStoreUnavailable, got %v", err)
	}
	if _, err := s.FindSecretPolicyByID(ctx, "00000000-0000-0000-0000-000000000001"); !errors.Is(err, domain.ErrStoreUnavailable) {
		t.Errorf("FindSecretPolicyByID: expected ErrStoreUnavailable, got %v", err)
	}
}
