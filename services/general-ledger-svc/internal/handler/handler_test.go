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

	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
	svcenvelope "zoiko.io/general-ledger-svc/internal/envelope"
	"zoiko.io/general-ledger-svc/internal/handler"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	journals      map[string]*domain.JournalHeader
	lines         map[string][]domain.JournalLine
	byCorrelation map[string]string // correlation_id -> journal_id, mirrors the real partial unique index

	createErr     error
	getErr        error
	listErr       error
	transitionErr error
	reverseErr    error
	sumErr        error

	// Minor units (cents), matching the real store — the balance invariant is
	// decided by exact integer equality, not a float comparison.
	debitTotal  int64
	creditTotal int64

	// lastListFilter records what ListJournals was actually asked for, so a
	// test can assert the tenant the handler passed down rather than only the
	// rows that came back.
	lastListFilter domain.ListJournalsFilter

	trialBalances map[string]*domain.TrialBalanceSnapshot // by snapshot id
	compileErr    error
	getTBErr      error
	// lastCompileArgs records what CompileTrialBalance was actually asked
	// for, so a test can assert the handler passed the right scope down.
	lastCompileLegalEntityID, lastCompileFiscalPeriod string

	accounts         map[string]*domain.Account // by account_id
	accountsByCode   map[string]*domain.Account // by "tenant|code"
	createAccountErr error

	currentMappings map[string]*domain.AccountMapping // by "tenant|mapping_key"
	setMappingErr   error
}

func newStubStore() *stubStore {
	return &stubStore{
		journals:        map[string]*domain.JournalHeader{},
		lines:           map[string][]domain.JournalLine{},
		byCorrelation:   map[string]string{},
		trialBalances:   map[string]*domain.TrialBalanceSnapshot{},
		accounts:        map[string]*domain.Account{},
		accountsByCode:  map[string]*domain.Account{},
		currentMappings: map[string]*domain.AccountMapping{},
	}
}

func (s *stubStore) CreateJournal(_ context.Context, h *domain.JournalHeader, lines []domain.JournalLine) ([]domain.JournalLine, bool, error) {
	if s.createErr != nil {
		return nil, false, s.createErr
	}
	if h.CorrelationID != "" {
		if existingID, ok := s.byCorrelation[h.CorrelationID]; ok {
			existing := s.journals[existingID]
			*h = *existing
			return s.lines[existingID], false, nil
		}
		s.byCorrelation[h.CorrelationID] = h.JournalID
	}
	s.journals[h.JournalID] = h
	s.lines[h.JournalID] = lines
	return lines, true, nil
}

func (s *stubStore) GetJournal(_ context.Context, journalID string) (*domain.JournalHeader, []domain.JournalLine, error) {
	if s.getErr != nil {
		return nil, nil, s.getErr
	}
	h, ok := s.journals[journalID]
	if !ok {
		return nil, nil, nil
	}
	return h, s.lines[journalID], nil
}

func (s *stubStore) GetJournalByCorrelationID(_ context.Context, tenantID, correlationID string) (*domain.JournalHeader, []domain.JournalLine, error) {
	if s.getErr != nil {
		return nil, nil, s.getErr
	}
	journalID, ok := s.byCorrelation[correlationID]
	if !ok {
		return nil, nil, nil
	}
	h, ok := s.journals[journalID]
	if !ok || h.TenantID != tenantID {
		return nil, nil, nil
	}
	return h, s.lines[journalID], nil
}

func (s *stubStore) ListJournals(_ context.Context, filter domain.ListJournalsFilter) ([]domain.JournalHeader, error) {
	s.lastListFilter = filter
	if s.listErr != nil {
		return nil, s.listErr
	}
	// Filters by tenant like the real store, so a test that asks for another
	// tenant's ledger cannot pass merely because the stub ignored the scope.
	var out []domain.JournalHeader
	for _, h := range s.journals {
		if h.TenantID == filter.TenantID {
			out = append(out, *h)
		}
	}
	return out, nil
}

func (s *stubStore) TransitionJournal(_ context.Context, _, journalID string, from, to domain.JournalStatus, actor string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	h, ok := s.journals[journalID]
	if !ok || h.Status != from {
		return domain.ErrInvalidTransition
	}
	h.Status = to
	return nil
}

// ReverseJournal mirrors the real store's atomicity: if the original is no
// longer FINALIZED the reversing journal is NOT created either. A stub that
// created it anyway would let the handler's double-counting regression pass.
func (s *stubStore) ReverseJournal(
	ctx context.Context,
	_, originalJournalID string,
	reversing *domain.JournalHeader,
	reversingLines []domain.JournalLine,
	actorPrincipalID string,
) ([]domain.JournalLine, bool, error) {
	if s.reverseErr != nil {
		return nil, false, s.reverseErr
	}
	if reversing.CorrelationID != "" {
		if existingID, ok := s.byCorrelation[reversing.CorrelationID]; ok {
			existing := s.journals[existingID]
			*reversing = *existing
			return s.lines[existingID], false, nil
		}
	}

	original, ok := s.journals[originalJournalID]
	if !ok || original.Status != domain.JournalStatusFinalized {
		return nil, false, domain.ErrInvalidTransition
	}

	if reversing.CorrelationID != "" {
		s.byCorrelation[reversing.CorrelationID] = reversing.JournalID
	}
	s.journals[reversing.JournalID] = reversing
	s.lines[reversing.JournalID] = reversingLines

	original.Status = domain.JournalStatusReversed
	original.ReversedByPrincipalID = &actorPrincipalID
	return reversingLines, true, nil
}

