// Package store provides the PostgreSQL implementation of the authorization
// read and write model, including the core evaluation queries (RBAC grant
// lookup, delegation resolution, SoD conflict check).
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers or domain packages.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// Store is the full read/write surface PgStore implements.
//
// Documentation, not a dependency: the handler declares its own narrower
// handler.AuthorizationStore and nothing references this one. It is kept
// because it is the one place the whole surface is listed with its contracts
// — but a method added to PgStore and not to this list makes the list a lie,
// which is the failure mode the comment it used to carry ("the interface
// consumed by the handler") already had.
type Store interface {
	CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error)
	FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error)
	SetRoleActive(ctx context.Context, roleID, tenantID string, active bool) (*domain.Role, error)
	CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error)

	CreateRoleAssignment(ctx context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error)
	RevokeRoleAssignment(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error)

	// ListRoleAssignments returns the assignments in tenantID scope, newest
	// first. principalID and roleID are optional filters — empty means "any".
	// activeOnly drops assignments that have already been revoked or have not
	// come into effect yet, which is what an operator deciding whether to
	// revoke needs to see; false returns the history too.
	//
	// Exists because there was no way to READ an assignment. Every write path
	// existed, so the console could create a grant and then never show it,
	// and revoking one required knowing an id that nothing surfaced.
	ListRoleAssignments(ctx context.Context, tenantID, principalID, roleID string, activeOnly bool) ([]domain.PrincipalRoleAssignment, error)

	CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error)
	FindDelegatedAuthorityByID(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error)
	RevokeDelegatedAuthority(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error)

	// ProjectDelegation and RevokeProjectedDelegation maintain the projection
	// of delegated-authority-svc's authority.* events — that service is the
	// authoritative owner of the concept (Doc 03 §9.3), and these are how this
	// table becomes the evaluation read-model rather than a rival write model.
	// Only internal/events.Consumer calls them; the admin API never does.
	ProjectDelegation(ctx context.Context, params domain.ProjectDelegationParams) (*domain.DelegatedAuthority, error)
	RevokeProjectedDelegation(ctx context.Context, sourceService, sourceDelegationID, tenantID string) (*domain.DelegatedAuthority, error)

	CreateSoDRule(ctx context.Context, params domain.CreateSoDRuleParams) (*domain.SoDRule, error)

	// ListSoDRules returns the SoD rules that apply in tenantID scope: the
	// tenant's own rules plus the globally-applicable ones (tenant_id NULL),
	// which is exactly the set CheckSoDConflict evaluates against. Listing
	// only the tenant's own would show an operator a shorter rule set than
	// the one actually denying their requests.
	ListSoDRules(ctx context.Context, tenantID string) ([]domain.SoDRule, error)

	// FindGrantedActions returns the union of permitted_actions from every
	// currently-active role assignment + bundle for principalID in
	// legalEntityID, plus a human-readable basis string naming the
	// role(s) that granted them (empty slice + "" basis if none).
	//
	// tenantID scopes the role join. Empty means "no verified tenant" and
	// falls back to platform scope — see the implementation for why that
	// fallback still exists and what it costs.
	FindGrantedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error)

	// FindDelegatedActions returns the union of actions available to
	// principalID in legalEntityID via active, non-expired delegations —
	// i.e. actions the delegator(s) hold, that principalID may act on
	// their behalf for. tenantID scopes it exactly as above.
	FindDelegatedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error)

	// CheckSoDConflict returns the conflicting action name and true if
	// grantedActions already contains an action that conflicts with
	// candidateAction per an active sod_rules row. Only globally-applicable
	// rules (tenant_id IS NULL) are considered when tenantID is "" — pass
	// the caller's tenant to also bring that tenant's own SoD rules in.
	CheckSoDConflict(ctx context.Context, grantedActions []string, candidateAction, tenantID string) (string, bool, error)

	// CheckOwnObjectSoD reports whether a data-declared rule forbids the same
	// principal both owning and performing actionType — the dynamic SoD layer.
	CheckOwnObjectSoD(ctx context.Context, actionType, tenantID string) (bool, error)

	// The ABAC surface. CreateABACRule validates effect and operator against
	// what internal/abac can actually execute and refuses anything else, so a
	// condition nobody can evaluate cannot be stored — such a rule denies its
	// action for every principal holding it.
	CreateABACRule(ctx context.Context, params domain.CreateABACRuleParams) (*domain.ABACRule, error)
	SetABACRuleActive(ctx context.Context, abacRuleID, tenantID string, active bool) (*domain.ABACRule, error)
	ListABACRules(ctx context.Context, tenantID, actionType string) ([]domain.ABACRule, error)

	// FindABACRules returns the ACTIVE conditions guarding actionType in
	// tenantID scope — the tenant's own plus the platform-wide ones. This is
	// the evaluation read, and it returns an empty slice until somebody
	// declares a rule, which is what makes layer 5 a no-op by default.
	FindABACRules(ctx context.Context, actionType, tenantID string) ([]domain.ABACRule, error)

	RecordAccessDecision(ctx context.Context, params domain.RecordAccessDecisionParams) (*domain.AccessDecisionLog, error)

	// FindAccessDecisionByID returns the decision only if it was recorded in
	// tenantID scope. A decision recorded without a tenant is not readable
	// here at all - see RecordAccessDecisionParams.TenantID.
	FindAccessDecisionByID(ctx context.Context, accessDecisionID, tenantID string) (*domain.AccessDecisionLog, error)
}

