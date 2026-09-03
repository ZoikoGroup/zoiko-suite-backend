package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/compensation-svc/internal/domain"
	"zoiko.io/compensation-svc/internal/handler"
)

// ── component store stubs ──────────────────────────────────────────────────────

func (s *stubStore) GetStructure(_ context.Context, structureID string) (*domain.CompensationStructure, error) {
	str, ok := s.structures[structureID]
	if !ok {
		return nil, domain.ErrStructureNotFound
	}
	return str, nil
}

func (s *stubStore) CreateComponent(_ context.Context, c *domain.SalaryComponent) error {
	// Mirrors idx_salary_components_entity_code, partial on ACTIVE.
	for _, existing := range s.components {
		if existing.LegalEntityID == c.LegalEntityID &&
			existing.Code == c.Code &&
			existing.Status == "ACTIVE" {
			return domain.ErrComponentCodeExists
		}
	}
	s.components[c.ComponentID] = c
	return nil
}

func (s *stubStore) GetComponent(_ context.Context, componentID string) (*domain.SalaryComponent, error) {
	c, ok := s.components[componentID]
	if !ok {
		return nil, domain.ErrComponentNotFound
	}
	return c, nil
}

func (s *stubStore) ListComponents(_ context.Context, legalEntityID, componentType string, includeInactive bool) ([]domain.SalaryComponent, error) {
	var out []domain.SalaryComponent
	for _, c := range s.components {
		if legalEntityID != "" && c.LegalEntityID != legalEntityID {
			continue
		}
		if componentType != "" && c.ComponentType != componentType {
			continue
		}
		if !includeInactive && c.Status != "ACTIVE" {
			continue
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ComponentType != out[j].ComponentType {
			return out[i].ComponentType < out[j].ComponentType
		}
		return out[i].Code < out[j].Code
	})
	return out, nil
}

func (s *stubStore) DeactivateComponent(_ context.Context, componentID string) error {
	c, ok := s.components[componentID]
	if !ok || c.Status != "ACTIVE" {
		return domain.ErrComponentNotFound
	}
	c.Status = "INACTIVE"
	return nil
}

func (s *stubStore) SetStructureComponents(_ context.Context, structureID string, inputs []domain.StructureComponentInput) error {
	var set []domain.StructureComponent
	for i, in := range inputs {
		c, ok := s.components[in.ComponentID]
		if !ok {
			return domain.ErrComponentNotFound
		}
		set = append(set, domain.StructureComponent{
			StructureComponentID: fmt.Sprintf("%s-sc-%d", structureID, i),
			TenantID:             "tenant-abc",
			StructureID:          structureID,
			ComponentID:          in.ComponentID,
			CalculationMethod:    in.CalculationMethod,
			CalculationValue:     in.CalculationValue,
			Sequence:             in.Sequence,
			ComponentName:        c.Name,
			ComponentCode:        c.Code,
			ComponentType:        c.ComponentType,
			IsTaxable:            c.IsTaxable,
		})
	}
	// Replacement, not merge — same as the real store.
	s.composition[structureID] = set
	return nil
}

func (s *stubStore) ListStructureComponents(_ context.Context, structureID string) ([]domain.StructureComponent, error) {
	set := append([]domain.StructureComponent(nil), s.composition[structureID]...)
	sort.Slice(set, func(i, j int) bool {
		if set[i].Sequence != set[j].Sequence {
			return set[i].Sequence < set[j].Sequence
		}
		return set[i].ComponentCode < set[j].ComponentCode
	})
	return set, nil
}

// ── helpers ────────────────────────────────────────────────────────────────────

func seedStructure(t *testing.T, r chi.Router, correlationID string) domain.CompensationStructure {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/compensation/structures", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Eng Grade 5",
		"pay_type":        "SALARY",
		"min_amount":      80000.0,
		"max_amount":      130000.0,
		"currency":        "USD",
		"correlation_id":  correlationID,
	}, "comp-admin")
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("seed structure: got %d: %s", rr.Code, rr.Body.String())
	}
	var str domain.CompensationStructure
	_ = json.NewDecoder(rr.Body).Decode(&str)
	return str
}

