// Package store implements goods-service-receipt-svc's persistence.
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

	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/middleware"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input syntax
// for type uuid" error (SQLSTATE 22P02) — see supplier-financial-profile-svc
// (AP-01) for the full rationale. Applied from the start here.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// nullString bridges a nullable TEXT column onto a plain (non-pointer)
// string field.
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
	CreateReceipt(ctx context.Context, tenantID string, req domain.CreateReceiptRequest, principalID string) (*domain.GoodsServiceReceipt, error)
	FindReceipt(ctx context.Context, receiptID string) (*domain.GoodsServiceReceipt, error)
	ListReceiptsForPO(ctx context.Context, purchaseOrderID string) ([]domain.GoodsServiceReceipt, error)
	AmendReceiptDraft(ctx context.Context, receiptID string, req domain.AmendReceiptDraftRequest, principalID string) (*domain.GoodsServiceReceipt, error)
	ConfirmReceipt(ctx context.Context, receiptID string, principalID string) (*domain.GoodsServiceReceipt, error)
	RejectReceipt(ctx context.Context, receiptID string, req domain.RejectReceiptRequest, principalID string) (*domain.GoodsServiceReceipt, error)
	ReverseReceipt(ctx context.Context, receiptID string, req domain.ReverseReceiptRequest, principalID string) (*domain.GoodsServiceReceipt, error)
	RecordServiceAcceptance(ctx context.Context, receiptID string, req domain.RecordServiceAcceptanceRequest, principalID string) (*domain.GoodsServiceReceipt, error)

	AttachReceiptEvidence(ctx context.Context, receiptID string, req domain.AttachReceiptEvidenceRequest, principalID string) (*domain.ReceiptEvidence, error)
	ListReceiptEvidence(ctx context.Context, receiptID string) ([]domain.ReceiptEvidence, error)

	SumNetConfirmedAmountForPO(ctx context.Context, purchaseOrderID string) (float64, error)

	RecordAccountingEvent(ctx context.Context, receiptID string, status domain.AccountingEventStatus, journalID *string, failureReason string) (*domain.ReceiptAccountingEvent, error)
	GetLatestAccountingEvent(ctx context.Context, receiptID string) (*domain.ReceiptAccountingEvent, error)
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

// ── receipts ─────────────────────────────────────────────────────────────────

const receiptColumns = `
	receipt_id, tenant_id, legal_entity_id, purchase_order_id, receipt_type, quantity, unit_of_measure,
	amount, currency_code, receipt_date, location, inspection_result, requires_independent_acceptance,
	tolerance_exception_ref, status, rejection_reason, reversed_amount, receiver_principal_id,
	created_by_principal_id, confirmed_by_principal_id, confirmed_at, created_at, updated_at`

