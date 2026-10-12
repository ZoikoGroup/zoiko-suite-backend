package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
)

// WithOverTolerancePercent lets received/invoiced quantity exceed the ordered
// quantity by this percentage before a push is refused (0 = never exceed).
// AP-04 has its own over-receipt exception flow; this is the commitment's own
// hard ceiling.
func (s *PgStore) WithOverTolerancePercent(p float64) *PgStore {
	if p >= 0 {
		s.overTolerancePct = p
	}
	return s
}

// RecordProgress applies one receipt/invoice delta to one line.
//
// It is idempotent on (tenant_id, source_ref, kind): the same push twice records
// once and answers Replayed=true, and a source_ref reused for different content
// is refused (ErrProgressRefReused) rather than silently merged. The insert
// uses ON CONFLICT DO NOTHING instead of catching the unique violation, which
// would abort the transaction and turn a harmless replay into a 503.
//
// Rules, all checked inside the transaction that records the push so a refused
// push leaves no trace:
//   - a positive receipt/invoice is accepted only while the order is ISSUED; a
//     reversal (delta -1) is also accepted on a held or closed order, because
//     undoing a receipt must remain possible;
//   - a CANCELLED/DRAFT order never accepts progress;
//   - the resulting quantity cannot go below zero, nor above the ordered
//     quantity plus the configured tolerance.
func (s *PgStore) RecordProgress(ctx context.Context, tenantID, orderID, lineID, actor string, req domain.ProgressRequest, correlationID string) (*domain.ProgressResult, error) {
	var out *domain.ProgressResult
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		order, err := loadOrderDetail(ctx, tx, tenantID, orderID, false)
		if err != nil {
			return err
		}
		switch {
		case order.Status == domain.OrderStatusIssued:
		case req.DeltaSign < 0 && (order.Status == domain.OrderStatusOnHold || order.Status == domain.OrderStatusClosed):
		default:
			return domain.ErrOrderNotIssued
		}

		// Lock the line so concurrent pushes serialize.
		var ordered, received, invoiced float64
		var lineNumber int
		if err := tx.QueryRow(ctx, `
			SELECT quantity, received_quantity, invoiced_quantity, line_number FROM purchase_order_lines
			WHERE line_id = $1 AND purchase_order_id = $2 AND tenant_id = $3 FOR UPDATE`, lineID, orderID, tenantID).
			Scan(&ordered, &received, &invoiced, &lineNumber); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrLineNotFound
			}
			return mapPgError(err)
		}

		var corr any
		if correlationID != "" {
			corr = correlationID
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO purchase_order_progress (progress_id, tenant_id, purchase_order_id, line_id, kind, quantity, amount, source_ref, delta_sign, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (tenant_id, source_ref, kind) DO NOTHING`,
			uuid.NewString(), tenantID, orderID, lineID, req.Kind, req.Quantity, req.Amount, req.SourceRef, req.DeltaSign, corr)
		if err != nil {
			return mapPgError(err)
		}
		if tag.RowsAffected() == 0 {
			// Replay — verify it is the same push.
			var existingLine string
			var q float64
			var sign int
			if err := tx.QueryRow(ctx, `SELECT line_id::text, quantity, delta_sign FROM purchase_order_progress
				WHERE tenant_id = $1 AND source_ref = $2 AND kind = $3`, tenantID, req.SourceRef, req.Kind).Scan(&existingLine, &q, &sign); err != nil {
				return err
			}
			if existingLine != lineID || math.Abs(q-req.Quantity) > 1e-9 || sign != req.DeltaSign {
				return domain.ErrProgressRefReused
			}
			out = &domain.ProgressResult{LineID: lineID, Kind: req.Kind, Replayed: true,
				OrderedQuantity: ordered, ReceivedQuantity: received, InvoicedQuantity: invoiced}
			return nil
		}

		delta := req.Quantity * float64(req.DeltaSign)
		column, current := "received_quantity", received
		if req.Kind == domain.ProgressInvoiced {
			column, current = "invoiced_quantity", invoiced
		}
		updated := current + delta
		if updated < -1e-9 {
			return domain.ErrProgressBelowZero
		}
		if updated > ordered*(1+s.overTolerancePct/100)+1e-9 {
			return domain.ErrProgressExceedsOrder
		}
		if updated < 0 {
			updated = 0
		}
		if _, err := tx.Exec(ctx, `UPDATE purchase_order_lines SET `+column+` = $1, updated_at = NOW() WHERE line_id = $2 AND tenant_id = $3`,
			updated, lineID, tenantID); err != nil {
			return mapPgError(err)
		}
		out = &domain.ProgressResult{LineID: lineID, Kind: req.Kind, OrderedQuantity: ordered, ReceivedQuantity: received, InvoicedQuantity: invoiced}
		if req.Kind == domain.ProgressInvoiced {
			out.InvoicedQuantity = updated
		} else {
			out.ReceivedQuantity = updated
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// OpenQuantity returns the per-line ordered/received/invoiced/open figures
// (cross-service contract #2). ok is false when the order does not exist in the
// caller's tenant.
func (s *PgStore) OpenQuantity(ctx context.Context, orderID string) (lines []domain.LineProgress, ok bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, false, nil
	}
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM purchase_orders WHERE purchase_order_id = $1 AND tenant_id = $2)`, orderID, tenantID).Scan(&exists); err != nil {
			return mapPgError(err)
		}
		if !exists {
			return nil
		}
		ok = true
		rows, err := tx.Query(ctx, `
			SELECT line_id::text, line_number, quantity, received_quantity, invoiced_quantity
			FROM purchase_order_lines WHERE purchase_order_id = $1 AND tenant_id = $2 ORDER BY line_number`, orderID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		lines = []domain.LineProgress{}
		for rows.Next() {
			var l domain.LineProgress
			if err := rows.Scan(&l.LineID, &l.LineNumber, &l.OrderedQuantity, &l.ReceivedQuantity, &l.InvoicedQuantity); err != nil {
				return err
			}
			l.OpenReceiptQuantity = math.Max(0, l.OrderedQuantity-l.ReceivedQuantity)
			l.OpenInvoiceQuantity = math.Max(0, l.OrderedQuantity-l.InvoicedQuantity)
			lines = append(lines, l)
		}
		return rows.Err()
	})
	if errors.Is(err, domain.ErrInvalidIdentifier) {
		return nil, false, nil
	}
	return lines, ok, err
}

