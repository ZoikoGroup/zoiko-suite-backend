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

	"zoiko.io/benefits-svc/internal/domain"
	"zoiko.io/benefits-svc/internal/employee"
	"zoiko.io/benefits-svc/internal/handler"
	"zoiko.io/benefits-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	plans           map[string]*domain.BenefitPlan
	plansByCorr     map[string]string
	elections       map[string]*domain.BenefitElection
	electionsByCorr map[string]string
}

func newStubStore() *stubStore {
	return &stubStore{
		plans:           make(map[string]*domain.BenefitPlan),
		plansByCorr:     make(map[string]string),
		elections:       make(map[string]*domain.BenefitElection),
		electionsByCorr: make(map[string]string),
	}
}

func (s *stubStore) CreatePlan(_ context.Context, p *domain.BenefitPlan) (bool, error) {
	if p.CorrelationID != "" {
		if existingID, ok := s.plansByCorr[p.CorrelationID]; ok {
			*p = *s.plans[existingID]
			return false, nil
		}
	}
	s.plans[p.PlanID] = p
	if p.CorrelationID != "" {
		s.plansByCorr[p.CorrelationID] = p.PlanID
	}
	return true, nil
}

func (s *stubStore) ListPlans(_ context.Context, legalEntityID, status string) ([]domain.BenefitPlan, error) {
	var out []domain.BenefitPlan
	for _, p := range s.plans {
		if legalEntityID != "" && p.LegalEntityID != legalEntityID {
			continue
		}
		if status != "" && p.Status != status {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *stubStore) GetPlan(_ context.Context, planID string) (*domain.BenefitPlan, error) {
	p, ok := s.plans[planID]
	if !ok {
		return nil, domain.ErrPlanNotFound
	}
	return p, nil
}

func (s *stubStore) CreateElection(_ context.Context, e *domain.BenefitElection) (bool, error) {
	if e.CorrelationID != "" {
		if existingID, ok := s.electionsByCorr[e.CorrelationID]; ok {
			*e = *s.elections[existingID]
			return false, nil
		}
	}
	s.elections[e.ElectionID] = e
	if e.CorrelationID != "" {
		s.electionsByCorr[e.CorrelationID] = e.ElectionID
	}
	return true, nil
}

func (s *stubStore) GetElection(_ context.Context, electionID string) (*domain.BenefitElection, error) {
	e, ok := s.elections[electionID]
	if !ok {
		return nil, domain.ErrElectionNotFound
	}
	return e, nil
}

func (s *stubStore) UpdateElection(_ context.Context, e *domain.BenefitElection) error {
	existing, ok := s.elections[e.ElectionID]
	if !ok || existing.Status != "ACTIVE" {
		return domain.ErrElectionNotFound
	}
	s.elections[e.ElectionID] = e
	return nil
}

func (s *stubStore) CancelElection(_ context.Context, electionID, cancelDate string) error {
	e, ok := s.elections[electionID]
	if !ok {
		return domain.ErrElectionNotFound
	}
	if e.Status == "CANCELLED" {
		return domain.ErrElectionAlreadyCancelled
	}
	e.Status = "CANCELLED"
	e.EffectiveTo = &cancelDate
	return nil
}

func (s *stubStore) ListElectionsByEmployee(_ context.Context, employeeID, status string) ([]domain.BenefitElection, error) {
	var out []domain.BenefitElection
	for _, e := range s.elections {
		if employeeID != "" && e.EmployeeID != employeeID {
			continue
		}
		if status != "" && e.Status != status {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

type stubPublisher struct {
	enrolled, changed, terminated int
}

func (p *stubPublisher) PublishBenefitEnrolled(_ context.Context, _, _, _ string, _ domain.BenefitElection) {
	p.enrolled++
}
func (p *stubPublisher) PublishBenefitChanged(_ context.Context, _, _, _ string, _ domain.BenefitElection) {
	p.changed++
}
func (p *stubPublisher) PublishBenefitTerminated(_ context.Context, _, _, _ string, _ domain.BenefitElection) {
	p.terminated++
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

// ── CreatePlan Tests ───────────────────────────────────────────────────────────

func TestCreatePlan_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodPost, "/v1/benefits/plans", map[string]any{
		"legal_entity_id":         "le-us",
		"name":                    "Gold Health Plan",
		"plan_type":               "HEALTH_INSURANCE",
		"provider_name":           "Aetna",
		"deduction_tax_treatment": "PRE_TAX",
		"currency":                "USD",
		"correlation_id":          "corr-plan-missing-principal",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreatePlan_HappyPath(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/benefits/plans", map[string]any{
		"legal_entity_id":              "le-us",
		"name":                         "Gold Health Plan",
		"plan_type":                    "HEALTH_INSURANCE",
		"provider_name":                "Aetna",
		"deduction_tax_treatment":      "PRE_TAX",
		"employer_contribution_pct":    50.0,
		"employee_contribution_amount": 200.0,
		"currency":                     "USD",
		"correlation_id":               "corr-plan-1",
	}, "hr-admin")

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var p domain.BenefitPlan
	_ = json.NewDecoder(rr.Body).Decode(&p)
	if p.Name != "Gold Health Plan" || p.DeductionTaxTreatment != "PRE_TAX" {
		t.Errorf("unexpected plan details: %+v", p)
	}
}

// ── EnrollBenefit & Deduction Summary Tests ─────────────────────────────────────

func TestEnrollAndDeductionSummary(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	// 1. Create PRE_TAX plan
	rrP1 := doReq(r, http.MethodPost, "/v1/benefits/plans", map[string]any{
		"legal_entity_id":              "le-us",
		"name":                         "Health Plan PreTax",
		"plan_type":                    "HEALTH_INSURANCE",
		"provider_name":                "BlueCross",
		"deduction_tax_treatment":      "PRE_TAX",
		"employer_contribution_pct":    50.0,
		"employee_contribution_amount": 300.0,
		"currency":                     "USD",
		"correlation_id":               "corr-plan-pretax",
	}, "hr-admin")

	var planPreTax domain.BenefitPlan
	_ = json.NewDecoder(rrP1.Body).Decode(&planPreTax)

	// 2. Create POST_TAX plan
	rrP2 := doReq(r, http.MethodPost, "/v1/benefits/plans", map[string]any{
		"legal_entity_id":              "le-us",
		"name":                         "Supplemental Life PostTax",
		"plan_type":                    "LIFE_INSURANCE",
		"provider_name":                "MetLife",
		"deduction_tax_treatment":      "POST_TAX",
		"employer_contribution_pct":    0.0,
		"employee_contribution_amount": 50.0,
		"currency":                     "USD",
		"correlation_id":               "corr-plan-posttax",
	}, "hr-admin")

	var planPostTax domain.BenefitPlan
	_ = json.NewDecoder(rrP2.Body).Decode(&planPostTax)

	// 3. Enroll in PreTax Plan
	rrE1 := doReq(r, http.MethodPost, "/v1/benefits/elections", map[string]any{
		"employee_id":    "emp-201",
		"plan_id":        planPreTax.PlanID,
		"coverage_level": "EMPLOYEE_PLUS_FAMILY",
		"effective_from": "2024-01-01",
		"correlation_id": "corr-elect-pretax",
	}, "hr-admin")

	if rrE1.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrE1.Code, rrE1.Body.String())
	}

	// 4. Enroll in PostTax Plan
	rrE2 := doReq(r, http.MethodPost, "/v1/benefits/elections", map[string]any{
		"employee_id":    "emp-201",
		"plan_id":        planPostTax.PlanID,
		"coverage_level": "EMPLOYEE_ONLY",
		"effective_from": "2024-01-01",
		"correlation_id": "corr-elect-posttax",
	}, "hr-admin")

	if rrE2.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rrE2.Code, rrE2.Body.String())
	}

	if pub.enrolled != 2 {
		t.Errorf("expected 2 enrolled events got %d", pub.enrolled)
	}

	// 5. Query Deduction Summary
	rrSummary := doReq(r, http.MethodGet, "/v1/benefits/deductions/employee/emp-201", nil, "hr-admin")
	if rrSummary.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrSummary.Code, rrSummary.Body.String())
	}

	var summary domain.DeductionSummary
	_ = json.NewDecoder(rrSummary.Body).Decode(&summary)

	if summary.PreTaxDeductionTotal != 300.0 {
		t.Errorf("expected pre-tax total 300.0 got %f", summary.PreTaxDeductionTotal)
	}
	if summary.PostTaxDeductionTotal != 50.0 {
		t.Errorf("expected post-tax total 50.0 got %f", summary.PostTaxDeductionTotal)
	}
	if summary.EmployerContributionTotal != 150.0 {
		t.Errorf("expected employer contribution 150.0 got %f", summary.EmployerContributionTotal)
	}
	if summary.ElectionsCount != 2 {
		t.Errorf("expected 2 elections got %d", summary.ElectionsCount)
	}
}

