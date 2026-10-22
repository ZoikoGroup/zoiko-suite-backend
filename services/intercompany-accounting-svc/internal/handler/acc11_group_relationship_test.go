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
// in the counterparty currency, whose lines net to the counterparty amount —
// exactly what MatchEntry's own Validations 1-4 require to pass, so a test
// using it exercises only Validation 5 (group relationship).
func validMatchedJournal(entry *domain.IntercompanyEntry, targetLegalEntityID string) *ledger.JournalDetail {
	return &ledger.JournalDetail{
		JournalID: "journal-1", LegalEntityID: targetLegalEntityID, Status: "FINALIZED", CurrencyCode: entry.CounterpartyCurrencyCode,
		Lines: []ledger.JournalLine{{AccountCode: "1300-Intercompany", DebitAmount: entry.CounterpartyAmount}},
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

func newRouterWithEntityRegistry(s *stubStoreReal, authz *stubAuthZ, leg *stubLedger, reg *stubEntityRegistry) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, authz, leg, zap.NewNop()).WithEntityRegistry(reg)
	handler.RegisterRoutes(r, h)
	return r
}

func TestMatchEntry_GroupRelationshipLost_RecordsMismatchNotOutage(t *testing.T) {
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	s.entries["e1"].SourceLegalEntityID = "entity-a"
	s.entries["e1"].TargetLegalEntityID = "entity-b"

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"], "entity-b")}
	// entity-a and entity-b have no shared parent and no direct link.
	reg := &stubEntityRegistry{hierarchies: map[string][]entityregistry.Hierarchy{
		"entity-a": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-a", EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0)}},
		"entity-b": {{ParentLegalEntityID: "parent-y", ChildLegalEntityID: "entity-b", EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0)}},
	}}
	r := newRouterWithEntityRegistry(s, &stubAuthZ{}, leg, reg)

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

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"], "entity-b")}
	// Both entities share the same open immediate parent.
	reg := &stubEntityRegistry{hierarchies: map[string][]entityregistry.Hierarchy{
		"entity-a": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-a", EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0)}},
		"entity-b": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-b", EffectiveFrom: time.Now().UTC().AddDate(-1, 0, 0)}},
	}}
	r := newRouterWithEntityRegistry(s, &stubAuthZ{}, leg, reg)

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

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"], "entity-b")}
	// newRouter (not newRouterWithEntityRegistry) wires no entity registry
	// at all — the check must be skipped, not fail closed.
	r := newRouter(s, &stubAuthZ{}, leg)

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "journal-1"}, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200 with no entity registry configured, got %d: %s", resp.Code, resp.Body.String())
	}
}

// pastPeriod returns a YYYY-MM period that ended well before now, and its
// last instant. Three months back keeps every date below unambiguously in
// the past on any run day (including month-end AddDate normalisation), so a
// test can tell "as of the pair's period" from "as of the match call".
func pastPeriod(t *testing.T) (string, time.Time) {
	t.Helper()
	period := time.Now().UTC().AddDate(0, -3, 0).Format("2006-01")
	end, ok := domain.FiscalPeriodEnd(period)
	if !ok {
		t.Fatalf("FiscalPeriodEnd(%q): not ok", period)
	}
	return period, end
}

// TestMatchEntry_GroupRelationshipEndedAfterPairPeriod_StillMatches is A11's
// first case: the relationship held through the pair's period and ended
// afterwards. Checking at the match call would fail today; checking at the
// period end must still match.
func TestMatchEntry_GroupRelationshipEndedAfterPairPeriod_StillMatches(t *testing.T) {
	period, periodEnd := pastPeriod(t)
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	s.entries["e1"].SourceLegalEntityID = "entity-a"
	s.entries["e1"].TargetLegalEntityID = "entity-b"
	s.entries["e1"].FiscalPeriod = &period

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"], "entity-b")}
	ended := periodEnd.Add(24 * time.Hour) // after the period, before today
	reg := &stubEntityRegistry{hierarchies: map[string][]entityregistry.Hierarchy{
		"entity-a": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-a", EffectiveFrom: periodEnd.AddDate(-3, 0, 0), EffectiveTo: &ended}},
		"entity-b": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-b", EffectiveFrom: periodEnd.AddDate(-3, 0, 0), EffectiveTo: &ended}},
	}}
	r := newRouterWithEntityRegistry(s, &stubAuthZ{}, leg, reg)

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "journal-1"}, "user-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("relationship held through the pair's period %s but ended later; expected 200, got %d: %s", period, resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusMatched {
		t.Fatalf("expected MATCHED, got %q", s.entries["e1"].MatchStatus)
	}
}

