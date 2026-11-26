package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/corporate-actions-svc/internal/boardresolutions"
	"zoiko.io/corporate-actions-svc/internal/domain"
	"zoiko.io/corporate-actions-svc/internal/events"
	"zoiko.io/corporate-actions-svc/internal/evidencereq"
)

type stubStore struct {
	actions map[string]*domain.CorporateAction
}

func newStubStore() *stubStore {
	return &stubStore{
		actions: make(map[string]*domain.CorporateAction),
	}
}

func (s *stubStore) CreateAction(_ context.Context, a *domain.CorporateAction) error {
	if a.ActionID == "" {
		a.ActionID = "act-test-001"
	}
	if a.Status == "" {
		a.Status = domain.ActionStatusProposed
	}
	s.actions[a.ActionID] = a
	return nil
}

func (s *stubStore) GetAction(_ context.Context, id string) (*domain.CorporateAction, error) {
	if a, ok := s.actions[id]; ok {
		return a, nil
	}
	return nil, domain.ErrCorporateActionNotFound
}

func (s *stubStore) ListActions(_ context.Context, _, _, _ string) ([]domain.CorporateAction, error) {
	var out []domain.CorporateAction
	for _, a := range s.actions {
		out = append(out, *a)
	}
	return out, nil
}

func (s *stubStore) UpdateAction(_ context.Context, a *domain.CorporateAction) error {
	s.actions[a.ActionID] = a
	return nil
}

func (s *stubStore) ExecuteAction(_ context.Context, id string, req *domain.ExecuteCorporateActionRequest) (*domain.CorporateAction, error) {
	a, ok := s.actions[id]
	if !ok {
		return nil, domain.ErrCorporateActionNotFound
	}
	if a.Status == domain.ActionStatusExecuted {
		return nil, domain.ErrActionAlreadyExecuted
	}
	a.Status = domain.ActionStatusExecuted
	a.ExecutedBy = &req.ExecutedBy
	return a, nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthzClient struct{}

func (s *stubAuthzClient) CheckAllowed(_ context.Context, _, _, _ string) error {
	return nil
}

// stubEvidenceReq grants sufficiency by default, matching a
// SATISFIED/NO_REQUIREMENTS_DEFINED outcome but skipping the network call.
// Tests can set err to exercise the fail-closed path.
type stubEvidenceReq struct {
	err error
}

func (s *stubEvidenceReq) EvaluateSufficient(_ context.Context, _, _, _, _, _, _ string, _ []evidencereq.Artifact) error {
	return s.err
}

// stubResolutions records the resolution lookups it was asked to make and
// answers with err (nil = a PASSED resolution of the same legal entity).
type stubResolutions struct {
	err    error
	called []string
	caller boardresolutions.Caller
}

func (s *stubResolutions) VerifyPassedResolution(_ context.Context, caller boardresolutions.Caller, resolutionID, _ string) error {
	s.called = append(s.called, resolutionID)
	s.caller = caller
	return s.err
}

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, &stubEvidenceReq{}, &stubResolutions{}, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
	r.Header.Set("X-Principal-Id", "principal-test-01")
	return r
}

func TestCreateAction(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateCorporateActionRequest{
		LegalEntityID:   "le-001",
		Title:           "Series B Share Issuance",
		ActionType:      domain.ActionTypeShareIssuance,
		Description:     "Issuance of 1,000,000 Preferred Shares",
		EffectiveDate:   "2026-06-01",
		ValuationAmount: 10000000,
		Currency:        "USD",
		EffectiveFrom:   "2026-01-01",
		// CreatedBy is intentionally absent: attribution should be pinned to
		// the X-Principal-Id header, not supplied by the body.
	}
	w := httptest.NewRecorder()
	h.CreateAction(w, buildRequest(http.MethodPost, "/v1/corporate-actions", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.CorporateAction
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Title != "Series B Share Issuance" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.ActionStatusProposed {
		t.Errorf("expected PROPOSED, got %s", resp.Status)
	}
	// created_by must be the authenticated principal, not an empty string.
	if resp.CreatedBy != "principal-test-01" {
		t.Errorf("expected created_by=principal-test-01 (pinned to X-Principal-Id), got %q", resp.CreatedBy)
	}
}

// TestCreateAction_SelfAttributionMismatch verifies that supplying a
// created_by in the request body that does not match X-Principal-Id is
// rejected 400 — the fix for the SoD bypass described in the audit.
func TestCreateAction_SelfAttributionMismatch(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateCorporateActionRequest{
		LegalEntityID: "le-001",
		Title:         "Fake Attribution Test",
		ActionType:    domain.ActionTypeMerger,
		EffectiveDate: "2026-06-01",
		CreatedBy:     "some-other-principal", // differs from X-Principal-Id
	}
	w := httptest.NewRecorder()
	req := buildRequest(http.MethodPost, "/v1/corporate-actions", body)
	req.Header.Set("X-Principal-Id", "principal-test-01")
	h.CreateAction(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for self-attribution mismatch, got %d — %s", w.Code, w.Body.String())
	}
}

// TestGetAction_RequiresPrincipal verifies that GET /v1/corporate-actions/{id}
// now returns 401 when X-Principal-Id is absent (previously unguarded).
func TestGetAction_RequiresPrincipal(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/corporate-actions/any-id", nil)
	req.Header.Set("X-Tenant-Id", "tenant-test-01")
	// deliberately no X-Principal-Id
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when X-Principal-Id absent, got %d — %s", w.Code, w.Body.String())
	}
}

// TestListActions_RequiresPrincipal verifies that GET /v1/corporate-actions
// now returns 401 when X-Principal-Id is absent.
func TestListActions_RequiresPrincipal(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/corporate-actions", nil)
	req.Header.Set("X-Tenant-Id", "tenant-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when X-Principal-Id absent, got %d — %s", w.Code, w.Body.String())
	}
}

