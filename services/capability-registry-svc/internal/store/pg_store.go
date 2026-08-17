// Package store provides the PostgreSQL implementation of
// capability-registry-svc's persistence layer.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/capability-registry-svc/internal/domain"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type Store interface {
	CreateCapability(ctx context.Context, c *domain.Capability) error
	GetCapability(ctx context.Context, capabilityID string) (*domain.Capability, error)
	GetCapabilityByCode(ctx context.Context, capabilityCode string) (*domain.Capability, error)

	CreateMarketRelease(ctx context.Context, m *domain.MarketRelease) error
	GetActiveMarketRelease(ctx context.Context, capabilityID, marketCode string) (*domain.MarketRelease, error)

	CreateIntegrationCapability(ctx context.Context, i *domain.IntegrationCapability) error
	ListIntegrationCapabilitiesByCapability(ctx context.Context, capabilityID string) ([]domain.IntegrationCapability, error)
	UpdateIntegrationHealth(ctx context.Context, integrationCapabilityID, healthStatus string) error

	CreateRelease(ctx context.Context, r *domain.Release) error
	GetCurrentRelease(ctx context.Context, capabilityID string) (*domain.Release, error)

	CreateCapabilityClaim(ctx context.Context, c *domain.CapabilityClaim) error
	ListClaimsByCapability(ctx context.Context, capabilityID string) ([]domain.CapabilityClaim, error)

	ResolveCapability(ctx context.Context, capabilityCode, marketCode string) (*domain.CapabilityResolution, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) CreateCapability(ctx context.Context, c *domain.Capability) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO capabilities (
			capability_id, capability_code, module_domain, version, dependencies,
			execution_risk_class, created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, c.CapabilityID, c.CapabilityCode, c.ModuleDomain, c.Version, c.Dependencies,
		c.ExecutionRiskClass, c.CreatedAt, c.CreatedByPrincipalID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: capability_code %s", domain.ErrConflict, c.CapabilityCode)
		}
		return fmt.Errorf("insert capability: %w", err)
	}
	return nil
}

func (s *PgStore) GetCapability(ctx context.Context, capabilityID string) (*domain.Capability, error) {
	var c domain.Capability
	err := s.pool.QueryRow(ctx, `
		SELECT capability_id, capability_code, module_domain, version, dependencies,
		       execution_risk_class, created_at, created_by_principal_id
		FROM capabilities WHERE capability_id = $1
	`, capabilityID).Scan(
		&c.CapabilityID, &c.CapabilityCode, &c.ModuleDomain, &c.Version, &c.Dependencies,
		&c.ExecutionRiskClass, &c.CreatedAt, &c.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCapabilityNotFound
	}
	return &c, err
}

func (s *PgStore) GetCapabilityByCode(ctx context.Context, capabilityCode string) (*domain.Capability, error) {
	var c domain.Capability
	err := s.pool.QueryRow(ctx, `
		SELECT capability_id, capability_code, module_domain, version, dependencies,
		       execution_risk_class, created_at, created_by_principal_id
		FROM capabilities WHERE capability_code = $1
	`, capabilityCode).Scan(
		&c.CapabilityID, &c.CapabilityCode, &c.ModuleDomain, &c.Version, &c.Dependencies,
		&c.ExecutionRiskClass, &c.CreatedAt, &c.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCapabilityNotFound
	}
	return &c, err
}

func (s *PgStore) CreateMarketRelease(ctx context.Context, m *domain.MarketRelease) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO market_releases (
			market_release_id, capability_id, market_code, language_code,
			legal_approval_status, state, effective_from, effective_to,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, m.MarketReleaseID, m.CapabilityID, m.MarketCode, m.LanguageCode,
		m.LegalApprovalStatus, string(m.State), m.EffectiveFrom, m.EffectiveTo,
		m.CreatedAt, m.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert market release: %w", err)
	}
	return nil
}

