package progress

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/purchaseorder"
)

type capturePO struct {
	purchaseorder.Client
	got purchaseorder.ProgressPush
	err error
}

func (c *capturePO) PostProgress(_ context.Context, p purchaseorder.ProgressPush) error {
	c.got = p
	return c.err
}

// The worker maps a queued push onto AP-03's progress contract unchanged: the
// source_ref is AP-03's idempotency key, so it must be carried exactly.
func TestDeliver_MapsThePushOntoAP03sContract_AndPassesErrorsThrough(t *testing.T) {
	po := &capturePO{}
	w := New(nil, po, "svc-principal", time.Second, 1, zap.NewNop())
	push := domain.ProgressPush{
		TenantID: "t", LegalEntityID: "le", ReceiptID: "r", PurchaseOrderID: "po", POLineID: "line",
		Quantity: 4, Amount: 400, DeltaSign: -1, SourceRef: "reversal-1", CorrelationID: "corr",
	}
	if err := w.deliver(context.Background(), push); err != nil {
		t.Fatal(err)
	}
	g := po.got
	if g.TenantID != "t" || g.LegalEntityID != "le" || g.PurchaseOrderID != "po" || g.LineID != "line" || g.Quantity != 4 ||
		g.Amount != 400 || g.DeltaSign != -1 || g.SourceRef != "reversal-1" || g.CorrelationID != "corr" || g.PrincipalID != "svc-principal" {
		t.Fatalf("the push must reach AP-03 unchanged, got %+v", g)
	}

	// The permanent and transient classifications are the client's; the worker
	// must hand them to the store untouched.
	po.err = domain.ErrProgressExceedsOrder
	if err := w.deliver(context.Background(), push); !errors.Is(err, domain.ErrProgressExceedsOrder) {
		t.Fatalf("PROGRESS_EXCEEDS_ORDER must pass through as permanent, got %v", err)
	}
	po.err = domain.ErrPurchaseOrderServiceUnavailable
	if err := w.deliver(context.Background(), push); !errors.Is(err, domain.ErrPurchaseOrderServiceUnavailable) {
		t.Fatalf("a transient error must pass through, got %v", err)
	}
}
