package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/domain"
	"zoiko.io/purchase-order-svc/internal/handler"
	svcmiddleware "zoiko.io/purchase-order-svc/internal/middleware"
	"zoiko.io/purchase-order-svc/internal/purchaserequest"
)

// tenant_id and legal_entity_id are uuid columns, so the fixtures are UUIDs —
// "t1"/"e1" would be refused by the handler's own identifier checks now that a
// malformed id is a 400 rather than a 503 from the driver.
const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
	entityA = "33333333-3333-3333-3333-333333333333"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	orders     map[string]*domain.PurchaseOrder
	amendments map[string][]domain.PurchaseOrderAmendment

	createErr     error
	getErr        error
	listErr       error
	amendErr      error
	closeErr      error
	amendmentsErr error
}

func newStubStore() *stubStore {
	return &stubStore{
		orders:     map[string]*domain.PurchaseOrder{},
		amendments: map[string][]domain.PurchaseOrderAmendment{},
	}
}

func (s *stubStore) ListAmendments(_ context.Context, orderID string) ([]domain.PurchaseOrderAmendment, error) {
	if s.amendmentsErr != nil {
		return nil, s.amendmentsErr
	}
	return s.amendments[orderID], nil
}

func (s *stubStore) CreateOrder(_ context.Context, o *domain.PurchaseOrder) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	for _, existing := range s.orders {
		if existing.TenantID == o.TenantID && existing.CorrelationID == o.CorrelationID {
			*o = *existing
			return false, nil
		}
	}
	o.Status = domain.OrderStatusIssued
	o.PONumber = fmt.Sprintf("PO-%06d", len(s.orders)+1)
	o.Version = 1
	s.orders[o.PurchaseOrderID] = o
	return true, nil
}

func (s *stubStore) GetOrder(_ context.Context, orderID string) (*domain.PurchaseOrder, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	o, ok := s.orders[orderID]
	if !ok {
		return nil, nil
	}
	return o, nil
}

func (s *stubStore) ListOrders(_ context.Context, _ domain.ListOrdersFilter) ([]domain.PurchaseOrder, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.PurchaseOrder
	for _, o := range s.orders {
		out = append(out, *o)
	}
	return out, nil
}

func (s *stubStore) AmendOrder(_ context.Context, _, orderID string, newTotalAmount float64, reason string, actorPrincipalID string) (*domain.PurchaseOrder, error) {
	if s.amendErr != nil {
		return nil, s.amendErr
	}
	o, ok := s.orders[orderID]
	if !ok || o.Status != domain.OrderStatusIssued {
		return nil, domain.ErrInvalidTransition
	}
	previous := o.TotalAmount
	fromVersion := o.Version
	o.TotalAmount = newTotalAmount
	o.Version++
	// Mirror the real store: an amend always appends to the ledger, which is
	// what ListAmendments reads back.
	s.amendments[orderID] = append(s.amendments[orderID], domain.PurchaseOrderAmendment{
		AmendmentID:          fmt.Sprintf("amend-%d", len(s.amendments[orderID])+1),
		PurchaseOrderID:      orderID,
		FromVersion:          fromVersion,
		ToVersion:            o.Version,
		PreviousTotalAmount:  previous,
		NewTotalAmount:       newTotalAmount,
		Reason:               reason,
		AmendedByPrincipalID: actorPrincipalID,
	})
	return o, nil
}

func (s *stubStore) CloseOrder(_ context.Context, _, orderID, actorPrincipalID string) (*domain.PurchaseOrder, error) {
	if s.closeErr != nil {
		return nil, s.closeErr
	}
	o, ok := s.orders[orderID]
	if !ok || o.Status != domain.OrderStatusIssued {
		return nil, domain.ErrInvalidTransition
	}
	o.Status = domain.OrderStatusClosed
	o.ClosedByPrincipalID = &actorPrincipalID
	return o, nil
}

type stubPublisher struct {
	issued, amended, closed int
}

func (p *stubPublisher) PublishOrderIssued(_ context.Context, _ domain.PurchaseOrder)  { p.issued++ }
func (p *stubPublisher) PublishOrderAmended(_ context.Context, _ domain.PurchaseOrder) { p.amended++ }
func (p *stubPublisher) PublishOrderClosed(_ context.Context, _ domain.PurchaseOrder)  { p.closed++ }

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubPRClient struct {
	summary *purchaserequest.Summary
	err     error

	// gotTenantID records the scope the upstream lookup was made in. The tenant
	// used to come from the issue body, so a body naming another tenant had its
	// referenced purchase request checked in THAT tenant too — the approval
	// gate and the order it guarded disagreed about whose data they were about.
	gotTenantID string
}