func (s *stubStore) SumLines(_ context.Context, _, _ string) (int64, int64, error) {
	return s.debitTotal, s.creditTotal, s.sumErr
}

// CompileTrialBalance mirrors the real store's own filtering rule (only
// FINALIZED/REVERSED journals contribute) and aggregation (net = debit -
// credit per account) against the stub's own journals/lines, rather than
// returning a canned result — a stub that faked this would let a handler
// regression that passes the wrong scope down go uncaught.
func (s *stubStore) CompileTrialBalance(_ context.Context, tenantID, legalEntityID, fiscalPeriod, principalID string) (*domain.TrialBalanceSnapshot, error) {
	s.lastCompileLegalEntityID, s.lastCompileFiscalPeriod = legalEntityID, fiscalPeriod
	if s.compileErr != nil {
		return nil, s.compileErr
	}
	snap := &domain.TrialBalanceSnapshot{
		TrialBalanceSnapshotID: "tb-" + legalEntityID + "-" + fiscalPeriod,
		TenantID:               tenantID,
		LegalEntityID:          legalEntityID,
		FiscalPeriod:           fiscalPeriod,
		CompiledByPrincipalID:  principalID,
	}
	balances := map[string]float64{}
	for _, h := range s.journals {
		if h.TenantID != tenantID || h.LegalEntityID != legalEntityID || h.FiscalPeriod != fiscalPeriod {
			continue
		}
		if h.Status != domain.JournalStatusFinalized && h.Status != domain.JournalStatusReversed {
			continue
		}
		snap.LedgerWatermark++
		for _, l := range s.lines[h.JournalID] {
			balances[l.AccountCode] += l.DebitAmount - l.CreditAmount
		}
	}
	for code, bal := range balances {
		snap.Lines = append(snap.Lines, domain.TrialBalanceLine{AccountCode: code, NetBalance: bal})
	}
	s.trialBalances[snap.TrialBalanceSnapshotID] = snap
	return snap, nil
}

func (s *stubStore) GetTrialBalance(_ context.Context, tenantID, snapshotID string) (*domain.TrialBalanceSnapshot, error) {
	if s.getTBErr != nil {
		return nil, s.getTBErr
	}
	snap, ok := s.trialBalances[snapshotID]
	if !ok || snap.TenantID != tenantID {
		return nil, domain.ErrTrialBalanceNotFound
	}
	return snap, nil
}

func (s *stubStore) CreateAccount(_ context.Context, a *domain.Account) error {
	if s.createAccountErr != nil {
		return s.createAccountErr
	}
	if a.ParentAccountID != nil {
		if _, ok := s.accounts[*a.ParentAccountID]; !ok {
			return domain.ErrParentAccountNotFound
		}
	}
	key := a.TenantID + "|" + a.AccountCode
	if _, exists := s.accountsByCode[key]; exists {
		return domain.ErrAccountAlreadyExists
	}
	s.accounts[a.AccountID] = a
	s.accountsByCode[key] = a
	return nil
}

func (s *stubStore) GetAccountByCode(_ context.Context, tenantID, accountCode string) (*domain.Account, error) {
	a, ok := s.accountsByCode[tenantID+"|"+accountCode]
	if !ok {
		return nil, domain.ErrAccountNotFound
	}
	return a, nil
}

