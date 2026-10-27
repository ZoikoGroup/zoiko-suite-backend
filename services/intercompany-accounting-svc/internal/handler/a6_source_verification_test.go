package handler_test

// SPEC_DEVIATIONS A6: the source journal gets the same four checks
// MatchEntry runs on the target, at create time. A journal that fails one
// creates the pair in MISMATCH with a named rule, not a 4xx/5xx that makes
// the caller retry a request that will fail identically.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	"zoiko.io/intercompany-accounting-svc/internal/ledger"
)

// sourceJournal returns a general-ledger-svc stub whose journal has the
// fields the source checks look at. The amount is irrelevant to A6 (the
// source amount is not compared at create), but journalTotal needs lines.
func sourceJournal(entity, status, currency string) *stubLedger {
	return &stubLedger{journal: &ledger.JournalDetail{
		JournalID: "j-001", LegalEntityID: entity, Status: status, CurrencyCode: currency,
		Lines: []ledger.JournalLine{{AccountCode: "1300", DebitAmount: "5000"}},
	}}
}

func createEntry(t *testing.T, s *stubStoreReal, leg *stubLedger) (domain.IntercompanyEntry, int, string) {
	t.Helper()
	rr := doReq(newRouter(s, &stubAuthZ{}, leg), http.MethodPost, "/v1/intercompany/entries/", createBody(nil), "p-1")
	var got domain.IntercompanyEntry
	_ = json.NewDecoder(rr.Body).Decode(&got)
	return got, rr.Code, rr.Body.String()
}

func createAttempt(t *testing.T, s *stubStoreReal, entryID string) domain.MatchAttempt {
	t.Helper()
	got := s.attempts[entryID]
	if len(got) != 1 {
		t.Fatalf("expected exactly one create-time attempt for %s, got %d", entryID, len(got))
	}
	return got[0]
}

func assertSourceMismatch(t *testing.T, s *stubStoreReal, leg *stubLedger, rule, reasonPrefix string) domain.MatchAttempt {
	t.Helper()
	entry, code, body := createEntry(t, s, leg)
	if code != http.StatusCreated {
		t.Fatalf("expected 201 (the pair is created in MISMATCH), got %d: %s", code, body)
	}
	if entry.MatchStatus != domain.MatchStatusMismatch {
		t.Fatalf("expected MISMATCH, got %q", entry.MatchStatus)
	}
	if entry.MismatchReason == nil || !strings.HasPrefix(*entry.MismatchReason, reasonPrefix) {
		t.Fatalf("expected a %q reason, got %v", reasonPrefix, entry.MismatchReason)
	}
	if len(s.createdEvents) != 1 {
		t.Fatalf("expected the created event, got %d", len(s.createdEvents))
	}

	a := createAttempt(t, s, entry.IntercompanyEntryID)
	if a.Outcome != domain.MatchStatusMismatch || a.FailedRule == nil || *a.FailedRule != rule {
		t.Fatalf("expected a %s attempt, got %+v", rule, a)
	}
	if a.Message == nil || *a.Message != *entry.MismatchReason {
		t.Fatalf("attempt message should be the reported reason: %v vs %v", a.Message, entry.MismatchReason)
	}
	if a.Source.JournalID == nil || *a.Source.JournalID != "j-001" ||
		a.Source.ExpectedAmount != "5000" || a.Source.ExpectedCurrency != "USD" {
		t.Fatalf("source expected side: %+v", a.Source)
	}
	if a.Target.JournalID != nil || a.Target.ExpectedAmount != "5000" || a.Target.ExpectedCurrency != "USD" {
		t.Fatalf("target side should be expected-only until a match names it: %+v", a.Target)
	}
	if len(a.ExaminedJournalIDs) != 1 || a.ExaminedJournalIDs[0] != "j-001" || a.Tolerance != "0.001" {
		t.Fatalf("examined/tolerance: %v / %s", a.ExaminedJournalIDs, a.Tolerance)
	}
	if a.AttemptedBy != "p-1" {
		t.Fatalf("attempted_by: %q", a.AttemptedBy)
	}
	return a
}

func TestCreateEntry_SourceJournalMissing_CreatesMismatchNotOutage(t *testing.T) {
	s := newStubStoreReal()
	a := assertSourceMismatch(t, s, &stubLedger{}, domain.MatchRuleSourceJournalMissing,
		"source journal does not exist in general-ledger-svc")
	if a.Source.ObservedAmount != nil || a.Source.ObservedLegalEntityID != nil {
		t.Fatalf("a journal that does not exist has nothing observed: %+v", a.Source)
	}
}