func (c *stubPRClient) GetApprovedRequest(_ context.Context, tenantID, _, _ string) (*purchaserequest.Summary, error) {
	c.gotTenantID = tenantID
	return c.summary, c.err
}

// newRouter mounts TenantContext, which the real server mounts in
// cmd/server/main.go. It used to be omitted, so every handler under test saw an
// empty tenant scope and fell back to the query parameter or the body — the very
// behaviour these tests are supposed to be checking. A handler harness must
// mount the middleware the handler depends on.
func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ, pr *stubPRClient) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, a, pr, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// doRequest sends a request in tenantA's scope, which is the ordinary case.
func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, principalID, tenantA)
}

// doRequestAs sends a request in an explicit tenant scope; tenantID "" omits the
// X-Tenant-Id header entirely, which is how a request with no verified scope is
// simulated.
func doRequestAs(r chi.Router, method, path string, body any, principalID, tenantID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ── IssueOrder ───────────────────────────────────────────────────────────────

func validIssueReq() domain.IssueOrderRequest {
	return domain.IssueOrderRequest{
		TenantID:      tenantA,
		LegalEntityID: entityA,
		TotalAmount:   50000,
		CurrencyCode:  "USD",
		CorrelationID: "corr-1",
	}
}

func TestIssueOrder_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIssueOrder_MissingPrincipalHeader_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", validIssueReq(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d", rec.Code)
	}
}

func TestIssueOrder_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authorization-svc denies, got %d", rec.Code)
	}
}

func TestIssueOrder_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationServiceUnavailable}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable (fail closed), got %d", rec.Code)
	}
}

func TestIssueOrder_ZeroAmount_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	req := validIssueReq()
	req.TotalAmount = 0
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a zero-amount order, got %d", rec.Code)
	}
}

func TestIssueOrder_WithPurchaseRequestID_VerifiesUpstream_Success(t *testing.T) {
	pr := &stubPRClient{summary: &purchaserequest.Summary{RequestID: "pr-1", TenantID: tenantA, LegalEntityID: entityA, Status: "APPROVED"}}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, pr)
	req := validIssueReq()
	reqID := "pr-1"
	req.PurchaseRequestID = &reqID
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", req, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIssueOrder_WithPurchaseRequestID_NotApproved_Rejected(t *testing.T) {
	pr := &stubPRClient{err: domain.ErrPurchaseRequestNotApproved}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, pr)
	req := validIssueReq()
	reqID := "pr-1"
	req.PurchaseRequestID = &reqID
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", req, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for a non-APPROVED purchase request, got %d", rec.Code)
	}
}

func TestIssueOrder_PurchaseRequestServiceUnavailable_FailsClosed(t *testing.T) {
	pr := &stubPRClient{err: domain.ErrPurchaseRequestServiceUnavailable}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, pr)
	req := validIssueReq()
	reqID := "pr-1"
	req.PurchaseRequestID = &reqID
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", req, "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when purchase-request-svc is unreachable (fail closed), got %d", rec.Code)
	}
}

func TestIssueOrder_IdempotentReplay_DoesNotRepublish(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubPRClient{})

	rec1 := doRequest(r, http.MethodPost, "/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec1.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first issue, got %d: %s", rec1.Code, rec1.Body.String())
	}
	rec2 := doRequest(r, http.MethodPost, "/v1/purchase-orders/", validIssueReq(), "principal-1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if pub.issued != 1 {
		t.Fatalf("expected purchase.order.issued to be published exactly once, got %d", pub.issued)
	}
}

// ── GetOrder / ListOrders ────────────────────────────────────────────────────

func TestGetOrder_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodGet, "/v1/purchase-orders/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestListOrders_NoTenantScope_Refused replaces a test that asserted a 400 when
// ?tenant_id= was absent — which documented the vulnerability as correct, since
// supplying the parameter was exactly how a caller read another tenant's
// register. The scope now comes from the header, so its absence is the failure.
func TestListOrders_NoTenantScope_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequestAs(r, http.MethodGet, "/v1/purchase-orders/", nil, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── tenant scope ─────────────────────────────────────────────────────────────

func TestListOrders_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequestAs(r, http.MethodGet, "/v1/purchase-orders/?tenant_id="+tenantB, nil, "", tenantA)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's register, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListOrders_OwnTenantQueryParam_Allowed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequestAs(r, http.MethodGet, "/v1/purchase-orders/?tenant_id="+tenantA, nil, "", tenantA)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when the query param agrees with the verified scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListOrders_UnknownStatusFilter_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodGet, "/v1/purchase-orders/?status=ISUSED", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unrecognised status filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListOrders_MalformedLegalEntityFilter_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodGet, "/v1/purchase-orders/?legal_entity_id=not-a-uuid", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed legal_entity_id filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIssueOrder_ForeignTenantBody_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	req := validIssueReq()
	req.TenantID = tenantB
	rec := doRequestAs(r, http.MethodPost, "/v1/purchase-orders/", req, "principal-1", tenantA)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 issuing into another tenant, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.orders) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(s.orders))
	}
}

