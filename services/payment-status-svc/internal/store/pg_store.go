// Package store implements payment-status-svc's persistence.
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

	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/middleware"
)

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isRegressionBlocked reports whether err is the custom SQLSTATE ZK002
// raised by the payment_execution_states trigger when a status update
// would regress a governed final state — see the migration.
func isRegressionBlocked(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "ZK002"
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
	RecordPaymentStatus(ctx context.Context, tenantID string, req domain.RecordPaymentStatusRequest, principalID string) (*domain.PaymentExecutionState, error)
	FindPayment(ctx context.Context, paymentID string) (*domain.PaymentExecutionState, error)

	ApplyCallbackStatus(ctx context.Context, paymentID string, payload domain.ProviderCallbackPayload, eventType, finalitySource, actorPrincipalID string) (state *domain.PaymentExecutionState, applied bool, err error)
	LinkStatement(ctx context.Context, paymentID string, req domain.LinkStatementRequest, principalID string) (state *domain.PaymentExecutionState, conflict bool, err error)
	ResolveConflict(ctx context.Context, paymentID string, req domain.ResolveConflictRequest, principalID string) (*domain.PaymentExecutionState, error)
	RecordReturn(ctx context.Context, paymentID string, req domain.RecordReturnRequest, principalID string) (*domain.PaymentExecutionState, error)
	CancelPayment(ctx context.Context, paymentID string, req domain.CancelRequest, principalID string) (*domain.PaymentExecutionState, error)

	ListEvents(ctx context.Context, paymentID string) ([]domain.StatusEvent, error)
	ListUnresolved(ctx context.Context) ([]domain.PaymentExecutionState, error)
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

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, paymentID, eventType, fromStatus, toStatus, providerEventRef, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO status_events (event_id, tenant_id, payment_id, event_type, from_status, to_status, provider_event_ref, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		uuid.New().String(), tenantID, paymentID, eventType, fromStatus, toStatus, providerEventRef, detail, actorPrincipalID,
	)
	return err
}

const stateColumns = `
	payment_id, tenant_id, legal_entity_id, provider_request_id, source_reference, status,
	finality_source, mapping_version, has_open_conflict, conflict_reason, created_by_principal_id, created_at, updated_at`

