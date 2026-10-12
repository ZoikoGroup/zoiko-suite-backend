package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// ── GOV-04 compensating-control exceptions (000027) ─────────────────────────

const sodExceptionColumns = `sod_exception_id, tenant_id, sod_rule_id, principal_id, compensating_control, reason, status,
	requested_by, approved_by, decided_at, effective_from, expires_at, revoked_by, revoked_at, created_at`

func scanSoDException(row pgx.Row) (*domain.SoDException, error) {
	e := &domain.SoDException{}
	err := row.Scan(&e.SoDExceptionID, &e.TenantID, &e.SoDRuleID, &e.PrincipalID, &e.CompensatingControl, &e.Reason, &e.Status,
		&e.RequestedBy, &e.ApprovedBy, &e.DecidedAt, &e.EffectiveFrom, &e.ExpiresAt, &e.RevokedBy, &e.RevokedAt, &e.CreatedAt)
	return e, err
}

// CreateSoDException records a REQUESTED exception for a rule visible in the
// tenant (its own or a platform-wide one). ErrSoDRuleNotFound otherwise.
func (s *PgStore) CreateSoDException(ctx context.Context, p domain.CreateSoDExceptionParams) (*domain.SoDException, error) {
	const query = `
		INSERT INTO sod_exceptions (tenant_id, sod_rule_id, principal_id, compensating_control, reason, requested_by, effective_from, expires_at)
		SELECT $1::uuid, sr.sod_rule_id, $3, $4, $5, $6, COALESCE($7, NOW()), $8
		  FROM sod_rules sr
		 WHERE sr.sod_rule_id = $2::uuid AND (sr.tenant_id IS NULL OR sr.tenant_id = $1::uuid)
		RETURNING ` + sodExceptionColumns
	var e *domain.SoDException
	err := s.withRLS(ctx, p.TenantID, func(tx pgx.Tx) error {
		var scanErr error
		e, scanErr = scanSoDException(tx.QueryRow(ctx, query, p.TenantID, p.SoDRuleID, p.PrincipalID, p.CompensatingControl,
			p.Reason, p.RequestedBy, p.EffectiveFrom, p.ExpiresAt))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSoDRuleNotFound
	}
	if err != nil {
		s.log.Error("pg CreateSoDException failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return e, nil
}

// FindSoDException reads one exception in the tenant.
func (s *PgStore) FindSoDException(ctx context.Context, id, tenantID string) (*domain.SoDException, error) {
	const query = `SELECT ` + sodExceptionColumns + ` FROM sod_exceptions WHERE sod_exception_id = $1::uuid AND tenant_id = $2::uuid`
	var e *domain.SoDException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		e, scanErr = scanSoDException(tx.QueryRow(ctx, query, id, tenantID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrSoDExceptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return e, nil
}

// ListSoDExceptions lists the tenant's exceptions, newest first, optionally
// for one principal and/or status.
func (s *PgStore) ListSoDExceptions(ctx context.Context, tenantID, principalID, status string) ([]domain.SoDException, error) {
	const query = `SELECT ` + sodExceptionColumns + ` FROM sod_exceptions
		 WHERE tenant_id = $1::uuid AND ($2 = '' OR principal_id = $2) AND ($3 = '' OR status = $3)
		 ORDER BY created_at DESC LIMIT 500`
	out := []domain.SoDException{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, tenantID, principalID, status)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanSoDException(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// TransitionSoDException moves an exception from one state to another in one
// guarded UPDATE: REQUESTED -> APPROVED | REJECTED (actor = approver), or
// APPROVED -> REVOKED (actor = revoker). ErrSoDExceptionState if it was not in
// `from` any more (a racing decision), ErrSoDExceptionNotFound if absent.
func (s *PgStore) TransitionSoDException(ctx context.Context, id, tenantID, from, to, actor string) (*domain.SoDException, error) {
	const query = `
		UPDATE sod_exceptions
		   SET status = $4::text,
		       approved_by = CASE WHEN $4::text IN ('APPROVED', 'REJECTED') THEN $5::text ELSE approved_by END,
		       decided_at  = CASE WHEN $4::text IN ('APPROVED', 'REJECTED') THEN NOW() ELSE decided_at END,
		       revoked_by  = CASE WHEN $4::text = 'REVOKED' THEN $5::text ELSE revoked_by END,
		       revoked_at  = CASE WHEN $4::text = 'REVOKED' THEN NOW() ELSE revoked_at END
		 WHERE sod_exception_id = $1::uuid AND tenant_id = $2::uuid AND status = $3::text
		RETURNING ` + sodExceptionColumns
	var e *domain.SoDException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		e, scanErr = scanSoDException(tx.QueryRow(ctx, query, id, tenantID, from, to, actor))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		if _, findErr := s.FindSoDException(ctx, id, tenantID); findErr != nil {
			return nil, findErr
		}
		return nil, domain.ErrSoDExceptionState
	}
	if err != nil {
		s.log.Error("pg TransitionSoDException failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return e, nil
}

// FindActiveSoDException returns the exception, if any, that currently lets
// principalID hold actionA together with actionB: APPROVED, within its period,
// on a rule (tenant or platform-wide, either order) pairing the two. The time
// predicate is evaluated here, at decision time, so an exception stops
// authorizing the instant it expires (GOV-04 negative path #3).
func (s *PgStore) FindActiveSoDException(ctx context.Context, principalID, tenantID, actionA, actionB string) (*domain.SoDException, error) {
	if tenantID == "" {
		return nil, nil // exceptions are tenant facts; a tenantless decision has none
	}
	const query = `
		SELECT ` + sodExceptionColumnsQualified + `
		  FROM sod_exceptions e
		  JOIN sod_rules sr ON sr.sod_rule_id = e.sod_rule_id AND sr.active_flag
		 WHERE e.tenant_id = $2::uuid AND e.principal_id = $1 AND e.status = 'APPROVED'
		   AND e.effective_from <= NOW() AND e.expires_at > NOW()
		   AND ((sr.action_a = $3 AND sr.action_b = $4) OR (sr.action_a = $4 AND sr.action_b = $3))
		 ORDER BY e.expires_at DESC
		 LIMIT 1`
	var e *domain.SoDException
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		e, scanErr = scanSoDException(tx.QueryRow(ctx, query, principalID, tenantID, actionA, actionB))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg FindActiveSoDException failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return e, nil
}

const sodExceptionColumnsQualified = `e.sod_exception_id, e.tenant_id, e.sod_rule_id, e.principal_id, e.compensating_control, e.reason, e.status,
	e.requested_by, e.approved_by, e.decided_at, e.effective_from, e.expires_at, e.revoked_by, e.revoked_at, e.created_at`

// ExpireDue marks what has run out: APPROVED exceptions past expires_at
// (emitting sod.exception.expired) and PENDING_APPROVAL assignments past their
// approval window. Decisions already treat both as ineffective the moment the
// time passes; this records the state. Per tenant, because the policies'
// WITH CHECK requires the tenant on an UPDATE.
func (s *PgStore) ExpireDue(ctx context.Context) (int, error) {
	const dueTenants = `
		SELECT tenant_id FROM sod_exceptions WHERE status = 'APPROVED' AND expires_at <= NOW()
		UNION
		SELECT r.tenant_id FROM principal_role_assignments pra JOIN roles r ON r.role_id = pra.role_id
		 WHERE pra.approval_status = 'PENDING_APPROVAL' AND pra.approval_expires_at <= NOW()`
	var tenants []string
	err := s.withPlatformScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, dueTenants)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return err
			}
			tenants = append(tenants, t)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	total := 0
	for _, t := range tenants {
		err := s.withRLS(ctx, t, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE sod_exceptions SET status = 'EXPIRED'
				WHERE tenant_id = $1::uuid AND status = 'APPROVED' AND expires_at <= NOW()`, t)
			if err != nil {
				return err
			}
			total += int(tag.RowsAffected())
			tag, err = tx.Exec(ctx, `UPDATE principal_role_assignments SET approval_status = 'EXPIRED', approval_decided_at = NOW()
				WHERE approval_status = 'PENDING_APPROVAL' AND approval_expires_at <= NOW()
				  AND role_id IN (SELECT role_id FROM roles WHERE tenant_id = $1::uuid)`, t)
			if err != nil {
				return err
			}
			total += int(tag.RowsAffected())
			return nil
		})
		if err != nil {
			return total, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
		}
	}
	return total, nil
}

// expirySweepInterval is how often ExpireDue runs; see RunExpirySweeper.
const expirySweepInterval = time.Minute

// RunExpirySweeper calls ExpireDue every minute until ctx ends.
func (s *PgStore) RunExpirySweeper(ctx context.Context) {
	t := time.NewTicker(expirySweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.ExpireDue(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("expiry sweep failed", zap.Error(err))
			} else if n > 0 {
				s.log.Info("expiry sweep", zap.Int("expired", n))
			}
		}
	}
}
