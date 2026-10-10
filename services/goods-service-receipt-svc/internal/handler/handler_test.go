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
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/goods-service-receipt-svc/internal/authz"
	"zoiko.io/goods-service-receipt-svc/internal/domain"
	"zoiko.io/goods-service-receipt-svc/internal/handler"
	"zoiko.io/goods-service-receipt-svc/internal/middleware"
	"zoiko.io/goods-service-receipt-svc/internal/purchaseorder"
)

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
	entity  = "33333333-3333-3333-3333-333333333333"

	receiver  = "principal-receiver"
	confirmer = "principal-confirmer"
)

func ver(n int) *int { return &n }

// ── stub authz — including the own-object SoD layer ─────────────────────────

// stubAuthz stands in for authorization-svc: a blanket deny, per-action denial,
// and — with sodRules — its own-object rule, which denies a principal acting on
// an object they own (a receiver self-certifying their own receipt).
type stubAuthz struct {
	deny        bool
	down        bool
	denyActions map[string]bool
	sodRules    bool
	checked     []string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, action string) error {
	a.checked = append(a.checked, action)
	if a.down {
		return authzpkg.ErrAuthzServiceUnavailable
	}
	if a.deny || a.denyActions[action] {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func (a *stubAuthz) CheckAllowedOwnObject(_ context.Context, principal, _, action, owner string) error {
	a.checked = append(a.checked, action)
	if a.down {
		return authzpkg.ErrAuthzServiceUnavailable
	}
	if a.deny || a.denyActions[action] || (a.sodRules && principal == owner) {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── stub purchase-order-svc client ──────────────────────────────────────────

type stubPO struct {
	orders map[string]*purchaseorder.Summary
	open   map[string]float64 // line id -> open_receipt_quantity
	down   bool
}

func newStubPO() *stubPO {
	return &stubPO{orders: map[string]*purchaseorder.Summary{}, open: map[string]float64{}}
}

// addOrder registers an order and returns its id; lines are (quantity, unit price).
func (p *stubPO) addOrder(tenant, status string, total float64, lines ...purchaseorder.Line) string {
	id := uuid.NewString()
	for i := range lines {
		if lines[i].LineID == "" {
			lines[i].LineID = uuid.NewString()
		}
		lines[i].LineNumber = i + 1
		p.open[lines[i].LineID] = lines[i].Quantity
	}
	p.orders[id] = &purchaseorder.Summary{PurchaseOrderID: id, TenantID: tenant, LegalEntityID: entity, Status: status,
		TotalAmount: total, CurrencyCode: "USD", Revision: 3, Lines: lines}
	return id
}

func (p *stubPO) GetOrder(_ context.Context, tenantID, legalEntityID, id string) (*purchaseorder.Summary, error) {
	if p.down {
		return nil, domain.ErrPurchaseOrderServiceUnavailable
	}
	s, ok := p.orders[id]
	if !ok {
		return nil, domain.ErrPurchaseOrderNotFound
	}
	if s.TenantID != tenantID || (legalEntityID != "" && s.LegalEntityID != legalEntityID) {
		return nil, domain.ErrPurchaseOrderMismatch
	}
	c := *s
	return &c, nil
}

func (p *stubPO) GetOpenOrder(ctx context.Context, tenantID, legalEntityID, id string) (*purchaseorder.Summary, error) {
	s, err := p.GetOrder(ctx, tenantID, legalEntityID, id)
	if err != nil {
		return nil, err
	}
	if s.Status != purchaseorder.StatusIssued {
		return nil, &domain.PurchaseOrderNotOpenError{Status: s.Status}
	}
	return s, nil
}

func (p *stubPO) GetOpenQuantity(_ context.Context, _, _, id string) ([]purchaseorder.OpenQuantityLine, error) {
	if p.down {
		return nil, domain.ErrPurchaseOrderServiceUnavailable
	}
	var out []purchaseorder.OpenQuantityLine
	for _, l := range p.orders[id].Lines {
		out = append(out, purchaseorder.OpenQuantityLine{LineID: l.LineID, OrderedQuantity: l.Quantity, OpenReceiptQuantity: p.open[l.LineID]})
	}
	return out, nil
}

func (p *stubPO) PostProgress(context.Context, purchaseorder.ProgressPush) error { return nil }

var _ purchaseorder.Client = (*stubPO)(nil)

// ── harness ──────────────────────────────────────────────────────────────────

type env struct {
	store  *stubStore
	authz  *stubAuthz
	po     *stubPO
	router chi.Router
}

func newEnv(tolerancePct float64) *env {
	e := &env{store: newStubStore(), authz: &stubAuthz{}, po: newStubPO()}
	h := handler.New(e.store, e.authz, e.po, handler.Config{OverReceiptTolerancePct: tolerancePct}, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	e.router = r
	return e
}

func (e *env) do(method, path string, body any, principal, tenant string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *env) as(principal, method, path string, body any) *httptest.ResponseRecorder {
	return e.do(method, path, body, principal, tenantA)
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e apiError
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Code
}

func reqFor(po string) domain.CreateReceiptRequest {
	return domain.CreateReceiptRequest{
		LegalEntityID: entity, PurchaseOrderID: po, ReceiptType: domain.ReceiptTypeGoods, Quantity: 10, UnitOfMeasure: "EA",
		Amount: 1000, CurrencyCode: "USD", ReceiptDate: time.Now().UTC(), Location: "warehouse-1",
	}
}

func (e *env) create(t *testing.T, req domain.CreateReceiptRequest) *domain.GoodsServiceReceipt {
	t.Helper()
	rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var r domain.GoodsServiceReceipt
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return &r
}

type confirmResp struct {
	Receipt         domain.GoodsServiceReceipt     `json:"receipt"`
	AccountingEvent *domain.ReceiptAccountingEvent `json:"accounting_event"`
}

func (e *env) confirm(t *testing.T, r *domain.GoodsServiceReceipt, body domain.ConfirmReceiptRequest) (*httptest.ResponseRecorder, confirmResp) {
	t.Helper()
	if body.ExpectedVersion == nil {
		body.ExpectedVersion = ver(e.store.receipts[r.ReceiptID].Version)
	}
	rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/confirm", body)
	var out confirmResp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (e *env) confirmed(t *testing.T, po string) *domain.GoodsServiceReceipt {
	t.Helper()
	r := e.create(t, reqFor(po))
	if rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	return e.store.receipts[r.ReceiptID]
}

// ── CreateReceipt ────────────────────────────────────────────────────────────

func TestCreateReceipt_Draft(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))
	if r.Status != domain.StatusDraft || r.Version != 1 || r.ReceiverPrincipalID != receiver {
		t.Fatalf("expected a DRAFT at version 1 received by the creator, got %+v", r)
	}
	if len(e.store.events) != 2 || e.store.events[0] != domain.EventReceiptCreated {
		t.Fatalf("expected the created event and its alias, got %v", e.store.events)
	}
}

// Negative path 2, create-time half: a receipt cannot be opened against a PO that
// is draft, held, cancelled or closed.
func TestCreateReceipt_NotIssuedPO_Rejected(t *testing.T) {
	for _, st := range []string{"CLOSED", "CANCELLED", "ON_HOLD", "DRAFT", "APPROVED"} {
		e := newEnv(0)
		po := e.po.addOrder(tenantA, st, 5000)
		rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", reqFor(po))
		if rec.Code != http.StatusConflict || codeOf(t, rec) != "PO_NOT_OPEN" {
			t.Fatalf("%s: expected 409 PO_NOT_OPEN, got %d %s", st, rec.Code, rec.Body.String())
		}
		if len(e.store.receipts) != 0 {
			t.Fatalf("%s: nothing may be written", st)
		}
	}
}

func TestCreateReceipt_PO_UnknownMismatchedAndUnavailable(t *testing.T) {
	e := newEnv(0)
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", reqFor(uuid.NewString())); rec.Code != http.StatusBadRequest || codeOf(t, rec) != "PO_NOT_FOUND" {
		t.Fatalf("unknown PO: expected 400 PO_NOT_FOUND, got %d", rec.Code)
	}
	other := e.po.addOrder(tenantB, "ISSUED", 5000)
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", reqFor(other)); rec.Code != http.StatusForbidden || codeOf(t, rec) != "PO_MISMATCH" {
		t.Fatalf("another tenant's PO: expected 403 PO_MISMATCH, got %d", rec.Code)
	}
	e.po.down = true
	own := e.po.addOrder(tenantA, "ISSUED", 5000)
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", reqFor(own)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable purchase-order-svc must fail closed (503), got %d", rec.Code)
	}
}

