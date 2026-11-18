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

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/events"
)

type mockAuthzClient struct {
	err error
}

func (a *mockAuthzClient) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	return a.err
}

type mockStore struct {
	cases       map[string]*domain.ExceptionCase
	escalations map[string]*domain.EscalationRecord
}

func newMockStore() *mockStore {
	return &mockStore{
		cases:       make(map[string]*domain.ExceptionCase),
		escalations: make(map[string]*domain.EscalationRecord),
	}
}

func (m *mockStore) CreateExceptionCase(ctx context.Context, params domain.CreateExceptionCaseParams) (*domain.ExceptionCase, error) {
	id := params.ExceptionCaseID
	if id == "" {
		id = "excase-test-01"
	}
	now := time.Now().UTC()
	c := &domain.ExceptionCase{
		ExceptionCaseID:        id,
		TenantID:               params.TenantID,
		LegalEntityID:          params.LegalEntityID,
		ExceptionType:          params.ExceptionType,
		SeverityLevel:          params.SeverityLevel,
		LinkedObjectType:       params.LinkedObjectType,
		LinkedObjectID:         params.LinkedObjectID,
		CaseStatus:             domain.CaseStatusOpen,
		CreatedAt:              now,
		CreatedByPrincipalID:   params.CreatedByPrincipalID,
		UpdatedAt:              now,
		Version:                1,
	}
	m.cases[id] = c
	return c, nil
}

func (m *mockStore) FindExceptionCaseByID(ctx context.Context, exceptionCaseID string) (*domain.ExceptionCase, error) {
	c, ok := m.cases[exceptionCaseID]
	if !ok {
		return nil, domain.ErrExceptionCaseNotFound
	}
	return c, nil
}

