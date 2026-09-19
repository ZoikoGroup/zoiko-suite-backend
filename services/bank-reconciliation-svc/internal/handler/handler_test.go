package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/handler"
	"zoiko.io/bank-reconciliation-svc/internal/ledger"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	lines         map[string]*domain.StatementLine
	byCorrelation map[string]string

	createErr      error
	getErr         error
	listErr        error
	transitionErr  error
	countUnmatched int
	countMatched   int
	countErr       error
	legalEntities  []string
	entitiesErr    error

	certificates map[string]*domain.ReconciliationCertificate
	certifyErr   error

	// BNK-05 backlog: run/policy/conflict error injection. Each defaults
	// to nil (success), and a test that needs a specific failure sets the
	// matching field before dispatching the request.
	startRunErr         error
	getRunErr           error
	freezeErr           error
	getPopulationErr    error
	certifyRunErr       error
	reperformErr        error
	bindPolicyErr       error
	createPolicyErr     error
	getCurrentPolicyErr error
	raiseConflictErr    error
	getConflictErr      error
	listConflictsErr    error
	resolveConflictErr  error

	// RunAutomaticMatching: the set of lines ListUnmatchedLinesInPopulation
	// returns, an error to inject in its place, and the population_id
	// GetRun's stubbed run reports as frozen (nil unless a test sets it).
	unmatchedInPopulation []domain.StatementLine
	listUnmatchedErr      error
	runPopulationID       *string

	// lastListFilter records what ListStatementLines was actually asked for,
	// so a test can assert the tenant came from the verified header rather
	// than from ?tenant_id.
	lastListFilter domain.ListStatementLinesFilter
}

func newStubStore() *stubStore {
	return &stubStore{lines: map[string]*domain.StatementLine{}, byCorrelation: map[string]string{}}
}

func (s *stubStore) CreateStatementLine(_ context.Context, l *domain.StatementLine) (bool, error) {
	if s.createErr != nil {
		return false, s.createErr
	}
	key := l.TenantID + "|" + l.CorrelationID
	if l.CorrelationID != "" {
		if existingID, ok := s.byCorrelation[key]; ok {
			*l = *s.lines[existingID]
			return false, nil
		}
		s.byCorrelation[key] = l.StatementLineID
	}
	s.lines[l.StatementLineID] = l
	return true, nil
}

func (s *stubStore) GetStatementLine(_ context.Context, statementLineID string) (*domain.StatementLine, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	l, ok := s.lines[statementLineID]
	if !ok {
		return nil, nil
	}
	return l, nil
}

func (s *stubStore) ListStatementLines(_ context.Context, f domain.ListStatementLinesFilter) ([]domain.StatementLine, error) {
	s.lastListFilter = f
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]domain.StatementLine, 0)
	for _, l := range s.lines {
		out = append(out, *l)
	}
	return out, nil
}

func (s *stubStore) MatchStatementLine(_ context.Context, _, statementLineID, journalID, _ string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	l, ok := s.lines[statementLineID]
	if !ok || (l.Status != domain.StatementLineStatusUnmatched && l.Status != domain.StatementLineStatusException) {
		return domain.ErrInvalidTransition
	}
	l.Status = domain.StatementLineStatusMatched
	l.MatchedJournalID = &journalID
	return nil
}

func (s *stubStore) FlagException(_ context.Context, _, statementLineID, reason, _ string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	l, ok := s.lines[statementLineID]
	if !ok || l.Status != domain.StatementLineStatusUnmatched {
		return domain.ErrInvalidTransition
	}
	l.Status = domain.StatementLineStatusException
	l.ExceptionReason = &reason
	return nil
}

func (s *stubStore) UnmatchWithReason(_ context.Context, _, statementLineID, reason, _ string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	l, ok := s.lines[statementLineID]
	if !ok || l.Status != domain.StatementLineStatusMatched {
		return domain.ErrInvalidTransition
	}
	l.Status = domain.StatementLineStatusException
	l.ExceptionReason = &reason
	return nil
}

func (s *stubStore) CountUnmatched(_ context.Context, _, _, _ string) (int, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.countUnmatched, nil
}

func (s *stubStore) CountMatched(_ context.Context, _, _, _ string) (int, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.countMatched, nil
}

func (s *stubStore) CertifyStatement(_ context.Context, tenantID, legalEntityID, bankAccountID, statementDate, certifiedByPrincipalID, correlationID string, matchedLineCount int) (*domain.ReconciliationCertificate, bool, error) {
	if s.certifyErr != nil {
		return nil, false, s.certifyErr
	}
	key := tenantID + "|" + bankAccountID + "|" + statementDate
	if s.certificates == nil {
		s.certificates = map[string]*domain.ReconciliationCertificate{}
	}
	if existing, ok := s.certificates[key]; ok {
		return existing, false, nil
	}
	c := &domain.ReconciliationCertificate{
		CertificateID: "cert-" + key, TenantID: tenantID, LegalEntityID: legalEntityID, BankAccountID: bankAccountID,
		StatementDate: statementDate, MatchedLineCount: matchedLineCount, CertifiedByPrincipalID: certifiedByPrincipalID,
		CertifiedAt: time.Now().UTC(), CorrelationID: correlationID,
	}
	s.certificates[key] = c
	return c, true, nil
}