func TestCreateReceipt_Validation(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	for name, mod := range map[string]func(*domain.CreateReceiptRequest){
		"bad purchase_order_id": func(r *domain.CreateReceiptRequest) { r.PurchaseOrderID = "po-1" },
		"bad legal_entity_id":   func(r *domain.CreateReceiptRequest) { r.LegalEntityID = "le-1" },
		"bad po_line_id":        func(r *domain.CreateReceiptRequest) { r.POLineID = "line-1" },
		"bad receipt_type":      func(r *domain.CreateReceiptRequest) { r.ReceiptType = "OTHER" },
		"zero amount":           func(r *domain.CreateReceiptRequest) { r.Amount = 0 },
		"zero quantity":         func(r *domain.CreateReceiptRequest) { r.Quantity = 0 },
		"no currency":           func(r *domain.CreateReceiptRequest) { r.CurrencyCode = "" },
		"no receipt date":       func(r *domain.CreateReceiptRequest) { r.ReceiptDate = time.Time{} },
	} {
		req := reqFor(po)
		mod(&req)
		if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", req); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	if len(e.store.receipts) != 0 {
		t.Fatal("an invalid create must write nothing")
	}
}

func TestCreateReceipt_CurrencyMismatch_And_LineNotOnPO(t *testing.T) {
	e := newEnv(0)
	line := purchaseorder.Line{Quantity: 100, UnitPrice: 10}
	po := e.po.addOrder(tenantA, "ISSUED", 1000, line)
	req := reqFor(po)
	req.CurrencyCode = "EUR"
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", req); rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "CURRENCY_MISMATCH" {
		t.Fatalf("expected 422 CURRENCY_MISMATCH, got %d %s", rec.Code, rec.Body.String())
	}
	req = reqFor(po)
	req.POLineID = uuid.NewString()
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", req); rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "PO_LINE_INVALID" {
		t.Fatalf("expected 422 PO_LINE_INVALID, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateReceipt_Auth_TenantAndIdentity(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	if rec := e.do(http.MethodPost, "/ap04/receipts/", reqFor(po), "", tenantA); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: expected 401, got %d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/ap04/receipts/", reqFor(po), receiver, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant scope: expected 401, got %d", rec.Code)
	}
	e.authz.deny = true
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", reqFor(po)); rec.Code != http.StatusForbidden || codeOf(t, rec) != "FORBIDDEN" {
		t.Fatalf("denied: expected 403 FORBIDDEN, got %d", rec.Code)
	}
	e.authz.deny, e.authz.down = false, true
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", reqFor(po)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authorization-svc down must fail closed (503), got %d", rec.Code)
	}
	if len(e.store.receipts) != 0 {
		t.Fatal("a refused create must write nothing")
	}
}

