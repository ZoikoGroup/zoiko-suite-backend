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
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// Store is the interface consumed by the handler.
type Store interface {
	CreateRole(ctx context.Context, params domain.CreateRoleParams) (*domain.Role, bool, error)
	FindRoleByID(ctx context.Context, roleID string) (*domain.Role, error)
	SetRoleActive(ctx context.Context, roleID, tenantID string, active bool) (*domain.Role, error)
	CreatePermissionBundle(ctx context.Context, params domain.CreatePermissionBundleParams) (*domain.PermissionBundle, error)

	CreateRoleAssignment(ctx context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error)
	RevokeRoleAssignment(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error)

	CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error)
	FindDelegatedAuthorityByID(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error)
	RevokeDelegatedAuthority(ctx context.Context, delegatedAuthorityID, tenantID string) (*domain.DelegatedAuthority, error)

	CreateSoDRule(ctx context.Context, params domain.CreateSoDRuleParams) (*domain.SoDRule, error)

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
	if _, err := s.FindRoleByID(ctx, params.RoleID); err != nil {
		return nil, err
	}
	if params.PermissionBundleID == "" {
		params.PermissionBundleID = uuid.New().String()
	}
	actionsJSON, err := json.Marshal(params.PermittedActions)
	if err != nil {
		return nil, fmt.Errorf("marshal permitted_actions: %w", err)
	}

	const query = `
		INSERT INTO permission_bundles (permission_bundle_id, role_id, bundle_code, permitted_actions)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions
		RETURNING permission_bundle_id, role_id, bundle_code, permitted_actions, active_flag, created_at;`

	row := s.pool.QueryRow(ctx, query, params.PermissionBundleID, params.RoleID, params.BundleCode, actionsJSON)
	b := &domain.PermissionBundle{}
	var rawActions []byte
	if err := row.Scan(&b.PermissionBundleID, &b.RoleID, &b.BundleCode, &rawActions, &b.ActiveFlag, &b.CreatedAt); err != nil {
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

	row := s.pool.QueryRow(ctx, query, params.PrincipalRoleAssignmentID, params.PrincipalID, params.RoleID, params.LegalEntityID, params.EffectiveFrom, params.AssignedBy)
	a, err := scanAssignment(row)
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

// ── delegated_authorities ────────────────────────────────────────────────────

const delegationColumns = `delegated_authority_id, tenant_id, delegator_principal_id, delegate_principal_id, scope_type, legal_entity_id, authority_limit_type, authority_limit_value, effective_from, effective_to, revocation_status, created_at`

func scanDelegation(row pgx.Row) (*domain.DelegatedAuthority, error) {
	d := &domain.DelegatedAuthority{}
	err := row.Scan(&d.DelegatedAuthorityID, &d.TenantID, &d.DelegatorPrincipalID, &d.DelegatePrincipalID, &d.ScopeType, &d.LegalEntityID,
		&d.AuthorityLimitType, &d.AuthorityLimitValue, &d.EffectiveFrom, &d.EffectiveTo, &d.RevocationStatus, &d.CreatedAt)
	return d, err
}

func (s *PgStore) CreateDelegatedAuthority(ctx context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error) {
	if params.DelegatedAuthorityID == "" {
		params.DelegatedAuthorityID = uuid.New().String()
	}

	if params.TenantID == "" {
		return nil, domain.ErrTenantScopeRequired
	}

	const query = `
		INSERT INTO delegated_authorities (delegated_authority_id, tenant_id, delegator_principal_id, delegate_principal_id, scope_type, legal_entity_id, authority_limit_type, authority_limit_value, effective_from, effective_to)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING ` + delegationColumns + `;`

	var d *domain.DelegatedAuthority
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanDelegation(tx.QueryRow(ctx, query, params.DelegatedAuthorityID, params.TenantID, params.DelegatorPrincipalID, params.DelegatePrincipalID,
			params.ScopeType, params.LegalEntityID, params.AuthorityLimitType, params.AuthorityLimitValue, params.EffectiveFrom, params.EffectiveTo))
		return scanErr
	})
	if err != nil {
		s.log.Error("pg CreateDelegatedAuthority failed", zap.Error(err))
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
// own currently-granted actions. A tenant-wide delegation (legal_entity_id
// IS NULL) matches regardless of which legal entity is being evaluated.
func (s *PgStore) FindDelegatedActions(ctx context.Context, principalID, legalEntityID, tenantID string) ([]string, string, error) {
	// $3 = '' keeps the no-verified-tenant fallback that FindGrantedActions
	// has, for the same reason: most callers of /v1/authorize do not forward
	// X-Tenant-Id yet, and scoping them to an empty tenant would silently drop
	// every delegation rather than evaluate it.
	const query = `
		SELECT delegator_principal_id
		FROM delegated_authorities
		WHERE delegate_principal_id = $1
		  AND (legal_entity_id = $2 OR legal_entity_id IS NULL)
		  AND ($3 = '' OR tenant_id::text = $3)
		  AND revocation_status = 'ACTIVE'
		  AND effective_from <= NOW()
		  AND (effective_to IS NULL OR effective_to > NOW());`

	rows, err := s.pool.Query(ctx, query, principalID, legalEntityID, tenantID)
	if err != nil {
		s.log.Error("pg FindDelegatedActions failed", zap.Error(err))
		return nil, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	var delegators []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
		delegators = append(delegators, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	seen := map[string]bool{}
	var actions []string
	var basis string
	for _, delegator := range delegators {
		// Same tenant as the delegation itself: a delegator's authority is
		// whatever they hold in the tenant the delegation was made in, not
		// whatever they hold anywhere on the platform.
		delegatorActions, _, err := s.FindGrantedActions(ctx, delegator, legalEntityID, tenantID)
		if err != nil {
			return nil, "", err
		}
		if len(delegatorActions) > 0 && basis == "" {
			basis = fmt.Sprintf("delegated:from=%s", delegator)
		}
		for _, a := range delegatorActions {
			if !seen[a] {
				seen[a] = true
				actions = append(actions, a)
			}
		}
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
