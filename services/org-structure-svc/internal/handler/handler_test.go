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

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/employee"
	"zoiko.io/org-structure-svc/internal/handler"
	"zoiko.io/org-structure-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	departments map[string]*domain.Department
	positions   map[string]*domain.Position
	assignments map[string]*domain.OrgAssignment
}

func newStubStore() *stubStore {
	return &stubStore{
		departments: make(map[string]*domain.Department),
		positions:   make(map[string]*domain.Position),
		assignments: make(map[string]*domain.OrgAssignment),
	}
}

func (s *stubStore) CreateDepartment(_ context.Context, d *domain.Department) error {
	s.departments[d.DepartmentID] = d
	return nil
}

func (s *stubStore) ListDepartments(_ context.Context, legalEntityID string) ([]domain.Department, error) {
	var out []domain.Department
	for _, d := range s.departments {
		if legalEntityID != "" && d.LegalEntityID != legalEntityID {
			continue
		}
		out = append(out, *d)
	}
	return out, nil
}

func (s *stubStore) GetDepartment(_ context.Context, id string) (*domain.Department, error) {
	d, ok := s.departments[id]
	if !ok {
		return nil, domain.ErrDepartmentNotFound
	}
	return d, nil
}

func (s *stubStore) CreatePosition(_ context.Context, p *domain.Position) error {
	s.positions[p.PositionID] = p
	return nil
}

func (s *stubStore) ListPositions(_ context.Context, departmentID string) ([]domain.Position, error) {
	var out []domain.Position
	for _, p := range s.positions {
		if departmentID != "" && p.DepartmentID != departmentID {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *stubStore) GetPosition(_ context.Context, id string) (*domain.Position, error) {
	p, ok := s.positions[id]
	if !ok {
		return nil, domain.ErrPositionNotFound
	}
	return p, nil
}

func (s *stubStore) AssignEmployee(_ context.Context, req *domain.AssignEmployeeRequest) (*domain.OrgAssignment, error) {
	oa := &domain.OrgAssignment{
		AssignmentID:      "assign-1",
		TenantID:          "tenant-abc",
		EmployeeID:        req.EmployeeID,
		DepartmentID:      req.DepartmentID,
		PositionID:        req.PositionID,
		ManagerEmployeeID: req.ManagerEmployeeID,
		EffectiveFrom:     req.EffectiveFrom,
		Status:            "ACTIVE",
	}
	s.assignments[req.EmployeeID] = oa
	return oa, nil
}

func (s *stubStore) GetEmployeeAssignment(_ context.Context, employeeID string) (*domain.OrgAssignment, error) {
	oa, ok := s.assignments[employeeID]
	if !ok {
		return nil, domain.ErrAssignmentNotFound
	}
	return oa, nil
}

type stubPublisher struct {
	posCreated, empAssigned, orgChanged int
}

func (p *stubPublisher) PublishPositionCreated(_ context.Context, _ string, _ domain.Position) {
	p.posCreated++
}
func (p *stubPublisher) PublishEmployeeAssigned(_ context.Context, _ string, _ domain.OrgAssignment) {
	p.empAssigned++
}
func (p *stubPublisher) PublishOrgStructureChanged(_ context.Context, _ string, _, _ string) {
	p.orgChanged++
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

// ── Tests ──────────────────────────────────────────────────────────────────────

func TestCreateDepartment_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodPost, "/v1/org/departments", map[string]any{
		"legal_entity_id":  "le-us",
		"name":             "Engineering",
		"code":             "ENG",
		"cost_center_code": "CC-101",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestOrgStructureLifecycle(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Create Department
	rrDept := doReq(r, http.MethodPost, "/v1/org/departments", map[string]any{
		"legal_entity_id":  "le-us",
		"name":             "Engineering",
		"code":             "ENG",
		"cost_center_code": "CC-101",
	}, "hr-admin")

	if rrDept.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrDept.Code, rrDept.Body.String())
	}
	var d domain.Department
	_ = json.NewDecoder(rrDept.Body).Decode(&d)

	if pub.orgChanged != 1 {
		t.Errorf("expected 1 orgChanged event got %d", pub.orgChanged)
	}

	// 2. Create Position
	rrPos := doReq(r, http.MethodPost, "/v1/org/positions", map[string]any{
		"legal_entity_id": "le-us",
		"department_id":   d.DepartmentID,
		"title":           "Senior Backend Engineer",
		"code":            "ENG-SR-BE",
		"job_level":       "L5",
		"max_headcount":   5,
	}, "hr-admin")

	if rrPos.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrPos.Code, rrPos.Body.String())
	}
	var pos domain.Position
	_ = json.NewDecoder(rrPos.Body).Decode(&pos)

	if pub.posCreated != 1 {
		t.Errorf("expected 1 posCreated event got %d", pub.posCreated)
	}

	// 3. Assign Employee to Position
	mgrID := "emp-mgr-01"
	rrAssign := doReq(r, http.MethodPost, "/v1/org/assignments", map[string]any{
		"employee_id":         "emp-101",
		"department_id":       d.DepartmentID,
		"position_id":         pos.PositionID,
		"manager_employee_id": &mgrID,
		"effective_from":      "2024-01-01",
	}, "hr-admin")

	if rrAssign.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrAssign.Code, rrAssign.Body.String())
	}
	if pub.empAssigned != 1 {
		t.Errorf("expected 1 empAssigned event got %d", pub.empAssigned)
	}
}