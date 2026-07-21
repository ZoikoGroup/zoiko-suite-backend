package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/purchase-request-svc/internal/domain"
	"zoiko.io/purchase-request-svc/internal/handler"
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

func (s *stubStore) TransitionRequest(_ context.Context, _, requestID string, toStatus domain.RequestStatus, _ string, reason *string) error {
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

func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, a, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ── CreateRequest ────────────────────────────────────────────────────────────

func validCreateReq() domain.CreateRequestRequest {
	return domain.CreateRequestRequest{
		TenantID:      "t1",
		LegalEntityID: "e1",
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
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: "t1", LegalEntityID: "e1", Status: domain.RequestStatusPending}

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
}

func TestApproveRequest_AlreadyApproved_Rejected(t *testing.T) {
	// Both fork branches are terminal — approving an already-APPROVED
	// request must be rejected, not silently re-approved.
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: "t1", LegalEntityID: "e1", Status: domain.RequestStatusApproved}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/approve", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 approving an already-APPROVED request, got %d", rec.Code)
	}
}

func TestRejectRequest_RequiresReason(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: "t1", LegalEntityID: "e1", Status: domain.RequestStatusPending}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/purchase-requests/r1/reject", domain.RejectRequestRequest{}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a reject with no reason, got %d", rec.Code)
	}
}

func TestRejectRequest_FromPending_Succeeds(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: "t1", LegalEntityID: "e1", Status: domain.RequestStatusPending}

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
}

func TestRejectRequest_AlreadyRejected_Rejected(t *testing.T) {
	s := newStubStore()
	s.requests["r1"] = &domain.PurchaseRequest{RequestID: "r1", TenantID: "t1", LegalEntityID: "e1", Status: domain.RequestStatusRejected}

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

func TestListRequests_RequiresTenantID(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/purchase-requests/", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without tenant_id query param, got %d", rec.Code)
	}
}
