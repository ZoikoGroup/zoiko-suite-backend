package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/store"
)

// Gap 10 on a real database (AUTHZ_IT_DSN, migrated to 000022): a stale
// expected_version is refused and changes nothing, and a replaced bundle's
// previous action set survives in authz_config_history.

func newVersionedRole(t *testing.T, pool *pgxpool.Pool, s *store.PgStore, tenant string) *domain.Role {
	t.Helper()
	role, _, err := s.CreateRole(context.Background(), domain.CreateRoleParams{
		TenantID: tenant, RoleCode: "IT_" + uuid.NewString()[:8], RoleName: "it", RoleScopeType: "TENANT", CreatedByPrincipalID: "it",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	return role
}

func TestVersionsIT_StaleExpectedVersionRefused(t *testing.T) {
	pool := outboxPool(t)
	s := store.New(pool, zap.NewNop())
	tenant := uuid.NewString()
	role := newVersionedRole(t, pool, s, tenant)
	if role.Version != 1 {
		t.Fatalf("new role version = %d, want 1", role.Version)
	}

	retired, err := s.SetRoleActive(context.Background(), role.RoleID, tenant, false, 1)
	if err != nil || retired.Version != 2 || retired.ActiveFlag {
		t.Fatalf("retire at the current version: %+v, %v", retired, err)
	}
	// A second writer still holding version 1.
	if _, err := s.SetRoleActive(context.Background(), role.RoleID, tenant, true, 1); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale expected_version: want ErrVersionConflict, got %v", err)
	}
	after, _ := s.FindRoleByID(context.Background(), role.RoleID)
	if after.ActiveFlag || after.Version != 2 {
		t.Fatalf("a refused write changed the role: %+v", after)
	}
	// Another tenant's role id still reads as not found, never as a conflict.
	if _, err := s.SetRoleActive(context.Background(), role.RoleID, uuid.NewString(), true, 2); !errors.Is(err, domain.ErrRoleNotFound) {
		t.Fatalf("foreign tenant: want ErrRoleNotFound, got %v", err)
	}
	// Omitted version is unchecked, as before.
	if _, err := s.SetRoleActive(context.Background(), role.RoleID, tenant, true, 0); err != nil {
		t.Fatalf("unchecked write: %v", err)
	}
}

func TestVersionsIT_BundleReplaceKeepsHistoryAndHonoursVersion(t *testing.T) {
	pool := outboxPool(t)
	s := store.New(pool, zap.NewNop())
	tenant := uuid.NewString()
	role := newVersionedRole(t, pool, s, tenant)
	ctx := context.Background()

	b1, created, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{RoleID: role.RoleID, BundleCode: "B", PermittedActions: []string{"payment.prepare"}})
	if err != nil || !created || b1.Version != 1 {
		t.Fatalf("create: %+v created=%v err=%v", b1, created, err)
	}
	b2, created, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{RoleID: role.RoleID, BundleCode: "B", PermittedActions: []string{"report.view"}, ExpectedVersion: 1})
	if err != nil || created || b2.Version != 2 {
		t.Fatalf("replace at version 1: %+v created=%v err=%v", b2, created, err)
	}
	if _, _, err := s.CreatePermissionBundle(ctx, domain.CreatePermissionBundleParams{RoleID: role.RoleID, BundleCode: "B", PermittedActions: []string{"payment.release"}, ExpectedVersion: 1}); !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale replace: want ErrVersionConflict, got %v", err)
	}
	now, _ := s.ListPermissionBundles(ctx, role.RoleID, tenant)
	if len(now) != 1 || now[0].PermittedActions[0] != "report.view" {
		t.Fatalf("a refused replace changed the bundle: %+v", now)
	}

	// The overwritten action set is still on record.
	var v1 string
	if err := pool.QueryRow(ctx, `SELECT snapshot->>'permitted_actions' FROM authz_config_history
		WHERE object_type = 'permission_bundles' AND object_id = $1 AND version = 1`, b1.PermissionBundleID).Scan(&v1); err != nil {
		t.Fatalf("history read: %v", err)
	}
	if v1 != `["payment.prepare"]` {
		t.Fatalf("history version 1 = %s, want the replaced [\"payment.prepare\"]", v1)
	}
}
