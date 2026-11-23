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

	"zoiko.io/contract-lifecycle-svc/internal/authz"
	"zoiko.io/contract-lifecycle-svc/internal/domain"
	"zoiko.io/contract-lifecycle-svc/internal/governancelog"
	"zoiko.io/contract-lifecycle-svc/internal/store"
)

// --- In-memory stub store ---

type stubStore struct {
	contracts map[string]*domain.Contract
	versions  map[string][]domain.ContractVersion
	// lastAudit is the attribution the handler handed to the last write.
	lastAudit store.Audit
}

func newStubStore() *stubStore {
	return &stubStore{
		contracts: make(map[string]*domain.Contract),
		versions:  make(map[string][]domain.ContractVersion),
	}
}

func (s *stubStore) CreateContract(_ context.Context, c *domain.Contract, a store.Audit) error {
	s.lastAudit = a
	if c.ContractID == "" {
		c.ContractID = "ctr-test-001"
	}
	if c.Status == "" {
		c.Status = domain.ContractStatusDraft
	}
	s.contracts[c.ContractID] = c
	return nil
}

func (s *stubStore) GetContract(_ context.Context, id string) (*domain.Contract, error) {
	if c, ok := s.contracts[id]; ok {
		return c, nil
	}
	return nil, domain.ErrContractNotFound
}

func (s *stubStore) ListContracts(_ context.Context, _ string) ([]domain.Contract, error) {
	var out []domain.Contract
	for _, c := range s.contracts {
		out = append(out, *c)
	}
	return out, nil
}

func (s *stubStore) UpdateContract(_ context.Context, c *domain.Contract, _ string, a store.Audit) error {
	s.lastAudit = a
	existing, ok := s.contracts[c.ContractID]
	if !ok {
		return domain.ErrContractNotFound
	}
	if existing.Status != domain.ContractStatusDraft && existing.Status != domain.ContractStatusReview {
		return domain.ErrWrongLifecycleStatus
	}
	s.contracts[c.ContractID] = c
	return nil
}

func (s *stubStore) SubmitReview(_ context.Context, id string, a store.Audit) (*domain.Contract, error) {
	s.lastAudit = a
	submittedBy := a.ActorID
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status != domain.ContractStatusDraft {
		return nil, domain.ErrWrongLifecycleStatus
	}
	c.Status = domain.ContractStatusReview
	c.SubmittedBy = &submittedBy
	return c, nil
}

func (s *stubStore) ApproveContract(_ context.Context, id, governanceDecisionID string, a store.Audit) (*domain.Contract, error) {
	s.lastAudit = a
	approvedBy := a.ActorID
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status != domain.ContractStatusReview {
		return nil, domain.ErrWrongLifecycleStatus
	}
	if c.SubmittedBy != nil && *c.SubmittedBy == approvedBy {
		return nil, domain.ErrSelfApprovalNotAllowed
	}
	c.Status = domain.ContractStatusApproved
	c.ApprovedBy = &approvedBy
	c.GovernanceDecisionID = &governanceDecisionID
	return c, nil
}

func (s *stubStore) SendForSignature(_ context.Context, id string, a store.Audit) (*domain.Contract, error) {
	s.lastAudit = a
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status != domain.ContractStatusApproved {
		return nil, domain.ErrWrongLifecycleStatus
	}
	c.SignatureStatus = domain.SignatureStatusSent
	return c, nil
}

func (s *stubStore) RecordExecution(_ context.Context, id string, req *domain.RecordExecutionRequest, a store.Audit) (*domain.Contract, error) {
	s.lastAudit = a
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status != domain.ContractStatusApproved {
		return nil, domain.ErrWrongLifecycleStatus
	}
	if c.SignatureStatus != domain.SignatureStatusSent && c.SignatureStatus != domain.SignatureStatusPartiallySigned {
		return nil, domain.ErrSignatureNotSent
	}
	c.Status = domain.ContractStatusEffective
	c.SignatureStatus = domain.SignatureStatusCompleted
	c.SignedBy = &req.SignedBy
	return c, nil
}