func (s *stubStore) ListAccounts(_ context.Context, tenantID string) ([]domain.Account, error) {
	var out []domain.Account
	for _, a := range s.accounts {
		if a.TenantID == tenantID {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (s *stubStore) DeactivateAccount(_ context.Context, tenantID, accountCode string) error {
	a, ok := s.accountsByCode[tenantID+"|"+accountCode]
	if !ok {
		return domain.ErrAccountNotFound
	}
	a.Status = "INACTIVE"
	return nil
}

// SetAccountMapping mirrors the real store's own validation rule (the
// target account must exist and be ACTIVE) against the stub's own
// accounts, rather than trusting the caller — a stub that skipped this
// would let a handler regression that forgets to validate go uncaught.
func (s *stubStore) SetAccountMapping(_ context.Context, m *domain.AccountMapping) error {
	if s.setMappingErr != nil {
		return s.setMappingErr
	}
	a, ok := s.accountsByCode[m.TenantID+"|"+m.AccountCode]
	if !ok || a.Status != "ACTIVE" {
		return domain.ErrMappingTargetAccountInvalid
	}
	s.currentMappings[m.TenantID+"|"+m.MappingKey] = m
	return nil
}

func (s *stubStore) GetCurrentAccountMapping(_ context.Context, tenantID, mappingKey string) (*domain.AccountMapping, error) {
	m, ok := s.currentMappings[tenantID+"|"+mappingKey]
	if !ok {
		return nil, domain.ErrAccountMappingNotFound
	}
	return m, nil
}

func (s *stubStore) ListAccountMappings(_ context.Context, tenantID string) ([]domain.AccountMapping, error) {
	var out []domain.AccountMapping
	for _, m := range s.currentMappings {
		if m.TenantID == tenantID {
			out = append(out, *m)
		}
	}
	return out, nil
}

// Compile-time proof the stub still satisfies the contract the handler
// depends on — a stub that has silently fallen behind the interface is how a
// green test suite stops meaning anything.
var _ handler.Store = (*stubStore)(nil)

type stubPublisher struct {
	created, validated, posted, reversed int
}

func (p *stubPublisher) PublishJournalCreated(_ context.Context, _ domain.JournalHeader) { p.created++ }
func (p *stubPublisher) PublishJournalValidated(_ context.Context, _ domain.JournalHeader) {
	p.validated++
}
func (p *stubPublisher) PublishJournalPosted(_ context.Context, _ domain.JournalHeader) { p.posted++ }
func (p *stubPublisher) PublishJournalReversed(_ context.Context, _ domain.JournalHeader, _ string) {
	p.reversed++
}

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

type stubClose struct {
	err error
}

func (c *stubClose) CheckPeriodOpen(_ context.Context, _, _, _ string) error { return c.err }

// Ensure stubClose satisfies the interface at compile-time.
var _ close.Client = (*stubClose)(nil)

func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ) chi.Router {
	return newRouterWithClose(s, p, a, &stubClose{})
}

func newRouterWithClose(s *stubStore, p *stubPublisher, a *stubAuthZ, c close.Client) chi.Router {
	r := chi.NewRouter()
	// The real TenantContext middleware, not a hand-rolled context stuffer:
	// tenant scope reaching the handler is now part of what these tests cover,
	// and a bare router would test a request pipeline this service never runs.
	r.Use(svcmiddleware.TenantContext())

	// The real §4 envelope middleware, pinned to observe mode.
	//
	// The handler reads book_id and evidence_refs off the envelope, so it has to
	// be parsed here or those code paths are never exercised. Observe rather
	// than the deployed write-strict because these are general-ledger tests:
	// what the envelope refuses is uniform middleware behaviour, covered once in
	// internal/envelope, and re-asserting it here would only mean every ledger
	// test had to carry a full envelope to reach the ledger logic it is about.
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))

	h := handler.New(s, p, a, c, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// testTenantID is the verified tenant every request carries unless a test is
// specifically about tenant scope. It matches validCreateReq's body tenant_id.
const testTenantID = "t1"

func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, principalID, testTenantID)
}

// doRequestAs sends a request as a caller the gateway verified as tenantID.
// An empty tenantID sends no X-Tenant-Id at all — a request that never passed
// ForwardAuth.
func doRequestAs(r chi.Router, method, path string, body any, principalID, tenantID string) *httptest.ResponseRecorder {
	return sendRequest(r, method, path, body, principalID, tenantID, nil)
}

// doRequestWithHeaders sends a request carrying extra §4 envelope headers, for
// the tests that cover inputs the handler reads off the envelope rather than
// the body — book_id and evidence_refs.
func doRequestWithHeaders(r chi.Router, method, path string, body any, principalID string, headers map[string]string) *httptest.ResponseRecorder {
	return sendRequest(r, method, path, body, principalID, testTenantID, headers)
}

func sendRequest(r chi.Router, method, path string, body any, principalID, tenantID string, headers map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ── CreateJournal ────────────────────────────────────────────────────────────

func validCreateReq() domain.CreateJournalRequest {
	return domain.CreateJournalRequest{
		TenantID:      "t1",
		LegalEntityID: "e1",
		FiscalPeriod:  "2026-07",
		Description:   "test journal",
		CorrelationID: "corr-1",

		// ACC-03 required business/source inputs. Posting date deliberately
		// later than transaction date — the ordinary case of a document dated
		// one day and posted another.
		JournalType:     domain.JournalTypeStandard,
		TransactionDate: domain.NewDate(2026, time.July, 28),
		PostingDate:     domain.NewDate(2026, time.July, 31),
		CurrencyCode:    "GBP",

		Lines: []domain.CreateJournalLineInput{
			{AccountCode: "1000", DebitAmount: 100},
			{AccountCode: "4000", CreditAmount: 100},
		},
	}
}

func TestCreateJournal_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateJournal_MissingPrincipalHeader_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d", rec.Code)
	}
}

func TestCreateJournal_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authorization-svc denies, got %d", rec.Code)
	}
}

