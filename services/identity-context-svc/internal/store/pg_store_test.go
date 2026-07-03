package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/store"
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

	_, filename, _, _ := runtime.Caller(0)
	migPath := filepath.Join(filepath.Dir(filename), "../../deployments/migrations/000001_initial_schema.up.sql")
	migSQL, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatalf("failed to read migration file %s: %v", migPath, err)
	}

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS access_decision_log, delegated_authorities, principal_role_assignments, principals CASCADE;`)
	if _, err := pool.Exec(ctx, string(migSQL)); err != nil {
		t.Fatalf("failed to execute migration: %v", err)
	}

	return pool
}

func insertPrincipal(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, tenantID, subject, status string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO principals (principal_id, tenant_id, principal_type, identity_provider_subject, email, display_name, status)
		VALUES ($1, $2, 'HUMAN', $3, $4, 'Test Principal', $5)
	`, id, tenantID, subject, subject+"@zoiko.io", status)
	if err != nil {
		t.Fatalf("failed to insert test principal: %v", err)
	}
}

func TestPgStore_FindByIDPSubject_Integration(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-active", "t-1", "idp|active", "ACTIVE")
	insertPrincipal(t, ctx, pool, "p-disabled", "t-1", "idp|disabled", "DISABLED")

	got, err := s.FindByIDPSubject(ctx, "idp|active", "t-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.PrincipalID != "p-active" {
		t.Fatalf("expected active principal, got %+v", got)
	}

	// DISABLED principals are excluded per the query's status filter.
	got, err = s.FindByIDPSubject(ctx, "idp|disabled", "t-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for disabled principal, got %+v", got)
	}

	// Tenant mismatch must not leak a principal from another tenant.
	got, err = s.FindByIDPSubject(ctx, "idp|active", "t-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for mismatched tenant, got %+v", got)
	}
}

func TestPgStore_FindByID_Integration(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")

	got, err := s.FindByID(ctx, "p-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.TenantID != "t-1" {
		t.Fatalf("expected principal p-1, got %+v", got)
	}

	got, err = s.FindByID(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for unknown principal, got %+v", got)
	}
}

func TestPgStore_FindActiveRoleAssignments_Integration(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")

	now := time.Now().UTC()
	insertAssignment := func(id, roleID string, legalEntityID *string, from, to time.Time) {
		_, err := pool.Exec(ctx, `
			INSERT INTO principal_role_assignments
				(assignment_id, principal_id, tenant_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by)
			VALUES ($1, 'p-1', 't-1', $2, $3, $4, $5, 'admin')
		`, id, roleID, legalEntityID, from, to)
		if err != nil {
			t.Fatalf("failed to insert role assignment %s: %v", id, err)
		}
	}
	entityA := "entity-a"
	entityB := "entity-b"

	insertAssignment("a-global", "role-global", nil, now.Add(-time.Hour), now.Add(time.Hour))
	insertAssignment("a-entity-a", "role-a", &entityA, now.Add(-time.Hour), now.Add(time.Hour))
	insertAssignment("a-entity-b", "role-b", &entityB, now.Add(-time.Hour), now.Add(time.Hour))
	insertAssignment("a-expired", "role-expired", nil, now.Add(-2*time.Hour), now.Add(-time.Hour))

	// No filter: expect the two currently-in-window assignments regardless of entity,
	// excluding the expired one.
	all, err := s.FindActiveRoleAssignments(ctx, "p-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 in-window assignments (global + entity-a + entity-b), got %d: %+v", len(all), all)
	}

	// Filter by entity-a: expect the global assignment plus the entity-a assignment,
	// but not entity-b's.
	scoped, err := s.FindActiveRoleAssignments(ctx, "p-1", &entityA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("expected 2 assignments scoped to entity-a (global + entity-a), got %d: %+v", len(scoped), scoped)
	}
	for _, a := range scoped {
		if a.AssignmentID == "a-entity-b" {
			t.Fatalf("entity-b assignment leaked into entity-a scoped query: %+v", scoped)
		}
	}
}

func TestPgStore_FindActiveDelegations_Integration(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "delegator", "t-1", "idp|delegator", "ACTIVE")
	insertPrincipal(t, ctx, pool, "delegate", "t-1", "idp|delegate", "ACTIVE")

	now := time.Now().UTC()
	insertDelegation := func(id, revocationStatus string, from, to time.Time) {
		_, err := pool.Exec(ctx, `
			INSERT INTO delegated_authorities
				(delegated_authority_id, delegator_principal_id, delegate_principal_id, tenant_id,
				 scope_type, legal_entity_id, authority_limit_type, authority_limit_value,
				 effective_from, effective_to, revocation_status)
			VALUES ($1, 'delegator', 'delegate', 't-1', 'GLOBAL', NULL, 'FINANCIAL_THRESHOLD', 1000, $2, $3, $4)
		`, id, from, to, revocationStatus)
		if err != nil {
			t.Fatalf("failed to insert delegation %s: %v", id, err)
		}
	}

	insertDelegation("d-active", "ACTIVE", now.Add(-time.Hour), now.Add(time.Hour))
	insertDelegation("d-revoked", "REVOKED", now.Add(-time.Hour), now.Add(time.Hour))
	insertDelegation("d-expired", "ACTIVE", now.Add(-2*time.Hour), now.Add(-time.Hour))

	got, err := s.FindActiveDelegations(ctx, "delegate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].DelegatedAuthorityID != "d-active" {
		t.Fatalf("expected only d-active, got %+v", got)
	}
}

func TestPgStore_UpdateStatus_Integration(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	insertPrincipal(t, ctx, pool, "p-1", "t-1", "idp|one", "ACTIVE")

	if err := s.UpdateStatus(ctx, "p-1", domain.PrincipalStatusSuspended, "admin", "corr-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := s.FindByID(ctx, "p-1")
	if err != nil || got == nil {
		t.Fatalf("failed to reload principal: %v", err)
	}
	if got.Status != domain.PrincipalStatusSuspended {
		t.Fatalf("expected SUSPENDED, got %s", got.Status)
	}

	var logCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM access_decision_log WHERE principal_id = 'p-1'`).Scan(&logCount); err != nil {
		t.Fatalf("failed to count decision log rows: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("expected exactly 1 decision log row after transition, got %d", logCount)
	}

	// Idempotent re-apply of the same status must not add a second decision log row.
	if err := s.UpdateStatus(ctx, "p-1", domain.PrincipalStatusSuspended, "admin", "corr-2"); err != nil {
		t.Fatalf("unexpected error on idempotent re-apply: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM access_decision_log WHERE principal_id = 'p-1'`).Scan(&logCount); err != nil {
		t.Fatalf("failed to count decision log rows: %v", err)
	}
	if logCount != 1 {
		t.Fatalf("expected idempotent re-apply to leave decision log count at 1, got %d", logCount)
	}
}
