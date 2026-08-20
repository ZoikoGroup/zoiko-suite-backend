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

	"zoiko.io/procurement-workflow-svc/internal/clients"
	"zoiko.io/procurement-workflow-svc/internal/domain"
	"zoiko.io/procurement-workflow-svc/internal/handler"
	"zoiko.io/procurement-workflow-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	byID          map[string]*domain.ProcurementCase
	byCorrelation map[string]*domain.ProcurementCase
}

func newStubStore() *stubStore {
	return &stubStore{byID: make(map[string]*domain.ProcurementCase), byCorrelation: make(map[string]*domain.ProcurementCase)}
}

func (s *stubStore) CreateCase(_ context.Context, c *domain.ProcurementCase) (bool, error) {
	if existing, ok := s.byCorrelation[c.CorrelationID]; ok {
		*c = *existing
		return false, nil
	}
	cp := *c
	s.byID[c.CaseID] = &cp
	s.byCorrelation[c.CorrelationID] = &cp
	return true, nil
}

func (s *stubStore) GetCase(_ context.Context, caseID string) (*domain.ProcurementCase, error) {
	c, ok := s.byID[caseID]
	if !ok {
		return nil, domain.ErrCaseNotFound
	}
	cp := *c
	return &cp, nil
}

func (s *stubStore) ListCases(_ context.Context, legalEntityID, status string) ([]domain.ProcurementCase, error) {
	var out []domain.ProcurementCase
	for _, c := range s.byID {
		if legalEntityID != "" && c.LegalEntityID != legalEntityID {
			continue
		}
		if status != "" && string(c.Status) != status {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) UpdateApproved(_ context.Context, caseID, principalID string) (*domain.ProcurementCase, error) {
	c, ok := s.byID[caseID]
	if !ok {
		return nil, domain.ErrCaseNotFound
	}
	if c.Status != domain.CaseStatusApprovalPending {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	c.Status = domain.CaseStatusApproved
	c.ApprovedByPrincipalID = &principalID
	c.ApprovedAt = &now
	c.UpdatedAt = now
	cp := *c
	return &cp, nil
}

func (s *stubStore) UpdateRejected(_ context.Context, caseID, principalID, reason string) (*domain.ProcurementCase, error) {
	c, ok := s.byID[caseID]
	if !ok {
		return nil, domain.ErrCaseNotFound
	}
	if c.Status != domain.CaseStatusApprovalPending {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	c.Status = domain.CaseStatusRejected
	c.RejectedByPrincipalID = &principalID
	c.RejectionReason = &reason
	c.RejectedAt = &now
	c.UpdatedAt = now
	cp := *c
	return &cp, nil
}

func (s *stubStore) UpdateOrderIssued(_ context.Context, caseID, purchaseOrderID string) (*domain.ProcurementCase, error) {
	c, ok := s.byID[caseID]
	if !ok {
		return nil, domain.ErrCaseNotFound
	}
	if c.Status != domain.CaseStatusApproved {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	c.Status = domain.CaseStatusCompleted
	c.PurchaseOrderID = &purchaseOrderID
	c.CompletedAt = &now
	c.UpdatedAt = now
	cp := *c
	return &cp, nil
}

type stubPublisher struct {
	requested, approvalStarted, completed int
}

func (p *stubPublisher) PublishRequested(_ context.Context, _ domain.ProcurementCase) { p.requested++ }
func (p *stubPublisher) PublishApprovalStarted(_ context.Context, _ domain.ProcurementCase) {
	p.approvalStarted++
}
func (p *stubPublisher) PublishCompleted(_ context.Context, _ string, _ domain.ProcurementCase) {
	p.completed++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubSpendChecker struct {
	decision string
	basis    string
	err      error
}

func (s *stubSpendChecker) SubmitCheck(_ context.Context, _, _, _, _, _, _, _ string, _ float64) (*clients.SpendCheckResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &clients.SpendCheckResult{DecisionOutcome: s.decision, DecisionBasis: s.basis}, nil
}

type stubOrderIssuer struct {
	orderID string
	err     error
}

func (o *stubOrderIssuer) IssueOrder(_ context.Context, _, _, _, _ string, _ *string, _ float64, _ string) (*clients.IssuedOrder, error) {
	if o.err != nil {
		return nil, o.err
	}
	return &clients.IssuedOrder{PurchaseOrderID: o.orderID, PONumber: "PO-TEST-0001"}, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, spend *stubSpendChecker, order *stubOrderIssuer) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, spend, order, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func createCaseBody(correlationID string) map[string]any {
	return map[string]any{
		"legal_entity_id": "le-us",
		"description":     "New laptops for engineering",
		"category":        "HARDWARE",
		"amount":          5000.0,
		"currency_code":   "USD",
		"correlation_id":  correlationID,
	}
}

// ── CreateCase tests ───────────────────────────────────────────────────────────

func TestCreateCase_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{})
	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/", createCaseBody(uuid.NewString()), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateCase_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{})
	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/", createCaseBody(uuid.NewString()), "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestCreateCase_SpendControlsUnavailable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{err: domain.ErrSpendControlsUnavailable}, &stubOrderIssuer{})
	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/", createCaseBody(uuid.NewString()), "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateCase_SpendAllowed_EntersApproval(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED", basis: "within_threshold"}, &stubOrderIssuer{})
	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/", createCaseBody(uuid.NewString()), "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var c domain.ProcurementCase
	if err := json.NewDecoder(rr.Body).Decode(&c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Status != domain.CaseStatusApprovalPending {
		t.Errorf("expected APPROVAL_PENDING got %q", c.Status)
	}
	if pub.requested != 1 || pub.approvalStarted != 1 {
		t.Errorf("expected 1 requested + 1 approval.started event, got requested=%d approvalStarted=%d", pub.requested, pub.approvalStarted)
	}
}

func TestCreateCase_SpendBlocked_TerminalNoApproval(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubSpendChecker{decision: "BLOCKED", basis: "threshold_exceeded"}, &stubOrderIssuer{})
	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/", createCaseBody(uuid.NewString()), "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var c domain.ProcurementCase
	_ = json.NewDecoder(rr.Body).Decode(&c)
	if c.Status != domain.CaseStatusSpendBlocked {
		t.Errorf("expected SPEND_BLOCKED got %q", c.Status)
	}
	if pub.requested != 1 || pub.approvalStarted != 0 {
		t.Errorf("expected 1 requested + 0 approval.started event, got requested=%d approvalStarted=%d", pub.requested, pub.approvalStarted)
	}
}

func TestCreateCase_IdempotentReplay(t *testing.T) {
	correlationID := uuid.NewString()
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{})

	rr1 := doReq(r, http.MethodPost, "/v1/procurement-cases/", createCaseBody(correlationID), "principal-1")
	var c1 domain.ProcurementCase
	_ = json.NewDecoder(rr1.Body).Decode(&c1)

	rr2 := doReq(r, http.MethodPost, "/v1/procurement-cases/", createCaseBody(correlationID), "principal-1")
	var c2 domain.ProcurementCase
	_ = json.NewDecoder(rr2.Body).Decode(&c2)

	if c2.CaseID != c1.CaseID {
		t.Fatalf("retried create resolved to a different case_id (%s) than the original (%s)", c2.CaseID, c1.CaseID)
	}
}

// ── approve / reject / issue-order tests ──────────────────────────────────────

func createApprovalPendingCase(t *testing.T, r chi.Router) domain.ProcurementCase {
	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/", createCaseBody(uuid.NewString()), "principal-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("case setup failed: %d %s", rr.Code, rr.Body.String())
	}
	var c domain.ProcurementCase
	_ = json.NewDecoder(rr.Body).Decode(&c)
	return c
}

func TestApproveCase_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{})
	c := createApprovalPendingCase(t, r)

	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/approve", nil, "approver-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.ProcurementCase
	_ = json.NewDecoder(rr.Body).Decode(&updated)
	if updated.Status != domain.CaseStatusApproved {
		t.Errorf("expected APPROVED got %q", updated.Status)
	}
}

