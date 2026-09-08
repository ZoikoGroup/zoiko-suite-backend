package handler_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/entityregistry"
	"zoiko.io/intercompany-accounting-svc/internal/handler"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
	"zoiko.io/intercompany-accounting-svc/internal/middleware"
)

// validMatchedJournal builds a FINALIZED journal detail, in targetLegalEntityID,
// whose lines net to amount — exactly what MatchEntry's own Validations 1-3
// require to pass, so a test using it exercises only Validation 4 (group
// relationship).
func validMatchedJournal(amount float64, targetLegalEntityID string) *ledger.JournalDetail {
	return &ledger.JournalDetail{
		JournalID: "journal-1", LegalEntityID: targetLegalEntityID, Status: "FINALIZED",
		Lines: []ledger.JournalLine{{AccountCode: "1300-Intercompany", DebitAmount: amount}},
	}
}

// stubEntityRegistry lets a test control exactly what tenant-entity-
// registry-svc would report for each legal entity, without a real HTTP call.
type stubEntityRegistry struct {
	hierarchies map[string][]entityregistry.Hierarchy // legal_entity_id -> its rows
	err         error
}

func (s *stubEntityRegistry) ListHierarchies(_ context.Context, _, legalEntityID string) ([]entityregistry.Hierarchy, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.hierarchies[legalEntityID], nil
}

func newRouterWithEntityRegistry(s *stubStoreReal, pub *stubPublisher, authz *stubAuthZ, leg *stubLedger, reg *stubEntityRegistry) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, leg, zap.NewNop()).WithEntityRegistry(reg)
	handler.RegisterRoutes(r, h)
	return r
}

func TestMatchEntry_GroupRelationshipLost_RecordsMismatchNotOutage(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	s.entries["e1"].SourceLegalEntityID = "entity-a"
	s.entries["e1"].TargetLegalEntityID = "entity-b"

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"].Amount, "entity-b")}
	// entity-a and entity-b have no shared parent and no direct link.
	reg := &stubEntityRegistry{hierarchies: map[string][]entityregistry.Hierarchy{
		"entity-a": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-a", EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0)}},
		"entity-b": {{ParentLegalEntityID: "parent-y", ChildLegalEntityID: "entity-b", EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0)}},
	}}
	r := newRouterWithEntityRegistry(s, &stubPublisher{}, &stubAuthZ{}, leg, reg)

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "journal-1"}, "user-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (a real accounting fact), not a 5xx outage, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected MISMATCH recorded, got %q", s.entries["e1"].MatchStatus)
	}
	if s.entries["e1"].MismatchReason == nil || *s.entries["e1"].MismatchReason != domain.ErrGroupRelationshipLost.Error() {
		t.Fatalf("expected the group-relationship-lost reason recorded, got %+v", s.entries["e1"].MismatchReason)
	}
}

func TestMatchEntry_GroupRelationshipIntact_MatchSucceeds(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	s.entries["e1"].SourceLegalEntityID = "entity-a"
	s.entries["e1"].TargetLegalEntityID = "entity-b"

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"].Amount, "entity-b")}
	// Both entities share the same open immediate parent.
	reg := &stubEntityRegistry{hierarchies: map[string][]entityregistry.Hierarchy{
		"entity-a": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-a", EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0)}},
		"entity-b": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-b", EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0)}},
	}}
	r := newRouterWithEntityRegistry(s, &stubPublisher{}, &stubAuthZ{}, leg, reg)

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "journal-1"}, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected MATCHED, got %q", s.entries["e1"].MatchStatus)
	}
}

func TestMatchEntry_NoEntityRegistryConfigured_CheckSkipped(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	s.entries["e1"].SourceLegalEntityID = "entity-a"
	s.entries["e1"].TargetLegalEntityID = "entity-b"

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"].Amount, "entity-b")}
	// newRouter (not newRouterWithEntityRegistry) wires no entity registry
	// at all — the check must be skipped, not fail closed.
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, leg)

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "journal-1"}, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 with no entity registry configured, got %d: %s", resp.Code, resp.Body.String())
	}
}
