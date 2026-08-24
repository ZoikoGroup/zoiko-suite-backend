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
	svcmiddleware "zoiko.io/retention-registry-svc/internal/middleware"
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

// withTenant runs fn inside a transaction with app.tenant_id set from the
// request context, so migration 000002's policies have a value to enforce
// against.
//
// A transaction is required rather than incidental: set_config's third
// argument is is_local, and only a transaction-local setting is safe on a
// pooled connection. Setting it session-wide would leak one request's
// tenant into whichever request acquires that connection next.
//
// The tenant comes from context (set by middleware.TenantContext from a
// gateway-verified X-Tenant-Id) and never from a query parameter, which is
// where Resolve used to take it. It returns "" when absent, and "" is
// meaningful HERE rather than merely fail-closed: under migration 000002's
// policy, "" matches no tenant-specific row but still matches every
// tenant_id IS NULL row. That is the correct answer to a platform-level
// retention question — see the policy header for why hiding those rows
// would permit deletion of records under a platform-wide hold.
func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on commit path

	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.tenant_id', $1, true)", svcmiddleware.TenantFromContext(ctx),
	); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO retention_policies (
				retention_policy_id, record_class, jurisdiction_code, tenant_id,
				min_retention_days, max_retention_days, legal_regulatory_basis,
				source_rights_basis, privacy_basis, effective_from, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, p.RetentionPolicyID, p.RecordClass, p.JurisdictionCode, p.TenantID,
			p.MinRetentionDays, p.MaxRetentionDays, p.LegalRegulatoryBasis,
			p.SourceRightsBasis, p.PrivacyBasis, p.EffectiveFrom, p.CreatedByPrincipalID,
		)
		return err
	})
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

	var p *domain.RetentionPolicy
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var scanErr error
		p, scanErr = scanPolicy(tx.QueryRow(ctx, query, recordClass, jurisdictionCode, tenantID))
		return scanErr
	})
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
	// A platform-wide hold (tenant_id NULL) passes WITH CHECK for any
	// caller, and that is the SAFE direction of this asymmetry: an
	// unauthorized extra hold blocks a deletion, it never permits one. The
	// dangerous operation is release, and release is authz-gated against
	// the hold's own tenant. See migration 000002's header.
	err = s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO legal_holds (
				legal_hold_id, scope_description, custodians_objects, authority,
				record_class, tenant_id, entity_ref, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, h.LegalHoldID, h.ScopeDescription, custodiansJSON, h.Authority,
			h.RecordClass, h.TenantID, h.EntityRef, h.CreatedByPrincipalID,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("insert legal hold: %w", err)
	}
	return nil
}

// FindLegalHoldByID reads one hold, bounded by the policy to the caller's
// own tenant plus platform-wide holds.
//
// The route in front of this had no tenant input and no authz, so any
// caller holding a legal_hold_id could read its scope_description,
// custodians_objects, authority and record_class — which is to say, learn
// that another tenant is under a legal hold, and over what. That a customer
// is subject to litigation or a regulatory investigation is not something
// their neighbours on the platform should be able to enumerate.
//
// Returns ErrLegalHoldNotFound for a hold outside the caller's scope — the
// same error as a genuinely absent one, so this cannot be used to confirm
// that another tenant's hold id exists.
func (s *PgStore) FindLegalHoldByID(ctx context.Context, id string) (*domain.LegalHold, error) {
	const query = `SELECT ` + holdColumns + ` FROM legal_holds WHERE legal_hold_id = $1;`
	var h *domain.LegalHold
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var scanErr error
		h, scanErr = scanHold(tx.QueryRow(ctx, query, id))
		return scanErr
	})
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

	var h *domain.LegalHold
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var scanErr error
		h, scanErr = scanHold(tx.QueryRow(ctx, query, releasedBy, releaseApprovedBy, id))
		return scanErr
	})
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

	var h *domain.LegalHold
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var scanErr error
		h, scanErr = scanHold(tx.QueryRow(ctx, query, recordClass, tenantID, entityRef))
		return scanErr
	})
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