func TestCreateReceipt_ExceptionRefNeedsToleranceOverridePermission(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	req := reqFor(po)
	req.ToleranceExceptionRef = "EXC-1"
	e.authz.denyActions = map[string]bool{handler.ToleranceOverride: true}
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", req); rec.Code != http.StatusForbidden {
		t.Fatalf("an exception reference without the override permission must be refused, got %d", rec.Code)
	}
	e.authz.denyActions = nil
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", req); rec.Code != http.StatusCreated {
		t.Fatalf("with the override permission: expected 201, got %d", rec.Code)
	}
}

// ── ConfirmReceipt ───────────────────────────────────────────────────────────

func TestConfirmReceipt_Success_QueuesGRNIOnce_AndRecordsEvidence(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))
	rec, resp := e.confirm(t, r, domain.ConfirmReceiptRequest{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := resp.Receipt
	if got.Status != domain.StatusConfirmed || got.Version != 2 || got.ConfirmedByPrincipalID == nil || *got.ConfirmedByPrincipalID != confirmer ||
		got.ConfirmedAt == nil || got.PORevision == nil || *got.PORevision != 3 {
		t.Fatalf("expected CONFIRMED v2 with confirmer, time and the PO revision observed, got %+v", got)
	}
	if resp.AccountingEvent == nil || resp.AccountingEvent.Status != domain.AccountingPending ||
		resp.AccountingEvent.Direction != domain.DirectionAccrue || *resp.AccountingEvent.SourceEventID != r.ReceiptID {
		t.Fatalf("expected one PENDING ACCRUE request keyed by the receipt id, got %+v", resp.AccountingEvent)
	}
	if len(e.store.posting) != 1 {
		t.Fatalf("expected exactly one GRNI posting request, got %d", len(e.store.posting))
	}
}

// Negative path 4: GRNI emitted twice on replay. A second confirmation is refused
// by the state machine, and even a forced re-queue of the same source event
// queues nothing: the consequence exists once.
func TestConfirmReceipt_Replay_NeverQueuesGRNITwice(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.confirmed(t, po)

	rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{ExpectedVersion: ver(2)})
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "INVALID_TRANSITION" {
		t.Fatalf("a second confirmation must be refused 409 INVALID_TRANSITION, got %d %s", rec.Code, rec.Body.String())
	}
	if len(e.store.posting) != 1 {
		t.Fatalf("a replay must not queue a second GRNI request, got %d", len(e.store.posting))
	}
	if again := e.store.queue(r, domain.DirectionAccrue, r.ReceiptID); again != nil {
		t.Fatal("the unique source-event key must swallow a duplicate request")
	}
}

func TestConfirmReceipt_RequiresExpectedVersion_AndRefusesStale(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))
	rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/confirm", nil)
	if rec.Code != http.StatusBadRequest || codeOf(t, rec) != "EXPECTED_VERSION_REQUIRED" {
		t.Fatalf("expected 400 EXPECTED_VERSION_REQUIRED, got %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = e.confirm(t, r, domain.ConfirmReceiptRequest{ExpectedVersion: ver(9)})
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "STALE_VERSION" {
		t.Fatalf("expected 409 STALE_VERSION, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.receipts[r.ReceiptID].Status != domain.StatusDraft || len(e.store.posting) != 0 {
		t.Fatal("a refused confirmation must change nothing and queue nothing")
	}
}

func TestConfirmReceipt_ExpectedVersionFromEnvelopeHeader(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))
	req := httptest.NewRequest(http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/confirm", nil)
	req.Header.Set("X-Principal-Id", confirmer)
	req.Header.Set("X-Tenant-Id", tenantA)
	req.Header.Set("X-Expected-Version", "1")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("X-Expected-Version must satisfy the requirement, got %d %s", rec.Code, rec.Body.String())
	}
}

