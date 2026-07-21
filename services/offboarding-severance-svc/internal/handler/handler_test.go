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

	"zoiko.io/offboarding-severance-svc/internal/domain"
	"zoiko.io/offboarding-severance-svc/internal/employee"
	"zoiko.io/offboarding-severance-svc/internal/handler"
	"zoiko.io/offboarding-severance-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	requests   map[string]*domain.TerminationRequest
	checklists map[string]*domain.OffboardingChecklist
}

func newStubStore() *stubStore {
	return &stubStore{
		requests:   make(map[string]*domain.TerminationRequest),
		checklists: make(map[string]*domain.OffboardingChecklist),
	}
}

func (s *stubStore) CreateTerminationRequest(_ context.Context, req *domain.TerminationRequest) error {
	req.TerminationID = "term-1"
	s.requests[req.TerminationID] = req
	return nil
}

func (s *stubStore) GetTerminationRequest(_ context.Context, id string) (*domain.TerminationRequest, error) {
	req, ok := s.requests[id]
	if !ok {
		return nil, domain.ErrTerminationNotFound
	}
	return req, nil
}

func (s *stubStore) ListTerminationRequests(_ context.Context, legalEntityID string) ([]domain.TerminationRequest, error) {
	var out []domain.TerminationRequest
	for _, r := range s.requests {
		if legalEntityID != "" && r.LegalEntityID != legalEntityID {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (s *stubStore) ApproveTerminationRequest(_ context.Context, id string, approvedBy string) (*domain.TerminationRequest, error) {
	req, ok := s.requests[id]
	if !ok {
		return nil, domain.ErrTerminationNotFound
	}
	if req.Status != domain.TerminationStatusInitiated {
		return nil, domain.ErrAlreadyApproved
	}
	req.Status = domain.TerminationStatusApproved
	req.ApprovedBy = &approvedBy
	return req, nil
}

func (s *stubStore) FinalizeEmployeeTermination(_ context.Context, id string) (*domain.TerminationRequest, error) {
	req, ok := s.requests[id]
	if !ok {
		return nil, domain.ErrTerminationNotFound
	}
	req.Status = domain.TerminationStatusTerminated
	effTo := "2024-12-31"
	req.EffectiveTo = &effTo
	return req, nil
}

func (s *stubStore) CreateOffboardingChecklist(_ context.Context, chk *domain.OffboardingChecklist) error {
	chk.ChecklistID = "chk-1"
	s.checklists[chk.EmployeeID] = chk
	return nil
}

func (s *stubStore) GetOffboardingChecklist(_ context.Context, employeeID string) (*domain.OffboardingChecklist, error) {
	chk, ok := s.checklists[employeeID]
	if !ok {
		return nil, domain.ErrChecklistNotFound
	}
	return chk, nil
}

func (s *stubStore) UpdateChecklistItemStatus(_ context.Context, itemID string, status domain.ChecklistItemStatus, completedBy string) error {
	for _, chk := range s.checklists {
		for i := range chk.Items {
			if chk.Items[i].ItemID == itemID {
				chk.Items[i].Status = status
				chk.Items[i].CompletedBy = &completedBy
				return nil
			}
		}
	}
	return nil
}

type stubPublisher struct {
	initiated, approved, terminated, completed int
}

func (p *stubPublisher) PublishTerminationInitiated(_ context.Context, _ string, _ domain.TerminationRequest) {
	p.initiated++
}
func (p *stubPublisher) PublishTerminationApproved(_ context.Context, _ string, _ domain.TerminationRequest) {
	p.approved++
}
func (p *stubPublisher) PublishEmployeeTerminated(_ context.Context, _ string, _ domain.TerminationRequest) {
	p.terminated++
}
func (p *stubPublisher) PublishOffboardingCompleted(_ context.Context, _ string, _ domain.OffboardingChecklist) {
	p.completed++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubEmployeeValidator struct{ err error }

func (v *stubEmployeeValidator) ValidateEmployee(_ context.Context, _, legalEntityID, empID string) (*employee.Employee, error) {
	if v.err != nil {
		return nil, v.err
	}
	return &employee.Employee{EmployeeID: empID, LegalEntityID: legalEntityID, Status: "ACTIVE"}, nil
}

func (v *stubEmployeeValidator) TerminateEmployee(_ context.Context, _, _ string) error {
	return nil
}

type stubJurisdiction struct{}

func (j *stubJurisdiction) ValidateNoticePeriod(_ context.Context, _ string, requestedDays int) (int, error) {
	if requestedDays < 30 {
		return 30, nil
	}
	return requestedDays, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, empValidator *stubEmployeeValidator, jRules *stubJurisdiction) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, empValidator, jRules, zap.NewNop())
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

// ── Tests ──────────────────────────────────────────────────────────────────────

func TestInitiateTermination_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{}, &stubJurisdiction{})
	rr := doReq(r, http.MethodPost, "/v1/terminations", map[string]any{
		"legal_entity_id":  "le-us",
		"employee_id":      "emp-101",
		"termination_type": "RESIGNATION",
		"reason_code":      "PERSONAL",
		"last_working_day": "2024-12-31",
		"effective_from":   "2024-12-31",
	}, "")

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestOffboardingSeveranceLifecycle(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{}, &stubJurisdiction{})

	// 1. Initiate Termination
	rrInit := doReq(r, http.MethodPost, "/v1/terminations", map[string]any{
		"legal_entity_id":    "le-us",
		"employee_id":        "emp-101",
		"termination_type":   "INVOLUNTARY",
		"reason_code":        "REDUNDANCY",
		"notice_period_days": 14, // Should be adjusted to 30 by jurisdiction rule
		"last_working_day":   "2024-12-31",
		"effective_from":     "2024-12-31",
		"severance_amount":   15000.00,
		"currency":           "USD",
	}, "hr-admin")

	if rrInit.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrInit.Code, rrInit.Body.String())
	}

	var req domain.TerminationRequest
	_ = json.NewDecoder(rrInit.Body).Decode(&req)

	if req.NoticePeriodDays != 30 {
		t.Errorf("expected notice_period_days to be 30 from jurisdiction rule, got %d", req.NoticePeriodDays)
	}
	if pub.initiated != 1 {
		t.Errorf("expected 1 initiated event got %d", pub.initiated)
	}

	// 2. Approve Termination
	rrApprove := doReq(r, http.MethodPost, "/v1/terminations/"+req.TerminationID+"/approve", nil, "hr-admin")
	if rrApprove.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrApprove.Code, rrApprove.Body.String())
	}
	if pub.approved != 1 {
		t.Errorf("expected 1 approved event got %d", pub.approved)
	}

	// 3. Create Checklist
	rrChk := doReq(r, http.MethodPost, "/v1/offboarding/checklists", map[string]any{
		"legal_entity_id": "le-us",
		"employee_id":     "emp-101",
		"termination_id":  req.TerminationID,
	}, "hr-admin")
	if rrChk.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrChk.Code, rrChk.Body.String())
	}
	var chk domain.OffboardingChecklist
	_ = json.NewDecoder(rrChk.Body).Decode(&chk)
	if len(chk.Items) != 4 {
		t.Errorf("expected 4 default checklist items got %d", len(chk.Items))
	}

	// 4. Finalize Termination
	rrFinal := doReq(r, http.MethodPost, "/v1/terminations/"+req.TerminationID+"/finalize", nil, "hr-admin")
	if rrFinal.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrFinal.Code, rrFinal.Body.String())
	}
	if pub.terminated != 1 {
		t.Errorf("expected 1 terminated event got %d", pub.terminated)
	}
}
