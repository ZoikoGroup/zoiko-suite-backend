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

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/handler"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	entries map[string]*domain.IntercompanyEntry

	createErr     error
	getErr        error
	listErr       error
	transitionErr error
}

func newStubStore() *stubStore {
	return &stubStore{entries: map[string]*domain.IntercompanyEntry{}}
}

func (s *stubStore) CreateEntry(_ context.Context, e *domain.IntercompanyEntry) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.entries[e.IntercompanyEntryID] = e
	return nil
}

func (s *stubStore) GetEntry(_ context.Context, entryID string) (*domain.IntercompanyEntry, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	e, ok := s.entries[entryID]
	if !ok {
		return nil, nil
	}
	return e, nil
}

func (s *stubStore) ListEntries(_ context.Context, _ domain.ListEntriesFilter) ([]domain.IntercompanyEntry, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.IntercompanyEntry
	for _, e := range s.entries {
		out = append(out, *e)
	}
	return out, nil
}

func (s *stubStore) MatchEntry(_ context.Context, _, entryID, targetJournalID, _ string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	e, ok := s.entries[entryID]
	if !ok || (e.MatchStatus != domain.MatchStatusPending && e.MatchStatus != domain.MatchStatusMismatched) {
		return domain.ErrInvalidTransition
	}
	e.MatchStatus = domain.MatchStatusMatched
	e.TargetJournalEntryID = &targetJournalID
	return nil
}

func (s *stubStore) MismatchEntry(_ context.Context, _, entryID, targetJournalID, reason string) error {
	if s.transitionErr != nil {
		return s.transitionErr
	}
	e, ok := s.entries[entryID]
	if !ok || (e.MatchStatus != domain.MatchStatusPending && e.MatchStatus != domain.MatchStatusMismatched) {
		return domain.ErrInvalidTransition
	}
	e.MatchStatus = domain.MatchStatusMismatched
	e.TargetJournalEntryID = &targetJournalID
	e.MismatchReason = &reason
	return nil
}

type stubPublisher struct {
	created, posted, mismatched int
}

func (p *stubPublisher) PublishEntryCreated(_ context.Context, _ domain.IntercompanyEntry) {
	p.created++
}
func (p *stubPublisher) PublishEntryPosted(_ context.Context, _ domain.IntercompanyEntry) { p.posted++ }
func (p *stubPublisher) PublishMismatchDetected(_ context.Context, _ domain.IntercompanyEntry) {
	p.mismatched++
}

type stubAuthZ struct {
	err error
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

// stubLedger lets each test control what general-ledger-svc "returns" per
// journal ID, without booting a real service.
type stubLedger struct {
	journals map[string]*ledger.Journal
	err      error
}

func newStubLedger() *stubLedger {
	return &stubLedger{journals: map[string]*ledger.Journal{}}
}

func (l *stubLedger) GetJournal(_ context.Context, _, journalID string) (*ledger.Journal, error) {
	if l.err != nil {
		return nil, l.err
	}
	j, ok := l.journals[journalID]
	if !ok {
		return nil, ledger.ErrJournalNotFound
	}
	return j, nil
}

func finalizedJournal(legalEntityID string, netAmount float64) *ledger.Journal {
	return &ledger.Journal{
		LegalEntityID: legalEntityID,
		Status:        "FINALIZED",
		Lines:         []ledger.JournalLine{{DebitAmount: netAmount}},
	}
}

func newRouter(s *stubStore, p *stubPublisher, a *stubAuthZ, l *stubLedger) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, p, a, l, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequest(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ── CreateEntry ──────────────────────────────────────────────────────────────

func validCreateReq() domain.CreateIntercompanyEntryRequest {
	return domain.CreateIntercompanyEntryRequest{
		TenantID:             "t1",
		SourceLegalEntityID:  "e1",
		TargetLegalEntityID:  "e2",
		SourceJournalEntryID: "j-src",
		Amount:               1000,
		CurrencyCode:         "USD",
		Description:          "Shared IT services Q3 allocation",
	}
}

func TestCreateEntry_Success(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubLedger())
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateEntry_MissingPrincipalHeader_Returns401(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubLedger())
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/", validCreateReq(), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestCreateEntry_AuthorizationDenied_Returns403(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, newStubLedger())
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestCreateEntry_AuthorizationServiceUnavailable_FailsClosed(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationServiceUnavailable}, newStubLedger())
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/", validCreateReq(), "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestCreateEntry_ZeroAmount_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubLedger())
	req := validCreateReq()
	req.Amount = 0
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a zero-amount entry, got %d", rec.Code)
	}
}

