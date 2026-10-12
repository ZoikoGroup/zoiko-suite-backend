// Package store is the Postgres persistence layer for supplier-recovery-svc.
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

	"zoiko.io/supplier-recovery-svc/internal/domain"
	"zoiko.io/supplier-recovery-svc/internal/middleware"
	"zoiko.io/supplier-recovery-svc/internal/outbox"
)

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

type Store interface {
	CreateCase(ctx context.Context, tenantID string, req domain.CreateCaseRequest, principalID string) (*domain.SupplierRecoveryCase, error)
	FindCase(ctx context.Context, caseID string) (*domain.SupplierRecoveryCase, error)
	ListOpenCases(ctx context.Context, legalEntityID string) ([]domain.SupplierRecoveryCase, error)

	ApproveRecoveryPlan(ctx context.Context, caseID, principalID string) (*domain.SupplierRecoveryCase, error)
	RecordCommitment(ctx context.Context, caseID string, req domain.RecordCommitmentRequest, principalID string) (*domain.RecoveryCommitment, error)
	ApplyRecovery(ctx context.Context, caseID, appType string, amount float64, idempotencyRef, detail, principalID string) (*domain.SupplierRecoveryCase, bool, error)
	EscalateCase(ctx context.Context, caseID string, req domain.EscalateRequest, principalID string) (*domain.SupplierRecoveryCase, error)
	WriteOffCase(ctx context.Context, caseID string, req domain.WriteOffRequest, principalID string) (*domain.SupplierRecoveryCase, error)
	CloseCase(ctx context.Context, caseID string, req domain.CloseCaseRequest, principalID string) (*domain.SupplierRecoveryCase, error)

	ListApplications(ctx context.Context, caseID string) ([]domain.RecoveryApplication, error)
	ListCommitments(ctx context.Context, caseID string) ([]domain.RecoveryCommitment, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// Pool exposes the pool so the outbox relay can share it.
func (s *PgStore) Pool() *pgxpool.Pool { return s.pool }

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

const caseColumns = `
	case_id, tenant_id, legal_entity_id, supplier_ref, recovery_basis, source_payable_id,
	total_amount::float8, recovered_amount::float8, currency, recovery_reason, status,
	escalation_reason, write_off_reason, close_note,
	created_by_principal_id, approved_by_principal_id, created_at, updated_at`

func scanCase(row pgx.Row) (*domain.SupplierRecoveryCase, error) {
	c := &domain.SupplierRecoveryCase{}
	err := row.Scan(&c.CaseID, &c.TenantID, &c.LegalEntityID, &c.SupplierRef, &c.RecoveryBasis, &c.SourcePayableID,
		&c.TotalAmount, &c.RecoveredAmount, &c.Currency, &c.RecoveryReason, &c.Status,
		&c.EscalationReason, &c.WriteOffReason, &c.CloseNote,
		&c.CreatedByPrincipalID, &c.ApprovedByPrincipalID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// tenantOf is the request's verified tenant. Every query also filters on it
// explicitly: row-level security is the backstop, not the only isolation, so a
// deployment that connects as a role which bypasses RLS is still tenant-safe.
func tenantOf(ctx context.Context) string { return middleware.TenantFromContext(ctx) }

func nullableTenant(tenantID string) *string {
	if tenantID == "" {
		return nil
	}
	return &tenantID
}

// emit writes one domain event to outbox_events inside the caller's
// transaction, so the state change and its event commit or roll back
// together. The payload is the Variant B envelope the relay publishes verbatim.
func (s *PgStore) emit(ctx context.Context, tx pgx.Tx, c *domain.SupplierRecoveryCase, eventType, actorPrincipalID string, payload any) error {
	id := uuid.New().String()
	var corr *string
	if v := middleware.CorrelationFromContext(ctx); v != "" {
		corr = &v
	}
	actor := actorPrincipalID
	return outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: id, AggregateType: "supplier_recovery_case", AggregateID: c.CaseID, EventType: eventType,
		TenantID: c.TenantID, LegalEntityID: c.LegalEntityID, ActorID: &actor, CorrelationID: corr,
		Payload: outbox.NewVariantBEnvelope(id, eventType, c.CaseID, c.TenantID, &actor, corr, payload),
	})
}

func (s *PgStore) unavailable(op string, err error) error {
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

func (s *PgStore) CreateCase(ctx context.Context, tenantID string, req domain.CreateCaseRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	caseID := uuid.New().String()
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `
			INSERT INTO supplier_recovery_cases (
				case_id, tenant_id, legal_entity_id, supplier_ref, recovery_basis, source_payable_id,
				total_amount, recovered_amount, currency, recovery_reason, status,
				escalation_reason, write_off_reason, close_note,
				created_by_principal_id, approved_by_principal_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8, $9, 'OPEN', '', '', '', $10, '', NOW(), NOW())
			RETURNING `+caseColumns,
			caseID, nullableTenant(tenantID), req.LegalEntityID, req.SupplierRef, req.RecoveryBasis, req.SourcePayableID,
			req.TotalAmount, req.Currency, req.RecoveryReason, principalID,
		))
		if err != nil {
			return err
		}
		return s.emit(ctx, tx, c, domain.EventRecoveryCaseCreated, principalID, c)
	})
	if err != nil {
		return nil, s.unavailable("CreateCase", err)
	}
	return c, nil
}

