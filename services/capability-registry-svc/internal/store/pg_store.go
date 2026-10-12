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

// isForeignKeyViolation reports whether err is a Postgres foreign-key
// violation (23503) — e.g. a market release, integration capability, or
// claim referencing a capability_id that does not exist. Callers map this to
// domain.ErrCapabilityNotFound so it surfaces as the same 404 GetCapability
// already returns for a missing capability, instead of a generic 500 that
// exposes a raw database error.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// isInvalidTextRepresentation reports whether err is Postgres code 22P02 —
// e.g. a capability_id path segment that isn't even syntactically a UUID
// (every capability_id column in this service is typed uuid). This is a
// distinct failure mode from isForeignKeyViolation (23503, a well-formed UUID
// that doesn't exist): Postgres rejects a malformed UUID literal before it
// ever reaches the foreign-key check. Both map to the same client-visible
// 404 "capability not found" — from the caller's perspective, a capability_id
// that isn't a real UUID can't possibly reference a real capability either.
func isInvalidTextRepresentation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

type Store interface {
	CreateCapability(ctx context.Context, c *domain.Capability, claim domain.IdempotencyClaim) error
	GetCapability(ctx context.Context, capabilityID string) (*domain.Capability, error)
	GetCapabilityByCode(ctx context.Context, capabilityCode string) (*domain.Capability, error)

	CreateMarketRelease(ctx context.Context, m *domain.MarketRelease, claim domain.IdempotencyClaim) error
	GetActiveMarketRelease(ctx context.Context, capabilityID, marketCode string) (*domain.MarketRelease, error)
	GetMarketReleaseByID(ctx context.Context, marketReleaseID string) (*domain.MarketRelease, error)

	CreateIntegrationCapability(ctx context.Context, i *domain.IntegrationCapability, claim domain.IdempotencyClaim) error
	ListIntegrationCapabilitiesByCapability(ctx context.Context, capabilityID string) ([]domain.IntegrationCapability, error)
	GetIntegrationCapabilityByID(ctx context.Context, integrationCapabilityID string) (*domain.IntegrationCapability, error)
	UpdateIntegrationHealth(ctx context.Context, integrationCapabilityID, healthStatus string, claim domain.IdempotencyClaim) error

	CreateRelease(ctx context.Context, r *domain.Release, claim domain.IdempotencyClaim) error
	GetCurrentRelease(ctx context.Context, capabilityID string) (*domain.Release, error)
	GetReleaseByID(ctx context.Context, releaseID string) (*domain.Release, error)

	CreateCapabilityClaim(ctx context.Context, c *domain.CapabilityClaim, claim domain.IdempotencyClaim) error
	ListClaimsByCapability(ctx context.Context, capabilityID string) ([]domain.CapabilityClaim, error)
	GetCapabilityClaimByID(ctx context.Context, claimID string) (*domain.CapabilityClaim, error)

	ResolveCapability(ctx context.Context, capabilityCode, marketCode string) (*domain.CapabilityResolution, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// claimIdempotency is INV-08's actual enforcement point, run inside the same
// transaction as the write it guards (commercial-account-svc's
// commercial_idempotency_keys/claimIdempotency is the sibling-service
// precedent this follows). The claim and the write either both land or
// both roll back — a claimed-but-failed write can never permanently block
// a legitimate retry.
//
//   - Row inserted (claimed): nil. Caller proceeds with the real write,
//     using the same ResourceID already reserved in the claim.
//   - Row already present, same request_sha256: *domain.IdempotentReplayError
//     — this exact request was already processed; the caller returns the
//     persisted original response, creating nothing new.
//   - Row already present, different request_sha256: domain.ErrIdempotencyKeyReused
//     — the same key was reused for a materially different request; refused.
func claimIdempotency(ctx context.Context, tx pgx.Tx, c domain.IdempotencyClaim) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (
			tenant_id, operation, principal_id, idempotency_key, request_sha256,
			resource_id, response_status, response_body
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, operation, principal_id, idempotency_key) DO NOTHING`,
		c.TenantID, c.Operation, c.PrincipalID, c.Key, c.RequestSHA256,
		c.ResourceID, c.ResponseStatus, c.ResponseBody)
	if err != nil {
		return fmt.Errorf("claim idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existingHash, existingResourceID string
	var existingStatus int
	var existingBody []byte
	if err := tx.QueryRow(ctx, `
		SELECT request_sha256, resource_id, response_status, response_body FROM idempotency_keys
		WHERE tenant_id = $1 AND operation = $2 AND principal_id = $3 AND idempotency_key = $4`,
		c.TenantID, c.Operation, c.PrincipalID, c.Key).
		Scan(&existingHash, &existingResourceID, &existingStatus, &existingBody); err != nil {
		return fmt.Errorf("read idempotency claim: %w", err)
	}
	if existingHash != c.RequestSHA256 {
		return domain.ErrIdempotencyKeyReused
	}
	return &domain.IdempotentReplayError{
		ResourceID: existingResourceID, ResponseStatus: existingStatus, ResponseBody: existingBody,
	}
}

// withTx runs fn inside a transaction, committing on success and rolling
// back otherwise — including on an *domain.IdempotentReplayError or
// domain.ErrIdempotencyKeyReused return from claimIdempotency, since
// neither of those should leave a half-applied write behind.
func (s *PgStore) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback error discarded intentionally on the commit path
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertOutboxEvent(ctx context.Context, tx pgx.Tx, claim domain.IdempotencyClaim) error {
	if claim.OutboxEventID == "" || len(claim.OutboxPayload) == 0 {
		return errors.New("outbox event is missing its ID or payload")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (outbox_event_id, entity_id, payload)
		VALUES ($1, $2, $3)
	`, claim.OutboxEventID, claim.OutboxEntityID, claim.OutboxPayload)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

func (s *PgStore) CreateCapability(ctx context.Context, c *domain.Capability, claim domain.IdempotencyClaim) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
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
		return insertOutboxEvent(ctx, tx, claim)
	})
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