func (s *stubStore) ProposeMatch(_ context.Context, _, statementLineID, journalID, proposedByPrincipalID string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	l, ok := s.lines[statementLineID]
	if !ok || (l.Status != domain.StatementLineStatusUnmatched && l.Status != domain.StatementLineStatusException) {
		return domain.ErrInvalidTransition
	}
	l.Status = domain.StatementLineStatusPendingConfirmation
	l.ProposedJournalID = &journalID
	l.ProposedByPrincipalID = &proposedByPrincipalID
	return nil
}

func (s *stubStore) ConfirmMatch(_ context.Context, _, statementLineID, confirmingPrincipalID string) (*domain.StatementLine, error) {
	if s.transitionErr != nil {
		return nil, s.transitionErr
	}
	l, ok := s.lines[statementLineID]
	if !ok {
		return nil, domain.ErrStatementLineNotFound
	}
	if l.Status != domain.StatementLineStatusPendingConfirmation {
		return nil, domain.ErrInvalidTransition
	}
	if l.ProposedByPrincipalID != nil && *l.ProposedByPrincipalID == confirmingPrincipalID {
		return nil, domain.ErrMatchSelfConfirmation
	}
	l.Status = domain.StatementLineStatusMatched
	l.MatchedJournalID = l.ProposedJournalID
	l.MatchedByPrincipalID = &confirmingPrincipalID
	return l, nil
}

func (s *stubStore) RejectProposedMatch(_ context.Context, _, statementLineID, reason, actorPrincipalID string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	l, ok := s.lines[statementLineID]
	if !ok || l.Status != domain.StatementLineStatusPendingConfirmation {
		return domain.ErrInvalidTransition
	}
	l.Status = domain.StatementLineStatusException
	l.ExceptionReason = &reason
	l.FlaggedByPrincipalID = &actorPrincipalID
	l.ProposedJournalID = nil
	l.ProposedByPrincipalID = nil
	return nil
}

func (s *stubStore) StatementLegalEntities(_ context.Context, _, _, _ string) ([]string, error) {
	if s.entitiesErr != nil {
		return nil, s.entitiesErr
	}
	if s.legalEntities == nil {
		// Default to the entity the tests authorize against, so the
		// resource-binding check is satisfied unless a test sets otherwise.
		return []string{"e1"}, nil
	}
	return s.legalEntities, nil
}

// ── stubStore: new BNK-05 backlog methods ────────────────────────────────────
// All return zero-values / nil errors — tests that need specific behaviour
// can add fields to stubStore and specialise these methods.

func (s *stubStore) MatchStatementLineWithCanonical(_ context.Context, _, statementLineID, transactionID, _ string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	l, ok := s.lines[statementLineID]
	if !ok {
		return domain.ErrInvalidTransition
	}
	l.Status = domain.StatementLineStatusMatched
	l.MatchedTransactionID = &transactionID
	return nil
}

func (s *stubStore) ProposeMatchWithCanonical(_ context.Context, _, statementLineID, transactionID, proposedBy string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	l, ok := s.lines[statementLineID]
	if !ok {
		return domain.ErrInvalidTransition
	}
	l.Status = domain.StatementLineStatusPendingConfirmation
	l.ProposedTransactionID = &transactionID
	l.ProposedByPrincipalID = &proposedBy
	return nil
}

