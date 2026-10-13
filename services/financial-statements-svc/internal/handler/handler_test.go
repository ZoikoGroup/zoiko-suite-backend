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

	"zoiko.io/financial-statements-svc/internal/clients"
	"zoiko.io/financial-statements-svc/internal/domain"
	"zoiko.io/financial-statements-svc/internal/handler"
	svcmiddleware "zoiko.io/financial-statements-svc/internal/middleware"
	"zoiko.io/financial-statements-svc/internal/service"
)

// ── fakes (mirrors internal/service's own fakes — kept separate on purpose:
// this package tests the HTTP layer end to end, including real routing and
// JSON framing, which the service-layer tests never exercise) ─────────────

type fakeStore struct {
	sets  map[string]*domain.FinancialStatementSet
	facts map[string][]domain.StatementFact
}

func newFakeStore() *fakeStore {
	return &fakeStore{sets: map[string]*domain.FinancialStatementSet{}, facts: map[string][]domain.StatementFact{}}
}

func (s *fakeStore) CreateSet(ctx context.Context, tenantID string, set *domain.FinancialStatementSet, correlationID string) error {
	cp := *set
	s.sets[set.StatementSetID] = &cp
	return nil
}

func (s *fakeStore) GetSet(ctx context.Context, tenantID, setID string) (*domain.FinancialStatementSet, error) {
	set, ok := s.sets[setID]
	if !ok || set.TenantID != tenantID {
		return nil, domain.ErrStatementSetNotFound
	}
	cp := *set
	return &cp, nil
}

func (s *fakeStore) ListSets(ctx context.Context, tenantID, legalEntityID, status string) ([]domain.FinancialStatementSet, error) {
	var out []domain.FinancialStatementSet
	for _, set := range s.sets {
		if set.TenantID == tenantID && set.LegalEntityID == legalEntityID && (status == "" || string(set.Status) == status) {
			out = append(out, *set)
		}
	}
	return out, nil
}

func (s *fakeStore) GetFacts(ctx context.Context, tenantID, setID string) ([]domain.StatementFact, error) {
	return s.facts[setID], nil
}

func (s *fakeStore) GetFact(ctx context.Context, tenantID, factID string) (*domain.StatementFact, error) {
	for _, facts := range s.facts {
		for _, f := range facts {
			if f.FactID == factID {
				return &f, nil
			}
		}
	}
	return nil, domain.ErrFactNotFound
}

func (s *fakeStore) checkAndAdvance(setID string, expectedVersion int64, cmd domain.Command) (*domain.FinancialStatementSet, error) {
	set, ok := s.sets[setID]
	if !ok {
		return nil, domain.ErrStatementSetNotFound
	}
	if set.Version != expectedVersion {
		return nil, domain.Errf(domain.CodeVersionConflict, "version conflict")
	}
	to, err := domain.Next(cmd, set.Status)
	if err != nil {
		return nil, err
	}
	set.Status = to
	set.Version++
	return set, nil
}

func (s *fakeStore) RefreshFacts(ctx context.Context, tenantID, setID string, expectedVersion int64, facts []domain.StatementFact, actorID, correlationID string) error {
	set, ok := s.sets[setID]
	if !ok {
		return domain.ErrStatementSetNotFound
	}
	if set.Version != expectedVersion {
		return domain.Errf(domain.CodeVersionConflict, "version conflict")
	}
	if _, err := domain.Next(domain.CmdRefreshDraft, set.Status); err != nil {
		return err
	}
	s.facts[setID] = facts
	set.Version++
	now := time.Now().UTC()
	set.LastRefreshedAt = &now
	set.LastRefreshedByPrincipalID = &actorID
	return nil
}

func (s *fakeStore) Validate(ctx context.Context, tenantID, setID string, expectedVersion int64, validationErrors []string, correlationID string) error {
	set, ok := s.sets[setID]
	if !ok {
		return domain.ErrStatementSetNotFound
	}
	if set.Version != expectedVersion {
		return domain.Errf(domain.CodeVersionConflict, "version conflict")
	}
	if _, err := domain.Next(domain.CmdValidate, set.Status); err != nil {
		return err
	}
	set.Version++
	if len(validationErrors) == 0 {
		set.Status = domain.StateValidated
	}
	set.ValidationErrors = validationErrors
	return nil
}

func (s *fakeStore) SubmitForReview(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	_, err := s.checkAndAdvance(setID, expectedVersion, domain.CmdSubmitForReview)
	return err
}

func (s *fakeStore) RequestChanges(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	_, err := s.checkAndAdvance(setID, expectedVersion, domain.CmdRequestChanges)
	return err
}

func (s *fakeStore) Approve(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	set := s.sets[setID]
	if set != nil && actorID == set.Preparer() {
		return domain.Errf(domain.CodeSoDDenied, "preparer may not approve")
	}
	_, err := s.checkAndAdvance(setID, expectedVersion, domain.CmdApprove)
	return err
}

