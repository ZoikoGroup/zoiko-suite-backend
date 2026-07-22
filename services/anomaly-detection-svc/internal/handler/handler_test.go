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

	"zoiko.io/anomaly-detection-svc/internal/authz"
	"zoiko.io/anomaly-detection-svc/internal/domain"
)

type mockStore struct {
	records map[string]*domain.AnomalyRecord
	rules   map[string]*domain.AnomalyRule
}

func newMockStore() *mockStore {
	return &mockStore{
		records: make(map[string]*domain.AnomalyRecord),
		rules:   make(map[string]*domain.AnomalyRule),
	}
}

func (m *mockStore) Detect(ctx context.Context, rec *domain.AnomalyRecord) error {
	if rec.AnomalyID == "" {
		rec.AnomalyID = "anom-test-01"
	}
	rec.DetectedAt = time.Now().UTC()
	rec.CreatedAt = time.Now().UTC()
	rec.UpdatedAt = time.Now().UTC()
	m.records[rec.AnomalyID] = rec
	return nil
}

func (m *mockStore) GetByID(ctx context.Context, id string) (*domain.AnomalyRecord, error) {
	rec, ok := m.records[id]
	if !ok {
		return nil, domain.ErrAnomalyRecordNotFound
	}
	return rec, nil
}

func (m *mockStore) ListAnomalies(ctx context.Context, legalEntityID, domainName, severity, status string) ([]domain.AnomalyRecord, error) {
	var out []domain.AnomalyRecord
	for _, rec := range m.records {
		if legalEntityID != "" && rec.LegalEntityID != legalEntityID {
			continue
		}
		if domainName != "" && rec.DomainName != domainName {
			continue
		}
		if severity != "" && string(rec.Severity) != severity {
			continue
		}
		if status != "" && string(rec.Status) != status {
			continue
		}
		out = append(out, *rec)
	}
	return out, nil
}

func (m *mockStore) UpdateStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.AnomalyRecord, error) {
	rec, ok := m.records[id]
	if !ok {
		return nil, domain.ErrAnomalyRecordNotFound
	}
	now := time.Now().UTC()
	rec.Status = req.Status
	rec.InvestigatedBy = req.InvestigatedBy
	rec.InvestigatedAt = &now
	rec.ResolutionNotes = req.ResolutionNotes
	rec.UpdatedAt = now
	return rec, nil
}

func (m *mockStore) CreateRule(ctx context.Context, rule *domain.AnomalyRule) error {
	if rule.RuleID == "" {
		rule.RuleID = "arule-test-01"
	}
	rule.CreatedAt = time.Now().UTC()
	rule.UpdatedAt = time.Now().UTC()
	rule.IsActive = true
	m.rules[rule.RuleID] = rule
	return nil
}

func (m *mockStore) ListRules(ctx context.Context, domainName string) ([]domain.AnomalyRule, error) {
	var out []domain.AnomalyRule
	for _, r := range m.rules {
		if domainName != "" && r.DomainName != domainName {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
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

func TestDetectAndUpdateAnomalyStatus(t *testing.T) {
	r, _ := setupTestRouter()

	// 1. Detect Anomaly
	reqPayload := domain.DetectAnomalyRequest{
		LegalEntityID:  "entity-001",
		DomainName:     "FINANCE",
		SourceEntityID: "tx-9901-suspicious",
		MetricType:     "transaction_amount",
		ObservedValue:  500000.00,
		ExpectedValue:  15000.00,
		StdDeviation:   5000.00,
		Description:    "High variance transaction amount detected",
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/anomalies/detect", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201 on anomaly detect, got %d", w.Code)
	}

	var createdRec domain.AnomalyRecord
	if err := json.NewDecoder(w.Body).Decode(&createdRec); err != nil {
		t.Fatalf("failed to decode created anomaly record: %v", err)
	}
	if createdRec.Severity != domain.SeverityCritical {
		t.Errorf("expected severity CRITICAL, got %s", createdRec.Severity)
	}
	if createdRec.Status != domain.StatusOpen {
		t.Errorf("expected status OPEN, got %s", createdRec.Status)
	}

	// 2. Update Status to UNDER_INVESTIGATION
	upPayload := domain.UpdateStatusRequest{
		Status:          domain.StatusUnderInvestigation,
		InvestigatedBy:  "sec_analyst_01",
		ResolutionNotes: "Reviewing wire transfer velocity logs",
	}
	upBody, _ := json.Marshal(upPayload)

	upReq := httptest.NewRequest("POST", "/v1/anomalies/"+createdRec.AnomalyID+"/status", bytes.NewBuffer(upBody))
	upReq.Header.Set("Content-Type", "application/json")
	upW := httptest.NewRecorder()

	r.ServeHTTP(upW, upReq)

	if upW.Code != http.StatusOK {
		t.Fatalf("expected status 200 on status update, got %d", upW.Code)
	}

	var updatedRec domain.AnomalyRecord
	if err := json.NewDecoder(upW.Body).Decode(&updatedRec); err != nil {
		t.Fatalf("failed to decode updated anomaly record: %v", err)
	}
	if updatedRec.Status != domain.StatusUnderInvestigation {
		t.Errorf("expected status UNDER_INVESTIGATION, got %s", updatedRec.Status)
	}

	// 3. Get By ID
	getReq := httptest.NewRequest("GET", "/v1/anomalies/"+createdRec.AnomalyID, nil)
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Errorf("expected status 200 on GetByID, got %d", getW.Code)
	}

	// 4. List Anomalies
	listReq := httptest.NewRequest("GET", "/v1/anomalies?legal_entity_id=entity-001", nil)
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200 on ListAnomalies, got %d", listW.Code)
	}
}

func TestCreateAndListAnomalyRules(t *testing.T) {
	r, _ := setupTestRouter()

	rulePayload := domain.CreateRuleRequest{
		RuleName:       "High Payroll Variance Limit",
		DomainName:     "WORKFORCE",
		MetricType:     "gross_pay_variance",
		ThresholdValue: 25.00,
		ZScoreCutoff:   3.50,
	}
	body, _ := json.Marshal(rulePayload)

	req := httptest.NewRequest("POST", "/v1/anomalies/rules", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201 on create rule, got %d", w.Code)
	}

	var rule domain.AnomalyRule
	if err := json.NewDecoder(w.Body).Decode(&rule); err != nil {
		t.Fatalf("failed to decode rule: %v", err)
	}
	if rule.ZScoreCutoff != 3.50 {
		t.Errorf("expected ZScoreCutoff 3.50, got %f", rule.ZScoreCutoff)
	}

	// List Rules
	listReq := httptest.NewRequest("GET", "/v1/anomalies/rules?domain_name=WORKFORCE", nil)
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200 on ListRules, got %d", listW.Code)
	}
}
