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

	"zoiko.io/employee-master-svc/internal/domain"
	"zoiko.io/employee-master-svc/internal/handler"
	"zoiko.io/employee-master-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	employees map[string]*domain.Employee
}

func newStubStore() *stubStore {
	return &stubStore{
		employees: make(map[string]*domain.Employee),
	}
}

func (s *stubStore) CreateEmployee(_ context.Context, emp *domain.Employee) error {
	for _, existing := range s.employees {
		if existing.TenantID == emp.TenantID && existing.Email == emp.Email {
			return domain.ErrEmailAlreadyExists
		}
	}
	s.employees[emp.EmployeeID] = emp
	return nil
}

func (s *stubStore) GetEmployee(_ context.Context, id string) (*domain.Employee, error) {
	emp, ok := s.employees[id]
	if !ok {
		return nil, domain.ErrEmployeeNotFound
	}
	return emp, nil
}

func (s *stubStore) ListEmployees(_ context.Context, legalEntityID, status, workerType string) ([]domain.Employee, error) {
	var out []domain.Employee
	for _, emp := range s.employees {
		if legalEntityID != "" && emp.LegalEntityID != legalEntityID {
			continue
		}
		if status != "" && emp.Status != status {
			continue
		}
		if workerType != "" && emp.WorkerType != workerType {
			continue
		}
		out = append(out, *emp)
	}
	return out, nil
}

func (s *stubStore) UpdateStatus(_ context.Context, id, newStatus string, terminationDate *string) error {
	emp, ok := s.employees[id]
	if !ok {
		return domain.ErrEmployeeNotFound
	}
	emp.Status = newStatus
	emp.TerminationDate = terminationDate
	emp.UpdatedAt = time.Now().UTC()
	return nil
}

type stubPublisher struct {
	created, hired, statusChanged, terminated int
}

func (p *stubPublisher) PublishEmployeeCreated(_ context.Context, _ string, _ domain.Employee) {
	p.created++
}
func (p *stubPublisher) PublishEmployeeHired(_ context.Context, _ string, _ domain.Employee) {
	p.hired++
}
func (p *stubPublisher) PublishStatusChanged(_ context.Context, _ string, _ domain.Employee, _ string) {
	p.statusChanged++
}
func (p *stubPublisher) PublishEmployeeTerminated(_ context.Context, _ string, _ domain.Employee) {
	p.terminated++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop())
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

// ── CreateEmployee Tests ───────────────────────────────────────────────────────

func TestCreateEmployee_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/employees/", map[string]any{
		"legal_entity_id": "le-us",
		"first_name":      "John",
		"last_name":       "Doe",
		"email":           "john.doe@example.com",
		"worker_type":     "FULL_TIME",
		"hire_date":       "2024-01-15",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateEmployee_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rr := doReq(r, http.MethodPost, "/v1/employees/", map[string]any{
		"legal_entity_id": "le-us",
		"first_name":      "John",
		"last_name":       "Doe",
		"email":           "john.doe@example.com",
		"worker_type":     "FULL_TIME",
		"hire_date":       "2024-01-15",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestCreateEmployee_HappyPath(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})

	rr := doReq(r, http.MethodPost, "/v1/employees/", map[string]any{
		"legal_entity_id": "le-us",
		"first_name":      "John",
		"last_name":       "Doe",
		"email":           "john.doe@example.com",
		"worker_type":     "FULL_TIME",
		"hire_date":       "2024-01-15",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var emp domain.Employee
	if err := json.NewDecoder(rr.Body).Decode(&emp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if emp.Status != "ACTIVE" {
		t.Errorf("expected status ACTIVE got %q", emp.Status)
	}
	if pub.created != 1 {
		t.Errorf("expected 1 created event got %d", pub.created)
	}
	if pub.hired != 1 {
		t.Errorf("expected 1 hired event got %d", pub.hired)
	}
}

// ── UpdateStatus Tests ─────────────────────────────────────────────────────────

func TestUpdateStatus_Termination(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})

	// 1. Create employee
	rrCreate := doReq(r, http.MethodPost, "/v1/employees/", map[string]any{
		"legal_entity_id": "le-us",
		"first_name":      "Jane",
		"last_name":       "Smith",
		"email":           "jane.smith@example.com",
		"worker_type":     "FULL_TIME",
		"hire_date":       "2023-05-01",
	}, "hr-manager")

	var emp domain.Employee
	_ = json.NewDecoder(rrCreate.Body).Decode(&emp)

	// 2. Terminate employee
	termDate := "2024-06-30"
	rrTerm := doReq(r, http.MethodPut, "/v1/employees/"+emp.EmployeeID+"/status", map[string]any{
		"status":           "TERMINATED",
		"termination_date": termDate,
	}, "hr-manager")

	if rrTerm.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrTerm.Code, rrTerm.Body.String())
	}

	var updated domain.Employee
	_ = json.NewDecoder(rrTerm.Body).Decode(&updated)

	if updated.Status != "TERMINATED" {
		t.Errorf("expected status TERMINATED got %q", updated.Status)
	}
	if pub.statusChanged != 1 {
		t.Errorf("expected 1 statusChanged event got %d", pub.statusChanged)
	}
	if pub.terminated != 1 {
		t.Errorf("expected 1 terminated event got %d", pub.terminated)
	}
}