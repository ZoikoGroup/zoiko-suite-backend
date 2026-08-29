package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/goods-service-receipt-svc/internal/authz"
	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/events"
	"zoiko.io/goods-service-receipt-svc/internal/handler"
	"zoiko.io/goods-service-receipt-svc/internal/ledger"
	"zoiko.io/goods-service-receipt-svc/internal/middleware"
	"zoiko.io/goods-service-receipt-svc/internal/purchaseorder"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub authz — including the own-object SoD layer ─────────────────────────

// stubAuthz is a real, working stand-in for authorization-svc's own
// behavior: CheckAllowedOwnObject denies whenever the deciding principal
// equals the resource owner, exactly like authorization-svc's real dynamic
// SoD rule would for a receiver attempting to self-certify their own
// receipt.
type stubAuthz struct {
	deny     bool
	sodRules bool
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func (a *stubAuthz) CheckAllowedOwnObject(_ context.Context, principalID, _, _, resourceOwnerPrincipalID string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	if a.sodRules && principalID == resourceOwnerPrincipalID {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── stub purchase-order-svc client ──────────────────────────────────────────

type stubPO struct {
	orders map[string]purchaseorder.Summary
}

func newStubPO() *stubPO { return &stubPO{orders: map[string]purchaseorder.Summary{}} }

func (p *stubPO) addOrder(id, tenantID, legalEntityID, status string, totalAmount float64) {
	p.orders[id] = purchaseorder.Summary{
		PurchaseOrderID: id, TenantID: tenantID, LegalEntityID: legalEntityID, Status: status,
		TotalAmount: totalAmount, CurrencyCode: "USD",
	}
}

func (p *stubPO) GetOrder(_ context.Context, tenantID, legalEntityID, purchaseOrderID string) (*purchaseorder.Summary, error) {
	s, ok := p.orders[purchaseOrderID]
	if !ok {
		return nil, domain.ErrPurchaseOrderNotFound
	}
	if s.TenantID != tenantID || (legalEntityID != "" && s.LegalEntityID != legalEntityID) {
		return nil, domain.ErrPurchaseOrderMismatch
	}
	return &s, nil
}

func (p *stubPO) GetOpenOrder(ctx context.Context, tenantID, legalEntityID, purchaseOrderID string) (*purchaseorder.Summary, error) {
	s, err := p.GetOrder(ctx, tenantID, legalEntityID, purchaseOrderID)
	if err != nil {
		return nil, err
	}
	if s.Status != "ISSUED" {
		return nil, domain.ErrPurchaseOrderNotOpen
	}
	return s, nil
}

var _ purchaseorder.Client = (*stubPO)(nil)

// ── stub general-ledger-svc client ──────────────────────────────────────────

// stubLedger tracks every correlation ID it was asked to post — a repeat
// call for the same CorrelationID is the real GL's idempotency contract,
// and this stub records how many times each was actually seen so a test can
// assert a replay never posted a second entry (negative-path #4).
type stubLedger struct {
	fail           bool
	postedByCorrID map[string]int
}

func newStubLedger() *stubLedger { return &stubLedger{postedByCorrID: map[string]int{}} }

func (l *stubLedger) PostGRNI(_ context.Context, params ledger.PostGRNIParams) (string, error) {
	if l.fail {
		return "", ledger.ErrGeneralLedgerUnavailable
	}
	l.postedByCorrID[params.CorrelationID]++
	return "journal-" + params.CorrelationID, nil
}

var _ ledger.Client = (*stubLedger)(nil)

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap04-1"
const testLegalEntity = "le-ap04-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, po *stubPO, gl *stubLedger) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, po, gl, handler.Config{
		GRNIDebitAccountCode: "5100-GRNI", GRNICreditAccountCode: "2100-GRNI", OverReceiptTolerancePct: 0,
	}, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequestAs(r http.Handler, method, path string, body interface{}, tenantID, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, tenantID, "principal-receiver")
}

func newReceiptReq(poID string) domain.CreateReceiptRequest {
	return domain.CreateReceiptRequest{
		LegalEntityID: testLegalEntity, PurchaseOrderID: poID, ReceiptType: domain.ReceiptTypeGoods,
		Quantity: 10, UnitOfMeasure: "EA", Amount: 1000, CurrencyCode: "USD", ReceiptDate: time.Now().UTC(),
		Location: "warehouse-1",
	}
}

func createReceipt(t *testing.T, r http.Handler, req domain.CreateReceiptRequest) *domain.GoodsServiceReceipt {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/ap04/receipts/", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createReceipt: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var rcpt domain.GoodsServiceReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &rcpt)
	return &rcpt
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreateReceipt_Draft(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-1", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-1"))
	if rcpt.Status != domain.StatusDraft {
		t.Fatalf("expected DRAFT, got %s", rcpt.Status)
	}
	if rcpt.ReceiverPrincipalID != "principal-receiver" {
		t.Fatalf("expected receiver to be the creating principal, got %s", rcpt.ReceiverPrincipalID)
	}
}

