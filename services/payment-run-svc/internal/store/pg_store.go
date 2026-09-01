// Package store implements payment-run-svc's persistence.
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

	"zoiko.io/payment-run-svc/internal/domain"
	"zoiko.io/payment-run-svc/internal/middleware"
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
	CreateRun(ctx context.Context, tenantID string, req domain.CreateRunRequest, instructions []domain.RunInstruction, principalID string) (*domain.PaymentRun, []domain.RunInstruction, error)
	FindRun(ctx context.Context, runID string) (*domain.PaymentRun, error)
	ListInstructions(ctx context.Context, runID string) ([]domain.RunInstruction, error)
	FindInstruction(ctx context.Context, instructionID string) (*domain.RunInstruction, error)

	ValidateRun(ctx context.Context, runID, principalID string) (*domain.PaymentRun, error)
	MarkInstructionConsumed(ctx context.Context, instructionID string) error
	SetInstructionProviderRefs(ctx context.Context, instructionID, providerAttemptID, bnk07PaymentID string) error
	LockRun(ctx context.Context, runID, principalID string) (*domain.PaymentRun, error)
	MarkRunException(ctx context.Context, runID, reason, principalID string) (*domain.PaymentRun, error)
	SubmitRun(ctx context.Context, runID, idempotencyKey, principalID string) (*domain.PaymentRun, error)
	CancelRun(ctx context.Context, runID string, req domain.CancelRunRequest, principalID string) (*domain.PaymentRun, error)
	CloseRun(ctx context.Context, runID string, req domain.CloseRunRequest, principalID string) (*domain.PaymentRun, error)

	ReconcileInstruction(ctx context.Context, req domain.ReconcileInstructionRequest, principalID string) (*domain.RunInstruction, bool, error)
	UpdateRunAggregateStatus(ctx context.Context, runID string, newStatus domain.RunStatus, principalID string) (*domain.PaymentRun, error)
	RetryInstruction(ctx context.Context, instructionID, principalID string) error

	ListEvents(ctx context.Context, runID string) ([]domain.RunEvent, error)
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

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, runID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO run_events (event_id, tenant_id, run_id, event_type, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), tenantID, runID, eventType, detail, actorPrincipalID,
	)
	return err
}

// ── runs ─────────────────────────────────────────────────────────────────────

const runColumns = `
	run_id, tenant_id, legal_entity_id, paying_bank_account_ref, currency, value_date, payment_method,
	status, idempotency_key, created_by_principal_id, validated_at, locked_at, submitted_at, closed_at,
	exception_reason, cancel_reason, close_note, created_at, updated_at`