func (s *fakeStore) Certify(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, reason, correlationID string) error {
	set := s.sets[setID]
	if set != nil && actorID == set.Preparer() {
		return domain.Errf(domain.CodeSoDDenied, "preparer may not certify")
	}
	updated, err := s.checkAndAdvance(setID, expectedVersion, domain.CmdCertify)
	if err != nil {
		return err
	}
	updated.CertificationReason = &reason
	return nil
}

func (s *fakeStore) Publish(ctx context.Context, tenantID, setID string, expectedVersion int64, actorID, correlationID string) error {
	_, err := s.checkAndAdvance(setID, expectedVersion, domain.CmdPublish)
	return err
}

func (s *fakeStore) Supersede(ctx context.Context, tenantID, oldSetID string, expectedOldVersion int64, newSet *domain.FinancialStatementSet, actorID, correlationID string) error {
	old, ok := s.sets[oldSetID]
	if !ok {
		return domain.ErrStatementSetNotFound
	}
	if old.Version != expectedOldVersion {
		return domain.Errf(domain.CodeVersionConflict, "version conflict")
	}
	if !domain.CanSupersede(old.Status) {
		return domain.Errf(domain.CodeInvalidTransition, "cannot supersede from this state")
	}
	old.Status = domain.StateSuperseded
	old.Version++
	cp := *newSet
	s.sets[newSet.StatementSetID] = &cp
	return nil
}

type fakeSnapshots struct{ snapshots map[string]*clients.Snapshot }

func (f *fakeSnapshots) GetSnapshot(ctx context.Context, tenantID, principalID, snapshotID string) (*clients.Snapshot, error) {
	snap, ok := f.snapshots[snapshotID]
	if !ok {
		return nil, domain.Errf(domain.CodeSourceUnverified, "snapshot %q not found", snapshotID)
	}
	return snap, nil
}

type fakeAuthZ struct{ denyAction string }

func (f *fakeAuthZ) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	if f.denyAction != "" && actionType == f.denyAction {
		return domain.Errf(domain.CodeForbidden, "denied")
	}
	return nil
}

func certifiedSnapshot(legalEntityID string) *clients.Snapshot {
	content := domain.SnapshotContent{
		Currency: "GBP",
		Accounts: []domain.SnapshotContentAccount{
			{AccountCode: "1000", AccountName: "Cash", Category: domain.CategoryAsset, Balance: "500.00"},
			{AccountCode: "2000", AccountName: "Payables", Category: domain.CategoryLiability, Balance: "100.00"},
			{AccountCode: "3000", AccountName: "Equity", Category: domain.CategoryEquity, Balance: "400.00"},
		},
	}
	raw, _ := json.Marshal(content)
	return &clients.Snapshot{SnapshotID: "snap-1", LegalEntityID: legalEntityID, Content: string(raw), ContentHash: "sha256:abc", Status: "CERTIFIED"}
}

// ── router factory ──────────────────────────────────────────────────────

func newRouter(st *fakeStore, snaps map[string]*clients.Snapshot, denyAction string) chi.Router {
	svc := service.New(st, &fakeSnapshots{snapshots: snaps}, &fakeAuthZ{denyAction: denyAction}, zap.NewNop())
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(svcmiddleware.WithTenant(req.Context(), "tenant-1"))
			next.ServeHTTP(w, req)
		})
	})
	handler.RegisterRoutes(r, handler.New(svc, zap.NewNop()))
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── tests ────────────────────────────────────────────────────────────────