func TestCreateJournal_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationServiceUnavailable})
	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable (fail closed), got %d", rec.Code)
	}
}

func TestCreateJournal_NoLines_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.Lines = nil
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a journal with no lines, got %d", rec.Code)
	}
}

func TestCreateJournal_LineWithBothDebitAndCredit_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.Lines[0].CreditAmount = 50 // now has both debit and credit set — invalid
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a line with both debit and credit set, got %d", rec.Code)
	}
}

func TestCreateJournal_MissingCorrelationID_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	req := validCreateReq()
	req.CorrelationID = ""
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no correlation_id (required as the idempotency key), got %d", rec.Code)
	}
}

func TestCreateJournal_RetriedCorrelationID_ReturnsOriginalNotDuplicate(t *testing.T) {
	s := newStubStore()
	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})

	req := validCreateReq()
	rec1 := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec1.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var first domain.JournalWithLines
	if err := json.Unmarshal(rec1.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	// Simulate a client retry after a network timeout: identical request,
	// same correlation_id.
	rec2 := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried correlation_id (idempotent replay, not a new journal), got %d: %s", rec2.Code, rec2.Body.String())
	}
	var second domain.JournalWithLines
	if err := json.Unmarshal(rec2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second response: %v", err)
	}

	if second.JournalID != first.JournalID {
		t.Fatalf("retried call returned a different journal_id (%s) than the original (%s) — this is the exact duplicate-posting bug idempotency keys exist to prevent",
			second.JournalID, first.JournalID)
	}
	if len(s.journals) != 1 {
		t.Fatalf("expected exactly 1 journal to exist in the store after a retry, got %d", len(s.journals))
	}
	if pub.created != 1 {
		t.Fatalf("expected journal.created to publish exactly once (not on the replay), got %d", pub.created)
	}
}

// ── ValidateJournal ──────────────────────────────────────────────────────────

func TestValidateJournal_Unbalanced_Rejected(t *testing.T) {
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusPending}
	s.debitTotal, s.creditTotal = 100, 90 // deliberately unbalanced

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j1/validate", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for an unbalanced journal, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j1"].Status != domain.JournalStatusPending {
		t.Fatalf("an unbalanced journal must NOT transition to VALIDATED, still got %s", s.journals["j1"].Status)
	}
}

func TestValidateJournal_Balanced_Succeeds(t *testing.T) {
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusPending}
	s.debitTotal, s.creditTotal = 100, 100

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j1/validate", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a balanced journal, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j1"].Status != domain.JournalStatusValidated {
		t.Fatalf("expected status VALIDATED, got %s", s.journals["j1"].Status)
	}
	if pub.validated != 1 {
		t.Fatalf("expected journal.validated to be published once, got %d", pub.validated)
	}
}

// ── PostJournal ──────────────────────────────────────────────────────────────

func TestPostJournal_FromPending_Rejected(t *testing.T) {
	// Tri-Phase Commit must be sequential: PENDING -> FINALIZED directly (skipping
	// VALIDATED) is not a legal transition.
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusPending}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j1/post", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 posting a PENDING (not VALIDATED) journal, got %d", rec.Code)
	}
}

func TestPostJournal_FromValidated_Succeeds(t *testing.T) {
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusValidated}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j1/post", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j1"].Status != domain.JournalStatusFinalized {
		t.Fatalf("expected status FINALIZED, got %s", s.journals["j1"].Status)
	}
	if pub.posted != 1 {
		t.Fatalf("expected journal.posted to be published once, got %d", pub.posted)
	}
}

// ── ReverseJournal ───────────────────────────────────────────────────────────

func TestReverseJournal_OnlyFinalizedIsReversible(t *testing.T) {
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusValidated}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j1/reverse",
		domain.ReverseJournalRequest{Reason: "correction", CorrelationID: "corr-reverse-1"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 reversing a non-FINALIZED journal, got %d", rec.Code)
	}
}

func TestReverseJournal_Finalized_CreatesInvertedReversingJournal(t *testing.T) {
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusFinalized}
	s.lines["j1"] = []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100, CreditAmount: 0},
		{AccountCode: "4000", DebitAmount: 0, CreditAmount: 100},
	}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j1/reverse",
		domain.ReverseJournalRequest{Reason: "correction", CorrelationID: "corr-reverse-1"}, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var result domain.JournalWithLines
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(result.Lines) != 2 {
		t.Fatalf("expected 2 lines on the reversing journal, got %d", len(result.Lines))
	}
	// Debit/credit must be inverted relative to the original.
	if result.Lines[0].DebitAmount != 0 || result.Lines[0].CreditAmount != 100 {
		t.Fatalf("expected line 0 inverted to credit=100/debit=0, got debit=%v credit=%v",
			result.Lines[0].DebitAmount, result.Lines[0].CreditAmount)
	}
	if result.Lines[1].DebitAmount != 100 || result.Lines[1].CreditAmount != 0 {
		t.Fatalf("expected line 1 inverted to debit=100/credit=0, got debit=%v credit=%v",
			result.Lines[1].DebitAmount, result.Lines[1].CreditAmount)
	}

	// The original must now be REVERSED, and its own lines must be untouched
	// (never hard-edited) — only its status column changed.
	if s.journals["j1"].Status != domain.JournalStatusReversed {
		t.Fatalf("expected original journal status REVERSED, got %s", s.journals["j1"].Status)
	}
	if s.lines["j1"][0].DebitAmount != 100 {
		t.Fatalf("original journal's lines must never be mutated by a reversal")
	}
	if pub.reversed != 1 {
		t.Fatalf("expected journal.reversed to be published once, got %d", pub.reversed)
	}
}

