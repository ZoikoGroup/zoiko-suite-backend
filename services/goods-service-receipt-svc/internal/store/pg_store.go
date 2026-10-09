// Package store implements goods-service-receipt-svc's persistence.
//
// Every query names tenant_id explicitly AND runs inside a transaction that sets
// app.tenant_id, so isolation holds both for an ordinary role (row-level
// security binds it) and for a superuser connection (RLS does not).
//
// accounting_posting_requests and po_progress_pushes are infrastructure queues
// drained across tenants by their workers, so -- like outbox_events -- they carry
// no RLS; every tenant-facing query on them filters by tenant_id.
package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/middleware"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input syntax
// for type uuid" error (SQLSTATE 22P02). A malformed id cannot name a row, so
// callers treat it as absent rather than as the database being down.
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// nullString bridges a nullable TEXT column onto a plain (non-pointer) string.
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
	CreateReceipt(ctx context.Context, tenantID string, req domain.CreateReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error)
	FindReceipt(ctx context.Context, receiptID string) (*domain.GoodsServiceReceipt, error)
	ListReceiptsForPO(ctx context.Context, purchaseOrderID string) ([]domain.GoodsServiceReceipt, error)
	AmendReceiptDraft(ctx context.Context, receiptID string, req domain.AmendReceiptDraftRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error)
	ConfirmReceipt(ctx context.Context, receiptID string, in domain.ConfirmInput, cmd domain.Command) (*domain.ConfirmResult, error)
	RejectReceipt(ctx context.Context, receiptID string, req domain.RejectReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error)
	ReverseReceipt(ctx context.Context, receiptID string, req domain.ReverseReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, *domain.ReceiptReversal, error)
	RecordServiceAcceptance(ctx context.Context, receiptID string, req domain.RecordServiceAcceptanceRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error)

	AttachReceiptEvidence(ctx context.Context, receiptID string, req domain.AttachReceiptEvidenceRequest, principalID string) (*domain.ReceiptEvidence, error)
	ListReceiptEvidence(ctx context.Context, receiptID string) ([]domain.ReceiptEvidence, error)

	SumNetConfirmedAmountForPO(ctx context.Context, purchaseOrderID string) (float64, error)
	ReceivedToDate(ctx context.Context, purchaseOrderID string) ([]domain.LineReceived, error)
	PendingLineQuantity(ctx context.Context, poLineID string) (float64, error)

	GetLatestAccountingEvent(ctx context.Context, receiptID string) (*domain.ReceiptAccountingEvent, error)
	ListAccountingRequests(ctx context.Context, receiptID string) ([]domain.ReceiptAccountingEvent, error)
	RequeueAccounting(ctx context.Context, receiptID string) (int64, error)
}

type PgStore struct {
	pool       *pgxpool.Pool
	log        *zap.Logger
	accounting AccountingConfig
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log, accounting: DefaultAccountingConfig()}
}

// Pool exposes the pool to the background workers.
func (s *PgStore) Pool() *pgxpool.Pool { return s.pool }

// withTenant runs fn in a transaction scoped to the caller's verified tenant. A
// request with no tenant scope has no honest answer and is refused.
func (s *PgStore) withTenant(ctx context.Context, fn func(tx pgx.Tx, tenantID string) error) error {
	tenantID := middleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrTenantScopeMissing
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx, tenantID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// storeErr maps a store failure the handler distinguishes onto its domain error;
// anything else is an outage.
func (s *PgStore) storeErr(op string, err error) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows), isInvalidUUID(err):
		return domain.ErrReceiptNotFound
	case errors.Is(err, domain.ErrTenantScopeMissing), errors.Is(err, domain.ErrReceiptNotFound),
		errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrStaleVersion),
		errors.Is(err, domain.ErrOverReversal), errors.Is(err, domain.ErrOverReceiptTolerance),
		errors.Is(err, domain.ErrPurchaseOrderLineInvalid):
		return err
	}
	s.log.Error("pg "+op+" failed", zap.Error(err))
	return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
}

// ── receipts ─────────────────────────────────────────────────────────────────

const receiptColumns = `
	receipt_id, tenant_id, legal_entity_id, purchase_order_id, po_line_id, po_revision, receipt_type, quantity,
	unit_of_measure, amount, currency_code, receipt_date, location, inspection_result,
	requires_independent_acceptance, tolerance_exception_ref, status, rejection_reason, reversed_amount,
	reversed_quantity, receiver_principal_id, created_by_principal_id, confirmed_by_principal_id, confirmed_at,
	version, created_at, updated_at`

