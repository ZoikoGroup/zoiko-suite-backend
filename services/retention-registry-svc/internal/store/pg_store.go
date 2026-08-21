// Package store provides the PostgreSQL implementation of
// retention-registry-svc's persistence layer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/retention-registry-svc/internal/domain"
)

type Store interface {
	CreateRetentionPolicy(ctx context.Context, p *domain.RetentionPolicy) error
	FindApplicableRetentionPolicy(ctx context.Context, recordClass string, jurisdictionCode, tenantID *string) (*domain.RetentionPolicy, error)
	ListRetentionPolicies(ctx context.Context, f domain.RetentionPolicyFilter) ([]domain.RetentionPolicy, error)

	CreateLegalHold(ctx context.Context, h *domain.LegalHold) error
	ListLegalHolds(ctx context.Context, f domain.LegalHoldFilter) ([]domain.LegalHold, error)
	// FindLegalHoldByID takes the caller's verified tenant, not only an id —
	// see the implementation for what reading a hold discloses.
	FindLegalHoldByID(ctx context.Context, id, callerTenantID string) (*domain.LegalHold, error)
	ReleaseLegalHold(ctx context.Context, id, callerTenantID, releasedBy, releaseApprovedBy string) (*domain.LegalHold, error)
	FindActiveHoldForScope(ctx context.Context, recordClass, tenantID, entityRef *string) (*domain.LegalHold, error)

	Resolve(ctx context.Context, recordClass string, jurisdictionCode, tenantID, entityRef *string) (*domain.RetentionResolution, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// ── retention_policies ───────────────────────────────────────────────────────

const policyColumns = `
	retention_policy_id, record_class, jurisdiction_code, tenant_id,
	min_retention_days, max_retention_days, legal_regulatory_basis,
	source_rights_basis, privacy_basis, policy_status,
	effective_from, effective_to, created_at, created_by_principal_id`

func scanPolicy(row pgx.Row) (*domain.RetentionPolicy, error) {
	p := &domain.RetentionPolicy{}
	err := row.Scan(
		&p.RetentionPolicyID, &p.RecordClass, &p.JurisdictionCode, &p.TenantID,
		&p.MinRetentionDays, &p.MaxRetentionDays, &p.LegalRegulatoryBasis,
		&p.SourceRightsBasis, &p.PrivacyBasis, &p.PolicyStatus,
		&p.EffectiveFrom, &p.EffectiveTo, &p.CreatedAt, &p.CreatedByPrincipalID,
	)
	return p, err
}

func (s *PgStore) CreateRetentionPolicy(ctx context.Context, p *domain.RetentionPolicy) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO retention_policies (
			retention_policy_id, record_class, jurisdiction_code, tenant_id,
			min_retention_days, max_retention_days, legal_regulatory_basis,
			source_rights_basis, privacy_basis, effective_from, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, p.RetentionPolicyID, p.RecordClass, p.JurisdictionCode, p.TenantID,
		p.MinRetentionDays, p.MaxRetentionDays, p.LegalRegulatoryBasis,
		p.SourceRightsBasis, p.PrivacyBasis, p.EffectiveFrom, p.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert retention policy: %w", err)
	}
	return nil
}

// FindApplicableRetentionPolicy returns the most specific ACTIVE policy
// compatible with the given record_class/jurisdiction/tenant — a stored
// row's jurisdiction_code/tenant_id of NULL always matches (broader), a
// non-NULL value must match exactly. Most-specific (most non-NULL
// dimensions) wins, tie-broken by most recent effective_from.
func (s *PgStore) FindApplicableRetentionPolicy(ctx context.Context, recordClass string, jurisdictionCode, tenantID *string) (*domain.RetentionPolicy, error) {
	const query = `
		SELECT ` + policyColumns + `
		FROM retention_policies
		WHERE record_class = $1
		  AND policy_status = 'ACTIVE'
		  AND (jurisdiction_code IS NULL OR jurisdiction_code = $2)
		  AND (tenant_id IS NULL OR tenant_id = $3::uuid)
		ORDER BY
			(jurisdiction_code IS NOT NULL)::int + (tenant_id IS NOT NULL)::int DESC,
			effective_from DESC
		LIMIT 1;`

	row := s.pool.QueryRow(ctx, query, recordClass, jurisdictionCode, tenantID)
	p, err := scanPolicy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find applicable retention policy: %w", err)
	}
	return p, nil
}

// ── legal_holds ───────────────────────────────────────────────────────────────

const holdColumns = `
	legal_hold_id, scope_description, custodians_objects, authority,
	record_class, tenant_id, entity_ref, hold_status,
	started_at, released_at, released_by_principal_id, release_approved_by_principal_id,
	created_at, created_by_principal_id`