// ── GetJournal / ListJournals ────────────────────────────────────────────────

func TestGetJournal_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/journals/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestListJournals_RequiresVerifiedTenantScope(t *testing.T) {
	// tenant_id in the query string is no longer what scopes this read, so
	// supplying one cannot substitute for having passed identity verification.
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodGet, "/v1/journals/?tenant_id=t1", nil, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no verified X-Tenant-Id, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── tenant scope ─────────────────────────────────────────────────────────────

func TestListJournals_ForeignTenantIDInQuery_Refused(t *testing.T) {
	// The leak this closes: ListJournals passed ?tenant_id straight to the
	// WHERE clause, so any verified caller could read any tenant's entire
	// general ledger without needing to guess a single id.
	s := newStubStore()
	s.journals["theirs"] = &domain.JournalHeader{
		JournalID: "theirs", TenantID: "t2", LegalEntityID: "e9",
		Status: domain.JournalStatusFinalized, Description: "another tenant's ledger",
	}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodGet, "/v1/journals/?tenant_id=t2", nil, "principal-1", "t1")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 asking for another tenant's ledger, got %d: %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("another tenant's ledger")) {
		t.Fatal("response leaked the other tenant's journal")
	}
}

func TestListJournals_ScopedToVerifiedTenant_NotQueryParam(t *testing.T) {
	s := newStubStore()
	s.journals["mine"] = &domain.JournalHeader{JournalID: "mine", TenantID: "t1", Status: domain.JournalStatusPending}
	s.journals["theirs"] = &domain.JournalHeader{JournalID: "theirs", TenantID: "t2", Status: domain.JournalStatusPending}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	// No tenant_id query parameter at all: the verified scope is enough, and
	// is what the store must be asked for.
	rec := doRequestAs(r, http.MethodGet, "/v1/journals/", nil, "principal-1", "t1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lastListFilter.TenantID != "t1" {
		t.Fatalf("store was asked for tenant %q, expected the verified tenant t1", s.lastListFilter.TenantID)
	}

	var journals []domain.JournalHeader
	if err := json.Unmarshal(rec.Body.Bytes(), &journals); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(journals) != 1 || journals[0].JournalID != "mine" {
		t.Fatalf("expected only this tenant's journal, got %+v", journals)
	}
}

func TestListJournals_EmptyLedger_IsEmptyArrayNotNull(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/journals/", nil, "principal-1")
	if got := bytes.TrimSpace(rec.Body.Bytes()); string(got) != "[]" {
		t.Fatalf("expected an empty JSON array, got %q — null forces every caller to special-case it", got)
	}
}

func TestListJournals_InvalidLimit_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	for _, limit := range []string{"0", "-5", "all"} {
		rec := doRequest(r, http.MethodGet, "/v1/journals/?limit="+limit, nil, "principal-1")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s: expected 400, got %d", limit, rec.Code)
		}
	}
}

func TestCreateJournal_BodyTenantDisagreesWithVerifiedScope_Refused(t *testing.T) {
	// The write half of the same leak: the row used to be inserted under the
	// body's tenant_id while the transaction was scoped to the verified one,
	// filing a journal into a ledger the caller has no relationship with.
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	req := validCreateReq()
	req.TenantID = "t2" // caller is verified as t1

	rec := doRequestAs(r, http.MethodPost, "/v1/journals/", req, "principal-1", "t1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Fatalf("expected no journal to be written, got %d", len(s.journals))
	}
}

func TestCreateJournal_MissingTenantScope_Returns401(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequestAs(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no verified tenant scope, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.journals) != 0 {
		t.Fatalf("expected no journal to be written, got %d", len(s.journals))
	}
}

