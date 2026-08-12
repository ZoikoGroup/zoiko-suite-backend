package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-determination-svc/internal/domain"
	"zoiko.io/tax-determination-svc/internal/events"
	"zoiko.io/tax-determination-svc/internal/rules"
)

type stubStore struct {
	determinations map[string]*domain.TaxDetermination
}

func newStubStore() *stubStore {
	return &stubStore{
		determinations: make(map[string]*domain.TaxDetermination),
	}
}

func (s *stubStore) CreateDetermination(_ context.Context, d *domain.TaxDetermination) error {
	if d.DeterminationID == "" {
		d.DeterminationID = "tdet-test-001"
	}
	if d.Status == "" {
		d.Status = domain.StatusCalculated
	}
	s.determinations[d.DeterminationID] = d
	return nil
}

func (s *stubStore) GetDetermination(_ context.Context, id string) (*domain.TaxDetermination, error) {
	if d, ok := s.determinations[id]; ok {
		return d, nil
	}
	return nil, domain.ErrTaxDeterminationNotFound
}

func (s *stubStore) ListDeterminations(_ context.Context, _, _, _ string) ([]domain.TaxDetermination, error) {
	var out []domain.TaxDetermination
	for _, d := range s.determinations {
		out = append(out, *d)
	}
	return out, nil
}

func (s *stubStore) UpdateDetermination(_ context.Context, d *domain.TaxDetermination) error {
	s.determinations[d.DeterminationID] = d
	return nil
}

func (s *stubStore) OverrideDetermination(_ context.Context, id string, req *domain.OverrideTaxRequest) (*domain.TaxDetermination, error) {
	d, ok := s.determinations[id]
	if !ok {
		return nil, domain.ErrTaxDeterminationNotFound
	}
	if d.Status == domain.StatusOverridden {
		return nil, domain.ErrAlreadyOverridden
	}
	d.Status = domain.StatusOverridden
	d.CalculatedTaxAmount = req.OverriddenTaxAmount
	return d, nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// stubAuthz is a stand-in for authz.Client in tests: it never makes a real
// HTTP call, and lets tests control the grant/deny/error outcome.
type stubAuthz struct {
	err error
}

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	return s.err
}

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	rulesClient := rules.NewClient("http://localhost:8125")
	return New(newStubStore(), &stubPublisher{}, &stubAuthz{}, rulesClient, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
	r.Header.Set("X-Principal-Id", "user-test-01")
	return r
}