// PgStore implements Store against a PostgreSQL cluster via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New returns an open PgStore. Caller must call pool.Close() when done.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withRLS runs fn inside a transaction with app.tenant_id set for the
// session, so roles/sod_rules' tenant_isolation_policy has a value to
// enforce against. Mirrors tenant-entity-registry-svc's PgStore.withRLS.
// An empty tenantID is valid — it means "no specific tenant" and only
// ever matches a sod_rules row whose own tenant_id is NULL (a globally-
// applicable rule), never a real tenant's row.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// withPlatformScope runs fn inside a transaction flagged as platform
// scope, which the roles policy treats as visible regardless of
// tenant_id.
//
// Two callers, for different reasons:
//
// FindRoleByID, whose entire purpose is to discover which tenant an unknown
// role_id belongs to — the caller cannot know the tenant to scope the read by
// until after this read returns, so this read structurally cannot be
// tenant-scoped without being pointless (same reasoning as
// secret-vault-integration-svc's FindSecretPolicyVersionByID). Safety comes
// from the caller (handler.go) always comparing the returned role's TenantID
// against the verified caller's own scope and refusing on mismatch — this
// function's result is never trusted as pre-authorized.
//
// FindGrantedActions, but ONLY when its caller supplied no tenant at all. That
// is a compatibility fallback for callers still on the pre-header convention,
// not a property of the query — see its own comment for what the unconditional
// form leaked. Anything else reaching for this should scope by tenant instead.
func (s *PgStore) withPlatformScope(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return fmt.Errorf("set_config app.platform_scope: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── roles ────────────────────────────────────────────────────────────────────

const roleColumns = `role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_at, created_by_principal_id`

func scanRole(row pgx.Row) (*domain.Role, error) {
	r := &domain.Role{}
	err := row.Scan(&r.RoleID, &r.TenantID, &r.RoleCode, &r.RoleName, &r.RoleScopeType, &r.ActiveFlag, &r.CreatedAt, &r.CreatedByPrincipalID)
	return r, err
}

// FindRoleByID looks up a role by its UUID primary key, regardless of
// tenant — see withPlatformScope's doc comment for why this specific
// read has to run unscoped, and why that's safe.
func (s *PgStore) FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error) {
	const query = `SELECT ` + roleColumns + ` FROM roles WHERE role_id = $1;`
	var r *domain.Role
	err := s.withPlatformScope(ctx, func(tx pgx.Tx) error {
		var scanErr error
		r, scanErr = scanRole(tx.QueryRow(ctx, query, roleID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRoleNotFound
		}
		s.log.Error("pg FindRoleByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// SetRoleActive flips a role's active_flag and returns the role as it now
// stands. This is the ONLY way to stop a role being enforced.
//
// WHY THIS EXISTS. active_flag has been on the roles table since the initial
// schema, and FindGrantedActions has always joined through it
// (`JOIN roles r ON ... AND r.active_flag`), so a false flag genuinely removes
// every action the role granted. What was missing was any route that could set
// it: this service could create a role and revoke an ASSIGNMENT, but never
// retire the role itself. access-control-svc, the governed authoring layer in
// front of this admin API, could therefore mark a role RETIRED in its own
// catalogue while every principal holding it kept the access — a retirement
// that was a label and not a control.
//
// Idempotent: retiring an already-retired role returns it unchanged with no
// error, because the caller's intent (this role must not grant anything) is
// already satisfied and a 409 would make a safe retry look like a failure.
// A missing role is still ErrRoleNotFound — that is a different fact.
//
// Deliberately does not touch principal_role_assignments. The assignments
// remain, and reactivating restores exactly the access that was suspended;
// cascading revocation would be irreversible and is a separate decision from
// "stop enforcing this role for now".
//
// TENANT SCOPE. tenantID is the caller's VERIFIED scope, and both controls
// that isolate it are applied: the explicit tenant_id predicate, and withRLS
// so the tenant_isolation_policy on `roles` has a value to enforce against.
// This UPDATE previously ran on s.pool with neither. Against a NOBYPASSRLS
// role - which is what zoiko_app is - app.tenant_id was never set, the FORCE
// policy predicate evaluated to NULL, and the statement matched zero rows, so
// every retire/reactivate answered 404 and role retirement did not work at
// all. Against an owner or superuser connection the same statement retired
// ANY tenant's role by id. One missing scope, two opposite failures.
func (s *PgStore) SetRoleActive(ctx context.Context, roleID, tenantID string, active bool) (*domain.Role, error) {
	const query = `
		UPDATE roles SET active_flag = $3
		WHERE role_id = $1 AND tenant_id = $2::uuid
		RETURNING ` + roleColumns + `;`

	var r *domain.Role
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		r, scanErr = scanRole(tx.QueryRow(ctx, query, roleID, tenantID, active))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRoleNotFound
		}
		s.log.Error("pg SetRoleActive failed", zap.Error(err), zap.String("role_id", roleID), zap.Bool("active", active))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error) {
	if params.RoleID == "" {
		params.RoleID = uuid.New().String()
	}

	const query = `
		INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, role_code) DO NOTHING
		RETURNING ` + roleColumns + `;`
	const lookupQuery = `SELECT ` + roleColumns + ` FROM roles WHERE tenant_id = $1 AND role_code = $2;`

	var result *domain.Role
	var created bool
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		r, err := scanRole(tx.QueryRow(ctx, query, params.RoleID, params.TenantID, params.RoleCode, params.RoleName, params.RoleScopeType, params.CreatedByPrincipalID))
		if err == nil {
			result, created = r, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		existing, err := scanRole(tx.QueryRow(ctx, lookupQuery, params.TenantID, params.RoleCode))
		if err != nil {
			return err
		}
		if existing.RoleName != params.RoleName || existing.RoleScopeType != params.RoleScopeType {
			return domain.ErrConflict
		}
		result, created = existing, false
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil, false, domain.ErrConflict
		}
		s.log.Error("pg CreateRole failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return result, created, nil
}

// ── permission_bundles ───────────────────────────────────────────────────────

func (s *PgStore) CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error) {
	// The role is fetched for existence, and its tenant is what scopes the
	// INSERT below. permission_bundles carries no tenant_id of its own, so
	// the owning tenant is only knowable through this FK — which is also
	// exactly how 000007's policy reads it.
	role, err := s.FindRoleByID(ctx, params.RoleID)
	if err != nil {
		return nil, err
	}
	if params.PermissionBundleID == "" {
		params.PermissionBundleID = uuid.New().String()
	}
	actionsJSON, marshalErr := json.Marshal(params.PermittedActions)
	if marshalErr != nil {
		return nil, fmt.Errorf("marshal permitted_actions: %w", marshalErr)
	}

	const query = `
		INSERT INTO permission_bundles (permission_bundle_id, role_id, bundle_code, permitted_actions)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions
		RETURNING permission_bundle_id, role_id, bundle_code, permitted_actions, active_flag, created_at;`

	// withRLS, not s.pool directly. This query went straight to the pool
	// until 000007 gave permission_bundles a policy, at which point every
	// INSERT began failing with SQLSTATE 42501 — app.tenant_id was never
	// set, so the policy's WITH CHECK could not match and refused the row.
	// The same shape obligations-svc had (nine pool-direct queries) and the
	// reason it only joined create-app-roles.sh after being fixed.
	b := &domain.PermissionBundle{}
	var rawActions []byte
	err = s.withRLS(ctx, role.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, params.PermissionBundleID, params.RoleID, params.BundleCode, actionsJSON).
			Scan(&b.PermissionBundleID, &b.RoleID, &b.BundleCode, &rawActions, &b.ActiveFlag, &b.CreatedAt)
	})
	if err != nil {
		s.log.Error("pg CreatePermissionBundle failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	_ = json.Unmarshal(rawActions, &b.PermittedActions)
	return b, nil
}

// ── principal_role_assignments ───────────────────────────────────────────────

const assignmentColumns = `principal_role_assignment_id, principal_id, role_id, legal_entity_id, effective_from, effective_to, assigned_by, created_at`

func scanAssignment(row pgx.Row) (*domain.PrincipalRoleAssignment, error) {
	a := &domain.PrincipalRoleAssignment{}
	err := row.Scan(&a.PrincipalRoleAssignmentID, &a.PrincipalID, &a.RoleID, &a.LegalEntityID, &a.EffectiveFrom, &a.EffectiveTo, &a.AssignedBy, &a.CreatedAt)
	return a, err
}

func (s *PgStore) CreateRoleAssignment(ctx context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error) {
	role, err := s.FindRoleByID(ctx, params.RoleID)
	if err != nil {
		return nil, err
	}
	// A nil LegalEntityID means "grant across the whole tenant" — only
	// coherent for a role whose own scope is TENANT. Granting an
	// entity-scoped role with no entity would silently make it apply
	// everywhere, defeating the point of role_scope_type.
	if params.LegalEntityID == nil && role.RoleScopeType != "TENANT" {
		return nil, domain.ErrLegalEntityRequiredForRoleScope
	}
	if params.PrincipalRoleAssignmentID == "" {
		params.PrincipalRoleAssignmentID = uuid.New().String()
	}

	const query = `
		INSERT INTO principal_role_assignments (principal_role_assignment_id, principal_id, role_id, legal_entity_id, effective_from, assigned_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING ` + assignmentColumns + `;`

	// Scoped to the role's tenant, for the reason CreatePermissionBundle
	// above is: principal_role_assignments has no tenant_id column, so
	// 000007's policy resolves the tenant through this same role FK, and the
	// INSERT needs app.tenant_id installed to satisfy its WITH CHECK.
	var a *domain.PrincipalRoleAssignment
	err = s.withRLS(ctx, role.TenantID, func(tx pgx.Tx) error {
		var scanErr error
		a, scanErr = scanAssignment(tx.QueryRow(ctx, query,
			params.PrincipalRoleAssignmentID, params.PrincipalID, params.RoleID,
			params.LegalEntityID, params.EffectiveFrom, params.AssignedBy))
		return scanErr
	})
	if err != nil {
		s.log.Error("pg CreateRoleAssignment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// RevokeRoleAssignment ends an assignment, scoped to tenantID through the
// assignment's role — principal_role_assignments has no tenant_id column
// of its own, only legal_entity_id, so the join through roles is how
// "does this assignment belong to the caller's tenant" is answered at
// all. A cross-tenant attempt reports ErrRoleAssignmentNotFound, same as
// a genuinely missing one — it must not confirm another tenant's
// assignment exists.
func (s *PgStore) RevokeRoleAssignment(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error) {
	const query = `
		UPDATE principal_role_assignments
		SET effective_to = NOW()
		WHERE principal_role_assignment_id = $1
		  AND (effective_to IS NULL OR effective_to > NOW())
		  AND role_id IN (SELECT role_id FROM roles WHERE tenant_id = $2)
		RETURNING ` + assignmentColumns + `;`

	// The subquery reads `roles`, which carries RLS — without app.tenant_id
	// set, the subquery would see zero rows regardless of the WHERE
	// tenant_id = $2 predicate, since RLS filters before that clause ever
	// runs.
	var a *domain.PrincipalRoleAssignment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		a, scanErr = scanAssignment(tx.QueryRow(ctx, query, assignmentID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRoleAssignmentNotFound
		}
		s.log.Error("pg RevokeRoleAssignment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// ListRoleAssignments implements Store.
//
// Scoped the same way RevokeRoleAssignment is — through the role's tenant,
// since principal_role_assignments carries no tenant_id of its own. As of
// migration 000007 the table also has its own RLS policy reading the same
// FK, so this is now defended twice: drop the predicate below and the policy
// still returns nothing rather than everything.
func (s *PgStore) ListRoleAssignments(ctx context.Context, tenantID, principalID, roleID string, activeOnly bool) ([]domain.PrincipalRoleAssignment, error) {
	// $2/$3 are optional filters: the NULLIF-style '' check keeps this one
	// query rather than four, and pgx sends the empty string as a real
	// parameter so there is no SQL assembled from input anywhere here.
	query := `
		SELECT ` + assignmentColumns + `
		  FROM principal_role_assignments
		 WHERE role_id IN (SELECT role_id FROM roles WHERE tenant_id = $1)
		   AND ($2 = '' OR principal_id = $2)
		   AND ($3 = '' OR role_id::text = $3)`
	if activeOnly {
		query += `
		   AND effective_from <= NOW()
		   AND (effective_to IS NULL OR effective_to > NOW())`
	}
	query += `
		 ORDER BY created_at DESC
		 LIMIT 500;`

	var out []domain.PrincipalRoleAssignment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, query, tenantID, principalID, roleID)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.PrincipalRoleAssignment
			if scanErr := rows.Scan(&a.PrincipalRoleAssignmentID, &a.PrincipalID, &a.RoleID,
				&a.LegalEntityID, &a.EffectiveFrom, &a.EffectiveTo, &a.AssignedBy, &a.CreatedAt); scanErr != nil {
				return scanErr
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListRoleAssignments failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── delegated_authorities ────────────────────────────────────────────────────

const delegationColumns = `delegated_authority_id, tenant_id, delegator_principal_id, delegate_principal_id, scope_type, legal_entity_id, authority_limit_type, authority_limit_value, delegated_actions, source_service, source_delegation_id, effective_from, effective_to, revocation_status, created_at`

func scanDelegation(row pgx.Row) (*domain.DelegatedAuthority, error) {
	d := &domain.DelegatedAuthority{}
	// delegated_actions is JSONB and nullable, so it is scanned as raw bytes
	// rather than into []string directly: pgx maps SQL NULL to a nil slice
	// only for array types, and this column is jsonb — a nil destination
	// would be an error instead of "full authority".
	var rawActions []byte
	err := row.Scan(&d.DelegatedAuthorityID, &d.TenantID, &d.DelegatorPrincipalID, &d.DelegatePrincipalID, &d.ScopeType, &d.LegalEntityID,
		&d.AuthorityLimitType, &d.AuthorityLimitValue, &rawActions, &d.SourceService, &d.SourceDelegationID,
		&d.EffectiveFrom, &d.EffectiveTo, &d.RevocationStatus, &d.CreatedAt)
	if err == nil && len(rawActions) > 0 {
		_ = json.Unmarshal(rawActions, &d.DelegatedActions)
	}
	return d, err
}

// marshalActionSubset renders a delegated-actions subset for the JSONB column.
// A nil or empty slice becomes SQL NULL, which is "the delegator's full
// authority" — the same meaning every row written before 000008 carries.
//
// Empty is folded into NULL deliberately: a JSON `[]` would be a delegation
// that confers precisely nothing, which is not a thing anyone means to create,
// and it would be indistinguishable in the register from one that confers
// everything.
func marshalActionSubset(actions []string) any {
	if len(actions) == 0 {
		return nil
	}
	raw, err := json.Marshal(actions)
	if err != nil {
		return nil
	}
	return raw
}

func (s *PgStore) CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error) {
	if params.DelegatedAuthorityID == "" {
		params.DelegatedAuthorityID = uuid.New().String()
	}

	if params.TenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}

	const query = `
		INSERT INTO delegated_authorities (delegated_authority_id, tenant_id, delegator_principal_id, delegate_principal_id, scope_type, legal_entity_id, authority_limit_type, authority_limit_value, delegated_actions, source_service, source_delegation_id, effective_from, effective_to)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), NULLIF($11, ''), $12, $13)
		RETURNING ` + delegationColumns + `;`

	var d *domain.DelegatedAuthority
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanDelegation(tx.QueryRow(ctx, query, params.DelegatedAuthorityID, params.TenantID, params.DelegatorPrincipalID, params.DelegatePrincipalID,
			params.ScopeType, params.LegalEntityID, params.AuthorityLimitType, params.AuthorityLimitValue,
			marshalActionSubset(params.DelegatedActions), params.SourceService, params.SourceDelegationID,
			params.EffectiveFrom, params.EffectiveTo))
		return scanErr
	})
	if err != nil {
		s.log.Error("pg CreateDelegatedAuthority failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

// ProjectDelegation upserts the delegation described by an upstream
// authority.delegated event, keyed on (source_service, source_delegation_id).
//
// UPSERT, not INSERT. Kafka redelivers, consumers restart, and an INSERT per
// delivery would turn one upstream delegation into several rows that
// FindDelegatedActions would then union — a duplicate grant, arrived at by
// retry. The unique partial index from 000008 is what ON CONFLICT targets.
//
// A projected row that arrives again with a LATER effective_to (upstream
// extended the delegation) is updated; the update also resets
// revocation_status to ACTIVE, because upstream re-emitting a delegated event
// for an id it had revoked means it has re-granted it, and upstream is the
// authority on that.
func (s *PgStore) ProjectDelegation(ctx context.Context, params domain.ProjectDelegationParams) (*domain.DelegatedAuthority, error) {
	if params.TenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}
	if params.SourceService == "" || params.SourceDelegationID == "" {
		return nil, domain.ErrProjectionSourceRequired
	}

	// scope_type is ACTION_SUBSET whenever the event named actions, which it
	// always does — upstream delegates one action_type per grant. FULL is the
	// honest label only for a delegation that confers everything.
	scopeType := "FULL"
	if len(params.DelegatedActions) > 0 {
		scopeType = "ACTION_SUBSET"
	}

	const query = `
		INSERT INTO delegated_authorities (
			tenant_id, delegator_principal_id, delegate_principal_id, scope_type,
			legal_entity_id, delegated_actions, source_service, source_delegation_id,
			effective_from, effective_to)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (source_service, source_delegation_id)
			WHERE source_delegation_id IS NOT NULL
		DO UPDATE SET
			delegator_principal_id = EXCLUDED.delegator_principal_id,
			delegate_principal_id  = EXCLUDED.delegate_principal_id,
			scope_type             = EXCLUDED.scope_type,
			legal_entity_id        = EXCLUDED.legal_entity_id,
			delegated_actions      = EXCLUDED.delegated_actions,
			effective_from         = EXCLUDED.effective_from,
			effective_to           = EXCLUDED.effective_to,
			revocation_status      = 'ACTIVE'
		RETURNING ` + delegationColumns + `;`

	var d *domain.DelegatedAuthority
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanDelegation(tx.QueryRow(ctx, query,
			params.TenantID, params.DelegatorPrincipalID, params.DelegatePrincipalID, scopeType,
			params.LegalEntityID, marshalActionSubset(params.DelegatedActions),
			params.SourceService, params.SourceDelegationID,
			params.EffectiveFrom, params.EffectiveTo))
		return scanErr
	})
	if err != nil {
		s.log.Error("pg ProjectDelegation failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

// RevokeProjectedDelegation ends a projected delegation on an upstream
// authority.revoked or authority.expired event.
//
// Returns ErrDelegatedAuthorityNotFound when no projected row matches, which
// the consumer treats as already-handled rather than as an error: the event
// may be a redelivery of one already applied, or may name a delegation
// created before this projection existed. It never falls back to revoking a
// LOCALLY-authored row — source_delegation_id IS NOT NULL is in the
// predicate — because an upstream id colliding with a local delegation must
// not let one service revoke another's grant.
func (s *PgStore) RevokeProjectedDelegation(ctx context.Context, sourceService, sourceDelegationID, tenantID string) (*domain.DelegatedAuthority, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}
	if sourceService == "" || sourceDelegationID == "" {
		return nil, domain.ErrProjectionSourceRequired
	}

	const query = `
		UPDATE delegated_authorities
		   SET revocation_status = 'REVOKED'
		 WHERE source_service = $1
		   AND source_delegation_id = $2
		   AND source_delegation_id IS NOT NULL
		   AND tenant_id = $3::uuid
		RETURNING ` + delegationColumns + `;`

	var d *domain.DelegatedAuthority
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanDelegation(tx.QueryRow(ctx, query, sourceService, sourceDelegationID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrDelegatedAuthorityNotFound
		}
		s.log.Error("pg RevokeProjectedDelegation failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

// FindDelegatedAuthorityByID looks up a delegation by its primary key —
// the pre-mutation fetch RevokeDelegatedAuthority's handler needs to
// verify the caller is the delegator before revoking (that check must
// happen BEFORE the transition, not after, same doctrine as
// secret-vault-integration-svc's RevokeLease).
// A delegation belonging to another tenant is reported as
// ErrDelegatedAuthorityNotFound, so a probe cannot distinguish "not yours"
// from "does not exist". The predicate is explicit rather than left to the
// policy: on the compose estate the service connects as a superuser, which
// bypasses row security entirely.
func (s *PgStore) FindDelegatedAuthorityByID(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}
	const query = `SELECT ` + delegationColumns + ` FROM delegated_authorities WHERE delegated_authority_id = $1 AND tenant_id = $2::uuid;`
	var d *domain.DelegatedAuthority
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanDelegation(tx.QueryRow(ctx, query, delegatedAuthorityID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrDelegatedAuthorityNotFound
		}
		s.log.Error("pg FindDelegatedAuthorityByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

func (s *PgStore) RevokeDelegatedAuthority(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}
	current, err := s.FindDelegatedAuthorityByID(ctx, delegatedAuthorityID, tenantID)
	if err != nil {
		return nil, err
	}
	if current.RevocationStatus == "REVOKED" {
		return nil, domain.ErrInvalidTransition
	}

	// tenant_id in the WHERE as well as in the fetch above: the fetch proves
	// the row is ours, this keeps the write from touching anything else if the
	// two ever drift apart.
	const query = `
		UPDATE delegated_authorities
		SET revocation_status = 'REVOKED'
		WHERE delegated_authority_id = $1 AND tenant_id = $2::uuid
		RETURNING ` + delegationColumns + `;`

	var d *domain.DelegatedAuthority
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanDelegation(tx.QueryRow(ctx, query, delegatedAuthorityID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrDelegatedAuthorityNotFound
		}
		s.log.Error("pg RevokeDelegatedAuthority failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

// ── sod_rules ─────────────────────────────────────────────────────────────────

func (s *PgStore) CreateSoDRule(ctx context.Context, params domain.CreateSoDRuleParams) (*domain.SoDRule, error) {
	if params.SoDRuleID == "" {
		params.SoDRuleID = uuid.New().String()
	}

	const query = `
		INSERT INTO sod_rules (sod_rule_id, domain_code, action_a, action_b, conflict_type, jurisdiction_id, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING sod_rule_id, domain_code, action_a, action_b, conflict_type, jurisdiction_id, tenant_id, active_flag, created_at;`

	var tenantID string
	if params.TenantID != nil {
		tenantID = *params.TenantID
	}
	r := &domain.SoDRule{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, params.SoDRuleID, params.DomainCode, params.ActionA, params.ActionB, params.ConflictType, params.JurisdictionID, params.TenantID).
			Scan(&r.SoDRuleID, &r.DomainCode, &r.ActionA, &r.ActionB, &r.ConflictType, &r.JurisdictionID, &r.TenantID, &r.ActiveFlag, &r.CreatedAt)
	})
	if err != nil {
		s.log.Error("pg CreateSoDRule failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// ListSoDRules implements Store.
//
// `tenant_id IS NULL OR tenant_id = $1` mirrors sod_rules' own RLS policy
// from 000004 and CheckSoDConflict's predicate: a NULL tenant means a
// globally-applicable rule that binds every tenant. An operator has to see
// those, because they deny requests just as hard as the tenant's own and are
// the ones nobody in the tenant can edit.
func (s *PgStore) ListSoDRules(ctx context.Context, tenantID string) ([]domain.SoDRule, error) {
	const query = `
		SELECT sod_rule_id, domain_code, action_a, action_b, conflict_type,
		       jurisdiction_id, tenant_id, active_flag, created_at
		  FROM sod_rules
		 WHERE tenant_id IS NULL
		    OR tenant_id = NULLIF($1, '')::uuid
		 ORDER BY active_flag DESC, domain_code, action_a
		 LIMIT 500;`

	var out []domain.SoDRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, query, tenantID)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.SoDRule
			if scanErr := rows.Scan(&r.SoDRuleID, &r.DomainCode, &r.ActionA, &r.ActionB,
				&r.ConflictType, &r.JurisdictionID, &r.TenantID, &r.ActiveFlag, &r.CreatedAt); scanErr != nil {
				return scanErr
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListSoDRules failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── abac_rules ───────────────────────────────────────────────────────────────

const abacRuleColumns = `abac_rule_id, tenant_id, rule_code, action_type, effect, attribute_key, operator, attribute_value, active_flag, created_at, created_by_principal_id`

func scanABACRule(row pgx.Row) (*domain.ABACRule, error) {
	r := &domain.ABACRule{}
	err := row.Scan(&r.ABACRuleID, &r.TenantID, &r.RuleCode, &r.ActionType, &r.Effect,
		&r.AttributeKey, &r.Operator, &r.AttributeValue, &r.ActiveFlag, &r.CreatedAt, &r.CreatedByPrincipalID)
	return r, err
}

// CreateABACRule declares one attribute condition.
//
// Effect and Operator are validated HERE as well as in the handler. The
// handler's check is what produces a 400 with a useful message; this one is
// what stops a row the evaluator cannot execute from existing at all,
// whichever path wrote it. An unevaluable condition on a deny-only layer
// denies the action for everybody, so the cheapest place to catch it is
// before it is stored.
func (s *PgStore) CreateABACRule(ctx context.Context, params domain.CreateABACRuleParams) (*domain.ABACRule, error) {
	if params.ABACRuleID == "" {
		params.ABACRuleID = uuid.New().String()
	}
	if params.Effect != domain.EffectRequire && params.Effect != domain.EffectForbid {
		return nil, domain.ErrUnsupportedABACEffect
	}
	operands, ok := domain.ABACOperators[params.Operator]
	if !ok {
		return nil, domain.ErrUnsupportedABACOperator
	}
	// A one-operand operator with no operand compares against nothing. Under
	// REQUIRE that denies every request for the action; under FORBID it
	// permits every one. Neither is what the author meant, and both are silent.
	if operands == 1 && (params.AttributeValue == nil || *params.AttributeValue == "") {
		return nil, domain.ErrABACOperandRequired
	}

	const query = `
		INSERT INTO abac_rules (abac_rule_id, tenant_id, rule_code, action_type, effect, attribute_key, operator, attribute_value, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING ` + abacRuleColumns + `;`

	var tenantID string
	if params.TenantID != nil {
		tenantID = *params.TenantID
	}
	var r *domain.ABACRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		r, scanErr = scanABACRule(tx.QueryRow(ctx, query, params.ABACRuleID, params.TenantID, params.RuleCode,
			params.ActionType, params.Effect, params.AttributeKey, params.Operator, params.AttributeValue,
			params.CreatedByPrincipalID))
		return scanErr
	})
	if err != nil {
		// A duplicate rule_code in the same scope is a conflict, not an
		// outage — the two partial unique indexes from 000010 are what raise
		// it, one per scope.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, domain.ErrConflict
		}
		s.log.Error("pg CreateABACRule failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// SetABACRuleActive retires or reactivates a rule. Retiring is how a rule
// stops denying: active_flag is in FindABACRules' predicate, so the next
// evaluation ignores it. No hard delete, same as roles — the rule has to stay
// resolvable for any decision it already caused.
func (s *PgStore) SetABACRuleActive(ctx context.Context, abacRuleID, tenantID string, active bool) (*domain.ABACRule, error) {
	// tenant_id = $3 with no IS NULL branch, deliberately: retiring a
	// PLATFORM-WIDE rule from a tenant's own scope would let one tenant
	// disable a control binding every other one. Those are authored behind the
	// platform-scope grant and must be retired the same way.
	const query = `
		UPDATE abac_rules
		   SET active_flag = $2
		 WHERE abac_rule_id = $1 AND tenant_id = $3::uuid
		RETURNING ` + abacRuleColumns + `;`

	var r *domain.ABACRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		r, scanErr = scanABACRule(tx.QueryRow(ctx, query, abacRuleID, active, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrABACRuleNotFound
		}
		s.log.Error("pg SetABACRuleActive failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// ListABACRules returns the rules that apply in tenantID scope: the tenant's
// own plus the platform-wide ones (tenant_id NULL), exactly the set
// FindABACRules evaluates against.
//
// Same reasoning as ListSoDRules: an operator has to see the platform-wide
// rules they cannot edit, because those deny just as hard and a denial
// attributable to a rule the console hides is unexplainable.
func (s *PgStore) ListABACRules(ctx context.Context, tenantID, actionType string) ([]domain.ABACRule, error) {
	const query = `
		SELECT ` + abacRuleColumns + `
		  FROM abac_rules
		 WHERE (tenant_id IS NULL OR tenant_id = NULLIF($1, '')::uuid)
		   AND ($2 = '' OR action_type = $2)
		 ORDER BY active_flag DESC, action_type, rule_code
		 LIMIT 500;`

	var out []domain.ABACRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, query, tenantID, actionType)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			r, scanErr := scanABACRule(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListABACRules failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// FindABACRules returns the ACTIVE attribute conditions guarding actionType in
// tenantID scope — the evaluation read, called from /v1/authorize's layer 5.
//
// `tenant_id IS NULL OR tenant_id = $2` mirrors CheckSoDConflict's predicate
// and abac_rules' own policy: a NULL-tenant rule binds every tenant, so an
// empty tenantID narrows this to platform-wide rules only rather than matching
// nothing.
//
// Returning an empty slice is the overwhelmingly common case and the reason
// this layer costs nothing until somebody declares a rule.
func (s *PgStore) FindABACRules(ctx context.Context, actionType, tenantID string) ([]domain.ABACRule, error) {
	const query = `
		SELECT ` + abacRuleColumns + `
		  FROM abac_rules
		 WHERE active_flag
		   AND action_type = $1
		   AND (tenant_id IS NULL OR tenant_id = NULLIF($2, '')::uuid)
		 ORDER BY rule_code;`

	var out []domain.ABACRule
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, query, actionType, tenantID)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			r, scanErr := scanABACRule(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg FindABACRules failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── evaluation queries ───────────────────────────────────────────────────────

// FindGrantedActions unions permitted_actions from every currently-active
// role assignment + active bundle for (principalID, legalEntityID). A
// tenant-wide assignment (pra.legal_entity_id IS NULL) matches regardless
// of which legal entity is being evaluated.
//
// Scoped to tenantID when there is one, platform-scoped when there is not.
//
// This used to be platform-scoped unconditionally, on the reasoning that the
// caller of /v1/authorize "is not required to supply — and generally does not
// know — which tenant owns principalID". The handler contradicts that: it
// resolves a verified tenant scope from X-Tenant-Id immediately before calling
// this. What it did not do was pass it.
//
// The cost was a cross-tenant privilege escalation, measured on a faithful
// reproduction. A principal holding a TENANT-WIDE assignment
// (principal_role_assignments.legal_entity_id IS NULL) in tenant A, evaluated
// against a legal entity in tenant B:
//
//	role_code        | role_owner_tenant | permitted_actions
//	TENANT_B_VIEWER  | ...tenant B       | ["REPORT_VIEW"]
//	TENANT_A_ADMIN   | ...tenant A       | ["PAYROLL_RUN_FINALIZE","GL_JOURNAL_POST"]
//
// The IS NULL half of the assignment predicate matches regardless of entity,
// and platform scope made tenant A's role visible to join against — so
// /v1/authorize granted PAYROLL_RUN_FINALIZE in tenant B on the strength of a
// role belonging to tenant A. With the tenant installed instead, the same query
// returns only TENANT_B_VIEWER.
//
// The fallback stays because the warning in the old comment was also true: with
// neither a tenant installed nor platform scope, the same query returns zero
// rows, which would deny every authorization check platform-wide. Callers that
// do not yet forward X-Tenant-Id are on the pre-header calling convention, and
// resolveTenantScope already logs each one so they can be found and fixed.
// Removing this fallback is safe only once that log is silent.
func (s *PgStore) FindGrantedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error) {
	// The tenant predicate is in the SQL, not left to the roles policy alone.
	//
	// RLS is the backstop, not the mechanism: it binds app_authorization on
	// Supabase, and it binds nothing at all on the compose estate, where every
	// service connects as the Postgres superuser and a superuser bypasses row
	// security unconditionally. A fix that lived only in withRLS would close
	// this on one deployment and leave it wide open on the other — verified,
	// by this exact query returning tenant A's PAYROLL_RUN_FINALIZE against a
	// superuser connection with tenant B installed.
	//
	// $3 = '' is the no-verified-tenant fallback, and it is the ONLY path that
	// still evaluates across tenants.
	const query = `
		SELECT r.role_code, pb.permitted_actions
		FROM principal_role_assignments pra
		JOIN roles r ON r.role_id = pra.role_id AND r.active_flag
		JOIN permission_bundles pb ON pb.role_id = r.role_id AND pb.active_flag
		WHERE pra.principal_id = $1
		  AND (pra.legal_entity_id = $2 OR pra.legal_entity_id IS NULL)
		  AND ($3 = '' OR r.tenant_id::text = $3)
		  AND pra.effective_from <= NOW()
		  AND (pra.effective_to IS NULL OR pra.effective_to > NOW());`

	seen := map[string]bool{}
	var actions []string
	var roleCodes []string

	evaluate := func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, principalID, legalEntityID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var roleCode string
			var rawActions []byte
			if err := rows.Scan(&roleCode, &rawActions); err != nil {
				return err
			}
			var bundleActions []string
			_ = json.Unmarshal(rawActions, &bundleActions)
			roleCodes = append(roleCodes, roleCode)
			for _, a := range bundleActions {
				if !seen[a] {
					seen[a] = true
					actions = append(actions, a)
				}
			}
		}
		return rows.Err()
	}

	// withRLS installs app.tenant_id, which is what the roles policy filters
	// on; withPlatformScope sets the flag that policy treats as "visible
	// regardless of tenant_id". The choice between them IS the fix.
	var err error
	if tenantID != "" {
		err = s.withRLS(ctx, tenantID, evaluate)
	} else {
		err = s.withPlatformScope(ctx, evaluate)
	}
	if err != nil {
		s.log.Error("pg FindGrantedActions failed", zap.Error(err))
		return nil, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	basis := ""
	if len(roleCodes) > 0 {
		basis = fmt.Sprintf("rbac:role=%s", roleCodes[0])
		for _, rc := range roleCodes[1:] {
			basis += "," + rc
		}
	}
	return actions, basis, nil
}

// FindDelegatedActions resolves active, non-expired delegations to
// principalID in legalEntityID, and returns the union of each delegator's
// own currently-granted actions — intersected with the delegation's own
// delegated_actions subset when it declares one. A tenant-wide delegation
// (legal_entity_id IS NULL) matches regardless of which legal entity is
// being evaluated.
//
// ── THIS LAYER GRANTED NOTHING AT ALL BEFORE 000008 ─────────────────────────
//
// The query below used to run on s.pool directly, outside both withRLS and
// withPlatformScope. 000006 had given delegated_authorities RLS with no
// app.platform_scope hatch, so a connection that installs neither setting
// matches no rows: current_setting returns NULL, the policy's NULLIF of it
// is NULL, and `tenant_id = NULL` is NULL, never true. Layer 2 of /v1/authorize
// therefore returned an empty action set for every request on any deployment
// where the policy actually binds — which is compose (DB_USER=zoiko_app,
// NOSUPERUSER NOBYPASSRLS) and Supabase (app_authorization) both.
//
// It failed CLOSED, which is why nothing broke visibly: a delegate was simply
// denied with basis `no_grant`, indistinguishable from having no delegation.
// Measured on Postgres 16.15 as a NOBYPASSRLS role with one ACTIVE, in-date,
// correctly-tenanted delegation present: 0 rows with neither setting, 1 row
// with app.tenant_id installed.
//
// The store-level tests did not catch it because they run as the migration
// user, and a superuser bypasses row security unconditionally.
//
// Which of the two branches below is load-bearing depends on the deployment,
// and it is worth being exact. On a default one the canonical input-contract
// middleware treats tenant_id as unconditionally mandatory and answers 401
// before this is reached, so the withRLS branch is the live path. The
// platform-scope branch serves observe mode — a documented migration state —
// and keeps this function's documented contract ("an empty tenant evaluates
// across tenants") true rather than silently returning nothing, which is
// precisely how the original defect survived review.
//
// ── ONE QUERY, NOT 1 + N ────────────────────────────────────────────────────
//
// This also used to run one transaction to list delegators and then a separate
// FindGrantedActions — itself another transaction — per delegator. Five
// delegators meant six transactions on the platform's hottest read path. The
// join below is one, inside one transaction, and returns the same set.
//
// ── THE DELEGATOR'S ROLES ARE BOUND TO THE DELEGATION'S OWN TENANT ──────────
//
// `r.tenant_id = da.tenant_id` is the predicate that makes this honest, and it
// is not redundant. When a tenant IS supplied, RLS plus the explicit tenant
// predicates already confine both sides to that tenant. When one is NOT — the
// fallback path, most callers — this whole query runs under platform scope,
// where roles and delegations from every tenant are visible; without this
// predicate a delegation made in tenant A would resolve against the
// delegator's roles in tenant B. That is exactly the cross-tenant escalation
// FindGrantedActions' own comment documents, arriving by a different route.
// delegated_authorities.tenant_id is NOT NULL since 000006, so the delegation
// always has a tenant to bind to.
//
// ── ACTION SUBSETS ARE HONOURED ─────────────────────────────────────────────
//
// scope_type has always accepted 'ACTION_SUBSET' and nothing ever read it, so
// a delegation recorded as a subset conferred the delegator's FULL authority.
// da.delegated_actions (000008) is the subset, and the CASE below intersects
// it with what the delegator actually holds. NULL means full authority, which
// is what every row written before 000008 means. A delegation can never
// confer an action its delegator does not hold — the intersection is with the
// delegator's live grants, resolved right here, not with a snapshot.
func (s *PgStore) FindDelegatedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error) {
	// $3 = '' keeps the no-verified-tenant fallback that FindGrantedActions
	// has, for the same reason: most callers of /v1/authorize do not forward
	// X-Tenant-Id yet, and scoping them to an empty tenant would silently drop
	// every delegation rather than evaluate it.
	//
	// The delegator's half of the predicate is FindGrantedActions' WHERE clause
	// verbatim, so "what the delegator holds" cannot drift between the two
	// paths.
	const query = `
		SELECT da.delegator_principal_id,
		       CASE
		         WHEN da.delegated_actions IS NULL THEN pb.permitted_actions
		         ELSE (
		           SELECT COALESCE(jsonb_agg(a), '[]'::jsonb)
		             FROM jsonb_array_elements(pb.permitted_actions) a
		            WHERE da.delegated_actions @> jsonb_build_array(a)
		         )
		       END AS effective_actions
		  FROM delegated_authorities da
		  JOIN principal_role_assignments pra
		    ON pra.principal_id = da.delegator_principal_id
		  JOIN roles r
		    ON r.role_id = pra.role_id AND r.active_flag
		  JOIN permission_bundles pb
		    ON pb.role_id = r.role_id AND pb.active_flag
		 WHERE da.delegate_principal_id = $1
		   AND (da.legal_entity_id = $2 OR da.legal_entity_id IS NULL)
		   AND ($3 = '' OR da.tenant_id::text = $3)
		   AND da.revocation_status = 'ACTIVE'
		   AND da.effective_from <= NOW()
		   AND (da.effective_to IS NULL OR da.effective_to > NOW())
		   AND r.tenant_id = da.tenant_id
		   AND (pra.legal_entity_id = $2 OR pra.legal_entity_id IS NULL)
		   AND pra.effective_from <= NOW()
		   AND (pra.effective_to IS NULL OR pra.effective_to > NOW())
		 ORDER BY da.delegator_principal_id;`

	seen := map[string]bool{}
	var actions []string
	var basis string

	evaluate := func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, principalID, legalEntityID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var delegator string
			var rawActions []byte
			if err := rows.Scan(&delegator, &rawActions); err != nil {
				return err
			}
			var bundleActions []string
			_ = json.Unmarshal(rawActions, &bundleActions)
			if len(bundleActions) > 0 && basis == "" {
				basis = fmt.Sprintf("delegated:from=%s", delegator)
			}
			for _, a := range bundleActions {
				if !seen[a] {
					seen[a] = true
					actions = append(actions, a)
				}
			}
		}
		return rows.Err()
	}

	// The same choice FindGrantedActions makes, and the same reason it is a
	// choice rather than always withRLS: an empty tenant installed as
	// app.tenant_id matches no delegation row, whereas platform scope matches
	// every tenant's and lets the r.tenant_id = da.tenant_id predicate above
	// keep the resolution within one tenant.
	var err error
	if tenantID != "" {
		err = s.withRLS(ctx, tenantID, evaluate)
	} else {
		err = s.withPlatformScope(ctx, evaluate)
	}
	if err != nil {
		s.log.Error("pg FindDelegatedActions failed", zap.Error(err))
		return nil, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return actions, basis, nil
}

// CheckSoDConflict returns (conflictingAction, true, nil) if grantedActions
// already contains an action that an active sod_rules row pairs with
// candidateAction — checked in both directions (action_a/action_b are
// unordered from the caller's perspective).
func (s *PgStore) CheckSoDConflict(ctx context.Context, grantedActions []string, candidateAction, tenantID string) (string, bool, error) {
	if len(grantedActions) == 0 {
		return "", false, nil
	}

	// tenant_id IS NULL matches globally-applicable rules unconditionally.
	// NULLIF($2, '')::uuid turns an empty tenantID into SQL NULL, so the
	// tenant_id = ... half of the OR is simply never true when the caller
	// didn't supply a tenant — global-only matching, same as before this
	// column existed.
	const query = `
		SELECT action_a, action_b
		FROM sod_rules
		WHERE active_flag
		  AND (action_a = $1 OR action_b = $1)
		  AND (tenant_id IS NULL OR tenant_id = NULLIF($2, '')::uuid);`

	grantedSet := map[string]bool{}
	for _, a := range grantedActions {
		grantedSet[a] = true
	}

	var conflicting string
	var hasConflict bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, candidateAction, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err != nil {
				return err
			}
			other := a
			if a == candidateAction {
				other = b
			}
			if grantedSet[other] {
				conflicting, hasConflict = other, true
				return nil
			}
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg CheckSoDConflict failed", zap.Error(err))
		return "", false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return conflicting, hasConflict, nil
}

// CheckOwnObjectSoD reports whether an active sod_rules row declares
// actionType a dynamic, own-object conflict — see
// domain.ConflictTypeOwnObjectForbidden. Unlike CheckSoDConflict, this is
// not a held-actions-pair lookup: it answers a single question, "does any
// rule forbid the same principal from both preparing and performing
// actionType on one object," independent of anything else the principal
// holds.
func (s *PgStore) CheckOwnObjectSoD(ctx context.Context, actionType, tenantID string) (bool, error) {
	const query = `
		SELECT 1
		FROM sod_rules
		WHERE active_flag
		  AND conflict_type = 'OWN_OBJECT_FORBIDDEN'
		  AND action_a = $1 AND action_b = $1
		  AND (tenant_id IS NULL OR tenant_id = NULLIF($2, '')::uuid)
		LIMIT 1;`

	var forbidden bool
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, actionType, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		forbidden = rows.Next()
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg CheckOwnObjectSoD failed", zap.Error(err))
		return false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return forbidden, nil
}

// ── access_decision_log ──────────────────────────────────────────────────────

const accessDecisionColumns = `access_decision_id, principal_id, legal_entity_id, action_type, decision_outcome, decision_basis, tenant_id, correlation_id, decided_at`

func scanAccessDecision(row pgx.Row) (*domain.AccessDecisionLog, error) {
	d := &domain.AccessDecisionLog{}
	err := row.Scan(&d.AccessDecisionID, &d.PrincipalID, &d.LegalEntityID, &d.ActionType,
		&d.DecisionOutcome, &d.DecisionBasis, &d.TenantID, &d.CorrelationID, &d.DecidedAt)
	return d, err
}

// RecordAccessDecision appends the decision artifact for one evaluation.
//
// params.TenantID is written as SQL NULL when empty, which is the honest
// record of a caller that supplied no tenant - see
// domain.RecordAccessDecisionParams.TenantID for why /v1/authorize cannot
// simply require one. The insert runs inside withRLS so the WITH CHECK on
// access_decision_log's policy has a value to test; that policy admits a NULL
// tenant_id deliberately, and the read path below is what keeps a NULL-tenant
// row from being served to an arbitrary tenant.
func (s *PgStore) RecordAccessDecision(ctx context.Context, params domain.RecordAccessDecisionParams) (*domain.AccessDecisionLog, error) {
	const query = `
		INSERT INTO access_decision_log (principal_id, legal_entity_id, action_type, decision_outcome, decision_basis, correlation_id, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid)
		RETURNING ` + accessDecisionColumns + `;`

	var d *domain.AccessDecisionLog
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanAccessDecision(tx.QueryRow(ctx, query,
			params.PrincipalID, params.LegalEntityID, params.ActionType,
			params.Outcome, params.Basis, params.CorrelationID, params.TenantID))
		return scanErr
	})
	if err != nil {
		s.log.Error("pg RecordAccessDecision failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}

// FindAccessDecisionByID reads one decision, scoped to tenantID.
//
// The explicit tenant_id predicate is the control here, not the RLS policy:
// the policy admits NULL-tenant rows (it has to, so RecordAccessDecision can
// write one), and this predicate is what excludes them. Before this, the read
// carried no tenant or entity predicate at all and the route required no
// authentication, so decision ids could be walked to read every tenant's
// principal_id / legal_entity_id / action_type / decision_basis - including
// the sod:conflict_with basis, which names where the SoD tripwires are.
//
// A row belonging to another tenant, and a row recorded with no tenant, are
// both reported as ErrAccessDecisionNotFound - the handler answers 404, so a
// probe cannot distinguish "not yours" from "does not exist".
func (s *PgStore) FindAccessDecisionByID(ctx context.Context, accessDecisionID, tenantID string) (*domain.AccessDecisionLog, error) {
	const query = `
		SELECT ` + accessDecisionColumns + `
		FROM access_decision_log
		WHERE access_decision_id = $1 AND tenant_id = $2::uuid;`

	var d *domain.AccessDecisionLog
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanAccessDecision(tx.QueryRow(ctx, query, accessDecisionID, tenantID))
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrAccessDecisionNotFound
		}
		s.log.Error("pg FindAccessDecisionByID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return d, nil
}