// Reconciliation run lifecycle stubs.
func (s *stubStore) StartRun(_ context.Context, _ string, req domain.StartRunRequest, _ string) (*domain.ReconciliationRun, bool, error) {
	if s.startRunErr != nil {
		return nil, false, s.startRunErr
	}
	return &domain.ReconciliationRun{RunID: "run-1", TenantID: req.TenantID, LegalEntityID: req.LegalEntityID,
		BankAccountID: req.BankAccountID, StatementDate: req.StatementDate, Status: domain.RunStatusDraft}, true, nil
}
func (s *stubStore) GetRun(_ context.Context, _, _ string) (*domain.ReconciliationRun, error) {
	if s.getRunErr != nil {
		return nil, s.getRunErr
	}
	return &domain.ReconciliationRun{RunID: "run-1", Status: domain.RunStatusDraft, LegalEntityID: "e1", PopulationID: s.runPopulationID}, nil
}
func (s *stubStore) FreezePopulation(_ context.Context, _, _, _, _ string) (*domain.ReconciliationPopulation, bool, error) {
	if s.freezeErr != nil {
		return nil, false, s.freezeErr
	}
	return &domain.ReconciliationPopulation{PopulationID: "pop-1"}, true, nil
}
func (s *stubStore) GetPopulation(_ context.Context, _, _ string) (*domain.ReconciliationPopulation, error) {
	if s.getPopulationErr != nil {
		return nil, s.getPopulationErr
	}
	return &domain.ReconciliationPopulation{PopulationID: "pop-1"}, nil
}
func (s *stubStore) AdvanceRunStatus(_ context.Context, _, _ string, _, _ domain.ReconciliationRunStatus) error {
	return nil
}
func (s *stubStore) BindPolicy(_ context.Context, _, _, _ string) (*domain.ReconciliationRun, error) {
	if s.bindPolicyErr != nil {
		return nil, s.bindPolicyErr
	}
	return &domain.ReconciliationRun{RunID: "run-1", Status: domain.RunStatusDraft, LegalEntityID: "e1"}, nil
}
func (s *stubStore) CertifyRun(_ context.Context, _, _, _, _ string) (*domain.ReconciliationCertificate, bool, error) {
	if s.certifyRunErr != nil {
		return nil, false, s.certifyRunErr
	}
	return &domain.ReconciliationCertificate{CertificateID: "cert-1"}, true, nil
}
func (s *stubStore) SupersedeRun(_ context.Context, _, _, _, _ string) (*domain.ReconciliationRun, error) {
	if s.reperformErr != nil {
		return nil, s.reperformErr
	}
	return &domain.ReconciliationRun{RunID: "run-2", Status: domain.RunStatusDraft, LegalEntityID: "e1"}, nil
}

func (s *stubStore) ListUnmatchedLinesInPopulation(_ context.Context, _, _ string) ([]domain.StatementLine, error) {
	if s.listUnmatchedErr != nil {
		return nil, s.listUnmatchedErr
	}
	return s.unmatchedInPopulation, nil
}

// Policy stubs.
func (s *stubStore) CreatePolicy(_ context.Context, _ string, req domain.CreatePolicyRequest, _ string) (*domain.ReconciliationPolicy, error) {
	if s.createPolicyErr != nil {
		return nil, s.createPolicyErr
	}
	return &domain.ReconciliationPolicy{PolicyID: "pol-1", TenantID: req.TenantID, LegalEntityID: req.LegalEntityID, PolicyVersion: 1}, nil
}
func (s *stubStore) GetCurrentPolicy(_ context.Context, _, _ string) (*domain.ReconciliationPolicy, error) {
	if s.getCurrentPolicyErr != nil {
		return nil, s.getCurrentPolicyErr
	}
	return &domain.ReconciliationPolicy{PolicyID: "pol-1", PolicyVersion: 1}, nil
}
func (s *stubStore) GetPolicy(_ context.Context, _, _ string) (*domain.ReconciliationPolicy, error) {
	return &domain.ReconciliationPolicy{PolicyID: "pol-1", PolicyVersion: 1}, nil
}

// Evidence conflict stubs.
func (s *stubStore) RaiseEvidenceConflict(_ context.Context, _ string, req domain.RaiseEvidenceConflictRequest) (*domain.EvidenceConflict, bool, error) {
	if s.raiseConflictErr != nil {
		return nil, false, s.raiseConflictErr
	}
	return &domain.EvidenceConflict{ConflictID: "c-1", TenantID: req.TenantID, StatementLineID: req.StatementLineID, PaymentID: req.PaymentID, ConflictStatus: "OPEN"}, true, nil
}
func (s *stubStore) GetEvidenceConflict(_ context.Context, _, _ string) (*domain.EvidenceConflict, error) {
	if s.getConflictErr != nil {
		return nil, s.getConflictErr
	}
	return &domain.EvidenceConflict{ConflictID: "c-1", ConflictStatus: "OPEN", LegalEntityID: "e1"}, nil
}
func (s *stubStore) ListOpenConflicts(_ context.Context, _ string, _ int) ([]domain.EvidenceConflict, error) {
	if s.listConflictsErr != nil {
		return nil, s.listConflictsErr
	}
	return []domain.EvidenceConflict{}, nil
}
func (s *stubStore) ResolveEvidenceConflict(_ context.Context, _, _, _, _ string) (*domain.EvidenceConflict, error) {
	if s.resolveConflictErr != nil {
		return nil, s.resolveConflictErr
	}
	return &domain.EvidenceConflict{ConflictID: "c-1", ConflictStatus: "RESOLVED"}, nil
}
func (s *stubStore) IsEventProcessed(_ context.Context, _, _ string) (bool, error) { return false, nil }
func (s *stubStore) MarkEventProcessed(_ context.Context, _, _ string) error       { return nil }