func (s *PgStore) CreateMarketRelease(ctx context.Context, m *domain.MarketRelease, claim domain.IdempotencyClaim) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
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
			if isForeignKeyViolation(err) || isInvalidTextRepresentation(err) {
				return fmt.Errorf("%w: capability_id %s", domain.ErrCapabilityNotFound, m.CapabilityID)
			}
			return fmt.Errorf("insert market release: %w", err)
		}
		return insertOutboxEvent(ctx, tx, claim)
	})
}

func (s *PgStore) GetMarketReleaseByID(ctx context.Context, marketReleaseID string) (*domain.MarketRelease, error) {
	var m domain.MarketRelease
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT market_release_id, capability_id, market_code, language_code,
		       legal_approval_status, state, effective_from, effective_to,
		       created_at, created_by_principal_id
		FROM market_releases WHERE market_release_id = $1
	`, marketReleaseID).Scan(
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

func (s *PgStore) CreateIntegrationCapability(ctx context.Context, i *domain.IntegrationCapability, claim domain.IdempotencyClaim) error {
	if i.HealthStatus == "" {
		i.HealthStatus = "UNKNOWN"
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
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
			if isForeignKeyViolation(err) || isInvalidTextRepresentation(err) {
				return fmt.Errorf("%w: capability_id %s", domain.ErrCapabilityNotFound, i.CapabilityID)
			}
			return fmt.Errorf("insert integration capability: %w", err)
		}
		return insertOutboxEvent(ctx, tx, claim)
	})
}

func (s *PgStore) GetIntegrationCapabilityByID(ctx context.Context, integrationCapabilityID string) (*domain.IntegrationCapability, error) {
	var i domain.IntegrationCapability
	err := s.pool.QueryRow(ctx, `
		SELECT integration_capability_id, capability_id, provider_code, certified,
		       health_status, created_at, updated_at, created_by_principal_id
		FROM integration_capabilities WHERE integration_capability_id = $1
	`, integrationCapabilityID).Scan(
		&i.IntegrationCapabilityID, &i.CapabilityID, &i.ProviderCode, &i.Certified,
		&i.HealthStatus, &i.CreatedAt, &i.UpdatedAt, &i.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrIntegrationCapabilityNotFound
	}
	return &i, err
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

func (s *PgStore) UpdateIntegrationHealth(ctx context.Context, integrationCapabilityID, healthStatus string, claim domain.IdempotencyClaim) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		now := time.Now().UTC()
		tag, err := tx.Exec(ctx, `
			UPDATE integration_capabilities SET health_status = $1, updated_at = $2
			WHERE integration_capability_id = $3
		`, healthStatus, now, integrationCapabilityID)
		if err != nil {
			return fmt.Errorf("update integration health: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrIntegrationCapabilityNotFound
		}
		return insertOutboxEvent(ctx, tx, claim)
	})
}

func (s *PgStore) CreateRelease(ctx context.Context, r *domain.Release, claim domain.IdempotencyClaim) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
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
		return insertOutboxEvent(ctx, tx, claim)
	})
}

func (s *PgStore) GetReleaseByID(ctx context.Context, releaseID string) (*domain.Release, error) {
	var r domain.Release
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT release_id, capability_id, state, reason, effective_from,
		       created_at, created_by_principal_id
		FROM releases WHERE release_id = $1
	`, releaseID).Scan(
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

func (s *PgStore) CreateCapabilityClaim(ctx context.Context, c *domain.CapabilityClaim, claim domain.IdempotencyClaim) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := claimIdempotency(ctx, tx, claim); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO capability_claims (
				claim_id, capability_id, claim_text, market_scope, wording_owner_principal_id,
				approved_by_principal_id, expiry_review_date, created_at, created_by_principal_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, c.ClaimID, c.CapabilityID, c.ClaimText, c.MarketScope, c.WordingOwnerPrincipalID,
			c.ApprovedByPrincipalID, c.ExpiryReviewDate, c.CreatedAt, c.CreatedByPrincipalID,
		)
		if err != nil {
			if isForeignKeyViolation(err) || isInvalidTextRepresentation(err) {
				return fmt.Errorf("%w: capability_id %s", domain.ErrCapabilityNotFound, c.CapabilityID)
			}
			return fmt.Errorf("insert capability claim: %w", err)
		}
		return insertOutboxEvent(ctx, tx, claim)
	})
}

