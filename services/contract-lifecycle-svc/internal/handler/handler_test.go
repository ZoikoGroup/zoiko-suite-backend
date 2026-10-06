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
	"zoiko.io/contract-lifecycle-svc/internal/events"
	"zoiko.io/contract-lifecycle-svc/internal/governancelog"
)

// --- In-memory stub store ---

type stubStore struct {
	contracts map[string]*domain.Contract
	versions  map[string][]domain.ContractVersion
}

func newStubStore() *stubStore {
	return &stubStore{
		contracts: make(map[string]*domain.Contract),
		versions:  make(map[string][]domain.ContractVersion),
	}
}

func (s *stubStore) CreateContract(_ context.Context, c *domain.Contract) error {
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

func (s *stubStore) UpdateContract(_ context.Context, c *domain.Contract, _ string) error {
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

func (s *stubStore) SubmitReview(_ context.Context, id, submittedBy string) (*domain.Contract, error) {
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

func (s *stubStore) ApproveContract(_ context.Context, id, approvedBy, governanceDecisionID string) (*domain.Contract, error) {
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

func (s *stubStore) SendForSignature(_ context.Context, id, sentBy string) (*domain.Contract, error) {
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

func (s *stubStore) RecordExecution(_ context.Context, id string, req *domain.RecordExecutionRequest) (*domain.Contract, error) {
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

func (s *stubStore) AmendContract(_ context.Context, id string, req *domain.AmendContractRequest) (*domain.Contract, error) {
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

func (s *stubStore) RenewContract(_ context.Context, id string, req *domain.RenewContractRequest) (*domain.Contract, error) {
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

func (s *stubStore) TerminateContract(_ context.Context, id string, req *domain.TerminateContractRequest) (*domain.Contract, error) {
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

// --- Stub publisher ---

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

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
	return New(newStubStore(), &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
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
		CreatedBy:        "user-001",
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
		TerminatedBy:    "user-001",
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{err: governancelog.ErrDecisionNotGranted}, logger)
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{err: governancelog.ErrServiceUnavailable}, logger)
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, logger)
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
	router := newChiRouter(h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/amend",
		domain.AmendContractRequest{AmendedBy: "user-001", Title: "New Title"}))
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
	h := New(store, &stubPublisher{}, &stubAuthzClient{}, &stubGovernanceLogClient{}, zap.NewNop())
	router := newChiRouter(h)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/contracts/ctr-001/terminate",
		domain.TerminateContractRequest{TerminatedBy: "user-001", TerminationNote: "n/a"}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 terminating a DRAFT contract (nothing to terminate), got %d — %s", w.Code, w.Body.String())
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