func TestDetermineTax(t *testing.T) {
	h := newTestHandler()
	body := domain.DetermineTaxRequest{
		TransactionID:  "tx-inv-1001",
		SourceModule:   "INVOICE",
		LegalEntityID:  "le-001",
		JurisdictionID: "uk-england",
		TaxCategory:    "VAT",
		GrossAmount:    1000.0,
		ExemptAmount:   0.0,
		Currency:       "GBP",
		EffectiveFrom:  "2026-01-01",
		EvaluatedBy:    "billing-engine",
	}
	w := httptest.NewRecorder()
	h.DetermineTax(w, buildRequest(http.MethodPost, "/v1/tax-determinations", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.TaxDetermination
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.TransactionID != "tx-inv-1001" {
		t.Errorf("unexpected transaction ID: %s", resp.TransactionID)
	}
	if resp.Status != domain.StatusCalculated {
		t.Errorf("expected CALCULATED, got %s", resp.Status)
	}
}

// TestDetermineTax_FallbackRuleHasNoSnapshot proves the zero-tax fallback
// (tax-rules-svc unreachable, exercised here since newTestHandler points at
// a non-listening address) is never given a fabricated snapshot reference —
// there is no real rule content behind it to snapshot.
func TestDetermineTax_FallbackRuleHasNoSnapshot(t *testing.T) {
	h := newTestHandler()
	body := domain.DetermineTaxRequest{
		TransactionID: "tx-inv-1002", SourceModule: "INVOICE", LegalEntityID: "le-001",
		JurisdictionID: "uk-england", TaxCategory: "VAT", GrossAmount: 1000.0,
		Currency: "GBP", EffectiveFrom: "2026-01-01", EvaluatedBy: "billing-engine",
	}
	w := httptest.NewRecorder()
	h.DetermineTax(w, buildRequest(http.MethodPost, "/v1/tax-determinations", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.TaxDetermination
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.TaxLogicSnapshotID != nil {
		t.Errorf("expected nil tax_logic_snapshot_id for the fallback rule, got %v", *resp.TaxLogicSnapshotID)
	}
}

// TestSnapshotTaxRule proves the snapshot is a real, deterministic,
// content-sensitive reference — not a fabricated or constant placeholder.
func TestSnapshotTaxRule(t *testing.T) {
	a := &rules.TaxRuleDTO{RuleID: "trule-1", RuleCode: "UK-VAT-STD", Category: "VAT", TaxRatePercentage: 20.0, Status: "ACTIVE"}
	b := &rules.TaxRuleDTO{RuleID: "trule-1", RuleCode: "UK-VAT-STD", Category: "VAT", TaxRatePercentage: 20.0, Status: "ACTIVE"}
	c := &rules.TaxRuleDTO{RuleID: "trule-1", RuleCode: "UK-VAT-STD", Category: "VAT", TaxRatePercentage: 5.0, Status: "ACTIVE"} // rate later edited

	snapA, snapB, snapC := snapshotTaxRule(a), snapshotTaxRule(b), snapshotTaxRule(c)
	if snapA == nil || snapB == nil || snapC == nil {
		t.Fatal("expected non-nil snapshots for real rule content")
	}
	if *snapA != *snapB {
		t.Errorf("expected identical rule content to produce identical snapshots, got %s vs %s", *snapA, *snapB)
	}
	if *snapA == *snapC {
		t.Errorf("expected a changed tax rate to change the snapshot — it did not, defeating the whole point")
	}
	if snapshotTaxRule(&rules.TaxRuleDTO{RuleID: "trule-fallback"}) != nil {
		t.Error("expected nil snapshot for the fallback sentinel rule")
	}
}

func TestOverrideDetermination(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Determine tax first
	body := domain.DetermineTaxRequest{
		TransactionID:  "tx-po-500",
		SourceModule:   "PURCHASE_ORDER",
		LegalEntityID:  "le-001",
		JurisdictionID: "us-california",
		TaxCategory:    "SALES_TAX",
		GrossAmount:    500.0,
		Currency:       "USD",
		EffectiveFrom:  "2026-01-01",
		EvaluatedBy:    "po-engine",
	}
	wDet := httptest.NewRecorder()
	r.ServeHTTP(wDet, buildRequest(http.MethodPost, "/v1/tax-determinations", body))
	var created domain.TaxDetermination
	_ = json.NewDecoder(wDet.Body).Decode(&created)

	// Override tax
	ovrBody := domain.OverrideTaxRequest{
		OverriddenTaxAmount: 25.0,
		Reason:              "Tax exemption certificate verified manually",
		UpdatedBy:           "tax-auditor",
	}
	wOvr := httptest.NewRecorder()
	r.ServeHTTP(wOvr, buildRequest(http.MethodPost, "/v1/tax-determinations/"+created.DeterminationID+"/override", ovrBody))
	if wOvr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wOvr.Code, wOvr.Body.String())
	}
	var updated domain.TaxDetermination
	_ = json.NewDecoder(wOvr.Body).Decode(&updated)
	if updated.CalculatedTaxAmount != 25.0 {
		t.Errorf("expected tax 25.0, got %f", updated.CalculatedTaxAmount)
	}
	if updated.Status != domain.StatusOverridden {
		t.Errorf("expected OVERRIDDEN, got %s", updated.Status)
	}
}