func (s *PgStore) GetCapabilityClaimByID(ctx context.Context, claimID string) (*domain.CapabilityClaim, error) {
	var c domain.CapabilityClaim
	err := s.pool.QueryRow(ctx, `
		SELECT claim_id, capability_id, claim_text, market_scope, wording_owner_principal_id,
		       approved_by_principal_id, expiry_review_date, created_at, created_by_principal_id
		FROM capability_claims WHERE claim_id = $1
	`, claimID).Scan(
		&c.ClaimID, &c.CapabilityID, &c.ClaimText, &c.MarketScope, &c.WordingOwnerPrincipalID,
		&c.ApprovedByPrincipalID, &c.ExpiryReviewDate, &c.CreatedAt, &c.CreatedByPrincipalID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrClaimNotFound
	}
	return &c, err
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
		case domain.ReleaseStateGA, domain.ReleaseStateBeta, domain.ReleaseStatePilot, domain.ReleaseStateInternal:
			// Recognized, non-blocking states — fall through to the next dimension.
		default:
			// Handler-layer validation (added alongside this check) should make
			// this unreachable for new rows, but it closes the fail-open gap for
			// any row written before that validation existed: an unrecognized
			// release state must never be silently treated as enabled.
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "DISABLED", Detail: "unrecognized release state: " + string(release.State)}, nil
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
		case domain.MarketReleaseInternal, domain.MarketReleasePilot, domain.MarketReleaseBeta, domain.MarketReleaseGA:
			// Recognized, non-blocking states — fall through to the next dimension.
		default:
			// Same fail-closed defense as the release-state switch above.
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "MARKET_BLOCKED", Detail: "unrecognized market release state: " + string(marketRelease.State)}, nil
		}
	}

	integrations, err := s.ListIntegrationCapabilitiesByCapability(ctx, cap.CapabilityID)
	if err != nil {
		return nil, err
	}
	for _, integ := range integrations {
		if !integ.Certified || !domain.IntegrationHealthStatus(integ.HealthStatus).Valid() || integ.HealthStatus == string(domain.HealthStatusFailed) {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "PROVIDER_UNAVAILABLE", Detail: integ.ProviderCode}, nil
		}
	}

	return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: true, ReasonCode: "ENABLED"}, nil
}
