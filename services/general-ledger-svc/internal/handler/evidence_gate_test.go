package handler_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcenvelope "zoiko.io/general-ledger-svc/internal/envelope"
	"zoiko.io/general-ledger-svc/internal/evidencereq"
	"zoiko.io/general-ledger-svc/internal/handler"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

type stubEvidence struct {
	err error

	calls       int
	tenantID    string
	entityID    string
	domainCode  string
	actionType  string
	correlation string
	principalID string
	refs        []string
}

func (s *stubEvidence) EvaluateSufficient(_ context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, refs []string) error {
	s.calls++
	s.tenantID = tenantID
	s.entityID = legalEntityID
	s.domainCode = domainCode
	s.actionType = actionType
	s.correlation = correlationID
	s.principalID = principalID
	s.refs = refs
	return s.err
}

func newRouterWithEvidenceGate(s *stubStore, e handler.EvidenceChecker) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	h := handler.New(s, &stubPublisher{}, &stubAuthZ{}, &stubClose{}, zap.NewNop()).WithEvidenceGate(e)
	handler.RegisterRoutes(r, h)
	return r
}

func validatedJournalWithEvidence(refs ...string) *stubStore {
	s := newStubStore()
	s.journals["j1"] = &domain.JournalHeader{
		JournalID: "j1", TenantID: "t1", LegalEntityID: "e1",
		Status: domain.JournalStatusValidated, EvidenceRefs: refs,
	}
	return s
}

func TestPostJournal_EvidenceMissingFailsClosedBeforeFinalizing(t *testing.T) {
	e := &stubEvidence{err: evidencereq.ErrEvidenceMissing}
	s := validatedJournalWithEvidence("doc-from-header")

	rec := doRequest(newRouterWithEvidenceGate(s, e), http.MethodPost, "/v1/journals/j1/post", nil, "principal-1")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j1"].Status != domain.JournalStatusValidated {
		t.Fatalf("journal must remain VALIDATED when evidence is missing, got %s", s.journals["j1"].Status)
	}
	if e.calls != 1 {
		t.Fatalf("evidence calls = %d, want 1", e.calls)
	}
	if e.tenantID != "t1" || e.entityID != "e1" || e.domainCode != "FINANCE" || e.actionType != "JOURNAL_POST" {
		t.Fatalf("unexpected scope: tenant=%q entity=%q domain=%q action=%q", e.tenantID, e.entityID, e.domainCode, e.actionType)
	}
	if e.principalID != "principal-1" {
		t.Fatalf("principal = %q, want principal-1", e.principalID)
	}
	if len(e.refs) != 1 || e.refs[0] != "doc-from-header" {
		t.Fatalf("refs = %v, want [doc-from-header]", e.refs)
	}
}

func TestPostJournal_EvidenceServiceUnavailableFailsClosed(t *testing.T) {
	e := &stubEvidence{err: evidencereq.ErrServiceUnavailable}
	s := validatedJournalWithEvidence()

	rec := doRequest(newRouterWithEvidenceGate(s, e), http.MethodPost, "/v1/journals/j1/post", nil, "principal-1")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j1"].Status != domain.JournalStatusValidated {
		t.Fatalf("journal must remain VALIDATED when the gate is unavailable, got %s", s.journals["j1"].Status)
	}
}

func TestPostJournal_EvidenceSatisfiedAllowsFinalization(t *testing.T) {
	e := &stubEvidence{}
	s := validatedJournalWithEvidence("doc-1")

	rec := doRequest(newRouterWithEvidenceGate(s, e), http.MethodPost, "/v1/journals/j1/post", nil, "principal-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if s.journals["j1"].Status != domain.JournalStatusFinalized {
		t.Fatalf("status = %s, want FINALIZED", s.journals["j1"].Status)
	}
}
