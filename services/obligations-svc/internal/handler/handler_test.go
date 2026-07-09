package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/handler"
)

// ── stub store ────────────────────────────────────────────────────────────────

// stubStore implements handler.ObligationStore for unit testing.
// No DB, no network — purely in-memory.
type stubStore struct {
	obligation        *domain.Obligation
	obligationCreated bool
	createErr         error

	findObligation *domain.Obligation
	findErr        error

	list    []*domain.Obligation
	listErr error

	updated      *domain.Obligation
	transitioned bool
	updateErr    error

	filingReq     *domain.FilingRequirement
	filingErr     error
	filingList    []*domain.FilingRequirement
	filingListErr error
}

func (s *stubStore) CreateObligation(_ context.Context, _ domain.CreateObligationParams) (*domain.Obligation, bool, error) {
	return s.obligation, s.obligationCreated, s.createErr
}

func (s *stubStore) FindObligationByID(_ context.Context, _ string) (*domain.Obligation, error) {
	return s.findObligation, s.findErr
}

func (s *stubStore) ListObligations(_ context.Context, _ domain.ListObligationsFilter) ([]*domain.Obligation, error) {
	return s.list, s.listErr
}

func (s *stubStore) UpdateObligationStatus(_ context.Context, _, _ string) (*domain.Obligation, bool, error) {
	return s.updated, s.transitioned, s.updateErr
}

func (s *stubStore) CreateFilingRequirement(_ context.Context, _ domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error) {
	return s.filingReq, s.filingErr
}

func (s *stubStore) ListFilingRequirements(_ context.Context, _ string) ([]*domain.FilingRequirement, error) {
	return s.filingList, s.filingListErr
}

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct {
	createdCalls int
	updatedCalls int
	overdueCalls int
	closedCalls  int
}

func (p *stubPublisher) PublishObligationCreated(_ context.Context, _ domain.Obligation, _ string) error {
	p.createdCalls++
	return nil
}

func (p *stubPublisher) PublishObligationUpdated(_ context.Context, _ domain.Obligation, _ string) error {
	p.updatedCalls++
	return nil
}

func (p *stubPublisher) PublishObligationOverdue(_ context.Context, _ domain.Obligation, _ string) error {
	p.overdueCalls++
	return nil
}

func (p *stubPublisher) PublishObligationClosed(_ context.Context, _ domain.Obligation, _ string) error {
	p.closedCalls++
	return nil
}

// ── stub jurisdiction validator ──────────────────────────────────────────────

type stubValidator struct {
	err error
}

func (v *stubValidator) ValidateExists(_ context.Context, _ string) error {
	return v.err
}

func newTestRouter(s *stubStore) chi.Router {
	return newTestRouterFull(s, &stubPublisher{}, &stubValidator{})
}

func newTestRouterWithPublisher(s *stubStore, p *stubPublisher) chi.Router {
	return newTestRouterFull(s, p, &stubValidator{})
}

func newTestRouterFull(s *stubStore, p *stubPublisher, v *stubValidator) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, v, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func validCreateBody() string {
	return `{
		"legal_entity_id": "le-1",
		"jurisdiction_id": "jur-1",
		"obligation_source_type": "JURISDICTION_RULE",
		"obligation_source_id": "rule-1",
		"obligation_code": "OBL-2026-001",
		"obligation_type": "FILING",
		"due_date": "2026-12-31T00:00:00Z",
		"severity_level": "HIGH",
		"responsible_function": "Tax",
		"source_reference": "IN-GST-FILING-RULE-07",
		"created_by_principal_id": "admin-1"
	}`
}

// ── CreateObligation ─────────────────────────────────────────────────────────