func TestExecuteAction(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Create as principal-test-01 (matches X-Principal-Id in buildRequest)
	body := domain.CreateCorporateActionRequest{
		LegalEntityID: "le-001",
		Title:         "Subsidiary Restructure",
		ActionType:    domain.ActionTypeRestructure,
		EffectiveDate: "2026-05-01",
		// CreatedBy deliberately omitted — pinned to X-Principal-Id
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/corporate-actions", body))
	var created domain.CorporateAction
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Execute as a different principal (not the creator) — SoD satisfied
	execBody := domain.ExecuteCorporateActionRequest{
		ExecutedBy: "another-principal",
	}
	execReq := buildRequest(http.MethodPost, "/v1/corporate-actions/"+created.ActionID+"/execute", execBody)
	execReq.Header.Set("X-Principal-Id", "another-principal")
	wExec := httptest.NewRecorder()
	r.ServeHTTP(wExec, execReq)
	if wExec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wExec.Code, wExec.Body.String())
	}
	var executed domain.CorporateAction
	_ = json.NewDecoder(wExec.Body).Decode(&executed)
	if executed.Status != domain.ActionStatusExecuted {
		t.Errorf("expected EXECUTED, got %s", executed.Status)
	}
}

func TestExecuteAction_EvidenceMissing_Returns422(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, &stubEvidenceReq{err: evidencereq.ErrEvidenceMissing}, &stubResolutions{}, logger)
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// CreatedBy omitted — pinned to X-Principal-Id (principal-test-01)
	body := domain.CreateCorporateActionRequest{
		LegalEntityID: "le-001", Title: "Merger with Acme", ActionType: domain.ActionTypeMerger,
		EffectiveDate: "2026-05-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/corporate-actions", body))
	var created domain.CorporateAction
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Execute as a different principal so SoD is satisfied; the evidence gate
	// is what should reject this.
	execBody := domain.ExecuteCorporateActionRequest{ExecutedBy: "another-principal"}
	execReq := buildRequest(http.MethodPost, "/v1/corporate-actions/"+created.ActionID+"/execute", execBody)
	execReq.Header.Set("X-Principal-Id", "another-principal")
	wExec := httptest.NewRecorder()
	r.ServeHTTP(wExec, execReq)
	if wExec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when required evidence is missing, got %d — %s", wExec.Code, wExec.Body.String())
	}
}

func TestExecuteAction_EvidenceServiceUnavailable_Returns503(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, &stubEvidenceReq{err: evidencereq.ErrServiceUnavailable}, &stubResolutions{}, logger)
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// CreatedBy omitted — pinned to X-Principal-Id (principal-test-01)
	body := domain.CreateCorporateActionRequest{
		LegalEntityID: "le-001", Title: "Merger with Acme", ActionType: domain.ActionTypeMerger,
		EffectiveDate: "2026-05-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/corporate-actions", body))
	var created domain.CorporateAction
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	execBody := domain.ExecuteCorporateActionRequest{ExecutedBy: "another-principal"}
	execReq := buildRequest(http.MethodPost, "/v1/corporate-actions/"+created.ActionID+"/execute", execBody)
	execReq.Header.Set("X-Principal-Id", "another-principal")
	wExec := httptest.NewRecorder()
	r.ServeHTTP(wExec, execReq)
	if wExec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when evidence-requirements-svc is unavailable, got %d — %s", wExec.Code, wExec.Body.String())
	}
}

