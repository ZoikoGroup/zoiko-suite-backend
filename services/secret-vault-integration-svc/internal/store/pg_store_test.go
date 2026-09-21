package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
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
		// A SILENT SKIP HERE TURNS THE WHOLE STORE SUITE INTO NOTHING while
		// `go test ./...` still prints ok. Skip locally, where a developer
		// without Postgres is a normal state, and FAIL wherever the run claims
		// to be a verification. CI is set by GitHub Actions; REQUIRE_DB_TESTS
		// is the local opt-in for reproducing a certification run by hand.
		if os.Getenv("CI") != "" || os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL is not set, but CI or REQUIRE_DB_TESTS is: " +
				"this run claims to verify the store and would instead have skipped every test in it")
		}
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS secret_access_audit_log, secret_leases, secret_policy_versions, secret_policies CASCADE;`)

	// Every .up.sql in the migrations directory, in filename order — NOT a
	// hardcoded list. This list used to be literal, so adding a migration and
	// forgetting to extend it left the whole store suite running against the
	// previous schema: every test still passed, against a database the service
	// would never see. The numbering prefix is what makes lexical order the
	// right order.
	for _, path := range migrationFiles(t) {
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read migration file %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("failed to execute migration %s: %v", path, err)
		}
	}
	return pool
}

// migrationFiles returns every forward migration, in apply order.
func migrationFiles(t *testing.T) []string {
	t.Helper()
	const dir = "../../deployments/migrations"
	matches, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		t.Fatalf("failed to list migrations: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no migrations found in %s — the suite would run against an empty schema", dir)
	}
	sort.Strings(matches)
	return matches
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

	revoked, transitioned, err := s.RevokeLease(ctx, lease.LeaseID, "")
	if err != nil || !transitioned || revoked.Status != "REVOKED" {
		t.Fatalf("expected revoke to succeed, got status=%v transitioned=%v err=%v", revoked, transitioned, err)
	}

	retryRevoked, retryTransitioned, err := s.RevokeLease(ctx, lease.LeaseID, "")
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

	got, err := s.FindLeaseByID(ctx, lease.LeaseID, "")
	if err != nil || got.Status != "EXPIRED" {
		t.Fatalf("expected FindLeaseByID to report computed status EXPIRED, got %+v err=%v", got, err)
	}

	listed, err := s.ListLeases(ctx, store.LeaseListFilter{})
	if err != nil || len(listed) != 1 || listed[0].Status != "EXPIRED" {
		t.Fatalf("expected ListLeases to report computed status EXPIRED, got %+v err=%v", listed, err)
	}

	// Revoking an already-expired lease is not a valid transition — there
	// is nothing left to revoke.
	if _, _, err := s.RevokeLease(ctx, lease.LeaseID, ""); !errors.Is(err, domain.ErrInvalidTransition) {
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
		l, err := s.FindLeaseByID(ctx, id, "")
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

// TestPgStore_ActorIsPersistedAndDistinctFromSubject proves migration 000004's
// column round-trips through a real Postgres, and that the two principal
// columns stay independent.
//
// The handler-level test asserts the right value is passed in; this asserts it
// survives the INSERT and comes back on the read path. Both are needed: the
// column was added by ALTER after the initial schema, so an un-migrated
// database and a stale column list are the two ways this silently reverts to
// storing nothing.
func TestPgStore_ActorIsPersistedAndDistinctFromSubject(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	const holder = "svc-lease-holder"
	const operator = "principal-on-call"

	if _, err := s.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType:              "REVOKED",
		SecretPath:             "kv/actor",
		RequestedByPrincipalID: holder,
		ActedByPrincipalID:     strPtr(operator),
	}); err != nil {
		t.Fatalf("failed to record REVOKED entry: %v", err)
	}

	got, err := s.ListAuditLog(ctx, store.AuditListFilter{EventType: "REVOKED"})
	if err != nil {
		t.Fatalf("ListAuditLog failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 REVOKED entry, got %d", len(got))
	}
	e := got[0]
	if e.RequestedByPrincipalID != holder {
		t.Errorf("subject: want %q, got %q", holder, e.RequestedByPrincipalID)
	}
	if e.ActedByPrincipalID == nil {
		t.Fatal("acted_by_principal_id came back nil — the column is not being written or not being read")
	}
	if *e.ActedByPrincipalID != operator {
		t.Errorf("actor: want %q, got %q", operator, *e.ActedByPrincipalID)
	}

	// An entry written with no actor stays NULL rather than defaulting to the
	// subject. Pre-000004 rows are genuinely actor-less and must read as such;
	// silently copying the subject across would manufacture evidence.
	if _, err := s.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType:              "REQUESTED",
		SecretPath:             "kv/actor",
		RequestedByPrincipalID: holder,
	}); err != nil {
		t.Fatalf("failed to record actor-less entry: %v", err)
	}
	legacy, err := s.ListAuditLog(ctx, store.AuditListFilter{EventType: "REQUESTED"})
	if err != nil || len(legacy) != 1 {
		t.Fatalf("expected 1 REQUESTED entry, got %d err=%v", len(legacy), err)
	}
	if legacy[0].ActedByPrincipalID != nil {
		t.Errorf("an entry recorded without an actor should read back NULL, got %q", *legacy[0].ActedByPrincipalID)
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

// ── row-level security ───────────────────────────────────────────────────────

// TestPgStore_TenantIsolation_ListVersionHistory proves the fix for the
// real gap found in this row: ListVersionHistory previously took no
// tenant at all, so any caller could list every tenant's
// allowed_workload_ids and lease-duration limits for a policy. Tenant B
// must see the global version but not tenant A's.
func TestPgStore_TenantIsolation_ListVersionHistory(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")
	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"

	globalV, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-global"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create global version: %v", err)
	}
	tenantAV, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, TenantID: strPtr(tenantA), AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond).Add(time.Second), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create tenant A version: %v", err)
	}

	// Probe: tenant B's context, listing the same policy tenant A has a version on.
	resultsB, err := s.ListVersionHistory(ctx, p.SecretPolicyID, tenantB)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, v := range resultsB {
		if v.SecretPolicyVersionID == tenantAV.SecretPolicyVersionID {
			t.Fatalf("ISOLATION FAILURE: ListVersionHistory returned tenant A's version under tenant B's context: %+v", v)
		}
	}
	foundGlobal := false
	for _, v := range resultsB {
		if v.SecretPolicyVersionID == globalV.SecretPolicyVersionID {
			foundGlobal = true
		}
	}
	if !foundGlobal {
		t.Fatalf("expected tenant B to see the global version, got %+v", resultsB)
	}

	// Sanity: tenant A can see both its own version and the global one.
	resultsA, err := s.ListVersionHistory(ctx, p.SecretPolicyID, tenantA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resultsA) != 2 {
		t.Fatalf("expected tenant A to see 2 versions (its own + global), got %d: %+v", len(resultsA), resultsA)
	}
}

// TestPgStore_TenantIsolation_FindLeaseByID_RevokeLease proves tenant B
// cannot read or revoke tenant A's lease by ID alone, at the DB layer
// (independent of the handler's own refuseForeignRow check).
func TestPgStore_TenantIsolation_FindLeaseByID_RevokeLease(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"

	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")
	v, _, _ := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, TenantID: strPtr(tenantA), AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	lease, _, err := s.CreateLease(ctx, domain.CreateLeaseParams{
		RequestID: "req-tenant-a", SecretPolicyVersionID: v.SecretPolicyVersionID,
		SecretClass: "DATABASE_CREDENTIAL", SecretPath: "kv/payroll/db", TenantID: strPtr(tenantA),
		RequestedByPrincipalID: "svc-a", ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
	})
	if err != nil {
		t.Fatalf("failed to create tenant A's lease: %v", err)
	}

	// Probe: tenant B's context, tenant A's lease ID.
	if got, err := s.FindLeaseByID(ctx, lease.LeaseID, tenantB); !errors.Is(err, domain.ErrLeaseNotFound) {
		t.Fatalf("ISOLATION FAILURE: FindLeaseByID returned tenant A's lease under tenant B's context: %+v (err=%v)", got, err)
	}
	if _, _, err := s.RevokeLease(ctx, lease.LeaseID, tenantB); !errors.Is(err, domain.ErrLeaseNotFound) {
		t.Fatalf("ISOLATION FAILURE: RevokeLease succeeded on tenant A's lease under tenant B's context: err=%v", err)
	}

	// Verify tenant A's lease is genuinely still GRANTED.
	stillGranted, err := s.FindLeaseByID(ctx, lease.LeaseID, tenantA)
	if err != nil || stillGranted.Status != "GRANTED" {
		t.Fatalf("expected tenant A's lease to remain GRANTED and readable by tenant A, got %+v err=%v", stillGranted, err)
	}
}

// TestPgStore_PlatformScope_ActivateVersionAcrossTenants proves the
// platform-scope RLS bypass actually works: ActivateVersion must be able
// to supersede/activate a TENANT-scoped version, not just global ones,
// since SECRET_POLICY_VERSION_ACTIVATE is a platform-authorized action by
// design. Without this test, a broken bypass would silently make
// activation only ever work for global-scope versions in production.
func TestPgStore_PlatformScope_ActivateVersionAcrossTenants(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := "11111111-1111-1111-1111-111111111111"
	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")

	first, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, TenantID: strPtr(tenantA), AllowedWorkloadIDs: []byte(`["svc-a"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create tenant A's first version: %v", err)
	}
	activated, _, transitioned, err := s.ActivateVersion(ctx, first.SecretPolicyVersionID, "platform-admin")
	if err != nil || !transitioned || activated.VersionStatus != "ACTIVE" {
		t.Fatalf("expected platform-scoped activation of a tenant-scoped version to succeed, got %+v transitioned=%v err=%v", activated, transitioned, err)
	}

	second, _, err := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, TenantID: strPtr(tenantA), AllowedWorkloadIDs: []byte(`["svc-a2"]`),
		MaxLeaseDurationSeconds: 600, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond).Add(time.Second), CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create tenant A's second version: %v", err)
	}
	_, superseded, transitioned, err := s.ActivateVersion(ctx, second.SecretPolicyVersionID, "platform-admin")
	if err != nil || !transitioned {
		t.Fatalf("expected second activation to succeed, err=%v", err)
	}
	if len(superseded) != 1 || superseded[0].SecretPolicyVersionID != first.SecretPolicyVersionID {
		t.Fatalf("expected activating the second tenant-scoped version to supersede the first, got %+v", superseded)
	}
}