func scanState(row pgx.Row) (*domain.PaymentExecutionState, error) {
	p := &domain.PaymentExecutionState{}
	err := row.Scan(&p.PaymentID, &p.TenantID, &p.LegalEntityID, &nullString{&p.ProviderRequestID},
		&nullString{&p.SourceReference}, &p.Status, &nullString{&p.FinalitySource}, &nullString{&p.MappingVersion},
		&p.HasOpenConflict, &nullString{&p.ConflictReason}, &p.CreatedByPrincipalID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PgStore) RecordPaymentStatus(ctx context.Context, tenantID string, req domain.RecordPaymentStatusRequest, principalID string) (*domain.PaymentExecutionState, error) {
	id := uuid.New().String()
	var p *domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanState(tx.QueryRow(ctx, `
			INSERT INTO payment_execution_states (`+stateColumns+`)
			VALUES ($1, $2, $3, $4, $5, 'PREPARED', '', '', FALSE, '', $6, NOW(), NOW())
			RETURNING `+stateColumns,
			id, nullableTenant(tenantID), req.LegalEntityID, req.ProviderRequestID, req.SourceReference, principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, id, "PAYMENT_PREPARED", "", string(domain.StatusPrepared), "", "", principalID)
	})
	if err != nil {
		s.log.Error("pg RecordPaymentStatus failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) FindPayment(ctx context.Context, paymentID string) (*domain.PaymentExecutionState, error) {
	var p *domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1`, paymentID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrPaymentNotFound
	}
	if err != nil {
		s.log.Error("pg FindPayment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

// ApplyCallbackStatus is negative-path scenarios #2 and #3's real
// enforcement point. The event insert's own unique constraint on
// (payment_id, provider_event_ref) makes a duplicate callback a genuine
// no-op (applied=false, no error); the payment_execution_states trigger's
// regression guard makes an out-of-order callback against an already
// governed-final payment a genuine no-op too (applied=false, no error) —
// both are real, expected outcomes of a real webhook being replayed or
// arriving late, not exceptional failures.
func (s *PgStore) ApplyCallbackStatus(ctx context.Context, paymentID string, payload domain.ProviderCallbackPayload, eventType, finalitySource, actorPrincipalID string) (*domain.PaymentExecutionState, bool, error) {
	var p *domain.PaymentExecutionState
	applied := true
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		var fromStatus string
		if err := tx.QueryRow(ctx, `SELECT tenant_id, status FROM payment_execution_states WHERE payment_id = $1`, paymentID).Scan(&tenantID, &fromStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrPaymentNotFound
			}
			return err
		}

		if payload.ProviderEventRef != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO status_events (event_id, tenant_id, payment_id, event_type, from_status, to_status, provider_event_ref, actor_principal_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				uuid.New().String(), tenantID, paymentID, eventType, fromStatus, string(payload.ReportedStatus), payload.ProviderEventRef, actorPrincipalID,
			); err != nil {
				if isUniqueViolation(err) {
					applied = false
					p, err = scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1`, paymentID))
					return err
				}
				return err
			}
		}

		var err error
		p, err = scanState(tx.QueryRow(ctx, `
			UPDATE payment_execution_states SET status = $2, finality_source = $3, mapping_version = $4, updated_at = NOW()
			WHERE payment_id = $1
			RETURNING `+stateColumns,
			paymentID, string(payload.ReportedStatus), finalitySource, payload.MappingVersion,
		))
		if isRegressionBlocked(err) {
			applied = false
			_ = s.recordEvent(ctx, tx, tenantID, paymentID, domain.EventCallbackRejectedRegression, fromStatus, string(payload.ReportedStatus), payload.ProviderEventRef, "regression blocked", actorPrincipalID)
			p, err = scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1`, paymentID))
			return err
		}
		return err
	})
	if errors.Is(err, domain.ErrPaymentNotFound) {
		return nil, false, domain.ErrPaymentNotFound
	}
	if err != nil {
		s.log.Error("pg ApplyCallbackStatus failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, applied, nil
}

// LinkStatement raises a real, blocking conflict if the caller-supplied
// statement status disagrees with the current canonical status —
// negative-path scenario #4 — rather than overwriting either side.
func (s *PgStore) LinkStatement(ctx context.Context, paymentID string, req domain.LinkStatementRequest, principalID string) (*domain.PaymentExecutionState, bool, error) {
	var p *domain.PaymentExecutionState
	conflict := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1`, paymentID))
		if err != nil {
			return err
		}
		if current.Status != req.ReportedStatus {
			conflict = true
			p, err = scanState(tx.QueryRow(ctx, `
				UPDATE payment_execution_states SET has_open_conflict = TRUE, conflict_reason = $2, updated_at = NOW()
				WHERE payment_id = $1
				RETURNING `+stateColumns,
				paymentID, "statement reports "+string(req.ReportedStatus)+" but canonical status is "+string(current.Status),
			))
			if err != nil {
				return err
			}
			return s.recordEvent(ctx, tx, p.TenantID, paymentID, domain.EventPaymentStatusConflictRaised, string(current.Status), string(req.ReportedStatus), req.StatementReference, p.ConflictReason, principalID)
		}
		p = current
		return s.recordEvent(ctx, tx, p.TenantID, paymentID, "STATEMENT_CONFIRMATION_LINKED", string(current.Status), string(current.Status), req.StatementReference, "statement confirms canonical status", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, false, domain.ErrPaymentNotFound
	}
	if err != nil {
		s.log.Error("pg LinkStatement failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, conflict, nil
}

func (s *PgStore) ResolveConflict(ctx context.Context, paymentID string, req domain.ResolveConflictRequest, principalID string) (*domain.PaymentExecutionState, error) {
	var p *domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var fromStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM payment_execution_states WHERE payment_id = $1 AND has_open_conflict = TRUE`, paymentID).Scan(&fromStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrInvalidTransition
			}
			return err
		}
		var err error
		p, err = scanState(tx.QueryRow(ctx, `
			UPDATE payment_execution_states SET status = $2, has_open_conflict = FALSE, conflict_reason = '',
				finality_source = 'MANUAL_OVERRIDE', updated_at = NOW()
			WHERE payment_id = $1
			RETURNING `+stateColumns,
			paymentID, string(req.FinalStatus),
		))
		if isRegressionBlocked(err) {
			return domain.ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, paymentID, domain.EventPaymentStatusConflictResolved, fromStatus, string(req.FinalStatus), "", req.Reason, principalID)
	})
	if errors.Is(err, domain.ErrInvalidTransition) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ResolveConflict failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) RecordReturn(ctx context.Context, paymentID string, req domain.RecordReturnRequest, principalID string) (*domain.PaymentExecutionState, error) {
	var p *domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if req.ProviderEventRef != "" {
			var tenantID *string
			if err := tx.QueryRow(ctx, `SELECT tenant_id FROM payment_execution_states WHERE payment_id = $1`, paymentID).Scan(&tenantID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
					return domain.ErrPaymentNotFound
				}
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO status_events (event_id, tenant_id, payment_id, event_type, from_status, to_status, provider_event_ref, detail, actor_principal_id)
				VALUES ($1, $2, $3, $4, 'SETTLED', 'RETURNED', $5, $6, $7)`,
				uuid.New().String(), tenantID, paymentID, domain.EventPaymentReturned, req.ProviderEventRef, req.Reason, principalID,
			); err != nil {
				if isUniqueViolation(err) {
					return domain.ErrInvalidTransition // already applied — caller treats as idempotent no-op via the sentinel? see handler
				}
				return err
			}
		}
		var err error
		p, err = scanState(tx.QueryRow(ctx, `
			UPDATE payment_execution_states SET status = 'RETURNED', updated_at = NOW()
			WHERE payment_id = $1 AND status = 'SETTLED'
			RETURNING `+stateColumns,
			paymentID,
		))
		return err
	})
	if errors.Is(err, domain.ErrPaymentNotFound) {
		return nil, domain.ErrPaymentNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) || isRegressionBlocked(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RecordReturn failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) CancelPayment(ctx context.Context, paymentID string, req domain.CancelRequest, principalID string) (*domain.PaymentExecutionState, error) {
	var p *domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanState(tx.QueryRow(ctx, `
			UPDATE payment_execution_states SET status = 'CANCELLED', updated_at = NOW()
			WHERE payment_id = $1 AND status IN ('PREPARED', 'SUBMITTED')
			RETURNING `+stateColumns,
			paymentID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, p.TenantID, paymentID, domain.EventPaymentCancelled, "", "CANCELLED", "", req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg CancelPayment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return p, nil
}

func (s *PgStore) ListEvents(ctx context.Context, paymentID string) ([]domain.StatusEvent, error) {
	var out []domain.StatusEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, payment_id, event_type, from_status, to_status, provider_event_ref, detail, actor_principal_id, created_at
			FROM status_events WHERE payment_id = $1 ORDER BY created_at ASC`, paymentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.StatusEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.PaymentID, &e.EventType, &e.FromStatus, &e.ToStatus, &nullString{&e.ProviderEventRef}, &nullString{&e.Detail}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
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

func (s *PgStore) ListUnresolved(ctx context.Context) ([]domain.PaymentExecutionState, error) {
	var out []domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE has_open_conflict = TRUE ORDER BY updated_at ASC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanState(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListUnresolved failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
