package handler

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

	"zoiko.io/compliance-status-svc/internal/authz"
	"zoiko.io/compliance-status-svc/internal/domain"
)

type mockStore struct {
	records map[string]*domain.ComplianceHealth
	gaps    map[string]*domain.ComplianceGap
}

func newMockStore() *mockStore {
	return &mockStore{
		records: make(map[string]*domain.ComplianceHealth),
		gaps:    make(map[string]*domain.ComplianceGap),
	}
}

func (m *mockStore) Evaluate(ctx context.Context, c *domain.ComplianceHealth) error {
	if c.StatusID == "" {
		c.StatusID = "cstat-test-01"
	}
	c.LastEvaluatedAt = time.Now().UTC()
	c.CreatedAt = time.Now().UTC()
	c.UpdatedAt = time.Now().UTC()
	c.CalculateHealthScore()
	m.records[c.StatusID] = c
	return nil
}

func (m *mockStore) GetByID(ctx context.Context, id string) (*domain.ComplianceHealth, error) {
	c, ok := m.records[id]
	if !ok {
		return nil, domain.ErrStatusRecordNotFound
	}
	return c, nil
}

func (m *mockStore) List(ctx context.Context, legalEntityID, jurisdictionID, domainName, status string) ([]domain.ComplianceHealth, error) {
	var out []domain.ComplianceHealth
	for _, item := range m.records {
		if legalEntityID != "" && item.LegalEntityID != legalEntityID {
			continue
		}
		if jurisdictionID != "" && item.JurisdictionID != jurisdictionID {
			continue
		}
		if domainName != "" && item.DomainName != domainName {
			continue
		}
		if status != "" && string(item.OverallStatus) != status {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

func (m *mockStore) CreateGap(ctx context.Context, g *domain.ComplianceGap) error {
	if g.GapID == "" {
		g.GapID = "cgap-test-01"
	}
	g.DetectedAt = time.Now().UTC()
	g.CreatedAt = time.Now().UTC()
	g.UpdatedAt = time.Now().UTC()
	if g.Status == "" {
		g.Status = domain.GapOpen
	}
	m.gaps[g.GapID] = g
	return nil
}

func (m *mockStore) ListGaps(ctx context.Context, legalEntityID, domainName, severity, status string) ([]domain.ComplianceGap, error) {
	var out []domain.ComplianceGap
	for _, item := range m.gaps {
		if legalEntityID != "" && item.LegalEntityID != legalEntityID {
			continue
		}
		if domainName != "" && item.DomainName != domainName {
			continue
		}
		if severity != "" && string(item.Severity) != severity {
			continue
		}
		if status != "" && string(item.Status) != status {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

func (m *mockStore) ResolveGap(ctx context.Context, id string, req *domain.ResolveGapRequest) (*domain.ComplianceGap, error) {
	g, ok := m.gaps[id]
	if !ok {
		return nil, domain.ErrGapNotFound
	}
	if g.Status == domain.GapResolved {
		return nil, domain.ErrGapAlreadyResolved
	}
	now := time.Now().UTC()
	g.Status = domain.GapResolved
	g.ResolvedAt = &now
	g.UpdatedAt = now
	return g, nil
}

type mockPublisher struct{}

func (p *mockPublisher) Publish(ctx context.Context, eventType, subjectID, tenantID string, payload interface{}) error {
	return nil
}

func setupTestRouter() (*chi.Mux, *mockStore) {
	st := newMockStore()
	pub := &mockPublisher{}
	az := authz.NewClient("http://localhost:8089")
	logger, _ := zap.NewDevelopment()
	h := New(st, pub, az, logger)

	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r, st
}

func TestEvaluateAndResolveComplianceGap(t *testing.T) {
	r, _ := setupTestRouter()

	evalReqPayload := domain.EvaluateComplianceRequest{
		LegalEntityID:        "entity-001",
		JurisdictionID:       "GB-UK",
		DomainName:           "TAX",
		TotalObligations:     10,
		FulfilledObligations: 9,
		PendingObligations:   1,
		OverdueObligations:   0,
		OpenExceptions:       0,
		EffectiveFrom:        "2026-01-01",
		CreatedBy:            "risk_officer",
	}
	body, _ := json.Marshal(evalReqPayload)

	req := httptest.NewRequest("POST", "/v1/compliance-status/evaluate", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var evalResult domain.ComplianceHealth
	if err := json.NewDecoder(w.Body).Decode(&evalResult); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if evalResult.HealthScore != 90.00 {
		t.Errorf("expected health score 90.00, got %f", evalResult.HealthScore)
	}
	if evalResult.OverallStatus != domain.StatusCompliant {
		t.Errorf("expected status COMPLIANT, got %s", evalResult.OverallStatus)
	}

	// Create Gap
	gapReqPayload := domain.CreateGapRequest{
		LegalEntityID:   "entity-001",
		JurisdictionID:  "GB-UK",
		DomainName:      "TAX",
		GapType:         "MISSING_EVIDENCE",
		Severity:        domain.SeverityHigh,
		SourceReference: "whto-2026-001",
		Description:     "Missing exemption certificate for cross-border WHT",
	}
	gapBody, _ := json.Marshal(gapReqPayload)

	gapReq := httptest.NewRequest("POST", "/v1/compliance-status/gaps", bytes.NewBuffer(gapBody))
	gapReq.Header.Set("Content-Type", "application/json")
	gapW := httptest.NewRecorder()

	r.ServeHTTP(gapW, gapReq)

	if gapW.Code != http.StatusCreated {
		t.Fatalf("expected status 201 on gap create, got %d", gapW.Code)
	}

	var createdGap domain.ComplianceGap
	if err := json.NewDecoder(gapW.Body).Decode(&createdGap); err != nil {
		t.Fatalf("failed to decode gap response: %v", err)
	}

	// Resolve Gap
	resReqPayload := domain.ResolveGapRequest{RemediationNotes: "Certificate provided by counterparty"}
	resBody, _ := json.Marshal(resReqPayload)

	resReq := httptest.NewRequest("POST", "/v1/compliance-status/gaps/"+createdGap.GapID+"/resolve", bytes.NewBuffer(resBody))
	resReq.Header.Set("Content-Type", "application/json")
	resW := httptest.NewRecorder()

	r.ServeHTTP(resW, resReq)

	if resW.Code != http.StatusOK {
		t.Fatalf("expected status 200 on gap resolve, got %d", resW.Code)
	}

	var resolvedGap domain.ComplianceGap
	if err := json.NewDecoder(resW.Body).Decode(&resolvedGap); err != nil {
		t.Fatalf("failed to decode resolved gap: %v", err)
	}
	if resolvedGap.Status != domain.GapResolved {
		t.Errorf("expected gap status RESOLVED, got %s", resolvedGap.Status)
	}
}
