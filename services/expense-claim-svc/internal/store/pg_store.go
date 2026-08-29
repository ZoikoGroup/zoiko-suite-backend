// Package store implements expense-claim-svc's persistence.
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

	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/middleware"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input syntax
// for type uuid" error (SQLSTATE 22P02).
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// isUniqueViolation reports whether err is Postgres's unique-constraint
// violation (SQLSTATE 23505) — what fires when a receipt document is
// attached to a second expense line (see the migration's partial unique
// index).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isClaimNotEditable reports whether err is the custom SQLSTATE ZK001
// raised by the expense_lines BEFORE INSERT/UPDATE trigger when the parent
// claim is no longer DRAFT/RETURNED — see the migration.
func isClaimNotEditable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "ZK001"
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
	CreateClaim(ctx context.Context, tenantID string, req domain.CreateExpenseClaimRequest, principalID string) (*domain.ExpenseClaim, error)
	FindClaim(ctx context.Context, claimID string) (*domain.ExpenseClaim, error)

	AddExpenseLine(ctx context.Context, claimID string, req domain.AddExpenseLineRequest, principalID string) (*domain.ExpenseLine, error)
	ListLines(ctx context.Context, claimID string) ([]domain.ExpenseLine, error)
	SetLineTaxDetermination(ctx context.Context, lineID, determinationID string, taxableAmount, calculatedTaxAmount float64) error

	SubmitClaim(ctx context.Context, claimID string, policyResult domain.PolicyAssessmentResult, policyVersionID string, principalID string) (*domain.ExpenseClaim, error)
	ApproveClaim(ctx context.Context, claimID string, principalID string) (*domain.ExpenseClaim, error)
	RejectClaim(ctx context.Context, claimID string, req domain.RejectClaimRequest, principalID string) (*domain.ExpenseClaim, error)
	ReturnClaim(ctx context.Context, claimID string, req domain.ReturnClaimRequest, principalID string) (*domain.ExpenseClaim, error)
	CancelClaim(ctx context.Context, claimID string, req domain.CancelClaimRequest, principalID string) (*domain.ExpenseClaim, error)
	RecordPolicyException(ctx context.Context, claimID string, req domain.RecordPolicyExceptionRequest, principalID string) (*domain.ExpenseClaim, error)

	IsReceiptInUse(ctx context.Context, documentID string) (inUse bool, claimID string, lineID string, err error)
	ListClaimEvents(ctx context.Context, claimID string) ([]domain.ExpenseClaimEvent, error)
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

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, claimID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO expense_claim_events (event_id, tenant_id, claim_id, event_type, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), tenantID, claimID, eventType, detail, actorPrincipalID,
	)
	return err
}

// ── claims ───────────────────────────────────────────────────────────────────

const claimColumns = `
	claim_id, tenant_id, legal_entity_id, claimant_principal_id, currency, business_purpose,
	project_cost_center, payment_preference_ref, status, rejection_reason, return_reason,
	has_policy_exception, policy_exception_reason, policy_assessment_result, policy_version_id,
	approved_by_principal_id, approved_at, created_at, updated_at`

