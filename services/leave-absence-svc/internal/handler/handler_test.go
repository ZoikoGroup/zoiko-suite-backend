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

	"zoiko.io/leave-absence-svc/internal/domain"
	"zoiko.io/leave-absence-svc/internal/employee"
	"zoiko.io/leave-absence-svc/internal/handler"
	"zoiko.io/leave-absence-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	types    map[string]*domain.LeaveType
	balances map[string]*domain.LeaveBalance
	requests map[string]*domain.LeaveRequest
	holidays map[string]*domain.Holiday
}

func newStubStore() *stubStore {
	return &stubStore{
		types:    make(map[string]*domain.LeaveType),
		balances: make(map[string]*domain.LeaveBalance),
		requests: make(map[string]*domain.LeaveRequest),
		holidays: make(map[string]*domain.Holiday),
	}
}

func (s *stubStore) CreateLeaveType(_ context.Context, lt *domain.LeaveType) error {
	// Mirrors idx_leave_types_tenant_entity_code.
	for _, existing := range s.types {
		if existing.LegalEntityID == lt.LegalEntityID && existing.Code == lt.Code {
			return domain.ErrLeaveTypeCodeExists
		}
	}
	s.types[lt.LeaveTypeID] = lt
	return nil
}

func (s *stubStore) ListLeaveTypes(_ context.Context, legalEntityID string) ([]domain.LeaveType, error) {
	var out []domain.LeaveType
	for _, lt := range s.types {
		if legalEntityID != "" && lt.LegalEntityID != legalEntityID {
			continue
		}
		out = append(out, *lt)
	}
	return out, nil
}

func (s *stubStore) GetLeaveType(_ context.Context, id string) (*domain.LeaveType, error) {
	lt, ok := s.types[id]
	if !ok {
		return nil, domain.ErrLeaveTypeNotFound
	}
	return lt, nil
}

