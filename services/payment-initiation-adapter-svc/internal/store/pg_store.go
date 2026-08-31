// Package store implements payment-initiation-adapter-svc's persistence.
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

	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	"zoiko.io/payment-initiation-adapter-svc/internal/middleware"
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
	PrepareAttempt(ctx context.Context, tenantID string, req domain.PrepareAttemptRequest, principalID string) (*domain.PaymentInitiationAttempt, error)
	FindAttempt(ctx context.Context, attemptID string) (*domain.PaymentInitiationAttempt, error)
	FindByIdempotencyKey(ctx context.Context, idempotencyKey string) (*domain.PaymentInitiationAttempt, error)

	MarkSubmitted(ctx context.Context, attemptID, providerRequestID, responseRef, principalID string) (*domain.PaymentInitiationAttempt, error)
	MarkPendingUnknown(ctx context.Context, attemptID, principalID string) (*domain.PaymentInitiationAttempt, error)
	MarkRejected(ctx context.Context, attemptID, reason, principalID string) (*domain.PaymentInitiationAttempt, error)
	CancelAttempt(ctx context.Context, attemptID, principalID string) (*domain.PaymentInitiationAttempt, error)
	QuarantineAttempt(ctx context.Context, attemptID string, req domain.QuarantineRequest, principalID string) (*domain.PaymentInitiationAttempt, error)
	ResolveAmbiguous(ctx context.Context, attemptID string, req domain.ResolveAmbiguousRequest, principalID string) (*domain.PaymentInitiationAttempt, error)

	ListEvents(ctx context.Context, attemptID string) ([]domain.AttemptEvent, error)
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

func nullableTenant(tenantID string) *string {
	if tenantID == "" {
		return nil
	}
	return &tenantID
}

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, attemptID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO attempt_events (event_id, tenant_id, attempt_id, event_type, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), tenantID, attemptID, eventType, detail, actorPrincipalID,
	)
	return err
}

const attemptColumns = `
	attempt_id, tenant_id, legal_entity_id, source_reference, authorization_fingerprint, payer_account_ref,
	payee_ref, amount, currency, execution_date, payment_reference, payer_account_verified, idempotency_key,
	status, provider_request_id, provider_response_ref, rejection_reason, quarantine_reason,
	ambiguous_resolution_note, submitted_at, resolved_at, created_by_principal_id, created_at, updated_at`

