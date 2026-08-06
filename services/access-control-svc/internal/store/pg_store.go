package store

import (
	"context"
	"errors"
	"fmt"

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
	status, created_by_principal_id, correlation_id, created_at, updated_at
`

func scanRole(row pgx.Row, r *domain.RoleDefinition) error {
	var status string
	if err := row.Scan(
		&r.RoleDefinitionID, &r.TenantID, &r.RoleCode, &r.RoleName, &r.RoleScopeType,
		&status, &r.CreatedByPrincipalID, &r.CorrelationID, &r.CreatedAt, &r.UpdatedAt,
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

func (s *PgStore) ListRoles(ctx context.Context, status string) ([]domain.RoleDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.RoleDefinition
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := "SELECT " + roleColumns + " FROM role_definitions WHERE tenant_id = $1"
		args := []any{tenantID}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

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

func (s *PgStore) UpdateRole(ctx context.Context, roleDefinitionID, roleName, status string) (*domain.RoleDefinition, error) {
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
			UPDATE role_definitions SET role_name = $1, status = $2, updated_at = now()
			WHERE tenant_id = $3 AND role_definition_id = $4
		`, roleName, status, tenantID, roleDefinitionID); err != nil {
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
	active_flag, correlation_id, created_at, updated_at
`

func scanBundle(row pgx.Row, b *domain.PermissionBundleDef) error {
	return row.Scan(
		&b.BundleID, &b.TenantID, &b.RoleDefinitionID, &b.BundleCode, &b.PermittedActions,
		&b.ActiveFlag, &b.CorrelationID, &b.CreatedAt, &b.UpdatedAt,
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