// GetActiveMarketRelease returns the market release currently in effect for
// (capabilityID, marketCode), most-recently-created first.
func (s *PgStore) GetActiveMarketRelease(ctx context.Context, capabilityID, marketCode string) (*domain.MarketRelease, error) {
	now := time.Now().UTC()
	var m domain.MarketRelease
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT market_release_id, capability_id, market_code, language_code,
		       legal_approval_status, state, effective_from, effective_to,
		       created_at, created_by_principal_id
		FROM market_releases
		WHERE capability_id = $1 AND market_code = $2
		  AND effective_from <= $3 AND (effective_to IS NULL OR effective_to > $3)
		ORDER BY effective_from DESC
		LIMIT 1
	`, capabilityID, marketCode, now).Scan(
		&m.MarketReleaseID, &m.CapabilityID, &m.MarketCode, &m.LanguageCode,
		&m.LegalApprovalStatus, &state, &m.EffectiveFrom, &m.EffectiveTo,
		&m.CreatedAt, &m.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrMarketReleaseNotFound
	}
	if err != nil {
		return nil, err
	}
	m.State = domain.MarketReleaseState(state)
	return &m, nil
}

func (s *PgStore) CreateIntegrationCapability(ctx context.Context, i *domain.IntegrationCapability) error {
	if i.HealthStatus == "" {
		i.HealthStatus = "UNKNOWN"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO integration_capabilities (
			integration_capability_id, capability_id, provider_code, certified,
			health_status, created_at, updated_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
	`, i.IntegrationCapabilityID, i.CapabilityID, i.ProviderCode, i.Certified,
		i.HealthStatus, i.CreatedAt, i.CreatedByPrincipalID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: capability_id/provider_code pair already registered", domain.ErrConflict)
		}
		return fmt.Errorf("insert integration capability: %w", err)
	}
	return nil
}

func (s *PgStore) ListIntegrationCapabilitiesByCapability(ctx context.Context, capabilityID string) ([]domain.IntegrationCapability, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT integration_capability_id, capability_id, provider_code, certified,
		       health_status, created_at, updated_at, created_by_principal_id
		FROM integration_capabilities WHERE capability_id = $1
	`, capabilityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.IntegrationCapability
	for rows.Next() {
		var i domain.IntegrationCapability
		if err := rows.Scan(
			&i.IntegrationCapabilityID, &i.CapabilityID, &i.ProviderCode, &i.Certified,
			&i.HealthStatus, &i.CreatedAt, &i.UpdatedAt, &i.CreatedByPrincipalID,
		); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *PgStore) UpdateIntegrationHealth(ctx context.Context, integrationCapabilityID, healthStatus string) error {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE integration_capabilities SET health_status = $1, updated_at = $2
		WHERE integration_capability_id = $3
	`, healthStatus, now, integrationCapabilityID)
	if err != nil {
		return fmt.Errorf("update integration health: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrIntegrationCapabilityNotFound
	}
	return nil
}

func (s *PgStore) CreateRelease(ctx context.Context, r *domain.Release) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO releases (
			release_id, capability_id, state, reason, effective_from,
			created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, r.ReleaseID, r.CapabilityID, string(r.State), r.Reason, r.EffectiveFrom,
		r.CreatedAt, r.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert release: %w", err)
	}
	return nil
}