func seedComponent(t *testing.T, r chi.Router, code, componentType string, taxable bool) domain.SalaryComponent {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/compensation/components", map[string]any{
		"legal_entity_id": "le-us",
		"name":            code + " component",
		"code":            code,
		"component_type":  componentType,
		"is_taxable":      taxable,
		"currency":        "USD",
	}, "comp-admin")
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed component %s: got %d: %s", code, rr.Code, rr.Body.String())
	}
	var c domain.SalaryComponent
	_ = json.NewDecoder(rr.Body).Decode(&c)
	return c
}

// ── component catalogue tests ──────────────────────────────────────────────────

func TestCreateComponent_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/compensation/components", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "House Rent Allowance",
		"code":            "HRA",
		"component_type":  "EARNING",
		"currency":        "USD",
	}, "comp-admin")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}

	var c domain.SalaryComponent
	_ = json.NewDecoder(rr.Body).Decode(&c)
	if c.Status != "ACTIVE" {
		t.Errorf("expected ACTIVE got %q", c.Status)
	}
	// Omitting is_taxable must not silently make pay tax-free.
	if !c.IsTaxable {
		t.Error("a component with is_taxable omitted must default to taxable")
	}
}

func TestCreateComponent_InvalidType_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/compensation/components", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Mystery",
		"code":            "MYST",
		"component_type":  "REIMBURSEMENT",
		"currency":        "USD",
	}, "comp-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateComponent_NegativeDefault_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/compensation/components", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "Negative",
		"code":            "NEG",
		"component_type":  "EARNING",
		"currency":        "USD",
		"default_amount":  -100.0,
	}, "comp-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateComponent_DuplicateCode_Returns409(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	seedComponent(t, r, "HRA", "EARNING", true)

	rr := doReq(r, http.MethodPost, "/v1/compensation/components", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "House Rent Allowance (again)",
		"code":            "HRA",
		"component_type":  "EARNING",
		"currency":        "USD",
	}, "comp-admin")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateComponent_AuthzDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/compensation/components", map[string]any{
		"legal_entity_id": "le-us",
		"name":            "House Rent Allowance",
		"code":            "HRA",
		"component_type":  "EARNING",
		"currency":        "USD",
	}, "comp-admin")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListComponents_FiltersByType(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	seedComponent(t, r, "HRA", "EARNING", true)
	seedComponent(t, r, "TRANSPORT", "EARNING", false)
	seedComponent(t, r, "PF", "DEDUCTION", false)

	rr := doReq(r, http.MethodGet, "/v1/compensation/components?legal_entity_id=le-us&component_type=DEDUCTION", nil, "comp-admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	var list []domain.SalaryComponent
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list) != 1 || list[0].Code != "PF" {
		t.Fatalf("expected only the PF deduction, got %+v", list)
	}
}