// cashAcct returns the ledger account code the tests treat as "the bank
// account". A statement line without one cannot be matched at all, so every
// seeded line needs it.
func cashAcct() *string { c := "1000"; return &c }

type stubPublisher struct {
	ingested, matched, exceptionRaised, completed int
	reperformed, superseded                       int
}

func (p *stubPublisher) PublishStatementIngested(_ context.Context, _ domain.StatementLine, _ string) {
	p.ingested++
}
func (p *stubPublisher) PublishReconciliationMatched(_ context.Context, _ domain.StatementLine) {
	p.matched++
}
func (p *stubPublisher) PublishReconciliationExceptionRaised(_ context.Context, _ domain.StatementLine) {
	p.exceptionRaised++
}
func (p *stubPublisher) PublishReconciliationCompleted(_ context.Context, _, _, _, _, _ string) {
	p.completed++
}
func (p *stubPublisher) PublishReconciliationStarted(_ context.Context, _ domain.ReconciliationRun) {}
func (p *stubPublisher) PublishReconciliationReperformed(_ context.Context, _ domain.ReconciliationRun, _ string) {
	p.reperformed++
}
func (p *stubPublisher) PublishReconciliationSuperseded(_ context.Context, _ domain.ReconciliationRun, _ string) {
	p.superseded++
}
func (p *stubPublisher) PublishReconciliationCertified(_ context.Context, _ domain.ReconciliationRun, _ domain.ReconciliationCertificate) {
}
func (p *stubPublisher) PublishEvidenceConflictRaised(_ context.Context, _ domain.EvidenceConflict) {}
func (p *stubPublisher) PublishEvidenceConflictResolved(_ context.Context, _ domain.EvidenceConflict) {
}

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// stubLedger lets each test control what general-ledger-svc "returns"
// without booting a real service.
type stubLedger struct {
	journal *ledger.Journal
	err     error
}

func (l *stubLedger) GetJournal(_ context.Context, _, _ string) (*ledger.Journal, error) {
	return l.journal, l.err
}

// stubBanking is a no-op banking.Client that returns ErrTransactionNotFound by
// default, so tests that don't care about canonical matching just get the
// journal path. Assign txn/err to override.
type stubBanking struct {
	txn *bankingCanonical
	err error
}

// bankingCanonical mirrors banking.CanonicalTransaction without importing it
// (test package is external; we just need the interface).
type bankingCanonical struct {
	TransactionID string
	TenantID      string
	Amount        float64
	Currency      string
	Status        string
}

// GetCanonicalTransaction satisfies banking.Client.
// We return nil / ErrTransactionNotFound by default so existing tests that
// don't pass a transaction_id don't break.
func (b *stubBanking) GetCanonicalTransaction(_ context.Context, _, _ string) (interface{}, error) {
	return nil, nil // never called in existing tests
}

// newRouter mirrors cmd/server/main.go's middleware stack. TenantContext was
// previously absent here, so every test ran with no verified tenant scope at
// all — which is precisely the condition the routes were getting wrong, and
// the reason the whole class of tenant-scope defects went unnoticed by a
// 24-test suite.
func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ, l *stubLedger) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	// Pass nil for banking client — existing tests all use journal_id path.
	h := handler.New(s, p, a, l, nil, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

// finalizedJournal builds a journal that moves `amount` through the test's
// cash account. A positive amount is a DEBIT to it (money into the bank), a
// negative amount a CREDIT (money out) — which is the direction a statement
// line of the same sign must have.
//
// The contra line is deliberately posted to a different account, so a journal
// really does have a direction rather than netting to nothing on one account.
func finalizedJournal(legalEntityID string, amount float64) *ledger.Journal {
	cash := ledger.JournalLine{AccountCode: "1000"}
	contra := ledger.JournalLine{AccountCode: "4000"}
	if amount >= 0 {
		cash.DebitAmount = amount
		contra.CreditAmount = amount
	} else {
		cash.CreditAmount = -amount
		contra.DebitAmount = -amount
	}
	return &ledger.Journal{
		JournalID:     "j1",
		LegalEntityID: legalEntityID,
		Status:        "FINALIZED",
		Lines:         []ledger.JournalLine{cash, contra},
	}
}

const testTenant = "t1"

func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, principalID, testTenant)
}