func scanReceipt(row pgx.Row) (*domain.GoodsServiceReceipt, error) {
	r := &domain.GoodsServiceReceipt{}
	err := row.Scan(&r.ReceiptID, &r.TenantID, &r.LegalEntityID, &r.PurchaseOrderID, &r.POLineID, &r.PORevision, &r.ReceiptType,
		&r.Quantity, &r.UnitOfMeasure, &r.Amount, &r.CurrencyCode, &r.ReceiptDate, &nullString{&r.Location},
		&nullString{&r.InspectionResult}, &r.RequiresIndependentAcceptance, &nullString{&r.ToleranceExceptionRef},
		&r.Status, &nullString{&r.RejectionReason}, &r.ReversedAmount, &r.ReversedQuantity, &r.ReceiverPrincipalID,
		&r.CreatedByPrincipalID, &r.ConfirmedByPrincipalID, &r.ConfirmedAt, &r.Version, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.ProgressPushStatus = domain.PushNotApplicable
	return r, nil
}

// pushStatuses reports the delivery state of each receipt's progress pushes.
func pushStatuses(ctx context.Context, tx pgx.Tx, tenantID string, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT receipt_id::text,
		       CASE WHEN bool_or(status = 'FAILED') THEN 'FAILED'
		            WHEN bool_or(status = 'PENDING') THEN 'PENDING'
		            ELSE 'DELIVERED' END
		FROM po_progress_pushes WHERE tenant_id = $1 AND receipt_id::text = ANY($2) GROUP BY receipt_id`, tenantID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

func fillPushStatus(ctx context.Context, tx pgx.Tx, tenantID string, rs ...*domain.GoodsServiceReceipt) error {
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, r.ReceiptID)
	}
	m, err := pushStatuses(ctx, tx, tenantID, ids)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if st, ok := m[r.ReceiptID]; ok {
			r.ProgressPushStatus = st
		}
	}
	return nil
}

// lockReceipt reads one receipt FOR UPDATE inside the caller's transaction.
func lockReceipt(ctx context.Context, tx pgx.Tx, tenantID, receiptID string) (*domain.GoodsServiceReceipt, error) {
	return scanReceipt(tx.QueryRow(ctx,
		`SELECT `+receiptColumns+` FROM goods_service_receipts WHERE receipt_id = $1 AND tenant_id = $2 FOR UPDATE`, receiptID, tenantID))
}

func checkVersion(cur *domain.GoodsServiceReceipt, cmd domain.Command) error {
	if cmd.ExpectedVersion != nil && cur.Version != *cmd.ExpectedVersion {
		return domain.ErrStaleVersion
	}
	return nil
}

func (s *PgStore) CreateReceipt(ctx context.Context, tenantID string, req domain.CreateReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		var line any
		if req.POLineID != "" {
			line = req.POLineID
		}
		var err error
		r, err = scanReceipt(tx.QueryRow(ctx, `
			INSERT INTO goods_service_receipts (
				receipt_id, tenant_id, legal_entity_id, purchase_order_id, po_line_id, receipt_type, quantity, unit_of_measure,
				amount, currency_code, receipt_date, location, inspection_result, requires_independent_acceptance,
				tolerance_exception_ref, status, receiver_principal_id, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, 'DRAFT', $16, $16)
			RETURNING `+receiptColumns,
			uuid.NewString(), tenant, req.LegalEntityID, req.PurchaseOrderID, line, req.ReceiptType, req.Quantity,
			req.UnitOfMeasure, req.Amount, req.CurrencyCode, req.ReceiptDate, req.Location, req.InspectionResult,
			req.RequiresIndependentAcceptance, req.ToleranceExceptionRef, cmd.PrincipalID))
		if err != nil {
			return err
		}
		return emit(ctx, tx, r, cmd, nil, domain.EventReceiptCreated, domain.AliasReceiptCreated)
	})
	if err != nil {
		return nil, s.storeErr("CreateReceipt", err)
	}
	return r, nil
}

func (s *PgStore) FindReceipt(ctx context.Context, receiptID string) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		var err error
		r, err = scanReceipt(tx.QueryRow(ctx,
			`SELECT `+receiptColumns+` FROM goods_service_receipts WHERE receipt_id = $1 AND tenant_id = $2`, receiptID, tenant))
		if err != nil {
			return err
		}
		return fillPushStatus(ctx, tx, tenant, r)
	})
	if err != nil {
		return nil, s.storeErr("FindReceipt", err)
	}
	return r, nil
}

func (s *PgStore) ListReceiptsForPO(ctx context.Context, purchaseOrderID string) ([]domain.GoodsServiceReceipt, error) {
	var out []domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		rows, err := tx.Query(ctx, `SELECT `+receiptColumns+` FROM goods_service_receipts
			WHERE purchase_order_id = $1 AND tenant_id = $2 ORDER BY created_at ASC`, purchaseOrderID, tenant)
		if err != nil {
			return err
		}
		var ptrs []*domain.GoodsServiceReceipt
		for rows.Next() {
			r, err := scanReceipt(rows)
			if err != nil {
				rows.Close()
				return err
			}
			ptrs = append(ptrs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := fillPushStatus(ctx, tx, tenant, ptrs...); err != nil {
			return err
		}
		for _, r := range ptrs {
			out = append(out, *r)
		}
		return nil
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.storeErr("ListReceiptsForPO", err)
	}
	return out, nil
}

// AmendReceiptDraft changes a DRAFT receipt's content. Anything past DRAFT is
// immutable evidence: a confirmed receipt is corrected by a linked reversal,
// never overwritten.
func (s *PgStore) AmendReceiptDraft(ctx context.Context, receiptID string, req domain.AmendReceiptDraftRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		cur, err := lockReceipt(ctx, tx, tenant, receiptID)
		if err != nil {
			return err
		}
		if err := checkVersion(cur, cmd); err != nil {
			return err
		}
		if !domain.CanAmendDraft(cur.Status) {
			return domain.ErrInvalidTransition
		}
		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET
				quantity = COALESCE($3, quantity), unit_of_measure = COALESCE($4, unit_of_measure),
				amount = COALESCE($5, amount), location = COALESCE($6, location),
				inspection_result = COALESCE($7, inspection_result),
				version = version + 1, updated_at = NOW()
			WHERE receipt_id = $1 AND tenant_id = $2
			RETURNING `+receiptColumns,
			receiptID, tenant, req.Quantity, req.UnitOfMeasure, req.Amount, req.Location, req.InspectionResult))
		return err
	})
	if err != nil {
		return nil, s.storeErr("AmendReceiptDraft", err)
	}
	return r, nil
}

