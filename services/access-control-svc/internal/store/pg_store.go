// Package store provides the PostgreSQL implementation of the access-control-svc
// read and write model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers or domain packages.
//
// Ownership boundary (docs/architecture/03-microservices.md §9.4):
//   - WRITES:  roles, permission_bundles, role_permission_bundle_links
//   - READS:   everything above (no PrincipalRoleAssignment writes here)
//
// Every mutating method in this package must be idempotent (doctrine §3.7).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
)

// Store is the interface consumed by the handler layer.
type Store interface {
	// Roles
	CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error)
	FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error)
	ListRolesByTenant(ctx context.Context, tenantID string) ([]domain.Role, error)
	DeactivateRole(ctx context.Context, roleID string) (*domain.Role, error)

	// Permission Bundles
	CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, bool, error)
	FindBundleByID(ctx context.Context, bundleID string) (*domain.PermissionBundle, error)
	ListBundlesByTenant(ctx context.Context, tenantID string) ([]domain.PermissionBundle, error)
	UpdatePermissionBundleActions(ctx context.Context, params domain.UpdatePermissionBundleActionsParams) (*domain.PermissionBundle, error)

	// Role-Bundle Links
	CreateRolePermissionBundleLink(ctx context.Context, params domain.CreateRolePermissionBundleLinkParams) (*domain.RolePermissionBundleLink, error)
	RemoveRolePermissionBundleLink(ctx context.Context, params domain.RemoveRolePermissionBundleLinkParams) error
	ListBundlesForRole(ctx context.Context, roleID string) ([]domain.PermissionBundle, error)
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. The caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// ── roles ─────────────────────────────────────────────────────────────────────

const roleColumns = `role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_at, created_by_principal_id`

func scanRole(row pgx.Row) (*domain.Role, error) {
	r := &domain.Role{}
	err := row.Scan(&r.RoleID, &r.TenantID, &r.RoleCode, &r.RoleName, &r.RoleScopeType, &r.ActiveFlag, &r.CreatedAt, &r.CreatedByPrincipalID)
	return r, err
}