// doRequestAs sends an explicit tenant scope. Pass "" to omit the header
// entirely — the one-argument default in doRequest cannot express that.
func doRequestAs(r chi.Router, method, path string, body any, principalID, tenantID string) *httptest.ResponseRecorder {
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
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// doRawRequest sends a body verbatim, so a test can send JSON that the Go
// request type cannot express — an unrecognised field, for instance.
func doRawRequest(r chi.Router, method, path, body, principalID, tenantID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// Compile-time proof the stubs still satisfy the contracts they stand in for.
// Without these an interface change surfaces as a confusing error at the
// handler.New call site instead of here.
var (
	_ handler.Store       = (*stubStore)(nil)
	_ handler.Publisher   = (*stubPublisher)(nil)
	_ handler.AuthZClient = (*stubAuthZ)(nil)
	_ ledger.Client       = (*stubLedger)(nil)
)

// ── CreateStatementLine ──────────────────────────────────────────────────────

func validCreateReq() domain.CreateStatementLineRequest {
	return domain.CreateStatementLineRequest{
		TenantID:          "t1",
		LegalEntityID:     "e1",
		BankAccountID:     "b1",
		StatementDate:     time.Now(),
		Amount:            1000,
		CurrencyCode:      "USD",
		BankReference:     "ACH-1234",
		GLCashAccountCode: "1000",
		CorrelationID:     "corr-1",
	}
}

func TestCreateStatementLine_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateStatementLine_MissingCorrelationID_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	req := validCreateReq()
	req.CorrelationID = ""
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no correlation_id, got %d", rec.Code)
	}
}

func TestCreateStatementLine_RetriedCorrelationID_ReturnsOriginalNotDuplicate(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubLedger{})
	req := validCreateReq()

	first := doRequest(r, http.MethodPost, "/v1/statement-lines/", req, "principal-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 on first call, got %d: %s", first.Code, first.Body.String())
	}
	var firstLine domain.StatementLine
	_ = json.NewDecoder(first.Body).Decode(&firstLine)

	retry := doRequest(r, http.MethodPost, "/v1/statement-lines/", req, "principal-1")
	if retry.Code != http.StatusOK {
		t.Fatalf("expected 200 on retried call with the same correlation_id, got %d: %s", retry.Code, retry.Body.String())
	}
	var retryLine domain.StatementLine
	_ = json.NewDecoder(retry.Body).Decode(&retryLine)
	if retryLine.StatementLineID != firstLine.StatementLineID {
		t.Fatalf("retried call resolved to a different statement_line_id (%s) than the original (%s)", retryLine.StatementLineID, firstLine.StatementLineID)
	}
	if pub.ingested != 1 {
		t.Fatalf("expected exactly 1 PublishStatementIngested call, got %d — replay must not re-publish", pub.ingested)
	}
}

func TestCreateStatementLine_MissingPrincipalHeader_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/", validCreateReq(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestCreateStatementLine_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestCreateStatementLine_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationServiceUnavailable}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestCreateStatementLine_ZeroAmount_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	req := validCreateReq()
	req.Amount = 0
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a zero-amount line, got %d", rec.Code)
	}
}

// ── MatchStatementLine ────────────────────────────────────────────────────────

func TestMatchStatementLine_LedgerVerifiedMatch_Succeeds(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubLedger{journal: finalizedJournal("e1", 1000)})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusMatched {
		t.Fatalf("expected status MATCHED, got %s", s.lines["l1"].Status)
	}
	if pub.matched != 1 {
		t.Fatalf("expected reconciliation.matched to be published once, got %d", pub.matched)
	}
}

func TestMatchStatementLine_JournalNotFound_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{err: ledger.ErrJournalNotFound})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "bogus"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a nonexistent journal, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusUnmatched {
		t.Fatalf("line must remain UNMATCHED when the journal doesn't exist, got %s", s.lines["l1"].Status)
	}
}

func TestMatchStatementLine_JournalNotFinalized_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	pending := finalizedJournal("e1", 1000)
	pending.Status = "PENDING"
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{journal: pending})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-FINALIZED journal, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMatchStatementLine_WrongLegalEntity_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{journal: finalizedJournal("some-other-entity", 1000)})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a journal belonging to a different legal entity, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMatchStatementLine_AmountMismatch_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{journal: finalizedJournal("e1", 500)})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an amount mismatch, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMatchStatementLine_LedgerUnavailable_FailsClosed(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{err: ledger.ErrUnavailable})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when general-ledger-svc is unreachable (fail closed), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMatchStatementLine_ResolvesException_Succeeds(t *testing.T) {
	// EXCEPTION -> MATCHED is legal, unlike purchase-request-svc's pure fork.
	s := newStubStore()
	reason := "unrecognized fee"
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusException, ExceptionReason: &reason, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{journal: finalizedJournal("e1", 1000)})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 resolving an EXCEPTION via match, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusMatched {
		t.Fatalf("expected status MATCHED, got %s", s.lines["l1"].Status)
	}
}

func TestMatchStatementLine_AlreadyMatched_Rejected(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusMatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{journal: finalizedJournal("e1", 1000)})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/match", domain.MatchStatementLineRequest{JournalID: "j1"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 matching an already-MATCHED line, got %d", rec.Code)
	}
}

// ── FlagException ────────────────────────────────────────────────────────────

func TestFlagException_RequiresReason(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/exception", domain.FlagExceptionRequest{}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a flag with no reason, got %d", rec.Code)
	}
}