// lockKey serializes concurrent confirmations that compete for the same
// capacity: the PO line for a line receipt, the PO for a header-level one.
func lockKey(r *domain.GoodsServiceReceipt, lineID *string) string {
	if lineID != nil {
		return "line:" + *lineID
	}
	return "po:" + r.PurchaseOrderID
}

// ConfirmReceipt moves a DRAFT/PENDING_CONFIRMATION receipt to CONFIRMED and, in
// the SAME transaction: re-checks the over-receipt tolerance under a lock that
// serializes competing confirmations, queues the received-quantity push to
// AP-03 (line receipts), queues the GRNI posting request, and writes the domain
// events to the outbox. Any failure rolls all of it back; a replay is refused by
// the status guard and, belt and braces, by the unique posting-request and push
// keys, so the consequence can never be queued twice.
func (s *PgStore) ConfirmReceipt(ctx context.Context, receiptID string, in domain.ConfirmInput, cmd domain.Command) (*domain.ConfirmResult, error) {
	var res domain.ConfirmResult
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		cur, err := lockReceipt(ctx, tx, tenant, receiptID)
		if err != nil {
			return err
		}
		if err := checkVersion(cur, cmd); err != nil {
			return err
		}
		if !domain.CanConfirm(cur.Status) {
			return domain.ErrInvalidTransition
		}

		lineID := cur.POLineID
		if in.POLineID != nil && *in.POLineID != "" {
			if lineID != nil && !strings.EqualFold(*lineID, *in.POLineID) {
				return domain.ErrPurchaseOrderLineInvalid // a line link can never be re-pointed
			}
			lineID = in.POLineID
		}

		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, tenant+"|"+lockKey(cur, lineID)); err != nil {
			return err
		}

		exception := in.ToleranceExceptionRef
		if exception == "" {
			exception = cur.ToleranceExceptionRef
		}
		if exception == "" {
			if err := s.checkTolerance(ctx, tx, tenant, cur, lineID, in.Limits); err != nil {
				return err
			}
		}

		var line any
		if lineID != nil {
			line = *lineID
		}
		r, err := scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET
				status = 'CONFIRMED', confirmed_by_principal_id = $3, confirmed_at = NOW(),
				po_line_id = COALESCE(po_line_id, $4::uuid), po_revision = COALESCE($5, po_revision),
				tolerance_exception_ref = $6, version = version + 1, updated_at = NOW()
			WHERE receipt_id = $1 AND tenant_id = $2
			RETURNING `+receiptColumns,
			receiptID, tenant, cmd.PrincipalID, line, in.PORevision, exception))
		if err != nil {
			return err
		}
		if err := queueProgressPush(ctx, tx, r, cmd, r.ReceiptID, r.Quantity, r.Amount, 1); err != nil {
			return err
		}
		acct, err := s.requestAccounting(ctx, tx, r, cmd, domain.DirectionAccrue, r.ReceiptID, r.Amount, r.ReceiptDate)
		if err != nil {
			return err
		}
		if err := emit(ctx, tx, r, cmd, nil, domain.EventReceiptConfirmed, domain.AliasReceiptConfirmed); err != nil {
			return err
		}
		if err := fillPushStatus(ctx, tx, tenant, r); err != nil {
			return err
		}
		res = domain.ConfirmResult{Receipt: r, Accounting: acct}
		return nil
	})
	if err != nil {
		return nil, s.storeErr("ConfirmReceipt", err)
	}
	return &res, nil
}

// checkTolerance is the authoritative over-receipt check, run under the
// confirmation lock. A line receipt must fit AP-03's open quantity, less this
// service's own queued-but-undelivered pushes (which AP-03 cannot yet see), plus
// the configured tolerance of the ordered quantity. A header-level receipt must
// fit the PO's total, net of confirmed receipts, plus the tolerance.
func (s *PgStore) checkTolerance(ctx context.Context, tx pgx.Tx, tenant string, cur *domain.GoodsServiceReceipt, lineID *string, lim domain.ConfirmLimits) error {
	const eps = 0.0001
	tol := lim.TolerancePct / 100
	if lineID != nil {
		var pending float64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(quantity * delta_sign), 0) FROM po_progress_pushes
			WHERE tenant_id = $1 AND po_line_id = $2::uuid AND status = 'PENDING'`, tenant, *lineID).Scan(&pending); err != nil {
			return err
		}
		ceiling := (lim.LineOpenReceiptQuantity - pending) + lim.LineOrderedQuantity*tol
		if cur.Quantity > ceiling+eps {
			return domain.ErrOverReceiptTolerance
		}
		return nil
	}
	var net float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount - reversed_amount), 0) FROM goods_service_receipts
		WHERE tenant_id = $1 AND purchase_order_id = $2 AND status IN ('CONFIRMED', 'PARTIALLY_REVERSED', 'FULLY_REVERSED')`,
		tenant, cur.PurchaseOrderID).Scan(&net); err != nil {
		return err
	}
	if net+cur.Amount > lim.POTotalAmount*(1+tol)+eps {
		return domain.ErrOverReceiptTolerance
	}
	return nil
}

func (s *PgStore) RejectReceipt(ctx context.Context, receiptID string, req domain.RejectReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		cur, err := lockReceipt(ctx, tx, tenant, receiptID)
		if err != nil {
			return err
		}
		if err := checkVersion(cur, cmd); err != nil {
			return err
		}
		if !domain.CanReject(cur.Status) {
			return domain.ErrInvalidTransition
		}
		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET status = 'REJECTED', rejection_reason = $3, version = version + 1, updated_at = NOW()
			WHERE receipt_id = $1 AND tenant_id = $2
			RETURNING `+receiptColumns, receiptID, tenant, req.Reason))
		return err
	})
	if err != nil {
		return nil, s.storeErr("RejectReceipt", err)
	}
	return r, nil
}