func scanClaim(row pgx.Row) (*domain.ExpenseClaim, error) {
	c := &domain.ExpenseClaim{}
	err := row.Scan(&c.ClaimID, &c.TenantID, &c.LegalEntityID, &c.ClaimantPrincipalID, &c.Currency, &nullString{&c.BusinessPurpose},
		&nullString{&c.ProjectCostCenter}, &nullString{&c.PaymentPreferenceRef}, &c.Status, &nullString{&c.RejectionReason},
		&nullString{&c.ReturnReason}, &c.HasPolicyException, &nullString{&c.PolicyExceptionReason}, &c.PolicyAssessmentResult,
		&nullString{&c.PolicyVersionID}, &c.ApprovedByPrincipalID, &c.ApprovedAt, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *PgStore) CreateClaim(ctx context.Context, tenantID string, req domain.CreateExpenseClaimRequest, principalID string) (*domain.ExpenseClaim, error) {
	id := uuid.New().String()
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `
			INSERT INTO expense_claims (`+claimColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'DRAFT', NULL, NULL, FALSE, NULL, 'NOT_ASSESSED', NULL, NULL, NULL, NOW(), NOW())
			RETURNING `+claimColumns,
			id, strPtrOrNil(tenantID), req.LegalEntityID, req.ClaimantPrincipalID, req.Currency,
			strPtrOrNil(req.BusinessPurpose), strPtrOrNil(req.ProjectCostCenter), strPtrOrNil(req.PaymentPreferenceRef),
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, c.TenantID, id, domain.EventClaimCreated, "", principalID)
	})
	if err != nil {
		s.log.Error("pg CreateClaim failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) FindClaim(ctx context.Context, claimID string) (*domain.ExpenseClaim, error) {
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `SELECT `+claimColumns+` FROM expense_claims WHERE claim_id = $1`, claimID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrClaimNotFound
	}
	if err != nil {
		s.log.Error("pg FindClaim failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

// ── expense lines ────────────────────────────────────────────────────────────

const lineColumns = `
	line_id, tenant_id, claim_id, merchant, expense_date, amount, currency, category, project_cost_center,
	receipt_document_id, claim_tax_recovery, jurisdiction, tax_category, tax_determination_id,
	taxable_amount, calculated_tax_amount, created_at`

func scanLine(row pgx.Row) (*domain.ExpenseLine, error) {
	l := &domain.ExpenseLine{}
	err := row.Scan(&l.LineID, &l.TenantID, &l.ClaimID, &l.Merchant, &l.ExpenseDate, &l.Amount, &l.Currency,
		&nullString{&l.Category}, &nullString{&l.ProjectCostCenter}, &nullString{&l.ReceiptDocumentID}, &l.ClaimTaxRecovery,
		&nullString{&l.Jurisdiction}, &nullString{&l.TaxCategory}, &nullString{&l.TaxDeterminationID},
		&l.TaxableAmount, &l.CalculatedTaxAmount, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// AddExpenseLine relies on the migration's BEFORE INSERT trigger to reject a
// line added to a claim that is no longer DRAFT/RETURNED, and on the
// partial unique index to reject a receipt document already attached
// elsewhere (negative-path scenario #2) — both genuine database
// invariants, not application checks a race could defeat.
func (s *PgStore) AddExpenseLine(ctx context.Context, claimID string, req domain.AddExpenseLineRequest, principalID string) (*domain.ExpenseLine, error) {
	id := uuid.New().String()
	var l *domain.ExpenseLine
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM expense_claims WHERE claim_id = $1`, claimID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrClaimNotFound
			}
			return err
		}
		var err error
		l, err = scanLine(tx.QueryRow(ctx, `
			INSERT INTO expense_lines (`+lineColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, '', 0, 0, NOW())
			RETURNING `+lineColumns,
			id, tenantID, claimID, req.Merchant, req.ExpenseDate, req.Amount, req.Currency, strPtrOrNil(req.Category),
			strPtrOrNil(req.ProjectCostCenter), strPtrOrNil(req.ReceiptDocumentID), req.ClaimTaxRecovery,
			strPtrOrNil(req.Jurisdiction), strPtrOrNil(req.TaxCategory),
		))
		return err
	})
	if errors.Is(err, domain.ErrClaimNotFound) {
		return nil, domain.ErrClaimNotFound
	}
	if isUniqueViolation(err) {
		return nil, domain.ErrDuplicateReceipt
	}
	if isClaimNotEditable(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg AddExpenseLine failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return l, nil
}

func (s *PgStore) ListLines(ctx context.Context, claimID string) ([]domain.ExpenseLine, error) {
	var out []domain.ExpenseLine
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+lineColumns+` FROM expense_lines WHERE claim_id = $1 ORDER BY created_at ASC`, claimID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLine(rows)
			if err != nil {
				return err
			}
			out = append(out, *l)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListLines failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) SetLineTaxDetermination(ctx context.Context, lineID, determinationID string, taxableAmount, calculatedTaxAmount float64) error {
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE expense_lines SET tax_determination_id = $2, taxable_amount = $3, calculated_tax_amount = $4
			WHERE line_id = $1`,
			lineID, determinationID, taxableAmount, calculatedTaxAmount,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrLineNotFound
		}
		return nil
	})
	if errors.Is(err, domain.ErrLineNotFound) {
		return domain.ErrLineNotFound
	}
	if isClaimNotEditable(err) {
		return domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg SetLineTaxDetermination failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) IsReceiptInUse(ctx context.Context, documentID string) (bool, string, string, error) {
	var claimID, lineID string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT claim_id, line_id FROM expense_lines WHERE receipt_document_id = $1 LIMIT 1`, documentID).Scan(&claimID, &lineID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return false, "", "", nil
	}
	if err != nil {
		s.log.Error("pg IsReceiptInUse failed", zap.Error(err))
		return false, "", "", fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return true, claimID, lineID, nil
}

// ── claim lifecycle ──────────────────────────────────────────────────────────

func (s *PgStore) SubmitClaim(ctx context.Context, claimID string, policyResult domain.PolicyAssessmentResult, policyVersionID string, principalID string) (*domain.ExpenseClaim, error) {
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `
			UPDATE expense_claims SET status = 'PENDING_APPROVAL', policy_assessment_result = $2, policy_version_id = $3, updated_at = NOW()
			WHERE claim_id = $1 AND status IN ('DRAFT', 'RETURNED')
			RETURNING `+claimColumns,
			claimID, string(policyResult), strPtrOrNil(policyVersionID),
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, c.TenantID, claimID, domain.EventClaimSubmitted, string(policyResult), principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg SubmitClaim failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

// ApproveClaim moves PENDING_APPROVAL directly to REIMBURSABLE — see
// internal/domain's package doc for why this service never persists a
// distinct APPROVED row status.
func (s *PgStore) ApproveClaim(ctx context.Context, claimID string, principalID string) (*domain.ExpenseClaim, error) {
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `
			UPDATE expense_claims SET status = 'REIMBURSABLE', approved_by_principal_id = $2, approved_at = NOW(), updated_at = NOW()
			WHERE claim_id = $1 AND status = 'PENDING_APPROVAL'
			RETURNING `+claimColumns,
			claimID, principalID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, c.TenantID, claimID, domain.EventClaimApproved, "", principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ApproveClaim failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) RejectClaim(ctx context.Context, claimID string, req domain.RejectClaimRequest, principalID string) (*domain.ExpenseClaim, error) {
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `
			UPDATE expense_claims SET status = 'REJECTED', rejection_reason = $2, updated_at = NOW()
			WHERE claim_id = $1 AND status = 'PENDING_APPROVAL'
			RETURNING `+claimColumns,
			claimID, req.Reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, c.TenantID, claimID, domain.EventClaimRejected, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RejectClaim failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) ReturnClaim(ctx context.Context, claimID string, req domain.ReturnClaimRequest, principalID string) (*domain.ExpenseClaim, error) {
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `
			UPDATE expense_claims SET status = 'RETURNED', return_reason = $2, updated_at = NOW()
			WHERE claim_id = $1 AND status = 'PENDING_APPROVAL'
			RETURNING `+claimColumns,
			claimID, req.Reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, c.TenantID, claimID, domain.EventClaimReturned, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ReturnClaim failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) CancelClaim(ctx context.Context, claimID string, req domain.CancelClaimRequest, principalID string) (*domain.ExpenseClaim, error) {
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `
			UPDATE expense_claims SET status = 'CANCELLED', updated_at = NOW()
			WHERE claim_id = $1 AND status IN ('DRAFT', 'PENDING_APPROVAL', 'RETURNED')
			RETURNING `+claimColumns,
			claimID,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, c.TenantID, claimID, domain.EventClaimCancelled, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg CancelClaim failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

func (s *PgStore) RecordPolicyException(ctx context.Context, claimID string, req domain.RecordPolicyExceptionRequest, principalID string) (*domain.ExpenseClaim, error) {
	var c *domain.ExpenseClaim
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = scanClaim(tx.QueryRow(ctx, `
			UPDATE expense_claims SET has_policy_exception = TRUE, policy_exception_reason = $2, updated_at = NOW()
			WHERE claim_id = $1 AND status = 'PENDING_APPROVAL'
			RETURNING `+claimColumns,
			claimID, req.Reason,
		))
		if err != nil {
			return err
		}
		return s.recordEvent(ctx, tx, c.TenantID, claimID, domain.EventPolicyExceptionRecorded, req.Reason, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RecordPolicyException failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return c, nil
}

// ── events ───────────────────────────────────────────────────────────────────

func (s *PgStore) ListClaimEvents(ctx context.Context, claimID string) ([]domain.ExpenseClaimEvent, error) {
	var out []domain.ExpenseClaimEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, claim_id, event_type, detail, actor_principal_id, created_at
			FROM expense_claim_events WHERE claim_id = $1 ORDER BY created_at ASC`, claimID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ExpenseClaimEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.ClaimID, &e.EventType, &nullString{&e.Detail}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
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
		s.log.Error("pg ListClaimEvents failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
