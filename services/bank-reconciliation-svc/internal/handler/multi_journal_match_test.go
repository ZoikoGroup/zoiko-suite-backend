package handler_test

// A2b part 2's many-to-one match path: a statement line booked as two or
// more journals, through the real propose/confirm maker-checker routes.
// See multi_journal_match_pg_smoke_test.go for the real-Postgres evidence
// (statement_line_matched_journals) and migration-guard coverage.

import (
	"net/http"
	"testing"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/ledger"
)

// journalOn returns a FINALIZED journal with the given id, legal entity and
// net cash movement on account "1000" (positive: debit/money in; negative:
// credit/money out) — finalizedJournal fixed at "j1", which multi-journal
// tests can't use since every journal here needs its own distinct id.
func journalOn(id, legalEntityID string, amount float64) *ledger.Journal {
	cash := ledger.JournalLine{AccountCode: "1000"}
	contra := ledger.JournalLine{AccountCode: "4000"}
	if amount >= 0 {
		cash.DebitAmount = amount
		contra.CreditAmount = amount
	} else {
		cash.CreditAmount = -amount
		contra.DebitAmount = -amount
	}
	return &ledger.Journal{JournalID: id, LegalEntityID: legalEntityID, Status: "FINALIZED", Lines: []ledger.JournalLine{cash, contra}}
}

func TestProposeMatch_MultiJournal_Succeeds(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	led := &stubLedger{byID: map[string]stubLedgerEntry{
		"j1": {journal: journalOn("j1", "e1", 400)},
		"j2": {journal: journalOn("j2", "e1", 600)},
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, led)
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/propose-match",
		domain.MatchStatementLineRequest{JournalIDs: []string{"j1", "j2"}}, "maker-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	l := s.lines["l1"]
	if l.Status != domain.StatementLineStatusPendingConfirmation {
		t.Fatalf("expected PENDING_CONFIRMATION, got %s", l.Status)
	}
	if len(l.ProposedJournalIDs) != 2 {
		t.Fatalf("expected both journal ids recorded, got %v", l.ProposedJournalIDs)
	}
	if l.ProposedJournalID != nil {
		t.Fatalf("the scalar proposed_journal_id must stay unset for a multi-journal proposal, got %v", *l.ProposedJournalID)
	}
}

func TestProposeMatch_MultiJournal_CombinedAmountMismatch_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	led := &stubLedger{byID: map[string]stubLedgerEntry{
		"j1": {journal: journalOn("j1", "e1", 400)},
		"j2": {journal: journalOn("j2", "e1", 500)}, // 400+500 = 900, not 1000
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, led)
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/propose-match",
		domain.MatchStatementLineRequest{JournalIDs: []string{"j1", "j2"}}, "maker-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a combined amount that doesn't sum to the line, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusUnmatched {
		t.Fatalf("line must remain UNMATCHED, got %s", s.lines["l1"].Status)
	}
}

