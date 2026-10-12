package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
)

const (
	// MaxPostingAttempts is how many transient failures a GRNI posting request
	// survives before it shows as FAILED (and waits for an operator requeue).
	MaxPostingAttempts = 10
	// MaxPushAttempts is how many transient failures a progress push survives
	// before it shows as FAILED on the receipt.
	MaxPushAttempts = 20
)

// backoff is the delay before attempt n+1: 5s doubling, capped at 5 minutes.
func backoff(attempts int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// ── GRNI posting dispatch ────────────────────────────────────────────────────

// PostingResult is what the ledger answered for a request.
type PostingResult struct {
	ExecutionID string
	JournalID   string
	// Final means the answer is definitive: Posted=true is POSTED; Posted=false
	// with Final=true is QUARANTINED (the ledger refused it in a way a retry will
	// not fix). Final=false is a transient failure.
	Final  bool
	Posted bool
	Err    string
}

// PostingRequest is one row handed to the dispatcher.
type PostingRequest struct {
	RequestID     string
	TenantID      string
	LegalEntityID string
	SourceEventID string
	Payload       json.RawMessage
	Attempts      int
}

// DispatchPending claims up to limit due PENDING requests (FOR UPDATE SKIP
// LOCKED, so replicas never post the same one) and submits each through post. A
// POSTED/QUARANTINED answer is final; a transient failure counts an attempt,
// backs off, and shows as FAILED after MaxPostingAttempts. Returns how many were
// POSTED. The queue carries no RLS, so no tenant scope is needed to read it.
func (s *PgStore) DispatchPending(ctx context.Context, limit int, post func(context.Context, PostingRequest) PostingResult) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT request_id::text, tenant_id::text, legal_entity_id::text, source_event_id, request_payload, attempts
		FROM accounting_posting_requests
		WHERE status = 'PENDING' AND next_attempt_at <= NOW()
		ORDER BY created_at ASC LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	var batch []PostingRequest
	for rows.Next() {
		var r PostingRequest
		if err := rows.Scan(&r.RequestID, &r.TenantID, &r.LegalEntityID, &r.SourceEventID, &r.Payload, &r.Attempts); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	posted := 0
	for _, r := range batch {
		res := post(ctx, r)
		switch {
		case res.Final && res.Posted:
			_, err = tx.Exec(ctx, `
				UPDATE accounting_posting_requests SET status = 'POSTED', posting_execution_id = $2, journal_id = $3,
					attempts = attempts + 1, last_error = NULL, posted_at = NOW(), updated_at = NOW()
				WHERE request_id = $1::uuid`, r.RequestID, nullIfEmpty(res.ExecutionID), nullIfEmpty(res.JournalID))
			posted++
		case res.Final:
			_, err = tx.Exec(ctx, `
				UPDATE accounting_posting_requests SET status = 'QUARANTINED', attempts = attempts + 1, last_error = $2, updated_at = NOW()
				WHERE request_id = $1::uuid`, r.RequestID, res.Err)
		default:
			status := "PENDING"
			if r.Attempts+1 >= MaxPostingAttempts {
				status = "FAILED"
			}
			_, err = tx.Exec(ctx, `
				UPDATE accounting_posting_requests SET status = $2, attempts = attempts + 1, last_error = $3,
					next_attempt_at = $4, updated_at = NOW()
				WHERE request_id = $1::uuid`, r.RequestID, status, res.Err, time.Now().Add(backoff(r.Attempts+1)))
		}
		if err != nil {
			return posted, err
		}
	}
	return posted, tx.Commit(ctx)
}

// ── AP-03 progress push delivery ─────────────────────────────────────────────

// DeliverPendingPushes claims up to limit due PENDING pushes and hands each to
// deliver. A nil error marks it DELIVERED; domain.ErrProgressExceedsOrder is
// permanent (FAILED, visible as progress_push_status on the receipt); any other
// error is transient: the attempt is counted, the push backs off and stays
// PENDING until MaxPushAttempts, after which it shows as FAILED.
//
// A reversal (negative) push is held back until the receipt's own original push
// has been DELIVERED, so AP-03 never sees received quantity go below zero.
func (s *PgStore) DeliverPendingPushes(ctx context.Context, limit int, deliver func(context.Context, domain.ProgressPush) error) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT p.push_id::text, p.tenant_id::text, p.legal_entity_id::text, p.receipt_id::text, p.purchase_order_id::text,
		       p.po_line_id::text, p.quantity, p.amount, p.delta_sign, p.source_ref, COALESCE(p.correlation_id, ''), p.attempts
		FROM po_progress_pushes p
		WHERE p.status = 'PENDING' AND p.next_attempt_at <= NOW()
		  AND (p.delta_sign > 0 OR NOT EXISTS (
		        SELECT 1 FROM po_progress_pushes o
		        WHERE o.tenant_id = p.tenant_id AND o.receipt_id = p.receipt_id AND o.delta_sign > 0 AND o.status <> 'DELIVERED'))
		ORDER BY p.created_at ASC LIMIT $1 FOR UPDATE OF p SKIP LOCKED`, limit)
	if err != nil {
		return 0, err
	}
	var batch []domain.ProgressPush
	for rows.Next() {
		var p domain.ProgressPush
		if err := rows.Scan(&p.PushID, &p.TenantID, &p.LegalEntityID, &p.ReceiptID, &p.PurchaseOrderID, &p.POLineID,
			&p.Quantity, &p.Amount, &p.DeltaSign, &p.SourceRef, &p.CorrelationID, &p.Attempts); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	delivered := 0
	for _, p := range batch {
		derr := deliver(ctx, p)
		switch {
		case derr == nil:
			_, err = tx.Exec(ctx, `UPDATE po_progress_pushes SET status = 'DELIVERED', attempts = attempts + 1,
				last_error = NULL, delivered_at = NOW() WHERE push_id = $1::uuid`, p.PushID)
			delivered++
		case errors.Is(derr, domain.ErrProgressExceedsOrder):
			_, err = tx.Exec(ctx, `UPDATE po_progress_pushes SET status = 'FAILED', attempts = attempts + 1, last_error = $2
				WHERE push_id = $1::uuid`, p.PushID, derr.Error())
		default:
			status := "PENDING"
			if p.Attempts+1 >= MaxPushAttempts {
				status = "FAILED"
			}
			_, err = tx.Exec(ctx, `UPDATE po_progress_pushes SET status = $2, attempts = attempts + 1, last_error = $3,
				next_attempt_at = $4 WHERE push_id = $1::uuid`, p.PushID, status, derr.Error(), time.Now().Add(backoff(p.Attempts+1)))
		}
		if err != nil {
			return delivered, err
		}
	}
	return delivered, tx.Commit(ctx)
}
