package handler_test

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

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/handler"
	svcmiddleware "zoiko.io/obligations-svc/internal/middleware"
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

	workflowUpdated *domain.Obligation
	workflowErr     error

	filingReq     *domain.FilingRequirement
	filingErr     error
	filingList    []*domain.FilingRequirement
	filingListErr error

	// Idempotency key methods
	idempotencyChecked bool
	idempotencyResult  struct {
		alreadyProcessed     bool
		existingResourceID   string
		existingResourceType string
		err                  error
	}
	idempotencyStored   bool
	idempotencyStoreErr error

	// Audit context the handler hands to the store, which writes the
	// transactional-outbox envelope from it.
	createParams        domain.CreateObligationParams
	statusActorID       string
	statusCorrelationID string

	workflowObligationID  string
	workflowLinkedID      string
	workflowActorID       string
	workflowCorrelationID string

	// Chunk 10: applicability decisions.
	applicabilityDecision   *domain.ApplicabilityDecision
	applicabilityCreateErr  error
	applicabilityList       []*domain.ApplicabilityDecision
	applicabilityListErr    error
	currentApplicability    *domain.CurrentApplicability
	currentApplicabilityErr error
	applicabilityReplayed   bool
	applicabilityParams     domain.CreateApplicabilityDecisionParams

	// Completion pass.
	lastLimit          int
	updateParams       domain.UpdateObligationParams
	filingStatusParams domain.UpdateFilingStatusParams
	definition         *domain.ObligationDefinition
	definitionErr      error
	definitionParams   domain.CreateDefinitionParams
	newVersionParams   domain.NewDefinitionVersionParams
	mapping            *domain.ObligationEntityMapping
	mappingErr         error
	mappingParams      domain.CreateMappingParams
}

func (s *stubStore) CreateObligation(_ context.Context, params domain.CreateObligationParams) (*domain.Obligation, bool, error) {
	s.createParams = params
	return s.obligation, s.obligationCreated, s.createErr
}

func (s *stubStore) FindObligationByID(_ context.Context, _ string) (*domain.Obligation, error) {
	if s.findErr != nil {
		return nil, s.findErr
	}
	if s.findObligation != nil {
		return s.findObligation, nil
	}
	// The mutating handlers now look the obligation up before authorizing —
	// the parent carries the legal entity, which is the authorization scope.
	// Tests written before that only configured the RESULT of the write, so
	// fall back to it rather than making every one of them restate the same
	// fixture twice. A test that genuinely wants "not found" sets findErr.
	if s.updated != nil {
		return s.updated, nil
	}
	if s.obligation != nil {
		return s.obligation, nil
	}
	return &domain.Obligation{ObligationID: "o-stub", LegalEntityID: "le-1"}, nil
}

func (s *stubStore) ListObligations(_ context.Context, _ domain.ListObligationsFilter) ([]*domain.Obligation, error) {
	return s.list, s.listErr
}

func (s *stubStore) UpdateObligationStatus(_ context.Context, _, _ string, _ int, actorID, correlationID string) (*domain.Obligation, bool, error) {
	s.statusActorID = actorID
	s.statusCorrelationID = correlationID
	return s.updated, s.transitioned, s.updateErr
}

func (s *stubStore) UpdateObligationWorkflow(_ context.Context, obligationID, workflowID, actorID, correlationID string) (*domain.Obligation, error) {
	s.workflowObligationID = obligationID
	s.workflowLinkedID = workflowID
	s.workflowActorID = actorID
	s.workflowCorrelationID = correlationID
	return s.workflowUpdated, s.workflowErr
}

func (s *stubStore) CreateFilingRequirement(_ context.Context, _ domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error) {
	return s.filingReq, s.filingErr
}

func (s *stubStore) ListFilingRequirements(_ context.Context, _ string, limit int) ([]*domain.FilingRequirement, error) {
	s.lastLimit = limit
	return s.filingList, s.filingListErr
}

// ── completion pass: attribute update, filing lifecycle, definitions ─────────

func (s *stubStore) UpdateObligation(_ context.Context, p domain.UpdateObligationParams) (*domain.Obligation, bool, error) {
	s.updateParams = p
	return s.updated, s.transitioned, s.updateErr
}

func (s *stubStore) FindFilingRequirement(_ context.Context, _, _ string) (*domain.FilingRequirement, error) {
	return s.filingReq, s.filingErr
}

func (s *stubStore) UpdateFilingStatus(_ context.Context, p domain.UpdateFilingStatusParams) (*domain.FilingRequirement, bool, error) {
	s.filingStatusParams = p
	return s.filingReq, true, s.filingErr
}

func (s *stubStore) ListFilingTransitions(_ context.Context, _, _ string) ([]*domain.FilingTransition, error) {
	return nil, s.filingErr
}

func (s *stubStore) CreateDefinition(_ context.Context, p domain.CreateDefinitionParams) (*domain.ObligationDefinition, bool, error) {
	s.definitionParams = p
	return s.definition, true, s.definitionErr
}

