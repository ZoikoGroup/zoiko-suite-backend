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

	"zoiko.io/payroll-exceptions-svc/internal/domain"
	"zoiko.io/payroll-exceptions-svc/internal/employee"
	"zoiko.io/payroll-exceptions-svc/internal/handler"
	"zoiko.io/payroll-exceptions-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	exceptions map[string]*domain.PayrollException
}

func newStubStore() *stubStore {
	return &stubStore{
		exceptions: make(map[string]*domain.PayrollException),
	}
}

func (s *stubStore) CreateException(_ context.Context, e *domain.PayrollException) error {
	s.exceptions[e.ExceptionID] = e
	return nil
}

func (s *stubStore) GetException(_ context.Context, id string) (*domain.PayrollException, error) {
	e, ok := s.exceptions[id]
	if !ok {
		return nil, domain.ErrExceptionNotFound
	}
	return e, nil
}

func (s *stubStore) ListExceptions(_ context.Context, payrollRunID, employeeID, status, severity string) ([]domain.PayrollException, error) {
	var out []domain.PayrollException
	for _, e := range s.exceptions {
		if payrollRunID != "" && e.PayrollRunID != payrollRunID {
			continue
		}
		if employeeID != "" && (e.EmployeeID == nil || *e.EmployeeID != employeeID) {
			continue
		}
		if status != "" && e.Status != status {
			continue
		}
		if severity != "" && e.Severity != severity {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

func (s *stubStore) ResolveException(_ context.Context, id, notes, resolvedBy, newStatus string) error {
	e, ok := s.exceptions[id]
	if !ok {
		return domain.ErrExceptionNotFound
	}
	if e.Status == "RESOLVED" || e.Status == "WAIVED" {
		return domain.ErrAlreadyResolved
	}
	e.Status = newStatus
	e.ResolutionNotes = &notes
	e.ResolvedBy = &resolvedBy
	return nil
}

func (s *stubStore) GetReleaseBlockers(_ context.Context, payrollRunID string) (*domain.ReleaseBlockerSummary, error) {
	sum := &domain.ReleaseBlockerSummary{PayrollRunID: payrollRunID}
	for _, e := range s.exceptions {
		if e.PayrollRunID == payrollRunID {
			sum.TotalExceptions++
			if (e.Status == "OPEN" || e.Status == "IN_REVIEW") && e.Severity == "BLOCKER" {
				sum.BlockerCount++
			}
			if (e.Status == "OPEN" || e.Status == "IN_REVIEW") && e.Severity == "WARNING" {
				sum.WarningCount++
			}
		}
	}
	sum.CanRelease = (sum.BlockerCount == 0)
	return sum, nil
}

type stubPublisher struct {
	raised, resolved, blockerFlagged int
}

func (p *stubPublisher) PublishExceptionRaised(_ context.Context, _ string, _ domain.PayrollException) {
	p.raised++
}
func (p *stubPublisher) PublishExceptionResolved(_ context.Context, _ string, _ domain.PayrollException) {
	p.resolved++
}
func (p *stubPublisher) PublishBlockerFlagged(_ context.Context, _, _ string, _ int) {
	p.blockerFlagged++
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

// ── RaiseException Tests ───────────────────────────────────────────────────────

func TestRaiseException_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodPost, "/v1/payroll-exceptions", map[string]any{
		"payroll_run_id": "prun-401",
		"exception_code": "NEGATIVE_NET_PAY",
		"severity":       "BLOCKER",
		"description":    "Net pay calculated as -$150.00",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestRaiseException_BlockerFlow(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Raise BLOCKER exception
	rrRaise := doReq(r, http.MethodPost, "/v1/payroll-exceptions", map[string]any{
		"payroll_run_id": "prun-401",
		"exception_code": "NEGATIVE_NET_PAY",
		"severity":       "BLOCKER",
		"description":    "Net pay calculated as -$150.00",
	}, "payroll-admin")

	if rrRaise.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrRaise.Code, rrRaise.Body.String())
	}

	var exc domain.PayrollException
	_ = json.NewDecoder(rrRaise.Body).Decode(&exc)
	if exc.Severity != "BLOCKER" || exc.Status != "OPEN" {
		t.Errorf("unexpected exception state: %+v", exc)
	}

	if pub.raised != 1 || pub.blockerFlagged != 1 {
		t.Errorf("expected 1 raised & 1 blockerFlagged event got raised=%d, blocker=%d", pub.raised, pub.blockerFlagged)
	}

	// 2. Query release blockers -> can_release = false
	rrBlockers := doReq(r, http.MethodGet, "/v1/payroll-exceptions/blockers/prun-401", nil, "payroll-admin")
	if rrBlockers.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrBlockers.Code, rrBlockers.Body.String())
	}

	var sum domain.ReleaseBlockerSummary
	_ = json.NewDecoder(rrBlockers.Body).Decode(&sum)
	if sum.CanRelease || sum.BlockerCount != 1 {
		t.Errorf("expected can_release=false, blocker_count=1 got %+v", sum)
	}

	// 3. Resolve exception
	rrResolve := doReq(r, http.MethodPost, "/v1/payroll-exceptions/"+exc.ExceptionID+"/resolve", map[string]any{
		"resolution_notes": "Recalculated tax deduction, net pay is now positive $2,450.00",
	}, "payroll-admin")

	if rrResolve.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrResolve.Code, rrResolve.Body.String())
	}

	if pub.resolved != 1 {
		t.Errorf("expected 1 resolved event got %d", pub.resolved)
	}

	// 4. Query release blockers again -> can_release = true
	rrBlockers2 := doReq(r, http.MethodGet, "/v1/payroll-exceptions/blockers/prun-401", nil, "payroll-admin")
	var sum2 domain.ReleaseBlockerSummary
	_ = json.NewDecoder(rrBlockers2.Body).Decode(&sum2)
	if !sum2.CanRelease || sum2.BlockerCount != 0 {
		t.Errorf("expected can_release=true, blocker_count=0 got %+v", sum2)
	}
}