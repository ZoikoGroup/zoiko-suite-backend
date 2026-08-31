// Package store implements payment-authorization-svc's persistence.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/middleware"
)

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type nullString struct{ dest *string }

func (n *nullString) Scan(src interface{}) error {
	if src == nil {
		*n.dest = ""
		return nil
	}
	s, _ := src.(string)
	*n.dest = s
	return nil
}

// Store is the interface the handler depends on.
type Store interface {
	RequestAuthorization(ctx context.Context, tenantID string, auth domain.PaymentAuthorization, snapshots []domain.PayeeSnapshot) (*domain.PaymentAuthorization, error)
	FindAuthorization(ctx context.Context, authorizationID string) (*domain.PaymentAuthorization, error)
	ListPayeeSnapshots(ctx context.Context, authorizationID string) ([]domain.PayeeSnapshot, error)

	ApproveAuthorization(ctx context.Context, authorizationID, policyResult, policyVersionID, principalID string) (*domain.PaymentAuthorization, error)
	RejectAuthorization(ctx context.Context, authorizationID string, req domain.RejectPaymentRequest, principalID string) (*domain.PaymentAuthorization, error)
	InvalidateAuthorization(ctx context.Context, authorizationID, reason string) (*domain.PaymentAuthorization, error)
	ConsumeAuthorization(ctx context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error)
	RevokeAuthorization(ctx context.Context, authorizationID string, req domain.RevokeAuthorizationRequest, principalID string) (*domain.PaymentAuthorization, error)
	ExpireAuthorization(ctx context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error)

	ListEvents(ctx context.Context, authorizationID string) ([]domain.AuthorizationEvent, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", middleware.TenantFromContext(ctx)); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, authorizationID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO authorization_events (event_id, tenant_id, authorization_id, event_type, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), tenantID, authorizationID, eventType, detail, actorPrincipalID,
	)
	return err
}

// ── authorizations ───────────────────────────────────────────────────────────

const authColumns = `
	authorization_id, tenant_id, legal_entity_id, proposal_id, proposal_fingerprint, net_amount, currency,
	status, policy_assessment_result, policy_version_id, requested_by_principal_id,
	approved_by_principal_id, approved_at, rejected_reason,
	revoked_by_principal_id, revoked_reason, revoked_at, expired_at,
	consumed_by_principal_id, consumed_at, invalidated_reason, created_at, updated_at`