func (s *PgStore) FindCase(ctx context.Context, caseID string) (*domain.SupplierRecoveryCase, error) {
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM supplier_recovery_cases WHERE case_id = $1 AND tenant_id::text = $2`, caseID, tenantOf(ctx)))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrCaseNotFound
	}
	if err != nil {
		return nil, s.unavailable("FindCase", err)
	}
	return c, nil
}

func (s *PgStore) ListOpenCases(ctx context.Context, legalEntityID string) ([]domain.SupplierRecoveryCase, error) {
	var out []domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+caseColumns+` FROM supplier_recovery_cases
			WHERE legal_entity_id = $1 AND tenant_id::text = $2 AND status NOT IN ('CLOSED', 'WRITTEN_OFF')
			ORDER BY created_at ASC`, legalEntityID, tenantOf(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCase(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, s.unavailable("ListOpenCases", err)
	}
	return out, nil
}

// transition runs one guarded status UPDATE and the matching outbox event in a
// single transaction. The guard lives in the UPDATE's WHERE clause (the same
// rule as the domain's Can* predicates), so a concurrent command cannot slip
// past a stale read: no row means the case is not in a state that allows it.
func (s *PgStore) transition(ctx context.Context, op, eventType, principalID, sql string, args ...any) (*domain.SupplierRecoveryCase, error) {
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, sql, args...))
		if err != nil {
			return err
		}
		return s.emit(ctx, tx, c, eventType, principalID, c)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		return nil, s.unavailable(op, err)
	}
	return c, nil
}

func (s *PgStore) ApproveRecoveryPlan(ctx context.Context, caseID, principalID string) (*domain.SupplierRecoveryCase, error) {
	return s.transition(ctx, "ApproveRecoveryPlan", domain.EventRecoveryPlanApproved, principalID, `
		UPDATE supplier_recovery_cases SET status = 'IN_RECOVERY', approved_by_principal_id = $1, updated_at = NOW()
		WHERE case_id = $2 AND tenant_id::text = $3 AND status = 'OPEN'
		RETURNING `+caseColumns, principalID, caseID, tenantOf(ctx))
}

func (s *PgStore) RecordCommitment(ctx context.Context, caseID string, req domain.RecordCommitmentRequest, principalID string) (*domain.RecoveryCommitment, error) {
	var commitment *domain.RecoveryCommitment
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		c, err := scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM supplier_recovery_cases WHERE case_id = $1 AND tenant_id::text = $2`, caseID, tenantOf(ctx)))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			return err
		}
		commitment = &domain.RecoveryCommitment{}
		if err := tx.QueryRow(ctx, `
			INSERT INTO recovery_commitments (commitment_id, tenant_id, case_id, detail, expected_method, actor_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING commitment_id, tenant_id, case_id, detail, expected_method, actor_principal_id, created_at`,
			uuid.New().String(), c.TenantID, caseID, req.Detail, req.ExpectedMethod, principalID,
		).Scan(&commitment.CommitmentID, &commitment.TenantID, &commitment.CaseID, &commitment.Detail,
			&commitment.ExpectedMethod, &commitment.ActorPrincipalID, &commitment.CreatedAt); err != nil {
			return err
		}
		return s.emit(ctx, tx, c, domain.EventCommitmentRecorded, principalID, commitment)
	})
	if errors.Is(err, domain.ErrCaseNotFound) || isInvalidUUID(err) {
		return nil, domain.ErrCaseNotFound
	}
	if err != nil {
		return nil, s.unavailable("RecordCommitment", err)
	}
	return commitment, nil
}