func scanAttempt(row pgx.Row) (*domain.PaymentInitiationAttempt, error) {
	a := &domain.PaymentInitiationAttempt{}
	err := row.Scan(&a.AttemptID, &a.TenantID, &a.LegalEntityID, &a.SourceReference, &a.AuthorizationFingerprint,
		&a.PayerAccountRef, &a.PayeeRef, &a.Amount, &a.Currency, &a.ExecutionDate, &nullString{&a.PaymentReference},
		&a.PayerAccountVerified, &a.IdempotencyKey, &a.Status, &nullString{&a.ProviderRequestID},
		&nullString{&a.ProviderResponseRef}, &nullString{&a.RejectionReason}, &nullString{&a.QuarantineReason},
		&nullString{&a.AmbiguousResolutionNote}, &a.SubmittedAt, &a.ResolvedAt, &a.CreatedByPrincipalID,
		&a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// PrepareAttempt is the durable-before-network-call guarantee itself: this
// row commits before any code path is allowed to call the provider
// adapter — see internal/domain's package doc. Relies on the migration's
// unique index on idempotency_key to reject a duplicate.
func (s *PgStore) PrepareAttempt(ctx context.Context, tenantID string, req domain.PrepareAttemptRequest, principalID string) (*domain.PaymentInitiationAttempt, error) {
	id := uuid.New().String()
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `
			INSERT INTO payment_initiation_attempts (`+attemptColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'PREPARED', '', '', '', '', '', NULL, NULL, $14, NOW(), NOW())
			RETURNING `+attemptColumns,
			id, nullableTenant(tenantID), req.LegalEntityID, req.SourceReference, req.AuthorizationFingerprint,
			req.PayerAccountRef, req.PayeeRef, req.Amount, req.Currency, req.ExecutionDate, req.PaymentReference,
			req.PayerAccountVerified, req.IdempotencyKey, principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, id, domain.EventInitiationPrepared, "", principalID)
	})
	if isUniqueViolation(err) {
		return nil, domain.ErrDuplicateIdempotencyKey
	}
	if err != nil {
		s.log.Error("pg PrepareAttempt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) FindAttempt(ctx context.Context, attemptID string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_initiation_attempts WHERE attempt_id = $1`, attemptID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrAttemptNotFound
	}
	if err != nil {
		s.log.Error("pg FindAttempt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) FindByIdempotencyKey(ctx context.Context, idempotencyKey string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM payment_initiation_attempts WHERE idempotency_key = $1`, idempotencyKey))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrAttemptNotFound
	}
	if err != nil {
		s.log.Error("pg FindByIdempotencyKey failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) MarkSubmitted(ctx context.Context, attemptID, providerRequestID, responseRef, principalID string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `
			UPDATE payment_initiation_attempts SET status = 'SUBMITTED', provider_request_id = $2, provider_response_ref = $3,
				submitted_at = NOW(), updated_at = NOW()
			WHERE attempt_id = $1 AND status IN ('PREPARED', 'PENDING_UNKNOWN')
			RETURNING `+attemptColumns,
			attemptID, providerRequestID, responseRef,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventPaymentSubmitted, providerRequestID, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg MarkSubmitted failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) MarkPendingUnknown(ctx context.Context, attemptID, principalID string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `
			UPDATE payment_initiation_attempts SET status = 'PENDING_UNKNOWN', updated_at = NOW()
			WHERE attempt_id = $1 AND status IN ('PREPARED', 'PENDING_UNKNOWN')
			RETURNING `+attemptColumns,
			attemptID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventSubmissionAmbiguous, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg MarkPendingUnknown failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) MarkRejected(ctx context.Context, attemptID, reason, principalID string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `
			UPDATE payment_initiation_attempts SET status = 'REJECTED_BEFORE_SUBMISSION', rejection_reason = $2, updated_at = NOW()
			WHERE attempt_id = $1 AND status IN ('PREPARED', 'PENDING_UNKNOWN')
			RETURNING `+attemptColumns,
			attemptID, reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventSubmissionRejected, reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg MarkRejected failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) CancelAttempt(ctx context.Context, attemptID, principalID string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `
			UPDATE payment_initiation_attempts SET status = 'CANCELLED', updated_at = NOW()
			WHERE attempt_id = $1 AND status = 'PREPARED'
			RETURNING `+attemptColumns,
			attemptID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventAttemptCancelled, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg CancelAttempt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) QuarantineAttempt(ctx context.Context, attemptID string, req domain.QuarantineRequest, principalID string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `
			UPDATE payment_initiation_attempts SET status = 'QUARANTINED', quarantine_reason = $2, updated_at = NOW()
			WHERE attempt_id = $1 AND status IN ('PREPARED', 'PENDING_UNKNOWN')
			RETURNING `+attemptColumns,
			attemptID, req.Reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventAttemptQuarantined, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg QuarantineAttempt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// ResolveAmbiguous is BNK-06's own honest admission that it cannot itself
// determine the true outcome of a PENDING_UNKNOWN attempt — only an
// operator, informed by BNK-07/reconciliation out-of-band, can.
func (s *PgStore) ResolveAmbiguous(ctx context.Context, attemptID string, req domain.ResolveAmbiguousRequest, principalID string) (*domain.PaymentInitiationAttempt, error) {
	var a *domain.PaymentInitiationAttempt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAttempt(tx.QueryRow(ctx, `
			UPDATE payment_initiation_attempts SET status = $2, ambiguous_resolution_note = $3, resolved_at = NOW(), updated_at = NOW()
			WHERE attempt_id = $1 AND status = 'PENDING_UNKNOWN'
			RETURNING `+attemptColumns,
			attemptID, string(req.ResolvedStatus), req.Note,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, a.TenantID, attemptID, domain.EventAmbiguousResolved, req.Note, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ResolveAmbiguous failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) ListEvents(ctx context.Context, attemptID string) ([]domain.AttemptEvent, error) {
	var out []domain.AttemptEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, attempt_id, event_type, detail, actor_principal_id, created_at
			FROM attempt_events WHERE attempt_id = $1 ORDER BY created_at ASC`, attemptID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.AttemptEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.AttemptID, &e.EventType, &nullString{&e.Detail}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
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