func TestCreateJournal_MalformedUUID_Returns400Not503(t *testing.T) {
	// SQLSTATE 22P02 from a uuid column used to reach the caller as
	// "store_unavailable" — a typo reported as an outage.
	s := newStubStore()
	s.createErr = domain.ErrInvalidIdentifier

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-UUID identifier, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── reversal integrity ───────────────────────────────────────────────────────

func TestReverseJournal_ReversingJournalCarriesLinkAndPostingStamps(t *testing.T) {
	// reversal_of_journal_id and the posted_* pair were absent from the INSERT
	// column list, so the link back to the original — the whole basis for
	// tracing a correction — was generated, returned, and then dropped.
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusFinalized}
	s.lines["j1"] = []domain.JournalLine{{AccountCode: "1000", DebitAmount: 100}}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j1/reverse",
		domain.ReverseJournalRequest{Reason: "correction", CorrelationID: "corr-reverse-1"}, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var result domain.JournalWithLines
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// What the response claims must be what was handed to the store — the
	// defect was precisely a response that outlived the row behind it.
	stored, ok := s.journals[result.JournalID]
	if !ok {
		t.Fatalf("the reversing journal in the response (%s) was never written to the store", result.JournalID)
	}
	if stored.ReversalOfJournalID == nil || *stored.ReversalOfJournalID != "j1" {
		t.Fatalf("stored reversing journal must point back at j1, got %v", stored.ReversalOfJournalID)
	}
	if stored.Status != domain.JournalStatusFinalized {
		t.Fatalf("a reversal is an authoritative posting, expected FINALIZED, got %s", stored.Status)
	}
	if stored.PostedByPrincipalID == nil || *stored.PostedByPrincipalID != "principal-1" {
		t.Fatalf("stored reversing journal must record who posted it, got %v", stored.PostedByPrincipalID)
	}
	if result.ReversalOfJournalID == nil || *result.ReversalOfJournalID != "j1" {
		t.Fatalf("response must carry the reversal link too, got %v", result.ReversalOfJournalID)
	}
}

func TestReverseJournal_TransitionRefused_LeavesNoOrphanReversal(t *testing.T) {
	// The double-counting window: the reversing journal was created FINALIZED
	// by one call and the original marked REVERSED by a second. If the second
	// failed, the books held a posting and its inverse, both live.
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusFinalized}
	s.lines["j1"] = []domain.JournalLine{{AccountCode: "1000", DebitAmount: 100}}
	s.reverseErr = domain.ErrInvalidTransition // the original stopped being FINALIZED

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/j1/reverse",
		domain.ReverseJournalRequest{Reason: "correction", CorrelationID: "corr-reverse-1"}, "principal-1")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when the original is no longer FINALIZED, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.journals) != 1 {
		t.Fatalf("a refused reversal must leave no reversing journal behind, store holds %d journals", len(s.journals))
	}
	if pub.reversed != 0 {
		t.Fatalf("journal.reversed must not be published for a reversal that did not happen, got %d", pub.reversed)
	}
}

func TestReverseJournal_RetriedCorrelationID_ReturnsStoredReversalNotAFreshID(t *testing.T) {
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{JournalID: "j1", TenantID: "t1", LegalEntityID: "e1", Status: domain.JournalStatusFinalized}
	s.lines["j1"] = []domain.JournalLine{{AccountCode: "1000", DebitAmount: 100}}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{})
	body := domain.ReverseJournalRequest{Reason: "correction", CorrelationID: "corr-reverse-1"}

	rec1 := doRequest(r, http.MethodPost, "/v1/journals/j1/reverse", body, "principal-1")
	if rec1.Code != http.StatusCreated {
		t.Fatalf("expected 201 on the first reversal, got %d: %s", rec1.Code, rec1.Body.String())
	}
	var first domain.JournalWithLines
	if err := json.Unmarshal(rec1.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}

	rec2 := doRequest(r, http.MethodPost, "/v1/journals/j1/reverse", body, "principal-1")
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 on the retry, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var second domain.JournalWithLines
	if err := json.Unmarshal(rec2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second: %v", err)
	}

	if second.JournalID != first.JournalID {
		t.Fatalf("the retry returned journal_id %s, but the stored reversal is %s — a fresh id for a row that was never written",
			second.JournalID, first.JournalID)
	}
	if pub.reversed != 1 {
		t.Fatalf("journal.reversed must publish once across a retry, got %d", pub.reversed)
	}
}

// ── Trial Balance (ACC-15) ────────────────────────────────────────────────────

// createFinalizedJournal drives a journal through the real Create -> Validate
// -> Post lifecycle over HTTP (not a shortcut that writes FINALIZED directly
// into the stub) — so a trial balance test genuinely exercises "only
// FINALIZED/REVERSED journals contribute," not a fixture that assumes it.
func createFinalizedJournal(t *testing.T, r chi.Router, req domain.CreateJournalRequest) string {
	t.Helper()
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created domain.JournalWithLines
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	recV := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/validate", nil, "principal-1")
	if recV.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", recV.Code, recV.Body.String())
	}
	recP := doRequest(r, http.MethodPost, "/v1/journals/"+created.JournalID+"/post", nil, "principal-1")
	if recP.Code != http.StatusOK {
		t.Fatalf("post: expected 200, got %d: %s", recP.Code, recP.Body.String())
	}
	return created.JournalID
}

