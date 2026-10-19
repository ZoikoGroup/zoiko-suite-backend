package handler_test

// SPEC_DEVIATIONS A3: every match attempt is recorded as structured
// evidence, and ExplainMismatch serves it.

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

func onlyAttempt(t *testing.T, s *stubStoreReal) domain.MatchAttempt {
	t.Helper()
	if len(s.attempts["e1"]) != 1 {
		t.Fatalf("expected exactly one recorded attempt, got %d", len(s.attempts["e1"]))
	}
	return s.attempts["e1"][0]
}

func TestMatchEntry_AmountMismatch_RecordsStructuredAttempt(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	resp := match(t, s, journal("USD", "4000"))
	if resp.FailedRule == nil || *resp.FailedRule != domain.MatchRuleAmountMismatch {
		t.Fatalf("response should name the failed rule, got %+v", resp)
	}

	a := onlyAttempt(t, s)
	if a.Outcome != domain.MatchStatusMismatch || a.FailedRule == nil || *a.FailedRule != domain.MatchRuleAmountMismatch {
		t.Fatalf("outcome/rule: %+v", a)
	}
	if a.Message == nil || *a.Message != *resp.MismatchReason {
		t.Fatalf("message should be the mismatch_reason sentence: %v vs %v", a.Message, resp.MismatchReason)
	}
	if a.Target.ExpectedAmount != "5000" || a.Target.ExpectedCurrency != "USD" {
		t.Fatalf("target expected: %+v", a.Target)
	}
	if a.Target.ObservedAmount == nil || *a.Target.ObservedAmount != "4000" || *a.Target.ObservedCurrency != "USD" ||
		*a.Target.ObservedLegalEntityID != "le-uk" || *a.Target.ObservedStatus != "FINALIZED" {
		t.Fatalf("target observed: %+v", a.Target)
	}
	if a.Source.ExpectedAmount != "5000" || a.Source.JournalID == nil || *a.Source.JournalID != "j-src" || a.Source.ObservedAmount != nil {
		t.Fatalf("source side (expected only, never examined): %+v", a.Source)
	}
	if len(a.ExaminedJournalIDs) != 1 || a.ExaminedJournalIDs[0] != "j-tgt" || a.Tolerance != "0.001" {
		t.Fatalf("examined/tolerance: %v / %s", a.ExaminedJournalIDs, a.Tolerance)
	}
	if a.AttemptedBy != "p-1" {
		t.Fatalf("attempted_by: %q", a.AttemptedBy)
	}
}

// Every negative path names its own rule, and records what was observed
// even when an earlier rule than the amount fails.
func TestMatchEntry_EachRuleIsNamed(t *testing.T) {
	cases := map[string]*stubLedger{
		domain.MatchRuleCounterpartyJournalMissing: {},
		domain.MatchRuleCurrencyMismatch:           journal("GBP", "5000"),
		domain.MatchRuleCurrencyUnverifiable:       journal("XXX", "5000"),
	}
	notFinal := journal("USD", "5000")
	notFinal.journal.Status = "DRAFT"
	cases[domain.MatchRuleTargetJournalNotFinalized] = notFinal
	wrongEntity := journal("USD", "5000")
	wrongEntity.journal.LegalEntityID = "le-de"
	cases[domain.MatchRuleTargetEntityMismatch] = wrongEntity

	for rule, leg := range cases {
		s := newStubStoreReal()
		seedPair(s, "5000", "USD", "5000", "USD")
		match(t, s, leg)
		a := onlyAttempt(t, s)
		if a.FailedRule == nil || *a.FailedRule != rule {
			t.Errorf("%s: recorded rule %v", rule, a.FailedRule)
			continue
		}
		if rule == domain.MatchRuleCounterpartyJournalMissing {
			if a.Target.ObservedAmount != nil {
				t.Errorf("%s: a missing journal has nothing observed, got %+v", rule, a.Target)
			}
		} else if a.Target.ObservedAmount == nil || *a.Target.ObservedAmount != "5000" {
			t.Errorf("%s: observed amount not recorded: %+v", rule, a.Target)
		}
	}
}

func TestMatchEntry_Success_RecordsMatchedAttempt(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	if resp := match(t, s, journal("USD", "5000")); resp.MatchStatus != domain.MatchStatusMatched || resp.FailedRule != nil {
		t.Fatalf("expected MATCHED with no rule, got %+v", resp)
	}
	a := onlyAttempt(t, s)
	if a.Outcome != domain.MatchStatusMatched || a.FailedRule != nil || a.Message != nil {
		t.Fatalf("matched attempt: %+v", a)
	}
}

func TestExplainMismatch(t *testing.T) {
	s := newStubStoreReal()
	seedPair(s, "5000", "USD", "5000", "USD")
	match(t, s, journal("USD", "4000"))
	match(t, s, journal("GBP", "5000"))
	r := newRouter(s, &stubAuthZ{}, &stubLedger{})

	resp := doReq(r, http.MethodGet, "/v1/intercompany/entries/e1/explanation", nil, "p-1")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var got domain.MismatchExplanation
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.MatchStatus != domain.MatchStatusMismatch || len(got.Attempts) != 2 {
		t.Fatalf("expected MISMATCH with both attempts, got %+v", got)
	}
	if *got.Attempts[0].FailedRule != domain.MatchRuleAmountMismatch || *got.Attempts[1].FailedRule != domain.MatchRuleCurrencyMismatch {
		t.Fatalf("attempts out of order or lost: %+v", got.Attempts)
	}
	if got.MismatchReason == nil || *got.MismatchReason != *got.Attempts[1].Message {
		t.Fatalf("mismatch_reason should be the latest attempt's message, got %v", got.MismatchReason)
	}

	if resp := doReq(r, http.MethodGet, "/v1/intercompany/entries/missing/explanation", nil, "p-1"); resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown pair, got %d", resp.Code)
	}
	if resp := doReq(newRouter(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubLedger{}), http.MethodGet,
		"/v1/intercompany/entries/e1/explanation", nil, "p-1"); resp.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when view is denied, got %d", resp.Code)
	}
}
