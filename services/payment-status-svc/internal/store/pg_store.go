// Package store implements payment-status-svc's persistence.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
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
	RecordPaymentStatus(ctx context.Context, tenantID string, req domain.RecordPaymentStatusRequest, principalID, correlationID string) (*domain.PaymentExecutionState, error)
	FindPayment(ctx context.Context, paymentID string) (*domain.PaymentExecutionState, error)

	ApplyCallbackStatus(ctx context.Context, paymentID string, payload domain.ProviderCallbackPayload, eventType, finalitySource, actorPrincipalID, correlationID string, emitEvent bool) (state *domain.PaymentExecutionState, applied bool, err error)
	LinkStatement(ctx context.Context, paymentID string, req domain.LinkStatementRequest, principalID, correlationID string) (state *domain.PaymentExecutionState, conflict bool, err error)
	ResolveConflict(ctx context.Context, paymentID string, req domain.ResolveConflictRequest, principalID, correlationID string) (*domain.PaymentExecutionState, error)
	RecordReturn(ctx context.Context, paymentID string, req domain.RecordReturnRequest, principalID string) (*domain.PaymentExecutionState, error)
	CancelPayment(ctx context.Context, paymentID string, req domain.CancelRequest, principalID, correlationID string) (*domain.PaymentExecutionState, error)

	ListEvents(ctx context.Context, paymentID string) ([]domain.StatusEvent, error)
	ListUnresolved(ctx context.Context, legalEntityID string) ([]domain.PaymentExecutionState, error)
}

type PgStore struct {
	pool        *pgxpool.Pool
	log         *zap.Logger
	eventRegion string
}

// Option configures a PgStore at construction.
type Option func(*PgStore)

// WithEventRegion sets the residency region carried by emitted events
// (ZS-EVENT-001 §4, envelope.Spec.ResidencyRegion — required, a controlled
// lower-case code such as "uk" or "eu-west").
func WithEventRegion(region string) Option {
	return func(s *PgStore) { s.eventRegion = region }
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger, opts ...Option) *PgStore {
	s := &PgStore{pool: pool, log: log}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// strOrEmpty dereferences a nullable tenant_id (this service's tables allow
// NULL tenant_id — see migration 000003's policy) for use as a plain string
// in an event envelope, which requires a non-empty TenantID.
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// enqueueEvent builds and enqueues one event inside the caller's own
// transaction — the entire point of the transactional outbox (ZS-EVENT-001
// §6): the event and the state change it reports commit atomically, or
// neither does. legacyType is the pre-standard event_type string existing
// consumers filter on; always pass it explicitly — envelope.New silently
// defaults it to the canonical type when empty, which would silently break
// any consumer still reading the old name.
func (s *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, canonicalType, legacyType, aggregateType, aggregateID, tenantID, legalEntityID, actorID, correlationID string, data any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            canonicalType,
		LegacyType:      legacyType,
		Service:         "payment-status-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   aggregateType,
		AggregateID:     aggregateID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		Classification:  envelope.Confidential,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", canonicalType, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", canonicalType, err)
	}
	return nil
}