// TestPgStore_PlatformScope_RevokeLeasesBySecretPath_AcrossTenants proves
// the platform-scope bypass lets secret rotation revoke every tenant's
// leases on a path, not just one — the whole point of the fix (context.md
// §7.2: rotating a secret must invalidate leases across the board).
func TestPgStore_PlatformScope_RevokeLeasesBySecretPath_AcrossTenants(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"
	p := createTestPolicy(t, ctx, s, "DATABASE_CREDENTIAL", "kv/payroll/db")
	v, _, _ := s.CreateSecretPolicyVersion(ctx, domain.CreateSecretPolicyVersionParams{
		SecretPolicyID: p.SecretPolicyID, AllowedWorkloadIDs: []byte(`["svc-a","svc-b"]`),
		MaxLeaseDurationSeconds: 300, EffectiveFrom: time.Now().UTC().Truncate(time.Microsecond), CreatedByPrincipalID: "admin-1",
	})

	leaseA, _, _ := s.CreateLease(ctx, domain.CreateLeaseParams{
		RequestID: "req-a", SecretPolicyVersionID: v.SecretPolicyVersionID, SecretClass: "DATABASE_CREDENTIAL",
		SecretPath: "kv/payroll/db", TenantID: strPtr(tenantA), RequestedByPrincipalID: "svc-a",
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
	})
	leaseB, _, _ := s.CreateLease(ctx, domain.CreateLeaseParams{
		RequestID: "req-b", SecretPolicyVersionID: v.SecretPolicyVersionID, SecretClass: "DATABASE_CREDENTIAL",
		SecretPath: "kv/payroll/db", TenantID: strPtr(tenantB), RequestedByPrincipalID: "svc-b",
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
	})

	revoked, err := s.RevokeLeasesBySecretPath(ctx, "kv/payroll/db")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(revoked) != 2 {
		t.Fatalf("expected rotation to revoke both tenants' leases, got %d: %+v", len(revoked), revoked)
	}

	for _, tc := range []struct{ tenant, leaseID string }{{tenantA, leaseA.LeaseID}, {tenantB, leaseB.LeaseID}} {
		got, err := s.FindLeaseByID(ctx, tc.leaseID, tc.tenant)
		if err != nil || got.Status != "REVOKED" {
			t.Errorf("expected lease %s (tenant %s) to be REVOKED after rotation, got %+v err=%v", tc.leaseID, tc.tenant, got, err)
		}
	}
}