func (s *stubStore) AmendContract(_ context.Context, id string, req *domain.AmendContractRequest, a store.Audit) (*domain.Contract, error) {
	s.lastAudit = a
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status != domain.ContractStatusEffective {
		return nil, domain.ErrWrongLifecycleStatus
	}
	if req.Title != "" {
		c.Title = req.Title
	}
	c.AmendedBy = &req.AmendedBy
	return c, nil
}

func (s *stubStore) RenewContract(_ context.Context, id string, req *domain.RenewContractRequest, a store.Audit) (*domain.Contract, error) {
	s.lastAudit = a
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status != domain.ContractStatusEffective {
		return nil, domain.ErrWrongLifecycleStatus
	}
	c.EffectiveTo = &req.NewEffectiveTo
	c.RenewedBy = &req.RenewedBy
	return c, nil
}

func (s *stubStore) TerminateContract(_ context.Context, id string, req *domain.TerminateContractRequest, a store.Audit) (*domain.Contract, error) {
	s.lastAudit = a
	c, ok := s.contracts[id]
	if !ok {
		return nil, domain.ErrContractNotFound
	}
	if c.Status.IsFinal() {
		return nil, domain.ErrContractTerminated
	}
	if c.Status == domain.ContractStatusDraft || c.Status == domain.ContractStatusReview {
		return nil, domain.ErrWrongLifecycleStatus
	}
	c.Status = domain.ContractStatusTerminated
	c.TerminatedBy = &req.TerminatedBy
	return c, nil
}

func (s *stubStore) ListContractVersions(_ context.Context, contractID string) ([]domain.ContractVersion, error) {
	return s.versions[contractID], nil
}

// --- Stub authz client ---

// stubAuthzClient grants every request by default, matching the
// authorization-svc contract's decision_outcome field but skipping the
// network call. Tests can flip deny/err to exercise failure paths.
type stubAuthzClient struct {
	deny bool
	err  error
}

func (s *stubAuthzClient) CheckAllowed(_ context.Context, _, _, _ string) error {
	if s.err != nil {
		return s.err
	}
	if s.deny {
		return authz.ErrAuthorizationDenied
	}
	return nil
}

// --- Stub governance-log client ---

// stubGovernanceLogClient grants every verification by default, matching
// the "GRANTED" outcome but skipping the network call. Tests can flip
// err to exercise the fail-closed path.
type stubGovernanceLogClient struct {
	err error
}

func (s *stubGovernanceLogClient) VerifyGranted(_ context.Context, _, _, _, _ string) error {
	return s.err
}

// --- Test helpers ---

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
	r.Header.Set("X-Principal-Id", "user-test-01")
	return r
}

// --- Tests ---