// TestApproveCase_SelfApproval_Rejected enforces the platform's Segregation
// of Duties doctrine (docs/original_doc/zoiko_suite_doc1.txt §12.3): the
// principal who requested a procurement case may not also be the one who
// approves it.
func TestApproveCase_SelfApproval_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{})
	c := createApprovalPendingCase(t, r) // requested by "principal-1"

	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/approve", nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for self-approval got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRejectCase_SelfApproval_Rejected mirrors the approve-side check for
// rejection — SoD applies to both decision outcomes.
func TestRejectCase_SelfApproval_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{})
	c := createApprovalPendingCase(t, r) // requested by "principal-1"

	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/reject", map[string]any{"reason": "self-reject attempt"}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for self-rejection got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRejectCase_RequiresReason(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{})
	c := createApprovalPendingCase(t, r)

	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/reject", map[string]any{"reason": ""}, "approver-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestIssueOrder_RequiresApprovedStatus(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{orderID: "po-1"})
	c := createApprovalPendingCase(t, r) // still APPROVAL_PENDING, not APPROVED

	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/issue-order", nil, "principal-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestIssueOrder_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{orderID: "po-1"})
	c := createApprovalPendingCase(t, r)

	approveRR := doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/approve", nil, "approver-1")
	if approveRR.Code != http.StatusOK {
		t.Fatalf("approve setup failed: %d", approveRR.Code)
	}

	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/issue-order", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.ProcurementCase
	_ = json.NewDecoder(rr.Body).Decode(&updated)
	if updated.Status != domain.CaseStatusCompleted {
		t.Errorf("expected COMPLETED got %q", updated.Status)
	}
	if updated.PurchaseOrderID == nil || *updated.PurchaseOrderID != "po-1" {
		t.Errorf("expected purchase_order_id to be recorded, got %v", updated.PurchaseOrderID)
	}
	if pub.completed != 1 {
		t.Errorf("expected 1 procurement.completed event, got %d", pub.completed)
	}
}

func TestIssueOrder_PurchaseOrderServiceUnavailable(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubSpendChecker{decision: "ALLOWED"}, &stubOrderIssuer{err: domain.ErrPurchaseOrderServiceUnavailable})
	c := createApprovalPendingCase(t, r)
	_ = doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/approve", nil, "approver-1")

	rr := doReq(r, http.MethodPost, "/v1/procurement-cases/"+c.CaseID+"/issue-order", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 got %d: %s", rr.Code, rr.Body.String())
	}
}