// TestPgStore_RotationAuditDedup_TenantBoundEntry covers the case the original
// dedup test did not: a ROTATED entry that carries a tenant_id.
//
// ROTATED rows used to be written with tenant_id = NULL, which made every
// rotation invisible in the audit query of the tenants whose leases it
// revoked. Binding the row to the rotating tenant fixes that — but it also
// puts the row behind this table's RLS policy, and this lookup is the
// idempotency check that stops a retried request from rotating the material
// twice. If the lookup cannot see a tenant-bound row it reports "no prior
// rotation" and the retry rotates again, which is the exact failure the
// request_id dedup exists to prevent. Hence withPlatformScope on the query.
//
// Note on what this test can and cannot prove here: TEST_DATABASE_URL
// normally points at a superuser, and Postgres exempts superusers from RLS
// entirely, so this asserts the query's correctness rather than the policy's
// enforcement. The enforcement half is proven by scripts/audit.sh, which
// drives the running container — that connects as app_secret_vault_integration,
// an ordinary role the policy genuinely applies to.
func TestPgStore_RotationAuditDedup_TenantBoundEntry(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	requestID := "rot-tenant-bound-1"
	rid := requestID
	tenant := "11111111-1111-1111-1111-111111111111"

	entry, err := s.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType:              "ROTATED",
		SecretPath:             "kv/payroll/db",
		RequestedByPrincipalID: "admin-1",
		TenantID:               &tenant,
		RequestID:              &rid,
		OutcomeDetail:          "revoked_lease_count=2",
	})
	if err != nil {
		t.Fatalf("failed to record tenant-bound ROTATED entry: %v", err)
	}
	if entry.TenantID == nil || *entry.TenantID != tenant {
		t.Fatalf("recorded entry lost its tenant binding: %+v", entry.TenantID)
	}

	found, err := s.FindAuditEntryByRotationRequestID(ctx, requestID)
	if err != nil {
		t.Fatalf("dedup lookup failed: %v", err)
	}
	if found == nil {
		t.Fatal("dedup lookup did not find a tenant-bound ROTATED entry: a retried rotate would rotate the material a second time")
	}
	if found.AuditLogID != entry.AuditLogID {
		t.Errorf("dedup found %s, want %s", found.AuditLogID, entry.AuditLogID)
	}
	// The count has to survive the round trip: the replay response reads it
	// back out of this field, and reporting 0 there says the rotation revoked
	// nothing.
	if found.OutcomeDetail != "revoked_lease_count=2" {
		t.Errorf("outcome_detail = %q, want revoked_lease_count=2", found.OutcomeDetail)
	}
}