func (s *stubStore) GetLeaveBalances(_ context.Context, employeeID string) ([]domain.LeaveBalance, error) {
	var out []domain.LeaveBalance
	for _, b := range s.balances {
		if b.EmployeeID == employeeID {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (s *stubStore) AccrueLeaveBalance(_ context.Context, employeeID, leaveTypeID string, hours float64) (*domain.LeaveBalance, error) {
	key := employeeID + ":" + leaveTypeID
	b, ok := s.balances[key]
	if !ok {
		b = &domain.LeaveBalance{
			BalanceID:      "bal-1",
			TenantID:       "tenant-abc",
			EmployeeID:     employeeID,
			LeaveTypeID:    leaveTypeID,
			AllocatedHours: 0,
		}
		s.balances[key] = b
	}
	b.AllocatedHours += hours
	b.AvailableHours = b.AllocatedHours - b.UsedHours - b.PendingHours
	return b, nil
}

func (s *stubStore) SubmitLeaveRequest(_ context.Context, req *domain.SubmitLeaveRequest) (*domain.LeaveRequest, error) {
	if req.CorrelationID != "" {
		for _, existing := range s.requests {
			if existing.CorrelationID == req.CorrelationID {
				return existing, nil
			}
		}
	}

	key := req.EmployeeID + ":" + req.LeaveTypeID
	b, ok := s.balances[key]
	if !ok || (b.AllocatedHours-b.UsedHours-b.PendingHours) < req.TotalHours {
		return nil, domain.ErrInsufficientBalance
	}

	b.PendingHours += req.TotalHours
	b.AvailableHours = b.AllocatedHours - b.UsedHours - b.PendingHours

	lr := &domain.LeaveRequest{
		RequestID:     fmt.Sprintf("req-%d", len(s.requests)+1),
		TenantID:      "tenant-abc",
		EmployeeID:    req.EmployeeID,
		LeaveTypeID:   req.LeaveTypeID,
		StartDate:     req.StartDate,
		EndDate:       req.EndDate,
		TotalHours:    req.TotalHours,
		Reason:        req.Reason,
		Status:        "SUBMITTED",
		CorrelationID: req.CorrelationID,
	}
	s.requests[lr.RequestID] = lr
	return lr, nil
}

func (s *stubStore) GetLeaveRequest(_ context.Context, id string) (*domain.LeaveRequest, error) {
	lr, ok := s.requests[id]
	if !ok {
		return nil, domain.ErrRequestNotFound
	}
	return lr, nil
}

func (s *stubStore) ListLeaveRequests(_ context.Context, employeeID, status string) ([]domain.LeaveRequest, error) {
	var out []domain.LeaveRequest
	for _, lr := range s.requests {
		if employeeID != "" && lr.EmployeeID != employeeID {
			continue
		}
		if status != "" && lr.Status != status {
			continue
		}
		out = append(out, *lr)
	}
	return out, nil
}

func (s *stubStore) ApproveLeaveRequest(_ context.Context, id, reviewerID, notes string) error {
	lr, ok := s.requests[id]
	if !ok || lr.Status != "SUBMITTED" {
		return domain.ErrInvalidStatusTransition
	}
	key := lr.EmployeeID + ":" + lr.LeaveTypeID
	b := s.balances[key]
	b.PendingHours -= lr.TotalHours
	b.UsedHours += lr.TotalHours
	b.AvailableHours = b.AllocatedHours - b.UsedHours - b.PendingHours

	lr.Status = "APPROVED"
	lr.ReviewerID = &reviewerID
	lr.ReviewerNotes = &notes
	return nil
}

func (s *stubStore) RejectLeaveRequest(_ context.Context, id, reviewerID, notes string) error {
	lr, ok := s.requests[id]
	if !ok || lr.Status != "SUBMITTED" {
		return domain.ErrInvalidStatusTransition
	}
	key := lr.EmployeeID + ":" + lr.LeaveTypeID
	b := s.balances[key]
	b.PendingHours -= lr.TotalHours
	b.AvailableHours = b.AllocatedHours - b.UsedHours - b.PendingHours

	lr.Status = "REJECTED"
	lr.ReviewerID = &reviewerID
	lr.ReviewerNotes = &notes
	return nil
}

type stubPublisher struct {
	requested, approved, rejected, balanceUpdated int
}

func (p *stubPublisher) PublishLeaveRequested(_ context.Context, _, _, _ string, _ domain.LeaveRequest) {
	p.requested++
}
func (p *stubPublisher) PublishLeaveApproved(_ context.Context, _, _ string, _ domain.LeaveRequest) {
	p.approved++
}
func (p *stubPublisher) PublishLeaveRejected(_ context.Context, _, _ string, _ domain.LeaveRequest) {
	p.rejected++
}
func (p *stubPublisher) PublishBalanceUpdated(_ context.Context, _, _, _ string, _ domain.LeaveBalance) {
	p.balanceUpdated++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubEmployeeValidator struct{ err error }

func (v *stubEmployeeValidator) ValidateEmployee(_ context.Context, _, _, empID string) (*employee.Employee, error) {
	if v.err != nil {
		return nil, v.err
	}
	return &employee.Employee{EmployeeID: empID, LegalEntityID: "le-us", Status: "ACTIVE"}, nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ, empValidator *stubEmployeeValidator) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, empValidator, zap.NewNop())
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

// ── CreateLeaveType Tests ──────────────────────────────────────────────────────

func TestCreateLeaveType_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodPost, "/v1/leave/types", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Annual Vacation",
		"code":            "VACATION",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestLeaveAccrualAndRequestLifecycle(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Create leave type
	rrType := doReq(r, http.MethodPost, "/v1/leave/types", map[string]any{
		"legal_entity_id":       "le-us",
		"name":                  "Paid Vacation",
		"code":                  "VACATION",
		"is_paid":               true,
		"accrual_rate_per_year": 120.0,
		"max_balance":           200.0,
	}, "hr-admin")

	if rrType.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrType.Code, rrType.Body.String())
	}
	var lt domain.LeaveType
	_ = json.NewDecoder(rrType.Body).Decode(&lt)

	// 2. Accrue 80 hours leave balance
	rrAccrue := doReq(r, http.MethodPost, "/v1/leave/balances/accrue", map[string]any{
		"employee_id":   "emp-501",
		"leave_type_id": lt.LeaveTypeID,
		"hours":         80.0,
	}, "hr-admin")

	if rrAccrue.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrAccrue.Code, rrAccrue.Body.String())
	}
	if pub.balanceUpdated != 1 {
		t.Errorf("expected 1 balanceUpdated event got %d", pub.balanceUpdated)
	}

	// 3. Submit leave request for 40 hours
	rrSub := doReq(r, http.MethodPost, "/v1/leave/requests", map[string]any{
		"employee_id":    "emp-501",
		"leave_type_id":  lt.LeaveTypeID,
		"start_date":     "2024-08-01",
		"end_date":       "2024-08-05",
		"total_hours":    40.0,
		"reason":         "Summer Vacation",
		"correlation_id": "corr-leave-request-1",
	}, "emp-501")

	if rrSub.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrSub.Code, rrSub.Body.String())
	}
	var req domain.LeaveRequest
	_ = json.NewDecoder(rrSub.Body).Decode(&req)

	if pub.requested != 1 {
		t.Errorf("expected 1 requested event got %d", pub.requested)
	}

	// 4. Approve leave request
	rrApprove := doReq(r, http.MethodPost, "/v1/leave/requests/"+req.RequestID+"/approve", map[string]any{
		"reviewer_notes": "Approved by manager",
	}, "manager-1")

	if rrApprove.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrApprove.Code, rrApprove.Body.String())
	}
	if pub.approved != 1 {
		t.Errorf("expected 1 approved event got %d", pub.approved)
	}
}