// ApplyRecovery is the shared path both ApplyApprovedOffset and
// LinkConfirmedSupplierRefund go through. The recovered amount and status are
// recomputed in SQL from the case's own row (never from a caller-supplied
// total), with NUMERIC arithmetic so cents never drift.
//
// Idempotency is decided FIRST, by INSERT ... ON CONFLICT DO NOTHING on
// (case, type, reference): a replay that already fully recovered the case
// returns the idempotent no-op rather than a wrongly-rejected "already
// terminal" error. (A plain INSERT that failed on the unique index would abort
// the transaction and make the follow-up read impossible.) Every refusal after
// the insert rolls the application row back with the transaction.
func (s *PgStore) ApplyRecovery(ctx context.Context, caseID, appType string, amount float64, idempotencyRef, detail, principalID string) (*domain.SupplierRecoveryCase, bool, error) {
	var c *domain.SupplierRecoveryCase
	var applied bool
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		cur, err := scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM supplier_recovery_cases WHERE case_id = $1 AND tenant_id::text = $2 FOR UPDATE`, caseID, tenantOf(ctx)))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			return err
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO recovery_applications (application_id, tenant_id, case_id, application_type, amount, idempotency_ref, detail, actor_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (case_id, application_type, idempotency_ref) DO NOTHING`,
			uuid.New().String(), cur.TenantID, caseID, appType, amount, idempotencyRef, detail, principalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			applied, c = false, cur
			return nil
		}

		if !domain.CanApplyRecovery(cur.Status) {
			return domain.ErrInvalidTransition
		}
		c, err = scanCase(tx.QueryRow(ctx, `
			UPDATE supplier_recovery_cases
			SET recovered_amount = recovered_amount + $1::numeric,
			    status = CASE WHEN recovered_amount + $1::numeric >= total_amount THEN 'RECOVERED' ELSE 'PARTIALLY_RECOVERED' END,
			    updated_at = NOW()
			WHERE case_id = $2 AND tenant_id::text = $3 AND recovered_amount + $1::numeric <= total_amount
			RETURNING `+caseColumns, amount, caseID, tenantOf(ctx)))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRecoveryExceedsOutstanding
		}
		if err != nil {
			return err
		}
		eventType := domain.EventRecoveryOffsetApplied
		if appType == "REFUND" {
			eventType = domain.EventSupplierRefundConfirmed
		}
		applied = true
		return s.emit(ctx, tx, c, eventType, principalID, c)
	})
	if errors.Is(err, domain.ErrCaseNotFound) || isInvalidUUID(err) {
		return nil, false, domain.ErrCaseNotFound
	}
	if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrRecoveryExceedsOutstanding) {
		return nil, false, err
	}
	if err != nil {
		return nil, false, s.unavailable("ApplyRecovery", err)
	}
	return c, applied, nil
}

// EscalateCase mirrors domain.CanEscalate: OPEN or an active recovery status.
func (s *PgStore) EscalateCase(ctx context.Context, caseID string, req domain.EscalateRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	return s.transition(ctx, "EscalateCase", domain.EventRecoveryEscalated, principalID, `
		UPDATE supplier_recovery_cases SET status = 'ESCALATED', escalation_reason = $1, updated_at = NOW()
		WHERE case_id = $2 AND tenant_id::text = $3 AND status IN ('OPEN', 'APPROVED', 'IN_RECOVERY', 'PARTIALLY_RECOVERED')
		RETURNING `+caseColumns, req.Reason, caseID, tenantOf(ctx))
}

// WriteOffCase mirrors domain.CanWriteOff: OPEN, an active recovery status, or ESCALATED.
func (s *PgStore) WriteOffCase(ctx context.Context, caseID string, req domain.WriteOffRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	return s.transition(ctx, "WriteOffCase", domain.EventRecoveryWrittenOff, principalID, `
		UPDATE supplier_recovery_cases SET status = 'WRITTEN_OFF', write_off_reason = $1, updated_at = NOW()
		WHERE case_id = $2 AND tenant_id::text = $3 AND status IN ('OPEN', 'APPROVED', 'IN_RECOVERY', 'PARTIALLY_RECOVERED', 'ESCALATED')
		RETURNING `+caseColumns, req.Reason, caseID, tenantOf(ctx))
}

// CloseCase mirrors domain.CanClose: only a fully RECOVERED case closes.
func (s *PgStore) CloseCase(ctx context.Context, caseID string, req domain.CloseCaseRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	return s.transition(ctx, "CloseCase", domain.EventRecoveryClosed, principalID, `
		UPDATE supplier_recovery_cases SET status = 'CLOSED', close_note = $1, updated_at = NOW()
		WHERE case_id = $2 AND tenant_id::text = $3 AND status = 'RECOVERED'
		RETURNING `+caseColumns, req.Note, caseID, tenantOf(ctx))
}

func (s *PgStore) ListApplications(ctx context.Context, caseID string) ([]domain.RecoveryApplication, error) {
	var out []domain.RecoveryApplication
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT application_id, tenant_id, case_id, application_type, amount::float8, idempotency_ref, detail, actor_principal_id, created_at
			FROM recovery_applications WHERE case_id = $1 AND tenant_id::text = $2 ORDER BY created_at ASC`, caseID, tenantOf(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.RecoveryApplication
			if err := rows.Scan(&a.ApplicationID, &a.TenantID, &a.CaseID, &a.ApplicationType, &a.Amount,
				&a.IdempotencyRef, &a.Detail, &a.ActorPrincipalID, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.unavailable("ListApplications", err)
	}
	return out, nil
}

func (s *PgStore) ListCommitments(ctx context.Context, caseID string) ([]domain.RecoveryCommitment, error) {
	var out []domain.RecoveryCommitment
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT commitment_id, tenant_id, case_id, detail, expected_method, actor_principal_id, created_at
			FROM recovery_commitments WHERE case_id = $1 AND tenant_id::text = $2 ORDER BY created_at ASC`, caseID, tenantOf(ctx))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.RecoveryCommitment
			if err := rows.Scan(&c.CommitmentID, &c.TenantID, &c.CaseID, &c.Detail, &c.ExpectedMethod, &c.ActorPrincipalID, &c.CreatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.unavailable("ListCommitments", err)
	}
	return out, nil
}
