package handler_test

// SPEC_DEVIATIONS A13: general-ledger-svc requires ACC-03's journal_type,
// transaction/posting dates and currency on every journal, and none of this
// service's posting paths sent them. These tests pin what each path now sends,
// and that a record which cannot supply them is refused before GL is called.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/financial-close-svc/internal/domain"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func assertJournalMeta(t *testing.T, cl *stubClients, wantType string, wantDate time.Time) {
	t.Helper()
	if cl.metaCalls != 1 {
		t.Fatalf("expected exactly one posting, got %d", cl.metaCalls)
	}
	m := cl.lastJournalMeta
	if m.JournalType != wantType || m.CurrencyCode != testCurrency ||
		!m.PostingDate.Equal(wantDate) || !m.TransactionDate.Equal(wantDate) {
		t.Fatalf("journal meta = %+v, want type %s, currency %s, dates %s", m, wantType, testCurrency, wantDate.Format("2006-01-02"))
	}
}

// Every posting path sends the right journal_type, the period's last day as
// both dates, and the record's currency.
func TestPostingPaths_SendACC03JournalContext(t *testing.T) {
	t.Run("ACC-07 accrual recognition", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		id := createApprovedAccrual(t, s, r, 900, 3, "2026-01")
		if rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}, "preparer-1"); rr.Code != http.StatusCreated {
			t.Fatalf("recognize: %d %s", rr.Code, rr.Body.String())
		}
		assertJournalMeta(t, cl, "ACCRUAL", day(2026, 1, 31))
	})
	t.Run("ACC-08 prepayment recognition", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		id := createApprovedPrepayment(t, s, r, 1200, 12, "2026-01")
		if rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/recognize", domain.RunPrepaymentRecognitionRequest{FiscalPeriod: "2026-02"}, "preparer-1"); rr.Code != http.StatusCreated {
			t.Fatalf("recognize: %d %s", rr.Code, rr.Body.String())
		}
		assertJournalMeta(t, cl, "ACCRUAL", day(2026, 2, 28))
	})
	t.Run("ACC-08 termination settlement", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		id := createApprovedPrepayment(t, s, r, 1200, 12, "2026-01")
		req := domain.TerminatePrepaymentRequest{Reason: "policy replaced", FinalBalanceTreatment: domain.TerminationTreatmentRecognizeRemaining, FiscalPeriod: "2026-03"}
		if rr := doReq(r, http.MethodPost, "/v1/prepayments/"+id+"/terminate", req, "preparer-1"); rr.Code != http.StatusOK {
			t.Fatalf("terminate: %d %s", rr.Code, rr.Body.String())
		}
		assertJournalMeta(t, cl, "ACCRUAL", day(2026, 3, 31))
	})
	t.Run("ACC-09 allocation", func(t *testing.T) {
		s := newStubStore()
		cl := &stubClients{trialBalances: map[string]float64{"5000-ITSharedCost": 1000}}
		r := newRouter(s, &stubAuthZ{}, cl)
		ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())
		if rr := doReq(r, http.MethodPost, "/v1/allocation-runs/", domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}, "preparer-1"); rr.Code != http.StatusCreated {
			t.Fatalf("execute: %d %s", rr.Code, rr.Body.String())
		}
		assertJournalMeta(t, cl, "RECLASS", day(2026, 1, 31))
	})
	t.Run("ACC-10 FX revaluation", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		s.fxRuns["run-1"] = &domain.FXRevaluationRun{CurrencyCode: testCurrency, RunID: "run-1", LegalEntityID: "le-1", FiscalPeriod: "2026-01",
			Status: domain.FXRevaluationStatusApproved, FXGainLossAccountCode: "7100-FXGainLoss",
			Items: []domain.FXRevaluationItem{{AccountCode: "1100-ForeignCash", AccountType: domain.AccountTypeAsset, AdjustmentAmount: 200}}}
		if rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/run-1/post", nil, "poster-1"); rr.Code != http.StatusOK {
			t.Fatalf("post: %d %s", rr.Code, rr.Body.String())
		}
		assertJournalMeta(t, cl, "ADJUSTMENT", day(2026, 1, 31))
	})
	t.Run("ACC-17 opening balances", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		batchID := createValidatedMigrationBatch(t, s, cl, r)
		if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
			t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
		}
		if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1"); rr.Code != http.StatusOK {
			t.Fatalf("commit: %d %s", rr.Code, rr.Body.String())
		}
		assertJournalMeta(t, cl, "OPENING", day(2026, 1, 31))
	})
}