func TestCreateContract(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateContractRequest{
		LegalEntityID:    "le-001",
		ContractType:     domain.ContractTypeVendor,
		Title:            "Cloud Services Agreement",
		CounterpartyID:   "cp-001",
		CounterpartyName: "Acme Cloud",
		EffectiveFrom:    "2026-01-01",
		Currency:         "USD",
		TotalValue:       50000,
		CreatedBy:        "user-test-01", // repeats the principal: accepted
	}
	w := httptest.NewRecorder()
	h.CreateContract(w, buildRequest(http.MethodPost, "/v1/contracts", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.Contract
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Title != "Cloud Services Agreement" {
		t.Errorf("unexpected title: %s", resp.Title)
	}
	if resp.Status != domain.ContractStatusDraft {
		t.Errorf("expected DRAFT, got %s", resp.Status)
	}
}

func TestCreateContract_MissingFields(t *testing.T) {
	h := newTestHandler()
	body := map[string]string{"title": ""}
	w := httptest.NewRecorder()
	h.CreateContract(w, buildRequest(http.MethodPost, "/v1/contracts", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetContract_NotFound(t *testing.T) {
	h := newTestHandler()

	router := newTestRouter(h)
	req := buildRequest(http.MethodGet, "/v1/contracts/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListContracts(t *testing.T) {
	h := newTestHandler()
	w := httptest.NewRecorder()
	h.ListContracts(w, buildRequest(http.MethodGet, "/v1/contracts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestTerminateContract_NotFound(t *testing.T) {
	h := newTestHandler()
	router := newTestRouter(h)
	body := domain.TerminateContractRequest{
		TerminationNote: "Project cancelled",
	}
	req := buildRequest(http.MethodPost, "/v1/contracts/nonexistent/terminate", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// newChiRouter builds the real production router (RegisterRoutes), unlike
// newTestRouter below which is a hand-rolled stub that never actually
// invokes the handler. Needed for any route reading chi.URLParam(r, "id").
func newChiRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r
}

func TestApproveContract_MissingGovernanceDecisionID(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusReview,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
	router := newChiRouter(h)

	body := domain.ApproveContractRequest{}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/approve", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when governance_decision_id is missing, got %d — %s", w.Code, w.Body.String())
	}
}

func TestApproveContract_GovernanceDecisionNotGranted(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusReview,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{err: governancelog.ErrDecisionNotGranted}, logger)
	router := newChiRouter(h)

	body := domain.ApproveContractRequest{GovernanceDecisionID: "dec-001"}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/approve", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when governance decision was not granted, got %d — %s", w.Code, w.Body.String())
	}
}

func TestApproveContract_GovernanceLogUnavailable(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusReview,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{err: governancelog.ErrServiceUnavailable}, logger)
	router := newChiRouter(h)

	body := domain.ApproveContractRequest{GovernanceDecisionID: "dec-001"}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/approve", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when governance log is unavailable, got %d — %s", w.Code, w.Body.String())
	}
}

// Segregation of Duties (LEG-05 "self-approval blocked"): the principal who
// submitted the contract for review may not be the one who approves it.
func TestApproveContract_BySameSubmitter_Returns403(t *testing.T) {
	store := newStubStore()
	submitter := "user-test-01" // matches buildRequest's X-Principal-Id
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusReview, SubmittedBy: &submitter,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
	router := newChiRouter(h)

	body := domain.ApproveContractRequest{GovernanceDecisionID: "dec-001"}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/approve", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 approving a contract submitted by the same principal, got %d — %s", w.Code, w.Body.String())
	}
}

func TestApproveContract_Success(t *testing.T) {
	store := newStubStore()
	submitter := "someone-else"
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusReview, SubmittedBy: &submitter,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
	router := newChiRouter(h)

	body := domain.ApproveContractRequest{GovernanceDecisionID: "dec-001"}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/approve", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.Contract
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != domain.ContractStatusApproved {
		t.Errorf("expected APPROVED, got %s", resp.Status)
	}
	if resp.GovernanceDecisionID == nil || *resp.GovernanceDecisionID != "dec-001" {
		t.Errorf("expected governance_decision_id to be recorded, got %v", resp.GovernanceDecisionID)
	}
}

// RecordExecution before SendForSignature is refused — execution evidence
// with no corresponding signature request is not a real signing event.
func TestRecordExecution_WithoutSendForSignature_Returns409(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusApproved, SignatureStatus: domain.SignatureStatusNotRequested,
	}
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
	router := newChiRouter(h)

	body := domain.RecordExecutionRequest{SignedBy: "user-001"}
	req := buildRequest(http.MethodPost, "/v1/contracts/ctr-001/record-execution", body)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 recording execution before signature was sent, got %d — %s", w.Code, w.Body.String())
	}
}

func TestSendForSignatureThenRecordExecution_MovesToEffective(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusApproved, SignatureStatus: domain.SignatureStatusNotRequested,
	}
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
	router := newChiRouter(h)

	wSend := httptest.NewRecorder()
	router.ServeHTTP(wSend, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/send-for-signature", nil))
	if wSend.Code != http.StatusOK {
		t.Fatalf("send-for-signature: expected 200, got %d — %s", wSend.Code, wSend.Body.String())
	}

	wExec := httptest.NewRecorder()
	router.ServeHTTP(wExec, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/record-execution",
		domain.RecordExecutionRequest{SignedBy: "user-001"}))
	if wExec.Code != http.StatusOK {
		t.Fatalf("record-execution: expected 200, got %d — %s", wExec.Code, wExec.Body.String())
	}
	var resp domain.Contract
	_ = json.NewDecoder(wExec.Body).Decode(&resp)
	if resp.Status != domain.ContractStatusEffective {
		t.Errorf("expected EFFECTIVE, got %s", resp.Status)
	}
	if resp.SignatureStatus != domain.SignatureStatusCompleted {
		t.Errorf("expected signature_status COMPLETED, got %s", resp.SignatureStatus)
	}
}