func (m *mockStore) ListExceptionCases(ctx context.Context, filter domain.ListExceptionCasesFilter) ([]*domain.ExceptionCase, error) {
	var out []*domain.ExceptionCase
	for _, item := range m.cases {
		if filter.LegalEntityID != "" && item.LegalEntityID != filter.LegalEntityID {
			continue
		}
		if filter.ExceptionType != "" && item.ExceptionType != filter.ExceptionType {
			continue
		}
		if filter.CaseStatus != "" && item.CaseStatus != filter.CaseStatus {
			continue
		}
		if filter.SeverityLevel != "" && item.SeverityLevel != filter.SeverityLevel {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (m *mockStore) UpdateExceptionCaseStatus(ctx context.Context, exceptionCaseID string, params domain.UpdateExceptionCaseStatusParams, actorID, correlationID string) (*domain.ExceptionCase, error) {
	c, ok := m.cases[exceptionCaseID]
	if !ok {
		return nil, domain.ErrExceptionCaseNotFound
	}
	if params.ExpectedVersion > 0 && c.Version != params.ExpectedVersion {
		return nil, domain.ErrVersionMismatch
	}
	c.CaseStatus = params.CaseStatus
	if params.CaseStatus == domain.CaseStatusEscalated && c.EscalatedAt == nil {
		now := time.Now().UTC()
		c.EscalatedAt = &now
	}
	c.UpdatedAt = time.Now().UTC()
	c.Version++
	return c, nil
}

func (m *mockStore) CreateEscalationRecord(ctx context.Context, params domain.CreateEscalationRecordParams) (*domain.EscalationRecord, error) {
	id := params.EscalationRecordID
	if id == "" {
		id = "escrec-test-01"
	}
	now := time.Now().UTC()
	er := &domain.EscalationRecord{
		EscalationRecordID:     id,
		ExceptionCaseID:        params.ExceptionCaseID,
		EscalatedToRole:        params.EscalatedToRole,
		EscalationReason:       params.EscalationReason,
		EscalationStatus:       domain.EscalationStatusPending,
		EscalatedAt:            now,
		CreatedAt:              now,
		CreatedByPrincipalID:   params.CreatedByPrincipalID,
	}
	m.escalations[id] = er
	return er, nil
}

func (m *mockStore) FindEscalationRecordByID(ctx context.Context, escalationRecordID string) (*domain.EscalationRecord, error) {
	er, ok := m.escalations[escalationRecordID]
	if !ok {
		return nil, domain.ErrEscalationRecordNotFound
	}
	return er, nil
}

func (m *mockStore) ListEscalationRecords(ctx context.Context, filter domain.ListEscalationRecordsFilter) ([]*domain.EscalationRecord, error) {
	var out []*domain.EscalationRecord
	for _, item := range m.escalations {
		if filter.ExceptionCaseID != "" && item.ExceptionCaseID != filter.ExceptionCaseID {
			continue
		}
		if filter.EscalatedToRole != "" && item.EscalatedToRole != filter.EscalatedToRole {
			continue
		}
		if filter.EscalationStatus != "" && item.EscalationStatus != filter.EscalationStatus {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

type mockPublisher struct{}

func (p *mockPublisher) Publish(ctx context.Context, params events.PublishParams) error {
	return nil
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

func TestCreateAndGetExceptionCase(t *testing.T) {
	r, _ := setupTestRouter()

	reqPayload := map[string]interface{}{
		"legal_entity_id":         "entity-001",
		"exception_type":          "MISSED_DEADLINE",
		"severity_level":          "HIGH",
		"linked_object_type":      "FILING",
		"linked_object_id":        "ftrk-2026-9901",
		"created_by_principal_id": "filing_tracker_svc",
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/exceptions/", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req.Header.Set("X-Tenant-Id", "tenant-001")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", w.Code)
	}

	var created domain.ExceptionCase
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if created.ExceptionCaseID == "" {
		t.Error("expected exception_case_id to be set")
	}
	if created.LegalEntityID != "entity-001" {
		t.Errorf("expected legal_entity_id entity-001, got %s", created.LegalEntityID)
	}
	if created.ExceptionType != "MISSED_DEADLINE" {
		t.Errorf("expected exception_type MISSED_DEADLINE, got %s", created.ExceptionType)
	}
	if created.SeverityLevel != "HIGH" {
		t.Errorf("expected severity_level HIGH, got %s", created.SeverityLevel)
	}
	if created.LinkedObjectType != "FILING" {
		t.Errorf("expected linked_object_type FILING, got %s", created.LinkedObjectType)
	}
	if created.CaseStatus != "OPEN" {
		t.Errorf("expected case_status OPEN, got %s", created.CaseStatus)
	}

	// Get by ID
	getReq := httptest.NewRequest("GET", "/v1/exceptions/"+created.ExceptionCaseID, nil)
	getReq.Header.Set("X-Principal-Id", "principal-test-01")
	getReq.Header.Set("X-Tenant-Id", "tenant-001")
	getW := httptest.NewRecorder()
	r.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Errorf("expected status 200 on GetByID, got %d", getW.Code)
	}

	var getResult domain.ExceptionCase
	_ = json.NewDecoder(getW.Body).Decode(&getResult)
	if getResult.ExceptionCaseID != created.ExceptionCaseID {
		t.Error("mismatch in GetByID result")
	}
}

func TestCreateEscalationRecord(t *testing.T) {
	r, st := setupTestRouter()

	// Create exception case first
	now := time.Now().UTC()
	ec := &domain.ExceptionCase{
		ExceptionCaseID:        "excase-escalate-01",
		TenantID:               "tenant-001",
		LegalEntityID:          "entity-001",
		ExceptionType:          "MISSED_DEADLINE",
		SeverityLevel:          "HIGH",
		LinkedObjectType:       "FILING",
		LinkedObjectID:         "ftrk-2026-9901",
		CaseStatus:             domain.CaseStatusOpen,
		CreatedAt:              now,
		CreatedByPrincipalID:   "filing_tracker_svc",
		UpdatedAt:              now,
		Version:                1,
	}
	st.cases[ec.ExceptionCaseID] = ec

	reqPayload := map[string]interface{}{
		"exception_case_id":        ec.ExceptionCaseID,
		"escalated_to_role":        "COMPLIANCE_HEAD",
		"escalation_reason":        "High severity overdue filing exceeding 7 days",
		"created_by_principal_id":  "tax_manager",
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/exceptions/"+ec.ExceptionCaseID+"/escalations", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req.Header.Set("X-Tenant-Id", "tenant-001")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", w.Code)
	}

	var created domain.EscalationRecord
	_ = json.NewDecoder(w.Body).Decode(&created)
	if created.EscalationRecordID == "" {
		t.Error("expected escalation_record_id to be set")
	}
	if created.ExceptionCaseID != ec.ExceptionCaseID {
		t.Errorf("expected exception_case_id %s, got %s", ec.ExceptionCaseID, created.ExceptionCaseID)
	}
	if created.EscalatedToRole != "COMPLIANCE_HEAD" {
		t.Errorf("expected escalated_to_role COMPLIANCE_HEAD, got %s", created.EscalatedToRole)
	}
	if created.EscalationStatus != "PENDING" {
		t.Errorf("expected escalation_status PENDING, got %s", created.EscalationStatus)
	}
}

func TestUpdateExceptionCaseStatus(t *testing.T) {
	r, st := setupTestRouter()

	// Create exception case
	now := time.Now().UTC()
	ec := &domain.ExceptionCase{
		ExceptionCaseID:        "excase-status-01",
		TenantID:               "tenant-001",
		LegalEntityID:          "entity-001",
		ExceptionType:          "MISSED_DEADLINE",
		SeverityLevel:          "HIGH",
		LinkedObjectType:       "FILING",
		LinkedObjectID:         "ftrk-2026-9901",
		CaseStatus:             domain.CaseStatusOpen,
		CreatedAt:              now,
		CreatedByPrincipalID:   "filing_tracker_svc",
		UpdatedAt:              now,
		Version:                1,
	}
	st.cases[ec.ExceptionCaseID] = ec

	// Update status to ESCALATED
	updateReq := map[string]interface{}{
		"case_status":       "ESCALATED",
		"expected_version":  1,
	}
	body, _ := json.Marshal(updateReq)

	req := httptest.NewRequest("POST", "/v1/exceptions/"+ec.ExceptionCaseID+"/status", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	req.Header.Set("X-Tenant-Id", "tenant-001")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var result domain.ExceptionCase
	_ = json.NewDecoder(w.Body).Decode(&result)
	if result.CaseStatus != "ESCALATED" {
		t.Errorf("expected case_status ESCALATED, got %s", result.CaseStatus)
	}
	if result.EscalatedAt == nil {
		t.Error("expected escalated_at to be set when status is ESCALATED")
	}
	if result.Version != 2 {
		t.Errorf("expected version 2, got %d", result.Version)
	}
}

func TestListExceptionCases(t *testing.T) {
	r, _ := setupTestRouter()

	// Create two exception cases
	cases := []map[string]interface{}{
		{
			"legal_entity_id":         "entity-101",
			"exception_type":          "MISSED_DEADLINE",
			"severity_level":          "HIGH",
			"linked_object_type":      "FILING",
			"linked_object_id":        "ftrk-2026-9901",
			"created_by_principal_id": "filing_tracker_svc",
		},
		{
			"legal_entity_id":         "entity-101",
			"exception_type":          "INSUFFICIENT_EVIDENCE",
			"severity_level":          "MEDIUM",
			"linked_object_type":      "OBLIGATION",
			"linked_object_id":        "obl-2026-001",
			"created_by_principal_id": "compliance_officer",
		},
	}

	for _, c := range cases {
		body, _ := json.Marshal(c)
		req := httptest.NewRequest("POST", "/v1/exceptions/", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Principal-Id", "principal-test-01")
		req.Header.Set("X-Tenant-Id", "tenant-001")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Errorf("failed to create exception case: %d", w.Code)
		}
	}

	// List for entity-101
	listReq := httptest.NewRequest("GET", "/v1/exceptions/?legal_entity_id=entity-101", nil)
	listReq.Header.Set("X-Principal-Id", "principal-test-01")
	listReq.Header.Set("X-Tenant-Id", "tenant-001")
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200 on List, got %d", listW.Code)
	}

	var listResp map[string]interface{}
	_ = json.NewDecoder(listW.Body).Decode(&listResp)
	records := listResp["exception_cases"]
	if records == nil {
		t.Error("expected exception_cases in response")
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
				"exception_type":         "MISSED_DEADLINE",
				"severity_level":         "HIGH",
				"linked_object_type":     "FILING",
				"linked_object_id":       "ftrk-2026-9901",
				"created_by_principal_id": "test",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid exception_type",
			payload: map[string]interface{}{
				"legal_entity_id":         "entity-001",
				"exception_type":          "INVALID",
				"severity_level":          "HIGH",
				"linked_object_type":      "FILING",
				"linked_object_id":        "ftrk-2026-9901",
				"created_by_principal_id": "test",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid severity_level",
			payload: map[string]interface{}{
				"legal_entity_id":         "entity-001",
				"exception_type":          "MISSED_DEADLINE",
				"severity_level":          "INVALID",
				"linked_object_type":      "FILING",
				"linked_object_id":        "ftrk-2026-9901",
				"created_by_principal_id": "test",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid linked_object_type",
			payload: map[string]interface{}{
				"legal_entity_id":         "entity-001",
				"exception_type":          "MISSED_DEADLINE",
				"severity_level":          "HIGH",
				"linked_object_type":      "INVALID",
				"linked_object_id":        "ftrk-2026-9901",
				"created_by_principal_id": "test",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid case_status",
			payload: map[string]interface{}{
				"case_status":        "INVALID",
				"expected_version":   1,
			},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.payload)
			var req *http.Request
			if tc.name == "invalid case_status" {
				req = httptest.NewRequest("POST", "/v1/exceptions/excase-status-01/status", bytes.NewBuffer(body))
			} else {
				req = httptest.NewRequest("POST", "/v1/exceptions/", bytes.NewBuffer(body))
			}
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