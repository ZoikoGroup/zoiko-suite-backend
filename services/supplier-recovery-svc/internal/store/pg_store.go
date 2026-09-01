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
)

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
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
	total_amount, recovered_amount, currency, recovery_reason, status,
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

func nullableTenant(tenantID string) *string {
	if tenantID == "" {
		return nil
	}
	return &tenantID
}

func (s *PgStore) recordApplication(ctx context.Context, tx pgx.Tx, tenantID *string, caseID, appType string, amount float64, idempotencyRef, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO recovery_applications (application_id, tenant_id, case_id, application_type, amount, idempotency_ref, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New().String(), tenantID, caseID, appType, amount, idempotencyRef, detail, actorPrincipalID,
	)
	return err
}

func (s *PgStore) CreateCase(ctx context.Context, tenantID string, req domain.CreateCaseRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	caseID := uuid.New().String()
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `
			INSERT INTO supplier_recovery_cases (`+caseColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8, $9, 'OPEN', '', '', '', $10, '', NOW(), NOW())
			RETURNING `+caseColumns,
			caseID, nullableTenant(tenantID), req.LegalEntityID, req.SupplierRef, req.RecoveryBasis, req.SourcePayableID,
			req.TotalAmount, req.Currency, req.RecoveryReason, principalID,
		))
		return err
	})
	if err != nil {
		s.log.Error("pg CreateCase failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) FindCase(ctx context.Context, caseID string) (*domain.SupplierRecoveryCase, error) {
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM supplier_recovery_cases WHERE case_id = $1`, caseID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrCaseNotFound
	}
	if err != nil {
		s.log.Error("pg FindCase failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) ListOpenCases(ctx context.Context, legalEntityID string) ([]domain.SupplierRecoveryCase, error) {
	var out []domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+caseColumns+` FROM supplier_recovery_cases
			WHERE legal_entity_id = $1 AND status NOT IN ('CLOSED', 'WRITTEN_OFF')
			ORDER BY created_at ASC`, legalEntityID)
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
		s.log.Error("pg ListOpenCases failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ApproveRecoveryPlan(ctx context.Context, caseID, principalID string) (*domain.SupplierRecoveryCase, error) {
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `
			UPDATE supplier_recovery_cases SET status = 'IN_RECOVERY', approved_by_principal_id = $1, updated_at = NOW()
			WHERE case_id = $2 AND status = 'OPEN'
			RETURNING `+caseColumns,
			principalID, caseID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ApproveRecoveryPlan failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) RecordCommitment(ctx context.Context, caseID string, req domain.RecordCommitmentRequest, principalID string) (*domain.RecoveryCommitment, error) {
	var commitment *domain.RecoveryCommitment
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM supplier_recovery_cases WHERE case_id = $1`, caseID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			return err
		}
		commitment = &domain.RecoveryCommitment{}
		return tx.QueryRow(ctx, `
			INSERT INTO recovery_commitments (commitment_id, tenant_id, case_id, detail, expected_method, actor_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING commitment_id, tenant_id, case_id, detail, expected_method, actor_principal_id, created_at`,
			uuid.New().String(), tenantID, caseID, req.Detail, req.ExpectedMethod, principalID,
		).Scan(&commitment.CommitmentID, &commitment.TenantID, &commitment.CaseID, &commitment.Detail,
			&commitment.ExpectedMethod, &commitment.ActorPrincipalID, &commitment.CreatedAt)
	})
	if errors.Is(err, domain.ErrCaseNotFound) || isInvalidUUID(err) {
		return nil, domain.ErrCaseNotFound
	}
	if err != nil {
		s.log.Error("pg RecordCommitment failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return commitment, nil
}

// ApplyRecovery is the shared path both ApplyApprovedOffset and
// LinkConfirmedSupplierRefund go through — it recomputes RecoveredAmount
// and status server-side from the case's own row (never a caller-supplied
// total), and the idempotency insert is attempted BEFORE the
// state-transition guard, exactly like payable-open-item-svc's own
// applyResidualDelta: a replayed application that already fully recovered
// the case on its first application must return the idempotent no-op, not
// a wrongly-rejected "already terminal" error.
func (s *PgStore) ApplyRecovery(ctx context.Context, caseID, appType string, amount float64, idempotencyRef, detail, principalID string) (*domain.SupplierRecoveryCase, bool, error) {
	var c *domain.SupplierRecoveryCase
	var applied bool
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		var recovered, total float64
		var status domain.CaseStatus
		if err := tx.QueryRow(ctx, `SELECT tenant_id, recovered_amount, total_amount, status FROM supplier_recovery_cases WHERE case_id = $1 FOR UPDATE`, caseID).
			Scan(&tenantID, &recovered, &total, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCaseNotFound
			}
			return err
		}

		if err := s.recordApplication(ctx, tx, tenantID, caseID, appType, amount, idempotencyRef, detail, principalID); err != nil {
			if isUniqueViolation(err) {
				applied = false
				var err2 error
				c, err2 = scanCase(tx.QueryRow(ctx, `SELECT `+caseColumns+` FROM supplier_recovery_cases WHERE case_id = $1`, caseID))
				return err2
			}
			return err
		}

		if !domain.CanApplyRecovery(status) {
			return domain.ErrInvalidTransition
		}
		newRecovered := recovered + amount
		if newRecovered > total {
			return domain.ErrRecoveryExceedsOutstanding
		}
		newStatus := domain.StatusPartiallyRecovered
		if newRecovered >= total {
			newStatus = domain.StatusRecovered
		}

		var err error
		c, err = scanCase(tx.QueryRow(ctx, `
			UPDATE supplier_recovery_cases SET recovered_amount = $1, status = $2, updated_at = NOW()
			WHERE case_id = $3
			RETURNING `+caseColumns,
			newRecovered, newStatus, caseID,
		))
		applied = true
		return err
	})
	if errors.Is(err, domain.ErrCaseNotFound) || isInvalidUUID(err) {
		return nil, false, domain.ErrCaseNotFound
	}
	if errors.Is(err, domain.ErrInvalidTransition) || errors.Is(err, domain.ErrRecoveryExceedsOutstanding) {
		return nil, false, err
	}
	if err != nil {
		s.log.Error("pg ApplyRecovery failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, applied, nil
}

func (s *PgStore) EscalateCase(ctx context.Context, caseID string, req domain.EscalateRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `
			UPDATE supplier_recovery_cases SET status = 'ESCALATED', escalation_reason = $1, updated_at = NOW()
			WHERE case_id = $2 AND status NOT IN ('CLOSED', 'WRITTEN_OFF', 'ESCALATED')
			RETURNING `+caseColumns,
			req.Reason, caseID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg EscalateCase failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) WriteOffCase(ctx context.Context, caseID string, req domain.WriteOffRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `
			UPDATE supplier_recovery_cases SET status = 'WRITTEN_OFF', write_off_reason = $1, updated_at = NOW()
			WHERE case_id = $2 AND status NOT IN ('CLOSED', 'WRITTEN_OFF', 'RECOVERED')
			RETURNING `+caseColumns,
			req.Reason, caseID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg WriteOffCase failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) CloseCase(ctx context.Context, caseID string, req domain.CloseCaseRequest, principalID string) (*domain.SupplierRecoveryCase, error) {
	var c *domain.SupplierRecoveryCase
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanCase(tx.QueryRow(ctx, `
			UPDATE supplier_recovery_cases SET status = 'CLOSED', close_note = $1, updated_at = NOW()
			WHERE case_id = $2 AND status = 'RECOVERED'
			RETURNING `+caseColumns,
			req.Note, caseID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg CloseCase failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) ListApplications(ctx context.Context, caseID string) ([]domain.RecoveryApplication, error) {
	var out []domain.RecoveryApplication
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT application_id, tenant_id, case_id, application_type, amount, idempotency_ref, detail, actor_principal_id, created_at
			FROM recovery_applications WHERE case_id = $1 ORDER BY created_at ASC`, caseID)
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
		s.log.Error("pg ListApplications failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) ListCommitments(ctx context.Context, caseID string) ([]domain.RecoveryCommitment, error) {
	var out []domain.RecoveryCommitment
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT commitment_id, tenant_id, case_id, detail, expected_method, actor_principal_id, created_at
			FROM recovery_commitments WHERE case_id = $1 ORDER BY created_at ASC`, caseID)
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
		s.log.Error("pg ListCommitments failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
