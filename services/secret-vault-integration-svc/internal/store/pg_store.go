// Package store provides the PostgreSQL implementation of the secret
// policy, lease, and audit read/write model.
//
// This package is the ONLY layer that touches the database directly. No
// SQL appears in handlers or domain packages. It never touches the
// actual secret material — that lives behind internal/vault.VaultBackend,
// called from the handler layer, not from here.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

// nilScopeUUID is the sentinel used in COALESCE() to make dedup/uniqueness
// constraints work across nullable tenant_id/legal_entity_id columns —
// mirrors policy-svc's handling of nullable scope columns.
const nilScopeUUID = "00000000-0000-0000-0000-000000000000"

// LeaseListFilter narrows ListLeases. All fields optional, compose with AND.
type LeaseListFilter struct {
	RequestedByPrincipalID string
	SecretClass            string
	TenantID               *string
	From                   time.Time
	To                     time.Time
	Limit                  int
	Offset                 int
}

// AuditListFilter narrows ListAuditLog. All fields optional, compose with AND.
type AuditListFilter struct {
	RequestedByPrincipalID string
	SecretPath             string
	EventType              string
	From                   time.Time
	To                     time.Time
	Limit                  int
	Offset                 int
}

// Store is the persistence interface for secret policies, versions,
// leases, and the audit log.
type Store interface {
	CreateSecretPolicy(ctx context.Context, params domain.CreateSecretPolicyParams) (*domain.SecretPolicy, bool, error)
	FindSecretPolicyByID(ctx context.Context, secretPolicyID string) (*domain.SecretPolicy, error)

	CreateSecretPolicyVersion(ctx context.Context, params domain.CreateSecretPolicyVersionParams) (*domain.SecretPolicyVersion, bool, error)
	FindSecretPolicyVersionByID(ctx context.Context, secretPolicyVersionID string) (*domain.SecretPolicyVersion, error)
	ActivateVersion(ctx context.Context, secretPolicyVersionID, actorID string) (*domain.SecretPolicyVersion, []*domain.SecretPolicyVersion, bool, error)
	ListVersionHistory(ctx context.Context, secretPolicyID string) ([]*domain.SecretPolicyVersion, error)

	FindApplicableVersions(ctx context.Context, secretClass string, tenantID, legalEntityID *string) ([]*domain.ApplicableSecretPolicyVersion, error)
	FindApplicableVersionByPath(ctx context.Context, secretPath string, tenantID, legalEntityID *string) (*domain.ApplicableSecretPolicyVersion, error)

	CreateLease(ctx context.Context, params domain.CreateLeaseParams) (*domain.SecretLease, bool, error)
	FindLeaseByID(ctx context.Context, leaseID string) (*domain.SecretLease, error)
	ListLeases(ctx context.Context, filter LeaseListFilter) ([]*domain.SecretLease, error)
	RevokeLease(ctx context.Context, leaseID string) (*domain.SecretLease, bool, error)
	RevokeLeasesBySecretPath(ctx context.Context, secretPath string) ([]*domain.SecretLease, error)

	RecordAuditEntry(ctx context.Context, params domain.RecordAuditEntryParams) (*domain.SecretAccessAuditLog, error)
	FindAuditEntryByRotationRequestID(ctx context.Context, requestID string) (*domain.SecretAccessAuditLog, error)
	ListAuditLog(ctx context.Context, filter AuditListFilter) ([]*domain.SecretAccessAuditLog, error)
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

// ── secret_policies ──────────────────────────────────────────────────────────

const secretPolicyColumns = `
	secret_policy_id,
	secret_class,
	secret_path,
	created_at,
	created_by_principal_id`

func scanSecretPolicy(row pgx.Row) (*domain.SecretPolicy, error) {
	p := &domain.SecretPolicy{}
	err := row.Scan(&p.SecretPolicyID, &p.SecretClass, &p.SecretPath, &p.CreatedAt, &p.CreatedByPrincipalID)
	return p, err
}

// FindSecretPolicyByID looks up a secret policy by its UUID primary key.
func (s *PgStore) FindSecretPolicyByID(ctx context.Context, secretPolicyID string) (*domain.SecretPolicy, error) {
	const query = `SELECT ` + secretPolicyColumns + ` FROM secret_policies WHERE secret_policy_id = $1;`
	p, err := scanSecretPolicy(s.pool.QueryRow(ctx, query, secretPolicyID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSecretPolicyNotFound
		}
		s.log.Error("pg FindSecretPolicyByID failed", zap.String("secret_policy_id", secretPolicyID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

// findSecretPolicyByPath looks up a secret policy by its unique secret_path.
func (s *PgStore) findSecretPolicyByPath(ctx context.Context, secretPath string) (*domain.SecretPolicy, error) {
	const query = `SELECT ` + secretPolicyColumns + ` FROM secret_policies WHERE secret_path = $1;`
	p, err := scanSecretPolicy(s.pool.QueryRow(ctx, query, secretPath))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSecretPolicyNotFound
		}
		s.log.Error("pg findSecretPolicyByPath failed", zap.String("secret_path", secretPath), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

// CreateSecretPolicy inserts a new secret policy or returns the existing
// one on dedup match (secret_path is the unique key — context.md §7.1).
// secret_policy_id is always server-generated (DEFAULT gen_random_uuid())
// — CreateSecretPolicyParams.SecretPolicyID exists for API symmetry with
// the other Create*Params structs in this repo but is not wired to a
// caller-suppliable value here; passing an empty string into a UUID
// column would fail, not silently succeed, so this is deliberately never
// referenced in the INSERT below.
func (s *PgStore) CreateSecretPolicy(ctx context.Context, params domain.CreateSecretPolicyParams) (*domain.SecretPolicy, bool, error) {
	const query = `
		INSERT INTO secret_policies (secret_class, secret_path, created_by_principal_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (secret_path)
		DO NOTHING
		RETURNING ` + secretPolicyColumns + `;`

	p, err := scanSecretPolicy(s.pool.QueryRow(ctx, query, params.SecretClass, params.SecretPath, params.CreatedByPrincipalID))
	if err == nil {
		return p, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateSecretPolicy failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	existing, err := s.findSecretPolicyByPath(ctx, params.SecretPath)
	if err != nil {
		return nil, false, err
	}
	if existing.SecretClass != params.SecretClass {
		s.log.Warn("secret policy dedup match but secret_class mismatch (409 conflict)",
			zap.String("existing_id", existing.SecretPolicyID),
		)
		return nil, false, domain.ErrConflict
	}
	return existing, false, nil
}

// ── secret_policy_versions ───────────────────────────────────────────────────

const secretPolicyVersionColumns = `
	secret_policy_version_id,
	secret_policy_id,
	tenant_id,
	legal_entity_id,
	allowed_workload_ids,
	max_lease_duration_seconds,
	effective_from,
	effective_to,
	version_status,
	created_at,
	created_by_principal_id`

func scanSecretPolicyVersion(row pgx.Row) (*domain.SecretPolicyVersion, error) {
	v := &domain.SecretPolicyVersion{}
	err := row.Scan(
		&v.SecretPolicyVersionID, &v.SecretPolicyID, &v.TenantID, &v.LegalEntityID,
		&v.AllowedWorkloadIDs, &v.MaxLeaseDurationSeconds, &v.EffectiveFrom, &v.EffectiveTo,
		&v.VersionStatus, &v.CreatedAt, &v.CreatedByPrincipalID,
	)
	return v, err
}

// FindSecretPolicyVersionByID looks up a version by its UUID primary key.
func (s *PgStore) FindSecretPolicyVersionByID(ctx context.Context, secretPolicyVersionID string) (*domain.SecretPolicyVersion, error) {
	const query = `SELECT ` + secretPolicyVersionColumns + ` FROM secret_policy_versions WHERE secret_policy_version_id = $1;`
	v, err := scanSecretPolicyVersion(s.pool.QueryRow(ctx, query, secretPolicyVersionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSecretPolicyVersionNotFound
		}
		s.log.Error("pg FindSecretPolicyVersionByID failed", zap.String("secret_policy_version_id", secretPolicyVersionID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return v, nil
}

// CreateSecretPolicyVersion inserts a new DRAFT version idempotently. It
// first validates the parent policy exists — mirrors policy-svc's
// CreatePolicyVersion validating the parent policy before inserting.
// secret_policy_version_id is always server-generated (DEFAULT
// gen_random_uuid()) — same reasoning as CreateSecretPolicy above; an
// empty CreateSecretPolicyVersionParams.SecretPolicyVersionID must never
// be passed into the UUID column.
func (s *PgStore) CreateSecretPolicyVersion(ctx context.Context, params domain.CreateSecretPolicyVersionParams) (*domain.SecretPolicyVersion, bool, error) {
	if _, err := s.FindSecretPolicyByID(ctx, params.SecretPolicyID); err != nil {
		return nil, false, err
	}

	if len(params.AllowedWorkloadIDs) == 0 {
		params.AllowedWorkloadIDs = []byte(`[]`)
	}

	const query = `
		INSERT INTO secret_policy_versions (
			secret_policy_id, tenant_id, legal_entity_id,
			allowed_workload_ids, max_lease_duration_seconds, effective_from, effective_to,
			version_status, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', $8)
		ON CONFLICT (
			secret_policy_id,
			COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID),
			COALESCE(legal_entity_id, '` + nilScopeUUID + `'::UUID),
			effective_from
		)
		DO NOTHING
		RETURNING ` + secretPolicyVersionColumns + `;`

	v, err := scanSecretPolicyVersion(s.pool.QueryRow(ctx, query,
		params.SecretPolicyID, params.TenantID, params.LegalEntityID,
		params.AllowedWorkloadIDs, params.MaxLeaseDurationSeconds, params.EffectiveFrom, params.EffectiveTo,
		params.CreatedByPrincipalID,
	))
	if err == nil {
		return v, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateSecretPolicyVersion failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	const lookupQuery = `
		SELECT ` + secretPolicyVersionColumns + `
		FROM secret_policy_versions
		WHERE secret_policy_id = $1
		  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($2::uuid, '` + nilScopeUUID + `'::UUID)
		  AND COALESCE(legal_entity_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
		  AND effective_from = $4;`

	existing, err := scanSecretPolicyVersion(s.pool.QueryRow(ctx, lookupQuery, params.SecretPolicyID, params.TenantID, params.LegalEntityID, params.EffectiveFrom))
	if err != nil {
		s.log.Error("pg CreateSecretPolicyVersion lookup existing failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if !jsonEqual(existing.AllowedWorkloadIDs, params.AllowedWorkloadIDs) || existing.MaxLeaseDurationSeconds != params.MaxLeaseDurationSeconds {
		s.log.Warn("secret policy version dedup match but payload mismatch (409 conflict)",
			zap.String("existing_id", existing.SecretPolicyVersionID),
		)
		return nil, false, domain.ErrConflict
	}
	return existing, false, nil
}

// ListVersionHistory returns all versions for a secret policy, newest first.
func (s *PgStore) ListVersionHistory(ctx context.Context, secretPolicyID string) ([]*domain.SecretPolicyVersion, error) {
	if _, err := s.FindSecretPolicyByID(ctx, secretPolicyID); err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + secretPolicyVersionColumns + `
		FROM secret_policy_versions
		WHERE secret_policy_id = $1
		ORDER BY effective_from DESC, created_at DESC;`

	rows, err := s.pool.Query(ctx, query, secretPolicyID)
	if err != nil {
		s.log.Error("pg ListVersionHistory failed", zap.String("secret_policy_id", secretPolicyID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.SecretPolicyVersion
	for rows.Next() {
		v, scanErr := scanSecretPolicyVersion(rows)
		if scanErr != nil {
			s.log.Error("pg ListVersionHistory scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// queryRower is satisfied by both *pgxpool.Pool and pgx.Tx.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// transitionVersionStatus is the generic, caller-parameterized transition
// primitive — mirrors policy-svc's TransitionRuleStatus-derived helper
// exactly: the caller supplies allowedPriors, this just enforces them
// atomically.
func transitionVersionStatus(ctx context.Context, q queryRower, secretPolicyVersionID, newStatus string, allowedPriors []string) (*domain.SecretPolicyVersion, error) {
	const query = `
		UPDATE secret_policy_versions
		SET version_status = $1
		WHERE secret_policy_version_id = $2 AND version_status = ANY($3::text[])
		RETURNING ` + secretPolicyVersionColumns + `;`

	row := q.QueryRow(ctx, query, newStatus, secretPolicyVersionID, allowedPriors)
	return scanSecretPolicyVersion(row)
}

// ActivateVersion transitions a DRAFT version to ACTIVE, atomically
// superseding whatever version was previously ACTIVE in the same
// (secret_policy_id, tenant_id, legal_entity_id) scope. Identical shape
// to policy-svc's ActivateVersion.
func (s *PgStore) ActivateVersion(ctx context.Context, secretPolicyVersionID, actorID string) (version *domain.SecretPolicyVersion, superseded []*domain.SecretPolicyVersion, transitioned bool, err error) {
	target, err := s.FindSecretPolicyVersionByID(ctx, secretPolicyVersionID)
	if err != nil {
		return nil, nil, false, err
	}
	if target.VersionStatus == "ACTIVE" {
		return target, nil, false, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.log.Error("pg ActivateVersion: begin tx failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const supersedeQuery = `
		UPDATE secret_policy_versions
		SET version_status = 'SUPERSEDED'
		WHERE secret_policy_id = $1
		  AND COALESCE(tenant_id, '` + nilScopeUUID + `'::UUID) = COALESCE($2::uuid, '` + nilScopeUUID + `'::UUID)
		  AND COALESCE(legal_entity_id, '` + nilScopeUUID + `'::UUID) = COALESCE($3::uuid, '` + nilScopeUUID + `'::UUID)
		  AND version_status = 'ACTIVE'
		  AND secret_policy_version_id != $4
		RETURNING ` + secretPolicyVersionColumns + `;`

	rows, err := tx.Query(ctx, supersedeQuery, target.SecretPolicyID, target.TenantID, target.LegalEntityID, secretPolicyVersionID)
	if err != nil {
		s.log.Error("pg ActivateVersion: supersede failed", zap.Error(err))
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	for rows.Next() {
		v, scanErr := scanSecretPolicyVersion(rows)
		if scanErr != nil {
			rows.Close()
			return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		superseded = append(superseded, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	activated, err := transitionVersionStatus(ctx, tx, secretPolicyVersionID, "ACTIVE", []string{"DRAFT"})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, domain.ErrInvalidTransition
		}
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return activated, superseded, true, nil
}

// ── evaluation support ───────────────────────────────────────────────────────

// applicableVersionQuery is shared by FindApplicableVersions (by class)
// and FindApplicableVersionByPath (by path) — same scope-precedence
// ordering, different WHERE predicate on which column identifies the
// policy.
const applicableVersionColumns = `
	spv.secret_policy_version_id, spv.secret_policy_id, spv.tenant_id, spv.legal_entity_id,
	spv.allowed_workload_ids, spv.max_lease_duration_seconds, spv.effective_from, spv.effective_to,
	spv.version_status, spv.created_at, spv.created_by_principal_id, sp.secret_class, sp.secret_path`

func scanApplicableVersion(row pgx.Row) (*domain.ApplicableSecretPolicyVersion, error) {
	v := &domain.ApplicableSecretPolicyVersion{}
	err := row.Scan(
		&v.SecretPolicyVersionID, &v.SecretPolicyID, &v.TenantID, &v.LegalEntityID,
		&v.AllowedWorkloadIDs, &v.MaxLeaseDurationSeconds, &v.EffectiveFrom, &v.EffectiveTo,
		&v.VersionStatus, &v.CreatedAt, &v.CreatedByPrincipalID, &v.SecretClass, &v.SecretPath,
	)
	return v, err
}

// FindApplicableVersions returns every ACTIVE version of the given
// secret_class whose scope is compatible with (tenantID, legalEntityID),
// most-specific-scope first — identical precedence rule to policy-svc's
// FindApplicableVersions.
func (s *PgStore) FindApplicableVersions(ctx context.Context, secretClass string, tenantID, legalEntityID *string) ([]*domain.ApplicableSecretPolicyVersion, error) {
	const query = `
		SELECT ` + applicableVersionColumns + `
		FROM secret_policy_versions spv
		JOIN secret_policies sp ON sp.secret_policy_id = spv.secret_policy_id
		WHERE sp.secret_class = $1
		  AND spv.version_status = 'ACTIVE'
		  AND (spv.tenant_id IS NULL OR spv.tenant_id = $2::uuid)
		  AND (spv.legal_entity_id IS NULL OR spv.legal_entity_id = $3::uuid)
		ORDER BY
			(CASE WHEN spv.tenant_id IS NOT NULL THEN 1 ELSE 0 END
			 + CASE WHEN spv.legal_entity_id IS NOT NULL THEN 1 ELSE 0 END) DESC,
			spv.effective_from DESC;`

	rows, err := s.pool.Query(ctx, query, secretClass, tenantID, legalEntityID)
	if err != nil {
		s.log.Error("pg FindApplicableVersions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.ApplicableSecretPolicyVersion
	for rows.Next() {
		v, scanErr := scanApplicableVersion(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// FindApplicableVersionByPath is the broker's own lookup (context.md
// §7.2's correction): resolves by the policy's unique secret_path rather
// than secret_class+scope, since class+scope alone cannot identify which
// of potentially several same-class secrets is being requested. Returns
// the single most-specific-scope match, or domain.ErrSecretPolicyNotFound
// if no policy is registered for that path at all, or
// domain.ErrSecretPolicyVersionNotFound if a policy exists but has no
// ACTIVE version for this scope.
func (s *PgStore) FindApplicableVersionByPath(ctx context.Context, secretPath string, tenantID, legalEntityID *string) (*domain.ApplicableSecretPolicyVersion, error) {
	if _, err := s.findSecretPolicyByPath(ctx, secretPath); err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + applicableVersionColumns + `
		FROM secret_policy_versions spv
		JOIN secret_policies sp ON sp.secret_policy_id = spv.secret_policy_id
		WHERE sp.secret_path = $1
		  AND spv.version_status = 'ACTIVE'
		  AND (spv.tenant_id IS NULL OR spv.tenant_id = $2::uuid)
		  AND (spv.legal_entity_id IS NULL OR spv.legal_entity_id = $3::uuid)
		ORDER BY
			(CASE WHEN spv.tenant_id IS NOT NULL THEN 1 ELSE 0 END
			 + CASE WHEN spv.legal_entity_id IS NOT NULL THEN 1 ELSE 0 END) DESC,
			spv.effective_from DESC
		LIMIT 1;`

	v, err := scanApplicableVersion(s.pool.QueryRow(ctx, query, secretPath, tenantID, legalEntityID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrSecretPolicyVersionNotFound
		}
		s.log.Error("pg FindApplicableVersionByPath failed", zap.String("secret_path", secretPath), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return v, nil
}

// jsonEqual compares two JSON payloads structurally rather than
// byte-for-byte — Postgres re-serialises JSONB with its own whitespace
// conventions (policy-svc precedent, context.md).
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

// ── secret_leases ────────────────────────────────────────────────────────────

const secretLeaseColumns = `
	lease_id,
	request_id,
	secret_policy_version_id,
	secret_class,
	secret_path,
	requested_by_principal_id,
	tenant_id,
	legal_entity_id,
	status,
	granted_at,
	expires_at,
	revoked_at,
	correlation_id,
	created_at`

// secretLeaseReadColumns is secretLeaseColumns with one difference: status
// is computed at read time rather than trusted verbatim from the stored
// column. context.md §7.1: EXPIRED is a computed read (status = 'GRANTED'
// AND expires_at < NOW()), deliberately never a background job flipping
// rows. Use this column set for every path that reports a lease's status
// back to a caller (FindLeaseByID, findLeaseByRequestID, ListLeases); use
// the raw secretLeaseColumns for INSERT/UPDATE RETURNING clauses, where
// the row was just written and its stored status is by definition current.
const secretLeaseReadColumns = `
	lease_id,
	request_id,
	secret_policy_version_id,
	secret_class,
	secret_path,
	requested_by_principal_id,
	tenant_id,
	legal_entity_id,
	CASE WHEN status = 'GRANTED' AND expires_at < NOW() THEN 'EXPIRED' ELSE status END,
	granted_at,
	expires_at,
	revoked_at,
	correlation_id,
	created_at`

func scanSecretLease(row pgx.Row) (*domain.SecretLease, error) {
	l := &domain.SecretLease{}
	err := row.Scan(
		&l.LeaseID, &l.RequestID, &l.SecretPolicyVersionID, &l.SecretClass, &l.SecretPath,
		&l.RequestedByPrincipalID, &l.TenantID, &l.LegalEntityID, &l.Status,
		&l.GrantedAt, &l.ExpiresAt, &l.RevokedAt, &l.CorrelationID, &l.CreatedAt,
	)
	return l, err
}

// CreateLease inserts a new granted lease, idempotent on request_id —
// mirrors governance-decision-log-svc's Insert dedup exactly
// (ON CONFLICT DO NOTHING, never a prior SELECT-EXISTS check).
func (s *PgStore) CreateLease(ctx context.Context, params domain.CreateLeaseParams) (*domain.SecretLease, bool, error) {
	const insertQuery = `
		INSERT INTO secret_leases (
			request_id, secret_policy_version_id, secret_class, secret_path,
			requested_by_principal_id, tenant_id, legal_entity_id, expires_at, correlation_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (request_id) DO NOTHING
		RETURNING ` + secretLeaseColumns + `;`

	l, err := scanSecretLease(s.pool.QueryRow(ctx, insertQuery,
		params.RequestID, params.SecretPolicyVersionID, params.SecretClass, params.SecretPath,
		params.RequestedByPrincipalID, params.TenantID, params.LegalEntityID, params.ExpiresAt, params.CorrelationID,
	))
	if err == nil {
		return l, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.log.Error("pg CreateLease failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	existing, err := s.findLeaseByRequestID(ctx, params.RequestID)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (s *PgStore) findLeaseByRequestID(ctx context.Context, requestID string) (*domain.SecretLease, error) {
	const query = `SELECT ` + secretLeaseReadColumns + ` FROM secret_leases WHERE request_id = $1;`
	l, err := scanSecretLease(s.pool.QueryRow(ctx, query, requestID))
	if err != nil {
		s.log.Error("pg findLeaseByRequestID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return l, nil
}

// FindLeaseByID retrieves a single lease record. Status reflects the
// computed EXPIRED read (secretLeaseReadColumns), not just the stored
// GRANTED/REVOKED value.
func (s *PgStore) FindLeaseByID(ctx context.Context, leaseID string) (*domain.SecretLease, error) {
	const query = `SELECT ` + secretLeaseReadColumns + ` FROM secret_leases WHERE lease_id = $1;`
	l, err := scanSecretLease(s.pool.QueryRow(ctx, query, leaseID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrLeaseNotFound
		}
		s.log.Error("pg FindLeaseByID failed", zap.String("lease_id", leaseID), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return l, nil
}

// ListLeases returns a paginated, optionally-filtered slice of leases,
// newest first. Mirrors governance-decision-log-svc's List exactly,
// including its pagination defaults.
func (s *PgStore) ListLeases(ctx context.Context, filter LeaseListFilter) ([]*domain.SecretLease, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	args := []any{}
	conditions := []string{}
	argIdx := 1
	addCond := func(cond string, val any) {
		conditions = append(conditions, fmt.Sprintf(cond, argIdx))
		args = append(args, val)
		argIdx++
	}

	if filter.RequestedByPrincipalID != "" {
		addCond("requested_by_principal_id = $%d", filter.RequestedByPrincipalID)
	}
	if filter.SecretClass != "" {
		addCond("secret_class = $%d", filter.SecretClass)
	}
	if filter.TenantID != nil {
		addCond("tenant_id = $%d", *filter.TenantID)
	}
	if !filter.From.IsZero() {
		addCond("granted_at >= $%d", filter.From)
	}
	if !filter.To.IsZero() {
		addCond("granted_at <= $%d", filter.To)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM secret_leases
		%s
		ORDER BY granted_at DESC
		LIMIT $%d OFFSET $%d`,
		secretLeaseReadColumns, where, argIdx, argIdx+1,
	)
	args = append(args, limit, filter.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg ListLeases failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.SecretLease
	for rows.Next() {
		l, scanErr := scanSecretLease(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// RevokeLease transitions a single lease GRANTED -> REVOKED. Idempotent:
// revoking an already-REVOKED lease returns it unchanged with
// transitioned=false. Returns domain.ErrInvalidTransition for any other
// status (EXPIRED — nothing to revoke).
func (s *PgStore) RevokeLease(ctx context.Context, leaseID string) (*domain.SecretLease, bool, error) {
	current, err := s.FindLeaseByID(ctx, leaseID)
	if err != nil {
		return nil, false, err
	}
	if current.Status == "REVOKED" {
		return current, false, nil
	}
	if current.Status != "GRANTED" {
		return nil, false, domain.ErrInvalidTransition
	}

	const query = `
		UPDATE secret_leases
		SET status = 'REVOKED', revoked_at = NOW()
		WHERE lease_id = $1 AND status = 'GRANTED'
		RETURNING ` + secretLeaseColumns + `;`

	revoked, err := scanSecretLease(s.pool.QueryRow(ctx, query, leaseID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, domain.ErrInvalidTransition
		}
		s.log.Error("pg RevokeLease failed", zap.String("lease_id", leaseID), zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return revoked, true, nil
}

// RevokeLeasesBySecretPath mass-revokes every currently GRANTED lease for
// a secret_path — called by Rotate (context.md §7.2's fix: rotating a
// secret must invalidate leases pointing at the now-stale material, not
// just record that a rotation happened). Returns every lease actually
// revoked so the caller can emit one audit entry per lease.
func (s *PgStore) RevokeLeasesBySecretPath(ctx context.Context, secretPath string) ([]*domain.SecretLease, error) {
	const query = `
		UPDATE secret_leases
		SET status = 'REVOKED', revoked_at = NOW()
		WHERE secret_path = $1 AND status = 'GRANTED'
		RETURNING ` + secretLeaseColumns + `;`

	rows, err := s.pool.Query(ctx, query, secretPath)
	if err != nil {
		s.log.Error("pg RevokeLeasesBySecretPath failed", zap.String("secret_path", secretPath), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var revoked []*domain.SecretLease
	for rows.Next() {
		l, scanErr := scanSecretLease(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		revoked = append(revoked, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return revoked, nil
}

// ── secret_access_audit_log ──────────────────────────────────────────────────

const auditLogColumns = `
	audit_log_id,
	event_type,
	secret_class,
	secret_path,
	requested_by_principal_id,
	tenant_id,
	legal_entity_id,
	lease_id,
	secret_policy_version_id,
	request_id,
	outcome_detail,
	correlation_id,
	recorded_at`

func scanAuditEntry(row pgx.Row) (*domain.SecretAccessAuditLog, error) {
	a := &domain.SecretAccessAuditLog{}
	err := row.Scan(
		&a.AuditLogID, &a.EventType, &a.SecretClass, &a.SecretPath,
		&a.RequestedByPrincipalID, &a.TenantID, &a.LegalEntityID,
		&a.LeaseID, &a.SecretPolicyVersionID, &a.RequestID,
		&a.OutcomeDetail, &a.CorrelationID, &a.RecordedAt,
	)
	return a, err
}

// RecordAuditEntry appends one immutable audit record. No conflict
// handling needed for REQUESTED/GRANTED/DENIED/REVOKED entries — every
// call is a new fact. Only ROTATED entries carry a request_id and are
// deduped, see RecordRotationAuditEntry... actually handled by the
// caller checking FindAuditEntryByRotationRequestID first (§below) since
// a plain INSERT here would violate the partial unique index on retry
// rather than gracefully no-op — callers rotating must check first.
func (s *PgStore) RecordAuditEntry(ctx context.Context, params domain.RecordAuditEntryParams) (*domain.SecretAccessAuditLog, error) {
	const query = `
		INSERT INTO secret_access_audit_log (
			event_type, secret_class, secret_path, requested_by_principal_id,
			tenant_id, legal_entity_id, lease_id, secret_policy_version_id,
			request_id, outcome_detail, correlation_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING ` + auditLogColumns + `;`

	a, err := scanAuditEntry(s.pool.QueryRow(ctx, query,
		params.EventType, params.SecretClass, params.SecretPath, params.RequestedByPrincipalID,
		params.TenantID, params.LegalEntityID, params.LeaseID, params.SecretPolicyVersionID,
		params.RequestID, params.OutcomeDetail, params.CorrelationID,
	))
	if err != nil {
		s.log.Error("pg RecordAuditEntry failed", zap.String("event_type", params.EventType), zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// FindAuditEntryByRotationRequestID looks up a prior ROTATED entry by its
// request_id — the dedup check Rotate must perform before calling the
// vault backend again, since a plain re-INSERT would just violate the
// partial unique index instead of returning the original outcome.
// Returns domain.ErrLeaseNotFound-shaped semantics via a plain nil,nil on
// no match (not found is a valid, expected outcome here, not an error).
func (s *PgStore) FindAuditEntryByRotationRequestID(ctx context.Context, requestID string) (*domain.SecretAccessAuditLog, error) {
	const query = `
		SELECT ` + auditLogColumns + `
		FROM secret_access_audit_log
		WHERE event_type = 'ROTATED' AND request_id = $1;`

	a, err := scanAuditEntry(s.pool.QueryRow(ctx, query, requestID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		s.log.Error("pg FindAuditEntryByRotationRequestID failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// ListAuditLog returns a paginated, optionally-filtered slice of audit
// entries, newest first. Same pagination contract as ListLeases.
func (s *PgStore) ListAuditLog(ctx context.Context, filter AuditListFilter) ([]*domain.SecretAccessAuditLog, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	args := []any{}
	conditions := []string{}
	argIdx := 1
	addCond := func(cond string, val any) {
		conditions = append(conditions, fmt.Sprintf(cond, argIdx))
		args = append(args, val)
		argIdx++
	}

	if filter.RequestedByPrincipalID != "" {
		addCond("requested_by_principal_id = $%d", filter.RequestedByPrincipalID)
	}
	if filter.SecretPath != "" {
		addCond("secret_path = $%d", filter.SecretPath)
	}
	if filter.EventType != "" {
		addCond("event_type = $%d", filter.EventType)
	}
	if !filter.From.IsZero() {
		addCond("recorded_at >= $%d", filter.From)
	}
	if !filter.To.IsZero() {
		addCond("recorded_at <= $%d", filter.To)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT %s
		FROM secret_access_audit_log
		%s
		ORDER BY recorded_at DESC
		LIMIT $%d OFFSET $%d`,
		auditLogColumns, where, argIdx, argIdx+1,
	)
	args = append(args, limit, filter.Offset)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg ListAuditLog failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.SecretAccessAuditLog
	for rows.Next() {
		a, scanErr := scanAuditEntry(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// ─── compile-time interface check ──────────────────────────────────────────

var _ Store = (*PgStore)(nil)