func scanRun(row pgx.Row) (*domain.PaymentRun, error) {
	r := &domain.PaymentRun{}
	err := row.Scan(&r.RunID, &r.TenantID, &r.LegalEntityID, &r.PayingBankAccountRef, &r.Currency, &r.ValueDate,
		&r.PaymentMethod, &r.Status, &nullString{&r.IdempotencyKey}, &r.CreatedByPrincipalID, &r.ValidatedAt,
		&r.LockedAt, &r.SubmittedAt, &r.ClosedAt, &nullString{&r.ExceptionReason}, &nullString{&r.CancelReason},
		&nullString{&r.CloseNote}, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

const instructionColumns = `
	instruction_id, tenant_id, run_id, authorization_id, payee_ref, net_amount, currency, status, consumed_at,
	provider_attempt_id, bnk07_payment_id, created_at`

func scanInstruction(row pgx.Row) (*domain.RunInstruction, error) {
	i := &domain.RunInstruction{}
	err := row.Scan(&i.InstructionID, &i.TenantID, &i.RunID, &i.AuthorizationID, &i.PayeeRef, &i.NetAmount,
		&i.Currency, &i.Status, &i.ConsumedAt, &i.ProviderAttemptID, &i.Bnk07PaymentID, &i.CreatedAt)
	if err != nil {
		return nil, err
	}
	return i, nil
}

// CreateRun relies on the migration's unique index to reject an
// authorization already consumed into another run instruction.
func (s *PgStore) CreateRun(ctx context.Context, tenantID string, req domain.CreateRunRequest, instructions []domain.RunInstruction, principalID string) (*domain.PaymentRun, []domain.RunInstruction, error) {
	runID := uuid.New().String()
	var run *domain.PaymentRun
	var out []domain.RunInstruction
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		run, err = scanRun(tx.QueryRow(ctx, `
			INSERT INTO payment_runs (`+runColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'DRAFT', '', $8, NULL, NULL, NULL, NULL, '', '', '', NOW(), NOW())
			RETURNING `+runColumns,
			runID, nullableTenant(tenantID), req.LegalEntityID, req.PayingBankAccountRef, req.Currency,
			req.ValueDate, req.PaymentMethod, principalID,
		))
		if err != nil {
			return err
		}
		for _, ins := range instructions {
			id := uuid.New().String()
			created, err := scanInstruction(tx.QueryRow(ctx, `
				INSERT INTO run_instructions (`+instructionColumns+`)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', NULL, '', '', NOW())
				RETURNING `+instructionColumns,
				id, run.TenantID, runID, ins.AuthorizationID, ins.PayeeRef, ins.NetAmount, ins.Currency,
			))
			if err != nil {
				return err
			}
			out = append(out, *created)
		}
		return s.recordEvent(ctx, tx, run.TenantID, runID, domain.EventRunCreated, "", principalID)
	})
	if isUniqueViolation(err) {
		return nil, nil, domain.ErrAuthorizationNotEligible
	}
	if err != nil {
		s.log.Error("pg CreateRun failed", zap.Error(err))
		return nil, nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return run, out, nil
}

func nullableTenant(tenantID string) *string {
	if tenantID == "" {
		return nil
	}
	return &tenantID
}

func (s *PgStore) FindRun(ctx context.Context, runID string) (*domain.PaymentRun, error) {
	var r *domain.PaymentRun
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+` FROM payment_runs WHERE run_id = $1`, runID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrRunNotFound
	}
	if err != nil {
		s.log.Error("pg FindRun failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) ListInstructions(ctx context.Context, runID string) ([]domain.RunInstruction, error) {
	var out []domain.RunInstruction
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+instructionColumns+` FROM run_instructions WHERE run_id = $1 ORDER BY created_at ASC`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			i, err := scanInstruction(rows)
			if err != nil {
				return err
			}
			out = append(out, *i)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListInstructions failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) FindInstruction(ctx context.Context, instructionID string) (*domain.RunInstruction, error) {
	var i *domain.RunInstruction
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		i, err = scanInstruction(tx.QueryRow(ctx, `SELECT `+instructionColumns+` FROM run_instructions WHERE instruction_id = $1`, instructionID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInstructionNotFound
	}
	if err != nil {
		s.log.Error("pg FindInstruction failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return i, nil
}

// ── lifecycle ────────────────────────────────────────────────────────────────

func (s *PgStore) ValidateRun(ctx context.Context, runID, principalID string) (*domain.PaymentRun, error) {
	var r *domain.PaymentRun
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRun(tx.QueryRow(ctx, `
			UPDATE payment_runs SET status = 'VALIDATED', validated_at = NOW(), updated_at = NOW()
			WHERE run_id = $1 AND status = 'DRAFT'
			RETURNING `+runColumns,
			runID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, r.TenantID, runID, domain.EventRunValidated, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ValidateRun failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) MarkInstructionConsumed(ctx context.Context, instructionID string) error {
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE run_instructions SET consumed_at = NOW() WHERE instruction_id = $1 AND consumed_at IS NULL`, instructionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInstructionNotFound
		}
		return nil
	})
	if errors.Is(err, domain.ErrInstructionNotFound) || isInvalidUUID(err) {
		return domain.ErrInstructionNotFound
	}
	if err != nil {
		s.log.Error("pg MarkInstructionConsumed failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// SetInstructionProviderRefs records the real correlation to BNK-06's
// attempt and BNK-07's execution record, once, right after SubmitPaymentRun
// actually hands an instruction to Banking. The WHERE clause only matches a
// row that has never had this set before — 000004's trigger enforces the
// same invariant at the database level as defense in depth.
func (s *PgStore) SetInstructionProviderRefs(ctx context.Context, instructionID, providerAttemptID, bnk07PaymentID string) error {
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE run_instructions SET provider_attempt_id = $1, bnk07_payment_id = $2
			WHERE instruction_id = $3 AND provider_attempt_id = ''`,
			providerAttemptID, bnk07PaymentID, instructionID,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrInstructionNotFound
		}
		return nil
	})
	if errors.Is(err, domain.ErrInstructionNotFound) || isInvalidUUID(err) {
		return domain.ErrInstructionNotFound
	}
	if err != nil {
		s.log.Error("pg SetInstructionProviderRefs failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) LockRun(ctx context.Context, runID, principalID string) (*domain.PaymentRun, error) {
	var r *domain.PaymentRun
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRun(tx.QueryRow(ctx, `
			UPDATE payment_runs SET status = 'LOCKED', locked_at = NOW(), updated_at = NOW()
			WHERE run_id = $1 AND status = 'VALIDATED'
			RETURNING `+runColumns,
			runID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, r.TenantID, runID, domain.EventRunLocked, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg LockRun failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// MarkRunException moves a run to EXCEPTION from any non-terminal status —
// used when LockRun's authorization-consumption sequence fails partway
// through, so a partial consumption is never silently left looking like a
// normal in-progress run.
func (s *PgStore) MarkRunException(ctx context.Context, runID, reason, principalID string) (*domain.PaymentRun, error) {
	var r *domain.PaymentRun
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRun(tx.QueryRow(ctx, `
			UPDATE payment_runs SET status = 'EXCEPTION', exception_reason = $2, updated_at = NOW()
			WHERE run_id = $1 AND status NOT IN ('COMPLETED', 'CANCELLED')
			RETURNING `+runColumns,
			runID, reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, r.TenantID, runID, domain.EventRunExceptionRaised, reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg MarkRunException failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// SubmitRun is idempotent on idempotency_key — negative-path scenario #1
// ("payment run replayed after timeout"). A repeat call on an already-
// SUBMITTED run with the SAME key is a no-op returning the current row; a
// different key is rejected by the caller (handler checks before calling
// this) rather than silently re-submitting.
func (s *PgStore) SubmitRun(ctx context.Context, runID, idempotencyKey, principalID string) (*domain.PaymentRun, error) {
	var r *domain.PaymentRun
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRun(tx.QueryRow(ctx, `
			UPDATE payment_runs SET status = 'SUBMITTED', idempotency_key = $2, submitted_at = NOW(), updated_at = NOW()
			WHERE run_id = $1 AND status = 'LOCKED'
			RETURNING `+runColumns,
			runID, idempotencyKey,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, r.TenantID, runID, domain.EventRunSubmitted, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg SubmitRun failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) CancelRun(ctx context.Context, runID string, req domain.CancelRunRequest, principalID string) (*domain.PaymentRun, error) {
	var r *domain.PaymentRun
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRun(tx.QueryRow(ctx, `
			UPDATE payment_runs SET status = 'CANCELLED', cancel_reason = $2, updated_at = NOW()
			WHERE run_id = $1 AND status IN ('DRAFT', 'VALIDATED')
			RETURNING `+runColumns,
			runID, req.Reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, r.TenantID, runID, domain.EventRunCancelled, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg CancelRun failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) CloseRun(ctx context.Context, runID string, req domain.CloseRunRequest, principalID string) (*domain.PaymentRun, error) {
	var r *domain.PaymentRun
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRun(tx.QueryRow(ctx, `
			UPDATE payment_runs SET status = 'COMPLETED', close_note = $2, closed_at = NOW(), updated_at = NOW()
			WHERE run_id = $1 AND status IN ('SETTLED', 'REJECTED', 'PARTIALLY_ACCEPTED', 'EXCEPTION')
			RETURNING `+runColumns,
			runID, req.Note,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, r.TenantID, runID, domain.EventRunCompleted, req.Note, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg CloseRun failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// ── reconciliation ───────────────────────────────────────────────────────────

// ReconcileInstruction inserts an append-only reconciliation event and
// updates the instruction's cached status. Relies on the migration's
// unique index on (instruction_id, provider_event_ref) for idempotency —
// negative-path scenario #2's real, buildable equivalent (see
// internal/domain's package doc). Returns applied=false, no error, if this
// exact event was already recorded.
func (s *PgStore) ReconcileInstruction(ctx context.Context, req domain.ReconcileInstructionRequest, principalID string) (*domain.RunInstruction, bool, error) {
	var i *domain.RunInstruction
	applied := true
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM run_instructions WHERE instruction_id = $1`, req.InstructionID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrInstructionNotFound
			}
			return err
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO instruction_reconciliation_events (event_id, tenant_id, instruction_id, provider_event_ref, external_status, recorded_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			uuid.New().String(), tenantID, req.InstructionID, req.ProviderEventRef, req.ExternalStatus, principalID,
		)
		if isUniqueViolation(err) {
			applied = false
			i, err = scanInstruction(tx.QueryRow(ctx, `SELECT `+instructionColumns+` FROM run_instructions WHERE instruction_id = $1`, req.InstructionID))
			return err
		}
		if err != nil {
			return err
		}

		i, err = scanInstruction(tx.QueryRow(ctx, `
			UPDATE run_instructions SET status = $2 WHERE instruction_id = $1
			RETURNING `+instructionColumns,
			req.InstructionID, req.ExternalStatus,
		))
		return err
	})
	if errors.Is(err, domain.ErrInstructionNotFound) {
		return nil, false, domain.ErrInstructionNotFound
	}
	if err != nil {
		s.log.Error("pg ReconcileInstruction failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return i, applied, nil
}

func (s *PgStore) UpdateRunAggregateStatus(ctx context.Context, runID string, newStatus domain.RunStatus, principalID string) (*domain.PaymentRun, error) {
	var r *domain.PaymentRun
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanRun(tx.QueryRow(ctx, `
			UPDATE payment_runs SET status = $2, updated_at = NOW()
			WHERE run_id = $1 AND status IN ('SUBMITTED', 'ACCEPTED', 'REJECTED', 'PARTIALLY_ACCEPTED')
			RETURNING `+runColumns,
			runID, string(newStatus),
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, r.TenantID, runID, "PAYMENT_RUN_STATUS_"+string(newStatus), "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg UpdateRunAggregateStatus failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// RetryInstruction records that a retry was attempted — reusing the run's
// already-captured idempotency key rather than minting a new submission —
// without creating any new state. Only meaningful evidence, since there is
// no real provider to actually re-send to (see internal/domain's package
// doc).
func (s *PgStore) RetryInstruction(ctx context.Context, instructionID, principalID string) error {
	var runID string
	var tenantID *string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT run_id, tenant_id FROM run_instructions WHERE instruction_id = $1`, instructionID).Scan(&runID, &tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrInstructionNotFound
			}
			return err
		}
		return s.recordEvent(ctx, tx, tenantID, runID, domain.EventInstructionRetried, instructionID, principalID)
	})
	if errors.Is(err, domain.ErrInstructionNotFound) {
		return domain.ErrInstructionNotFound
	}
	if err != nil {
		s.log.Error("pg RetryInstruction failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// ── events ───────────────────────────────────────────────────────────────────

func (s *PgStore) ListEvents(ctx context.Context, runID string) ([]domain.RunEvent, error) {
	var out []domain.RunEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, run_id, event_type, detail, actor_principal_id, created_at
			FROM run_events WHERE run_id = $1 ORDER BY created_at ASC`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.RunEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.RunID, &e.EventType, &nullString{&e.Detail}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
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
