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

	"zoiko.io/exception-escalation-svc/internal/authz"
	"zoiko.io/exception-escalation-svc/internal/domain"
)

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

func (m *mockStore) CreateException(ctx context.Context, c *domain.ExceptionCase) error {
	if c.ExceptionCaseID == "" {
		c.ExceptionCaseID = "excase-test-01"
	}
	c.CreatedAt = time.Now().UTC()
	c.UpdatedAt = time.Now().UTC()
	if c.CaseStatus == "" {
		c.CaseStatus = domain.CaseOpen
	}
	m.cases[c.ExceptionCaseID] = c
	return nil
}

func (m *mockStore) GetExceptionByID(ctx context.Context, id string) (*domain.ExceptionCase, error) {
	c, ok := m.cases[id]
	if !ok {
		return nil, domain.ErrExceptionCaseNotFound
	}
	return c, nil
}

func (m *mockStore) ListExceptions(ctx context.Context, legalEntityID, caseStatus, severityLevel, exceptionType string) ([]domain.ExceptionCase, error) {
	var out []domain.ExceptionCase
	for _, item := range m.cases {
		if legalEntityID != "" && item.LegalEntityID != legalEntityID {
			continue
		}
		if caseStatus != "" && string(item.CaseStatus) != caseStatus {
			continue
		}
		if severityLevel != "" && string(item.SeverityLevel) != severityLevel {
			continue
		}
		if exceptionType != "" && item.ExceptionType != exceptionType {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

func (m *mockStore) EscalateException(ctx context.Context, id string, req *domain.EscalateCaseRequest) (*domain.EscalationRecord, *domain.ExceptionCase, error) {
	c, ok := m.cases[id]
	if !ok {
		return nil, nil, domain.ErrExceptionCaseNotFound
	}
	if c.CaseStatus == domain.CaseClosed || c.CaseStatus == domain.CaseResolved {
		return nil, nil, domain.ErrCaseAlreadyClosed
	}

	now := time.Now().UTC()
	escRecord := domain.EscalationRecord{
		EscalationRecordID: "escrec-test-01",
		ExceptionCaseID:    id,
		EscalatedToRole:    req.EscalatedToRole,
		EscalatedToUser:    req.EscalatedToUser,
		EscalationReason:   req.EscalationReason,
		EscalationStatus:   domain.EscalationPending,
		EscalatedBy:        req.EscalatedBy,
		EscalatedAt:        now,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	m.escalations[escRecord.EscalationRecordID] = &escRecord

	c.CaseStatus = domain.CaseEscalated
	c.EscalatedAt = &now
	c.AssignedToRole = req.EscalatedToRole
	c.UpdatedAt = now
	c.Escalations = append(c.Escalations, escRecord)

	return &escRecord, c, nil
}

func (m *mockStore) ResolveException(ctx context.Context, id string, req *domain.ResolveCaseRequest) (*domain.ExceptionCase, error) {
	c, ok := m.cases[id]
	if !ok {
		return nil, domain.ErrExceptionCaseNotFound
	}
	if c.CaseStatus == domain.CaseClosed {
		return nil, domain.ErrCaseAlreadyClosed
	}
	now := time.Now().UTC()
	c.CaseStatus = domain.CaseClosed
	c.ClosedAt = &now
	c.ClosedBy = req.ClosedBy
	c.ClosureReason = req.ClosureReason
	c.UpdatedAt = now
	return c, nil
}

func (m *mockStore) ListEscalations(ctx context.Context, role, status string) ([]domain.EscalationRecord, error) {
	var out []domain.EscalationRecord
	for _, item := range m.escalations {
		if role != "" && item.EscalatedToRole != role {
			continue
		}
		if status != "" && string(item.EscalationStatus) != status {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

type mockPublisher struct{}

func (p *mockPublisher) Publish(ctx context.Context, eventType, caseID, tenantID string, payload interface{}) error {
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

func TestCreateEscalateAndResolveException(t *testing.T) {
	r, _ := setupTestRouter()

	createReqPayload := domain.CreateExceptionRequest{
		LegalEntityID:    "entity-001",
		JurisdictionID:   "GB-UK",
		ExceptionType:    "OVERDUE_FILING",
		SeverityLevel:    domain.SeverityHigh,
		LinkedObjectType: "FILING",
		LinkedObjectID:   "ftrk-2026-9901",
		Description:      "VAT return not submitted before due date",
		AssignedToRole:   "TAX_MANAGER",
		CreatedBy:        "filing_tracker_svc",
	}
	body, _ := json.Marshal(createReqPayload)

	req := httptest.NewRequest("POST", "/v1/exception-escalation/exceptions", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", w.Code)
	}

	var created domain.ExceptionCase
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Escalate case
	escReqPayload := domain.EscalateCaseRequest{
		EscalatedToRole:  "COMPLIANCE_HEAD",
		EscalationReason: "High severity overdue filing exceeding 7 days",
		EscalatedBy:      "tax_manager",
	}
	escBody, _ := json.Marshal(escReqPayload)

	escReq := httptest.NewRequest("POST", "/v1/exception-escalation/exceptions/"+created.ExceptionCaseID+"/escalate", bytes.NewBuffer(escBody))
	escReq.Header.Set("Content-Type", "application/json")
	escW := httptest.NewRecorder()

	r.ServeHTTP(escW, escReq)

	if escW.Code != http.StatusOK {
		t.Fatalf("expected status 200 on escalate, got %d", escW.Code)
	}

	// Resolve case
	resReqPayload := domain.ResolveCaseRequest{
		ClosedBy:      "compliance_head",
		ClosureReason: "Late filing approved and submitted with penalty waiver",
	}
	resBody, _ := json.Marshal(resReqPayload)

	resReq := httptest.NewRequest("POST", "/v1/exception-escalation/exceptions/"+created.ExceptionCaseID+"/resolve", bytes.NewBuffer(resBody))
	resReq.Header.Set("Content-Type", "application/json")
	resW := httptest.NewRecorder()

	r.ServeHTTP(resW, resReq)

	if resW.Code != http.StatusOK {
		t.Fatalf("expected status 200 on resolve, got %d", resW.Code)
	}

	var resolved domain.ExceptionCase
	if err := json.NewDecoder(resW.Body).Decode(&resolved); err != nil {
		t.Fatalf("failed to decode resolved response: %v", err)
	}
	if resolved.CaseStatus != domain.CaseClosed {
		t.Errorf("expected status CLOSED, got %s", resolved.CaseStatus)
	}
}