func (s *stubStore) GetDefinition(_ context.Context, _ string) (*domain.ObligationDefinition, error) {
	return s.definition, s.definitionErr
}

func (s *stubStore) ListDefinitions(_ context.Context, _ string, _ int, _ string) ([]*domain.ObligationDefinition, error) {
	return nil, s.definitionErr
}

func (s *stubStore) ListDefinitionVersions(_ context.Context, _ string) ([]*domain.ObligationDefinitionVersion, error) {
	return nil, s.definitionErr
}

func (s *stubStore) NewDefinitionVersion(_ context.Context, p domain.NewDefinitionVersionParams) (*domain.ObligationDefinition, error) {
	s.newVersionParams = p
	return s.definition, s.definitionErr
}

func (s *stubStore) RetireDefinition(_ context.Context, _ string, _ int, _, _ string) (*domain.ObligationDefinition, error) {
	return s.definition, s.definitionErr
}

func (s *stubStore) CreateMapping(_ context.Context, p domain.CreateMappingParams) (*domain.ObligationEntityMapping, error) {
	s.mappingParams = p
	return s.mapping, s.mappingErr
}

func (s *stubStore) ListMappings(_ context.Context, _ string) ([]*domain.ObligationEntityMapping, error) {
	return nil, s.mappingErr
}

func (s *stubStore) FindMapping(_ context.Context, _, _ string) (*domain.ObligationEntityMapping, error) {
	return s.mapping, s.mappingErr
}

func (s *stubStore) EndMapping(_ context.Context, _, _ string, _ time.Time, _, _ string) (*domain.ObligationEntityMapping, error) {
	return s.mapping, s.mappingErr
}

func (s *stubStore) CheckIdempotencyKey(_ context.Context, _, _ string) (bool, string, string, error) {
	return s.idempotencyResult.alreadyProcessed, s.idempotencyResult.existingResourceID, s.idempotencyResult.existingResourceType, s.idempotencyResult.err
}

func (s *stubStore) StoreIdempotencyKey(_ context.Context, _, _, _, _ string) error {
	s.idempotencyStored = true
	return s.idempotencyStoreErr
}

// ── Chunk 10: applicability decisions ───────────────────────────────────────

func (s *stubStore) CreateApplicabilityDecision(_ context.Context, p domain.CreateApplicabilityDecisionParams) (*domain.ApplicabilityDecision, bool, error) {
	s.applicabilityParams = p
	return s.applicabilityDecision, s.applicabilityReplayed, s.applicabilityCreateErr
}

func (s *stubStore) ListApplicabilityDecisions(_ context.Context, _, _, _ string, limit int) ([]*domain.ApplicabilityDecision, error) {
	s.lastLimit = limit
	return s.applicabilityList, s.applicabilityListErr
}

func (s *stubStore) FindCurrentApplicability(_ context.Context, _, _, _ string) (*domain.CurrentApplicability, error) {
	return s.currentApplicability, s.currentApplicabilityErr
}

// ── stub jurisdiction validator ──────────────────────────────────────────────

type stubValidator struct {
	err error
}

func (v *stubValidator) ValidateExists(_ context.Context, _ string) error {
	return v.err
}

func newTestRouter(s *stubStore) chi.Router {
	return newTestRouterFull(s, &stubValidator{})
}

// stubAuthz GRANTS by default. Tests that care about the gate set err.
type stubAuthz struct {
	err   error
	calls []string // action types, in order
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _, actionType, _ string) error {
	a.calls = append(a.calls, actionType)
	return a.err
}

func newTestRouterFull(s *stubStore, v *stubValidator) chi.Router {
	return newTestRouterAuthz(s, v, &stubAuthz{})
}

// newTestRouterAuthz wires the SAME middleware chain the server does.
//
// TenantContext is not optional here: the handlers read the tenant from the
// request CONTEXT, and only that middleware puts it there. A harness that sets
// the X-Tenant-Id header but skips the middleware runs every test with no
// tenant at all — which is precisely the class of bug this service had, so a
// harness blind to it would be worse than useless.
func newTestRouterAuthz(s *stubStore, v *stubValidator, a *stubAuthz) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, v, a, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

const (
	testTenant      = "11111111-1111-1111-1111-111111111111"
	testPrincipal   = "admin-1"
	testCorrelation = "corr-test-1"
)

// withIdentity stamps the headers the gateway sets on a verified request.
func withIdentity(req *http.Request) *http.Request {
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("X-Correlation-ID", testCorrelation)
	return req
}

// newApplicabilityTestRouter wires a router with applicability-decision
// routes included — the base newTestRouter* helpers above only register
// handler.RegisterRoutes.
//
// Merge note: this arrived from main built on the 4-argument handler.New and a
// bare chi router. It now passes a GRANTING stubAuthz and installs
// TenantContext, matching newTestRouterAuthz — without the middleware every
// request through it would carry no tenant, which this service's store now
// refuses outright.
func newApplicabilityTestRouter(s *stubStore) chi.Router {
	return newApplicabilityTestRouterAuthz(s, &stubAuthz{})
}