func TestCreateEntry_SourceJournalWrongEntity_CreatesMismatch(t *testing.T) {
	s := newStubStoreReal()
	a := assertSourceMismatch(t, s, sourceJournal("le-de", "FINALIZED", "USD"), domain.MatchRuleSourceEntityMismatch,
		"source_journal_entity_mismatch:")
	if a.Source.ObservedLegalEntityID == nil || *a.Source.ObservedLegalEntityID != "le-de" {
		t.Fatalf("expected the observed entity recorded, got %+v", a.Source)
	}
}

func TestCreateEntry_SourceJournalNotFinalized_CreatesMismatch(t *testing.T) {
	s := newStubStoreReal()
	a := assertSourceMismatch(t, s, sourceJournal("le-us", "DRAFT", "USD"), domain.MatchRuleSourceJournalNotFinalized,
		"source_journal_not_finalized:")
	if a.Source.ObservedStatus == nil || *a.Source.ObservedStatus != "DRAFT" {
		t.Fatalf("expected the observed status recorded, got %+v", a.Source)
	}
}

func TestCreateEntry_SourceJournalWrongCurrency_CreatesMismatch(t *testing.T) {
	s := newStubStoreReal()
	a := assertSourceMismatch(t, s, sourceJournal("le-us", "FINALIZED", "GBP"), domain.MatchRuleSourceCurrencyMismatch,
		"currency_mismatch:")
	if a.Source.ObservedCurrency == nil || *a.Source.ObservedCurrency != "GBP" {
		t.Fatalf("expected the observed currency recorded, got %+v", a.Source)
	}
}

// general-ledger-svc's "XXX" (journals predating its currency column) is not
// a currency, so it is unverifiable rather than merely different.
func TestCreateEntry_SourceJournalCurrencyUnverifiable_CreatesMismatch(t *testing.T) {
	s := newStubStoreReal()
	assertSourceMismatch(t, s, sourceJournal("le-us", "FINALIZED", "XXX"), domain.MatchRuleSourceCurrencyUnverifiable,
		"currency_unverifiable:")
}

func TestCreateEntry_VerifiedSourceJournal_StaysUnmatched(t *testing.T) {
	s := newStubStoreReal()
	entry, code, body := createEntry(t, s, ledgerWithSourceJournal("le-us", "USD"))
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", code, body)
	}
	if entry.MatchStatus != domain.MatchStatusUnmatched || entry.MismatchReason != nil {
		t.Fatalf("a verified source journal must leave the pair UNMATCHED, got %q / %v", entry.MatchStatus, entry.MismatchReason)
	}
	if len(s.attempts) != 0 {
		t.Fatalf("a successful source check is not a mismatch attempt: %+v", s.attempts)
	}
}

// A general-ledger-svc outage is transient: unlike a wrong journal, it
// creates nothing and answers 503 so the caller can retry.
func TestCreateEntry_SourceJournalQueryOutage_Is503(t *testing.T) {
	s := newStubStoreReal()
	_, code, body := createEntry(t, s, &stubLedger{err: domain.ErrGLServiceUnavailable})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", code, body)
	}
	if len(s.entries) != 0 || len(s.createdEvents) != 0 {
		t.Fatalf("an outage must not create a pair: %d entries, %d events", len(s.entries), len(s.createdEvents))
	}
}

// A replay is resolved by source_journal_id before general-ledger-svc is
// consulted, so a later outage cannot turn a successful create's retry into
// a 503, and no second attempt is written.
func TestCreateEntry_ReplayDoesNotRecheckSourceJournal(t *testing.T) {
	s := newStubStoreReal()
	leg := ledgerWithSourceJournal("le-us", "USD")
	first, code, body := createEntry(t, s, leg)
	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", code, body)
	}

	leg.err = domain.ErrGLServiceUnavailable
	var replay struct {
		IntercompanyEntryID string `json:"intercompany_entry_id"`
	}
	rr := doReq(newRouter(s, &stubAuthZ{}, leg), http.MethodPost, "/v1/intercompany/entries/", createBody(nil), "p-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a replay, got %d: %s", rr.Code, rr.Body.String())
	}
	_ = json.NewDecoder(rr.Body).Decode(&replay)
	if replay.IntercompanyEntryID != first.IntercompanyEntryID {
		t.Fatalf("replay resolved to %s, original was %s", replay.IntercompanyEntryID, first.IntercompanyEntryID)
	}
	if len(s.entries) != 1 || len(s.attempts) != 0 {
		t.Fatalf("replay must not create or record anything: %d entries, %d attempt lists", len(s.entries), len(s.attempts))
	}
}