func TestListComponents_RequiresLegalEntity(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	rr := doReq(r, http.MethodGet, "/v1/compensation/components", nil, "comp-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeactivateComponent_HidesFromDefaultListing(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	c := seedComponent(t, r, "HRA", "EARNING", true)

	if rr := doReq(r, http.MethodDelete, "/v1/compensation/components/"+c.ComponentID, nil, "comp-admin"); rr.Code != http.StatusOK {
		t.Fatalf("deactivate: expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodGet, "/v1/compensation/components?legal_entity_id=le-us", nil, "comp-admin")
	var list []domain.SalaryComponent
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list) != 0 {
		t.Errorf("expected retired component to be hidden, got %d", len(list))
	}

	rrAll := doReq(r, http.MethodGet, "/v1/compensation/components?legal_entity_id=le-us&include_inactive=true", nil, "comp-admin")
	var all []domain.SalaryComponent
	_ = json.NewDecoder(rrAll.Body).Decode(&all)
	if len(all) != 1 || all[0].Status != "INACTIVE" {
		t.Errorf("expected the component to remain on record as INACTIVE, got %+v", all)
	}
}

// ── structure composition tests ────────────────────────────────────────────────

func TestSetStructureComponents_HappyPath(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	str := seedStructure(t, r, "corr-comp-1")
	hra := seedComponent(t, r, "HRA", "EARNING", true)
	pf := seedComponent(t, r, "PF", "DEDUCTION", false)

	rr := doReq(r, http.MethodPut, "/v1/compensation/structures/"+str.StructureID+"/components", map[string]any{
		"components": []map[string]any{
			{"component_id": hra.ComponentID, "calculation_method": "PERCENT_OF_BASE", "calculation_value": 40.0, "sequence": 1},
			{"component_id": pf.ComponentID, "calculation_method": "FIXED", "calculation_value": 1800.0, "sequence": 2},
		},
	}, "comp-admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	var list []domain.StructureComponent
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list) != 2 {
		t.Fatalf("expected 2 components, got %d", len(list))
	}
	if list[0].ComponentCode != "HRA" || list[1].ComponentCode != "PF" {
		t.Errorf("expected sequence order HRA then PF, got %s then %s", list[0].ComponentCode, list[1].ComponentCode)
	}
}

func TestSetStructureComponents_ReplacesRatherThanMerges(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	str := seedStructure(t, r, "corr-comp-2")
	hra := seedComponent(t, r, "HRA", "EARNING", true)
	transport := seedComponent(t, r, "TRANSPORT", "EARNING", false)

	path := "/v1/compensation/structures/" + str.StructureID + "/components"

	if rr := doReq(r, http.MethodPut, path, map[string]any{
		"components": []map[string]any{
			{"component_id": hra.ComponentID, "calculation_method": "PERCENT_OF_BASE", "calculation_value": 40.0},
		},
	}, "comp-admin"); rr.Code != http.StatusOK {
		t.Fatalf("first set: expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodPut, path, map[string]any{
		"components": []map[string]any{
			{"component_id": transport.ComponentID, "calculation_method": "FIXED", "calculation_value": 500.0},
		},
	}, "comp-admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("second set: expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	var list []domain.StructureComponent
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list) != 1 || list[0].ComponentCode != "TRANSPORT" {
		t.Fatalf("expected the composition to be replaced, got %+v", list)
	}
}

func TestSetStructureComponents_DuplicateComponent_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	str := seedStructure(t, r, "corr-comp-3")
	hra := seedComponent(t, r, "HRA", "EARNING", true)

	rr := doReq(r, http.MethodPut, "/v1/compensation/structures/"+str.StructureID+"/components", map[string]any{
		"components": []map[string]any{
			{"component_id": hra.ComponentID, "calculation_method": "FIXED", "calculation_value": 100.0},
			{"component_id": hra.ComponentID, "calculation_method": "FIXED", "calculation_value": 200.0},
		},
	}, "comp-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSetStructureComponents_InvalidMethodOrValue_Rejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		value  float64
	}{
		{"unknown method", "FORMULA", 10},
		{"negative value", "FIXED", -1},
		{"percent over 100", "PERCENT_OF_BASE", 101},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
			str := seedStructure(t, r, "corr-"+tc.name)
			hra := seedComponent(t, r, "HRA", "EARNING", true)

			rr := doReq(r, http.MethodPut, "/v1/compensation/structures/"+str.StructureID+"/components", map[string]any{
				"components": []map[string]any{
					{"component_id": hra.ComponentID, "calculation_method": tc.method, "calculation_value": tc.value},
				},
			}, "comp-admin")
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSetStructureComponents_UnknownStructure_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPut, "/v1/compensation/structures/no-such-structure/components", map[string]any{
		"components": []map[string]any{},
	}, "comp-admin")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 got %d: %s", rr.Code, rr.Body.String())
	}
}

