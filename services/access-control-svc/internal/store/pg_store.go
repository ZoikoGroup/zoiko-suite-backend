package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/events"
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

// ── id guards ────────────────────────────────────────────────────────────────

// validRoleID reports whether id can name a row in role_definitions.
//
// role_definition_id and bundle_id are UUID columns, so a query carrying a
// non-UUID string raises 22P02 (invalid input syntax for type uuid) from
// Postgres rather than returning no rows. That error used to travel all the way
// out as 503 store_unavailable — with the raw SQLSTATE text in the response
// body — so GET /v1/role-definitions/not-a-uuid reported a database outage,
// sent whoever was on call to look at a healthy database, and leaked the
// database's own error dialect to any caller who wanted to probe it.
//
// A malformed id cannot name a row. That is what 404 means, and this is the
// cheapest place to say so: ahead of the query, in one expression, instead of
// pattern-matching on a driver error afterwards.
func validID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// uniqueViolation reports whether err is a Postgres 23505 on the named
// constraint or index, so a duplicate can be answered as a conflict rather than
// as an outage.
func uniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == constraint
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

// ── outbox ───────────────────────────────────────────────────────────────────

// enqueue writes one built envelope into the outbox, inside the caller's
// transaction.
//
// It takes the tx, not the pool, and that is the whole point: the event and the
// state change it describes commit or roll back together. A caller that reaches
// for the pool here has reintroduced exactly the defect the outbox exists to
// close.
func enqueue(ctx context.Context, tx pgx.Tx, tenantID string, out events.Outbound) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO event_outbox (tenant_id, event_type, aggregate_key, payload)
		VALUES ($1, $2, $3, $4)
	`, tenantID, out.EventType, out.Key, out.Body)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", out.EventType, err)
	}
	return nil
}

// enqueueBundleChange writes the pair of events a bundle change produces.
//
// TWO events, not one, and the second is the one that matters. A bundle change
// IS a change to what its role grants, but identity-context-svc — the consumer
// that ends the sessions carrying the old grant — dispatches on role.updated
// and has no case for permission.bundle.updated at all. Emitting only the
// bundle event would leave every live session still asserting the actions the
// bundle used to permit, no matter how the payload were shaped.
//
// authorization-svc's lifecycle consumer handles both names and invalidates on
// either, so the pair costs it one redundant cache invalidation per bundle
// write — a cache miss, against a session that would otherwise keep a
// withdrawn grant.
//
// permission.bundle.updated is still emitted because it is the event that says
// WHAT changed, and consumers built against it should not have to infer a
// bundle edit from a role event.
func enqueueBundleChange(ctx context.Context, tx pgx.Tx, b domain.PermissionBundleDef, role *domain.RoleDefinition, actorID string) error {
	bundleEvent, err := events.BundleUpdated(b, actorID)
	if err != nil {
		return err
	}
	if err := enqueue(ctx, tx, b.TenantID, bundleEvent); err != nil {
		return err
	}
	if role == nil {
		return nil
	}
	// The role event carries the bundle's correlation id, not the role's own:
	// this role.updated was caused by this bundle write, and joining it to the
	// request that caused it is the point of a correlation id.
	forRole := *role
	forRole.CorrelationID = b.CorrelationID
	roleEvent, err := events.RoleUpdated(forRole, actorID)
	if err != nil {
		return err
	}
	return enqueue(ctx, tx, b.TenantID, roleEvent)
}

// readRole reads one role inside an open transaction.
func readRole(ctx context.Context, tx pgx.Tx, tenantID, roleDefinitionID string) (*domain.RoleDefinition, error) {
	var r domain.RoleDefinition
	row := tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND role_definition_id = $2", tenantID, roleDefinitionID)
	if err := scanRole(row, &r); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRoleNotFound
		}
		return nil, err
	}
	return &r, nil
}

// ── roles ────────────────────────────────────────────────────────────────────

// CreateRole inserts a new role definition and enqueues role.created in the
// same transaction. Idempotent on (tenant_id, correlation_id).
//
// A role_code another definition in this tenant already holds is answered with
// domain.ErrRoleCodeExists rather than the bare UNIQUE violation, which used to
// reach the caller as 503 store_unavailable.
func (s *PgStore) CreateRole(ctx context.Context, r *domain.RoleDefinition, actorID string) (created bool, err error) {
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
			// ON CONFLICT covers the idempotency index only. A collision on
			// (tenant_id, role_code) is a different thing entirely — a second
			// definition of a role that already exists — and is the caller's
			// to resolve, not on-call's.
			if uniqueViolation(err, "role_definitions_tenant_id_role_code_key") {
				return domain.ErrRoleCodeExists
			}
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			out, err := events.RoleCreated(*r, actorID)
			if err != nil {
				return err
			}
			return enqueue(ctx, tx, tenantID, out)
		}
		// A replay. The original row is returned and NO event is enqueued:
		// nothing changed, and a consumer that saw role.created twice for one
		// role would be acting on a creation that did not happen.
		row := tx.QueryRow(ctx, "SELECT "+roleColumns+" FROM role_definitions WHERE tenant_id = $1 AND correlation_id = $2", tenantID, r.CorrelationID)
		return scanRole(row, r)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// RoleCodeTaken reports whether this tenant already defines roleCode under
// SOME OTHER correlation id.
//
// Checked BEFORE the role is provisioned into authorization-svc, not only at
// insert time. The insert is the backstop and holds under a race; this is what
// stops a duplicate create from leaving a provisioned role in authorization-svc
// that this register then refuses to record — an orphan grant nobody here can
// see, retire or explain.
//
// The correlation-id exclusion is not a nicety. A retried create sends the same
// code under the same key, and is meant to resolve to the original definition;
// a plain existence check turns every retry into a 409 and destroys the
// idempotency the key exists to provide.
func (s *PgStore) RoleCodeTaken(ctx context.Context, roleCode, exceptCorrelationID string) (bool, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	var exists bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM role_definitions
				WHERE tenant_id = $1 AND role_code = $2 AND correlation_id <> $3
			)`, tenantID, roleCode, exceptCorrelationID).Scan(&exists)
	})
	return exists, err
}