func scanReceipt(row pgx.Row) (*domain.GoodsServiceReceipt, error) {
	r := &domain.GoodsServiceReceipt{}
	err := row.Scan(&r.ReceiptID, &r.TenantID, &r.LegalEntityID, &r.PurchaseOrderID, &r.ReceiptType, &r.Quantity, &r.UnitOfMeasure,
		&r.Amount, &r.CurrencyCode, &r.ReceiptDate, &nullString{&r.Location}, &nullString{&r.InspectionResult}, &r.RequiresIndependentAcceptance,
		&nullString{&r.ToleranceExceptionRef}, &r.Status, &nullString{&r.RejectionReason}, &r.ReversedAmount, &r.ReceiverPrincipalID,
		&r.CreatedByPrincipalID, &r.ConfirmedByPrincipalID, &r.ConfirmedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s *PgStore) CreateReceipt(ctx context.Context, tenantID string, req domain.CreateReceiptRequest, principalID string) (*domain.GoodsServiceReceipt, error) {
	id := uuid.New().String()
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanReceipt(tx.QueryRow(ctx, `
			INSERT INTO goods_service_receipts (`+receiptColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 'DRAFT', NULL, 0, $15, $16, NULL, NULL, NOW(), NOW())
			RETURNING `+receiptColumns,
			id, strPtrOrNil(tenantID), req.LegalEntityID, req.PurchaseOrderID, req.ReceiptType, req.Quantity, req.UnitOfMeasure,
			req.Amount, req.CurrencyCode, req.ReceiptDate, strPtrOrNil(req.Location), strPtrOrNil(req.InspectionResult),
			req.RequiresIndependentAcceptance, strPtrOrNil(req.ToleranceExceptionRef), principalID, principalID,
		))
		return err
	})
	if err != nil {
		s.log.Error("pg CreateReceipt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) FindReceipt(ctx context.Context, receiptID string) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanReceipt(tx.QueryRow(ctx, `SELECT `+receiptColumns+` FROM goods_service_receipts WHERE receipt_id = $1`, receiptID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrReceiptNotFound
	}
	if err != nil {
		s.log.Error("pg FindReceipt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) ListReceiptsForPO(ctx context.Context, purchaseOrderID string) ([]domain.GoodsServiceReceipt, error) {
	var out []domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+receiptColumns+` FROM goods_service_receipts WHERE purchase_order_id = $1 ORDER BY created_at ASC`, purchaseOrderID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanReceipt(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListReceiptsForPO failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) AmendReceiptDraft(ctx context.Context, receiptID string, req domain.AmendReceiptDraftRequest, principalID string) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET
				quantity = COALESCE($2, quantity),
				unit_of_measure = COALESCE($3, unit_of_measure),
				amount = COALESCE($4, amount),
				location = COALESCE($5, location),
				inspection_result = COALESCE($6, inspection_result),
				updated_at = NOW()
			WHERE receipt_id = $1 AND status = 'DRAFT'
			RETURNING `+receiptColumns,
			receiptID, req.Quantity, req.UnitOfMeasure, req.Amount, req.Location, req.InspectionResult,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg AmendReceiptDraft failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) ConfirmReceipt(ctx context.Context, receiptID string, principalID string) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET status = 'CONFIRMED', confirmed_by_principal_id = $2, confirmed_at = NOW(), updated_at = NOW()
			WHERE receipt_id = $1 AND status IN ('DRAFT', 'PENDING_CONFIRMATION')
			RETURNING `+receiptColumns,
			receiptID, principalID,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ConfirmReceipt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

func (s *PgStore) RejectReceipt(ctx context.Context, receiptID string, req domain.RejectReceiptRequest, principalID string) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET status = 'REJECTED', rejection_reason = $2, updated_at = NOW()
			WHERE receipt_id = $1 AND status IN ('DRAFT', 'PENDING_CONFIRMATION')
			RETURNING `+receiptColumns,
			receiptID, req.Reason,
		))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RejectReceipt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// ReverseReceipt records an append-only ReceiptReversal and recomputes the
// receipt's cumulative reversed_amount/status atomically. Multiple partial
// reversals may accumulate up to (never past) the original Amount — enforced
// here, not left to the caller to get right.
func (s *PgStore) ReverseReceipt(ctx context.Context, receiptID string, req domain.ReverseReceiptRequest, principalID string) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanReceipt(tx.QueryRow(ctx, `SELECT `+receiptColumns+` FROM goods_service_receipts WHERE receipt_id = $1 AND status IN ('CONFIRMED', 'PARTIALLY_REVERSED') FOR UPDATE`, receiptID))
		if err != nil {
			return err
		}
		newReversedAmount := current.ReversedAmount + req.ReversedAmount
		if newReversedAmount > current.Amount+0.0001 {
			return domain.ErrOverReversal
		}
		newStatus := domain.StatusPartiallyReversed
		if newReversedAmount >= current.Amount-0.0001 {
			newStatus = domain.StatusFullyReversed
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO receipt_reversals (reversal_id, tenant_id, receipt_id, reversed_amount, reason, reversed_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			uuid.New().String(), current.TenantID, receiptID, req.ReversedAmount, req.Reason, principalID,
		); err != nil {
			return err
		}

		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET status = $2, reversed_amount = $3, updated_at = NOW()
			WHERE receipt_id = $1
			RETURNING `+receiptColumns,
			receiptID, newStatus, newReversedAmount,
		))
		return err
	})
	if errors.Is(err, domain.ErrOverReversal) {
		return nil, domain.ErrOverReversal
	}
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ReverseReceipt failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// RecordServiceAcceptance records acceptance evidence and moves a Draft
// service receipt to PendingConfirmation, ready for ConfirmReceipt. The
// caller (handler) is responsible for the authorization-svc own-object SoD
// check BEFORE calling this when RequiresIndependentAcceptance is set.
func (s *PgStore) RecordServiceAcceptance(ctx context.Context, receiptID string, req domain.RecordServiceAcceptanceRequest, principalID string) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET status = 'PENDING_CONFIRMATION', updated_at = NOW()
			WHERE receipt_id = $1 AND status = 'DRAFT'
			RETURNING `+receiptColumns,
			receiptID,
		))
		if err != nil {
			return err
		}
		if req.EvidenceRef != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO receipt_evidence (evidence_id, tenant_id, receipt_id, evidence_ref, description, recorded_by_principal_id)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				uuid.New().String(), r.TenantID, receiptID, req.EvidenceRef, req.Notes, principalID,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RecordServiceAcceptance failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return r, nil
}

// ── evidence ─────────────────────────────────────────────────────────────────