// A registered period's own period_end wins over the calendar month: a
// 4-4-5 calendar's "2026-01" can end on the 30th.
func TestPostingDate_UsesTheRegisteredPeriodEnd(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	s.periods["fp-jan"] = &domain.FiscalPeriod{FiscalPeriodID: "fp-jan", TenantID: testTenantID, LegalEntityID: "le-1", PeriodName: "2026-01",
		PeriodStart: day(2026, 1, 1), PeriodEnd: time.Date(2026, 1, 30, 23, 59, 59, 0, time.UTC), CloseStatus: domain.PeriodOpen}
	id := createApprovedAccrual(t, s, r, 900, 3, "2026-01")
	if rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}, "preparer-1"); rr.Code != http.StatusCreated {
		t.Fatalf("recognize: %d %s", rr.Code, rr.Body.String())
	}
	assertJournalMeta(t, cl, "ACCRUAL", day(2026, 1, 30))
}

// A period that is neither registered nor YYYY-MM has no knowable posting
// date; it is refused, never guessed, and GL is never called.
func TestPostingDate_Unresolvable_RefusedBeforeGL(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{CurrencyCode: testCurrency, RunID: "run-1", LegalEntityID: "le-1", FiscalPeriod: "FY26-P01",
		Status: domain.FXRevaluationStatusApproved, FXGainLossAccountCode: "7100-FXGainLoss",
		Items: []domain.FXRevaluationItem{{AccountCode: "1100-ForeignCash", AccountType: domain.AccountTypeAsset, AdjustmentAmount: 200}}}
	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/run-1/post", nil, "poster-1")
	if rr.Code != http.StatusUnprocessableEntity || !containsErrorCode(rr.Body.Bytes(), "posting_date_unresolvable") {
		t.Fatalf("expected 422 posting_date_unresolvable, got %d %s", rr.Code, rr.Body.String())
	}
	if cl.postMultiLineCalls != 0 || s.fxRuns["run-1"].Status != domain.FXRevaluationStatusApproved {
		t.Fatalf("expected no GL call and the run still APPROVED; calls=%d status=%s", cl.postMultiLineCalls, s.fxRuns["run-1"].Status)
	}
}

// A record written before migration 000022 has no currency. It is refused
// with currency_code_missing before GL is called and before any state moves,
// instead of reaching GL as a 400 that surfaced as "GL unavailable".
func TestLegacyRecordWithoutCurrency_RefusedBeforeGL(t *testing.T) {
	t.Run("accrual", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		id := createApprovedAccrual(t, s, r, 900, 3, "2026-01")
		s.schedules[id].CurrencyCode = ""
		rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognize", domain.RunAccrualRecognitionRequest{FiscalPeriod: "2026-01"}, "preparer-1")
		if rr.Code != http.StatusUnprocessableEntity || !containsErrorCode(rr.Body.Bytes(), "currency_code_missing") {
			t.Fatalf("expected 422 currency_code_missing, got %d %s", rr.Code, rr.Body.String())
		}
		if cl.postJournalCalls != 0 {
			t.Fatalf("GL was called %d times", cl.postJournalCalls)
		}
	})
	t.Run("allocation leaves no run behind", func(t *testing.T) {
		s := newStubStore()
		cl := &stubClients{trialBalances: map[string]float64{"5000-ITSharedCost": 1000}}
		r := newRouter(s, &stubAuthZ{}, cl)
		ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())
		s.allocationRules[ruleID].CurrencyCode = ""
		rr := doReq(r, http.MethodPost, "/v1/allocation-runs/", domain.ExecuteAllocationRequest{RuleID: ruleID, FiscalPeriod: "2026-01"}, "preparer-1")
		if rr.Code != http.StatusUnprocessableEntity || !containsErrorCode(rr.Body.Bytes(), "currency_code_missing") {
			t.Fatalf("expected 422 currency_code_missing, got %d %s", rr.Code, rr.Body.String())
		}
		if cl.postAllocationCalls != 0 || len(s.allocationRuns) != 0 {
			t.Fatalf("expected no GL call and no run; calls=%d runs=%d", cl.postAllocationCalls, len(s.allocationRuns))
		}
	})
	t.Run("migration batch stays APPROVED", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		batchID := createValidatedMigrationBatch(t, s, cl, r)
		if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
			t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
		}
		s.migrationBatches[batchID].CurrencyCode = ""
		rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1")
		if rr.Code != http.StatusUnprocessableEntity || !containsErrorCode(rr.Body.Bytes(), "currency_code_missing") {
			t.Fatalf("expected 422 currency_code_missing, got %d %s", rr.Code, rr.Body.String())
		}
		if cl.postMultiLineCalls != 0 || s.migrationBatches[batchID].Status != domain.MigrationBatchStatusApproved {
			t.Fatalf("expected no GL call and the batch still APPROVED; calls=%d status=%s", cl.postMultiLineCalls, s.migrationBatches[batchID].Status)
		}
	})
}