// TestMatchEntry_GroupRelationshipBeganAfterPairPeriod_RecordsMismatch is
// A11's second case: the relationship is open today but did not exist during
// the pair's period, so it must fail regardless of today's structure.
func TestMatchEntry_GroupRelationshipBeganAfterPairPeriod_RecordsMismatch(t *testing.T) {
	period, periodEnd := pastPeriod(t)
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	s.entries["e1"].SourceLegalEntityID = "entity-a"
	s.entries["e1"].TargetLegalEntityID = "entity-b"
	s.entries["e1"].FiscalPeriod = &period

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"], "entity-b")}
	started := periodEnd.Add(24 * time.Hour) // after the period, open today
	reg := &stubEntityRegistry{hierarchies: map[string][]entityregistry.Hierarchy{
		"entity-a": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-a", EffectiveFrom: started}},
		"entity-b": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-b", EffectiveFrom: started}},
	}}
	r := newRouterWithEntityRegistry(s, &stubAuthZ{}, leg, reg)

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "journal-1"}, "user-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("relationship did not exist in the pair's period %s; expected 422, got %d: %s", period, resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected MISMATCH recorded, got %q", s.entries["e1"].MatchStatus)
	}
	if s.entries["e1"].MismatchReason == nil || *s.entries["e1"].MismatchReason != domain.ErrGroupRelationshipLost.Error() {
		t.Fatalf("expected the group-relationship-lost reason recorded, got %+v", s.entries["e1"].MismatchReason)
	}
}

// TestMatchEntry_GroupRelationshipEndedInsidePairPeriod_RecordsMismatch
// covers the negative path A11 is named after: the relationship was lost
// during the pair's own period, even though the check runs after the fact.
func TestMatchEntry_GroupRelationshipEndedInsidePairPeriod_RecordsMismatch(t *testing.T) {
	period, periodEnd := pastPeriod(t)
	s := newStubStoreReal()
	seedEntry(s, "e1", domain.MatchStatusUnmatched)
	s.entries["e1"].SourceLegalEntityID = "entity-a"
	s.entries["e1"].TargetLegalEntityID = "entity-b"
	s.entries["e1"].FiscalPeriod = &period

	leg := &stubLedger{journal: validMatchedJournal(s.entries["e1"], "entity-b")}
	ended := periodEnd.Add(-48 * time.Hour) // still inside the period
	reg := &stubEntityRegistry{hierarchies: map[string][]entityregistry.Hierarchy{
		"entity-a": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-a", EffectiveFrom: periodEnd.AddDate(-3, 0, 0), EffectiveTo: &ended}},
		"entity-b": {{ParentLegalEntityID: "parent-x", ChildLegalEntityID: "entity-b", EffectiveFrom: periodEnd.AddDate(-3, 0, 0), EffectiveTo: &ended}},
	}}
	r := newRouterWithEntityRegistry(s, &stubAuthZ{}, leg, reg)

	resp := doReq(r, http.MethodPost, "/v1/intercompany/entries/e1/match", map[string]string{"target_journal_id": "journal-1"}, "user-1")
	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("relationship was lost inside the pair's period %s; expected 422, got %d: %s", period, resp.Code, resp.Body.String())
	}
	if s.entries["e1"].MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected MISMATCH recorded, got %q", s.entries["e1"].MatchStatus)
	}
}
