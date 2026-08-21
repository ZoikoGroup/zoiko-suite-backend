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
	countErr       error
	legalEntities  []string
	entitiesErr    error

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

func (s *stubStore) CountUnmatched(_ context.Context, _, _, _ string) (int, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.countUnmatched, nil
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

// cashAcct returns the ledger account code the tests treat as "the bank
// account". A statement line without one cannot be matched at all, so every
// seeded line needs it.
func cashAcct() *string { c := "1000"; return &c }

type stubPublisher struct {
	ingested, matched, exceptionRaised, completed int
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

// newRouter mirrors cmd/server/main.go's middleware stack. TenantContext was
// previously absent here, so every test ran with no verified tenant scope at
// all — which is precisely the condition the routes were getting wrong, and
// the reason the whole class of tenant-scope defects went unnoticed by a
// 24-test suite.
func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ, l *stubLedger) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, p, a, l, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
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
