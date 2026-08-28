package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/identity-context-svc/internal/domain"
)

// InsertSessionContext writes the durable evidence record for an issued
// session.
//
// Idempotent on session_context_id. The id is a fresh ULID per resolution so a
// natural collision is not expected; the ON CONFLICT is there because the
// caller writes Redis and Postgres on the same call and a retry of that call
// must not fail on the half that already succeeded.
func (s *PgStore) InsertSessionContext(ctx context.Context, sc domain.SessionContext) error {
	if sc.TenantID == "" {
		return errors.New("InsertSessionContext: tenant_id is required")
	}
	return s.withRLS(ctx, sc.TenantID, func(tx pgx.Tx) error {
		var legalEntityID *string
		if sc.LegalEntityID != "" {
			legalEntityID = &sc.LegalEntityID
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO session_contexts (
				session_context_id, principal_id, tenant_id, legal_entity_id,
				correlation_id, trust_posture, mfa_verified, device_trust_score,
				adaptive_risk_score, risk_signal_source, envelope_jwt_jti,
				issued_at, expires_at, data_residency_policy_id,
				source_service, schema_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (session_context_id) DO NOTHING`,
			sc.SessionContextID, sc.PrincipalID, sc.TenantID, legalEntityID,
			sc.CorrelationID, string(sc.TrustPosture), sc.MFAVerified, sc.DeviceTrustScore,
			sc.AdaptiveRiskScore, sc.RiskSignalSource, sc.EnvelopeJWTJTI,
			sc.IssuedAt, sc.ExpiresAt, sc.DataResidencyPolicyID,
			sc.SourceService, sc.SchemaVersion,
		)
		if err != nil {
			return fmt.Errorf("insert session_context: %w", err)
		}
		return nil
	})
}

// MarkSessionInvalidated appends the invalidation to the durable record.
//
// The WHERE clause carries `invalidated_at IS NULL`, so the FIRST invalidation
// wins and a later one is a no-op rather than an overwrite. That is what makes
// the column append-only in practice: a session revoked for RISK_ESCALATION and
// then logged out of must not have its reason rewritten to LOGOUT, because the
// reason is the part an investigation actually reads.
func (s *PgStore) MarkSessionInvalidated(
	ctx context.Context,
	sessionContextID, tenantID string,
	reason domain.InvalidationReason,
	at time.Time,
) error {
	if tenantID == "" {
		return errors.New("MarkSessionInvalidated: tenant_id is required")
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE session_contexts
			   SET invalidated_at = $1, invalidation_reason = $2
			 WHERE session_context_id = $3
			   AND tenant_id = $4
			   AND invalidated_at IS NULL`,
			at, string(reason), sessionContextID, tenantID,
		)
		if err != nil {
			return fmt.Errorf("mark session_context invalidated: %w", err)
		}
		return nil
	})
}

// FindSessionContext reads the durable record. Returns (nil, nil) when absent.
//
// The hot path reads Redis; this is the fallback for a session whose cache
// entry has aged out. Without it, invalidating an expired-from-cache session
// looked like success while writing nothing, because the resolver treats a
// missing record as an idempotent no-op.
func (s *PgStore) FindSessionContext(
	ctx context.Context,
	sessionContextID, tenantID string,
) (*domain.SessionContext, error) {
	if tenantID == "" {
		return nil, errors.New("FindSessionContext: tenant_id is required")
	}
	var sc domain.SessionContext
	var legalEntityID *string
	var reason *string

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT session_context_id, principal_id, tenant_id, legal_entity_id,
			       correlation_id, trust_posture, mfa_verified, device_trust_score,
			       adaptive_risk_score, risk_signal_source, envelope_jwt_jti,
			       issued_at, expires_at, invalidated_at, invalidation_reason,
			       data_residency_policy_id, source_service, schema_version
			  FROM session_contexts
			 WHERE session_context_id = $1 AND tenant_id = $2`,
			sessionContextID, tenantID,
		).Scan(
			&sc.SessionContextID, &sc.PrincipalID, &sc.TenantID, &legalEntityID,
			&sc.CorrelationID, &sc.TrustPosture, &sc.MFAVerified, &sc.DeviceTrustScore,
			&sc.AdaptiveRiskScore, &sc.RiskSignalSource, &sc.EnvelopeJWTJTI,
			&sc.IssuedAt, &sc.ExpiresAt, &sc.InvalidatedAt, &reason,
			&sc.DataResidencyPolicyID, &sc.SourceService, &sc.SchemaVersion,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find session_context: %w", err)
	}

	if legalEntityID != nil {
		sc.LegalEntityID = *legalEntityID
	}
	if reason != nil {
		r := domain.InvalidationReason(*reason)
		sc.InvalidationReason = &r
	}
	return &sc, nil
}

// FindLiveSessionIDsForPrincipal returns the sessions that are neither expired
// nor already invalidated.
//
// This is the durable counterpart to the Redis reverse index, and exists so a
// revocation can still find sessions to mark when the cache has been flushed or
// the process that issued them has been replaced. It returns ids only: the
// caller is revoking, not reading session state.
func (s *PgStore) FindLiveSessionIDsForPrincipal(
	ctx context.Context,
	principalID, tenantID string,
	now time.Time,
) ([]string, error) {
	if tenantID == "" {
		return nil, errors.New("FindLiveSessionIDsForPrincipal: tenant_id is required")
	}
	var ids []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT session_context_id
			  FROM session_contexts
			 WHERE principal_id = $1
			   AND tenant_id = $2
			   AND invalidated_at IS NULL
			   AND expires_at > $3`,
			principalID, tenantID, now,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("find live sessions for principal: %w", err)
	}
	return ids, nil
}

// FindPrincipalIDsByRole returns the principals currently holding an active
// assignment to a role.
//
// role.updated changes what a role GRANTS, and envelopes already issued carry
// the permission bundles it granted before. Finding who holds it is the first
// half of revoking those; the caller does the revoking.
//
// "Currently" is the assignment's own effective window, matching
// FindActiveRoleAssignments — an assignment that has not started or has already
// ended grants nothing, so a session held by its principal is not stale on
// account of this role.
func (s *PgStore) FindPrincipalIDsByRole(
	ctx context.Context,
	roleID, tenantID string,
	now time.Time,
) ([]string, error) {
	if tenantID == "" {
		return nil, errors.New("FindPrincipalIDsByRole: tenant_id is required")
	}
	var ids []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT principal_id
			  FROM principal_role_assignments
			 WHERE role_id = $1
			   AND tenant_id = $2
			   AND effective_from <= $3
			   AND effective_to   >  $3`,
			roleID, tenantID, now,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("find principals by role: %w", err)
	}
	return ids, nil
}