func TestAmendContract_RequiresEffective(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusDraft,
	}
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
	router := newChiRouter(h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/amend",
		domain.AmendContractRequest{Title: "New Title"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 amending a DRAFT contract, got %d — %s", w.Code, w.Body.String())
	}
}

func TestTerminateContract_DraftIsRefused(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusDraft,
	}
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
	router := newChiRouter(h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/terminate",
		domain.TerminateContractRequest{TerminationNote: "n/a"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 terminating a DRAFT contract (nothing to terminate), got %d — %s", w.Code, w.Body.String())
	}
}

func TestContract_ReadEndpoints_RequirePrincipal(t *testing.T) {
	store := newStubStore()
	store.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusDraft,
	}
	h := New(store, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
	router := newChiRouter(h)

	// GET /v1/contracts without principal
	r1 := httptest.NewRequest(http.MethodGet, "/v1/contracts", nil)
	r1.Header.Set("X-Tenant-Id", "tenant-test-01")
	w1 := httptest.NewRecorder()
	router.ServeHTTP(w1, r1)
	if w1.Code != http.StatusUnauthorized {
		t.Errorf("ListContracts: expected 401 without X-Principal-Id, got %d", w1.Code)
	}

	// GET /v1/contracts/ctr-001 without principal
	r2 := httptest.NewRequest(http.MethodGet, "/v1/contracts/ctr-001", nil)
	r2.Header.Set("X-Tenant-Id", "tenant-test-01")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("GetContract: expected 401 without X-Principal-Id, got %d", w2.Code)
	}

	// GET /v1/contracts/ctr-001/versions without principal
	r3 := httptest.NewRequest(http.MethodGet, "/v1/contracts/ctr-001/versions", nil)
	r3.Header.Set("X-Tenant-Id", "tenant-test-01")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, r3)
	if w3.Code != http.StatusUnauthorized {
		t.Errorf("ListContractVersions: expected 401 without X-Principal-Id, got %d", w3.Code)
	}
}

func newTestRouter(h *Handler) http.Handler {
	// Import chi inline for tests
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/contracts/", func(w http.ResponseWriter, r *http.Request) {
		// Route based on path suffix
		path := r.URL.Path
		switch {
		case len(path) > len("/v1/contracts/") && r.Method == http.MethodGet:
			// Simple stub: treat everything as not found
			http.Error(w, `{"error":"contract not found"}`, http.StatusNotFound)
		case len(path) > len("/v1/contracts/") && r.Method == http.MethodPost:
			http.Error(w, `{"error":"contract not found"}`, http.StatusNotFound)
		}
	})
	return mux
}

// --- Actor identity (the actor of a write is X-Principal-Id) ---

func effectiveContractHandler(t *testing.T) (*stubStore, http.Handler) {
	t.Helper()
	st := newStubStore()
	st.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusEffective,
	}
	return st, newChiRouter(New(st, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop()))
}

func assertCoded(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("expected %d, got %d — %s", status, w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.NewDecoder(w.Body).Decode(&body)
	if body["code"] != code {
		t.Fatalf("expected code %q, got %q", code, body["code"])
	}
}

func TestWrites_BodyActorDisagreeingWithPrincipal_Returns403ActorMismatch(t *testing.T) {
	cases := []struct {
		name, path string
		body       interface{}
	}{
		{"amend", "/v1/contracts/ctr-001/amend", domain.AmendContractRequest{AmendedBy: "someone-else", Title: "x"}},
		{"renew", "/v1/contracts/ctr-001/renew", domain.RenewContractRequest{RenewedBy: "someone-else", NewEffectiveTo: "2030-01-01"}},
		{"terminate", "/v1/contracts/ctr-001/terminate", domain.TerminateContractRequest{TerminatedBy: "someone-else"}},
		{"create", "/v1/contracts", domain.CreateContractRequest{Title: "t", CounterpartyID: "cp", EffectiveFrom: "2026-01-01", LegalEntityID: "le-001", CreatedBy: "someone-else"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, router := effectiveContractHandler(t)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, buildRequest(http.MethodPost, tc.path, tc.body))
			assertCoded(t, w, http.StatusForbidden, "actor_mismatch")
			if st.lastAudit.ActorID != "" {
				t.Fatalf("a refused write must not reach the store, got audit %+v", st.lastAudit)
			}
		})
	}
}