// TestExecuteAction_SelfApprovalForbidden enforces Segregation of Duties
// (docs/original_doc/zoiko_suite_doc1.txt §12.3): the principal who created
// a corporate action may not be the same principal who executes it.
func TestExecuteAction_SelfApprovalForbidden(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	const creator = "board-member-001"

	// Create the action as `creator` — body CreatedBy matches the header so
	// the self-attribution check passes, and created_by is pinned to `creator`.
	createBody := domain.CreateCorporateActionRequest{
		LegalEntityID: "le-001",
		Title:         "Subsidiary Restructure",
		ActionType:    domain.ActionTypeRestructure,
		EffectiveDate: "2026-05-01",
		// CreatedBy omitted — will be pinned to X-Principal-Id = creator
	}
	createReq := buildRequest(http.MethodPost, "/v1/corporate-actions", createBody)
	createReq.Header.Set("X-Principal-Id", creator)
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, createReq)
	var created domain.CorporateAction
	_ = json.NewDecoder(wCreate.Body).Decode(&created)

	// Attempt to execute as the same principal that created it.
	execBody := domain.ExecuteCorporateActionRequest{
		ExecutedBy: creator,
	}
	execReq := buildRequest(http.MethodPost, "/v1/corporate-actions/"+created.ActionID+"/execute", execBody)
	execReq.Header.Set("X-Principal-Id", creator)
	wExec := httptest.NewRecorder()
	r.ServeHTTP(wExec, execReq)

	if wExec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval rejection, got %d — %s", wExec.Code, wExec.Body.String())
	}
}

// TestUpdateAction_StatusGuard verifies that only PROPOSED actions can be
// updated — the fix for the missing lifecycle status guard in UpdateAction.
func TestUpdateAction_StatusGuard(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	// Insert an EXECUTED action directly into the store stub.
	executedAction := &domain.CorporateAction{
		ActionID:      "act-executed-001",
		LegalEntityID: "le-001",
		Title:         "Already Executed",
		ActionType:    domain.ActionTypeMerger,
		EffectiveDate: "2026-01-01",
		Status:        domain.ActionStatusExecuted,
		CreatedBy:     "principal-test-01",
	}
	h.store.(*stubStore).actions[executedAction.ActionID] = executedAction

	updateBody := domain.UpdateCorporateActionRequest{
		Title: "Attempting to overwrite EXECUTED record",
	}
	updReq := buildRequest(http.MethodPut, "/v1/corporate-actions/act-executed-001", updateBody)
	wUpd := httptest.NewRecorder()
	r.ServeHTTP(wUpd, updReq)

	if wUpd.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for EXECUTED record update, got %d — %s", wUpd.Code, wUpd.Body.String())
	}
}

// --- resolution_id verification (regression for "accepted a resolution_id that
// does not exist in board-resolutions-svc", 2026-10-27 live e2e) ---------------

func createWithResolution(t *testing.T, rv *stubResolutions) *httptest.ResponseRecorder {
	t.Helper()
	logger := zap.NewNop()
	h := New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, &stubEvidenceReq{}, rv, logger)
	body := domain.CreateCorporateActionRequest{
		LegalEntityID: "le-001",
		Title:         "Dividend declaration",
		ActionType:    domain.ActionTypeMerger,
		EffectiveDate: "2026-06-01",
		ResolutionID:  "res-123",
	}
	w := httptest.NewRecorder()
	h.CreateAction(w, buildRequest(http.MethodPost, "/v1/corporate-actions", body))
	return w
}

func TestCreateAction_PassedResolutionIsAccepted(t *testing.T) {
	rv := &stubResolutions{}
	w := createWithResolution(t, rv)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a verified PASSED resolution, got %d — %s", w.Code, w.Body.String())
	}
	if len(rv.called) != 1 || rv.called[0] != "res-123" {
		t.Fatalf("expected one lookup of res-123, got %v", rv.called)
	}
	if rv.caller.PrincipalID != "principal-test-01" {
		t.Errorf("lookup must carry the caller's principal, got %q", rv.caller.PrincipalID)
	}
}

func TestCreateAction_UnverifiableResolutionIsRefused(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"resolution does not exist", boardresolutions.ErrResolutionNotFound, http.StatusUnprocessableEntity},
		{"resolution of another legal entity", boardresolutions.ErrResolutionEntityMismatch, http.StatusUnprocessableEntity},
		{"resolution not passed", boardresolutions.ErrResolutionNotPassed, http.StatusUnprocessableEntity},
		{"board-resolutions-svc unreachable fails closed", boardresolutions.ErrServiceUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := createWithResolution(t, &stubResolutions{err: tc.err})
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d — %s", tc.want, w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateAction_WithoutResolutionDoesNotCallBoardResolutions(t *testing.T) {
	rv := &stubResolutions{err: boardresolutions.ErrServiceUnavailable}
	logger := zap.NewNop()
	h := New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, &stubEvidenceReq{}, rv, logger)
	body := domain.CreateCorporateActionRequest{
		LegalEntityID: "le-001", Title: "No resolution", ActionType: domain.ActionTypeMerger, EffectiveDate: "2026-06-01",
	}
	w := httptest.NewRecorder()
	h.CreateAction(w, buildRequest(http.MethodPost, "/v1/corporate-actions", body))
	if w.Code != http.StatusCreated || len(rv.called) != 0 {
		t.Fatalf("resolution_id is optional: expected 201 and no lookup, got %d and %v", w.Code, rv.called)
	}
}