// TestCreateReceipt_ClosedPO_Rejected is the create-time half of negative-
// path scenario #2 ("receipt confirmed against cancelled PO" — the nearest
// real equivalent purchase-order-svc has is CLOSED, see internal/domain's
// package doc).
func TestCreateReceipt_ClosedPO_Rejected(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-closed", testTenant, testLegalEntity, "CLOSED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	w := doRequest(r, http.MethodPost, "/ap04/receipts/", newReceiptReq("po-closed"), testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for closed PO, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateReceipt_UnknownPO_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPO(), newStubLedger())
	w := doRequest(r, http.MethodPost, "/ap04/receipts/", newReceiptReq("po-does-not-exist"), testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown PO, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfirmReceipt_Success_PostsGRNI(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-2", testTenant, testLegalEntity, "ISSUED", 5000)
	gl := newStubLedger()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, gl)

	rcpt := createReceipt(t, r, newReceiptReq("po-2"))
	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/confirm", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 confirming, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Receipt         domain.GoodsServiceReceipt    `json:"receipt"`
		AccountingEvent domain.ReceiptAccountingEvent `json:"accounting_event"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Receipt.Status != domain.StatusConfirmed {
		t.Fatalf("expected CONFIRMED, got %s", resp.Receipt.Status)
	}
	if resp.AccountingEvent.Status != domain.AccountingPosted {
		t.Fatalf("expected accounting event POSTED, got %s", resp.AccountingEvent.Status)
	}
	if gl.postedByCorrID[rcpt.ReceiptID] != 1 {
		t.Fatalf("expected exactly one GRNI post keyed by receipt ID, got %d", gl.postedByCorrID[rcpt.ReceiptID])
	}
}

// TestConfirmReceipt_GLFailure_RecordsExceptionButStillConfirms is the
// literal enforcement of AP-04's own stated failure semantics: "accounting
// event failure leaves visible pending/exception state, never silent
// receipt deletion." The receipt confirmation must stand even when GL
// posting fails.
func TestConfirmReceipt_GLFailure_RecordsExceptionButStillConfirms(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-3", testTenant, testLegalEntity, "ISSUED", 5000)
	gl := newStubLedger()
	gl.fail = true
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, gl)

	rcpt := createReceipt(t, r, newReceiptReq("po-3"))
	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/confirm", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 confirming even on GL failure, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Receipt         domain.GoodsServiceReceipt    `json:"receipt"`
		AccountingEvent domain.ReceiptAccountingEvent `json:"accounting_event"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Receipt.Status != domain.StatusConfirmed {
		t.Fatalf("expected receipt to remain CONFIRMED despite GL failure, got %s", resp.Receipt.Status)
	}
	if resp.AccountingEvent.Status != domain.AccountingException {
		t.Fatalf("expected accounting event EXCEPTION, got %s", resp.AccountingEvent.Status)
	}
}

// TestConfirmReceipt_OverTolerance_Rejected is negative-path scenario #1.
func TestConfirmReceipt_OverTolerance_Rejected(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-4", testTenant, testLegalEntity, "ISSUED", 500) // receipt amount (1000) exceeds this
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-4"))
	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/confirm", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 over-tolerance, got %d: %s", w.Code, w.Body.String())
	}
}

func TestConfirmReceipt_OverTolerance_WithApprovedException_Succeeds(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-5", testTenant, testLegalEntity, "ISSUED", 500)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	req := newReceiptReq("po-5")
	req.ToleranceExceptionRef = "exception-approval-ref-123"
	rcpt := createReceipt(t, r, req)
	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/confirm", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with approved exception, got %d: %s", w.Code, w.Body.String())
	}
}

// TestConfirmReceipt_POClosedSinceCreate_Rejected is negative-path scenario
// #2's confirm-time half: the PO may have closed after the receipt was
// created but before it was confirmed — this must be re-checked live.
func TestConfirmReceipt_POClosedSinceCreate_Rejected(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-6", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-6"))
	po.addOrder("po-6", testTenant, testLegalEntity, "CLOSED", 5000) // PO closes after receipt was drafted

	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/confirm", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for PO closed since create, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRejectReceipt(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-7", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-7"))
	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/reject", domain.RejectReceiptRequest{Reason: "wrong item delivered"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 rejecting, got %d: %s", w.Code, w.Body.String())
	}
	var rejected domain.GoodsServiceReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &rejected)
	if rejected.Status != domain.StatusRejected {
		t.Fatalf("expected REJECTED, got %s", rejected.Status)
	}
}

