package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"zoiko.io/compliance-risk-scoring-svc/internal/authz"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	"zoiko.io/compliance-risk-scoring-svc/internal/events"
	"zoiko.io/compliance-risk-scoring-svc/internal/handler"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

func setupTestRouter() http.Handler {
	logger := zap.NewNop()
	memStore := store.NewMemoryStore()
	publisher := events.NewPublisher([]string{"localhost:9092"}, "zoiko.compliance-risk-scoring.events", logger)
	authzClient := authz.NewClient("http://localhost:8089", logger)
	h := handler.NewHandler(memStore, publisher, authzClient, logger)

	return handler.NewRouter(h)
}

func TestHealthCheck(t *testing.T) {
	router := setupTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal health response: %v", err)
	}

	if resp["status"] != "ok" || resp["service"] != "compliance-risk-scoring-svc" {
		t.Fatalf("unexpected response body: %v", resp)
	}
}

func TestCalculateAndLifecycleRiskScore(t *testing.T) {
	router := setupTestRouter()

	// 1. Calculate Risk Score
	calcReq := domain.CalculateRiskScoreRequest{
		LegalEntityID:         "LE-2002",
		AssessmentName:        "Q3 Organization Risk Review",
		OpenObligationsCount:  5,
		PolicyViolationsCount: 2,
		AuditExceptionsCount:  3,
		PrivacyIncidentsCount: 1,
		TaxPenaltiesCount:     1,
	}

	body, _ := json.Marshal(calcReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/risk-scores/calculate", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-risk-99")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var createdAssessment domain.RiskScoreAssessment
	if err := json.Unmarshal(rec.Body.Bytes(), &createdAssessment); err != nil {
		t.Fatalf("failed to unmarshal created assessment: %v", err)
	}

	if createdAssessment.ID == "" {
		t.Fatalf("expected non-empty assessment ID")
	}

	if createdAssessment.CompositeRiskScore <= 0 {
		t.Fatalf("expected positive composite risk score, got %f", createdAssessment.CompositeRiskScore)
	}

	if len(createdAssessment.FactorBreakdowns) != 5 {
		t.Fatalf("expected 5 factor breakdowns, got %d", len(createdAssessment.FactorBreakdowns))
	}

	// 2. Get Assessment by ID
	getReq := httptest.NewRequest(http.MethodGet, "/v1/risk-scores/"+createdAssessment.ID, nil)
	getReq.Header.Set("X-Tenant-ID", "tenant-risk-99")
	getRec := httptest.NewRecorder()

	router.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", getRec.Code)
	}

	// 3. List Assessments
	listReq := httptest.NewRequest(http.MethodGet, "/v1/risk-scores?legal_entity_id=LE-2002", nil)
	listReq.Header.Set("X-Tenant-ID", "tenant-risk-99")
	listRec := httptest.NewRecorder()

	router.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", listRec.Code)
	}

	// 4. Create Threshold Rule
	ruleReq := domain.RiskThresholdRule{
		RuleName:            "High Policy Breach Threshold",
		RiskCategory:        domain.CategoryPolicyViolations,
		HighThreshold:       50.0,
		CriticalThreshold:   75.0,
		NotificationChannel: "COMPLIANCE_OPS_SLACK",
	}
	ruleBody, _ := json.Marshal(ruleReq)
	ruleHTTPReq := httptest.NewRequest(http.MethodPost, "/v1/risk-scores/thresholds", bytes.NewBuffer(ruleBody))
	ruleHTTPReq.Header.Set("X-Tenant-ID", "tenant-risk-99")
	ruleHTTPReq.Header.Set("Content-Type", "application/json")
	ruleRec := httptest.NewRecorder()

	router.ServeHTTP(ruleRec, ruleHTTPReq)

	if ruleRec.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created on threshold rule, got %d", ruleRec.Code)
	}

	// 5. List Threshold Rules
	listRuleReq := httptest.NewRequest(http.MethodGet, "/v1/risk-scores/thresholds", nil)
	listRuleReq.Header.Set("X-Tenant-ID", "tenant-risk-99")
	listRuleRec := httptest.NewRecorder()

	router.ServeHTTP(listRuleRec, listRuleReq)

	if listRuleRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on list thresholds, got %d", listRuleRec.Code)
	}

	// 6. Archive Assessment
	delReq := httptest.NewRequest(http.MethodDelete, "/v1/risk-scores/"+createdAssessment.ID, nil)
	delReq.Header.Set("X-Tenant-ID", "tenant-risk-99")
	delRec := httptest.NewRecorder()

	router.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK on archive, got %d", delRec.Code)
	}
}

func TestValidationErrors(t *testing.T) {
	router := setupTestRouter()

	invalidReq := domain.CalculateRiskScoreRequest{
		LegalEntityID: "", // Missing
	}

	body, _ := json.Marshal(invalidReq)
	req := httptest.NewRequest(http.MethodPost, "/v1/risk-scores/calculate", bytes.NewBuffer(body))
	req.Header.Set("X-Tenant-ID", "tenant-risk-99")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 Bad Request, got %d", rec.Code)
	}
}