func TestCreateEntry_SameSourceAndTargetEntity_Rejected(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubLedger())
	req := validCreateReq()
	req.TargetLegalEntityID = req.SourceLegalEntityID
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/", req, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when source and target entities are the same, got %d", rec.Code)
	}
}

// ── LinkTargetJournal ─────────────────────────────────────────────────────────

func TestLinkTargetJournal_BothJournalsVerified_ResultsInMatched(t *testing.T) {
	s := newStubStore()
	s.entries["ic1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "ic1", TenantID: "t1",
		SourceLegalEntityID: "e1", TargetLegalEntityID: "e2",
		SourceJournalEntryID: "j-src", Amount: 1000, MatchStatus: domain.MatchStatusPending,
	}

	l := newStubLedger()
	l.journals["j-src"] = finalizedJournal("e1", 1000)
	l.journals["j-tgt"] = finalizedJournal("e2", 1000)

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, l)
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/ic1/link-target",
		domain.LinkTargetJournalRequest{TargetJournalEntryID: "j-tgt"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.entries["ic1"].MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected status MATCHED, got %s", s.entries["ic1"].MatchStatus)
	}
	if pub.posted != 1 {
		t.Fatalf("expected intercompany.entry.posted to be published once, got %d", pub.posted)
	}
}

func TestLinkTargetJournal_AmountMismatch_ResultsInMismatchedNot400(t *testing.T) {
	// A failed verification is a valid 200 MISMATCHED outcome, never a 4xx error.
	s := newStubStore()
	s.entries["ic1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "ic1", TenantID: "t1",
		SourceLegalEntityID: "e1", TargetLegalEntityID: "e2",
		SourceJournalEntryID: "j-src", Amount: 1000, MatchStatus: domain.MatchStatusPending,
	}

	l := newStubLedger()
	l.journals["j-src"] = finalizedJournal("e1", 1000)
	l.journals["j-tgt"] = finalizedJournal("e2", 500) // wrong amount

	pub := &stubPublisher{}
	r := newRouter(s, pub, &stubAuthZ{}, l)
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/ic1/link-target",
		domain.LinkTargetJournalRequest{TargetJournalEntryID: "j-tgt"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (mismatch is a valid outcome, not an error), got %d: %s", rec.Code, rec.Body.String())
	}
	if s.entries["ic1"].MatchStatus != domain.MatchStatusMismatched {
		t.Fatalf("expected status MISMATCHED, got %s", s.entries["ic1"].MatchStatus)
	}
	if pub.mismatched != 1 {
		t.Fatalf("expected intercompany.mismatch.detected to be published once, got %d", pub.mismatched)
	}
}

func TestLinkTargetJournal_TargetJournalNotFound_ResultsInMismatched(t *testing.T) {
	s := newStubStore()
	s.entries["ic1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "ic1", TenantID: "t1",
		SourceLegalEntityID: "e1", TargetLegalEntityID: "e2",
		SourceJournalEntryID: "j-src", Amount: 1000, MatchStatus: domain.MatchStatusPending,
	}

	l := newStubLedger()
	l.journals["j-src"] = finalizedJournal("e1", 1000)
	// j-tgt intentionally absent from l.journals.

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/ic1/link-target",
		domain.LinkTargetJournalRequest{TargetJournalEntryID: "j-tgt"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.entries["ic1"].MatchStatus != domain.MatchStatusMismatched {
		t.Fatalf("expected status MISMATCHED, got %s", s.entries["ic1"].MatchStatus)
	}
}

func TestLinkTargetJournal_WrongLegalEntity_ResultsInMismatched(t *testing.T) {
	s := newStubStore()
	s.entries["ic1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "ic1", TenantID: "t1",
		SourceLegalEntityID: "e1", TargetLegalEntityID: "e2",
		SourceJournalEntryID: "j-src", Amount: 1000, MatchStatus: domain.MatchStatusPending,
	}

	l := newStubLedger()
	l.journals["j-src"] = finalizedJournal("e1", 1000)
	l.journals["j-tgt"] = finalizedJournal("some-other-entity", 1000)

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/ic1/link-target",
		domain.LinkTargetJournalRequest{TargetJournalEntryID: "j-tgt"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.entries["ic1"].MatchStatus != domain.MatchStatusMismatched {
		t.Fatalf("expected status MISMATCHED, got %s", s.entries["ic1"].MatchStatus)
	}
}

func TestLinkTargetJournal_LedgerUnavailable_FailsClosed(t *testing.T) {
	s := newStubStore()
	s.entries["ic1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "ic1", TenantID: "t1",
		SourceLegalEntityID: "e1", TargetLegalEntityID: "e2",
		SourceJournalEntryID: "j-src", Amount: 1000, MatchStatus: domain.MatchStatusPending,
	}

	l := newStubLedger()
	l.err = ledger.ErrUnavailable

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/ic1/link-target",
		domain.LinkTargetJournalRequest{TargetJournalEntryID: "j-tgt"}, "principal-1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when general-ledger-svc is unreachable (fail closed), got %d: %s", rec.Code, rec.Body.String())
	}
	if s.entries["ic1"].MatchStatus != domain.MatchStatusPending {
		t.Fatalf("entry must remain PENDING when the ledger couldn't be checked, got %s", s.entries["ic1"].MatchStatus)
	}
}