// Negative path 1: receipt exceeds PO tolerance without an approved exception.
func TestConfirmReceipt_HeaderLevel_OverTolerance_BlockedUnlessApprovedException(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 1500)
	first := e.confirmed(t, po) // 1000 of 1500
	_ = first
	second := e.create(t, reqFor(po)) // another 1000: 2000 > 1500
	rec, _ := e.confirm(t, second, domain.ConfirmReceiptRequest{})
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "OVER_RECEIPT_TOLERANCE" {
		t.Fatalf("expected 409 OVER_RECEIPT_TOLERANCE, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.receipts[second.ReceiptID].Status != domain.StatusDraft || len(e.store.posting) != 1 {
		t.Fatal("a blocked over-tolerance confirmation must change nothing and queue nothing")
	}

	// An approved exception (reference + the override permission) lets it through.
	rec, _ = e.confirm(t, second, domain.ConfirmReceiptRequest{ToleranceExceptionRef: "EXC-42"})
	if rec.Code != http.StatusOK {
		t.Fatalf("with an approved exception: expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.receipts[second.ReceiptID].ToleranceExceptionRef != "EXC-42" {
		t.Fatal("the exception reference must be recorded on the receipt as evidence")
	}
}

func TestConfirmReceipt_ExceptionWithoutOverridePermission_Refused(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 500)
	r := e.create(t, reqFor(po)) // 1000 > 500
	e.authz.denyActions = map[string]bool{handler.ToleranceOverride: true}
	rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{ToleranceExceptionRef: "EXC-1"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a confirmer without the override permission must not use an exception, got %d", rec.Code)
	}
	if e.store.receipts[r.ReceiptID].Status != domain.StatusDraft {
		t.Fatal("nothing may change")
	}
}