func TestReverseReceipt_PartialThenFull(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-8", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-8"))
	doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/confirm", nil, testTenant)

	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/reverse",
		domain.ReverseReceiptRequest{ReversedAmount: 400, Reason: "partial return"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 partial reverse, got %d: %s", w.Code, w.Body.String())
	}
	var partial domain.GoodsServiceReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &partial)
	if partial.Status != domain.StatusPartiallyReversed {
		t.Fatalf("expected PARTIALLY_REVERSED, got %s", partial.Status)
	}

	w = doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/reverse",
		domain.ReverseReceiptRequest{ReversedAmount: 600, Reason: "rest returned"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 full reverse, got %d: %s", w.Code, w.Body.String())
	}
	var full domain.GoodsServiceReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &full)
	if full.Status != domain.StatusFullyReversed {
		t.Fatalf("expected FULLY_REVERSED, got %s", full.Status)
	}
}

func TestReverseReceipt_OverReversal_Rejected(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-9", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-9"))
	doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/confirm", nil, testTenant)

	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/reverse",
		domain.ReverseReceiptRequest{ReversedAmount: 5000, Reason: "way too much"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 over-reversal, got %d: %s", w.Code, w.Body.String())
	}
}

// TestServiceAcceptance_SelfCertification_Denied is AP-04's own SoD line:
// "receiver/acceptor cannot self-certify sensitive services where policy
// requires independent acceptance" — enforced via authorization-svc's
// dynamic own-object SoD layer, same feature and calling pattern as AP-01.
func TestServiceAcceptance_SelfCertification_Denied(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-10", testTenant, testLegalEntity, "ISSUED", 5000)
	az := &stubAuthz{sodRules: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, po, newStubLedger())

	req := newReceiptReq("po-10")
	req.ReceiptType = domain.ReceiptTypeService
	req.RequiresIndependentAcceptance = true
	rcpt := createReceipt(t, r, req) // created (and thus received) by "principal-receiver"

	w := doRequestAs(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/service-acceptance",
		domain.RecordServiceAcceptanceRequest{EvidenceRef: "evidence-1"}, testTenant, "principal-receiver")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-certification denied, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServiceAcceptance_IndependentAcceptor_Succeeds(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-11", testTenant, testLegalEntity, "ISSUED", 5000)
	az := &stubAuthz{sodRules: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, po, newStubLedger())

	req := newReceiptReq("po-11")
	req.ReceiptType = domain.ReceiptTypeService
	req.RequiresIndependentAcceptance = true
	rcpt := createReceipt(t, r, req)

	w := doRequestAs(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/service-acceptance",
		domain.RecordServiceAcceptanceRequest{EvidenceRef: "evidence-1"}, testTenant, "principal-independent-acceptor")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 independent acceptance, got %d: %s", w.Code, w.Body.String())
	}
	var accepted domain.GoodsServiceReceipt
	_ = json.Unmarshal(w.Body.Bytes(), &accepted)
	if accepted.Status != domain.StatusPendingConfirmation {
		t.Fatalf("expected PENDING_CONFIRMATION, got %s", accepted.Status)
	}
}

func TestAttachReceiptEvidence(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-12", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-12"))
	w := doRequest(r, http.MethodPost, "/ap04/receipts/"+rcpt.ReceiptID+"/evidence",
		domain.AttachReceiptEvidenceRequest{EvidenceRef: "delivery-note-99", Description: "signed delivery note"}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 attaching evidence, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequest(r, http.MethodGet, "/ap04/receipts/"+rcpt.ReceiptID+"/evidence", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 listing evidence, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetAvailableActions_Draft(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-13", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-13"))
	w := doRequest(r, http.MethodGet, "/ap04/receipts/"+rcpt.ReceiptID+"/available-actions", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		AvailableActions []string `json:"available_actions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	found := map[string]bool{}
	for _, a := range resp.AvailableActions {
		found[a] = true
	}
	if !found["ConfirmReceipt"] || !found["RejectReceipt"] || !found["AmendReceiptDraft"] {
		t.Fatalf("expected DRAFT goods receipt to allow confirm/reject/amend, got %v", resp.AvailableActions)
	}
	if found["ReverseReceipt"] {
		t.Fatalf("did not expect ReverseReceipt to be available on a DRAFT receipt, got %v", resp.AvailableActions)
	}
}

func TestGetReceiptAccountingStatus_NotYetConfirmed(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-14", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, po, newStubLedger())

	rcpt := createReceipt(t, r, newReceiptReq("po-14"))
	w := doRequest(r, http.MethodGet, "/ap04/receipts/"+rcpt.ReceiptID+"/accounting-status", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "NOT_APPLICABLE" {
		t.Fatalf("expected NOT_APPLICABLE before confirmation, got %s", resp.Status)
	}
}

func TestGetReceipt_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPO(), newStubLedger())
	w := doRequest(r, http.MethodGet, "/ap04/receipts/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateReceipt_AuthorizationDenied(t *testing.T) {
	po := newStubPO()
	po.addOrder("po-15", testTenant, testLegalEntity, "ISSUED", 5000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{deny: true}, po, newStubLedger())

	w := doRequest(r, http.MethodPost, "/ap04/receipts/", newReceiptReq("po-15"), testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}