func TestProposeMatch_MultiJournal_OneJournalWrongEntity_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	led := &stubLedger{byID: map[string]stubLedgerEntry{
		"j1": {journal: journalOn("j1", "e1", 400)},
		"j2": {journal: journalOn("j2", "other-entity", 600)},
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, led)
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/propose-match",
		domain.MatchStatementLineRequest{JournalIDs: []string{"j1", "j2"}}, "maker-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when one journal belongs to another legal entity, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProposeMatch_MultiJournal_DuplicateID_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	led := &stubLedger{byID: map[string]stubLedgerEntry{"j1": {journal: journalOn("j1", "e1", 1000)}}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, led)
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/propose-match",
		domain.MatchStatementLineRequest{JournalIDs: []string{"j1", "j1"}}, "maker-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a duplicated journal id (never silently double-counted), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestProposeMatch_MultiJournal_TooFewOrTooMany_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}

	tooMany := make([]string, domain.MaxMatchJournals+1)
	byID := map[string]stubLedgerEntry{}
	for i := range tooMany {
		id := "j" + string(rune('a'+i))
		tooMany[i] = id
		byID[id] = stubLedgerEntry{journal: journalOn(id, "e1", 1)}
	}
	for name, ids := range map[string][]string{"one id": {"j1"}, "too many": tooMany} {
		t.Run(name, func(t *testing.T) {
			r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{byID: byID})
			rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/propose-match",
				domain.MatchStatementLineRequest{JournalIDs: ids}, "maker-1")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestProposeMatch_JournalIDsMutuallyExclusiveWithOthers_Returns400(t *testing.T) {
	s := newStubStore()
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000, Status: domain.StatementLineStatusUnmatched, GLCashAccountCode: cashAcct()}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/propose-match",
		domain.MatchStatementLineRequest{JournalID: "j1", JournalIDs: []string{"j1", "j2"}}, "maker-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when journal_id and journal_ids are both set, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestConfirmMatch_MultiJournal_DifferentPrincipal_Succeeds(t *testing.T) {
	s := newStubStore()
	maker := "maker-1"
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000,
		Status: domain.StatementLineStatusPendingConfirmation, GLCashAccountCode: cashAcct(),
		ProposedJournalIDs: []string{"j1", "j2"}, ProposedByPrincipalID: &maker}

	led := &stubLedger{byID: map[string]stubLedgerEntry{
		"j1": {journal: journalOn("j1", "e1", 400)},
		"j2": {journal: journalOn("j2", "e1", 600)},
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, led)
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/confirm-match", nil, "checker-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	l := s.lines["l1"]
	if l.Status != domain.StatementLineStatusMatched {
		t.Fatalf("expected MATCHED, got %s", l.Status)
	}
	if len(l.MatchedJournalIDs) != 2 {
		t.Fatalf("expected both journal ids recorded as matched, got %v", l.MatchedJournalIDs)
	}
	if l.MatchedJournalID != nil {
		t.Fatalf("the scalar matched_journal_id must stay unset for a multi-journal match, got %v", *l.MatchedJournalID)
	}
}

func TestConfirmMatch_MultiJournal_SelfConfirmation_Forbidden(t *testing.T) {
	s := newStubStore()
	maker := "maker-1"
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000,
		Status: domain.StatementLineStatusPendingConfirmation, GLCashAccountCode: cashAcct(),
		ProposedJournalIDs: []string{"j1", "j2"}, ProposedByPrincipalID: &maker}

	// Re-verification (which the handler always runs before the store's own
	// self-confirmation check) must still pass here — valid journals, same
	// as the maker would have seen — so this proves the self-confirmation
	// rule itself, not a verification failure that happens to also be 4xx.
	led := &stubLedger{byID: map[string]stubLedgerEntry{
		"j1": {journal: journalOn("j1", "e1", 400)},
		"j2": {journal: journalOn("j2", "e1", 600)},
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, led)
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/confirm-match", nil, "maker-1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for self-confirmation, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Defense in depth: a journal that passed verification at propose time but
// no longer does at confirm time (e.g. it was reversed) must still refuse —
// the checker step never trusts that the propose-time check is still true.
func TestConfirmMatch_MultiJournal_ReVerificationFails_Returns400(t *testing.T) {
	s := newStubStore()
	maker := "maker-1"
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000,
		Status: domain.StatementLineStatusPendingConfirmation, GLCashAccountCode: cashAcct(),
		ProposedJournalIDs: []string{"j1", "j2"}, ProposedByPrincipalID: &maker}

	reversed := journalOn("j2", "e1", 600)
	reversed.Status = "REVERSED"
	led := &stubLedger{byID: map[string]stubLedgerEntry{
		"j1": {journal: journalOn("j1", "e1", 400)},
		"j2": {journal: reversed},
	}}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, led)
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/confirm-match", nil, "checker-1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when a proposed journal no longer verifies, got %d: %s", rec.Code, rec.Body.String())
	}
	if s.lines["l1"].Status != domain.StatementLineStatusPendingConfirmation {
		t.Fatalf("a failed re-verification must not advance the line, got %s", s.lines["l1"].Status)
	}
}

func TestRejectProposedMatch_MultiJournal_ClearsProposal(t *testing.T) {
	s := newStubStore()
	maker := "maker-1"
	s.lines["l1"] = &domain.StatementLine{StatementLineID: "l1", TenantID: "t1", LegalEntityID: "e1", Amount: 1000,
		Status: domain.StatementLineStatusPendingConfirmation, GLCashAccountCode: cashAcct(),
		ProposedJournalIDs: []string{"j1", "j2"}, ProposedByPrincipalID: &maker}

	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubLedger{})
	rec := doRequest(r, http.MethodPost, "/v1/statement-lines/l1/reject-match",
		domain.FlagExceptionRequest{Reason: "wrong journals"}, "checker-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	l := s.lines["l1"]
	if l.Status != domain.StatementLineStatusException {
		t.Fatalf("expected EXCEPTION, got %s", l.Status)
	}
	if len(l.ProposedJournalIDs) != 0 {
		t.Fatalf("expected the withdrawn proposal cleared, got %v", l.ProposedJournalIDs)
	}
}