func TestLinkTargetJournal_ResolvesMismatch_Succeeds(t *testing.T) {
	// MISMATCHED -> MATCHED is legal, unlike a pure fork.
	s := newStubStore()
	reason := "target journal amount does not match the entry amount"
	oldTgt := "j-tgt-wrong"
	s.entries["ic1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "ic1", TenantID: "t1",
		SourceLegalEntityID: "e1", TargetLegalEntityID: "e2",
		SourceJournalEntryID: "j-src", Amount: 1000,
		MatchStatus: domain.MatchStatusMismatched, MismatchReason: &reason, TargetJournalEntryID: &oldTgt,
	}

	l := newStubLedger()
	l.journals["j-src"] = finalizedJournal("e1", 1000)
	l.journals["j-tgt-correct"] = finalizedJournal("e2", 1000)

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/ic1/link-target",
		domain.LinkTargetJournalRequest{TargetJournalEntryID: "j-tgt-correct"}, "principal-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 resolving a MISMATCHED entry via link-target, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.entries["ic1"].MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected status MATCHED, got %s", s.entries["ic1"].MatchStatus)
	}
}

func TestLinkTargetJournal_AlreadyMatched_Rejected(t *testing.T) {
	s := newStubStore()
	tgt := "j-tgt"
	s.entries["ic1"] = &domain.IntercompanyEntry{
		IntercompanyEntryID: "ic1", TenantID: "t1",
		SourceLegalEntityID: "e1", TargetLegalEntityID: "e2",
		SourceJournalEntryID: "j-src", Amount: 1000,
		MatchStatus: domain.MatchStatusMatched, TargetJournalEntryID: &tgt,
	}

	l := newStubLedger()
	l.journals["j-src"] = finalizedJournal("e1", 1000)
	l.journals["j-tgt"] = finalizedJournal("e2", 1000)

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, l)
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/ic1/link-target",
		domain.LinkTargetJournalRequest{TargetJournalEntryID: "j-tgt"}, "principal-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 re-linking an already-MATCHED entry, got %d", rec.Code)
	}
}

func TestLinkTargetJournal_RequiresTargetJournalID(t *testing.T) {
	s := newStubStore()
	s.entries["ic1"] = &domain.IntercompanyEntry{IntercompanyEntryID: "ic1", TenantID: "t1", MatchStatus: domain.MatchStatusPending}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, newStubLedger())
	rec := doRequest(r, http.MethodPost, "/v1/intercompany-entries/ic1/link-target",
		domain.LinkTargetJournalRequest{}, "principal-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no target_journal_entry_id, got %d", rec.Code)
	}
}

// ── GetEntry / ListEntries ────────────────────────────────────────────────────

func TestGetEntry_NotFound(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubLedger())
	rec := doRequest(r, http.MethodGet, "/v1/intercompany-entries/does-not-exist", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestListEntries_RequiresTenantID(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, newStubLedger())
	rec := doRequest(r, http.MethodGet, "/v1/intercompany-entries/", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without tenant_id query param, got %d", rec.Code)
	}
}