func TestConfirmReceipt_ConfiguredTolerancePercent_Allows(t *testing.T) {
	e := newEnv(10) // 10% over
	po := e.po.addOrder(tenantA, "ISSUED", 950)
	r := e.create(t, reqFor(po)) // 1000 <= 950 * 1.10 = 1045
	if rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusOK {
		t.Fatalf("within the configured tolerance: expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	r2 := e.create(t, reqFor(po)) // net 1000 + 1000 > 1045
	if rec, _ := e.confirm(t, r2, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusConflict {
		t.Fatalf("beyond the configured tolerance: expected 409, got %d", rec.Code)
	}
}

// Line receipts: quantity is checked against AP-03's open quantity, less this
// service's own undelivered pushes, plus the tolerance of the ordered quantity.
func TestConfirmReceipt_LineLevel_UsesOpenQuantityAndPendingPushes(t *testing.T) {
	e := newEnv(0)
	line := purchaseorder.Line{Quantity: 15, UnitPrice: 100}
	po := e.po.addOrder(tenantA, "ISSUED", 1500, line)
	lineID := e.po.orders[po].Lines[0].LineID

	req := reqFor(po)
	req.POLineID = lineID
	r1 := e.create(t, req) // 10 of 15
	r2 := e.create(t, req) // also 10 of 15: both drafts fit while nothing is confirmed
	rec, resp := e.confirm(t, r1, domain.ConfirmReceiptRequest{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	if resp.Receipt.ProgressPushStatus != domain.PushPending || len(e.store.pushes) != 1 || e.store.pushes[0].DeltaSign != 1 || e.store.pushes[0].Quantity != 10 {
		t.Fatalf("a line confirmation must queue one +10 push to AP-03, got %+v / %s", e.store.pushes, resp.Receipt.ProgressPushStatus)
	}

	// AP-03 has not been told yet (open is still 15); the 10 pending must count.
	if rec, _ := e.confirm(t, r2, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusConflict || codeOf(t, rec) != "OVER_RECEIPT_TOLERANCE" {
		t.Fatalf("5 open less 10 pending cannot take 10: expected 409, got %d %s", rec.Code, rec.Body.String())
	}
	// Once AP-03 has been told (push delivered, open now 5) a receipt of 5 fits.
	e.store.pushes[0].Status = "DELIVERED"
	e.po.open[lineID] = 5
	fit := reqFor(po)
	fit.POLineID, fit.Quantity, fit.Amount = lineID, 5, 500
	r3 := e.create(t, fit)
	if rec, _ := e.confirm(t, r3, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusOK {
		t.Fatalf("5 against 5 open: expected 200, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateReceipt_LineOverOpenQuantity_RefusedEarly(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 1500, purchaseorder.Line{Quantity: 5, UnitPrice: 100})
	req := reqFor(po)
	req.POLineID = e.po.orders[po].Lines[0].LineID // quantity 10 > 5 open
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/", req); rec.Code != http.StatusConflict || codeOf(t, rec) != "OVER_RECEIPT_TOLERANCE" {
		t.Fatalf("expected 409 at create, got %d %s", rec.Code, rec.Body.String())
	}
}

// Negative path 2, confirm-time half: the PO was cancelled after the draft.
func TestConfirmReceipt_POCancelledSinceCreate_Rejected(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))
	e.po.orders[po].Status = "CANCELLED"
	rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{})
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "PO_NOT_OPEN" {
		t.Fatalf("expected 409 PO_NOT_OPEN, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.receipts[r.ReceiptID].Status != domain.StatusDraft || len(e.store.posting) != 0 {
		t.Fatal("a refused confirmation must change nothing and queue nothing")
	}
}

func TestConfirmReceipt_PODown_FailsClosed(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))
	e.po.down = true
	if rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if e.store.receipts[r.ReceiptID].Status != domain.StatusDraft {
		t.Fatal("an unverifiable PO must not let a receipt through")
	}
}

// SoD: where independent acceptance is required, the receiver cannot confirm
// their own receipt — authorization-svc's own-object layer denies it.
func TestConfirmReceipt_ReceiverCannotSelfCertify_WhenIndependentAcceptanceRequired(t *testing.T) {
	e := newEnv(0)
	e.authz.sodRules = true
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	req := reqFor(po)
	req.RequiresIndependentAcceptance = true
	r := e.create(t, req)

	rec := e.as(receiver, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/confirm", domain.ConfirmReceiptRequest{ExpectedVersion: ver(1)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-certification, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.receipts[r.ReceiptID].Status != domain.StatusDraft {
		t.Fatal("a refused self-certification must change nothing")
	}
	if rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusOK {
		t.Fatalf("an independent confirmer must succeed, got %d", rec.Code)
	}
}

// ── Reject ───────────────────────────────────────────────────────────────────

func TestRejectReceipt(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))

	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/reject", domain.RejectReceiptRequest{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a rejection needs a reason, got %d", rec.Code)
	}
	rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/reject", domain.RejectReceiptRequest{Reason: "damaged", ExpectedVersion: ver(1)})
	var got domain.GoodsServiceReceipt
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Status != domain.StatusRejected || got.RejectionReason != "damaged" || got.Version != 2 {
		t.Fatalf("expected REJECTED v2 with the reason, got %d %+v", rec.Code, got)
	}
	if len(e.store.posting) != 0 {
		t.Fatal("a rejection has no accounting consequence")
	}
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/reject", domain.RejectReceiptRequest{Reason: "again"}); rec.Code != http.StatusConflict {
		t.Fatalf("a rejected receipt is terminal, got %d", rec.Code)
	}
	if rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusConflict {
		t.Fatalf("a rejected receipt cannot be confirmed, got %d", rec.Code)
	}
}

func TestRejectReceipt_ConfirmedIsNotRejectable(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.confirmed(t, po)
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/reject", domain.RejectReceiptRequest{Reason: "x"}); rec.Code != http.StatusConflict {
		t.Fatalf("a confirmed receipt is corrected by reversal, not rejection; got %d", rec.Code)
	}
}

// ── Reverse / immutability ───────────────────────────────────────────────────

