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

	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/events"
)

// mockStore implements the Store interface for testing
type mockStore struct {
	statuses map[string]*domain.ComplianceStatus
}

func newMockStore() *mockStore {
	return &mockStore{
		statuses: make(map[string]*domain.ComplianceStatus),
	}
}

func (m *mockStore) CreateComplianceStatus(ctx context.Context, params domain.CreateComplianceStatusParams) (*domain.ComplianceStatus, error) {
	id := params.ComplianceStatusID
	if id == "" {
		id = "cstat-test-01"
	}
	now := time.Now().UTC()
	cs := &domain.ComplianceStatus{
		ComplianceStatusID:       id,
		TenantID:                 params.TenantID,
		LegalEntityID:            params.LegalEntityID,
		DomainCode:               params.DomainCode,
		StatusCode:               params.StatusCode,
		StatusReason:             params.StatusReason,
		EvidenceSufficiencyState: params.EvidenceSufficiencyState,
		EvaluatedAt:              params.EvaluatedAt,
		CreatedAt:                now,
		CreatedByPrincipalID:     params.CreatedByPrincipalID,
		UpdatedAt:                now,
		Version:                  1,
	}
	m.statuses[id] = cs
	return cs, nil
}

func (m *mockStore) FindComplianceStatusByID(ctx context.Context, complianceStatusID string) (*domain.ComplianceStatus, error) {
	cs, ok := m.statuses[complianceStatusID]
	if !ok {
		return nil, domain.ErrComplianceStatusNotFound
	}
	return cs, nil
}