// ── UpdateElection / CancelElection Tests ───────────────────────────────────────

func TestUpdateElection_HappyPath(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rrP := doReq(r, http.MethodPost, "/v1/benefits/plans", map[string]any{
		"legal_entity_id":         "le-us",
		"name":                    "Health Plan",
		"plan_type":               "HEALTH_INSURANCE",
		"provider_name":           "Aetna",
		"deduction_tax_treatment": "PRE_TAX",
		"currency":                "USD",
		"correlation_id":          "corr-plan-update",
	}, "hr-admin")
	var plan domain.BenefitPlan
	_ = json.NewDecoder(rrP.Body).Decode(&plan)

	rrE := doReq(r, http.MethodPost, "/v1/benefits/elections", map[string]any{
		"employee_id":    "emp-301",
		"plan_id":        plan.PlanID,
		"coverage_level": "EMPLOYEE_ONLY",
		"effective_from": "2024-01-01",
		"correlation_id": "corr-elect-update",
	}, "hr-admin")
	var election domain.BenefitElection
	_ = json.NewDecoder(rrE.Body).Decode(&election)

	rrU := doReq(r, http.MethodPut, "/v1/benefits/elections/"+election.ElectionID, map[string]any{
		"coverage_level": "EMPLOYEE_PLUS_SPOUSE",
	}, "hr-admin")
	if rrU.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrU.Code, rrU.Body.String())
	}

	var updated domain.BenefitElection
	_ = json.NewDecoder(rrU.Body).Decode(&updated)
	if updated.CoverageLevel != "EMPLOYEE_PLUS_SPOUSE" {
		t.Errorf("expected coverage_level updated, got %q", updated.CoverageLevel)
	}
}