func TestReverseReceipt_PartialThenFull_AccumulatesAndMirrorsAccounting(t *testing.T) {
	e := newEnv(0)
	line := purchaseorder.Line{Quantity: 10, UnitPrice: 100}
	po := e.po.addOrder(tenantA, "ISSUED", 1000, line)
	req := reqFor(po)
	req.POLineID = e.po.orders[po].Lines[0].LineID
	r := e.create(t, req)
	e.confirm(t, r, domain.ConfirmReceiptRequest{})
	id := r.ReceiptID

	rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+id+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 400, Reason: "damaged"})
	var got struct {
		domain.GoodsServiceReceipt
		Reversal *domain.ReceiptReversal `json:"reversal"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Status != domain.StatusPartiallyReversed || got.ReversedAmount != 400 || got.ReversedQuantity != 4 ||
		got.Reversal == nil || got.Reversal.ReversedQuantity != 4 {
		t.Fatalf("expected PARTIALLY_REVERSED 400 / 4 units, got %d %+v", rec.Code, got)
	}

	rec = e.as(confirmer, http.MethodPost, "/ap04/receipts/"+id+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 600, Reason: "rest"})
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Status != domain.StatusFullyReversed || got.ReversedQuantity != 10 {
		t.Fatalf("expected FULLY_REVERSED with all 10 units reversed, got %d %+v", rec.Code, got)
	}

	var neg, accrue, reverse int
	for _, p := range e.store.pushes {
		if p.DeltaSign < 0 {
			neg++
		}
	}
	for _, p := range e.store.posting {
		if p.Direction == domain.DirectionAccrue {
			accrue++
		} else {
			reverse++
		}
	}
	if len(e.store.pushes) != 3 || neg != 2 || accrue != 1 || reverse != 2 {
		t.Fatalf("expected 1 +push and 2 -pushes, 1 ACCRUE and 2 REVERSE requests; got pushes=%d neg=%d accrue=%d reverse=%d", len(e.store.pushes), neg, accrue, reverse)
	}
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+id+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 1, Reason: "more"}); rec.Code != http.StatusConflict {
		t.Fatalf("a fully reversed receipt accepts no further reversal, got %d", rec.Code)
	}
}

func TestReverseReceipt_OverReversal_Rejected(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.confirmed(t, po)
	rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 1000.01, Reason: "x"})
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "OVER_REVERSAL" {
		t.Fatalf("expected 409 OVER_REVERSAL, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.receipts[r.ReceiptID].ReversedAmount != 0 {
		t.Fatal("a refused reversal must change nothing")
	}
}

func TestReverseReceipt_Validation_NotConfirmed_Unknown_Stale(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	draft := e.create(t, reqFor(po))
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+draft.ReceiptID+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 10}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a reversal needs a reason, got %d", rec.Code)
	}
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+draft.ReceiptID+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 10, Reason: "x"}); rec.Code != http.StatusConflict {
		t.Fatalf("an unconfirmed receipt cannot be reversed, got %d", rec.Code)
	}
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+uuid.NewString()+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 10, Reason: "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown receipt is 404, got %d", rec.Code)
	}
	conf := e.confirmed(t, po)
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+conf.ReceiptID+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 10, Reason: "x", ExpectedVersion: ver(1)}); rec.Code != http.StatusConflict || codeOf(t, rec) != "STALE_VERSION" {
		t.Fatalf("expected 409 STALE_VERSION, got %d %s", rec.Code, rec.Body.String())
	}
}

// Negative path 3: a confirmed receipt cannot be deleted or edited to fix a
// mismatch. There is no delete route, amend is DRAFT-only, and the only
// correction is the linked reversal.
func TestConfirmedReceipt_CannotBeDeletedOrAmended(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.confirmed(t, po)
	if rec := e.as(confirmer, http.MethodDelete, "/ap04/receipts/"+r.ReceiptID, nil); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Fatalf("there must be no way to delete a receipt, got %d", rec.Code)
	}
	q := 99.0
	rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/amend", domain.AmendReceiptDraftRequest{Quantity: &q})
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "INVALID_TRANSITION" {
		t.Fatalf("a confirmed receipt must refuse amendment, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.receipts[r.ReceiptID].Quantity != 10 {
		t.Fatal("the confirmed quantity must be untouched")
	}
}

func TestAmendReceiptDraft_Succeeds_BumpsVersion_Validates(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))
	q := 12.0
	rec := e.as(receiver, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/amend", domain.AmendReceiptDraftRequest{Quantity: &q, ExpectedVersion: ver(1)})
	var got domain.GoodsServiceReceipt
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Quantity != 12 || got.Version != 2 {
		t.Fatalf("expected quantity 12 at v2, got %d %+v", rec.Code, got)
	}
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/amend", domain.AmendReceiptDraftRequest{Quantity: &q, ExpectedVersion: ver(1)}); rec.Code != http.StatusConflict || codeOf(t, rec) != "STALE_VERSION" {
		t.Fatalf("expected 409 STALE_VERSION, got %d", rec.Code)
	}
	neg := -1.0
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/amend", domain.AmendReceiptDraftRequest{Amount: &neg}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a non-positive amount must be refused, got %d", rec.Code)
	}
}

// ── Service acceptance & evidence ────────────────────────────────────────────

func serviceReq(po string) domain.CreateReceiptRequest {
	r := reqFor(po)
	r.ReceiptType, r.RequiresIndependentAcceptance = domain.ReceiptTypeService, true
	return r
}

func TestServiceAcceptance_SelfCertificationDenied_IndependentAcceptorSucceeds(t *testing.T) {
	e := newEnv(0)
	e.authz.sodRules = true
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, serviceReq(po))

	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/service-acceptance", domain.RecordServiceAcceptanceRequest{EvidenceRef: "SIGNOFF-1"}); rec.Code != http.StatusForbidden {
		t.Fatalf("the receiver must not accept their own service, got %d", rec.Code)
	}
	rec := e.as("principal-acceptor", http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/service-acceptance", domain.RecordServiceAcceptanceRequest{EvidenceRef: "SIGNOFF-1", Notes: "milestone 1 done"})
	var got domain.GoodsServiceReceipt
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Status != domain.StatusPendingConfirmation || got.Version != 2 {
		t.Fatalf("expected PENDING_CONFIRMATION v2, got %d %+v", rec.Code, got)
	}
	if len(e.store.evidence[r.ReceiptID]) != 1 {
		t.Fatalf("expected the acceptance evidence recorded, got %d", len(e.store.evidence[r.ReceiptID]))
	}
	found := false
	for _, ev := range e.store.events {
		found = found || ev == domain.EventServiceAcceptanceRecorded
	}
	if !found {
		t.Fatalf("expected %s, got %v", domain.EventServiceAcceptanceRecorded, e.store.events)
	}
	// PENDING_CONFIRMATION can then be confirmed by an independent confirmer.
	if rec, _ := e.confirm(t, r, domain.ConfirmReceiptRequest{}); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 confirming an accepted service, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestServiceAcceptance_OnlyServiceReceiptsInDraft(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	goods := e.create(t, reqFor(po))
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+goods.ReceiptID+"/service-acceptance", domain.RecordServiceAcceptanceRequest{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a GOODS receipt takes no service acceptance, got %d", rec.Code)
	}
	svc := e.create(t, serviceReq(po))
	e.as(confirmer, http.MethodPost, "/ap04/receipts/"+svc.ReceiptID+"/service-acceptance", domain.RecordServiceAcceptanceRequest{})
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+svc.ReceiptID+"/service-acceptance", domain.RecordServiceAcceptanceRequest{}); rec.Code != http.StatusConflict {
		t.Fatalf("acceptance is recorded once, got %d", rec.Code)
	}
}

func TestEvidence_AttachAndList(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.confirmed(t, po)
	if rec := e.as(receiver, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/evidence", domain.AttachReceiptEvidenceRequest{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("evidence needs a reference, got %d", rec.Code)
	}
	rec := e.as(receiver, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/evidence", domain.AttachReceiptEvidenceRequest{EvidenceRef: "DN-1", Description: "delivery note"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = e.as(receiver, http.MethodGet, "/ap04/receipts/"+r.ReceiptID+"/evidence", nil)
	var list struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != http.StatusOK || list.Count != 1 {
		t.Fatalf("expected one evidence row, got %d %s", rec.Code, rec.Body.String())
	}
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestGetReceipt_NotFound_And_CrossTenantIsAbsent(t *testing.T) {
	e := newEnv(0)
	if rec := e.as(receiver, http.MethodGet, "/ap04/receipts/"+uuid.NewString(), nil); rec.Code != http.StatusNotFound || codeOf(t, rec) != "RECEIPT_NOT_FOUND" {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.create(t, reqFor(po))
	if rec := e.do(http.MethodGet, "/ap04/receipts/"+r.ReceiptID, nil, receiver, tenantB); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant's receipt must look absent (404), got %d", rec.Code)
	}
	if rec := e.do(http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 1, Reason: "x"}, receiver, tenantB); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant must not be able to command it, got %d", rec.Code)
	}
	if rec := e.do(http.MethodGet, "/ap04/receipts/"+r.ReceiptID, nil, receiver, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no tenant scope: expected 401, got %d", rec.Code)
	}
}

// A read with no principal is refused — unless it declares itself an internal
// service call (accounts-payable-svc's matching reading the receipt basis).
func TestReads_NoPrincipal_RefusedUnlessSystemChannel(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	r := e.confirmed(t, po)
	if rec := e.do(http.MethodGet, "/ap04/purchase-orders/"+po+"/received-to-date", nil, "", tenantA); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no principal, got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/ap04/purchase-orders/"+po+"/received-to-date", nil)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Tenant-Id", tenantA)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a system read must succeed, got %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/ap04/purchase-orders/"+po+"/received-to-date", nil)
	req.Header.Set("X-Source-Channel", "system")
	req.Header.Set("X-Tenant-Id", tenantB)
	rec = httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	var resp struct {
		Net float64 `json:"net_confirmed_amount"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Net != 0 {
		t.Fatalf("a system read is still tenant-scoped; tenant B saw %v", resp.Net)
	}
	_ = r
}