func (m *mockStore) ListComplianceStatuses(ctx context.Context, filter domain.ListComplianceStatusFilter) ([]*domain.ComplianceStatus, error) {
	var out []*domain.ComplianceStatus
	for _, item := range m.statuses {
		if filter.LegalEntityID != "" && item.LegalEntityID != filter.LegalEntityID {
			continue
		}
		if filter.DomainCode != "" && item.DomainCode != filter.DomainCode {
			continue
		}
		if filter.StatusCode != "" && item.StatusCode != filter.StatusCode {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (m *mockStore) UpdateComplianceStatus(ctx context.Context, complianceStatusID string, params domain.UpdateComplianceStatusParams, actorID, correlationID string) (*domain.ComplianceStatus, error) {
	cs, ok := m.statuses[complianceStatusID]
	if !ok {
		return nil, domain.ErrComplianceStatusNotFound
	}
	if params.ExpectedVersion > 0 && cs.Version != params.ExpectedVersion {
		return nil, domain.ErrVersionMismatch
	}
	cs.StatusCode = params.StatusCode
	cs.StatusReason = params.StatusReason
	cs.EvidenceSufficiencyState = params.EvidenceSufficiencyState
	cs.UpdatedAt = time.Now().UTC()
	cs.Version++
	return cs, nil
}

type mockPublisher struct{}

func (p *mockPublisher) Publish(ctx context.Context, params events.PublishParams) error {
	return nil
}

type mockAuthzClient struct {
	err error
}

func (m *mockAuthzClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	return m.err
}

func setupTestRouter() (*chi.Mux, *mockStore) {
	st := newMockStore()
	pub := &mockPublisher{}
	az := &mockAuthzClient{}
	logger, _ := zap.NewDevelopment()
	h := New(st, pub, az, logger)

	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r, st
}

func TestCreateAndGetComplianceStatus(t *testing.T) {
	r, _ := setupTestRouter()

	reqPayload := map[string]interface{}{
		"legal_entity_id":             "entity-001",
		"domain_code":                 "TAX",
		"status_code":                 "COMPLIANT",
		"status_reason":               "All obligations fulfilled on time",
		"evidence_sufficiency_state":  "SUFFICIENT",
		"evaluated_at":                time.Now().UTC().Format(time.RFC3339),
		"created_by_principal_id":     "principal-test-01",
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/compliance-status/", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req.Header.Set("X-Tenant-Id", "tenant-001")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", w.Code)
	}

	var result domain.ComplianceStatus
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result.ComplianceStatusID == "" {
		t.Error("expected compliance_status_id to be set")
	}
	if result.LegalEntityID != "entity-001" {
		t.Errorf("expected legal_entity_id entity-001, got %s", result.LegalEntityID)
	}
	if result.DomainCode != "TAX" {
		t.Errorf("expected domain_code TAX, got %s", result.DomainCode)
	}
	if result.StatusCode != "COMPLIANT" {
		t.Errorf("expected status_code COMPLIANT, got %s", result.StatusCode)
	}

	// Get by ID
	getReq := httptest.NewRequest("GET", "/v1/compliance-status/"+result.ComplianceStatusID, nil)
	getReq.Header.Set("X-Principal-Id", "principal-test-01")
	getReq.Header.Set("X-Tenant-Id", "tenant-001")
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Errorf("expected status 200 on GetByID, got %d", getW.Code)
	}

	var getResult domain.ComplianceStatus
	_ = json.NewDecoder(getW.Body).Decode(&getResult)
	if getResult.ComplianceStatusID != result.ComplianceStatusID {
		t.Error("mismatch in GetByID result")
	}
}

func TestUpdateComplianceStatus(t *testing.T) {
	r, st := setupTestRouter()

	// Create initial status
	now := time.Now().UTC()
	cs := &domain.ComplianceStatus{
		ComplianceStatusID:       "cstat-update-01",
		TenantID:                 "tenant-001",
		LegalEntityID:            "entity-001",
		DomainCode:               "TAX",
		StatusCode:               "COMPLIANT",
		StatusReason:             "All obligations fulfilled",
		EvidenceSufficiencyState: "SUFFICIENT",
		EvaluatedAt:              now,
		CreatedAt:                now,
		CreatedByPrincipalID:     "risk_officer",
		UpdatedAt:                now,
		Version:                  1,
	}
	st.statuses[cs.ComplianceStatusID] = cs

	// Update with correct version
	updateReq := map[string]interface{}{
		"status_code":                "AT_RISK",
		"status_reason":              "One obligation overdue",
		"evidence_sufficiency_state": "PENDING",
		"expected_version":           1,
	}
	body, _ := json.Marshal(updateReq)

	req := httptest.NewRequest("POST", "/v1/compliance-status/"+cs.ComplianceStatusID, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req.Header.Set("X-Tenant-Id", "tenant-001")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result domain.ComplianceStatus
	_ = json.NewDecoder(w.Body).Decode(&result)
	if result.StatusCode != "AT_RISK" {
		t.Errorf("expected status_code AT_RISK, got %s", result.StatusCode)
	}
	if result.Version != 2 {
		t.Errorf("expected version 2, got %d", result.Version)
	}
}

func TestListComplianceStatuses(t *testing.T) {
	r, _ := setupTestRouter()

	// Create two statuses
	now := time.Now().UTC()
	cs1 := &domain.ComplianceStatus{
		ComplianceStatusID:       "cstat-list-01",
		TenantID:                 "tenant-001",
		LegalEntityID:            "entity-101",
		DomainCode:               "FINANCE",
		StatusCode:               "COMPLIANT",
		StatusReason:             "All good",
		EvidenceSufficiencyState: "SUFFICIENT",
		EvaluatedAt:              now,
		CreatedAt:                now,
		CreatedByPrincipalID:     "auditor",
		UpdatedAt:                now,
		Version:                  1,
	}
	cs2 := &domain.ComplianceStatus{
		ComplianceStatusID:       "cstat-list-02",
		TenantID:                 "tenant-001",
		LegalEntityID:            "entity-101",
		DomainCode:               "TAX",
		StatusCode:               "AT_RISK",
		StatusReason:             "Overdue",
		EvidenceSufficiencyState: "INSUFFICIENT",
		EvaluatedAt:              now,
		CreatedAt:                now,
		CreatedByPrincipalID:     "auditor",
		UpdatedAt:                now,
		Version:                  1,
	}

	reqPayload1 := map[string]interface{}{
		"legal_entity_id":             cs1.LegalEntityID,
		"domain_code":                 string(cs1.DomainCode),
		"status_code":                 string(cs1.StatusCode),
		"status_reason":               cs1.StatusReason,
		"evidence_sufficiency_state":  string(cs1.EvidenceSufficiencyState),
		"evaluated_at":                cs1.EvaluatedAt.Format(time.RFC3339),
		"created_by_principal_id":     cs1.CreatedByPrincipalID,
	}
	body1, _ := json.Marshal(reqPayload1)
	req1 := httptest.NewRequest("POST", "/v1/compliance-status/", bytes.NewBuffer(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Principal-Id", "principal-test-01")
	req1.Header.Set("X-Tenant-Id", "tenant-001")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	reqPayload2 := map[string]interface{}{
		"legal_entity_id":             cs2.LegalEntityID,
		"domain_code":                 string(cs2.DomainCode),
		"status_code":                 string(cs2.StatusCode),
		"status_reason":               cs2.StatusReason,
		"evidence_sufficiency_state":  string(cs2.EvidenceSufficiencyState),
		"evaluated_at":                cs2.EvaluatedAt.Format(time.RFC3339),
		"created_by_principal_id":     cs2.CreatedByPrincipalID,
	}
	body2, _ := json.Marshal(reqPayload2)
	req2 := httptest.NewRequest("POST", "/v1/compliance-status/", bytes.NewBuffer(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Principal-Id", "principal-test-01")
	req2.Header.Set("X-Tenant-Id", "tenant-001")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	// List for entity-101
	listReq := httptest.NewRequest("GET", "/v1/compliance-status/?legal_entity_id=entity-101", nil)
	listReq.Header.Set("X-Principal-Id", "principal-test-01")
	listReq.Header.Set("X-Tenant-Id", "tenant-001")
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200 on List, got %d", listW.Code)
	}

	var listResp map[string]interface{}
	_ = json.NewDecoder(listW.Body).Decode(&listResp)
	records := listResp["compliance_statuses"]
	if records == nil {
		t.Error("expected compliance_statuses in response")
	}
}

func TestValidationErrors(t *testing.T) {
	r, _ := setupTestRouter()

	testCases := []struct {
		name       string
		payload    map[string]interface{}
		wantStatus int
	}{
		{
			name: "missing legal_entity_id",
			payload: map[string]interface{}{
				"domain_code":                "TAX",
				"status_code":                "COMPLIANT",
				"status_reason":              "test",
				"evidence_sufficiency_state": "SUFFICIENT",
				"evaluated_at":               time.Now().UTC().Format(time.RFC3339),
				"created_by_principal_id":    "test",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid domain_code",
			payload: map[string]interface{}{
				"legal_entity_id":            "entity-001",
				"domain_code":                "INVALID",
				"status_code":                "COMPLIANT",
				"status_reason":              "test",
				"evidence_sufficiency_state": "SUFFICIENT",
				"evaluated_at":               time.Now().UTC().Format(time.RFC3339),
				"created_by_principal_id":    "test",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid status_code",
			payload: map[string]interface{}{
				"legal_entity_id":            "entity-001",
				"domain_code":                "TAX",
				"status_code":                "INVALID",
				"status_reason":              "test",
				"evidence_sufficiency_state": "SUFFICIENT",
				"evaluated_at":               time.Now().UTC().Format(time.RFC3339),
				"created_by_principal_id":    "test",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid evidence_sufficiency_state",
			payload: map[string]interface{}{
				"legal_entity_id":             "entity-001",
				"domain_code":                 "TAX",
				"status_code":                 "COMPLIANT",
				"status_reason":               "test",
				"evidence_sufficiency_state":  "INVALID",
				"evaluated_at":                time.Now().UTC().Format(time.RFC3339),
				"created_by_principal_id":     "test",
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.payload)
			req := httptest.NewRequest("POST", "/v1/compliance-status/", bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Principal-Id", "principal-test-01")
			req.Header.Set("X-Tenant-Id", "tenant-001")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("expected status %d, got %d", tc.wantStatus, w.Code)
			}
		})
	}
}