func TestFlagException_FromUnmatched_Succeeds(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/exception", domain.FlagExceptionRequest{Reason: "unrecognized bank fee"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusException {
		t.Fatalf("expected status EXCEPTION, got %s", s.lines["l1"].Status)
	}
	if pub.exceptionRaised != 1 {
		t.Fatalf("expected reconciliation.exception.raised to be published once, got %d", pub.exceptionRaised)
	}
}

func TestFlagException_AlreadyMatched_Rejected(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Status: domain.StatementLineStatusMatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/exception", domain.FlagExceptionRequest{Reason: "trying anyway"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 flagging an already-MATCHED line, got %d", rec.Code)
	}
}

// ── UnmatchWithReason ─────────────────────────────────────────────────────────

func TestUnmatchWithReason_MissingReason_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Status: domain.StatementLineStatusMatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/unmatch", domain.FlagExceptionRequest{}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unmatch with no reason, got %d", rec.Code)
	}
}

func TestUnmatchWithReason_MatchedLine_RevertsToException(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Status: domain.StatementLineStatusMatched, GLCashAccountCode: cashAcct()}

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/unmatch", domain.FlagExceptionRequest{Reason: "matched to the wrong journal"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusException {
		t.Fatalf("expected status EXCEPTION, got %s", s.lines["l1"].Status)
	}
	if s.lines["l1"].ExceptionReason == nil || *s.lines["l1"].ExceptionReason != "matched to the wrong journal" {
		t.Fatalf("expected the exception reason to be recorded, got %+v", s.lines["l1"].ExceptionReason)
	}
	if pub.exceptionRaised != 1 {
		t.Errorf("expected PublishReconciliationExceptionRaised to be called once, got %d", pub.exceptionRaised)
	}
}

// TestUnmatchWithReason_UnmatchedLine_Rejected is the negative control:
// unmatch only ever applies to a MATCHED line — an already-UNMATCHED line
// has nothing to revert.
func TestUnmatchWithReason_UnmatchedLine_Rejected(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/unmatch", domain.FlagExceptionRequest{Reason: "no match to undo"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 unmatching an UNMATCHED line, got %d", rec.Code)
	}
}

// ── GetStatementLine / ListStatementLines ────────────────────────────────────

