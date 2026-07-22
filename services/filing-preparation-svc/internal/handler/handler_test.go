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

	"zoiko.io/filing-preparation-svc/internal/authz"
	"zoiko.io/filing-preparation-svc/internal/domain"
)

type mockStore struct {
	items map[string]*domain.FilingDraft
}

func newMockStore() *mockStore {
	return &mockStore{items: make(map[string]*domain.FilingDraft)}
}

func (m *mockStore) Create(ctx context.Context, d *domain.FilingDraft) error {
	if d.DraftID == "" {
		d.DraftID = "fprep-test-101"
	}
	d.CreatedAt = time.Now().UTC()
	d.UpdatedAt = time.Now().UTC()
	if d.ValidationStatus == "" {
		d.ValidationStatus = domain.StatusDraft
	}
	m.items[d.DraftID] = d
	return nil
}

func (m *mockStore) GetByID(ctx context.Context, id string) (*domain.FilingDraft, error) {
	d, ok := m.items[id]
	if !ok {
		return nil, domain.ErrDraftNotFound
	}
	return d, nil
}

func (m *mockStore) List(ctx context.Context, legalEntityID, jurisdictionID, filingType, status string) ([]domain.FilingDraft, error) {
	var out []domain.FilingDraft
	for _, item := range m.items {
		if legalEntityID != "" && item.LegalEntityID != legalEntityID {
			continue
		}
		if jurisdictionID != "" && item.JurisdictionID != jurisdictionID {
			continue
		}
		if filingType != "" && item.FilingType != filingType {
			continue
		}
		if status != "" && string(item.ValidationStatus) != status {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

func (m *mockStore) Update(ctx context.Context, d *domain.FilingDraft) error {
	if _, ok := m.items[d.DraftID]; !ok {
		return domain.ErrDraftNotFound
	}
	d.UpdatedAt = time.Now().UTC()
	m.items[d.DraftID] = d
	return nil
}

func (m *mockStore) Validate(ctx context.Context, id string, req *domain.ValidateDraftRequest) (*domain.FilingDraft, error) {
	d, ok := m.items[id]
	if !ok {
		return nil, domain.ErrDraftNotFound
	}
	d.ValidateEvidence(req.RequiredDocumentTypes)
	d.UpdatedAt = time.Now().UTC()
	return d, nil
}

func (m *mockStore) Finalize(ctx context.Context, id string, req *domain.FinalizeDraftRequest) (*domain.FilingDraft, error) {
	d, ok := m.items[id]
	if !ok {
		return nil, domain.ErrDraftNotFound
	}
	if d.ValidationStatus == domain.StatusBlocked {
		return nil, domain.ErrValidationBlocked
	}
	d.ValidationStatus = domain.StatusReadyForSubmission
	d.UpdatedAt = time.Now().UTC()
	return d, nil
}

type mockPublisher struct{}

func (p *mockPublisher) Publish(ctx context.Context, eventType, draftID, tenantID string, payload interface{}) error {
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

func TestCreateAndValidateFilingDraft(t *testing.T) {
	r, _ := setupTestRouter()

	reqPayload := domain.CreateDraftRequest{
		LegalEntityID:       "entity-001",
		JurisdictionID:      "GB-UK",
		FilingType:          "VAT",
		PeriodKey:           "2026-Q2",
		DueDate:             "2026-08-07",
		PayloadData:         `{"total_vat_due": 12500.00}`,
		EvidenceManifestRef: "manifest-2026-q2-vat",
		CreatedBy:           "compliance_officer",
	}
	body, _ := json.Marshal(reqPayload)

	req := httptest.NewRequest("POST", "/v1/filing-preparation/drafts", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", w.Code)
	}

	var created domain.FilingDraft
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Validate draft
	valReqPayload := domain.ValidateDraftRequest{
		RequiredDocumentTypes: []string{"INVOICE_SUMMARY", "VAT_RECONCILIATION"},
		ValidatedBy:           "checker",
	}
	valBody, _ := json.Marshal(valReqPayload)

	valReq := httptest.NewRequest("POST", "/v1/filing-preparation/drafts/"+created.DraftID+"/validate", bytes.NewBuffer(valBody))
	valReq.Header.Set("Content-Type", "application/json")
	valW := httptest.NewRecorder()

	r.ServeHTTP(valW, valReq)

	if valW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", valW.Code)
	}

	var validated domain.FilingDraft
	if err := json.NewDecoder(valW.Body).Decode(&validated); err != nil {
		t.Fatalf("failed to decode validated response: %v", err)
	}
	if validated.ValidationStatus != domain.StatusPrepared {
		t.Errorf("expected validation status PREPARED, got %s", validated.ValidationStatus)
	}

	// Finalize draft
	finReqPayload := domain.FinalizeDraftRequest{FinalizedBy: "tax_head"}
	finBody, _ := json.Marshal(finReqPayload)

	finReq := httptest.NewRequest("POST", "/v1/filing-preparation/drafts/"+created.DraftID+"/finalize", bytes.NewBuffer(finBody))
	finReq.Header.Set("Content-Type", "application/json")
	finW := httptest.NewRecorder()

	r.ServeHTTP(finW, finReq)

	if finW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", finW.Code)
	}

	var finalized domain.FilingDraft
	if err := json.NewDecoder(finW.Body).Decode(&finalized); err != nil {
		t.Fatalf("failed to decode finalized response: %v", err)
	}
	if finalized.ValidationStatus != domain.StatusReadyForSubmission {
		t.Errorf("expected status READY_FOR_SUBMISSION, got %s", finalized.ValidationStatus)
	}
}
