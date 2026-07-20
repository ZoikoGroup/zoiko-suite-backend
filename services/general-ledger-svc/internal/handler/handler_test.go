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

	"zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/handler"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubStore struct {
	journals map[string]*domain.JournalHeader
	lines    map[string][]domain.JournalLine

	createErr      error
	getErr         error
	listErr        error
	transitionErr  error
	sumErr         error
	debitTotal     float64
	creditTotal    float64
}

func newStubStore() *stubStore {
	return &stubStore{journals: map[string]*domain.JournalHeader{}, lines: map[string][]domain.JournalLine{}}
}

func (s *stubStore) CreateJournal(_ context.Context, h *domain.JournalHeader, lines []domain.JournalLine) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.journals[h.JournalID] = h
	s.lines[h.JournalID] = lines
	return nil
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

func (s *stubStore) ListJournals(_ context.Context, _ domain.ListJournalsFilter) ([]domain.JournalHeader, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []domain.JournalHeader
	for _, h := range s.journals {
		out = append(out, *h)
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

func (s *stubStore) SumLines(_ context.Context, _, _ string) (float64, float64, error) {
	return s.debitTotal, s.creditTotal, s.sumErr
}

type stubPublisher struct {
	created, validated, posted, reversed int
}

func (p *stubPublisher) PublishJournalCreated(_ context.Context, _ domain.JournalHeader)     { p.created++ }
func (p *stubPublisher) PublishJournalValidated(_ context.Context, _ domain.JournalHeader)   { p.validated++ }
func (p *stubPublisher) PublishJournalPosted(_ context.Context, _ domain.JournalHeader)      { p.posted++ }
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
	h := handler.New(s, p, a, c, zap.NewNop())
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

// ── CreateJournal ────────────────────────────────────────────────────────────

func validCreateReq() domain.CreateJournalRequest {
	return domain.CreateJournalRequest{
		TenantID:      "t1",
		LegalEntityID: "e1",
		FiscalPeriod:  "2026-07",
		Description:   "test journal",
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
		domain.ReverseJournalRequest{Reason: "correction"}, "principal-1")
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
		domain.ReverseJournalRequest{Reason: "correction"}, "principal-1")
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

func TestListJournals_RequiresTenantID(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rec := doRequest(r, http.MethodGet, "/v1/journals/", nil, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without tenant_id query param, got %d", rec.Code)
	}
}