func TestUpdateElection_NotFound(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rrU := doReq(r, http.MethodPut, "/v1/benefits/elections/does-not-exist", map[string]any{
		"coverage_level": "EMPLOYEE_ONLY",
	}, "hr-admin")
	if rrU.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", rrU.Code, rrU.Body.String())
	}
}

func TestUpdateElection_EmployeeValidationServiceDown_FailsClosed(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rrP := doReq(r, http.MethodPost, "/v1/benefits/plans", map[string]any{
		"legal_entity_id":         "le-us",
		"name":                    "Health Plan",
		"plan_type":               "HEALTH_INSURANCE",
		"provider_name":           "Aetna",
		"deduction_tax_treatment": "PRE_TAX",
		"currency":                "USD",
		"correlation_id":          "corr-plan-update-2",
	}, "hr-admin")
	var plan domain.BenefitPlan
	_ = json.NewDecoder(rrP.Body).Decode(&plan)

	rrE := doReq(r, http.MethodPost, "/v1/benefits/elections", map[string]any{
		"employee_id":    "emp-302",
		"plan_id":        plan.PlanID,
		"coverage_level": "EMPLOYEE_ONLY",
		"effective_from": "2024-01-01",
		"correlation_id": "corr-elect-update-2",
	}, "hr-admin")
	var election domain.BenefitElection
	_ = json.NewDecoder(rrE.Body).Decode(&election)

	// Simulate employee-master-svc being unreachable for the update itself.
	downValidator := &stubEmployeeValidator{err: domain.ErrStoreUnavailable}
	rDown := newRouter(s, &stubPublisher{}, &stubAuthZ{}, downValidator)

	rrU := doReq(rDown, http.MethodPut, "/v1/benefits/elections/"+election.ElectionID, map[string]any{
		"coverage_level": "EMPLOYEE_PLUS_SPOUSE",
	}, "hr-admin")
	if rrU.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (fail closed) got %d: %s", rrU.Code, rrU.Body.String())
	}
}