// TestPgStore_ListAuditLog_TenantScopedRotationIsVisible is the read half of
// the same fix, and the assertion that would have caught the original bug:
// the rotating tenant must actually get its ROTATED row back from the query
// the console and any auditor use. Before the fix this returned nothing,
// because ListAuditLog filters on tenant_id and the row had none.
func TestPgStore_ListAuditLog_TenantScopedRotationIsVisible(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	tenant := "11111111-1111-1111-1111-111111111111"
	rid := "rot-visible-1"
	secretPath := "kv/visible/db"

	if _, err := s.RecordAuditEntry(ctx, domain.RecordAuditEntryParams{
		EventType:              "ROTATED",
		SecretPath:             secretPath,
		RequestedByPrincipalID: "admin-1",
		TenantID:               &tenant,
		RequestID:              &rid,
		OutcomeDetail:          "revoked_lease_count=1",
	}); err != nil {
		t.Fatalf("failed to record ROTATED entry: %v", err)
	}

	got, err := s.ListAuditLog(ctx, store.AuditListFilter{
		TenantID:   &tenant,
		SecretPath: secretPath,
		EventType:  "ROTATED",
	})
	if err != nil {
		t.Fatalf("ListAuditLog failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the rotating tenant to see 1 ROTATED entry, got %d: rotation is invisible in its own audit trail", len(got))
	}

	// And it must NOT leak to a tenant that had nothing to do with it: a
	// secret_path is a platform-wide address, so a globally-readable rotation
	// row would let any tenant enumerate every other tenant's secret paths.
	other := "22222222-2222-2222-2222-222222222222"
	leaked, err := s.ListAuditLog(ctx, store.AuditListFilter{
		TenantID:   &other,
		SecretPath: secretPath,
		EventType:  "ROTATED",
	})
	if err != nil {
		t.Fatalf("ListAuditLog for the other tenant failed: %v", err)
	}
	if len(leaked) != 0 {
		t.Errorf("an unrelated tenant can see %d rotation entries for %s", len(leaked), secretPath)
	}
}
