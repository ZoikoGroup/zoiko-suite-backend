package purchaseorder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/goods-service-receipt-svc/internal/domain"
)

// FAKE-SERVER tests: an httptest server serves the response shapes read from
// purchase-order-svc's own handlers and domain types (OrderDetail, LineProgress,
// the errorResponse with a stable code, and RecordProgress's rules). They prove
// how this client interprets AP-03's documented contract; they are not an
// integration test against a running purchase-order-svc.

const (
	tenant = "11111111-1111-1111-1111-111111111111"
	entity = "33333333-3333-3333-3333-333333333333"
	poID   = "44444444-4444-4444-4444-444444444444"
	lineID = "55555555-5555-5555-5555-555555555555"
)

func orderJSON(status string) string {
	return `{"purchase_order_id":"` + poID + `","tenant_id":"` + tenant + `","legal_entity_id":"` + entity + `",
		"po_number":"PO-1","po_status":"` + status + `","total_amount":1500.5,"currency_code":"USD","version":4,"revision":2,
		"lines":[{"line_id":"` + lineID + `","line_number":1,"item_ref":"SKU","description":"x","quantity":15,"unit_price":100,"uom":"EA","line_amount":1500}]}`
}

func serve(t *testing.T, h http.HandlerFunc) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewHTTPClient(srv.URL, zap.NewNop())
}

func TestGetOpenOrder_DecodesAP03sOrderDetail_AndSendsServiceHeaders(t *testing.T) {
	var seen *http.Request
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		_, _ = w.Write([]byte(orderJSON("ISSUED")))
	})
	s, err := c.GetOpenOrder(context.Background(), tenant, entity, poID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "ISSUED" || s.TotalAmount != 1500.5 || s.CurrencyCode != "USD" || s.Revision != 2 || len(s.Lines) != 1 {
		t.Fatalf("unexpected summary %+v", s)
	}
	if l, ok := s.Line(lineID); !ok || l.Quantity != 15 || l.LineID != lineID {
		t.Fatalf("the PO line must be found by id, got %+v %v", l, ok)
	}
	if seen.URL.Path != "/v1/purchase-orders/"+poID || seen.Header.Get("X-Tenant-Id") != tenant ||
		seen.Header.Get("X-Source-Channel") != "system" || seen.Header.Get("X-Workload-Id") != WorkloadID {
		t.Fatalf("expected a tenant-scoped service call, got %s %v", seen.URL.Path, seen.Header)
	}
}

func TestGetOpenOrder_NotIssued_NotFound_Mismatch_AndFailClosed(t *testing.T) {
	for _, st := range []string{"DRAFT", "ON_HOLD", "CANCELLED", "CLOSED"} {
		c := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(orderJSON(st))) })
		_, err := c.GetOpenOrder(context.Background(), tenant, entity, poID)
		var ne *domain.PurchaseOrderNotOpenError
		if !errors.Is(err, domain.ErrPurchaseOrderNotOpen) || !errors.As(err, &ne) || ne.Status != st {
			t.Fatalf("%s: expected a not-open refusal naming the status, got %v", st, err)
		}
	}
	c := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if _, err := c.GetOpenOrder(context.Background(), tenant, entity, poID); !errors.Is(err, domain.ErrPurchaseOrderNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	c = serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(orderJSON("ISSUED"))) })
	if _, err := c.GetOpenOrder(context.Background(), "99999999-9999-9999-9999-999999999999", entity, poID); !errors.Is(err, domain.ErrPurchaseOrderMismatch) {
		t.Fatalf("an order of another tenant must be a mismatch, got %v", err)
	}
	for _, code := range []int{500, 503, 401} {
		c = serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
		if _, err := c.GetOpenOrder(context.Background(), tenant, entity, poID); !errors.Is(err, domain.ErrPurchaseOrderServiceUnavailable) {
			t.Fatalf("%d must fail closed, got %v", code, err)
		}
	}
	c = serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`not json`)) })
	if _, err := c.GetOpenOrder(context.Background(), tenant, entity, poID); !errors.Is(err, domain.ErrPurchaseOrderServiceUnavailable) {
		t.Fatalf("an undecodable answer must fail closed, got %v", err)
	}
}

