// Package store provides the PostgreSQL implementation of the policy
// read and write model.
//
// This package is the ONLY layer that touches the database directly.
// No SQL appears in handlers or domain packages.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
)

// nilScopeUUID is the sentinel used in COALESCE() to make dedup/uniqueness
// constraints work across nullable tenant_id/legal_entity_id columns —
// mirrors jurisdiction-rules-svc's handling of nullable parent_jurisdiction_id.
const nilScopeUUID = "00000000-0000-0000-0000-000000000000"

// Store is the interface consumed by the handler for policy/version CRUD.
type Store interface {
	// CreatePolicy inserts a new policy idempotently.
	CreatePolicy(ctx context.Context, params domain.CreatePolicyParams) (*domain.Policy, bool, error)

	// FindPolicyByID returns the Policy with the given policy_id.
	FindPolicyByID(ctx context.Context, policyID string) (*domain.Policy, error)

	// CreatePolicyVersion inserts a new DRAFT version idempotently.
	// Fails with ErrPolicyNotFound if policy_id does not exist.
	CreatePolicyVersion(ctx context.Context, params domain.CreatePolicyVersionParams) (*domain.PolicyVersion, bool, error)

	// FindPolicyVersionByID looks up a version by ID.
	FindPolicyVersionByID(ctx context.Context, policyVersionID string) (*domain.PolicyVersion, error)

	// ListVersionHistory returns all versions for a policy, newest first.
	// Fails with ErrPolicyNotFound if policy_id does not exist.
	ListVersionHistory(ctx context.Context, policyID string) ([]*domain.PolicyVersion, error)

	// ActivateVersion transitions a DRAFT version to ACTIVE, atomically
	// superseding whatever version was previously ACTIVE in the same
	// (policy_id, tenant_id, legal_entity_id) scope. Returns the activated
	// version, every version superseded as a side effect, and whether this
	// call actually performed a transition (false = idempotent no-op).
	ActivateVersion(ctx context.Context, policyVersionID, actorID string) (*domain.PolicyVersion, []*domain.PolicyVersion, bool, error)

	// FindApplicableVersions returns all ACTIVE versions of the given
	// policy_type whose scope is compatible with (tenantID,
	// legalEntityID), most-specific-scope first. See the method's own
	// doc comment on PgStore for the precedence rule.
	FindApplicableVersions(ctx context.Context, policyType string, tenantID, legalEntityID *string) ([]*domain.ApplicablePolicyVersion, error)
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

// ── policies ─────────────────────────────────────────────────────────────────

// policyColumns is the standard SELECT column list shared by all policy
// queries. Order must match scanPolicy exactly.
const policyColumns = `
	policy_id,
	policy_code,
	policy_name,
	policy_type,
	created_at,
	created_by_principal_id`

// scanPolicy scans one row produced by a policyColumns SELECT.
func scanPolicy(row pgx.Row) (*domain.Policy, error) {
	p := &domain.Policy{}
	err := row.Scan(
		&p.PolicyID,
		&p.PolicyCode,
		&p.PolicyName,
		&p.PolicyType,
		&p.CreatedAt,
		&p.CreatedByPrincipalID,
	)
	return p, err
}

// FindPolicyByID looks up a policy by its UUID primary key.
func (s *PgStore) FindPolicyByID(ctx context.Context, policyID string) (*domain.Policy, error) {
	const query = `
		SELECT ` + policyColumns + `
		FROM policies
		WHERE policy_id = $1;`

	row := s.pool.QueryRow(ctx, query, policyID)
	p, err := scanPolicy(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPolicyNotFound
		}
		s.log.Error("pg FindPolicyByID failed", zap.String("policy_id", policyID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

// CreatePolicy inserts a new policy or returns the existing one on dedup match.
func (s *PgStore) CreatePolicy(ctx context.Context, params domain.CreatePolicyParams) (*domain.Policy, bool, error) {
	if params.PolicyID == "" {
		params.PolicyID = uuid.New().String()
	}

	const query = `
		INSERT INTO policies (
			policy_id, policy_code, policy_name, policy_type, created_by_principal_id
		)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (policy_code)
		DO NOTHING
		RETURNING ` + policyColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.PolicyID, params.PolicyCode, params.PolicyName, params.PolicyType, params.CreatedByPrincipalID,
	)

	p, err := scanPolicy(row)
	if err == nil {
		return p, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreatePolicy failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict occurred on policy_code. Lookup existing record.
	const lookupQuery = `
		SELECT ` + policyColumns + `
		FROM policies
		WHERE policy_code = $1;`

	row = s.pool.QueryRow(ctx, lookupQuery, params.PolicyCode)
	p, err = scanPolicy(row)
	if err != nil {
		s.log.Error("pg CreatePolicy lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if p.PolicyName != params.PolicyName || p.PolicyType != params.PolicyType {
		s.log.Warn("policy dedup match but attribute mismatch (409 conflict)",
			zap.String("existing_id", p.PolicyID),
			zap.String("req_id", params.PolicyID),
		)
		return nil, false, domain.ErrConflict
	}

	return p, false, nil
}

// ── policy_versions ──────────────────────────────────────────────────────────

// policyVersionColumns is the standard SELECT column list for
// policy_versions queries. Order must match scanPolicyVersion exactly.
const policyVersionColumns = `
	policy_version_id,
	policy_id,
	tenant_id,
	legal_entity_id,
	rule_payload,
	effective_from,
	effective_to,
	version_status,
	activated_by_principal_id,
	activated_at,
	created_at,
	created_by_principal_id`

// scanPolicyVersion scans one row produced by a policyVersionColumns SELECT.
func scanPolicyVersion(row pgx.Row) (*domain.PolicyVersion, error) {
	v := &domain.PolicyVersion{}
	err := row.Scan(
		&v.PolicyVersionID,
		&v.PolicyID,
		&v.TenantID,
		&v.LegalEntityID,
		&v.RulePayload,
		&v.EffectiveFrom,
		&v.EffectiveTo,
		&v.VersionStatus,
		&v.ActivatedByPrincipalID,
		&v.ActivatedAt,
		&v.CreatedAt,
		&v.CreatedByPrincipalID,
	)
	return v, err
}

// FindPolicyVersionByID looks up a version by its UUID primary key.
func (s *PgStore) FindPolicyVersionByID(ctx context.Context, policyVersionID string) (*domain.PolicyVersion, error) {
	const query = `
		SELECT ` + policyVersionColumns + `
		FROM policy_versions
		WHERE policy_version_id = $1;`

	row := s.pool.QueryRow(ctx, query, policyVersionID)
	v, err := scanPolicyVersion(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrPolicyVersionNotFound
		}
		s.log.Error("pg FindPolicyVersionByID failed", zap.String("policy_version_id", policyVersionID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return v, nil
}

// CreatePolicyVersion inserts a new DRAFT version idempotently. It first
// validates that the parent policy exists — mirrors jurisdiction-rules-svc's
// FindRules, which validates the parent jurisdiction exists before querying
// its rules, rather than relying on a bare FK-violation error to surface as
// a misleading 503.
func (s *PgStore) CreatePolicyVersion(ctx context.Context, params domain.CreatePolicyVersionParams) (*domain.PolicyVersion, bool, error) {
	if _, err := s.FindPolicyByID(ctx, params.PolicyID); err != nil {
		return nil, false, err
	}

	if params.PolicyVersionID == "" {
		params.PolicyVersionID = uuid.New().String()
	}
	if len(params.RulePayload) == 0 {
		params.RulePayload = []byte(`{}`)
	}

	const query = `
		INSERT INTO policy_versions (
			policy_version_id, policy_id, tenant_id, legal_entity_id, rule_payload,
			effective_from, effective_to, version_status, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', $8)
		ON CONFLICT (
			policy_id,
			COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID),
			COALESCE(legal_entity_id, '` + nilScopeUUID + `'::UUID),
			effective_from
		)
		DO NOTHING
		RETURNING ` + policyVersionColumns + `;`

	row := s.pool.QueryRow(ctx, query,
		params.PolicyVersionID, params.PolicyID, params.TenantID, params.LegalEntityID, params.RulePayload,
		params.EffectiveFrom, params.EffectiveTo, params.CreatedByPrincipalID,
	)

	v, err := scanPolicyVersion(row)
	if err == nil {
		return v, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreatePolicyVersion failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// Conflict occurred on the dedup key. Lookup existing record.
	const lookupQuery = `
		SELECT ` + policyVersionColumns + `
		FROM policy_versions
		WHERE policy_id = $1
		  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($2::uuid, '` + nilScopeUUID + `'::UUID)
		  AND COALESCE(legal_entity_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
		  AND effective_from = $4;`

	row = s.pool.QueryRow(ctx, lookupQuery, params.PolicyID, params.TenantID, params.LegalEntityID, params.EffectiveFrom)
	v, err = scanPolicyVersion(row)
	if err != nil {
		s.log.Error("pg CreatePolicyVersion lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if !jsonEqual(v.RulePayload, params.RulePayload) {
		s.log.Warn("policy version dedup match but payload mismatch (409 conflict)",
			zap.String("existing_id", v.PolicyVersionID),
			zap.String("req_id", params.PolicyVersionID),
		)
		return nil, false, domain.ErrConflict
	}

	return v, false, nil
}

// jsonEqual compares two JSON payloads structurally rather than
// byte-for-byte. Postgres's JSONB type does not preserve input
// formatting — it re-serialises with its own whitespace conventions
// (e.g. a space after every ':' and ','), so a byte comparison between a
// freshly-inserted request body and a value read back from JSONB will
// spuriously mismatch on formatting alone, even when semantically
// identical. json.Unmarshal into `any` followed by reflect.DeepEqual is
// insensitive to whitespace, key order, and numeric literal formatting.
func jsonEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// ListVersionHistory returns all versions for a policy, newest first.
func (s *PgStore) ListVersionHistory(ctx context.Context, policyID string) ([]*domain.PolicyVersion, error) {
	if _, err := s.FindPolicyByID(ctx, policyID); err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + policyVersionColumns + `
		FROM policy_versions
		WHERE policy_id = $1
		ORDER BY effective_from DESC, created_at DESC;`

	rows, err := s.pool.Query(ctx, query, policyID)
	if err != nil {
		s.log.Error("pg ListVersionHistory failed", zap.String("policy_id", policyID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.PolicyVersion
	for rows.Next() {
		v, scanErr := scanPolicyVersion(rows)
		if scanErr != nil {
			s.log.Error("pg ListVersionHistory scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, v)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg ListVersionHistory rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// queryRower is satisfied by both *pgxpool.Pool and pgx.Tx — lets
// transitionVersionStatus run either standalone or inside a transaction.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// transitionVersionStatus is the generic, caller-parameterized transition
// primitive — mirrors jurisdiction-rules-svc's TransitionRuleStatus shape
// exactly: the caller supplies allowedPriors, this function just enforces
// them atomically via UPDATE ... WHERE version_status = ANY(allowedPriors).
// It does not decide the state machine itself.
//
// activatedByPrincipalID is optional (nil for transitions that aren't an
// activation, e.g. a future dedicated retire endpoint): when non-nil, the
// activation audit columns are stamped on this row; when nil, they are
// left untouched. This keeps the helper generic rather than hardcoding
// "activation" semantics for every caller.
func transitionVersionStatus(ctx context.Context, q queryRower, policyVersionID, newStatus string, allowedPriors []string, activatedByPrincipalID *string) (*domain.PolicyVersion, error) {
	const query = `
		UPDATE policy_versions
		SET version_status = $1,
		    activated_by_principal_id = COALESCE($4::text, activated_by_principal_id),
		    activated_at = CASE WHEN $4::text IS NOT NULL THEN NOW() ELSE activated_at END
		WHERE policy_version_id = $2 AND version_status = ANY($3::text[])
		RETURNING ` + policyVersionColumns + `;`

	row := q.QueryRow(ctx, query, newStatus, policyVersionID, allowedPriors, activatedByPrincipalID)
	return scanPolicyVersion(row)
}

// ActivateVersion transitions a DRAFT version to ACTIVE, atomically
// superseding whatever version was previously ACTIVE in the same
// (policy_id, tenant_id, legal_entity_id) scope. Both transitions happen in
// one database transaction, superseding first, so no two versions are ever
// ACTIVE in the same scope at once — also enforced by the partial unique
// index idx_policy_versions_one_active_per_scope.
//
// Returns the activated version, every version that was superseded as a
// side effect (normally 0 or 1), and a transitioned flag — false means
// this call was an idempotent no-op (the version was already ACTIVE) and
// callers must NOT re-publish policy.version.activated/policy.rule.retired
// for it. Callers use the superseded slice to publish policy.rule.retired
// for each (see internal/events/publisher.go).
func (s *PgStore) ActivateVersion(ctx context.Context, policyVersionID, actorID string) (version *domain.PolicyVersion, superseded []*domain.PolicyVersion, transitioned bool, err error) {
	target, err := s.FindPolicyVersionByID(ctx, policyVersionID)
	if err != nil {
		return nil, nil, false, err
	}
	if target.VersionStatus == "ACTIVE" {
		s.log.Debug("policy version activation idempotent no-op",
			zap.String("policy_version_id", policyVersionID),
		)
		return target, nil, false, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg ActivateVersion: begin tx failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	// 1. Supersede whatever is currently ACTIVE in the same scope. Must run
	// BEFORE activating the target so the one-active-per-scope unique index
	// is never violated mid-transaction. RETURNING lets the caller publish
	// policy.rule.retired for exactly the rows this call actually changed.
	const supersedeQuery = `
		UPDATE policy_versions
		SET version_status = 'SUPERSEDED'
		WHERE policy_id = $1
		  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($2::uuid, '` + nilScopeUUID + `'::UUID)
		  AND COALESCE(legal_entity_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
		  AND version_status = 'ACTIVE'
		  AND policy_version_id != $4
		RETURNING ` + policyVersionColumns + `;`

	rows, err := tx.Query(ctx, supersedeQuery, target.PolicyID, target.TenantID, target.LegalEntityID, policyVersionID)
	if err != nil {
		s.log.Error("pg ActivateVersion: supersede failed", zap.String("policy_version_id", policyVersionID), zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	for rows.Next() {
		v, scanErr := scanPolicyVersion(rows)
		if scanErr != nil {
			rows.Close()
			s.log.Error("pg ActivateVersion: supersede scan failed", zap.Error(scanErr))
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		superseded = append(superseded, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.log.Error("pg ActivateVersion: supersede rows error", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	// 2. Activate the target version. Only legal from DRAFT. Stamps
	// activated_by_principal_id/activated_at on this specific row.
	activated, err := transitionVersionStatus(ctx, tx, policyVersionID, "ACTIVE", []string{"DRAFT"}, &actorID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, domain.ErrInvalidTransition
		}
		s.log.Error("pg ActivateVersion: activate failed", zap.String("policy_version_id", policyVersionID), zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if err := tx.Commit(ctx); err != nil {
		s.log.Error("pg ActivateVersion: commit failed", zap.String("policy_version_id", policyVersionID), zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return activated, superseded, true, nil
}

// ── evaluation support ───────────────────────────────────────────────────────

// FindApplicableVersions returns every ACTIVE policy_versions row of the
// given policy_type whose scope is compatible with (tenantID,
// legalEntityID) — i.e. its tenant_id/legal_entity_id are either NULL
// (applies more broadly) or match exactly. A tenant_id/legal_entity_id
// set on the version but NOT matching the request's is excluded outright
// (never leaks across tenants/entities).
//
// Results are ordered most-specific-scope first: an exact
// (tenant_id, legal_entity_id) match sorts before a tenant-only match,
// which sorts before a global (both NULL) match. GET /v1/policies
// returns this full ordered set as-is (the "applicable policy set" per
// 03-microservices.md §8.1); Evaluate takes the first entry as "the"
// applicable version for that type+scope. If more than one *distinct
// policy* of the same policy_type is active at the same specificity
// tier, the tie-break is effective_from DESC — a known v1 simplification
// (see PROGRESS.md); v1 assumes at most one Policy per policy_type is
// the realistic case.
func (s *PgStore) FindApplicableVersions(ctx context.Context, policyType string, tenantID, legalEntityID *string) ([]*domain.ApplicablePolicyVersion, error) {
	const query = `
		SELECT
			pv.policy_version_id, pv.policy_id, pv.tenant_id, pv.legal_entity_id,
			pv.rule_payload, pv.effective_from, pv.effective_to, pv.version_status,
			pv.created_at, pv.created_by_principal_id, p.policy_code
		FROM policy_versions pv
		JOIN policies p ON p.policy_id = pv.policy_id
		WHERE p.policy_type = $1
		  AND pv.version_status = 'ACTIVE'
		  AND (pv.tenant_id IS NULL OR pv.tenant_id = $2::uuid)
		  AND (pv.legal_entity_id IS NULL OR pv.legal_entity_id = $3::uuid)
		ORDER BY
			(CASE WHEN pv.tenant_id IS NOT NULL THEN 1 ELSE 0 END
			 + CASE WHEN pv.legal_entity_id IS NOT NULL THEN 1 ELSE 0 END) DESC,
			pv.effective_from DESC;`

	rows, err := s.pool.Query(ctx, query, policyType, tenantID, legalEntityID)
	if err != nil {
		s.log.Error("pg FindApplicableVersions failed", zap.String("policy_type", policyType), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.ApplicablePolicyVersion
	for rows.Next() {
		v := &domain.ApplicablePolicyVersion{}
		if scanErr := rows.Scan(
			&v.PolicyVersionID, &v.PolicyID, &v.TenantID, &v.LegalEntityID,
			&v.RulePayload, &v.EffectiveFrom, &v.EffectiveTo, &v.VersionStatus,
			&v.CreatedAt, &v.CreatedByPrincipalID, &v.PolicyCode,
		); scanErr != nil {
			s.log.Error("pg FindApplicableVersions scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, v)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg FindApplicableVersions rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}