// canonicalForLegacy maps this service's existing SCREAMING_SNAKE event_type
// strings (internal/handler's eventTypeFor and the domain.Event* constants)
// to the kebab-case canonical type envelope.New requires. Centralized here
// rather than duplicated per call site, since ApplyCallbackStatus's legacy
// type is itself computed (eventTypeFor) and passed in as a plain string.
func canonicalForLegacy(legacy string) string {
	switch legacy {
	case "PAYMENT_EXECUTION_STATE_RECORDED":
		return "com.zoikosuite.banking.payment-execution-state.recorded"
	case domain.EventPaymentAccepted:
		return "com.zoikosuite.banking.payment-execution-state.accepted"
	case domain.EventPaymentPending:
		return "com.zoikosuite.banking.payment-execution-state.pending"
	case domain.EventPaymentSettled:
		return "com.zoikosuite.banking.payment-execution-state.settled"
	case domain.EventPaymentRejected:
		return "com.zoikosuite.banking.payment-execution-state.rejected"
	case domain.EventPaymentReturned:
		return "com.zoikosuite.banking.payment-execution-state.returned"
	case domain.EventPaymentCancelled:
		return "com.zoikosuite.banking.payment-execution-state.cancelled"
	case domain.EventPaymentStatusConflictRaised:
		return "com.zoikosuite.banking.payment-execution-state.conflict-raised"
	case domain.EventPaymentStatusConflictResolved:
		return "com.zoikosuite.banking.payment-execution-state.conflict-resolved"
	default:
		// eventTypeFor's own fallback ("PAYMENT_STATUS_UPDATED") and any
		// future unmapped legacy type land here rather than failing closed
		// — envelope.New still validates the result is well-formed kebab.
		return "com.zoikosuite.banking.payment-execution-state.status-updated"
	}
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
	payment_id, tenant_id, legal_entity_id, provider_request_id, source_reference, provider_key, status,
	finality_source, mapping_version, has_open_conflict, conflict_reason, created_by_principal_id, created_at, updated_at`

func scanState(row pgx.Row) (*domain.PaymentExecutionState, error) {
	p := &domain.PaymentExecutionState{}
	err := row.Scan(&p.PaymentID, &p.TenantID, &p.LegalEntityID, &nullString{&p.ProviderRequestID},
		&nullString{&p.SourceReference}, &nullString{&p.ProviderKey}, &p.Status, &nullString{&p.FinalitySource}, &nullString{&p.MappingVersion},
		&p.HasOpenConflict, &nullString{&p.ConflictReason}, &p.CreatedByPrincipalID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (s *PgStore) RecordPaymentStatus(ctx context.Context, tenantID string, req domain.RecordPaymentStatusRequest, principalID, correlationID string) (*domain.PaymentExecutionState, error) {
	id := uuid.New().String()
	var p *domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = scanState(tx.QueryRow(ctx, `
			INSERT INTO payment_execution_states (`+stateColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, 'PREPARED', '', '', FALSE, '', $7, NOW(), NOW())
			RETURNING `+stateColumns,
			id, nullableTenant(tenantID), req.LegalEntityID, req.ProviderRequestID, req.SourceReference, req.ProviderKey, principalID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, p.TenantID, id, "PAYMENT_PREPARED", "", string(domain.StatusPrepared), "", "", principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, canonicalForLegacy("PAYMENT_EXECUTION_STATE_RECORDED"), "PAYMENT_EXECUTION_STATE_RECORDED",
			"payment-execution-state", id, tenantID, req.LegalEntityID, principalID, correlationID, p)
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
//
// emitEvent exists because this method is shared by two handlers with
// different event histories: ProcessProviderCallback (the real webhook,
// which has always published its event) and PollPaymentStatus (the
// caller-attested manual path, which never did — not a decision recorded
// anywhere, just an absent Publish call in the old dual-write). Fixing the
// dual-write here must not silently start emitting events for a path that
// never emitted any; that would be a scope expansion past this fix. Pass
// false from PollPaymentStatus to preserve its exact prior behavior.
func (s *PgStore) ApplyCallbackStatus(ctx context.Context, paymentID string, payload domain.ProviderCallbackPayload, eventType, finalitySource, actorPrincipalID, correlationID string, emitEvent bool) (*domain.PaymentExecutionState, bool, error) {
	var p *domain.PaymentExecutionState
	applied := true
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		var fromStatus string
		var hasOpenConflict bool
		if err := tx.QueryRow(ctx, `SELECT tenant_id, status, has_open_conflict FROM payment_execution_states WHERE payment_id = $1 FOR UPDATE`, paymentID).Scan(&tenantID, &fromStatus, &hasOpenConflict); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrPaymentNotFound
			}
			return err
		}
		if hasOpenConflict {
			// Conflicting provider/statement evidence is an exception: it must
			// be resolved through ResolveStatusConflict, never overwritten by a
			// later poll or legacy callback.
			var scanErr error
			p, scanErr = scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1`, paymentID))
			if scanErr != nil {
				return scanErr
			}
			return domain.ErrOpenConflictExists
		}

		// Both recovery branches below run a follow-up query on THIS SAME
		// transaction after an expected, already-handled error (a
		// duplicate provider_event_ref; a regression the trigger blocked).
		// Postgres aborts the whole transaction on any statement error —
		// without a SAVEPOINT to roll back to first, that follow-up query
		// would itself fail with "current transaction is aborted" (25P02),
		// turning an expected no-op into a real error. This was a real,
		// pre-existing bug: this service had zero real-Postgres tests
		// before this session, so neither recovery path had ever actually
		// run against a real transaction.
		if payload.ProviderEventRef != "" {
			if _, err := tx.Exec(ctx, "SAVEPOINT apply_callback_insert"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO status_events (event_id, tenant_id, payment_id, event_type, from_status, to_status, provider_event_ref, actor_principal_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				uuid.New().String(), tenantID, paymentID, eventType, fromStatus, string(payload.ReportedStatus), payload.ProviderEventRef, actorPrincipalID,
			); err != nil {
				if isUniqueViolation(err) {
					if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT apply_callback_insert"); rbErr != nil {
						return rbErr
					}
					applied = false
					var scanErr error
					p, scanErr = scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1`, paymentID))
					return scanErr
				}
				return err
			}
			if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT apply_callback_insert"); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, "SAVEPOINT apply_callback_update"); err != nil {
			return err
		}
		var err error
		p, err = scanState(tx.QueryRow(ctx, `
			UPDATE payment_execution_states SET status = $2, finality_source = $3, mapping_version = $4, updated_at = NOW()
			WHERE payment_id = $1
			RETURNING `+stateColumns,
			paymentID, string(payload.ReportedStatus), finalitySource, payload.MappingVersion,
		))
		if isRegressionBlocked(err) {
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT apply_callback_update"); rbErr != nil {
				return rbErr
			}
			applied = false
			// This recordEvent call can collide with the row the earlier
			// unconditional INSERT already wrote for the same
			// provider_event_ref (uq_status_events_provider_event_ref is
			// on (payment_id, provider_event_ref) alone, with no
			// event_type dimension — a real, pre-existing design
			// constraint, not new to this fix) whenever ProviderEventRef
			// is non-empty. That earlier row already durably records the
			// attempted (and rejected) transition, so a second-insert
			// collision here is a harmless duplicate, not a lost fact —
			// its own savepoint lets this degrade gracefully instead of
			// aborting the whole ApplyCallbackStatus call.
			if _, err := tx.Exec(ctx, "SAVEPOINT apply_callback_regression_event"); err != nil {
				return err
			}
			if err := s.recordEvent(ctx, tx, tenantID, paymentID, domain.EventCallbackRejectedRegression, fromStatus, string(payload.ReportedStatus), payload.ProviderEventRef, "regression blocked", actorPrincipalID); err != nil {
				if !isUniqueViolation(err) {
					return err
				}
				if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT apply_callback_regression_event"); rbErr != nil {
					return rbErr
				}
			} else if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT apply_callback_regression_event"); err != nil {
				return err
			}
			var scanErr error
			p, scanErr = scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1`, paymentID))
			return scanErr
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT apply_callback_update"); err != nil {
			return err
		}
		if !emitEvent {
			return nil
		}
		return s.enqueueEvent(ctx, tx, canonicalForLegacy(eventType), eventType,
			"payment-execution-state", paymentID, strOrEmpty(tenantID), p.LegalEntityID, actorPrincipalID, correlationID, p)
	})
	if errors.Is(err, domain.ErrPaymentNotFound) {
		return nil, false, domain.ErrPaymentNotFound
	}
	if errors.Is(err, domain.ErrOpenConflictExists) {
		return p, false, domain.ErrOpenConflictExists
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
func (s *PgStore) LinkStatement(ctx context.Context, paymentID string, req domain.LinkStatementRequest, principalID, correlationID string) (*domain.PaymentExecutionState, bool, error) {
	var p *domain.PaymentExecutionState
	conflict := false
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanState(tx.QueryRow(ctx, `SELECT `+stateColumns+` FROM payment_execution_states WHERE payment_id = $1 FOR UPDATE`, paymentID))
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
			if err := s.recordEvent(ctx, tx, p.TenantID, paymentID, domain.EventPaymentStatusConflictRaised, string(current.Status), string(req.ReportedStatus), req.StatementReference, p.ConflictReason, principalID); err != nil {
				return err
			}
			// bank-reconciliation-svc consumes this event to create an
			// evidence_conflict record — payload shape preserved exactly
			// from the dual-write it replaces so that consumer needs no
			// changes.
			return s.enqueueEvent(ctx, tx, canonicalForLegacy(domain.EventPaymentStatusConflictRaised), domain.EventPaymentStatusConflictRaised,
				"payment-execution-state", paymentID, strOrEmpty(p.TenantID), p.LegalEntityID, principalID, correlationID, map[string]interface{}{
					"payment_id":          paymentID,
					"statement_line_id":   req.StatementLineID,
					"provider_request_id": p.ProviderRequestID,
					"bank_rec_status":     req.BankRecStatus,
					"reported_status":     req.ReportedStatus,
					"current_status":      p.Status,
					"statement_reference": req.StatementReference,
				})
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

func (s *PgStore) ResolveConflict(ctx context.Context, paymentID string, req domain.ResolveConflictRequest, principalID, correlationID string) (*domain.PaymentExecutionState, error) {
	if !domain.ValidResolutionStatus(req.FinalStatus) {
		return nil, domain.ErrInvalidResolution
	}
	var p *domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var fromStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM payment_execution_states WHERE payment_id = $1 AND has_open_conflict = TRUE FOR UPDATE`, paymentID).Scan(&fromStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrInvalidTransition
			}
			return err
		}
		var err error
		p, err = scanState(tx.QueryRow(ctx, `
			UPDATE payment_execution_states SET status = $2, has_open_conflict = FALSE, conflict_reason = '',
				finality_source = 'MANUAL_OVERRIDE', updated_at = NOW()
			WHERE payment_id = $1 AND has_open_conflict = TRUE
			RETURNING `+stateColumns,
			paymentID, string(req.FinalStatus),
		))
		if isRegressionBlocked(err) {
			return domain.ErrInvalidTransition
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, p.TenantID, paymentID, domain.EventPaymentStatusConflictResolved, fromStatus, string(req.FinalStatus), "", req.Reason, principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, canonicalForLegacy(domain.EventPaymentStatusConflictResolved), domain.EventPaymentStatusConflictResolved,
			"payment-execution-state", paymentID, strOrEmpty(p.TenantID), p.LegalEntityID, principalID, correlationID, p)
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
		var tenantID *string
		if req.ProviderEventRef != "" {
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
		if err != nil {
			return err
		}
		// correlationID is req.ProviderEventRef, same as the dual-write
		// this replaces (not the request's X-Correlation-ID header). Use
		// p.TenantID (from the RETURNING clause above), not the local
		// tenantID var, which stays nil when ProviderEventRef is empty.
		return s.enqueueEvent(ctx, tx, canonicalForLegacy(domain.EventPaymentReturned), domain.EventPaymentReturned,
			"payment-execution-state", paymentID, strOrEmpty(p.TenantID), p.LegalEntityID, principalID, req.ProviderEventRef, p)
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

func (s *PgStore) CancelPayment(ctx context.Context, paymentID string, req domain.CancelRequest, principalID, correlationID string) (*domain.PaymentExecutionState, error) {
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
		if err := s.recordEvent(ctx, tx, p.TenantID, paymentID, domain.EventPaymentCancelled, "", "CANCELLED", "", req.Reason, principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, canonicalForLegacy(domain.EventPaymentCancelled), domain.EventPaymentCancelled,
			"payment-execution-state", paymentID, strOrEmpty(p.TenantID), p.LegalEntityID, principalID, correlationID, p)
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

func (s *PgStore) ListUnresolved(ctx context.Context, legalEntityID string) ([]domain.PaymentExecutionState, error) {
	var out []domain.PaymentExecutionState
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+stateColumns+` FROM payment_execution_states
			WHERE has_open_conflict = TRUE AND legal_entity_id = $1::uuid
			ORDER BY updated_at ASC`, legalEntityID)
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