func TestSubmitLeaveRequest_EmployeeValidationServiceDown_FailsClosed(t *testing.T) {
	s := newStubStore()
	downValidator := &stubEmployeeValidator{err: domain.ErrStoreUnavailable}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, downValidator)

	rr := doReq(r, http.MethodPost, "/v1/leave/requests", map[string]any{
		"employee_id":    "emp-501",
		"leave_type_id":  "lt-1",
		"start_date":     "2024-08-01",
		"end_date":       "2024-08-05",
		"total_hours":    40.0,
		"correlation_id": "corr-leave-request-down",
	}, "emp-501")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (fail closed) got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeaveRequest_MissingCorrelationID_Rejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/leave/requests", map[string]any{
		"employee_id":   "emp-501",
		"leave_type_id": "lt-1",
		"start_date":    "2024-08-01",
		"end_date":      "2024-08-05",
		"total_hours":   40.0,
	}, "emp-501")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestApproveLeaveRequest_EmployeeValidationServiceDown_FailsClosed(t *testing.T) {
	s := newStubStore()

	// leave_type_id is a foreign key in the real schema, and the submit path
	// reads the leave type to apply its policy, so it has to exist.
	s.types["lt-1"] = &domain.LeaveType{
		LeaveTypeID: "lt-1", TenantID: "tenant-abc", LegalEntityID: "le-us",
		Name: "Annual Vacation", Code: "VACATION", IsPaid: true,
		Status: "ACTIVE", RequiresApproval: true,
	}
	s.balances["emp-501:lt-1"] = &domain.LeaveBalance{
		BalanceID: "bal-1", TenantID: "tenant-abc", EmployeeID: "emp-501", LeaveTypeID: "lt-1", AllocatedHours: 80,
	}
	s.requests["req-existing"] = &domain.LeaveRequest{
		RequestID: "req-existing", TenantID: "tenant-abc", EmployeeID: "emp-501", LeaveTypeID: "lt-1",
		StartDate: "2024-08-01", EndDate: "2024-08-05", TotalHours: 40, Status: "SUBMITTED",
	}

	downValidator := &stubEmployeeValidator{err: domain.ErrStoreUnavailable}
	rDown := newRouter(s, &stubPublisher{}, &stubAuthZ{}, downValidator)

	rr := doReq(rDown, http.MethodPost, "/v1/leave/requests/req-existing/approve", map[string]any{
		"reviewer_notes": "Approved",
	}, "manager-1")

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (fail closed) got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitLeaveRequest_RetriedCorrelationID_ReturnsOriginal(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// leave_type_id is a foreign key in the real schema, and the submit path
	// reads the leave type to apply its policy, so it has to exist.
	s.types["lt-1"] = &domain.LeaveType{
		LeaveTypeID: "lt-1", TenantID: "tenant-abc", LegalEntityID: "le-us",
		Name: "Annual Vacation", Code: "VACATION", IsPaid: true,
		Status: "ACTIVE", RequiresApproval: true,
	}
	s.balances["emp-501:lt-1"] = &domain.LeaveBalance{
		BalanceID: "bal-1", TenantID: "tenant-abc", EmployeeID: "emp-501", LeaveTypeID: "lt-1", AllocatedHours: 80,
	}

	body := map[string]any{
		"employee_id":    "emp-501",
		"leave_type_id":  "lt-1",
		"start_date":     "2024-08-01",
		"end_date":       "2024-08-05",
		"total_hours":    40.0,
		"correlation_id": "corr-leave-retry",
	}

	rr1 := doReq(r, http.MethodPost, "/v1/leave/requests", body, "emp-501")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr1.Code, rr1.Body.String())
	}
	var lr1 domain.LeaveRequest
	_ = json.NewDecoder(rr1.Body).Decode(&lr1)

	rr2 := doReq(r, http.MethodPost, "/v1/leave/requests", body, "emp-501")
	if rr2.Code != http.StatusCreated {
		t.Fatalf("expected 201 on replay got %d: %s", rr2.Code, rr2.Body.String())
	}
	var lr2 domain.LeaveRequest
	_ = json.NewDecoder(rr2.Body).Decode(&lr2)

	if lr2.RequestID != lr1.RequestID {
		t.Errorf("expected replay to resolve to the original request %q, got %q", lr1.RequestID, lr2.RequestID)
	}

	bal := s.balances["emp-501:lt-1"]
	if bal.PendingHours != 40.0 {
		t.Errorf("expected pending_hours to be locked exactly once (40.0), got %f — replay must not double-lock", bal.PendingHours)
	}
}