// ReverseReceipt records an append-only ReceiptReversal and recomputes the
// receipt's cumulative reversed amount/quantity and status atomically. Partial
// reversals accumulate up to -- never past -- the original amount and quantity.
// A line reversal queues a negative progress push; every reversal queues the
// mirrored GRNI posting.
func (s *PgStore) ReverseReceipt(ctx context.Context, receiptID string, req domain.ReverseReceiptRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, *domain.ReceiptReversal, error) {
	var r *domain.GoodsServiceReceipt
	var rev domain.ReceiptReversal
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		cur, err := lockReceipt(ctx, tx, tenant, receiptID)
		if err != nil {
			return err
		}
		if err := checkVersion(cur, cmd); err != nil {
			return err
		}
		if !domain.CanReverse(cur.Status) {
			return domain.ErrInvalidTransition
		}
		const eps = 0.0001
		remainingAmt := cur.Amount - cur.ReversedAmount
		remainingQty := cur.Quantity - cur.ReversedQuantity
		if req.ReversedAmount > remainingAmt+eps {
			return domain.ErrOverReversal
		}
		full := req.ReversedAmount >= remainingAmt-eps

		var qty float64
		switch {
		case req.ReversedQuantity != nil:
			qty = *req.ReversedQuantity
		case full:
			qty = remainingQty // never leave a rounding residue on the last reversal
		default:
			qty = math.Round(cur.Quantity*req.ReversedAmount/cur.Amount*10000) / 10000
		}
		if qty < 0 || qty > remainingQty+eps {
			return domain.ErrOverReversal
		}
		if qty > remainingQty {
			qty = remainingQty
		}

		newAmt := cur.ReversedAmount + req.ReversedAmount
		newQty := cur.ReversedQuantity + qty
		status := domain.StatusPartiallyReversed
		if newAmt >= cur.Amount-eps {
			status = domain.StatusFullyReversed
		}

		rev = domain.ReceiptReversal{
			ReversalID: uuid.NewString(), TenantID: tenant, ReceiptID: receiptID, ReversedAmount: req.ReversedAmount,
			ReversedQuantity: qty, Reason: req.Reason, ReversedByPrincipalID: cmd.PrincipalID,
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO receipt_reversals (reversal_id, tenant_id, receipt_id, reversed_amount, reversed_quantity, reason, reversed_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
			rev.ReversalID, tenant, receiptID, rev.ReversedAmount, rev.ReversedQuantity, rev.Reason, rev.ReversedByPrincipalID,
		).Scan(&rev.CreatedAt); err != nil {
			return err
		}

		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET status = $3, reversed_amount = $4, reversed_quantity = $5,
				version = version + 1, updated_at = NOW()
			WHERE receipt_id = $1 AND tenant_id = $2
			RETURNING `+receiptColumns, receiptID, tenant, string(status), newAmt, newQty))
		if err != nil {
			return err
		}
		if err := queueProgressPush(ctx, tx, r, cmd, rev.ReversalID, qty, req.ReversedAmount, -1); err != nil {
			return err
		}
		if _, err := s.requestAccounting(ctx, tx, r, cmd, domain.DirectionReverse,
			receiptID+":reversal:"+rev.ReversalID, req.ReversedAmount, time.Now()); err != nil {
			return err
		}
		if err := emit(ctx, tx, r, cmd, &rev, domain.EventReceiptReversed, domain.AliasReceiptReversed); err != nil {
			return err
		}
		return fillPushStatus(ctx, tx, tenant, r)
	})
	if err != nil {
		return nil, nil, s.storeErr("ReverseReceipt", err)
	}
	return r, &rev, nil
}

// RecordServiceAcceptance records acceptance evidence and moves a DRAFT service
// receipt to PENDING_CONFIRMATION, ready for ConfirmReceipt. The handler runs the
// authorization-svc own-object SoD check BEFORE calling this when the receipt
// requires independent acceptance.
func (s *PgStore) RecordServiceAcceptance(ctx context.Context, receiptID string, req domain.RecordServiceAcceptanceRequest, cmd domain.Command) (*domain.GoodsServiceReceipt, error) {
	var r *domain.GoodsServiceReceipt
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		cur, err := lockReceipt(ctx, tx, tenant, receiptID)
		if err != nil {
			return err
		}
		if err := checkVersion(cur, cmd); err != nil {
			return err
		}
		if cur.Status != domain.StatusDraft {
			return domain.ErrInvalidTransition
		}
		r, err = scanReceipt(tx.QueryRow(ctx, `
			UPDATE goods_service_receipts SET status = 'PENDING_CONFIRMATION', version = version + 1, updated_at = NOW()
			WHERE receipt_id = $1 AND tenant_id = $2
			RETURNING `+receiptColumns, receiptID, tenant))
		if err != nil {
			return err
		}
		if req.EvidenceRef != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO receipt_evidence (evidence_id, tenant_id, receipt_id, evidence_ref, description, recorded_by_principal_id)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				uuid.NewString(), tenant, receiptID, req.EvidenceRef, req.Notes, cmd.PrincipalID); err != nil {
				return err
			}
		}
		return emit(ctx, tx, r, cmd, nil, domain.EventServiceAcceptanceRecorded)
	})
	if err != nil {
		return nil, s.storeErr("RecordServiceAcceptance", err)
	}
	return r, nil
}

// ── evidence ─────────────────────────────────────────────────────────────────

func (s *PgStore) AttachReceiptEvidence(ctx context.Context, receiptID string, req domain.AttachReceiptEvidenceRequest, principalID string) (*domain.ReceiptEvidence, error) {
	var e domain.ReceiptEvidence
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		var found string
		if err := tx.QueryRow(ctx, `SELECT receipt_id::text FROM goods_service_receipts WHERE receipt_id = $1 AND tenant_id = $2`,
			receiptID, tenant).Scan(&found); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO receipt_evidence (evidence_id, tenant_id, receipt_id, evidence_ref, description, recorded_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING evidence_id, tenant_id, receipt_id, evidence_ref, description, recorded_by_principal_id, created_at`,
			uuid.NewString(), tenant, receiptID, req.EvidenceRef, req.Description, principalID,
		).Scan(&e.EvidenceID, &e.TenantID, &e.ReceiptID, &e.EvidenceRef, &nullString{&e.Description}, &e.RecordedByPrincipalID, &e.CreatedAt)
	})
	if err != nil {
		return nil, s.storeErr("AttachReceiptEvidence", err)
	}
	return &e, nil
}

