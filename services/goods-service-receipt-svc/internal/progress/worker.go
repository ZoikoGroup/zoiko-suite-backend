// Package progress delivers received-quantity deltas to purchase-order-svc
// (AP-03), which owns received and invoiced quantities.
//
// Confirming or reversing a LINE receipt queues a po_progress_pushes row in the
// same transaction as the state change. This worker drains the queue: delivery
// is idempotent at AP-03 on (kind, source_ref), a transient failure is retried
// with backoff and never lost, and a PROGRESS_EXCEEDS_ORDER refusal marks the
// push FAILED -- visible as progress_push_status on the receipt, never silently
// dropped.
package progress

import (
	"context"
	"time"

	"go.uber.org/zap"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/purchaseorder"
	"zoiko.io/goods-service-receipt-svc/internal/store"
)

// Worker drains the push queue on an interval.
type Worker struct {
	store     *store.PgStore
	po        purchaseorder.Client
	principal string
	interval  time.Duration
	batch     int
	log       *zap.Logger
}

// New builds a worker. principal is the service identity sent to AP-03 as
// X-Principal-Id; it must hold AP-03's PO_PROGRESS_RECORD action.
func New(st *store.PgStore, po purchaseorder.Client, principal string, interval time.Duration, batch int, log *zap.Logger) *Worker {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	if batch <= 0 {
		batch = 50
	}
	return &Worker{store: st, po: po, principal: principal, interval: interval, batch: batch, log: log}
}

// Start delivers until ctx ends.
func (w *Worker) Start(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.RunOnce(ctx)
		}
	}
}

// RunOnce delivers one batch and returns how many were DELIVERED.
func (w *Worker) RunOnce(ctx context.Context) int {
	n, err := w.store.DeliverPendingPushes(ctx, w.batch, w.deliver)
	if err != nil {
		w.log.Error("progress push delivery failed", zap.Error(err))
	}
	return n
}

func (w *Worker) deliver(ctx context.Context, p domain.ProgressPush) error {
	return w.po.PostProgress(ctx, purchaseorder.ProgressPush{
		TenantID: p.TenantID, LegalEntityID: p.LegalEntityID, PurchaseOrderID: p.PurchaseOrderID, LineID: p.POLineID,
		Quantity: p.Quantity, Amount: p.Amount, SourceRef: p.SourceRef, DeltaSign: p.DeltaSign, CorrelationID: p.CorrelationID,
		PrincipalID: w.principal,
	})
}