func TestWrites_AbsentBodyActor_IsFilledFromPrincipal(t *testing.T) {
	cases := []struct {
		name, path string
		body       interface{}
		got        func(c *domain.Contract) *string
	}{
		{"amend", "/v1/contracts/ctr-001/amend", map[string]string{"title": "x"}, func(c *domain.Contract) *string { return c.AmendedBy }},
		{"renew", "/v1/contracts/ctr-001/renew", map[string]string{"new_effective_to": "2030-01-01"}, func(c *domain.Contract) *string { return c.RenewedBy }},
		// terminated_by used to be required (400); absent now means "the principal".
		{"terminate", "/v1/contracts/ctr-001/terminate", map[string]string{"termination_note": "n/a"}, func(c *domain.Contract) *string { return c.TerminatedBy }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, router := effectiveContractHandler(t)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, buildRequest(http.MethodPost, tc.path, tc.body))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d — %s", w.Code, w.Body.String())
			}
			if st.lastAudit.ActorID != "user-test-01" {
				t.Fatalf("store must be handed the principal as actor, got %q", st.lastAudit.ActorID)
			}
			if got := tc.got(st.contracts["ctr-001"]); got == nil || *got != "user-test-01" {
				t.Fatalf("expected the principal recorded as actor, got %v", got)
			}
		})
	}
}

func TestWrites_MatchingBodyActor_IsAccepted(t *testing.T) {
	_, router := effectiveContractHandler(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/amend",
		domain.AmendContractRequest{AmendedBy: "user-test-01", Title: "x"}))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when the body repeats the principal, got %d — %s", w.Code, w.Body.String())
	}
}

func TestWrites_WithoutPrincipal_Return401(t *testing.T) {
	_, router := effectiveContractHandler(t)
	for _, path := range []string{"/v1/contracts/ctr-001/amend", "/v1/contracts/ctr-001/renew", "/v1/contracts/ctr-001/terminate"} {
		r := buildRequest(http.MethodPost, path, map[string]string{"new_effective_to": "2030-01-01"})
		r.Header.Del("X-Principal-Id")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401 without X-Principal-Id, got %d", path, w.Code)
		}
	}
}

func TestWrites_AuthzDeny_Returns403(t *testing.T) {
	st := newStubStore()
	st.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusEffective,
	}
	router := newChiRouter(New(st, &stubAuthzClient{deny: true}, &stubGovernanceLogClient{}, zap.NewNop()))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/terminate", map[string]string{}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on authz deny, got %d", w.Code)
	}
	if st.lastAudit.ActorID != "" {
		t.Fatal("a denied write must not reach the store")
	}
}

// RecordExecution: signed_by is signing evidence and may differ from the
// caller; the caller is recorded as the actor (execution_recorded_by).
func TestRecordExecution_SignedByIsEvidence_RecorderIsPrincipal(t *testing.T) {
	st := newStubStore()
	st.contracts["ctr-001"] = &domain.Contract{
		ContractID: "ctr-001", TenantID: "tenant-test-01", LegalEntityID: "le-001",
		Status: domain.ContractStatusApproved, SignatureStatus: domain.SignatureStatusSent,
	}
	router := newChiRouter(New(st, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop()))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/record-execution",
		domain.RecordExecutionRequest{SignedBy: "Jane Counterparty"}))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", w.Code, w.Body.String())
	}
	if st.lastAudit.ActorID != "user-test-01" {
		t.Fatalf("expected the principal as the recording actor, got %q", st.lastAudit.ActorID)
	}
}
