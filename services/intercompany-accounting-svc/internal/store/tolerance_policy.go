package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

// Tolerance policy governance (migration 000011; SPEC_DEVIATIONS A5's
// tolerance/matching-policy gap, B15's deferred half). See the migration's
// own comment for the full reasoning: this turns the fixed 0.001 literal
// into a governed, versioned object without changing its value.

// defaultTolerance/defaultToleranceVersion are the fixed pre-governance
// tolerance and its sentinel version (0): what ResolveTolerancePolicy
// returns for a tenant that has never published a version effective by the
// instant asked for. Never a stored row — publishing this migration changes
// no tenant's match outcome until that tenant actually opts in.
const defaultTolerance = "0.001"
const defaultToleranceVersion = 0

// PublishTolerancePolicy records a new, immutable tolerance policy version
// for tenantID: who published it, why, and when it takes effect. The version
// number is caller-chosen (not a SERIAL), so a repeat publish for the same
// version is a conflict, not a silent second row — the same posture
// commercial-account-svc's entitlement_policy_versions takes.
func (s *PgStore) PublishTolerancePolicy(ctx context.Context, tenantID string, v *domain.TolerancePolicyVersion) (*domain.TolerancePolicyVersion, error) {
	var out *domain.TolerancePolicyVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO intercompany_tolerance_policy_versions
				(tenant_id, policy_version, tolerance, effective_from, reason, created_at, created_by_principal_id)
			VALUES ($1, $2, $3::numeric, $4, $5, $6, $7)
			RETURNING tolerance::text, created_at`,
			tenantID, v.PolicyVersion, string(v.Tolerance), v.EffectiveFrom, v.Reason, v.CreatedAt, v.CreatedByPrincipalID)
		var tolerance string
		if err := row.Scan(&tolerance, &v.CreatedAt); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.ErrTolerancePolicyVersionExists
			}
			return err
		}
		v.TenantID = tenantID
		v.Tolerance = domain.Decimal(tolerance)
		out = v
		return nil
	})
	return out, err
}

// ResolveTolerancePolicy returns the tolerance version effective for
// tenantID at instant `at`: the latest published
// intercompany_tolerance_policy_versions row whose effective_from is at or
// before `at`, or the fixed pre-governance 0.001 at version 0 when the
// tenant has never published one (or none is effective yet).
func (s *PgStore) ResolveTolerancePolicy(ctx context.Context, tenantID string, at time.Time) (int, domain.Decimal, error) {
	version := defaultToleranceVersion
	tolerance := defaultTolerance
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT policy_version, tolerance::text FROM intercompany_tolerance_policy_versions
			WHERE tenant_id = $1 AND effective_from <= $2
			ORDER BY effective_from DESC, policy_version DESC LIMIT 1`, tenantID, at).Scan(&version, &tolerance)
		if errors.Is(err, pgx.ErrNoRows) {
			version, tolerance = defaultToleranceVersion, defaultTolerance
			return nil
		}
		return err
	})
	return version, domain.Decimal(tolerance), err
}