func (s *PgStore) AttachReceiptEvidence(ctx context.Context, receiptID string, req domain.AttachReceiptEvidenceRequest, principalID string) (*domain.ReceiptEvidence, error) {
	var e domain.ReceiptEvidence
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM goods_service_receipts WHERE receipt_id = $1`, receiptID).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
				return domain.ErrReceiptNotFound
			}
			return err
		}
		id := uuid.New().String()
		return tx.QueryRow(ctx, `
			INSERT INTO receipt_evidence (evidence_id, tenant_id, receipt_id, evidence_ref, description, recorded_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING evidence_id, tenant_id, receipt_id, evidence_ref, description, recorded_by_principal_id, created_at`,
			id, tenantID, receiptID, req.EvidenceRef, req.Description, principalID,
		).Scan(&e.EvidenceID, &e.TenantID, &e.ReceiptID, &e.EvidenceRef, &nullString{&e.Description}, &e.RecordedByPrincipalID, &e.CreatedAt)
	})
	if errors.Is(err, domain.ErrReceiptNotFound) {
		return nil, domain.ErrReceiptNotFound
	}
	if err != nil {
		s.log.Error("pg AttachReceiptEvidence failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &e, nil
}

func (s *PgStore) ListReceiptEvidence(ctx context.Context, receiptID string) ([]domain.ReceiptEvidence, error) {
	var out []domain.ReceiptEvidence
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT evidence_id, tenant_id, receipt_id, evidence_ref, description, recorded_by_principal_id, created_at
			FROM receipt_evidence WHERE receipt_id = $1 ORDER BY created_at ASC`, receiptID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.ReceiptEvidence
			if err := rows.Scan(&e.EvidenceID, &e.TenantID, &e.ReceiptID, &e.EvidenceRef, &nullString{&e.Description}, &e.RecordedByPrincipalID, &e.CreatedAt); err != nil {
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
		s.log.Error("pg ListReceiptEvidence failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── PO amount aggregation ────────────────────────────────────────────────────

// SumNetConfirmedAmountForPO sums (amount - reversed_amount) across every
// receipt against purchaseOrderID that has ever been confirmed (CONFIRMED,
// PARTIALLY_REVERSED or FULLY_REVERSED) — the real, aggregate-amount
// equivalent of "received to date" that purchase-order-svc's header-only
// model cannot itself provide. See internal/domain's package doc.
func (s *PgStore) SumNetConfirmedAmountForPO(ctx context.Context, purchaseOrderID string) (float64, error) {
	var total float64
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount - reversed_amount), 0)
			FROM goods_service_receipts
			WHERE purchase_order_id = $1 AND status IN ('CONFIRMED', 'PARTIALLY_REVERSED', 'FULLY_REVERSED')`,
			purchaseOrderID,
		).Scan(&total)
	})
	if isInvalidUUID(err) {
		return 0, nil
	}
	if err != nil {
		s.log.Error("pg SumNetConfirmedAmountForPO failed", zap.Error(err))
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return total, nil
}

// ── accounting events ────────────────────────────────────────────────────────

func (s *PgStore) RecordAccountingEvent(ctx context.Context, receiptID string, status domain.AccountingEventStatus, journalID *string, failureReason string) (*domain.ReceiptAccountingEvent, error) {
	var e domain.ReceiptAccountingEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var tenantID *string
		if err := tx.QueryRow(ctx, `SELECT tenant_id FROM goods_service_receipts WHERE receipt_id = $1`, receiptID).Scan(&tenantID); err != nil {
			return err
		}
		id := uuid.New().String()
		return tx.QueryRow(ctx, `
			INSERT INTO receipt_accounting_events (event_id, tenant_id, receipt_id, status, journal_id, failure_reason)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING event_id, tenant_id, receipt_id, status, journal_id, failure_reason, created_at`,
			id, tenantID, receiptID, status, journalID, failureReason,
		).Scan(&e.EventID, &e.TenantID, &e.ReceiptID, &e.Status, &e.JournalID, &nullString{&e.FailureReason}, &e.CreatedAt)
	})
	if err != nil {
		s.log.Error("pg RecordAccountingEvent failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &e, nil
}

func (s *PgStore) GetLatestAccountingEvent(ctx context.Context, receiptID string) (*domain.ReceiptAccountingEvent, error) {
	var e domain.ReceiptAccountingEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT event_id, tenant_id, receipt_id, status, journal_id, failure_reason, created_at
			FROM receipt_accounting_events WHERE receipt_id = $1 ORDER BY created_at DESC LIMIT 1`, receiptID,
		).Scan(&e.EventID, &e.TenantID, &e.ReceiptID, &e.Status, &e.JournalID, &nullString{&e.FailureReason}, &e.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg GetLatestAccountingEvent failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &e, nil
}