func TestGetStatementLine_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet, "/v1/statement-lines/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// The tenant scope now comes from the verified X-Tenant-Id header, not from
// ?tenant_id, so its absence is an unauthenticated request (401) rather than
// a malformed one (400). Reading it from the query string is what let a
// caller name somebody else's tenant.
func TestListStatementLines_RequiresVerifiedTenantScope(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequestAs(r, http.MethodGet, "/v1/statement-lines/", nil, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no verified tenant scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── CompleteStatement ────────────────────────────────────────────────────────

func TestCompleteStatement_UnmatchedLinesRemain_Returns422(t *testing.T) {
	s := newStubStore()
	s.countUnmatched = 2

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/bank-accounts/b1/statements/2026-07-01/complete?tenant_id=t1&legal_entity_id=e1", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 while UNMATCHED lines remain, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCompleteStatement_AllResolved_Succeeds(t *testing.T) {
	s := newStubStore()
	s.countUnmatched = 0

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/bank-accounts/b1/statements/2026-07-01/complete?tenant_id=t1&legal_entity_id=e1", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if pub.completed != 1 {
		t.Fatalf("expected reconciliation.completed to be published once, got %d", pub.completed)
	}
}

func TestCompleteStatement_RequiresVerifiedTenantScope(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequestAs(r, http.MethodPost, "/v1/bank-accounts/b1/statements/2026-07-01/complete?legal_entity_id=e1", nil, "principal-1", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no verified tenant scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCompleteStatement_RequiresLegalEntityID(t *testing.T) {
	// A bank account belongs to exactly one legal entity, but this endpoint
	// has no single statement line to read one from — legal_entity_id must
	// be supplied explicitly, or the authz check downstream would be sent
	// an empty legal_entity_id and authorization-svc rejects that (400),
	// which without this guard would always look like a permanent 503.
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/bank-accounts/b1/statements/2026-07-01/complete?tenant_id=t1", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without legal_entity_id query param, got %d", rec.Code)
	}
}

// ── BNK-05 backlog: reconciliation runs ─────────────────────────────────────

func TestStartRun_Success_Returns201(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	body := map[string]string{"legal_entity_id": "e1", "bank_account_id": "b1", "statement_date": "2026-07-01", "correlation_id": "corr-1"}
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs", body, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var run domain.ReconciliationRun
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if run.Status != domain.RunStatusDraft {
		t.Errorf("expected a new run to be DRAFT, got %q", run.Status)
	}
}

func TestStartRun_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs", map[string]string{"legal_entity_id": "e1"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no bank_account_id/statement_date, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestStartRun_AlreadyExists_Returns409(t *testing.T) {
	s := newStubStore()
	s.startRunErr = domain.ErrRunAlreadyExists
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	body := map[string]string{"legal_entity_id": "e1", "bank_account_id": "b1", "statement_date": "2026-07-01", "correlation_id": "corr-1"}
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs", body, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 when a run already exists for a different correlation_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetRun_NotFound_Returns404(t *testing.T) {
	s := newStubStore()
	s.getRunErr = domain.ErrRunNotFound
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet, "/v1/reconciliation-runs/does-not-exist", nil, "principal-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFreezePopulation_Success_Returns201(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/freeze", nil, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFreezePopulation_InvalidTransition_Returns409(t *testing.T) {
	s := newStubStore()
	s.freezeErr = domain.ErrRunInvalidTransition
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/freeze", nil, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 re-freezing a non-DRAFT run, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCertifyRun_Success_Returns201(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/certify", nil, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCertifyRun_MaterialResidualBlocked_Returns422(t *testing.T) {
	s := newStubStore()
	s.certifyRunErr = domain.ErrMaterialResidualBlocked
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/certify", nil, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when the unmatched residual exceeds the bound policy's threshold, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCertifyRun_PopulationNotFrozen_Returns409(t *testing.T) {
	s := newStubStore()
	s.certifyRunErr = domain.ErrRunPopulationNotFrozen
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/certify", nil, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 certifying a run whose population was never frozen, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReperformRun_Success_Returns201(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/reperform", nil, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var run domain.ReconciliationRun
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if run.RunID == "run-1" {
		t.Error("expected reperform to return a NEW run id, not the superseded one")
	}
	// Wave 8c: both the OLD run's own terminal event and the NEW run's
	// start event must be published — previously only the latter existed,
	// so a listener had no way to learn a run had ended vs merely that
	// another one started.
	if pub.superseded != 1 {
		t.Errorf("expected PublishReconciliationSuperseded to be called once, got %d", pub.superseded)
	}
	if pub.reperformed != 1 {
		t.Errorf("expected PublishReconciliationReperformed to be called once, got %d", pub.reperformed)
	}
}

func TestReperformRun_AlreadySuperseded_Returns409(t *testing.T) {
	s := newStubStore()
	s.reperformErr = domain.ErrRunSuperseded
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/reperform", nil, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 reperforming an already-superseded run, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRunAutomaticMatching_VerifiedCandidate_Matches proves the real
// batch-matching path: a candidate whose statement line is genuinely
// UNMATCHED in the run's frozen population and whose journal
// independently verifies (same verifyJournalMatches used by the manual
// path) is matched directly, with no separate propose/confirm step.
func TestRunAutomaticMatching_VerifiedCandidate_Matches(t *testing.T) {
	s := newStubStore()
	line := domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}
	s.lines["l1"] = &line
	s.unmatchedInPopulation = []domain.StatementLine{line}
	popID := "pop-1"
	s.runPopulationID = &popID

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, &stubLedger{journal: finalizedJournal("e1", 1000)})
	req := domain.RunAutomaticMatchingRequest{Candidates: []domain.AutoMatchCandidate{{StatementLineID: "l1", JournalID: "j1"}}}
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/auto-match", req, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp domain.RunAutomaticMatchingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MatchedCount != 1 || resp.SkippedCount != 0 {
		t.Fatalf("expected 1 matched, 0 skipped, got matched=%d skipped=%d: %+v", resp.MatchedCount, resp.SkippedCount, resp.Results)
	}
	if s.lines["l1"].Status != domain.StatementLineStatusMatched {
		t.Fatalf("expected line to be MATCHED, got %s", s.lines["l1"].Status)
	}
	if pub.matched != 1 {
		t.Errorf("expected PublishReconciliationMatched to be called once, got %d", pub.matched)
	}
}

// TestRunAutomaticMatching_UnverifiedCandidate_IsSkippedNotMatched is the
// negative control: a candidate whose journal fails independent
// verification (wrong amount) is never matched — it is skipped with a
// reason, and the batch still returns 200 rather than aborting.
func TestRunAutomaticMatching_UnverifiedCandidate_IsSkippedNotMatched(t *testing.T) {
	s := newStubStore()
	line := domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}
	s.lines["l1"] = &line
	s.unmatchedInPopulation = []domain.StatementLine{line}
	popID := "pop-1"
	s.runPopulationID = &popID

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{journal: finalizedJournal("e1", 500)}) // amount mismatch
	req := domain.RunAutomaticMatchingRequest{Candidates: []domain.AutoMatchCandidate{{StatementLineID: "l1", JournalID: "j1"}}}
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/auto-match", req, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 even with a skipped candidate, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp domain.RunAutomaticMatchingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MatchedCount != 0 || resp.SkippedCount != 1 || len(resp.Results) != 1 || resp.Results[0].Matched {
		t.Fatalf("expected 1 skipped with a reason, not matched: %+v", resp)
	}
	if resp.Results[0].Reason == "" {
		t.Fatal("expected a non-empty skip reason")
	}
	if s.lines["l1"].Status != domain.StatementLineStatusUnmatched {
		t.Fatalf("expected the line to remain UNMATCHED, got %s", s.lines["l1"].Status)
	}
}