func TestCompileTrialBalance_OnlyFinalizedJournalsContribute(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})

	// One FINALIZED journal (100 debit to 1000, 100 credit to 4000)...
	createFinalizedJournal(t, r, validCreateReq())

	// ...and one left PENDING (never validated/posted) — must NOT contribute.
	pendingReq := validCreateReq()
	pendingReq.CorrelationID = "corr-pending"
	doRequest(r, http.MethodPost, "/v1/journals/", pendingReq, "principal-1")

	rec := doRequest(r, http.MethodPost, "/v1/trial-balance/compile",
		domain.CompileTrialBalanceRequest{LegalEntityID: "e1", FiscalPeriod: "2026-07"}, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var snap domain.TrialBalanceSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.LedgerWatermark != 1 {
		t.Errorf("expected watermark reflecting exactly 1 included journal, got %d", snap.LedgerWatermark)
	}
	byAccount := map[string]float64{}
	for _, l := range snap.Lines {
		byAccount[l.AccountCode] = l.NetBalance
	}
	if byAccount["1000"] != 100 || byAccount["4000"] != -100 {
		t.Errorf("expected only the FINALIZED journal's lines counted, got %+v", byAccount)
	}
}

func TestCompileTrialBalance_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/trial-balance/compile",
		domain.CompileTrialBalanceRequest{LegalEntityID: "e1"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCompileTrialBalance_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	rec := doRequest(r, http.MethodPost, "/v1/trial-balance/compile",
		domain.CompileTrialBalanceRequest{LegalEntityID: "e1", FiscalPeriod: "2026-07"}, "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestGetTrialBalance_RoundTrip(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createFinalizedJournal(t, r, validCreateReq())

	compileRec := doRequest(r, http.MethodPost, "/v1/trial-balance/compile",
		domain.CompileTrialBalanceRequest{LegalEntityID: "e1", FiscalPeriod: "2026-07"}, "principal-1")
	var snap domain.TrialBalanceSnapshot
	_ = json.Unmarshal(compileRec.Body.Bytes(), &snap)

	rec := doRequest(r, http.MethodGet, "/v1/trial-balance/"+snap.TrialBalanceSnapshotID, nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var fetched domain.TrialBalanceSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fetched.TrialBalanceSnapshotID != snap.TrialBalanceSnapshotID || len(fetched.Lines) != len(snap.Lines) {
		t.Errorf("expected the fetched snapshot to match what was compiled, got %+v", fetched)
	}
}

func TestGetTrialBalance_NotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/trial-balance/does-not-exist", nil, "principal-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ── Chart of Accounts (ACC-01) ────────────────────────────────────────────────

func TestCreateAccount_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/chart-of-accounts/", domain.CreateAccountRequest{
		AccountCode: "1000-Cash", AccountName: "Cash", AccountType: domain.AccountTypeAsset,
	}, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAccount_InvalidType_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/chart-of-accounts/", domain.CreateAccountRequest{
		AccountCode: "1000-Cash", AccountName: "Cash", AccountType: "NOT_A_REAL_TYPE",
	}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreateAccount_DuplicateCode_Returns409(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	req := domain.CreateAccountRequest{AccountCode: "1000-Cash", AccountName: "Cash", AccountType: domain.AccountTypeAsset}
	doRequest(r, http.MethodPost, "/v1/chart-of-accounts/", req, "principal-1")
	rec := doRequest(r, http.MethodPost, "/v1/chart-of-accounts/", req, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

// createAccount is a test helper that drives the real HTTP endpoint rather
// than writing into the stub directly — so downstream tests genuinely
// exercise "this account was registered through the real API," not a
// fixture that assumes the registration path works.
func createAccount(t *testing.T, r chi.Router, req domain.CreateAccountRequest) {
	t.Helper()
	rec := doRequest(r, http.MethodPost, "/v1/chart-of-accounts/", req, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("createAccount: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateJournal_UnregisteredAccountCode_StillAllowed proves the
// deliberate bootstrap gap: an account code the chart has never heard of
// must NOT block posting — every existing caller platform-wide posts using
// codes nothing has ever validated, and this must not become a flag day.
func TestCreateJournal_UnregisteredAccountCode_StillAllowed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 (unregistered account codes must not block posting), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateJournal_InactiveAccount_Returns422 proves a REGISTERED but
// deactivated account genuinely blocks posting — unlike the unregistered
// case above, this is a real, deliberate fact someone recorded.
func TestCreateJournal_InactiveAccount_Returns422(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createAccount(t, r, domain.CreateAccountRequest{AccountCode: "1000", AccountName: "Cash", AccountType: domain.AccountTypeAsset})
	doRequest(r, http.MethodPost, "/v1/chart-of-accounts/1000/deactivate", nil, "principal-1")

	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 posting to an INACTIVE registered account, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateJournal_ControlAccountRestricted_BlocksWithoutOverride and its
// pair below are the direct test of invariant #7: "Control accounts cannot
// be bypassed by ordinary manual journals where policy restricts direct
// posting."
func TestCreateJournal_ControlAccountRestricted_BlocksWithoutOverride(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createAccount(t, r, domain.CreateAccountRequest{
		AccountCode: "1000", AccountName: "Cash", AccountType: domain.AccountTypeAsset,
		IsControlAccount: true, DirectPostingRestricted: true,
	})

	rec := doRequest(r, http.MethodPost, "/v1/journals/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 blocking direct posting to a restricted control account, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateJournal_ControlAccountRestricted_OverrideRequiresAuthorization(t *testing.T) {
	s := newStubStore()
	// Denies specifically the override action — proves the override needs
	// its OWN authorization, not merely the ordinary journal-create grant.
	denyOverride := &stubAuthZ{}
	r := newRouter(s, &stubPublisher{}, denyOverride)
	createAccount(t, r, domain.CreateAccountRequest{
		AccountCode: "1000", AccountName: "Cash", AccountType: domain.AccountTypeAsset,
		IsControlAccount: true, DirectPostingRestricted: true,
	})

	req := validCreateReq()
	req.OverrideControlAccountRestriction = true
	denyOverride.err = domain.ErrAuthorizationDenied
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the override itself is denied, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateJournal_ControlAccountRestricted_OverrideSucceeds(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}) // grants by default
	createAccount(t, r, domain.CreateAccountRequest{
		AccountCode: "1000", AccountName: "Cash", AccountType: domain.AccountTypeAsset,
		IsControlAccount: true, DirectPostingRestricted: true,
	})

	req := validCreateReq()
	req.OverrideControlAccountRestriction = true
	rec := doRequest(r, http.MethodPost, "/v1/journals/", req, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 with a granted override, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── Account Mapping (ACC-02) ─────────────────────────────────────────────────

func TestSetAccountMapping_Success(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createAccount(t, r, domain.CreateAccountRequest{AccountCode: "6100-Meals", AccountName: "Meals Expense", AccountType: domain.AccountTypeExpense})

	rec := doRequest(r, http.MethodPost, "/v1/account-mappings/", domain.SetAccountMappingRequest{
		MappingKey: "EXPENSE_CATEGORY:MEALS", AccountCode: "6100-Meals",
	}, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSetAccountMapping_TargetAccountDoesNotExist_Returns400 proves ACC-02
// cannot map a business concept onto an account the chart doesn't know
// about — unlike CreateJournal's own deliberate bootstrap allowance for
// unregistered codes, a NEW mapping is an explicit administrative act with
// no such backward-compatibility need, so it's validated strictly.
func TestSetAccountMapping_TargetAccountDoesNotExist_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodPost, "/v1/account-mappings/", domain.SetAccountMappingRequest{
		MappingKey: "EXPENSE_CATEGORY:MEALS", AccountCode: "does-not-exist",
	}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSetAccountMapping_TargetAccountInactive_Returns400(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createAccount(t, r, domain.CreateAccountRequest{AccountCode: "6100-Meals", AccountName: "Meals Expense", AccountType: domain.AccountTypeExpense})
	doRequest(r, http.MethodPost, "/v1/chart-of-accounts/6100-Meals/deactivate", nil, "principal-1")

	rec := doRequest(r, http.MethodPost, "/v1/account-mappings/", domain.SetAccountMappingRequest{
		MappingKey: "EXPENSE_CATEGORY:MEALS", AccountCode: "6100-Meals",
	}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 mapping onto an INACTIVE account, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSetAccountMapping_SecondMapping_SupersedesRatherThanDuplicates proves
// the effective-dated versioning: setting a new mapping for a key already
// mapped must replace what GetAccountMapping resolves as current, not
// create two simultaneously "current" answers.
func TestSetAccountMapping_SecondMapping_SupersedesRatherThanDuplicates(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{})
	createAccount(t, r, domain.CreateAccountRequest{AccountCode: "6100-Meals", AccountName: "Meals Expense", AccountType: domain.AccountTypeExpense})
	createAccount(t, r, domain.CreateAccountRequest{AccountCode: "6200-Meals-New", AccountName: "Meals Expense v2", AccountType: domain.AccountTypeExpense})

	doRequest(r, http.MethodPost, "/v1/account-mappings/", domain.SetAccountMappingRequest{
		MappingKey: "EXPENSE_CATEGORY:MEALS", AccountCode: "6100-Meals",
	}, "principal-1")
	doRequest(r, http.MethodPost, "/v1/account-mappings/", domain.SetAccountMappingRequest{
		MappingKey: "EXPENSE_CATEGORY:MEALS", AccountCode: "6200-Meals-New",
	}, "principal-1")

	rec := doRequest(r, http.MethodGet, "/v1/account-mappings/EXPENSE_CATEGORY:MEALS", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var m domain.AccountMapping
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.AccountCode != "6200-Meals-New" {
		t.Fatalf("expected the current mapping to be the newer one (6200-Meals-New), got %q", m.AccountCode)
	}
}

func TestGetAccountMapping_NotFound_Returns404(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/account-mappings/does-not-exist", nil, "principal-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