func TestGetAvailableActions_FollowStateAndSoD(t *testing.T) {
	actions := func(e *env, id, caller string) map[string]bool {
		rec := e.as(caller, http.MethodGet, "/ap04/receipts/"+id+"/available-actions", nil)
		var resp struct {
			AvailableActions []string `json:"available_actions"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		m := map[string]bool{}
		for _, a := range resp.AvailableActions {
			m[a] = true
		}
		return m
	}
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	draft := e.create(t, reqFor(po))
	if a := actions(e, draft.ReceiptID, receiver); !a["AmendReceiptDraft"] || !a["ConfirmReceipt"] || !a["RejectReceipt"] || a["ReverseReceipt"] || a["RecordServiceAcceptance"] {
		t.Fatalf("DRAFT goods actions wrong: %v", a)
	}
	svc := e.create(t, serviceReq(po))
	if a := actions(e, svc.ReceiptID, receiver); a["ConfirmReceipt"] || a["RecordServiceAcceptance"] {
		t.Fatalf("the receiver must not be offered Confirm/Accept on a receipt needing independent acceptance: %v", a)
	}
	if a := actions(e, svc.ReceiptID, "principal-acceptor"); !a["ConfirmReceipt"] || !a["RecordServiceAcceptance"] {
		t.Fatalf("an independent principal must be offered them: %v", a)
	}
	conf := e.confirmed(t, po)
	if a := actions(e, conf.ReceiptID, confirmer); !a["ReverseReceipt"] || a["ConfirmReceipt"] || a["AmendReceiptDraft"] {
		t.Fatalf("CONFIRMED actions wrong: %v", a)
	}
}

func TestGetReceiptAccountingStatus_NotApplicable_ThenPending_AndRequeue(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	draft := e.create(t, reqFor(po))
	rec := e.as(receiver, http.MethodGet, "/ap04/receipts/"+draft.ReceiptID+"/accounting-status", nil)
	var na map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &na)
	if rec.Code != http.StatusOK || na["status"] != "NOT_APPLICABLE" {
		t.Fatalf("an unconfirmed receipt has no accounting consequence, got %d %v", rec.Code, na)
	}

	r := e.confirmed(t, po)
	rec = e.as(receiver, http.MethodGet, "/ap04/receipts/"+r.ReceiptID+"/accounting-status", nil)
	var ev domain.ReceiptAccountingEvent
	_ = json.Unmarshal(rec.Body.Bytes(), &ev)
	if ev.Status != domain.AccountingPending || ev.Direction != domain.DirectionAccrue {
		t.Fatalf("expected the PENDING ACCRUE request, got %+v", ev)
	}

	// A quarantined request (e.g. a missing ACC-02 mapping) is visible and requeueable;
	// a POSTED one is never touched.
	e.store.posting[0].Status = domain.AccountingQuarantined
	e.authz.denyActions = map[string]bool{handler.ReceiptConfirm: true}
	if rec := e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/accounting/requeue", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("requeue needs the confirm authority, got %d", rec.Code)
	}
	e.authz.denyActions = nil
	rec = e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/accounting/requeue", nil)
	var rq struct {
		Requeued int64 `json:"requeued"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rq)
	if rec.Code != http.StatusOK || rq.Requeued != 1 || e.store.posting[0].Status != domain.AccountingPending {
		t.Fatalf("expected 1 requeued, got %d %+v", rec.Code, rq)
	}
	e.store.posting[0].Status = domain.AccountingPosted
	_ = json.Unmarshal(e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/accounting/requeue", nil).Body.Bytes(), &rq)
	if rq.Requeued != 0 || e.store.posting[0].Status != domain.AccountingPosted {
		t.Fatal("a POSTED request must never be requeued")
	}
}

func TestGetReceivedToDate_NetOfReversals_PerLine(t *testing.T) {
	e := newEnv(0)
	l1 := purchaseorder.Line{Quantity: 10, UnitPrice: 100}
	po := e.po.addOrder(tenantA, "ISSUED", 1000, l1)
	req := reqFor(po)
	req.POLineID = e.po.orders[po].Lines[0].LineID
	r := e.create(t, req)
	e.confirm(t, r, domain.ConfirmReceiptRequest{})
	e.as(confirmer, http.MethodPost, "/ap04/receipts/"+r.ReceiptID+"/reverse", domain.ReverseReceiptRequest{ReversedAmount: 300, Reason: "x"})

	rec := e.as(receiver, http.MethodGet, "/ap04/purchase-orders/"+po+"/received-to-date", nil)
	var resp struct {
		Net   float64               `json:"net_confirmed_amount"`
		Total float64               `json:"po_total_amount"`
		Lines []domain.LineReceived `json:"lines"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.Net != 700 || resp.Total != 1000 || len(resp.Lines) != 1 ||
		resp.Lines[0].ReceivedQuantity != 7 || resp.Lines[0].ReceivedAmount != 700 {
		t.Fatalf("expected net 700 / 7 units on the line against a 1000 PO, got %d %+v", rec.Code, resp)
	}
}

func TestListReceiptsForPO(t *testing.T) {
	e := newEnv(0)
	po := e.po.addOrder(tenantA, "ISSUED", 5000)
	e.create(t, reqFor(po))
	e.create(t, reqFor(po))
	rec := e.as(receiver, http.MethodGet, "/ap04/purchase-orders/"+po+"/receipts", nil)
	var resp struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.Count != 2 {
		t.Fatalf("expected 2 receipts, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(http.MethodGet, "/ap04/purchase-orders/"+po+"/receipts", nil, receiver, tenantB); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (empty) for another tenant, got %d", rec.Code)
	} else {
		var other struct {
			Count int `json:"count"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &other)
		if other.Count != 0 {
			t.Fatalf("another tenant must see none of tenant A's receipts, saw %d", other.Count)
		}
	}
}
