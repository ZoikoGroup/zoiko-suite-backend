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

	CreateLegalHold(ctx context.Context, h *domain.LegalHold) error
	FindLegalHoldByID(ctx context.Context, id string) (*domain.LegalHold, error)
	ReleaseLegalHold(ctx context.Context, id, releasedBy, releaseApprovedBy string) (*domain.LegalHold, error)
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

func (s *PgStore) FindLegalHoldByID(ctx context.Context, id string) (*domain.LegalHold, error) {
	const query = `SELECT ` + holdColumns + ` FROM legal_holds WHERE legal_hold_id = $1;`
	row := s.pool.QueryRow(ctx, query, id)
	h, err := scanHold(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrLegalHoldNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find legal hold: %w", err)
	}
	return h, nil
}

// ReleaseLegalHold transitions a hold ACTIVE -> RELEASED, atomically, only
// from ACTIVE — same fail-closed WHERE-status pattern used throughout this
// codebase (ValidXTransitions-style single-transition check).
func (s *PgStore) ReleaseLegalHold(ctx context.Context, id, releasedBy, releaseApprovedBy string) (*domain.LegalHold, error) {
	const query = `
		UPDATE legal_holds
		SET hold_status = 'RELEASED', released_at = NOW(),
		    released_by_principal_id = $1, release_approved_by_principal_id = $2
		WHERE legal_hold_id = $3 AND hold_status = 'ACTIVE'
		RETURNING ` + holdColumns + `;`

	row := s.pool.QueryRow(ctx, query, releasedBy, releaseApprovedBy, id)
	h, err := scanHold(row)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, findErr := s.FindLegalHoldByID(ctx, id); findErr != nil {
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
