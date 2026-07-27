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

	"zoiko.io/invoice-approval-svc/internal/clients"
	"zoiko.io/invoice-approval-svc/internal/domain"
	"zoiko.io/invoice-approval-svc/internal/handler"
	"zoiko.io/invoice-approval-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	requests  map[string]*domain.InvoiceApprovalRequest
	decisions map[string][]domain.ApprovalDecision
}

func newStubStore() *stubStore {
	return &stubStore{
		requests:  make(map[string]*domain.InvoiceApprovalRequest),
		decisions: make(map[string][]domain.ApprovalDecision),
	}
}

func (s *stubStore) CreateRequest(_ context.Context, req *domain.InvoiceApprovalRequest) error {
	s.requests[req.ApprovalRequestID] = req
	return nil
}

func (s *stubStore) GetRequest(_ context.Context, id string) (*domain.InvoiceApprovalRequest, error) {
	req, ok := s.requests[id]
	if !ok {
		return nil, domain.ErrRequestNotFound
	}
	return req, nil
}

func (s *stubStore) ListRequests(_ context.Context, legalEntityID, invoiceID, status string) ([]domain.InvoiceApprovalRequest, error) {
	var out []domain.InvoiceApprovalRequest
	for _, req := range s.requests {
		if legalEntityID != "" && req.LegalEntityID != legalEntityID {
			continue
		}
		if invoiceID != "" && req.InvoiceID != invoiceID {
			continue
		}
		if status != "" && req.Status != status {
			continue
		}
		out = append(out, *req)
	}
	return out, nil
}

func (s *stubStore) AddDecisionAndUpdateStatus(_ context.Context, decision *domain.ApprovalDecision, newStatus string, newStep int) error {
	req, ok := s.requests[decision.ApprovalRequestID]
	if !ok {
		return domain.ErrRequestNotFound
	}
	s.decisions[decision.ApprovalRequestID] = append(s.decisions[decision.ApprovalRequestID], *decision)
	req.Status = newStatus
	req.CurrentStep = newStep
	req.UpdatedAt = time.Now().UTC()
	return nil
}

func (s *stubStore) GetDecisionsByRequest(_ context.Context, requestID string) ([]domain.ApprovalDecision, error) {
	decs, ok := s.decisions[requestID]
	if !ok {
		return []domain.ApprovalDecision{}, nil
	}
	return decs, nil
}

type stubPublisher struct {
	started, approved, rejected int
}

func (p *stubPublisher) PublishApprovalStarted(_ context.Context, _ string, _ domain.InvoiceApprovalRequest) {
	p.started++
}
func (p *stubPublisher) PublishApproved(_ context.Context, _ string, _ domain.InvoiceApprovalRequest) {
	p.approved++
}
func (p *stubPublisher) PublishRejected(_ context.Context, _ string, _ domain.InvoiceApprovalRequest, _ string) {
	p.rejected++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubClients struct{}

func (c *stubClients) FetchInvoice(_ context.Context, _, invoiceID string) (*clients.APInvoice, error) {
	return &clients.APInvoice{
		InvoiceID:     invoiceID,
		Amount:        1500.0,
		CurrencyCode:  "USD",
		Status:        "VALIDATED",
		LegalEntityID: "le-us",
	}, nil
}

func (c *stubClients) StartWorkflowInstance(_ context.Context, _, _, _ string) (string, error) {
	return "wf-instance-123", nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, cl *stubClients) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, cl, zap.NewNop())
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

// ── CreateRequest Tests ────────────────────────────────────────────────────────

func TestCreateRequest_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/invoice-approvals/", map[string]any{
		"invoice_id":      "inv-100",
		"legal_entity_id": "le-us",
		"invoice_amount":  1500.0,
		"currency_code":   "USD",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateRequest_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/invoice-approvals/", map[string]any{
		"invoice_id":      "inv-100",
		"legal_entity_id": "le-us",
		"invoice_amount":  1500.0,
		"currency_code":   "USD",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestCreateRequest_HappyPath(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubClients{})
	rr := doReq(r, http.MethodPost, "/v1/invoice-approvals/", map[string]any{
		"invoice_id":      "inv-100",
		"legal_entity_id": "le-us",
		"invoice_amount":  1500.0,
		"currency_code":   "USD",
		"total_steps":     2,
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var req domain.InvoiceApprovalRequest
	if err := json.NewDecoder(rr.Body).Decode(&req); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if req.Status != "PENDING" {
		t.Errorf("expected PENDING status got %q", req.Status)
	}
	if req.TotalSteps != 2 {
		t.Errorf("expected 2 total steps got %d", req.TotalSteps)
	}
	if pub.started != 1 {
		t.Errorf("expected 1 started event got %d", pub.started)
	}
}

// ── SubmitDecision Tests ───────────────────────────────────────────────────────

func TestSubmitDecision_MultiStepApprovalAndRejection(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubClients{})

	// 1. Create a 2-step approval request
	rrCreate := doReq(r, http.MethodPost, "/v1/invoice-approvals/", map[string]any{
		"invoice_id":      "inv-200",
		"legal_entity_id": "le-us",
		"invoice_amount":  5000.0,
		"currency_code":   "USD",
		"total_steps":     2,
	}, "principal-initiator")

	var created domain.InvoiceApprovalRequest
	_ = json.NewDecoder(rrCreate.Body).Decode(&created)

	// 2. Submit Step 1 Decision (APPROVED)
	rrStep1 := doReq(r, http.MethodPost, "/v1/invoice-approvals/"+created.ApprovalRequestID+"/decide", map[string]any{
		"decision":        "APPROVED",
		"decision_reason": "Step 1 manager signoff",
	}, "manager-1")

	if rrStep1.Code != http.StatusOK {
		t.Fatalf("step 1 expected 200 got %d: %s", rrStep1.Code, rrStep1.Body.String())
	}
	var step1Resp domain.InvoiceApprovalRequest
	_ = json.NewDecoder(rrStep1.Body).Decode(&step1Resp)

	if step1Resp.CurrentStep != 2 {
		t.Errorf("expected current_step to advance to 2, got %d", step1Resp.CurrentStep)
	}
	if step1Resp.Status != "PENDING" {
		t.Errorf("expected status to remain PENDING after step 1, got %q", step1Resp.Status)
	}

	// 3. Submit Step 2 Decision (APPROVED) -> final state
	rrStep2 := doReq(r, http.MethodPost, "/v1/invoice-approvals/"+created.ApprovalRequestID+"/decide", map[string]any{
		"decision":        "APPROVED",
		"decision_reason": "Step 2 VP signoff",
	}, "vp-1")

	if rrStep2.Code != http.StatusOK {
		t.Fatalf("step 2 expected 200 got %d: %s", rrStep2.Code, rrStep2.Body.String())
	}
	var step2Resp domain.InvoiceApprovalRequest
	_ = json.NewDecoder(rrStep2.Body).Decode(&step2Resp)

	if step2Resp.Status != "APPROVED" {
		t.Errorf("expected final status to be APPROVED, got %q", step2Resp.Status)
	}
	if pub.approved != 1 {
		t.Errorf("expected 1 approved event got %d", pub.approved)
	}

	// 4. Try submitting decision on finalized request -> 409 Conflict
	rrConflict := doReq(r, http.MethodPost, "/v1/invoice-approvals/"+created.ApprovalRequestID+"/decide", map[string]any{
		"decision": "APPROVED",
	}, "vp-1")
	if rrConflict.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d", rrConflict.Code)
	}
}