// GetCurrentRelease returns the most recent release row for capabilityID —
// releases are append-only (see migration comment), so "current" means
// latest by effective_from, not the result of any UPDATE.
func (s *PgStore) GetCurrentRelease(ctx context.Context, capabilityID string) (*domain.Release, error) {
	var r domain.Release
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT release_id, capability_id, state, reason, effective_from,
		       created_at, created_by_principal_id
		FROM releases WHERE capability_id = $1
		ORDER BY effective_from DESC LIMIT 1
	`, capabilityID).Scan(
		&r.ReleaseID, &r.CapabilityID, &state, &r.Reason, &r.EffectiveFrom,
		&r.CreatedAt, &r.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrReleaseNotFound
	}
	if err != nil {
		return nil, err
	}
	r.State = domain.ReleaseState(state)
	return &r, nil
}

func (s *PgStore) CreateCapabilityClaim(ctx context.Context, c *domain.CapabilityClaim) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO capability_claims (
			claim_id, capability_id, claim_text, market_scope, wording_owner_principal_id,
			approved_by_principal_id, expiry_review_date, created_at, created_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, c.ClaimID, c.CapabilityID, c.ClaimText, c.MarketScope, c.WordingOwnerPrincipalID,
		c.ApprovedByPrincipalID, c.ExpiryReviewDate, c.CreatedAt, c.CreatedByPrincipalID,
	)
	if err != nil {
		return fmt.Errorf("insert capability claim: %w", err)
	}
	return nil
}

func (s *PgStore) ListClaimsByCapability(ctx context.Context, capabilityID string) ([]domain.CapabilityClaim, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT claim_id, capability_id, claim_text, market_scope, wording_owner_principal_id,
		       approved_by_principal_id, expiry_review_date, created_at, created_by_principal_id
		FROM capability_claims WHERE capability_id = $1
		ORDER BY created_at DESC
	`, capabilityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.CapabilityClaim
	for rows.Next() {
		var c domain.CapabilityClaim
		if err := rows.Scan(
			&c.ClaimID, &c.CapabilityID, &c.ClaimText, &c.MarketScope, &c.WordingOwnerPrincipalID,
			&c.ApprovedByPrincipalID, &c.ExpiryReviewDate, &c.CreatedAt, &c.CreatedByPrincipalID,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveCapability composes this service's own three dimensions into the
// structured reason-code answer doc7 §C1 requires: capability existence,
// operational release state, and market availability. See the domain
// package doc comment for why commercial entitlement and security/privacy
// eligibility are deliberately NOT composed in here.
func (s *PgStore) ResolveCapability(ctx context.Context, capabilityCode, marketCode string) (*domain.CapabilityResolution, error) {
	cap, err := s.GetCapabilityByCode(ctx, capabilityCode)
	if err != nil {
		if errors.Is(err, domain.ErrCapabilityNotFound) {
			return &domain.CapabilityResolution{
				CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "CAPABILITY_UNKNOWN",
			}, nil
		}
		return nil, err
	}

	release, err := s.GetCurrentRelease(ctx, cap.CapabilityID)
	if err != nil && !errors.Is(err, domain.ErrReleaseNotFound) {
		return nil, err
	}
	if release != nil {
		switch release.State {
		case domain.ReleaseStateDisabled:
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "DISABLED"}, nil
		case domain.ReleaseStateIncidentRestricted:
			detail := ""
			if release.Reason != nil {
				detail = *release.Reason
			}
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "INCIDENT_RESTRICTED", Detail: detail}, nil
		}
	}

	if marketCode != "" {
		marketRelease, err := s.GetActiveMarketRelease(ctx, cap.CapabilityID, marketCode)
		if err != nil && !errors.Is(err, domain.ErrMarketReleaseNotFound) {
			return nil, err
		}
		if marketRelease == nil {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "MARKET_BLOCKED", Detail: "no active market release for " + marketCode}, nil
		}
		switch marketRelease.State {
		case domain.MarketReleaseRestricted, domain.MarketReleaseSuspended, domain.MarketReleaseRetired:
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "MARKET_BLOCKED", Detail: string(marketRelease.State)}, nil
		}
	}

	integrations, err := s.ListIntegrationCapabilitiesByCapability(ctx, cap.CapabilityID)
	if err != nil {
		return nil, err
	}
	for _, integ := range integrations {
		if !integ.Certified || integ.HealthStatus == "FAILED" {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "PROVIDER_UNAVAILABLE", Detail: integ.ProviderCode}, nil
		}
	}

	return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: true, ReasonCode: "ENABLED"}, nil
}