// newApplicabilityTestRouterAuthz lets a test supply the authz stub, which the
// applicability routes need now that recording a decision is gated. Without a
// way to inject a DENY, the gate could only ever be tested in its allowing
// direction — and a gate is only proven by the request it refuses.
func newApplicabilityTestRouterAuthz(s *stubStore, a *stubAuthz) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, &stubValidator{}, a, zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterApplicabilityRoutes(r, h)
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
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody())))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if store.createParams.CorrelationID != testCorrelation {
		t.Errorf("expected the correlation to reach the store for the outbox envelope, got %q", store.createParams.CorrelationID)
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
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody())))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on idempotent replay, got %d", w.Code)
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
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["field"] != "source_reference" {
		t.Errorf("expected missing field source_reference, got %q", got["field"])
	}
}

func TestCreateObligation_JurisdictionNotFound(t *testing.T) {
	r := newTestRouterFull(&stubStore{}, &stubValidator{err: domain.ErrJurisdictionNotFound})

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody())))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateObligation_JurisdictionServiceUnavailable_FailsClosed(t *testing.T) {
	r := newTestRouterFull(&stubStore{}, &stubValidator{err: domain.ErrJurisdictionServiceUnavailable})

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody())))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (fail-closed), got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateObligation_Conflict(t *testing.T) {
	store := &stubStore{createErr: domain.ErrConflict}
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody())))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestCreateObligation_StoreUnavailable(t *testing.T) {
	store := &stubStore{createErr: domain.ErrStoreUnavailable}
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations", bytes.NewBufferString(validCreateBody())))
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

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations/o-1", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestGetObligation_NotFound(t *testing.T) {
	store := &stubStore{findErr: domain.ErrObligationNotFound}
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations/missing", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ── ListObligations ──────────────────────────────────────────────────────────

func TestListObligations_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{list: nil})

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations?legal_entity_id=le-1", nil))
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

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations?legal_entity_id=le-1&due_before=not-a-date", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── UpdateObligationStatus ───────────────────────────────────────────────────

func TestUpdateObligationStatus_TransitionToOverdue_PassesAuditContextToStore(t *testing.T) {
	store := &stubStore{
		updated:      &domain.Obligation{ObligationID: "o-1", ObligationStatus: "OVERDUE"},
		transitioned: true,
	}
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{"obligation_status":"OVERDUE"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// The store writes obligation.updated + obligation.overdue to the outbox
	// in the transition's own transaction; the handler must hand it the
	// already-verified actor and correlation for the envelope.
	if store.statusActorID != testPrincipal {
		t.Errorf("expected actor %q handed to the store, got %q", testPrincipal, store.statusActorID)
	}
	if store.statusCorrelationID != testCorrelation {
		t.Errorf("expected correlation %q handed to the store, got %q", testCorrelation, store.statusCorrelationID)
	}
}

func TestUpdateObligationStatus_TransitionToClosed_PassesAuditContextToStore(t *testing.T) {
	store := &stubStore{
		updated:      &domain.Obligation{ObligationID: "o-1", ObligationStatus: "CLOSED"},
		transitioned: true,
	}
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{"obligation_status":"CLOSED"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if store.statusActorID != testPrincipal || store.statusCorrelationID != testCorrelation {
		t.Errorf("expected actor/correlation handed to the store, got actor=%q correlation=%q",
			store.statusActorID, store.statusCorrelationID)
	}
}

func TestUpdateObligationStatus_IdempotentNoOp_StillOK(t *testing.T) {
	store := &stubStore{
		updated:      &domain.Obligation{ObligationID: "o-1", ObligationStatus: "OPEN"},
		transitioned: false,
	}
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{"obligation_status":"OPEN"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestUpdateObligationStatus_InvalidTransition(t *testing.T) {
	store := &stubStore{updateErr: domain.ErrInvalidTransition}
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{"obligation_status":"OPEN"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestUpdateObligationStatus_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/status", bytes.NewBufferString(`{}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestUpdateObligationStatus_NotFound(t *testing.T) {
	store := &stubStore{updateErr: domain.ErrObligationNotFound}
	r := newTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/missing/status", bytes.NewBufferString(`{"obligation_status":"CLOSED"}`)))
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
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/filing-requirements", bytes.NewBufferString(body)))
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
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/missing/filing-requirements", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestCreateFilingRequirement_MissingField(t *testing.T) {
	r := newTestRouter(&stubStore{})

	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/filing-requirements", bytes.NewBufferString(`{}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ── ListFilingRequirements ───────────────────────────────────────────────────

func TestListFilingRequirements_EmptyReturnsArray(t *testing.T) {
	r := newTestRouter(&stubStore{filingList: nil})

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations/o-1/filing-requirements", nil))
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

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations/missing/filing-requirements", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