func scanHold(row pgx.Row) (*domain.LegalHold, error) {
	h := &domain.LegalHold{}
	var custodians []byte
	err := row.Scan(
		&h.LegalHoldID, &h.ScopeDescription, &custodians, &h.Authority,
		&h.RecordClass, &h.TenantID, &h.EntityRef, &h.HoldStatus,
		&h.StartedAt, &h.ReleasedAt, &h.ReleasedByPrincipalID, &h.ReleaseApprovedByPrincipalID,
		&h.CreatedAt, &h.CreatedByPrincipalID,
	)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(custodians, &h.CustodiansObjects)
	return h, nil
}

func (s *PgStore) CreateLegalHold(ctx context.Context, h *domain.LegalHold) error {
	custodians := h.CustodiansObjects
	if custodians == nil {
		custodians = []string{}
	}
	custodiansJSON, err := json.Marshal(custodians)
	if err != nil {
		return fmt.Errorf("marshal custodians_objects: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO legal_holds (
			legal_hold_id, scope_description, custodians_objects, authority,
			record_class, tenant_id, entity_ref, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, h.LegalHoldID, h.ScopeDescription, custodiansJSON, h.Authority,
		h.RecordClass, h.TenantID, h.EntityRef, h.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert legal hold: %w", err)
	}
	return nil
}

// FindLegalHoldByID returns one hold, scoped to the caller's verified tenant.
//
// THE TENANT PREDICATE IS NEW AND IT IS THE POINT. This query was
// `WHERE legal_hold_id = $1` and the handler above it required no principal at
// all, so anything that could reach the port could read any hold in any tenant
// by id. What a hold discloses is not incidental: `authority` names the court or
// regulator that ordered the freeze, `scope_description` describes the matter,
// and `custodians_objects` lists the people and systems holding the evidence.
// That is the existence and subject of another tenant's legal proceeding.
//
// Platform-wide holds (tenant_id IS NULL) stay readable by every tenant, because
// they apply to every tenant — a caller unable to see the hold freezing its own
// records would conclude deletion was safe.
//
// A hold belonging to another tenant answers ErrLegalHoldNotFound rather than a
// forbidden, so a probe cannot confirm that an id exists. The same posture
// tenant-entity-registry-svc adopted for the same reason.
func (s *PgStore) FindLegalHoldByID(ctx context.Context, id, callerTenantID string) (*domain.LegalHold, error) {
	if callerTenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `
		SELECT ` + holdColumns + `
		FROM legal_holds
		WHERE legal_hold_id = $1
		  AND (tenant_id IS NULL OR tenant_id = $2::uuid);`

	row := s.pool.QueryRow(ctx, query, id, callerTenantID)
	h, err := scanHold(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrLegalHoldNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find legal hold: %w", err)
	}
	return h, nil
}

// ListRetentionPolicies is the register read. There was no list endpoint on this
// service at all, which is why the console had no way to show what retention
// rules a tenant is operating under — only to resolve one record class at a
// time, which answers a different question.
//
// Ordered newest-effective first: a reader wants the rule in force now at the
// top, and effective_from is the dimension that decides which that is.
func (s *PgStore) ListRetentionPolicies(ctx context.Context, f domain.RetentionPolicyFilter) ([]domain.RetentionPolicy, error) {
	if f.CallerTenantID == "" {
		return nil, domain.ErrTenantMissing
	}
	limit, offset, err := pageBounds(f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + policyColumns + `
		FROM retention_policies
		WHERE (tenant_id IS NULL OR tenant_id = $1::uuid)
		  AND ($2 = '' OR record_class = $2)
		  AND ($3 = '' OR policy_status = $3)
		ORDER BY effective_from DESC, created_at DESC
		LIMIT $4 OFFSET $5;`

	rows, err := s.pool.Query(ctx, query, f.CallerTenantID, f.RecordClass, f.PolicyStatus, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list retention policies: %w", err)
	}
	defer rows.Close()

	out := make([]domain.RetentionPolicy, 0)
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan retention policy: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ListLegalHolds is the other half of the register.
//
// ACTIVE holds sort first regardless of date, because an active hold is the
// thing that blocks a deletion right now and a released one is history. Within
// each group, newest first.
func (s *PgStore) ListLegalHolds(ctx context.Context, f domain.LegalHoldFilter) ([]domain.LegalHold, error) {
	if f.CallerTenantID == "" {
		return nil, domain.ErrTenantMissing
	}
	limit, offset, err := pageBounds(f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}

	const query = `
		SELECT ` + holdColumns + `
		FROM legal_holds
		WHERE (tenant_id IS NULL OR tenant_id = $1::uuid)
		  AND ($2 = '' OR hold_status = $2)
		  AND ($3 = '' OR record_class = $3)
		ORDER BY (hold_status = 'ACTIVE') DESC, started_at DESC
		LIMIT $4 OFFSET $5;`

	rows, err := s.pool.Query(ctx, query, f.CallerTenantID, f.HoldStatus, f.RecordClass, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list legal holds: %w", err)
	}
	defer rows.Close()

	out := make([]domain.LegalHold, 0)
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, fmt.Errorf("scan legal hold: %w", err)
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

// pageBounds applies the estate's usual register limits. An out-of-range value
// is refused rather than clamped: a caller who asked for 5000 rows and silently
// received 500 would read a truncated register as a complete one.
func pageBounds(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 || offset < 0 {
		return 0, 0, domain.ErrInvalidFilter
	}
	return limit, offset, nil
}

// ReleaseLegalHold transitions a hold ACTIVE -> RELEASED, atomically, only
// from ACTIVE — same fail-closed WHERE-status pattern used throughout this
// codebase (ValidXTransitions-style single-transition check).
// ReleaseLegalHold transitions ACTIVE -> RELEASED for a hold in the caller's
// own tenant (or a platform-wide one).
//
// The handler already authorizes LEGAL_HOLD_RELEASE against the hold's own
// tenant, which is the real control and was correct before this change. The
// predicate here is belt-and-braces of the kind purchase-order-svc and
// general-ledger-svc adopted after real CI failures: the authorization check and
// the query that acts on the row should not be able to disagree about which
// tenant's row is being written. Releasing a hold unblocks deletion of records
// something ordered frozen, so this is a poor place to rely on one gate.
func (s *PgStore) ReleaseLegalHold(ctx context.Context, id, callerTenantID, releasedBy, releaseApprovedBy string) (*domain.LegalHold, error) {
	if callerTenantID == "" {
		return nil, domain.ErrTenantMissing
	}

	const query = `
		UPDATE legal_holds
		SET hold_status = 'RELEASED', released_at = NOW(),
		    released_by_principal_id = $1, release_approved_by_principal_id = $2
		WHERE legal_hold_id = $3
		  AND hold_status = 'ACTIVE'
		  AND (tenant_id IS NULL OR tenant_id = $4::uuid)
		RETURNING ` + holdColumns + `;`

	row := s.pool.QueryRow(ctx, query, releasedBy, releaseApprovedBy, id, callerTenantID)
	h, err := scanHold(row)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, findErr := s.FindLegalHoldByID(ctx, id, callerTenantID); findErr != nil {
			return nil, findErr
		}
		return nil, domain.ErrHoldNotActive
	}
	if err != nil {
		return nil, fmt.Errorf("release legal hold: %w", err)
	}
	return h, nil
}

// FindActiveHoldForScope returns the most specific ACTIVE hold compatible
// with the given scope — same nullable-dimension matching as
// kill-switch-registry-svc's ResolveKillSwitch.
func (s *PgStore) FindActiveHoldForScope(ctx context.Context, recordClass, tenantID, entityRef *string) (*domain.LegalHold, error) {
	const query = `
		SELECT ` + holdColumns + `
		FROM legal_holds
		WHERE hold_status = 'ACTIVE'
		  AND (record_class IS NULL OR record_class = $1)
		  AND (tenant_id IS NULL OR tenant_id = $2::uuid)
		  AND (entity_ref IS NULL OR entity_ref = $3)
		ORDER BY
			(record_class IS NOT NULL)::int + (tenant_id IS NOT NULL)::int + (entity_ref IS NOT NULL)::int DESC,
			started_at DESC
		LIMIT 1;`

	row := s.pool.QueryRow(ctx, query, recordClass, tenantID, entityRef)
	h, err := scanHold(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find active hold for scope: %w", err)
	}
	return h, nil
}

// Resolve composes both independent findings doc7 §J1/§J3 requires before
// any deletion/export/migration: is there an active legal hold (checked
// first — a hold blocks regardless of what any policy says), and
// separately, what does the applicable retention policy say. Never
// collapsed into one boolean.
func (s *PgStore) Resolve(ctx context.Context, recordClass string, jurisdictionCode, tenantID, entityRef *string) (*domain.RetentionResolution, error) {
	hold, err := s.FindActiveHoldForScope(ctx, &recordClass, tenantID, entityRef)
	if err != nil {
		return nil, err
	}
	policy, err := s.FindApplicableRetentionPolicy(ctx, recordClass, jurisdictionCode, tenantID)
	if err != nil {
		return nil, err
	}
	return &domain.RetentionResolution{
		Blocked:          hold != nil,
		MatchedHold:      hold,
		ApplicablePolicy: policy,
	}, nil
}