func TestCreateObligation_Created(t *testing.T) {
	store := &stubStore{
		obligation: &domain.Obligation{
			ObligationID:   "o-1",
			ObligationCode: "OBL-2026-001",
			LegalEntityID:  "le-1",
			JurisdictionID: "jur-1",
		},
		obligationCreated: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if pub.createdCalls != 1 {
		t.Errorf("expected obligation.created published once, got %d", pub.createdCalls)
	}
	var got domain.Obligation
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.ObligationCode != "OBL-2026-001" {
		t.Errorf("expected obligation_code OBL-2026-001, got %s", got.ObligationCode)
	}
}

func TestCreateObligation_IdempotentReplay(t *testing.T) {
	store := &stubStore{
		obligation:        &domain.Obligation{ObligationID: "o-1", ObligationCode: "OBL-2026-001"},
		obligationCreated: false,
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d", w.Code)
	}
	if pub.createdCalls != 0 {
		t.Errorf("expected obligation.created NOT published on idempotent replay, got %d calls", pub.createdCalls)
	}
}

func TestCreateObligation_MissingSourceReference(t *testing.T) {
	// Atomic Linking — source_reference is required. Build the body without it.
	r := newTestRouter(&stubStore{})

	body := `{
		"legal_entity_id": "le-1",
		"jurisdiction_id": "jur-1",
		"obligation_source_type": "JURISDICTION_RULE",
		"obligation_source_id": "rule-1",
		"obligation_code": "OBL-2026-001",
		"obligation_type": "FILING",
		"due_date": "2026-12-31T00:00:00Z",
		"severity_level": "HIGH",
		"responsible_function": "Tax",
		"created_by_principal_id": "admin-1"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["field"] != "source_reference" {
		t.Errorf("expected missing field source_reference, got %q", got["field"])
	}
}

func TestCreateObligation_JurisdictionNotFound(t *testing.T) {
	r := newTestRouterFull(&stubStore{}, &stubPublisher{}, &stubValidator{err: domain.ErrJurisdictionNotFound})

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateObligation_JurisdictionServiceUnavailable_FailsClosed(t *testing.T) {
	r := newTestRouterFull(&stubStore{}, &stubPublisher{}, &stubValidator{err: domain.ErrJurisdictionServiceUnavailable})

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (fail-closed), got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateObligation_Conflict(t *testing.T) {
	store := &stubStore{createErr: domain.ErrConflict}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestCreateObligation_StoreUnavailable(t *testing.T) {
	store := &stubStore{createErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// ── GetObligation ────────────────────────────────────────────────────────────

func TestGetObligation_Found(t *testing.T) {
	store := &stubStore{findObligation: &domain.Obligation{ObligationID: "o-1"}}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/obligations/o-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetObligation_NotFound(t *testing.T) {
	store := &stubStore{findErr: domain.ErrObligationNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/obligations/missing", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── ListObligations ──────────────────────────────────────────────────────────

func TestListObligations_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{list: nil})

	req := httptest.NewRequest(http.MethodGet, "/v1/obligations", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "[]\n" {
		t.Errorf("expected empty JSON array, got %q", w.Body.String())
	}
}

func TestListObligations_InvalidDueBefore(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodGet, "/v1/obligations?due_before=not-a-date", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── UpdateObligationStatus ───────────────────────────────────────────────────

func TestUpdateObligationStatus_TransitionToOverdue_PublishesBoth(t *testing.T) {
	store := &stubStore{
		updated:      &domain.Obligation{ObligationID: "o-1", ObligationStatus: "OVERDUE"},
		transitioned: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{"obligation_status":"OVERDUE"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if pub.updatedCalls != 1 {
		t.Errorf("expected obligation.updated published once, got %d", pub.updatedCalls)
	}
	if pub.overdueCalls != 1 {
		t.Errorf("expected obligation.overdue published once, got %d", pub.overdueCalls)
	}
	if pub.closedCalls != 0 {
		t.Errorf("expected obligation.closed NOT published, got %d", pub.closedCalls)
	}
}

func TestUpdateObligationStatus_TransitionToClosed_PublishesBoth(t *testing.T) {
	store := &stubStore{
		updated:      &domain.Obligation{ObligationID: "o-1", ObligationStatus: "CLOSED"},
		transitioned: true,
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{"obligation_status":"CLOSED"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if pub.updatedCalls != 1 || pub.closedCalls != 1 {
		t.Errorf("expected updated+closed published once each, got updated=%d closed=%d", pub.updatedCalls, pub.closedCalls)
	}
}

func TestUpdateObligationStatus_IdempotentNoOp_DoesNotRepublish(t *testing.T) {
	store := &stubStore{
		updated:      &domain.Obligation{ObligationID: "o-1", ObligationStatus: "OPEN"},
		transitioned: false,
	}
	pub := &stubPublisher{}
	r := newTestRouterWithPublisher(store, pub)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{"obligation_status":"OPEN"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if pub.updatedCalls != 0 {
		t.Errorf("expected no publish on idempotent no-op, got %d", pub.updatedCalls)
	}
}

func TestUpdateObligationStatus_InvalidTransition(t *testing.T) {
	store := &stubStore{updateErr: domain.ErrInvalidTransition}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{"obligation_status":"OPEN"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestUpdateObligationStatus_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateObligationStatus_NotFound(t *testing.T) {
	store := &stubStore{updateErr: domain.ErrObligationNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/missing/status", bytes.NewBufferString(`{"obligation_status":"CLOSED"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── CreateFilingRequirement ──────────────────────────────────────────────────

func TestCreateFilingRequirement_Created(t *testing.T) {
	store := &stubStore{filingReq: &domain.FilingRequirement{FilingRequirementID: "fr-1", ObligationID: "o-1"}}
	r := newTestRouter(store)

	body := `{"filing_type":"ANNUAL_RETURN","filing_authority":"IRS","submission_channel":"E_FILE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/filing-requirements", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateFilingRequirement_ObligationNotFound(t *testing.T) {
	store := &stubStore{filingErr: domain.ErrObligationNotFound}
	r := newTestRouter(store)

	body := `{"filing_type":"ANNUAL_RETURN","filing_authority":"IRS","submission_channel":"E_FILE"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/missing/filing-requirements", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestCreateFilingRequirement_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/filing-requirements", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── ListFilingRequirements ───────────────────────────────────────────────────

func TestListFilingRequirements_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{filingList: nil})

	req := httptest.NewRequest(http.MethodGet, "/v1/obligations/o-1/filing-requirements", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "[]\n" {
		t.Errorf("expected empty JSON array, got %q", w.Body.String())
	}
}

func TestListFilingRequirements_ObligationNotFound(t *testing.T) {
	store := &stubStore{filingListErr: domain.ErrObligationNotFound}
	r := newTestRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/v1/obligations/missing/filing-requirements", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