// Every command that creates a record which will post requires a well-formed
// currency up front, so the gap cannot reopen for new records.
func TestCreateCommands_RequireAValidCurrency(t *testing.T) {
	creates := []struct {
		path string
		body func(currency string) any
	}{
		{"/v1/accruals/", func(c string) any {
			return domain.CreateAccrualRequest{CurrencyCode: c, LegalEntityID: "le-1", Description: "d", PolicyVersion: "v1", TotalAmount: 900,
				StartFiscalPeriod: "2026-01", PeriodCount: 3, DebitAccountCode: "6100", CreditAccountCode: "2100"}
		}},
		{"/v1/prepayments/", func(c string) any {
			return domain.CreatePrepaymentRequest{CurrencyCode: c, LegalEntityID: "le-1", Description: "d", TotalAmount: 1200,
				StartFiscalPeriod: "2026-01", PeriodCount: 12, DebitAccountCode: "6200", CreditAccountCode: "1400"}
		}},
		{"/v1/allocation-rules/", func(c string) any {
			return domain.CreateAllocationRuleRequest{CurrencyCode: c, LegalEntityID: "le-1", Name: "IT", SourceAccountCode: "5000", Drivers: evenDrivers()}
		}},
		{"/v1/fx-revaluations/", func(c string) any {
			return domain.StartRevaluationRequest{CurrencyCode: c, LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7900",
				RateSet: map[string]float64{"EUR": 1.1}, Items: []domain.RevaluationItemInput{{AccountCode: "1100", CurrencyCode: "EUR", ForeignAmount: 10}}}
		}},
		{"/v1/migration-batches/", func(c string) any {
			return domain.CreateMigrationBatchRequest{CurrencyCode: c, LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "ERP",
				SourceExtractHash: "sha256:x", ExpectedRowCount: 2, ExpectedTotalDebits: 10000, ExpectedTotalCredits: 10000, Entries: balancedMigrationEntries()}
		}},
	}
	for _, c := range creates {
		t.Run(c.path, func(t *testing.T) {
			r := newRouter(newStubStore(), &stubAuthZ{}, &stubClients{})
			if rr := doReq(r, http.MethodPost, c.path, c.body(""), "preparer-1"); rr.Code != http.StatusBadRequest || !containsErrorCode(rr.Body.Bytes(), "missing_fields") {
				t.Errorf("no currency: expected 400 missing_fields, got %d %s", rr.Code, rr.Body.String())
			}
			if rr := doReq(r, http.MethodPost, c.path, c.body("gbp"), "preparer-1"); rr.Code != http.StatusBadRequest || !containsErrorCode(rr.Body.Bytes(), "invalid_currency_code") {
				t.Errorf("lower-case currency: expected 400 invalid_currency_code, got %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// A superseding rule version keeps the rule's currency, may give a legacy
// rule its first one, and never changes one the rule already has.
func TestSupersedeAllocationRule_Currency(t *testing.T) {
	supersede := func(currency string) domain.SupersedeAllocationRuleRequest {
		req := supersedeReq()
		req.CurrencyCode = currency
		return req
	}
	t.Run("kept when omitted", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())
		if rr := doReq(r, http.MethodPost, "/v1/allocation-rules/"+ruleID+"/supersede", supersede(""), "preparer-2"); rr.Code != http.StatusCreated {
			t.Fatalf("supersede: %d %s", rr.Code, rr.Body.String())
		}
		if got := s.allocationRules[ruleID].CurrencyCode; got != testCurrency {
			t.Fatalf("new version currency = %q, want %q", got, testCurrency)
		}
	})
	t.Run("never changed", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())
		rr := doReq(r, http.MethodPost, "/v1/allocation-rules/"+ruleID+"/supersede", supersede("EUR"), "preparer-2")
		if rr.Code != http.StatusUnprocessableEntity || !containsErrorCode(rr.Body.Bytes(), "currency_code_immutable") {
			t.Fatalf("expected 422 currency_code_immutable, got %d %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("a legacy rule acquires one", func(t *testing.T) {
		s, cl := newStubStore(), &stubClients{}
		r := newRouter(s, &stubAuthZ{}, cl)
		ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())
		s.allocationRules[ruleID].CurrencyCode = ""
		if rr := doReq(r, http.MethodPost, "/v1/allocation-rules/"+ruleID+"/supersede", supersede(""), "preparer-2"); rr.Code != http.StatusBadRequest {
			t.Fatalf("legacy rule, no currency: expected 400, got %d %s", rr.Code, rr.Body.String())
		}
		if rr := doReq(r, http.MethodPost, "/v1/allocation-rules/"+ruleID+"/supersede", supersede("EUR"), "preparer-2"); rr.Code != http.StatusCreated {
			t.Fatalf("legacy rule with currency: %d %s", rr.Code, rr.Body.String())
		}
		if got := s.allocationRules[ruleID].CurrencyCode; got != "EUR" {
			t.Fatalf("new version currency = %q, want EUR", got)
		}
	})
}

// A reversal run is booked in the currency of the run it reverses.
func TestReversePriorRevaluation_InheritsTheCurrency(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	postedAt := time.Now().UTC()
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{CurrencyCode: "USD", RunID: "run-1", LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		Status: domain.FXRevaluationStatusPosted, FXGainLossAccountCode: "7100-FXGainLoss", PostedAt: &postedAt,
		Items: []domain.FXRevaluationItem{{AccountCode: "1100-ForeignCash", AccountType: domain.AccountTypeAsset, AdjustmentAmount: 200}}}
	rr := doReq(r, http.MethodPost, "/v1/fx-revaluations/reverse", domain.ReversePriorRevaluationRequest{PriorRunID: "run-1"}, "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("reverse: %d %s", rr.Code, rr.Body.String())
	}
	var rev domain.FXRevaluationRun
	if err := json.NewDecoder(rr.Body).Decode(&rev); err != nil {
		t.Fatal(err)
	}
	if rev.CurrencyCode != "USD" {
		t.Fatalf("reversal currency = %q, want USD", rev.CurrencyCode)
	}
}

// The reversal's GL call names the schedule's legal entity, which
// general-ledger-svc's envelope requires on a write.
func TestReverseAccrualRecognition_PassesTheLegalEntityToGL(t *testing.T) {
	s, cl := newStubStore(), &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	id := createApprovedAccrual(t, s, r, 900, 3, "2026-01")
	recognizeOnePeriod(t, r, id, "2026-01")
	if rr := doReq(r, http.MethodPost, "/v1/accruals/"+id+"/recognitions/2026-01/reverse", map[string]string{"reason": "estimate corrected"}, "reverser-1"); rr.Code != http.StatusCreated {
		t.Fatalf("reverse: %d %s", rr.Code, rr.Body.String())
	}
	if cl.lastReverseLegalEntity != "le-1" {
		t.Fatalf("ReverseGLJournal legal entity = %q, want le-1", cl.lastReverseLegalEntity)
	}
}

// containsErrorCode reports whether an error response body carries code.
func containsErrorCode(body []byte, code string) bool {
	var e struct {
		ErrorCode string `json:"error_code"`
	}
	return json.Unmarshal(body, &e) == nil && e.ErrorCode == code
}
