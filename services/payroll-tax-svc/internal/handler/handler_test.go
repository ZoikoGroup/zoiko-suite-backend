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

	"zoiko.io/payroll-tax-svc/internal/domain"
	"zoiko.io/payroll-tax-svc/internal/employee"
	"zoiko.io/payroll-tax-svc/internal/handler"
	"zoiko.io/payroll-tax-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	profiles     map[string]*domain.TaxJurisdictionProfile
	calculations map[string]*domain.TaxCalculationRecord
	audits       map[string]*domain.TaxBasisAudit
}

func newStubStore() *stubStore {
	return &stubStore{
		profiles:     make(map[string]*domain.TaxJurisdictionProfile),
		calculations: make(map[string]*domain.TaxCalculationRecord),
		audits:       make(map[string]*domain.TaxBasisAudit),
	}
}

func (s *stubStore) CreateProfile(_ context.Context, p *domain.TaxJurisdictionProfile) error {
	s.profiles[p.ProfileID] = p
	return nil
}

func (s *stubStore) ListProfiles(_ context.Context, legalEntityID, jurisdictionCode string) ([]domain.TaxJurisdictionProfile, error) {
	var out []domain.TaxJurisdictionProfile
	for _, p := range s.profiles {
		if legalEntityID != "" && p.LegalEntityID != legalEntityID {
			continue
		}
		if jurisdictionCode != "" && p.JurisdictionCode != jurisdictionCode {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *stubStore) CreateCalculationWithAudit(_ context.Context, calc *domain.TaxCalculationRecord, audit *domain.TaxBasisAudit) error {
	s.calculations[calc.CalculationID] = calc
	s.audits[calc.CalculationID] = audit
	return nil
}

func (s *stubStore) GetCalculation(_ context.Context, calculationID string) (*domain.TaxCalculationRecord, error) {
	c, ok := s.calculations[calculationID]
	if !ok {
		return nil, domain.ErrCalculationNotFound
	}
	return c, nil
}

func (s *stubStore) ListCalculations(_ context.Context, payrollRunID, employeeID string) ([]domain.TaxCalculationRecord, error) {
	var out []domain.TaxCalculationRecord
	for _, c := range s.calculations {
		if payrollRunID != "" && c.PayrollRunID != payrollRunID {
			continue
		}
		if employeeID != "" && c.EmployeeID != employeeID {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) GetTaxBasisAudit(_ context.Context, calculationID string) (*domain.TaxBasisAudit, error) {
	a, ok := s.audits[calculationID]
	if !ok {
		return nil, domain.ErrAuditNotFound
	}
	return a, nil
}

func (s *stubStore) AdjustCalculation(_ context.Context, calculationID string, newBreakdown []domain.TaxComponent, newTotal float64, _ string) error {
	c, ok := s.calculations[calculationID]
	if !ok {
		return domain.ErrCalculationNotFound
	}
	c.TaxBreakdown = newBreakdown
	c.TotalTaxAmount = newTotal
	c.Status = "ADJUSTED"
	return nil
}

type stubPublisher struct {
	calculated, adjusted, exception int
}

func (p *stubPublisher) PublishTaxCalculated(_ context.Context, _ string, _ domain.TaxCalculationRecord) {
	p.calculated++
}
func (p *stubPublisher) PublishTaxAdjusted(_ context.Context, _ string, _ domain.TaxCalculationRecord) {
	p.adjusted++
}
func (p *stubPublisher) PublishTaxException(_ context.Context, _, _, _ string) {
	p.exception++
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

// ── CreateProfile Tests ────────────────────────────────────────────────────────

func TestCreateProfile_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodPost, "/v1/payroll-tax/profiles", map[string]any{
		"legal_entity_id":   "le-us",
		"jurisdiction_code": "US-CA",
		"tax_engine_type":   "STANDARD_ENGINE",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateProfile_HappyPath(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/payroll-tax/profiles", map[string]any{
		"legal_entity_id":   "le-us",
		"jurisdiction_code": "US-CA",
		"tax_engine_type":   "STANDARD_ENGINE",
	}, "tax-admin")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var p domain.TaxJurisdictionProfile
	_ = json.NewDecoder(rr.Body).Decode(&p)
	if p.JurisdictionCode != "US-CA" {
		t.Errorf("expected US-CA got %q", p.JurisdictionCode)
	}
}

// ── CalculateTax & Audit Log Tests ─────────────────────────────────────────────

func TestCalculateTax_AndGetAuditLog(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Calculate tax
	rrCalc := doReq(r, http.MethodPost, "/v1/payroll-tax/calculate", map[string]any{
		"payroll_run_id":           "prun-301",
		"employee_id":              "emp-301",
		"jurisdiction_code":        "US-NY",
		"gross_taxable_amount":     10000.0,
		"pre_tax_deduction_amount": 1000.0,
		"currency":                 "USD",
	}, "payroll-admin")

	if rrCalc.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrCalc.Code, rrCalc.Body.String())
	}

	var calc domain.TaxCalculationRecord
	_ = json.NewDecoder(rrCalc.Body).Decode(&calc)

	if calc.TaxableBasis != 9000.0 {
		t.Errorf("expected taxable basis 9000.0 got %f", calc.TaxableBasis)
	}
	if len(calc.TaxBreakdown) != 3 {
		t.Errorf("expected 3 tax components got %d", len(calc.TaxBreakdown))
	}
	if pub.calculated != 1 {
		t.Errorf("expected 1 calculated event got %d", pub.calculated)
	}

	// 2. Fetch Audit Log
	rrAudit := doReq(r, http.MethodGet, "/v1/payroll-tax/calculations/"+calc.CalculationID+"/audit", nil, "payroll-admin")
	if rrAudit.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrAudit.Code, rrAudit.Body.String())
	}

	var audit domain.TaxBasisAudit
	_ = json.NewDecoder(rrAudit.Body).Decode(&audit)
	if audit.CalculationID != calc.CalculationID {
		t.Errorf("audit calculation_id mismatch: %s vs %s", audit.CalculationID, calc.CalculationID)
	}

	// 3. Adjust calculation
	rrAdj := doReq(r, http.MethodPost, "/v1/payroll-tax/calculations/"+calc.CalculationID+"/adjust", map[string]any{
		"new_tax_breakdown": []domain.TaxComponent{
			{TaxName: "Income Tax (Adjusted)", TaxType: "STATE_FEDERAL", RatePct: 14.0, TaxAmount: 1260.0},
		},
		"new_total_tax_amount": 1260.0,
		"reason":               "Tax exemption certificate filed",
	}, "payroll-admin")

	if rrAdj.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrAdj.Code, rrAdj.Body.String())
	}

	var adjCalc domain.TaxCalculationRecord
	_ = json.NewDecoder(rrAdj.Body).Decode(&adjCalc)
	if adjCalc.Status != "ADJUSTED" || adjCalc.TotalTaxAmount != 1260.0 {
		t.Errorf("unexpected adjusted calculation details: %+v", adjCalc)
	}
	if pub.adjusted != 1 {
		t.Errorf("expected 1 adjusted event got %d", pub.adjusted)
	}
}