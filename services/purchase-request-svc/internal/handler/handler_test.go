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

	"zoiko.io/purchase-request-svc/internal/domain"
	"zoiko.io/purchase-request-svc/internal/handler"
	svcmiddleware "zoiko.io/purchase-request-svc/internal/middleware"
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
	requests      map[string]*domain.PurchaseRequest
	byCorrelation map[string]string

	createErr     error
	getErr        error
	listErr       error
	transitionErr error
}

func newStubStore() *stubStore {
	return &stubStore{requests: map[string]*domain.PurchaseRequest{}, byCorrelation: map[string]string{}}
}

func (s *stubStore) CreateRequest(_ context.Context, r *domain.PurchaseRequest) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	key := r.TenantID + "|" + r.CorrelationID
	if r.CorrelationID != "" {
		if existingID, ok := s.byCorrelation[key]; ok {
			*r = *s.requests[existingID]
			return false, nil
		}
		s.byCorrelation[key] = r.RequestID
	}
	s.requests[r.RequestID] = r
	return true, nil
}

func (s *stubStore) GetRequest(_ context.Context, requestID string) (*domain.PurchaseRequest, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	r, ok := s.requests[requestID]
	if !ok {
		return nil, nil
	}
	return r, nil
}

func (s *stubStore) ListRequests(_ context.Context, _ domain.ListRequestsFilter) ([]domain.PurchaseRequest, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.PurchaseRequest
	for _, r := range s.requests {
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) TransitionRequest(_ context.Context, _, requestID string, toStatus domain.RequestStatus, _ string, _ time.Time, reason *string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	r, ok := s.requests[requestID]
	if !ok || r.Status != domain.RequestStatusPending {
		return domain.ErrInvalidTransition
	}
	r.Status = toStatus
	r.RejectionReason = reason
	return nil
}

type stubPublisher struct {
	created, approved, rejected int
}

func (p *stubPublisher) PublishRequestCreated(_ context.Context, _ domain.PurchaseRequest)  { p.created++ }
func (p *stubPublisher) PublishRequestApproved(_ context.Context, _ domain.PurchaseRequest) { p.approved++ }
func (p *stubPublisher) PublishRequestRejected(_ context.Context, _ domain.PurchaseRequest) { p.rejected++ }

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// newRouter mounts TenantContext, which the real server mounts in
// cmd/server/main.go. It used to be omitted, so every handler under test saw an
// empty tenant scope and fell back to the query parameter or the body — the very
// behaviour these tests are supposed to be checking. A handler harness must
// mount the middleware the handler depends on.
func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, a, zap.NewNop())
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

// ── CreateRequest ────────────────────────────────────────────────────────────

func validCreateReq() domain.CreateRequestRequest {
	return domain.CreateRequestRequest{
		TenantID:      tenantA,
		LegalEntityID: entityA,
		Description:   "50 laptops",
		Amount:        50000,
		CurrencyCode:  "USD",
		CorrelationID: "corr-1",
	}
}

func TestCreateRequest_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateRequest_MissingCorrelationID_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.CorrelationID = ""
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no correlation_id, got %d", rec.Code)
	}
}

func TestCreateRequest_RetriedCorrelationID_ReturnsOriginalNotDuplicate(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{})
	req := validCreateReq()

	first := doRequest(r, http.MethodPost, "/v1/purchase-requests/", req, "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	var firstPR domain.PurchaseRequest
	_ = json.NewDecoder(first.Body).Decode(&firstPR)

	retry := doRequest(r, http.MethodPost, "/v1/purchase-requests/", req, "principal-1")
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call with the same correlation_id, got %d: %s", retry.Code, retry.Body.String())
	}
	var retryPR domain.PurchaseRequest
	_ = json.NewDecoder(retry.Body).Decode(&retryPR)
	if retryPR.RequestID != firstPR.RequestID {
		t.Fatalf("retried call resolved to a different request_id (%s) than the original (%s)", retryPR.RequestID, firstPR.RequestID)
	}
	if pub.created != 1 {
		t.Fatalf("expected exactly 1 PublishRequestCreated call, got %d — replay must not re-publish", pub.created)
	}
}

func TestCreateRequest_MissingPrincipalHeader_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/", validCreateReq(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d", rec.Code)
	}
}

func TestCreateRequest_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authorization-svc denies, got %d", rec.Code)
	}
}

func TestCreateRequest_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationServiceUnavailable})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable (fail closed), got %d", rec.Code)
	}
}

func TestCreateRequest_ZeroAmount_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.Amount = 0
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a zero-amount request, got %d", rec.Code)
	}
}

// ── ApproveRequest / RejectRequest (the fork) ─────────────────────────────────

func TestApproveRequest_FromPending_Succeeds(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.RequestStatusPending}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/approve", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.requests["r1"].Status != domain.RequestStatusApproved {
		t.Fatalf("expected status APPROVED, got %s", s.requests["r1"].Status)
	}
	if pub.approved != 1 {
		t.Fatalf("expected purchase.request.approved to be published once, got %d", pub.approved)
	}
	// The response must echo who decided and when — a 200 that says APPROVED
	// without the approver would be a record-shaped lie about what was written.
	var got domain.PurchaseRequest
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.ApprovedByPrincipalID == nil || *got.ApprovedByPrincipalID != "principal-1" {
		t.Fatalf("expected approved_by_principal_id principal-1 in response, got %v", got.ApprovedByPrincipalID)
	}
	if got.ApprovedAt == nil {
		t.Fatalf("expected approved_at in response")
	}
}

func TestApproveRequest_AlreadyApproved_Rejected(t *testing.T) {
	// Both fork branches are terminal — approving an already-APPROVED
	// request must be rejected, not silently re-approved.
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.RequestStatusApproved}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/approve", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 approving an already-APPROVED request, got %d", rec.Code)
	}
}