// CreateRole creates a new role, or returns the existing one if (tenant_id,
// role_code) already exists. Returns (role, true, nil) on insert, (role, false, nil)
// on idempotent replay, (nil, false, ErrConflict) if the same code exists with
// different name/scope.
func (s *PgStore) CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error) {
	if params.RoleID == "" {
		params.RoleID = uuid.New().String()
	}

	const insertQuery = `
		INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, role_code) DO NOTHING
		RETURNING ` + roleColumns + `;`

	row := s.pool.QueryRow(ctx, insertQuery, params.RoleID, params.TenantID, params.RoleCode, params.RoleName, params.RoleScopeType, params.CreatedByPrincipalID)
	r, err := scanRole(row)
	if err == nil {
		return r, true, nil // fresh insert
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateRole insert failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict path — fetch existing and validate payload matches.
	const lookupQuery = `SELECT ` + roleColumns + ` FROM roles WHERE tenant_id = $1 AND role_code = $2;`
	row = s.pool.QueryRow(ctx, lookupQuery, params.TenantID, params.RoleCode)
	r, err = scanRole(row)
	if err != nil {
		s.log.Error("pg CreateRole lookup failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if r.RoleName != params.RoleName || r.RoleScopeType != params.RoleScopeType {
		return nil, false, domain.ErrConflict
	}
	return r, false, nil // idempotent replay
}

// FindRoleByID fetches a single role by its primary key.
func (s *PgStore) FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error) {
	const query = `SELECT ` + roleColumns + ` FROM roles WHERE role_id = $1;`
	row := s.pool.QueryRow(ctx, query, roleID)
	r, err := scanRole(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRoleNotFound
		}
		s.log.Error("pg FindRoleByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// ListRolesByTenant returns all roles (active and inactive) for a tenant.
// The caller decides whether to filter by active_flag.
func (s *PgStore) ListRolesByTenant(ctx context.Context, tenantID string) ([]domain.Role, error) {
	const query = `SELECT ` + roleColumns + ` FROM roles WHERE tenant_id = $1 ORDER BY role_code;`
	rows, err := s.pool.Query(ctx, query, tenantID)
	if err != nil {
		s.log.Error("pg ListRolesByTenant failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var result []domain.Role
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		result = append(result, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return result, nil
}

// DeactivateRole sets active_flag = FALSE for the given role.
// Per no-soft-delete doctrine, the record is never deleted — it becomes
// permanently inactive.
func (s *PgStore) DeactivateRole(ctx context.Context, roleID string) (*domain.Role, error) {
	const query = `
		UPDATE roles
		SET active_flag = FALSE
		WHERE role_id = $1 AND active_flag = TRUE
		RETURNING ` + roleColumns + `;`

	row := s.pool.QueryRow(ctx, query, roleID)
	r, err := scanRole(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either not found or already inactive — check which.
			existing, lookupErr := s.FindRoleByID(ctx, roleID)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if !existing.ActiveFlag {
				return existing, nil // idempotent: already deactivated
			}
			return nil, domain.ErrRoleNotFound
		}
		s.log.Error("pg DeactivateRole failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// ── permission_bundles ────────────────────────────────────────────────────────

const bundleColumns = `permission_bundle_id, tenant_id, bundle_code, bundle_name, permitted_actions, active_flag, created_at`

func scanBundle(row pgx.Row) (*domain.PermissionBundle, error) {
	b := &domain.PermissionBundle{}
	var rawActions []byte
	err := row.Scan(&b.PermissionBundleID, &b.TenantID, &b.BundleCode, &b.BundleName, &rawActions, &b.ActiveFlag, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(rawActions, &b.PermittedActions)
	return b, nil
}

// CreatePermissionBundle creates or idempotently replays a permission bundle.
// Returns (bundle, true, nil) on insert, (bundle, false, nil) on idempotent
// replay where the payload matches. On mismatch: (nil, false, ErrConflict).
func (s *PgStore) CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, bool, error) {
	if params.PermissionBundleID == "" {
		params.PermissionBundleID = uuid.New().String()
	}
	actionsJSON, err := json.Marshal(params.PermittedActions)
	if err != nil {
		return nil, false, fmt.Errorf("marshal permitted_actions: %w", err)
	}

	const insertQuery = `
		INSERT INTO permission_bundles (permission_bundle_id, tenant_id, bundle_code, bundle_name, permitted_actions)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, bundle_code) DO NOTHING
		RETURNING ` + bundleColumns + `;`

	row := s.pool.QueryRow(ctx, insertQuery, params.PermissionBundleID, params.TenantID, params.BundleCode, params.BundleName, actionsJSON)
	b, err := scanBundle(row)
	if err == nil {
		return b, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreatePermissionBundle insert failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict path — fetch existing.
	const lookupQuery = `SELECT ` + bundleColumns + ` FROM permission_bundles WHERE tenant_id = $1 AND bundle_code = $2;`
	row = s.pool.QueryRow(ctx, lookupQuery, params.TenantID, params.BundleCode)
	b, err = scanBundle(row)
	if err != nil {
		s.log.Error("pg CreatePermissionBundle lookup failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if b.BundleName != params.BundleName {
		return nil, false, domain.ErrConflict
	}
	return b, false, nil
}

// FindBundleByID fetches a single permission bundle by primary key.
func (s *PgStore) FindBundleByID(ctx context.Context, bundleID string) (*domain.PermissionBundle, error) {
	const query = `SELECT ` + bundleColumns + ` FROM permission_bundles WHERE permission_bundle_id = $1;`
	row := s.pool.QueryRow(ctx, query, bundleID)
	b, err := scanBundle(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrBundleNotFound
		}
		s.log.Error("pg FindBundleByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return b, nil
}

// ListBundlesByTenant returns all bundles (active and inactive) for a tenant.
func (s *PgStore) ListBundlesByTenant(ctx context.Context, tenantID string) ([]domain.PermissionBundle, error) {
	const query = `SELECT ` + bundleColumns + ` FROM permission_bundles WHERE tenant_id = $1 ORDER BY bundle_code;`
	rows, err := s.pool.Query(ctx, query, tenantID)
	if err != nil {
		s.log.Error("pg ListBundlesByTenant failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var result []domain.PermissionBundle
	for rows.Next() {
		b, err := scanBundle(rows)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		result = append(result, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return result, nil
}

// UpdatePermissionBundleActions atomically replaces the permitted_actions of
// a bundle. This is the designated path for evolving a bundle's grants —
// the full new action set replaces the old one. History is preserved in
// the event log (role.updated / permission.bundle.updated events).
func (s *PgStore) UpdatePermissionBundleActions(ctx context.Context, params domain.UpdatePermissionBundleActionsParams) (*domain.PermissionBundle, error) {
	actionsJSON, err := json.Marshal(params.PermittedActions)
	if err != nil {
		return nil, fmt.Errorf("marshal permitted_actions: %w", err)
	}

	const query = `
		UPDATE permission_bundles
		SET permitted_actions = $2
		WHERE permission_bundle_id = $1 AND active_flag = TRUE
		RETURNING ` + bundleColumns + `;`

	row := s.pool.QueryRow(ctx, query, params.PermissionBundleID, actionsJSON)
	b, err := scanBundle(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrBundleNotFound
		}
		s.log.Error("pg UpdatePermissionBundleActions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return b, nil
}

// ── role_permission_bundle_links ──────────────────────────────────────────────

const linkColumns = `link_id, role_id, permission_bundle_id, active_flag, created_at, created_by`

func scanLink(row pgx.Row) (*domain.RolePermissionBundleLink, error) {
	l := &domain.RolePermissionBundleLink{}
	var activeFlag bool
	var createdAt time.Time
	err := row.Scan(&l.LinkID, &l.RoleID, &l.PermissionBundleID, &activeFlag, &createdAt, &l.CreatedBy)
	l.CreatedAt = createdAt
	return l, err
}

// CreateRolePermissionBundleLink links a bundle to a role. Idempotent:
// if the link already exists and is active it is returned unchanged.
// If it was previously deactivated it is re-activated.
func (s *PgStore) CreateRolePermissionBundleLink(ctx context.Context, params domain.CreateRolePermissionBundleLinkParams) (*domain.RolePermissionBundleLink, error) {
	// Verify role exists and is active.
	role, err := s.FindRoleByID(ctx, params.RoleID)
	if err != nil {
		return nil, err
	}
	if !role.ActiveFlag {
		return nil, domain.ErrRoleDeactivated
	}
	// Verify bundle exists.
	if _, err := s.FindBundleByID(ctx, params.PermissionBundleID); err != nil {
		return nil, err
	}

	if params.LinkID == "" {
		params.LinkID = uuid.New().String()
	}

	// Upsert: insert or re-activate if previously deactivated.
	const query = `
		INSERT INTO role_permission_bundle_links (link_id, role_id, permission_bundle_id, created_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (role_id, permission_bundle_id) DO UPDATE
		    SET active_flag = TRUE, created_by = EXCLUDED.created_by
		RETURNING ` + linkColumns + `;`

	row := s.pool.QueryRow(ctx, query, params.LinkID, params.RoleID, params.PermissionBundleID, params.CreatedBy)
	l, err := scanLink(row)
	if err != nil {
		s.log.Error("pg CreateRolePermissionBundleLink failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return l, nil
}

// RemoveRolePermissionBundleLink deactivates a role-bundle link (sets
// active_flag = FALSE). The link record is preserved for audit.
func (s *PgStore) RemoveRolePermissionBundleLink(ctx context.Context, params domain.RemoveRolePermissionBundleLinkParams) error {
	const query = `
		UPDATE role_permission_bundle_links
		SET active_flag = FALSE
		WHERE role_id = $1 AND permission_bundle_id = $2 AND active_flag = TRUE;`

	tag, err := s.pool.Exec(ctx, query, params.RoleID, params.PermissionBundleID)
	if err != nil {
		s.log.Error("pg RemoveRolePermissionBundleLink failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrLinkNotFound
	}
	return nil
}

// ListBundlesForRole returns the active permission bundles linked to a role.
func (s *PgStore) ListBundlesForRole(ctx context.Context, roleID string) ([]domain.PermissionBundle, error) {
	const query = `
		SELECT pb.permission_bundle_id, pb.tenant_id, pb.bundle_code, pb.bundle_name,
		       pb.permitted_actions, pb.active_flag, pb.created_at
		FROM permission_bundles pb
		JOIN role_permission_bundle_links l ON l.permission_bundle_id = pb.permission_bundle_id
		WHERE l.role_id = $1 AND l.active_flag = TRUE AND pb.active_flag = TRUE
		ORDER BY pb.bundle_code;`

	rows, err := s.pool.Query(ctx, query, roleID)
	if err != nil {
		s.log.Error("pg ListBundlesForRole failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var result []domain.PermissionBundle
	for rows.Next() {
		b, err := scanBundle(rows)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		result = append(result, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return result, nil
}