func TestIssueOrder_NoTenantInBody_UsesVerifiedScope(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	req := validIssueReq()
	req.TenantID = ""
	rec := doRequestAs(r, http.MethodPost, "/v1/purchase-orders/", req, "principal-1", tenantA)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.PurchaseOrder
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.TenantID != tenantA {
		t.Fatalf("expected the order filed under the verified tenant %s, got %s", tenantA, got.TenantID)
	}
}

func TestIssueOrder_NoTenantScope_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequestAs(r, http.MethodPost, "/v1/purchase-orders/", validIssueReq(), "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.orders) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(s.orders))
	}
}

func TestIssueOrder_MalformedLegalEntityID_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	req := validIssueReq()
	req.LegalEntityID = "e1"
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID legal_entity_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The upstream approval gate must be consulted in the caller's VERIFIED tenant.
// It used to be consulted in whatever tenant the body named.
func TestIssueOrder_UpstreamLookupUsesVerifiedTenant(t *testing.T) {
	pr := &stubPRClient{summary: &purchaserequest.Summary{RequestID: "pr-1", TenantID: tenantA, LegalEntityID: entityA, Status: "APPROVED"}}
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, pr)
	req := validIssueReq()
	req.TenantID = ""
	reqID := "pr-1"
	req.PurchaseRequestID = &reqID
	rec := doRequestAs(r, http.MethodPost, "/v1/purchase-orders/", req, "principal-1", tenantA)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if pr.gotTenantID != tenantA {
		t.Fatalf("expected the purchase-request lookup scoped to the verified tenant %s, got %q", tenantA, pr.gotTenantID)
	}
}

// ── AmendOrder ───────────────────────────────────────────────────────────────

func TestAmendOrder_FromIssued_Succeeds(t *testing.T) {
	s := newStubStore()
	s.orders["po1"] = &domain.PurchaseOrder{PurchaseOrderID: "po1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.OrderStatusIssued, TotalAmount: 100, Version: 1}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/po1/amend",
		domain.AmendOrderRequest{NewTotalAmount: 200, Reason: "vendor price change"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.orders["po1"].TotalAmount != 200 || s.orders["po1"].Version != 2 {
		t.Fatalf("expected total_amount=200 version=2, got %v/%d", s.orders["po1"].TotalAmount, s.orders["po1"].Version)
	}
	if pub.amended != 1 {
		t.Fatalf("expected purchase.order.amended to be published once, got %d", pub.amended)
	}
}

func TestAmendOrder_RequiresReason(t *testing.T) {
	s := newStubStore()
	s.orders["po1"] = &domain.PurchaseOrder{PurchaseOrderID: "po1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.OrderStatusIssued}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/po1/amend",
		domain.AmendOrderRequest{NewTotalAmount: 200}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an amend with no reason, got %d", rec.Code)
	}
}

func TestAmendOrder_NotIssued_Rejected(t *testing.T) {
	s := newStubStore()
	s.orders["po1"] = &domain.PurchaseOrder{PurchaseOrderID: "po1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.OrderStatusClosed}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/po1/amend",
		domain.AmendOrderRequest{NewTotalAmount: 200, Reason: "too late"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 amending a CLOSED order, got %d", rec.Code)
	}
}

// ── CloseOrder ───────────────────────────────────────────────────────────────

func TestCloseOrder_FromIssued_Succeeds(t *testing.T) {
	s := newStubStore()
	s.orders["po1"] = &domain.PurchaseOrder{PurchaseOrderID: "po1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.OrderStatusIssued}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/po1/close", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.orders["po1"].Status != domain.OrderStatusClosed {
		t.Fatalf("expected status CLOSED, got %s", s.orders["po1"].Status)
	}
	if pub.closed != 1 {
		t.Fatalf("expected purchase.order.closed to be published once, got %d", pub.closed)
	}
}

func TestCloseOrder_AlreadyClosed_Rejected(t *testing.T) {
	s := newStubStore()
	s.orders["po1"] = &domain.PurchaseOrder{PurchaseOrderID: "po1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.OrderStatusClosed}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubPRClient{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-orders/po1/close", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 closing an already-CLOSED order, got %d", rec.Code)
	}
}