func (s *PgStore) ListReceiptEvidence(ctx context.Context, receiptID string) ([]domain.ReceiptEvidence, error) {
	var out []domain.ReceiptEvidence
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		rows, err := tx.Query(ctx, `
			SELECT evidence_id, tenant_id, receipt_id, evidence_ref, description, recorded_by_principal_id, created_at
			FROM receipt_evidence WHERE receipt_id = $1 AND tenant_id = $2 ORDER BY created_at ASC`, receiptID, tenant)
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
		return nil, s.storeErr("ListReceiptEvidence", err)
	}
	return out, nil
}

// ── received-to-date ─────────────────────────────────────────────────────────

// SumNetConfirmedAmountForPO sums (amount - reversed_amount) across every
// receipt against purchaseOrderID that has ever been confirmed.
func (s *PgStore) SumNetConfirmedAmountForPO(ctx context.Context, purchaseOrderID string) (float64, error) {
	var total float64
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(amount - reversed_amount), 0) FROM goods_service_receipts
			WHERE tenant_id = $1 AND purchase_order_id = $2 AND status IN ('CONFIRMED', 'PARTIALLY_REVERSED', 'FULLY_REVERSED')`,
			tenant, purchaseOrderID).Scan(&total)
	})
	if isInvalidUUID(err) {
		return 0, nil
	}
	if err != nil {
		return 0, s.storeErr("SumNetConfirmedAmountForPO", err)
	}
	return total, nil
}

// ReceivedToDate is the net confirmed receipt per PO line (received less
// reversed), the basis AP-06 matching reads.
func (s *PgStore) ReceivedToDate(ctx context.Context, purchaseOrderID string) ([]domain.LineReceived, error) {
	var out []domain.LineReceived
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		rows, err := tx.Query(ctx, `
			SELECT po_line_id::text, SUM(quantity - reversed_quantity), SUM(amount - reversed_amount)
			FROM goods_service_receipts
			WHERE tenant_id = $1 AND purchase_order_id = $2 AND po_line_id IS NOT NULL
			  AND status IN ('CONFIRMED', 'PARTIALLY_REVERSED', 'FULLY_REVERSED')
			GROUP BY po_line_id ORDER BY po_line_id`, tenant, purchaseOrderID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l domain.LineReceived
			if err := rows.Scan(&l.POLineID, &l.ReceivedQuantity, &l.ReceivedAmount); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.storeErr("ReceivedToDate", err)
	}
	return out, nil
}

// PendingLineQuantity is the net quantity this service has queued for a PO line
// that AP-03 has not yet been told about.
func (s *PgStore) PendingLineQuantity(ctx context.Context, poLineID string) (float64, error) {
	var q float64
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(quantity * delta_sign), 0) FROM po_progress_pushes
			WHERE tenant_id = $1 AND po_line_id = $2::uuid AND status = 'PENDING'`, tenant, poLineID).Scan(&q)
	})
	if isInvalidUUID(err) {
		return 0, nil
	}
	if err != nil {
		return 0, s.storeErr("PendingLineQuantity", err)
	}
	return q, nil
}