func TestRejectRequest_RequiresReason(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.RequestStatusPending}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/reject", domain.RejectRequestRequest{}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a reject with no reason, got %d", rec.Code)
	}
}

func TestRejectRequest_FromPending_Succeeds(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.RequestStatusPending}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/reject",
		domain.RejectRequestRequest{Reason: "over budget"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.requests["r1"].Status != domain.RequestStatusRejected {
		t.Fatalf("expected status REJECTED, got %s", s.requests["r1"].Status)
	}
	if pub.rejected != 1 {
		t.Fatalf("expected purchase.request.rejected to be published once, got %d", pub.rejected)
	}
	var got domain.PurchaseRequest
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.RejectedByPrincipalID == nil || *got.RejectedByPrincipalID != "principal-1" {
		t.Fatalf("expected rejected_by_principal_id principal-1 in response, got %v", got.RejectedByPrincipalID)
	}
	if got.RejectedAt == nil || got.RejectionReason == nil || *got.RejectionReason != "over budget" {
		t.Fatalf("expected rejected_at and reason in response, got at=%v reason=%v", got.RejectedAt, got.RejectionReason)
	}
}

// TestApproveRequest_SelfApproval_Rejected enforces the platform's
// Segregation of Duties doctrine (docs/original_doc/zoiko_suite_doc1.txt
// §12.3): a purchase request's creator may not also be the one who approves
// it.
func TestApproveRequest_SelfApproval_Rejected(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: tenantA, LegalEntityID: entityA, RequestedByPrincipalID: "principal-1", Status: domain.RequestStatusPending}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/approve", nil, "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for self-approval, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.requests["r1"].Status != domain.RequestStatusPending {
		t.Fatalf("expected status to remain PENDING after rejected self-approval, got %s", s.requests["r1"].Status)
	}
}

// TestRejectRequest_SelfApproval_Rejected mirrors the approve-side check:
// the requester may not reject their own request either — SoD applies to
// both decision outcomes.
func TestRejectRequest_SelfApproval_Rejected(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: tenantA, LegalEntityID: entityA, RequestedByPrincipalID: "principal-1", Status: domain.RequestStatusPending}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/reject",
		domain.RejectRequestRequest{Reason: "trying to self-reject"}, "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for self-rejection, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.requests["r1"].Status != domain.RequestStatusPending {
		t.Fatalf("expected status to remain PENDING after rejected self-rejection, got %s", s.requests["r1"].Status)
	}
}

func TestRejectRequest_AlreadyRejected_Rejected(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: tenantA, LegalEntityID: entityA, Status: domain.RequestStatusRejected}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/reject",
		domain.RejectRequestRequest{Reason: "trying again"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 rejecting an already-REJECTED request, got %d", rec.Code)
	}
}

// ── GetRequest / ListRequests ────────────────────────────────────────────────

func TestGetRequest_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/purchase-requests/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestListRequests_NoTenantScope_Refused replaces a test that asserted a 400
// when ?tenant_id= was absent — which documented the vulnerability as correct,
// since supplying the parameter was exactly how a caller read another tenant's
// register. The scope now comes from the header, so its absence is the failure.
func TestListRequests_NoTenantScope_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodGet, "/v1/purchase-requests/", nil, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── tenant scope ─────────────────────────────────────────────────────────────

// TestListRequests_ForeignTenantQueryParam_Refused is the regression test for
// the headline defect: ?tenant_id= was handed straight to the store, which both
// filtered on it and set app.tenant_id from it, so the tenant the caller named
// satisfied the RLS policy on the way past.
func TestListRequests_ForeignTenantQueryParam_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodGet, "/v1/purchase-requests/?tenant_id="+tenantB, nil, "", tenantA)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 listing another tenant's register, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListRequests_OwnTenantQueryParam_Allowed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodGet, "/v1/purchase-requests/?tenant_id="+tenantA, nil, "", tenantA)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when the query param agrees with the verified scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListRequests_UnknownStatusFilter_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/purchase-requests/?status=PENDNIG", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unrecognised status filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A malformed legal_entity_id is compared as text against a cast column, so it
// matched nothing and read as "this entity has raised no requests".
func TestListRequests_MalformedLegalEntityFilter_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/purchase-requests/?legal_entity_id=not-a-uuid", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed legal_entity_id filter, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateRequest_ForeignTenantBody_Refused is the write half of the same
// defect: tenant_id in the body was the only source of the stored tenant.
func TestCreateRequest_ForeignTenantBody_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.TenantID = tenantB
	rec := doRequestAs(r, http.MethodPost, "/v1/purchase-requests/", req, "principal-1", tenantA)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 creating into another tenant, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.requests) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(s.requests))
	}
}

// A body that omits tenant_id is fine — the tenant is the verified scope, and
// that is the tenant the row must be filed under.
func TestCreateRequest_NoTenantInBody_UsesVerifiedScope(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.TenantID = ""
	rec := doRequestAs(r, http.MethodPost, "/v1/purchase-requests/", req, "principal-1", tenantA)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.PurchaseRequest
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.TenantID != tenantA {
		t.Fatalf("expected the request filed under the verified tenant %s, got %s", tenantA, got.TenantID)
	}
}

func TestCreateRequest_NoTenantScope_Refused(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodPost, "/v1/purchase-requests/", validCreateReq(), "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.requests) != 0 {
		t.Fatalf("expected nothing written, got %d rows", len(s.requests))
	}
}

func TestCreateRequest_MalformedLegalEntityID_Refused(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.LegalEntityID = "e1"
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID legal_entity_id, got %d: %s", rec.Code, rec.Body.String())
	}
}