func scanAuth(row pgx.Row) (*domain.PaymentAuthorization, error) {
	a := &domain.PaymentAuthorization{}
	err := row.Scan(&a.AuthorizationID, &a.TenantID, &a.LegalEntityID, &a.ProposalID, &a.ProposalFingerprint,
		&a.NetAmount, &a.Currency, &a.Status, &nullString{&a.PolicyAssessmentResult}, &nullString{&a.PolicyVersionID},
		&a.RequestedByPrincipalID, &a.ApprovedByPrincipalID, &a.ApprovedAt, &nullString{&a.RejectedReason},
		&a.RevokedByPrincipalID, &nullString{&a.RevokedReason}, &a.RevokedAt, &a.ExpiredAt,
		&a.ConsumedByPrincipalID, &a.ConsumedAt, &nullString{&a.InvalidatedReason}, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// RequestAuthorization relies on the migration's partial unique index to
// reject a second active (PENDING/APPROVED) authorization against the same
// proposal — a real database invariant beyond AP-10's own four named
// negative-path scenarios.
func (s *PgStore) RequestAuthorization(ctx context.Context, tenantID string, auth domain.PaymentAuthorization, snapshots []domain.PayeeSnapshot) (*domain.PaymentAuthorization, error) {
	id := uuid.New().String()
	var out *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanAuth(tx.QueryRow(ctx, `
			INSERT INTO payment_authorizations (`+authColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', '', '', $8, NULL, NULL, '', NULL, '', NULL, NULL, NULL, NULL, '', NOW(), NOW())
			RETURNING `+authColumns,
			id, strPtrOrNil(tenantID), auth.LegalEntityID, auth.ProposalID, auth.ProposalFingerprint,
			auth.NetAmount, auth.Currency, auth.RequestedByPrincipalID,
		))
		if err != nil {
			return err
		}
		for _, snap := range snapshots {
			if _, err := tx.Exec(ctx, `
				INSERT INTO authorization_payee_snapshots (snapshot_id, tenant_id, authorization_id, payee_ref, payee_snapshot_at)
				VALUES ($1, $2, $3, $4, $5)`,
				uuid.New().String(), out.TenantID, id, snap.PayeeRef, snap.PayeeSnapshotAt,
			); err != nil {
				return err
			}
		}
		return s.recordEvent(ctx, tx, out.TenantID, id, domain.EventAuthorizationRequested, auth.ProposalID, auth.RequestedByPrincipalID)
	})
	if isUniqueViolation(err) {
		return nil, domain.ErrProposalAlreadyRequested
	}
	if err != nil {
		s.log.Error("pg RequestAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) FindAuthorization(ctx context.Context, authorizationID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `SELECT `+authColumns+` FROM payment_authorizations WHERE authorization_id = $1`, authorizationID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrAuthorizationNotFound
	}
	if err != nil {
		s.log.Error("pg FindAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) ListPayeeSnapshots(ctx context.Context, authorizationID string) ([]domain.PayeeSnapshot, error) {
	var out []domain.PayeeSnapshot
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT snapshot_id, tenant_id, authorization_id, payee_ref, payee_snapshot_at, created_at
			FROM authorization_payee_snapshots WHERE authorization_id = $1 ORDER BY created_at ASC`, authorizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var snap domain.PayeeSnapshot
			if err := rows.Scan(&snap.SnapshotID, &snap.TenantID, &snap.AuthorizationID, &snap.PayeeRef, &snap.PayeeSnapshotAt, &snap.CreatedAt); err != nil {
				return err
			}
			out = append(out, snap)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListPayeeSnapshots failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── lifecycle ────────────────────────────────────────────────────────────────

func (s *PgStore) ApproveAuthorization(ctx context.Context, authorizationID, policyResult, policyVersionID, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'APPROVED', policy_assessment_result = $2, policy_version_id = $3,
				approved_by_principal_id = $4, approved_at = NOW(), updated_at = NOW()
			WHERE authorization_id = $1 AND status = 'PENDING'
			RETURNING `+authColumns,
			authorizationID, policyResult, policyVersionID, principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventPaymentAuthorized, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ApproveAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) RejectAuthorization(ctx context.Context, authorizationID string, req domain.RejectPaymentRequest, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'REJECTED', rejected_reason = $2, updated_at = NOW()
			WHERE authorization_id = $1 AND status = 'PENDING'
			RETURNING `+authColumns,
			authorizationID, req.Reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationRejected, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RejectAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// InvalidateAuthorization moves a PENDING or APPROVED authorization to
// INVALIDATED — the literal enforcement of negative-path scenario #1's
// outcome ("any protected-field mismatch invalidates"), reachable from
// either the ApprovePayment or ConsumePaymentAuthorization checkpoint.
func (s *PgStore) InvalidateAuthorization(ctx context.Context, authorizationID, reason string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'INVALIDATED', invalidated_reason = $2, updated_at = NOW()
			WHERE authorization_id = $1 AND status IN ('PENDING', 'APPROVED')
			RETURNING `+authColumns,
			authorizationID, reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationInvalidated, reason, "system")
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg InvalidateAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// ConsumeAuthorization's WHERE status = 'APPROVED' guard is the first layer
// of negative-path scenario #4's replay protection — the migration's
// terminal-status trigger is the second, permanent layer.
func (s *PgStore) ConsumeAuthorization(ctx context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'CONSUMED', consumed_by_principal_id = $2, consumed_at = NOW(), updated_at = NOW()
			WHERE authorization_id = $1 AND status = 'APPROVED'
			RETURNING `+authColumns,
			authorizationID, principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationConsumed, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ConsumeAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) RevokeAuthorization(ctx context.Context, authorizationID string, req domain.RevokeAuthorizationRequest, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'REVOKED', revoked_by_principal_id = $2, revoked_reason = $3, revoked_at = NOW(), updated_at = NOW()
			WHERE authorization_id = $1 AND status IN ('PENDING', 'APPROVED')
			RETURNING `+authColumns,
			authorizationID, principalID, req.Reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationRevoked, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RevokeAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) ExpireAuthorization(ctx context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'EXPIRED', expired_at = NOW(), updated_at = NOW()
			WHERE authorization_id = $1 AND status IN ('PENDING', 'APPROVED')
			RETURNING `+authColumns,
			authorizationID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationExpired, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ExpireAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// ── events ───────────────────────────────────────────────────────────────────

func (s *PgStore) ListEvents(ctx context.Context, authorizationID string) ([]domain.AuthorizationEvent, error) {
	var out []domain.AuthorizationEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, authorization_id, event_type, detail, actor_principal_id, created_at
			FROM authorization_events WHERE authorization_id = $1 ORDER BY created_at ASC`, authorizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.AuthorizationEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.AuthorizationID, &e.EventType, &nullString{&e.Detail}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListEvents failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
