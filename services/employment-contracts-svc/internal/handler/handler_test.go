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

	"zoiko.io/employment-contracts-svc/internal/domain"
	"zoiko.io/employment-contracts-svc/internal/employee"
	"zoiko.io/employment-contracts-svc/internal/handler"
	"zoiko.io/employment-contracts-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	contracts  map[string]*domain.EmploymentContract
	amendments map[string]*domain.ContractAmendment
}

func newStubStore() *stubStore {
	return &stubStore{
		contracts:  make(map[string]*domain.EmploymentContract),
		amendments: make(map[string]*domain.ContractAmendment),
	}
}

func (s *stubStore) IssueContract(_ context.Context, c *domain.EmploymentContract) error {
	s.contracts[c.ContractID] = c
	return nil
}

func (s *stubStore) GetContract(_ context.Context, id string) (*domain.EmploymentContract, error) {
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	return c, nil
}

func (s *stubStore) GetActiveContractByEmployee(_ context.Context, employeeID string) (*domain.EmploymentContract, error) {
	for _, c := range s.contracts {
		if c.EmployeeID == employeeID && c.Status == "ACTIVE" {
			return c, nil
		}
	}
	return nil, domain.ErrContractNotFound
}

func (s *stubStore) ListContracts(_ context.Context, legalEntityID, employeeID, status string) ([]domain.EmploymentContract, error) {
	var out []domain.EmploymentContract
	for _, c := range s.contracts {
		if legalEntityID != "" && c.LegalEntityID != legalEntityID {
			continue
		}
		if employeeID != "" && c.EmployeeID != employeeID {
			continue
		}
		if status != "" && c.Status != status {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) GetContractVersionHistory(_ context.Context, contractNumber string) ([]domain.EmploymentContract, error) {
	var out []domain.EmploymentContract
	for _, c := range s.contracts {
		if c.ContractNumber == contractNumber {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (s *stubStore) AmendContract(_ context.Context, oldContractID string, newContract *domain.EmploymentContract, amd *domain.ContractAmendment) error {
	old, ok := s.contracts[oldContractID]
	if !ok || old.Status != "ACTIVE" {
		return domain.ErrContractAlreadyTerminated
	}
	old.Status = "SUPERSEDED"
	old.EffectiveTo = &amd.EffectiveFrom
	old.UpdatedAt = time.Now().UTC()

	s.contracts[newContract.ContractID] = newContract
	s.amendments[amd.AmendmentID] = amd
	return nil
}

func (s *stubStore) TerminateContract(_ context.Context, contractID, terminationDate string) error {
	c, ok := s.contracts[contractID]
	if !ok || (c.Status != "ACTIVE" && c.Status != "DRAFT") {
		return domain.ErrContractAlreadyTerminated
	}
	c.Status = "TERMINATED"
	c.EffectiveTo = &terminationDate
	c.UpdatedAt = time.Now().UTC()
	return nil
}

type stubPublisher struct {
	issued, amended, terminated int
}

func (p *stubPublisher) PublishContractIssued(_ context.Context, _ string, _ domain.EmploymentContract) {
	p.issued++
}
func (p *stubPublisher) PublishContractAmended(_ context.Context, _ string, _ domain.EmploymentContract, _ domain.ContractAmendment) {
	p.amended++
}
func (p *stubPublisher) PublishContractTerminated(_ context.Context, _ string, _ domain.EmploymentContract) {
	p.terminated++
}

type stubAuthZ struct{ err error }

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubEmployeeValidator struct{ err error }

func (v *stubEmployeeValidator) ValidateEmployee(_ context.Context, _, _, _ string) (*employee.EmployeeResponse, error) {
	if v.err != nil {
		return nil, v.err
	}
	return &employee.EmployeeResponse{Status: "ACTIVE"}, nil
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

// ── IssueContract Tests ────────────────────────────────────────────────────────

func TestIssueContract_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodPost, "/v1/contracts/", map[string]any{
		"legal_entity_id":    "le-us",
		"employee_id":        "emp-101",
		"contract_type":      "FULL_TIME",
		"title":              "Senior Software Engineer",
		"base_salary_amount": 120000.0,
		"currency":           "USD",
		"pay_frequency":      "MONTHLY",
		"effective_from":     "2024-01-01",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestIssueContract_AuthzDenied(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodPost, "/v1/contracts/", map[string]any{
		"legal_entity_id":    "le-us",
		"employee_id":        "emp-101",
		"contract_type":      "FULL_TIME",
		"title":              "Senior Software Engineer",
		"base_salary_amount": 120000.0,
		"currency":           "USD",
		"pay_frequency":      "MONTHLY",
		"effective_from":     "2024-01-01",
	}, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d", rr.Code)
	}
}

func TestIssueContract_HappyPath(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/contracts/", map[string]any{
		"legal_entity_id":    "le-us",
		"employee_id":        "emp-101",
		"contract_number":    "CTR-2024-001",
		"contract_type":      "FULL_TIME",
		"title":              "Senior Software Engineer",
		"base_salary_amount": 120000.0,
		"currency":           "USD",
		"pay_frequency":      "MONTHLY",
		"effective_from":     "2024-01-01",
	}, "principal-1")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var c domain.EmploymentContract
	if err := json.NewDecoder(rr.Body).Decode(&c); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if c.ContractNumber != "CTR-2024-001" {
		t.Errorf("expected CTR-2024-001 got %q", c.ContractNumber)
	}
	if c.Version != 1 {
		t.Errorf("expected version 1 got %d", c.Version)
	}
	if c.Status != "ACTIVE" {
		t.Errorf("expected status ACTIVE got %q", c.Status)
	}
	if pub.issued != 1 {
		t.Errorf("expected 1 issued event got %d", pub.issued)
	}
}

// ── AmendContract Tests ────────────────────────────────────────────────────────

func TestAmendContract_AppendOnlyVersionLineage(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Issue v1 contract
	rrIssue := doReq(r, http.MethodPost, "/v1/contracts/", map[string]any{
		"legal_entity_id":    "le-us",
		"employee_id":        "emp-101",
		"contract_number":    "CTR-2024-001",
		"contract_type":      "FULL_TIME",
		"title":              "Software Engineer",
		"base_salary_amount": 100000.0,
		"currency":           "USD",
		"pay_frequency":      "MONTHLY",
		"effective_from":     "2024-01-01",
	}, "hr-admin")

	var v1Contract domain.EmploymentContract
	_ = json.NewDecoder(rrIssue.Body).Decode(&v1Contract)

	// 2. Amend contract to v2
	newSalary := 130000.0
	rrAmend := doReq(r, http.MethodPost, "/v1/contracts/"+v1Contract.ContractID+"/amend", map[string]any{
		"title":              "Lead Software Engineer",
		"base_salary_amount": newSalary,
		"amendment_reason":   "Annual Promotion",
		"effective_from":     "2024-06-01",
	}, "hr-admin")

	if rrAmend.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrAmend.Code, rrAmend.Body.String())
	}

	var res struct {
		Contract  domain.EmploymentContract `json:"contract"`
		Amendment domain.ContractAmendment  `json:"amendment"`
	}
	_ = json.NewDecoder(rrAmend.Body).Decode(&res)

	// Verify v2 contract properties
	if res.Contract.Version != 2 {
		t.Errorf("expected v2 got version %d", res.Contract.Version)
	}
	if res.Contract.BaseSalaryAmount != newSalary {
		t.Errorf("expected salary %f got %f", newSalary, res.Contract.BaseSalaryAmount)
	}
	if res.Contract.Status != "ACTIVE" {
		t.Errorf("expected v2 status ACTIVE got %q", res.Contract.Status)
	}

	// Verify v1 contract was SUPERSEDED and NOT overwritten
	oldC, err := s.GetContract(context.Background(), v1Contract.ContractID)
	if err != nil {
		t.Fatalf("fetch v1: %v", err)
	}
	if oldC.Status != "SUPERSEDED" {
		t.Errorf("expected v1 status SUPERSEDED got %q", oldC.Status)
	}
	if oldC.Version != 1 {
		t.Errorf("expected v1 version 1 got %d", oldC.Version)
	}

	if pub.amended != 1 {
		t.Errorf("expected 1 amended event got %d", pub.amended)
	}
}

// ── TerminateContract Tests ────────────────────────────────────────────────────

func TestTerminateContract_Success(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Issue contract
	rrIssue := doReq(r, http.MethodPost, "/v1/contracts/", map[string]any{
		"legal_entity_id":    "le-us",
		"employee_id":        "emp-102",
		"contract_type":      "FULL_TIME",
		"title":              "Architect",
		"base_salary_amount": 150000.0,
		"currency":           "USD",
		"pay_frequency":      "MONTHLY",
		"effective_from":     "2024-01-01",
	}, "hr-admin")

	var c domain.EmploymentContract
	_ = json.NewDecoder(rrIssue.Body).Decode(&c)

	// 2. Terminate contract
	rrTerm := doReq(r, http.MethodPost, "/v1/contracts/"+c.ContractID+"/terminate", map[string]any{
		"termination_date": "2024-12-31",
	}, "hr-admin")

	if rrTerm.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrTerm.Code, rrTerm.Body.String())
	}

	var terminatedC domain.EmploymentContract
	_ = json.NewDecoder(rrTerm.Body).Decode(&terminatedC)

	if terminatedC.Status != "TERMINATED" {
		t.Errorf("expected status TERMINATED got %q", terminatedC.Status)
	}
	if pub.terminated != 1 {
		t.Errorf("expected 1 terminated event got %d", pub.terminated)
	}
}