func TestGetOpenQuantity_DecodesLineProgress(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/purchase-orders/"+poID+"/open-quantity" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"lines":[{"line_id":"` + lineID + `","line_number":1,"ordered_quantity":15,"received_quantity":5,"invoiced_quantity":0,"open_receipt_quantity":10,"open_invoice_quantity":15}]}`))
	})
	lines, err := c.GetOpenQuantity(context.Background(), tenant, entity, poID)
	if err != nil || len(lines) != 1 || lines[0].LineID != lineID || lines[0].OpenReceiptQuantity != 10 || lines[0].OrderedQuantity != 15 {
		t.Fatalf("unexpected open quantity %v %+v", err, lines)
	}
}

func push() ProgressPush {
	return ProgressPush{TenantID: tenant, LegalEntityID: entity, PurchaseOrderID: poID, LineID: lineID, Quantity: 4, Amount: 400,
		SourceRef: "reversal-1", DeltaSign: -1, CorrelationID: "corr-1", PrincipalID: "svc-principal"}
}

// AP-03's RecordProgress refuses a call with no principal and authorizes
// PO_PROGRESS_RECORD, so the push must carry one, along with AP-03's idempotency
// key and the body RecordProgress validates.
func TestPostProgress_CarriesPrincipal_IdempotencyKey_AndAP03sBody(t *testing.T) {
	var seen *http.Request
	var body map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
	})
	if err := c.PostProgress(context.Background(), push()); err != nil {
		t.Fatal(err)
	}
	if seen.Method != http.MethodPost || seen.URL.Path != "/v1/purchase-orders/"+poID+"/lines/"+lineID+"/progress" {
		t.Fatalf("unexpected request %s %s", seen.Method, seen.URL.Path)
	}
	if seen.Header.Get("X-Principal-Id") != "svc-principal" || seen.Header.Get("Idempotency-Key") != "progress:RECEIVED:reversal-1" ||
		seen.Header.Get("X-Tenant-Id") != tenant || seen.Header.Get("X-Correlation-ID") != "corr-1" {
		t.Fatalf("missing identity/idempotency headers: %v", seen.Header)
	}
	if body["kind"] != "RECEIVED" || body["quantity"] != 4.0 || body["amount"] != 400.0 || body["source_ref"] != "reversal-1" || body["delta_sign"] != -1.0 {
		t.Fatalf("the body must match AP-03's ProgressRequest, got %v", body)
	}
}

func TestPostProgress_Classification_DeliveredPermanentTransient(t *testing.T) {
	status := func(code int, body string) error {
		c := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		})
		return c.PostProgress(context.Background(), push())
	}
	if err := status(http.StatusCreated, ``); err != nil {
		t.Fatalf("201 is delivered, got %v", err)
	}
	if err := status(http.StatusOK, ``); err != nil {
		t.Fatalf("200 (AP-03's replay of the same source_ref) is delivered, got %v", err)
	}
	if err := status(http.StatusConflict, `{"error":"progress_exceeds_order","code":"PROGRESS_EXCEEDS_ORDER"}`); !errors.Is(err, domain.ErrProgressExceedsOrder) {
		t.Fatalf("PROGRESS_EXCEEDS_ORDER is permanent, got %v", err)
	}
	// Anything else — other 409s (ORDER_NOT_ISSUED), 401/403 (principal not yet
	// granted), 5xx — is transient: retried with backoff, never dropped.
	for _, c := range []struct {
		code int
		body string
	}{{409, `{"code":"ORDER_NOT_ISSUED"}`}, {401, `{}`}, {403, `{}`}, {500, ``}, {503, ``}} {
		if err := status(c.code, c.body); err == nil || errors.Is(err, domain.ErrProgressExceedsOrder) {
			t.Fatalf("%d must be transient, got %v", c.code, err)
		}
	}
}