// A structure must not borrow another legal entity's components.
func TestSetStructureComponents_CrossEntityComponent_Rejected(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	str := seedStructure(t, r, "corr-comp-cross")
	foreign := seedComponent(t, r, "HRA", "EARNING", true)
	// Move the component to a different entity behind the API.
	s.components[foreign.ComponentID].LegalEntityID = "le-in"

	rr := doReq(r, http.MethodPut, "/v1/compensation/structures/"+str.StructureID+"/components", map[string]any{
		"components": []map[string]any{
			{"component_id": foreign.ComponentID, "calculation_method": "FIXED", "calculation_value": 100.0},
		},
	}, "comp-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── breakdown tests ────────────────────────────────────────────────────────────

func TestGetStructureBreakdown_ComputesPayslipArithmetic(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	str := seedStructure(t, r, "corr-breakdown-1")
	hra := seedComponent(t, r, "HRA", "EARNING", true)
	transport := seedComponent(t, r, "TRANSPORT", "EARNING", false)
	pf := seedComponent(t, r, "PF", "DEDUCTION", false)

	if rr := doReq(r, http.MethodPut, "/v1/compensation/structures/"+str.StructureID+"/components", map[string]any{
		"components": []map[string]any{
			{"component_id": hra.ComponentID, "calculation_method": "PERCENT_OF_BASE", "calculation_value": 40.0, "sequence": 1},
			{"component_id": transport.ComponentID, "calculation_method": "FIXED", "calculation_value": 500.0, "sequence": 2},
			{"component_id": pf.ComponentID, "calculation_method": "PERCENT_OF_BASE", "calculation_value": 12.0, "sequence": 3},
		},
	}, "comp-admin"); rr.Code != http.StatusOK {
		t.Fatalf("set components: got %d: %s", rr.Code, rr.Body.String())
	}

	rr := doReq(r, http.MethodGet, "/v1/compensation/structures/"+str.StructureID+"/breakdown?base_amount=10000", nil, "comp-admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}

	var b domain.CompensationBreakdown
	if err := json.NewDecoder(rr.Body).Decode(&b); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// base 10000; HRA 40% = 4000; transport 500; PF 12% = 1200
	for _, c := range []struct {
		field string
		got   float64
		want  float64
	}{
		{"base_amount", b.BaseAmount, 10000},
		{"total_earnings", b.TotalEarnings, 4500},
		{"total_deductions", b.TotalDeductions, 1200},
		{"gross_earnings", b.GrossEarnings, 14500},
		{"net_amount", b.NetAmount, 13300},
		// Transport is non-taxable, so taxable pay is base + HRA only.
		{"taxable_amount", b.TaxableAmount, 14000},
	} {
		if c.got != c.want {
			t.Errorf("%s: expected %.2f got %.2f", c.field, c.want, c.got)
		}
	}

	if len(b.Lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(b.Lines))
	}
	if b.Lines[0].ComponentCode != "HRA" || b.Lines[0].Amount != 4000 {
		t.Errorf("first line should be HRA at 4000, got %s at %.2f", b.Lines[0].ComponentCode, b.Lines[0].Amount)
	}
}

// Percentages are taken against the base, never a running total, so the answer
// does not depend on the order components are stored in.
func TestComputeBreakdown_PercentagesAreOrderIndependent(t *testing.T) {
	structure := domain.CompensationStructure{
		StructureID: "s-1", Name: "Grade 5", Currency: "USD",
	}
	components := []domain.StructureComponent{
		{ComponentID: "c1", ComponentCode: "HRA", ComponentType: domain.ComponentEarning, IsTaxable: true,
			CalculationMethod: domain.MethodPercentOfBase, CalculationValue: 40, Sequence: 1},
		{ComponentID: "c2", ComponentCode: "SPECIAL", ComponentType: domain.ComponentEarning, IsTaxable: true,
			CalculationMethod: domain.MethodPercentOfBase, CalculationValue: 20, Sequence: 2},
	}

	forward := handler.ComputeBreakdown(structure, components, 10000)
	reversed := handler.ComputeBreakdown(structure, []domain.StructureComponent{components[1], components[0]}, 10000)

	if forward.GrossEarnings != reversed.GrossEarnings {
		t.Errorf("gross should not depend on component order: %.2f vs %.2f",
			forward.GrossEarnings, reversed.GrossEarnings)
	}
	if forward.GrossEarnings != 16000 {
		t.Errorf("expected 16000 got %.2f", forward.GrossEarnings)
	}
}

func TestComputeBreakdown_TaxableFloorsAtZero(t *testing.T) {
	structure := domain.CompensationStructure{StructureID: "s-1", Currency: "USD"}
	components := []domain.StructureComponent{
		{ComponentID: "c1", ComponentCode: "SACRIFICE", ComponentType: domain.ComponentDeduction, IsTaxable: true,
			CalculationMethod: domain.MethodFixed, CalculationValue: 5000},
	}

	b := handler.ComputeBreakdown(structure, components, 1000)
	if b.TaxableAmount != 0 {
		t.Errorf("taxable pay must not go negative, got %.2f", b.TaxableAmount)
	}
	// Net is allowed to go negative — that is a real, reportable state.
	if b.NetAmount != -4000 {
		t.Errorf("expected net -4000 got %.2f", b.NetAmount)
	}
}

func TestComputeBreakdown_EmptyComposition(t *testing.T) {
	structure := domain.CompensationStructure{StructureID: "s-1", Currency: "USD"}

	b := handler.ComputeBreakdown(structure, nil, 10000)
	if b.GrossEarnings != 10000 || b.NetAmount != 10000 {
		t.Errorf("a structure with no components should pass the base through, got gross %.2f net %.2f",
			b.GrossEarnings, b.NetAmount)
	}
	if b.Lines == nil {
		t.Error("lines should be an empty slice, not nil, so it serialises as []")
	}
}

func TestComputeBreakdown_RoundsToCents(t *testing.T) {
	structure := domain.CompensationStructure{StructureID: "s-1", Currency: "USD"}
	components := []domain.StructureComponent{
		{ComponentID: "c1", ComponentCode: "ODD", ComponentType: domain.ComponentEarning, IsTaxable: true,
			CalculationMethod: domain.MethodPercentOfBase, CalculationValue: 33.33},
	}

	// 33.33% of 1000.05 = 333.316665, which must round up to 333.32.
	b := handler.ComputeBreakdown(structure, components, 1000.05)
	if b.Lines[0].Amount != 333.32 {
		t.Errorf("expected 333.32 got %.4f", b.Lines[0].Amount)
	}
	// Totals must equal the sum of the visible lines.
	if b.GrossEarnings != round2Test(b.BaseAmount+b.Lines[0].Amount) {
		t.Errorf("gross %.4f should equal base + line %.4f", b.GrossEarnings, b.BaseAmount+b.Lines[0].Amount)
	}
}

func round2Test(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func TestGetStructureBreakdown_NegativeBase_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	str := seedStructure(t, r, "corr-breakdown-neg")

	rr := doReq(r, http.MethodGet, "/v1/compensation/structures/"+str.StructureID+"/breakdown?base_amount=-1", nil, "comp-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetStructureBreakdown_MissingBase_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	str := seedStructure(t, r, "corr-breakdown-missing")

	rr := doReq(r, http.MethodGet, "/v1/compensation/structures/"+str.StructureID+"/breakdown", nil, "comp-admin")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetStructureBreakdown_AuthzDenied_Returns403(t *testing.T) {
	s := newStubStore()
	allowAll := &stubAuthZ{}
	r := newRouter(s, &stubPublisher{}, allowAll, &stubEmployeeValidator{})
	str := seedStructure(t, r, "corr-breakdown-authz")

	denied := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubEmployeeValidator{})
	rr := doReq(denied, http.MethodGet, "/v1/compensation/structures/"+str.StructureID+"/breakdown?base_amount=100", nil, "comp-admin")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
}