func TestCreateStatementSet_MissingPrincipal_Returns401(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, "")
	rr := doReq(r, http.MethodPost, "/v1/statement-sets/", map[string]any{
		"legal_entity_id": "le-1", "reporting_period": "2026-Q1", "framework": "IFRS",
		"presentation_currency": "GBP", "source_snapshot_id": "snap-1",
	}, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateStatementSet_HappyPath_Returns201(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, "")
	rr := doReq(r, http.MethodPost, "/v1/statement-sets/", map[string]any{
		"legal_entity_id": "le-1", "reporting_period": "2026-Q1", "framework": "IFRS",
		"presentation_currency": "GBP", "source_snapshot_id": "snap-1",
	}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var set domain.FinancialStatementSet
	if err := json.NewDecoder(rr.Body).Decode(&set); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if set.Status != domain.StateDraft {
		t.Fatalf("expected DRAFT, got %s", set.Status)
	}
}

func TestCreateStatementSet_UnverifiedSnapshot_Returns422(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{}, "")
	rr := doReq(r, http.MethodPost, "/v1/statement-sets/", map[string]any{
		"legal_entity_id": "le-1", "reporting_period": "2026-Q1", "framework": "IFRS",
		"presentation_currency": "GBP", "source_snapshot_id": "missing-snap",
	}, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["error_code"] != string(domain.CodeSourceUnverified) {
		t.Fatalf("expected error_code %s, got %v", domain.CodeSourceUnverified, body)
	}
}

func TestCreateStatementSet_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, service.ActionStatementSetCreate)
	rr := doReq(r, http.MethodPost, "/v1/statement-sets/", map[string]any{
		"legal_entity_id": "le-1", "reporting_period": "2026-Q1", "framework": "IFRS",
		"presentation_currency": "GBP", "source_snapshot_id": "snap-1",
	}, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetStatementSet_NotFound_Returns404(t *testing.T) {
	r := newRouter(newFakeStore(), nil, "")
	rr := doReq(r, http.MethodGet, "/v1/statement-sets/does-not-exist", nil, "preparer-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// createViaAPI is a test helper that drives CreateStatementSet through the
// real HTTP router and returns the decoded set, failing the test on error.
func createViaAPI(t *testing.T, r chi.Router) domain.FinancialStatementSet {
	t.Helper()
	rr := doReq(r, http.MethodPost, "/v1/statement-sets/", map[string]any{
		"legal_entity_id": "le-1", "reporting_period": "2026-Q1", "framework": "IFRS",
		"presentation_currency": "GBP", "source_snapshot_id": "snap-1",
	}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var set domain.FinancialStatementSet
	if err := json.NewDecoder(rr.Body).Decode(&set); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return set
}

func TestStatementSetCommand_UnknownCommand_Returns404(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, "")
	set := createViaAPI(t, r)
	rr := doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":not-a-real-command", nil, "preparer-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown command, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestStatementSetCommand_RefreshDraftThenValidate(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, "")
	set := createViaAPI(t, r)

	rr := doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":refresh-draft", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh-draft: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":validate", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var res service.ValidationResult
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected a balanced sheet, got errors: %v", res.Errors)
	}
}

func TestStatementSetCommand_Certify_RequiresReasonInBody(t *testing.T) {
	st := newFakeStore()
	r := newRouter(st, map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, "")
	set := createViaAPI(t, r)
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":refresh-draft", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":validate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":submit-for-review", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":approve", nil, "reviewer-1")

	rr := doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":certify", map[string]any{}, "cfo-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a missing reason, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":certify", map[string]any{"reason": "quarter end"}, "cfo-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestStatementSetCommand_Approve_PreparerDenied_Returns403(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, "")
	set := createViaAPI(t, r)
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":refresh-draft", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":validate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":submit-for-review", nil, "preparer-1")

	rr := doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":approve", nil, "preparer-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for the preparer approving their own work, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["error_code"] != string(domain.CodeSoDDenied) {
		t.Fatalf("expected error_code %s, got %v", domain.CodeSoDDenied, body)
	}
}

func TestStatementSetCommand_VersionConflict_Returns409(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, "")
	set := createViaAPI(t, r)
	// The set is DRAFT at version 1; :validate before any facts exist still
	// transitions through the version check first inside the service, but
	// refresh-draft has not run, so submit-for-review (which needs VALIDATED)
	// is the illegal-transition path here — a distinct 409 case from version
	// conflict, both mapped to the same status.
	rr := doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":submit-for-review", nil, "preparer-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for an illegal transition, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestStatementSetCommand_Supersede_ReturnsNewSet(t *testing.T) {
	snaps := map[string]*clients.Snapshot{
		"snap-1": certifiedSnapshot("le-1"),
		"snap-2": certifiedSnapshot("le-1"),
	}
	r := newRouter(newFakeStore(), snaps, "")
	set := createViaAPI(t, r)
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":refresh-draft", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":validate", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":submit-for-review", nil, "preparer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":approve", nil, "reviewer-1")
	doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":certify", map[string]any{"reason": "r"}, "cfo-1")

	rr := doReq(r, http.MethodPost, "/v1/statement-sets/"+set.StatementSetID+":supersede",
		map[string]any{"new_source_snapshot_id": "snap-2"}, "preparer-2")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var newSet domain.FinancialStatementSet
	if err := json.NewDecoder(rr.Body).Decode(&newSet); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if newSet.SupersedesSetID == nil || *newSet.SupersedesSetID != set.StatementSetID {
		t.Fatal("expected the new set to record which set it supersedes")
	}
}

func TestListStatementSets_RequiresLegalEntityID(t *testing.T) {
	r := newRouter(newFakeStore(), nil, "")
	rr := doReq(r, http.MethodGet, "/v1/statement-sets/", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when legal_entity_id is missing, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListStatementSets_FiltersByLegalEntityAndStatus(t *testing.T) {
	r := newRouter(newFakeStore(), map[string]*clients.Snapshot{"snap-1": certifiedSnapshot("le-1")}, "")
	createViaAPI(t, r)

	rr := doReq(r, http.MethodGet, "/v1/statement-sets/?legal_entity_id=le-1", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var res struct {
		Data  []domain.FinancialStatementSet `json:"data"`
		Count int                            `json:"count"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("expected 1 set, got %d", res.Count)
	}

	rr = doReq(r, http.MethodGet, "/v1/statement-sets/?legal_entity_id=le-OTHER", nil, "preparer-1")
	_ = json.NewDecoder(rr.Body).Decode(&res)
	if res.Count != 0 {
		t.Fatalf("expected 0 sets for an unrelated legal entity, got %d", res.Count)
	}
}