// TestRunAutomaticMatching_CandidateOutsidePopulation_IsSkipped proves a
// candidate naming a statement_line_id that is not actually part of this
// run's frozen population (even if UNMATCHED elsewhere) is never matched.
func TestRunAutomaticMatching_CandidateOutsidePopulation_IsSkipped(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}
	s.unmatchedInPopulation = nil // l1 is NOT part of the frozen population's unmatched set
	popID := "pop-1"
	s.runPopulationID = &popID

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{journal: finalizedJournal("e1", 1000)})
	req := domain.RunAutomaticMatchingRequest{Candidates: []domain.AutoMatchCandidate{{StatementLineID: "l1", JournalID: "j1"}}}
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/auto-match", req, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp domain.RunAutomaticMatchingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MatchedCount != 0 || resp.SkippedCount != 1 {
		t.Fatalf("expected the out-of-population candidate to be skipped, got %+v", resp)
	}
	if s.lines["l1"].Status != domain.StatementLineStatusUnmatched {
		t.Fatalf("expected the line to remain untouched, got %s", s.lines["l1"].Status)
	}
}

// TestRunAutomaticMatching_PopulationNotFrozen_Returns409 proves the run
// must have a frozen population before auto-matching can run at all.
func TestRunAutomaticMatching_PopulationNotFrozen_Returns409(t *testing.T) {
	s := newStubStore() // runPopulationID left nil
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	req := domain.RunAutomaticMatchingRequest{Candidates: []domain.AutoMatchCandidate{{StatementLineID: "l1", JournalID: "j1"}}}
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/auto-match", req, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 with no frozen population, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBindPolicy_MissingPolicyID_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/bind-policy", map[string]string{}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no policy_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBindPolicy_Success_Returns200(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-runs/run-1/bind-policy", map[string]string{"policy_id": "pol-1"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── BNK-05 backlog: reconciliation policies ─────────────────────────────────

func TestCreatePolicy_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-policies", map[string]string{}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no legal_entity_id/effective_from, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreatePolicy_Success_Returns201(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	body := map[string]string{"legal_entity_id": "e1", "effective_from": "2026-07-01"}
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-policies", body, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreatePolicy_WideningBlockedWhenRunActive_Returns409(t *testing.T) {
	s := newStubStore()
	s.createPolicyErr = domain.ErrPolicyWouldWidenActiveRun
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	body := map[string]string{"legal_entity_id": "e1", "effective_from": "2026-07-01"}
	rec := doRequest(r, http.MethodPost, "/v1/reconciliation-policies", body, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 widening a policy while a run is bound to a stricter one, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetCurrentPolicy_MissingLegalEntityID_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet, "/v1/reconciliation-policies/current", nil, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no legal_entity_id query param, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetCurrentPolicy_NotFound_Returns404(t *testing.T) {
	s := newStubStore()
	s.getCurrentPolicyErr = domain.ErrPolicyNotFound
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet, "/v1/reconciliation-policies/current?legal_entity_id=e1", nil, "principal-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no policy has ever been created for this entity, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ── BNK-05 backlog: evidence conflicts ──────────────────────────────────────

func TestRaiseConflict_MissingFields_Returns400(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/evidence-conflicts", map[string]string{"legal_entity_id": "e1"}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no statement_line_id/payment_id, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRaiseConflict_Success_Returns201(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	body := map[string]string{"legal_entity_id": "e1", "statement_line_id": "line-1", "payment_id": "pay-1"}
	rec := doRequest(r, http.MethodPost, "/v1/evidence-conflicts", body, "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListConflicts_Success_Returns200(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet, "/v1/evidence-conflicts", nil, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetConflict_NotFound_Returns404(t *testing.T) {
	s := newStubStore()
	s.getConflictErr = domain.ErrConflictNotFound
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodGet, "/v1/evidence-conflicts/does-not-exist", nil, "principal-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestResolveConflict_Success_Returns200(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/evidence-conflicts/c-1/resolve", map[string]string{"resolution_note": "verified with bank"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestResolveConflict_AlreadyResolved_Returns409(t *testing.T) {
	s := newStubStore()
	s.resolveConflictErr = domain.ErrConflictAlreadyResolved
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/evidence-conflicts/c-1/resolve", map[string]string{"resolution_note": "n/a"}, "principal-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 resolving an already-resolved conflict, got %d: %s", rec.Code, rec.Body.String())
	}
}
