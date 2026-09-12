package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

const roleColumns = `
	role_definition_id, tenant_id, role_code, role_name, role_scope_type,
	status, created_by_principal_id, COALESCE(updated_by_principal_id, '') AS updated_by_principal_id, correlation_id, created_at, updated_at
`

func scanRole(row pgx.Row, r *domain.RoleDefinition) error {
	var status string
	if err := row.Scan(
		&r.RoleDefinitionID, &r.TenantID, &r.RoleCode, &r.RoleName, &r.RoleScopeType,
		&status, &r.CreatedByPrincipalID, &r.UpdatedByPrincipalID, &r.CorrelationID, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return err
	}
	r.Status = domain.RoleStatus(status)
	return nil
}

// CreateRole inserts a new role definition, idempotent on
// (tenant_id, correlation_id).
func (s *PgStore) CreateRole(ctx context.Context, r *domain.RoleDefinition) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO role_definitions (
				role_definition_id, tenant_id, role_code, role_name, role_scope_type,
				status, created_by_principal_id, correlation_id, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, r.RoleDefinitionID, tenantID, r.RoleCode, r.RoleName, r.RoleScopeType,
			string(r.Status), r.CreatedByPrincipalID, r.CorrelationID, r.CreatedAt, r.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}
		row := tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND correlation_id = $2", tenantID, r.CorrelationID)
		return scanRole(row, r)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) GetRole(ctx context.Context, roleDefinitionID string) (*domain.RoleDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var r domain.RoleDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND role_definition_id = $2", tenantID, roleDefinitionID)
		return scanRole(row, &r)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRoleNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PgStore) ListRoles(ctx context.Context, filter domain.ListFilter) ([]domain.RoleDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.RoleDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := "SELECT " + roleColumns + " FROM role_definitions WHERE tenant_id = $1"
		args := []any{tenantID}
		if filter.Status != "" {
			args = append(args, filter.Status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if filter.ScopeType != "" {
			args = append(args, filter.ScopeType)
			query += fmt.Sprintf(" AND role_scope_type = $%d", len(args))
		}
		if filter.Query != "" {
			args = append(args, "%"+filter.Query+"%")
			// The user searched, so match on the two human/administrative names
			// together — a role_code is how the platform scopes its checks, and
			// role_name is how an administrator thinks about the role.
			query += fmt.Sprintf(" AND (role_code ILIKE $%d OR role_name ILIKE $%d)", len(args), len(args))
		}
		query += " ORDER BY created_at DESC"
		if filter.Limit > 0 {
			args = append(args, filter.Limit)
			query += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		if filter.Offset > 0 {
			args = append(args, filter.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.RoleDefinition
			if err := scanRole(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) UpdateRole(ctx context.Context, roleDefinitionID, roleName, status, updatedByPrincipalID string) (*domain.RoleDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out domain.RoleDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var current domain.RoleDefinition
		row := tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND role_definition_id = $2 FOR UPDATE", tenantID, roleDefinitionID)
		if err := scanRole(row, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRoleNotFound
			}
			return err
		}

		if roleName == "" {
			roleName = current.RoleName
		}
		if status == "" {
			status = string(current.Status)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE role_definitions SET role_name = $1, status = $2, updated_by_principal_id = $3, updated_at = now()
			WHERE tenant_id = $4 AND role_definition_id = $5
		`, roleName, status, updatedByPrincipalID, tenantID, roleDefinitionID); err != nil {
			return err
		}

		row = tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND role_definition_id = $2", tenantID, roleDefinitionID)
		return scanRole(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── permission bundles ─────────────────────────────────────────────────────────

const bundleColumns = `
	bundle_id, tenant_id, role_definition_id, bundle_code, permitted_actions,
	active_flag, COALESCE(updated_by_principal_id, '') AS updated_by_principal_id, correlation_id, created_at, updated_at
`

func scanBundle(row pgx.Row, b *domain.PermissionBundleDef) error {
	return row.Scan(
		&b.BundleID, &b.TenantID, &b.RoleDefinitionID, &b.BundleCode, &b.PermittedActions,
		&b.ActiveFlag, &b.UpdatedByPrincipalID, &b.CorrelationID, &b.CreatedAt, &b.UpdatedAt,
	)
}

// CreateBundle inserts a new permission bundle, idempotent on
// (tenant_id, correlation_id).
func (s *PgStore) CreateBundle(ctx context.Context, b *domain.PermissionBundleDef) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO permission_bundle_defs (
				bundle_id, tenant_id, role_definition_id, bundle_code, permitted_actions,
				active_flag, correlation_id, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, b.BundleID, tenantID, b.RoleDefinitionID, b.BundleCode, b.PermittedActions,
			b.ActiveFlag, b.CorrelationID, b.CreatedAt, b.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}
		row := tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND correlation_id = $2", tenantID, b.CorrelationID)
		return scanBundle(row, b)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) ListBundles(ctx context.Context, roleDefinitionID string) ([]domain.PermissionBundleDef, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.PermissionBundleDef
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND role_definition_id = $2 ORDER BY created_at DESC", tenantID, roleDefinitionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b domain.PermissionBundleDef
			if err := scanBundle(rows, &b); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetBundle reads one bundle, addressed by BOTH ids because it is written
// under a role's collection: a bundle_id that exists but belongs to a
// different role is indistinguishable from one that does not exist. That is
// deliberate — see ErrBundleNotFound's stance on existence oracles.
func (s *PgStore) GetBundle(ctx context.Context, roleDefinitionID, bundleID string) (*domain.PermissionBundleDef, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var b domain.PermissionBundleDef
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND role_definition_id = $2 AND bundle_id = $3", tenantID, roleDefinitionID, bundleID)
		return scanBundle(row, &b)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrBundleNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// UpdateBundle applies an edit to one bundle and stamps the verified caller.
// nil permittedActions / nil activeFlag mean "leave that field alone"; both
// nil would be a no-op, which the handler refuses before reaching here.
//
// Scoped by roleDefinitionID as well as bundleID, matching GetBundle: a write
// addressed under the wrong role resolves to ErrBundleNotFound rather than
// editing another role's bundle.
func (s *PgStore) UpdateBundle(ctx context.Context, roleDefinitionID, bundleID string, permittedActions []string, activeFlag *bool, updatedByPrincipalID string) (*domain.PermissionBundleDef, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out domain.PermissionBundleDef
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND role_definition_id = $2 AND bundle_id = $3 FOR UPDATE", tenantID, roleDefinitionID, bundleID)
		if err := scanBundle(row, &out); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrBundleNotFound
			}
			return err
		}

		sets := []string{}
		args := []any{}
		if permittedActions != nil {
			args = append(args, permittedActions)
			sets = append(sets, fmt.Sprintf("permitted_actions = $%d", len(args)))
		}
		if activeFlag != nil {
			args = append(args, *activeFlag)
			sets = append(sets, fmt.Sprintf("active_flag = $%d", len(args)))
		}
		args = append(args, updatedByPrincipalID)
		sets = append(sets, fmt.Sprintf("updated_by_principal_id = $%d", len(args)))
		if len(sets) == 0 {
			return nil
		}
		sets = append(sets, "updated_at = now()")
		query := fmt.Sprintf("UPDATE permission_bundle_defs SET %s WHERE tenant_id = $%d AND role_definition_id = $%d AND bundle_id = $%d",
			strings.Join(sets, ", "), len(args)+1, len(args)+2, len(args)+3)
		if _, err := tx.Exec(ctx, query, append(args, tenantID, roleDefinitionID, bundleID)...); err != nil {
			return err
		}

		row = tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND bundle_id = $2", tenantID, bundleID)
		return scanBundle(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAllBundles reads every bundle in the tenant, optionally narrowed to one
// role or one active state. The flat catalogue the role-scoped ListBundles
// cannot provide, and the read a detach flow needs to pick one bundle across
// roles.
func (s *PgStore) ListAllBundles(ctx context.Context, filter domain.BundleListFilter) ([]domain.PermissionBundleDef, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.PermissionBundleDef
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := "SELECT " + bundleColumns + " FROM permission_bundle_defs WHERE tenant_id = $1"
		args := []any{tenantID}
		if filter.RoleID != "" {
			args = append(args, filter.RoleID)
			query += fmt.Sprintf(" AND role_definition_id = $%d", len(args))
		}
		if filter.ActiveFlag != nil {
			args = append(args, *filter.ActiveFlag)
			query += fmt.Sprintf(" AND active_flag = $%d", len(args))
		}
		if filter.Query != "" {
			args = append(args, "%"+filter.Query+"%")
			query += fmt.Sprintf(" AND bundle_code ILIKE $%d", len(args))
		}
		query += " ORDER BY created_at DESC"
		if filter.Limit > 0 {
			args = append(args, filter.Limit)
			query += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		if filter.Offset > 0 {
			args = append(args, filter.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b domain.PermissionBundleDef
			if err := scanBundle(rows, &b); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