func (s *PgStore) GetRole(ctx context.Context, roleDefinitionID string) (*domain.RoleDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if !validID(roleDefinitionID) {
		return nil, domain.ErrRoleNotFound
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

// UpdateRole applies a rename and/or a status change and enqueues role.updated
// in the same transaction.
func (s *PgStore) UpdateRole(ctx context.Context, roleDefinitionID, roleName, status, updatedByPrincipalID string) (*domain.RoleDefinition, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	if !validID(roleDefinitionID) {
		return nil, domain.ErrRoleNotFound
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
		if err := scanRole(row, &out); err != nil {
			return err
		}
		// The correlation id of the REQUEST, not the one the row has carried
		// since creation. A role.updated stamped with the create's correlation
		// id cannot be joined to the change it describes.
		forEvent := out
		if cid := svcmiddleware.CorrelationFromContext(ctx); cid != "" {
			forEvent.CorrelationID = cid
		}
		ev, err := events.RoleUpdated(forEvent, updatedByPrincipalID)
		if err != nil {
			return err
		}
		return enqueue(ctx, tx, tenantID, ev)
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

// CreateBundle inserts a new permission bundle and enqueues its events in the
// same transaction. Idempotent on (tenant_id, correlation_id).
//
// A bundle_code the role already carries is answered with
// domain.ErrBundleCodeExists — see that error for why two bundles sharing a
// code on one role is not a cosmetic problem.
func (s *PgStore) CreateBundle(ctx context.Context, b *domain.PermissionBundleDef, actorID string) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	if !validID(b.RoleDefinitionID) {
		return false, domain.ErrRoleNotFound
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
			if uniqueViolation(err, "idx_permission_bundle_defs_role_code") {
				return domain.ErrBundleCodeExists
			}
			return err
		}
		if tag.RowsAffected() != 1 {
			// A replay. No event: nothing changed.
			row := tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND correlation_id = $2", tenantID, b.CorrelationID)
			return scanBundle(row, b)
		}
		created = true
		role, err := readRole(ctx, tx, tenantID, b.RoleDefinitionID)
		if err != nil {
			return err
		}
		return enqueueBundleChange(ctx, tx, *b, role, actorID)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// BundleCodeTaken reports whether roleDefinitionID already carries bundleCode
// under some other correlation id.
//
// Checked before provisioning into authorization-svc, for the same reason as
// RoleCodeTaken, and with the same replay exclusion: attaching there is an
// upsert-REPLACE on (role_id, bundle_code), so a create this register is about
// to refuse would still have overwritten the permitted actions of the bundle
// that already holds the code.
func (s *PgStore) BundleCodeTaken(ctx context.Context, roleDefinitionID, bundleCode, exceptCorrelationID string) (bool, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}
	if !validID(roleDefinitionID) {
		return false, domain.ErrRoleNotFound
	}
	var exists bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM permission_bundle_defs
				WHERE tenant_id = $1 AND role_definition_id = $2 AND bundle_code = $3
				  AND correlation_id <> $4
			)`, tenantID, roleDefinitionID, bundleCode, exceptCorrelationID).Scan(&exists)
	})
	return exists, err
}

func (s *PgStore) ListBundles(ctx context.Context, roleDefinitionID string) ([]domain.PermissionBundleDef, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}
	// An empty slice, not a refusal: this is a collection read, and a malformed
	// role id names no bundles the same way an unknown one does.
	if !validID(roleDefinitionID) {
		return []domain.PermissionBundleDef{}, nil
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
	if !validID(roleDefinitionID) || !validID(bundleID) {
		return nil, domain.ErrBundleNotFound
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

// UpdateBundle applies an edit to one bundle, stamps the verified caller, and
// enqueues its events in the same transaction.
//
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
	if !validID(roleDefinitionID) || !validID(bundleID) {
		return nil, domain.ErrBundleNotFound
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
		sets = append(sets, "updated_at = now()")
		query := fmt.Sprintf("UPDATE permission_bundle_defs SET %s WHERE tenant_id = $%d AND role_definition_id = $%d AND bundle_id = $%d",
			strings.Join(sets, ", "), len(args)+1, len(args)+2, len(args)+3)
		if _, err := tx.Exec(ctx, query, append(args, tenantID, roleDefinitionID, bundleID)...); err != nil {
			return err
		}

		// Re-read under BOTH ids, matching the write above. Reading back by
		// bundle_id alone was a scope the update never used.
		row = tx.QueryRow(ctx, "SELECT "+bundleColumns+" FROM permission_bundle_defs WHERE tenant_id = $1 AND role_definition_id = $2 AND bundle_id = $3", tenantID, roleDefinitionID, bundleID)
		if err := scanBundle(row, &out); err != nil {
			return err
		}

		forEvent := out
		if cid := svcmiddleware.CorrelationFromContext(ctx); cid != "" {
			forEvent.CorrelationID = cid
		}
		role, err := readRole(ctx, tx, tenantID, roleDefinitionID)
		if err != nil {
			return err
		}
		return enqueueBundleChange(ctx, tx, forEvent, role, updatedByPrincipalID)
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
	// A malformed role_id filter narrows to nothing rather than raising 22P02
	// from inside the query and surfacing as a store outage.
	if filter.RoleID != "" && !validID(filter.RoleID) {
		return []domain.PermissionBundleDef{}, nil
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

// ── outbox relay surface ─────────────────────────────────────────────────────

// OutboxRecord is one claimed, unpublished event.
type OutboxRecord struct {
	OutboxID  int64
	EventType string
	Key       string
	Body      []byte
}

// withRelay runs fn with app.outbox_relay installed instead of a tenant.
//
// The relay is the one code path in this service that legitimately crosses
// tenants: it drains every tenant's backlog from a single loop. Rather than
// letting it run unscoped — which under FORCE ROW LEVEL SECURITY would simply
// see nothing, and would present as a relay that publishes nothing while
// reporting no error at all — it names itself, so migration 000004's policy
// admits it by an explicit, auditable disjunct rather than by the absence of a
// control.
func (s *PgStore) withRelay(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin relay transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.outbox_relay', 'true', true)"); err != nil {
		return fmt.Errorf("set relay context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit relay transaction: %w", err)
	}
	return nil
}

// ClaimOutbox takes up to limit unpublished events, oldest first, locking them
// for the duration of the caller's drain.
//
// FOR UPDATE SKIP LOCKED is what makes more than one replica safe: a second
// relay claims the next batch instead of blocking on, or duplicating, this one.
func (s *PgStore) ClaimOutbox(ctx context.Context, limit int, fn func([]OutboxRecord) error) error {
	return s.withRelay(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT outbox_id, event_type, aggregate_key, payload
			FROM event_outbox
			WHERE published_at IS NULL
			ORDER BY created_at, outbox_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		`, limit)
		if err != nil {
			return fmt.Errorf("claim outbox: %w", err)
		}
		var claimed []OutboxRecord
		for rows.Next() {
			var r OutboxRecord
			if err := rows.Scan(&r.OutboxID, &r.EventType, &r.Key, &r.Body); err != nil {
				rows.Close()
				return fmt.Errorf("scan outbox row: %w", err)
			}
			claimed = append(claimed, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read outbox rows: %w", err)
		}
		if len(claimed) == 0 {
			return nil
		}

		// fn publishes. It runs while the rows are still locked and BEFORE the
		// marking commits, so a crash mid-publish rolls the marking back and
		// the events are re-delivered rather than lost. At-least-once, chosen
		// deliberately: a duplicate role.updated revokes sessions that are
		// already being revoked, a lost one leaves them holding a grant the
		// register says was withdrawn.
		if err := fn(claimed); err != nil {
			ids := make([]int64, 0, len(claimed))
			for _, r := range claimed {
				ids = append(ids, r.OutboxID)
			}
			// Recorded on the rows themselves, so a stuck event can be
			// diagnosed from the table without correlating against logs.
			if _, uerr := tx.Exec(ctx, `
				UPDATE event_outbox
				SET attempts = attempts + 1, last_error = $2
				WHERE outbox_id = ANY($1)
			`, ids, err.Error()); uerr != nil {
				return fmt.Errorf("record publish failure: %w (original: %v)", uerr, err)
			}
			// Committed: the attempt count and the error are worth keeping even
			// though the publish failed. published_at is untouched, so the rows
			// are claimed again on the next tick.
			if cerr := tx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit publish failure: %w (original: %v)", cerr, err)
			}
			return err
		}

		ids := make([]int64, 0, len(claimed))
		for _, r := range claimed {
			ids = append(ids, r.OutboxID)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE event_outbox SET published_at = now() WHERE outbox_id = ANY($1)
		`, ids); err != nil {
			return fmt.Errorf("mark published: %w", err)
		}
		return nil
	})
}

// OutboxDepth reports the unpublished backlog and the age of its oldest entry.
//
// Both, not just the depth. A backlog of ten that is three seconds old is a
// service under load; a backlog of ten that is an hour old is a relay that has
// stopped, and on this service that means a role retirement has not reached the
// consumer that ends the sessions still carrying its grants. Depth alone cannot
// tell them apart, which is why the alert rule uses the age.
func (s *PgStore) OutboxDepth(ctx context.Context) (pending int64, oldestAge time.Duration, err error) {
	err = s.withRelay(ctx, func(tx pgx.Tx) error {
		var oldest *time.Time
		row := tx.QueryRow(ctx, `
			SELECT count(*), min(created_at) FROM event_outbox WHERE published_at IS NULL
		`)
		if err := row.Scan(&pending, &oldest); err != nil {
			return fmt.Errorf("outbox depth: %w", err)
		}
		if oldest != nil {
			oldestAge = time.Since(*oldest)
		}
		return nil
	})
	return pending, oldestAge, err
}