// ── accounting status ────────────────────────────────────────────────────────

const postingColumns = `request_id::text, tenant_id::text, receipt_id::text, source_event_id, direction, status,
	posting_execution_id, journal_id, posting_policy_version, attempts, last_error, created_at`

func scanPosting(row pgx.Row) (*domain.ReceiptAccountingEvent, error) {
	var e domain.ReceiptAccountingEvent
	var status string
	err := row.Scan(&e.EventID, &e.TenantID, &e.ReceiptID, &e.SourceEventID, &e.Direction, &status,
		&e.PostingExecutionID, &e.JournalID, &e.PostingPolicyVersion, &e.Attempts, &nullString{&e.FailureReason}, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	e.Status = domain.AccountingEventStatus(status)
	return &e, nil
}

// ListAccountingRequests returns every GRNI posting request of a receipt, newest
// first.
func (s *PgStore) ListAccountingRequests(ctx context.Context, receiptID string) ([]domain.ReceiptAccountingEvent, error) {
	var out []domain.ReceiptAccountingEvent
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		rows, err := tx.Query(ctx, `SELECT `+postingColumns+` FROM accounting_posting_requests
			WHERE tenant_id = $1 AND receipt_id = $2 ORDER BY created_at DESC, request_id`, tenant, receiptID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanPosting(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.storeErr("ListAccountingRequests", err)
	}
	return out, nil
}

// GetLatestAccountingEvent is GetReceiptAccountingStatus's source: the newest
// posting request, falling back to the legacy direct-journal rows (history from
// the retired path) for a receipt that predates the queue. nil means none.
func (s *PgStore) GetLatestAccountingEvent(ctx context.Context, receiptID string) (*domain.ReceiptAccountingEvent, error) {
	list, err := s.ListAccountingRequests(ctx, receiptID)
	if err != nil {
		return nil, err
	}
	if len(list) > 0 {
		return &list[0], nil
	}
	var e domain.ReceiptAccountingEvent
	err = s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		return tx.QueryRow(ctx, `
			SELECT event_id::text, tenant_id::text, receipt_id::text, status, journal_id, failure_reason, created_at
			FROM receipt_accounting_events WHERE tenant_id = $1 AND receipt_id = $2 ORDER BY created_at DESC LIMIT 1`,
			tenant, receiptID).Scan(&e.EventID, &e.TenantID, &e.ReceiptID, &e.Status, &e.JournalID, &nullString{&e.FailureReason}, &e.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		return nil, s.storeErr("GetLatestAccountingEvent", err)
	}
	return &e, nil
}

// RequeueAccounting puts a receipt's FAILED/QUARANTINED posting requests back to
// PENDING after an operator fixed the cause (for example a missing ACC-02
// mapping). A POSTED request is never touched.
func (s *PgStore) RequeueAccounting(ctx context.Context, receiptID string) (int64, error) {
	var n int64
	err := s.withTenant(ctx, func(tx pgx.Tx, tenant string) error {
		tag, err := tx.Exec(ctx, `
			UPDATE accounting_posting_requests SET status = 'PENDING', attempts = 0, next_attempt_at = NOW(), updated_at = NOW()
			WHERE tenant_id = $1 AND receipt_id = $2 AND status IN ('FAILED', 'QUARANTINED')`, tenant, receiptID)
		n = tag.RowsAffected()
		return err
	})
	if isInvalidUUID(err) {
		return 0, domain.ErrReceiptNotFound
	}
	if err != nil {
		return 0, s.storeErr("RequeueAccounting", err)
	}
	return n, nil
}
