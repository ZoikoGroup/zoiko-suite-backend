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
	"zoiko.io/filing-preparation-svc/internal/evidencereq"
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

func (m *mockStore) Validate(ctx context.Context, id string, req *domain.ValidateDraftRequest, evidenceSufficient bool, blockReason string) (*domain.FilingDraft, error) {
	d, ok := m.items[id]
	if !ok {
		return nil, domain.ErrDraftNotFound
	}
	d.ApplyEvidenceOutcome(evidenceSufficient, blockReason)
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

// stubEvidenceReq grants sufficiency by default, matching a
// SATISFIED/NO_REQUIREMENTS_DEFINED outcome but skipping the network call.
// Tests can set result/err to exercise the blocked and fail-closed paths.
type stubEvidenceReq struct {
	result evidencereq.EvaluateResult
	err    error
}

func (s *stubEvidenceReq) Evaluate(_ context.Context, _, _, _, _, _, _ string, _ []evidencereq.Artifact) (evidencereq.EvaluateResult, error) {
	if s.err != nil {
		return evidencereq.EvaluateResult{}, s.err
	}
	return s.result, nil
}

// newSufficientEvidenceReq is the default stub: always reports sufficient
// evidence, matching a SATISFIED/NO_REQUIREMENTS_DEFINED outcome.
func newSufficientEvidenceReq() *stubEvidenceReq {
	return &stubEvidenceReq{result: evidencereq.EvaluateResult{Sufficient: true}}
}

// newGrantingAuthzServer starts a stub authorization-svc that grants every
// request, mirroring the real service's always-200 contract.
func newGrantingAuthzServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"decision_outcome": "GRANTED"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupTestRouter(t *testing.T) (*chi.Mux, *mockStore) {
	st := newMockStore()
	pub := &mockPublisher{}
	authzSrv := newGrantingAuthzServer(t)
	az := authz.NewClient(authzSrv.URL)
	logger, _ := zap.NewDevelopment()
	h := New(st, pub, az, newSufficientEvidenceReq(), logger)

	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r, st
}

func TestCreateAndValidateFilingDraft(t *testing.T) {
	r, _ := setupTestRouter(t)

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
	req.Header.Set("X-Principal-Id", "user-001")
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
	valReq.Header.Set("X-Principal-Id", "user-001")
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
	finReq.Header.Set("X-Principal-Id", "user-001")
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

// TestValidate_IgnoresCallerSuppliedRequiredDocs is a regression test for
// the actual bug this fix closes: RequiredDocumentTypes is caller-supplied
// and must NOT be what decides sufficiency — only evidence-requirements-svc's
// real evaluation may. An empty list must not silently pass when the
// catalog says evidence is missing.
func TestValidate_IgnoresCallerSuppliedRequiredDocs(t *testing.T) {
	st := newMockStore()
	pub := &mockPublisher{}
	authzSrv := newGrantingAuthzServer(t)
	az := authz.NewClient(authzSrv.URL)
	logger, _ := zap.NewDevelopment()
	evidenceReq := &stubEvidenceReq{result: evidencereq.EvaluateResult{Sufficient: false, Reason: "catalog requires INVOICE_SUMMARY"}}
	h := New(st, pub, az, evidenceReq, logger)
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	created := &domain.FilingDraft{DraftID: "fprep-test-201", LegalEntityID: "entity-001", FilingType: "VAT", ValidationStatus: domain.StatusDraft}
	st.items[created.DraftID] = created

	// Caller sends an EMPTY required_document_types — the old bug let this
	// always pass. The real catalog (stubbed above) says evidence is missing.
	valBody, _ := json.Marshal(domain.ValidateDraftRequest{RequiredDocumentTypes: nil, ValidatedBy: "checker"})
	valReq := httptest.NewRequest("POST", "/v1/filing-preparation/drafts/"+created.DraftID+"/validate", bytes.NewBuffer(valBody))
	valReq.Header.Set("Content-Type", "application/json")
	valReq.Header.Set("X-Principal-Id", "user-001")
	valW := httptest.NewRecorder()
	r.ServeHTTP(valW, valReq)

	if valW.Code != http.StatusOK {
		t.Fatalf("expected status 200 (Validate always 200s, BLOCKED is a valid persisted state), got %d", valW.Code)
	}
	var validated domain.FilingDraft
	if err := json.NewDecoder(valW.Body).Decode(&validated); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if validated.ValidationStatus != domain.StatusBlocked {
		t.Fatalf("expected BLOCKED despite empty required_document_types, got %s", validated.ValidationStatus)
	}
	if validated.BlockReasons != "catalog requires INVOICE_SUMMARY" {
		t.Errorf("expected the real catalog's reason to be recorded, got %q", validated.BlockReasons)
	}

	// Finalize must now be refused — BLOCKED is enforced downstream too.
	finBody, _ := json.Marshal(domain.FinalizeDraftRequest{FinalizedBy: "tax_head"})
	finReq := httptest.NewRequest("POST", "/v1/filing-preparation/drafts/"+created.DraftID+"/finalize", bytes.NewBuffer(finBody))
	finReq.Header.Set("Content-Type", "application/json")
	finReq.Header.Set("X-Principal-Id", "user-001")
	finW := httptest.NewRecorder()
	r.ServeHTTP(finW, finReq)
	if finW.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 finalizing a BLOCKED draft, got %d — %s", finW.Code, finW.Body.String())
	}
}

// TestValidate_EvidenceServiceUnavailable_Returns503 proves a transport
// failure is never conflated with "evidence missing" — it must fail closed
// without writing any status at all.
func TestValidate_EvidenceServiceUnavailable_Returns503(t *testing.T) {
	st := newMockStore()
	pub := &mockPublisher{}
	authzSrv := newGrantingAuthzServer(t)
	az := authz.NewClient(authzSrv.URL)
	logger, _ := zap.NewDevelopment()
	evidenceReq := &stubEvidenceReq{err: evidencereq.ErrServiceUnavailable}
	h := New(st, pub, az, evidenceReq, logger)
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	created := &domain.FilingDraft{DraftID: "fprep-test-202", LegalEntityID: "entity-001", FilingType: "VAT", ValidationStatus: domain.StatusDraft}
	st.items[created.DraftID] = created

	valBody, _ := json.Marshal(domain.ValidateDraftRequest{ValidatedBy: "checker"})
	valReq := httptest.NewRequest("POST", "/v1/filing-preparation/drafts/"+created.DraftID+"/validate", bytes.NewBuffer(valBody))
	valReq.Header.Set("Content-Type", "application/json")
	valReq.Header.Set("X-Principal-Id", "user-001")
	valW := httptest.NewRecorder()
	r.ServeHTTP(valW, valReq)

	if valW.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when evidence-requirements-svc is unavailable, got %d — %s", valW.Code, valW.Body.String())
	}
	if st.items[created.DraftID].ValidationStatus != domain.StatusDraft {
		t.Errorf("draft status must not change when evidence check fails closed, got %s", st.items[created.DraftID].ValidationStatus)
	}
}