func TestCancelElection_HappyPath(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubEmployeeValidator{})

	rrP := doReq(r, http.MethodPost, "/v1/benefits/plans", map[string]any{
		"legal_entity_id":         "le-us",
		"name":                    "Health Plan",
		"plan_type":               "HEALTH_INSURANCE",
		"provider_name":           "Aetna",
		"deduction_tax_treatment": "PRE_TAX",
		"currency":                "USD",
		"correlation_id":          "corr-plan-cancel",
	}, "hr-admin")
	var plan domain.BenefitPlan
	_ = json.NewDecoder(rrP.Body).Decode(&plan)

	rrE := doReq(r, http.MethodPost, "/v1/benefits/elections", map[string]any{
		"employee_id":    "emp-303",
		"plan_id":        plan.PlanID,
		"coverage_level": "EMPLOYEE_ONLY",
		"effective_from": "2024-01-01",
		"correlation_id": "corr-elect-cancel",
	}, "hr-admin")
	var election domain.BenefitElection
	_ = json.NewDecoder(rrE.Body).Decode(&election)

	rrC := doReq(r, http.MethodPost, "/v1/benefits/elections/"+election.ElectionID+"/cancel", nil, "hr-admin")
	if rrC.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rrC.Code, rrC.Body.String())
	}
	if pub.terminated != 1 {
		t.Errorf("expected 1 terminated event got %d", pub.terminated)
	}

	// A second cancel of the same election must not succeed twice.
	rrC2 := doReq(r, http.MethodPost, "/v1/benefits/elections/"+election.ElectionID+"/cancel", nil, "hr-admin")
	if rrC2.Code != http.StatusConflict {
		t.Fatalf("expected 409 on double-cancel got %d: %s", rrC2.Code, rrC2.Body.String())
	}
}

func TestCreatePlan_Idempotent_ReplayReturns200(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	body := map[string]any{
		"legal_entity_id":         "le-us",
		"name":                    "Gold Health Plan",
		"plan_type":               "HEALTH_INSURANCE",
		"provider_name":           "Aetna",
		"deduction_tax_treatment": "PRE_TAX",
		"currency":                "USD",
		"correlation_id":          "corr-plan-replay",
	}

	rr1 := doReq(r, http.MethodPost, "/v1/benefits/plans", body, "hr-admin")
	if rr1.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr1.Code, rr1.Body.String())
	}
	var p1 domain.BenefitPlan
	_ = json.NewDecoder(rr1.Body).Decode(&p1)

	rr2 := doReq(r, http.MethodPost, "/v1/benefits/plans", body, "hr-admin")
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay got %d: %s", rr2.Code, rr2.Body.String())
	}
	var p2 domain.BenefitPlan
	_ = json.NewDecoder(rr2.Body).Decode(&p2)
	if p2.PlanID != p1.PlanID {
		t.Errorf("expected replay to resolve to the original plan %q, got %q", p1.PlanID, p2.PlanID)
	}
}
