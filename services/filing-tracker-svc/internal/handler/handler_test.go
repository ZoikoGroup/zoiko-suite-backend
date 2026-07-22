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

	"zoiko.io/filing-tracker-svc/internal/authz"
	"zoiko.io/filing-tracker-svc/internal/domain"
)

type mockStore struct {
	items map[string]*domain.FilingRequirement
}

func newMockStore() *mockStore {
	return &mockStore{items: make(map[string]*domain.FilingRequirement)}
}

func (m *mockStore) Create(ctx context.Context, f *domain.FilingRequirement) error {
	if f.FilingID == "" {
		f.FilingID = "ftrk-test-202"
	}
	f.CreatedAt = time.Now().UTC()
	f.UpdatedAt = time.Now().UTC()
	if f.Status == "" {
		f.Status = domain.StatusScheduled
	}
	m.items[f.FilingID] = f
	return nil
}

func (m *mockStore) GetByID(ctx context.Context, id string) (*domain.FilingRequirement, error) {
	f, ok := m.items[id]
	if !ok {
		return nil, domain.ErrRequirementNotFound
	}
	return f, nil
}

func (m *mockStore) List(ctx context.Context, legalEntityID, jurisdictionID, filingAuthority, status string) ([]domain.FilingRequirement, error) {
	var out []domain.FilingRequirement
	for _, item := range m.items {
		if legalEntityID != "" && item.LegalEntityID != legalEntityID {
			continue
		}
		if jurisdictionID != "" && item.JurisdictionID != jurisdictionID {
			continue
		}
		if filingAuthority != "" && item.FilingAuthority != filingAuthority {
			continue
		}
		if status != "" && string(item.Status) != status {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

func (m *mockStore) Update(ctx context.Context, f *domain.FilingRequirement) error {
	if _, ok := m.items[f.FilingID]; !ok {
		return domain.ErrRequirementNotFound
	}
	f.UpdatedAt = time.Now().UTC()
	m.items[f.FilingID] = f
	return nil
}

func (m *mockStore) Submit(ctx context.Context, id string, req *domain.SubmitFilingRequest) (*domain.FilingRequirement, error) {
	f, ok := m.items[id]
	if !ok {
		return nil, domain.ErrRequirementNotFound
	}
	if f.Status == domain.StatusSubmitted || f.Status == domain.StatusConfirmed {
		return nil, domain.ErrAlreadySubmitted
	}
	now := time.Now().UTC()
	f.Status = domain.StatusSubmitted
	f.SubmissionReference = &req.SubmissionReference
	f.SubmittedAt = &now
	f.SubmittedBy = &req.SubmittedBy
	f.UpdatedAt = now
	return f, nil
}

func (m *mockStore) Confirm(ctx context.Context, id string, req *domain.ConfirmFilingRequest) (*domain.FilingRequirement, error) {
	f, ok := m.items[id]
	if !ok {
		return nil, domain.ErrRequirementNotFound
	}
	if f.Status == domain.StatusConfirmed {
		return nil, domain.ErrAlreadyConfirmed
	}
	now := time.Now().UTC()
	f.Status = domain.StatusConfirmed
	f.ConfirmationReference = &req.ConfirmationReference
	f.ConfirmedAt = &now
	f.UpdatedAt = now
	return f, nil
}

func (m *mockStore) MarkOverdue(ctx context.Context, id, todayStr string) (*domain.FilingRequirement, error) {
	f, ok := m.items[id]
	if !ok {
		return nil, domain.ErrRequirementNotFound
	}
	f.CheckOverdue(todayStr)
	f.UpdatedAt = time.Now().UTC()
	return f, nil
}

type mockPublisher struct{}

func (p *mockPublisher) Publish(ctx context.Context, eventType, filingID, tenantID string, payload interface{}) error {
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

func TestScheduleSubmitAndConfirmFilingRequirement(t *testing.T) {
	r, _ := setupTestRouter()

	reqPayload := domain.CreateRequirementRequest{
		LegalEntityID:   "entity-001",
		JurisdictionID:  "GB-UK",
		FilingAuthority: "HMRC",
		FilingType:      "VAT",
		PeriodKey:       "2026-Q2",
		DueDate:         "2026-08-07",
		Notes:           "Quarterly VAT return filing",
		CreatedBy:       "tax_manager",
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/filing-tracker/requirements", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", w.Code)
	}

	var created domain.FilingRequirement
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Submit filing
	subReqPayload := domain.SubmitFilingRequest{
		SubmissionReference: "HMRC-SUB-2026-9901",
		SubmittedBy:         "tax_specialist",
	}
	subBody, _ := json.Marshal(subReqPayload)

	subReq := httptest.NewRequest("POST", "/v1/filing-tracker/requirements/"+created.FilingID+"/submit", bytes.NewBuffer(subBody))
	subReq.Header.Set("Content-Type", "application/json")
	subW := httptest.NewRecorder()

	r.ServeHTTP(subW, subReq)

	if subW.Code != http.StatusOK {
		t.Fatalf("expected status 200 on submit, got %d", subW.Code)
	}

	var submitted domain.FilingRequirement
	if err := json.NewDecoder(subW.Body).Decode(&submitted); err != nil {
		t.Fatalf("failed to decode submitted response: %v", err)
	}
	if submitted.Status != domain.StatusSubmitted {
		t.Errorf("expected status SUBMITTED, got %s", submitted.Status)
	}

	// Confirm filing
	confReqPayload := domain.ConfirmFilingRequest{
		ConfirmationReference: "HMRC-CONFIRM-9901-OK",
	}
	confBody, _ := json.Marshal(confReqPayload)

	confReq := httptest.NewRequest("POST", "/v1/filing-tracker/requirements/"+created.FilingID+"/confirm", bytes.NewBuffer(confBody))
	confReq.Header.Set("Content-Type", "application/json")
	confW := httptest.NewRecorder()

	r.ServeHTTP(confW, confReq)

	if confW.Code != http.StatusOK {
		t.Fatalf("expected status 200 on confirm, got %d", confW.Code)
	}

	var confirmed domain.FilingRequirement
	if err := json.NewDecoder(confW.Body).Decode(&confirmed); err != nil {
		t.Fatalf("failed to decode confirmed response: %v", err)
	}
	if confirmed.Status != domain.StatusConfirmed {
		t.Errorf("expected status CONFIRMED, got %s", confirmed.Status)
	}
}