// ── idempotency ──────────────────────────────────────────────────────────────

// BeginIdempotent claims (tenant, scope, key). An earlier claim that never
// completed (a crash mid-command) is taken over after a minute so a retry is
// not locked out forever; a completed one is returned for replay.
func (s *PgStore) BeginIdempotent(ctx context.Context, scope, key, requestHash string) (*domain.IdempotencyRecord, bool, error) {
	tenantKey := svcmiddleware.TenantFromContext(ctx)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_key, scope, idem_key, request_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_key, scope, idem_key) DO UPDATE
			SET request_hash = EXCLUDED.request_hash, created_at = NOW()
			WHERE idempotency_keys.status_code IS NULL AND idempotency_keys.created_at < NOW() - INTERVAL '60 seconds'`,
		tenantKey, scope, key, requestHash)
	if err != nil {
		s.log.Error("pg BeginIdempotent failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 1 {
		return nil, true, nil
	}
	var rec domain.IdempotencyRecord
	var status *int
	if err := s.pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response_body FROM idempotency_keys
		WHERE tenant_key = $1 AND scope = $2 AND idem_key = $3`, tenantKey, scope, key,
	).Scan(&rec.RequestHash, &status, &rec.Body); err != nil {
		s.log.Error("pg BeginIdempotent read failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if status != nil {
		rec.Completed, rec.StatusCode = true, *status
	}
	return &rec, false, nil
}

func (s *PgStore) CompleteIdempotent(ctx context.Context, scope, key string, statusCode int, body []byte) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys SET status_code = $4, response_body = $5, completed_at = NOW()
		WHERE tenant_key = $1 AND scope = $2 AND idem_key = $3 AND status_code IS NULL`,
		svcmiddleware.TenantFromContext(ctx), scope, key, statusCode, body)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) ReleaseIdempotent(ctx context.Context, scope, key string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM idempotency_keys WHERE tenant_key = $1 AND scope = $2 AND idem_key = $3 AND status_code IS NULL`,
		svcmiddleware.TenantFromContext(ctx), scope, key)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) PurgeIdempotency(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < NOW() - make_interval(secs => $1)`, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return tag.RowsAffected(), nil
}
