package handler_test

// ZS-SVC-B-001 first survey (SPEC_DEVIATIONS.md B1-B8): maker-checker on
// ACC-07/08/09/10/17, command idempotency on the four aggregate creates, one
// reversal per FX run, ACC-17's source-to-target reconciliation, one chart
// read per command, and ACC-18's permission and rebuild corrections.

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	"zoiko.io/financial-close-svc/internal/handler"
	"zoiko.io/financial-close-svc/internal/middleware"
)

func newRouterWithAuthZ(s *stubStore, authz handler.AuthZClient, cl *stubClients) chi.Router {
	rt := chi.NewRouter()
	rt.Use(middleware.TenantContext())
	handler.RegisterRoutes(rt, handler.New(s, authz, cl, testSigningKey, zap.NewNop()).SetSubledgerControlGateEnforced(false))
	return rt
}

func requireSelfApprovalRefused(t *testing.T, rr interface {
	Result() *http.Response
}, what string) {
	t.Helper()
	res := rr.Result()
	defer res.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != http.StatusForbidden || body["error_code"] != "self_approval_forbidden" {
		t.Fatalf("%s: want 403 self_approval_forbidden, got %d %v", what, res.StatusCode, body)
	}
}

// ── B1. Maker-checker ────────────────────────────────────────────────────────

func TestApproveAccrual_PreparerOrSubmitterCannotApprove(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	creator, submitter := "preparer-1", "submitter-1"
	s.schedules["sch-1"] = &domain.AccrualSchedule{ScheduleID: "sch-1", LegalEntityID: "le-1",
		Status: domain.AccrualStatusPendingApproval, CreatedByPrincipalID: creator, SubmittedByPrincipalID: &submitter}

	for _, self := range []string{creator, submitter} {
		requireSelfApprovalRefused(t, doReq(r, http.MethodPost, "/v1/accruals/sch-1/approve", nil, self), "accrual approved by "+self)
	}
	if s.schedules["sch-1"].Status != domain.AccrualStatusPendingApproval {
		t.Fatalf("a refused approval changed the schedule to %s", s.schedules["sch-1"].Status)
	}
	if rr := doReq(r, http.MethodPost, "/v1/accruals/sch-1/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("independent approval: want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestApprove_PreparerCannotApproveOwnItem_ACC08_09_10_17(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"6100-Sales": "ACTIVE"}}
	r := newRouter(s, &stubAuthZ{}, cl)
	s.prepaymentSchedules["ppy-1"] = &domain.PrepaymentSchedule{ScheduleID: "ppy-1", LegalEntityID: "le-1",
		Status: domain.PrepaymentStatusDraft, CreatedByPrincipalID: "preparer-1"}
	s.allocationRules["rule-1"] = &domain.AllocationRule{RuleID: "rule-1", RuleVersionID: "rule-1", LegalEntityID: "le-1",
		Status: domain.AllocationRuleStatusDraft, CreatedByPrincipalID: "preparer-1",
		Drivers: []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}}}
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{RunID: "run-1", LegalEntityID: "le-1",
		Status: domain.FXRevaluationStatusReview, CreatedByPrincipalID: "preparer-1"}
	s.migrationBatches["batch-1"] = &domain.MigrationBatch{BatchID: "batch-1", LegalEntityID: "le-1",
		Status: domain.MigrationBatchStatusValidated, CreatedByPrincipalID: "preparer-1"}

	for _, path := range []string{
		"/v1/prepayments/ppy-1/approve",
		"/v1/allocation-rules/rule-1/approve",
		"/v1/fx-revaluations/run-1/approve",
		"/v1/migration-batches/batch-1/approve",
	} {
		requireSelfApprovalRefused(t, doReq(r, http.MethodPost, path, nil, "preparer-1"), path)
		if rr := doReq(r, http.MethodPost, path, nil, "approver-1"); rr.Code != http.StatusOK {
			t.Fatalf("%s by an independent approver: want 200, got %d %s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestCertifyMigration_PreparerCannotCertifyOwnCutover(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	s.migrationBatches["batch-1"] = &domain.MigrationBatch{BatchID: "batch-1", LegalEntityID: "le-1",
		Status: domain.MigrationBatchStatusReconciled, CreatedByPrincipalID: "preparer-1"}

	requireSelfApprovalRefused(t, doReq(r, http.MethodPost, "/v1/migration-batches/batch-1/certify",
		domain.CertifyMigrationBatchRequest{Reason: "cutover signed"}, "preparer-1"), "self-certification")
	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/batch-1/certify",
		domain.CertifyMigrationBatchRequest{Reason: "cutover signed"}, "certifier-1"); rr.Code != http.StatusOK {
		t.Fatalf("independent certification: want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

// The database's sod_* constraints are the backstop: a refusal that reaches
// the store is still answered as maker-checker, not as an outage.
func TestApprove_StoreSelfApprovalIsForbiddenNotUnavailable(t *testing.T) {
	s := newStubStore()
	s.transitionAccrualErr = domain.ErrSelfApproval
	s.schedules["sch-1"] = &domain.AccrualSchedule{ScheduleID: "sch-1", LegalEntityID: "le-1",
		Status: domain.AccrualStatusPendingApproval, CreatedByPrincipalID: "preparer-1"}
	r := newRouter(s, &stubAuthZ{}, &stubClients{})
	requireSelfApprovalRefused(t, doReq(r, http.MethodPost, "/v1/accruals/sch-1/approve", nil, "approver-1"), "store-level refusal")
}

// ── B3. Command idempotency ──────────────────────────────────────────────────

func TestCreateCommands_IdempotencyKey_RequiredReplayedAndConflicting(t *testing.T) {
	accrual := domain.CreateAccrualRequest{LegalEntityID: "le-1", Description: "audit fee", PolicyVersion: "v1", TotalAmount: 900,
		StartFiscalPeriod: "2026-01", PeriodCount: 3, DebitAccountCode: "6100-AuditFee", CreditAccountCode: "2100-Accrued"}
	prepayment := domain.CreatePrepaymentRequest{LegalEntityID: "le-1", Description: "insurance", TotalAmount: 1200,
		StartFiscalPeriod: "2026-01", PeriodCount: 12, DebitAccountCode: "6200-Insurance", CreditAccountCode: "1400-Prepaid"}
	rule := domain.CreateAllocationRuleRequest{LegalEntityID: "le-1", Name: "IT", SourceAccountCode: "5000-IT",
		Drivers: []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}}}
	revaluation := domain.StartRevaluationRequest{LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7900-FX",
		RateSet: map[string]float64{"EUR": 1.1},
		Items:   []domain.RevaluationItemInput{{AccountCode: "1100-EURCash", CurrencyCode: "EUR", ForeignAmount: 1000}}}

	cases := []struct {
		name, path string
		body       any
		changed    any
		idOf       func(*testing.T, []byte) string
	}{
		{"CreateAccrual", "/v1/accruals/", accrual, func() any { c := accrual; c.TotalAmount = 901; return c }(),
			func(t *testing.T, b []byte) string {
				var v domain.AccrualSchedule
				mustDecode(t, b, &v)
				return v.ScheduleID
			}},
		{"CreateRecognitionSchedule", "/v1/prepayments/", prepayment, func() any { c := prepayment; c.PeriodCount = 6; return c }(),
			func(t *testing.T, b []byte) string {
				var v domain.PrepaymentSchedule
				mustDecode(t, b, &v)
				return v.ScheduleID
			}},
		{"CreateAllocationRule", "/v1/allocation-rules/", rule, func() any { c := rule; c.Name = "IT v2"; return c }(),
			func(t *testing.T, b []byte) string {
				var v domain.AllocationRule
				mustDecode(t, b, &v)
				return v.RuleID
			}},
		{"StartRevaluation", "/v1/fx-revaluations/", revaluation, func() any { c := revaluation; c.FiscalPeriod = "2026-02"; return c }(),
			func(t *testing.T, b []byte) string {
				var v domain.FXRevaluationRun
				mustDecode(t, b, &v)
				return v.RunID
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubStore()
			cl := &stubClients{
				trialBalances: map[string]float64{"1100-EURCash": 1080},
				accountTypes:  map[string]string{"1100-EURCash": domain.AccountTypeAsset},
			}
			r := newRouter(s, &stubAuthZ{}, cl)

			if rr := doReqKeyed(r, http.MethodPost, tc.path, tc.body, "preparer-1", testTenantID, ""); rr.Code != http.StatusBadRequest {
				t.Fatalf("no Idempotency-Key: want 400, got %d %s", rr.Code, rr.Body.String())
			}

			first := doReqKeyed(r, http.MethodPost, tc.path, tc.body, "preparer-1", testTenantID, "key-1")
			if first.Code != http.StatusCreated {
				t.Fatalf("first: want 201, got %d %s", first.Code, first.Body.String())
			}
			replay := doReqKeyed(r, http.MethodPost, tc.path, tc.body, "preparer-1", testTenantID, "key-1")
			if replay.Code != http.StatusOK {
				t.Fatalf("replay: want 200, got %d %s", replay.Code, replay.Body.String())
			}
			if a, b := tc.idOf(t, first.Body.Bytes()), tc.idOf(t, replay.Body.Bytes()); a == "" || a != b {
				t.Fatalf("replay answered %q, want the original %q", b, a)
			}
			if rr := doReqKeyed(r, http.MethodPost, tc.path, tc.changed, "preparer-1", testTenantID, "key-1"); rr.Code != http.StatusConflict {
				t.Fatalf("same key, different body: want 409, got %d %s", rr.Code, rr.Body.String())
			}
			if len(s.claims) != 1 {
				t.Fatalf("want exactly one claim, got %d", len(s.claims))
			}
		})
	}
}

func mustDecode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

// ── B2. One reversal per FX revaluation run ──────────────────────────────────

func TestReversePriorRevaluation_SecondCallReturnsTheExistingReversal(t *testing.T) {
	s := newStubStore()
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{RunID: "run-1", LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		Status: domain.FXRevaluationStatusPosted, CreatedByPrincipalID: "preparer-1",
		Items: []domain.FXRevaluationItem{{ItemID: "i-1", AccountCode: "1100-EURCash", AccountType: domain.AccountTypeAsset, AdjustmentAmount: 20}}}
	r := newRouter(s, &stubAuthZ{}, &stubClients{})

	first := doReq(r, http.MethodPost, "/v1/fx-revaluations/reverse", domain.ReversePriorRevaluationRequest{PriorRunID: "run-1"}, "preparer-2")
	if first.Code != http.StatusCreated {
		t.Fatalf("first reversal: want 201, got %d %s", first.Code, first.Body.String())
	}
	second := doReq(r, http.MethodPost, "/v1/fx-revaluations/reverse", domain.ReversePriorRevaluationRequest{PriorRunID: "run-1"}, "preparer-3")
	if second.Code != http.StatusOK {
		t.Fatalf("second reversal: want 200 (the existing one), got %d %s", second.Code, second.Body.String())
	}
	var a, b domain.FXRevaluationRun
	mustDecode(t, first.Body.Bytes(), &a)
	mustDecode(t, second.Body.Bytes(), &b)
	if a.RunID != b.RunID || len(s.fxRuns) != 2 {
		t.Fatalf("want one reversal run; got %s then %s, %d runs in total", a.RunID, b.RunID, len(s.fxRuns))
	}
}

// ── ACC-17 source-to-target reconciliation ───────────────────────────────────

func approvedMigrationBatch(t *testing.T, s *stubStore, cl *stubClients, r chi.Router) string {
	t.Helper()
	batchID := createValidatedMigrationBatch(t, s, cl, r)
	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/approve", nil, "approver-1"); rr.Code != http.StatusOK {
		t.Fatalf("approve failed: %d %s", rr.Code, rr.Body.String())
	}
	return batchID
}

func TestCommitOpeningPosting_ReconcilesTheLedgerJournalAgainstTheCrosswalk(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	batchID := approvedMigrationBatch(t, s, cl, r)

	rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("commit: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if cl.getJournalCalls != 1 {
		t.Fatalf("reconciliation must read the posted journal back from GL; read it %d times", cl.getJournalCalls)
	}
	if got := s.migrationBatches[batchID].Status; got != domain.MigrationBatchStatusReconciled {
		t.Fatalf("matching journal: want RECONCILED, got %s", got)
	}
}

func TestCommitOpeningPosting_LedgerDisagrees_QuarantinesAndCannotBeCertified(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	batchID := approvedMigrationBatch(t, s, cl, r)

	// The ledger holds a journal whose equity side was booked short: the
	// kind of drift a status flag alone would have certified.
	cl.postedJournal = &domain.PostedJournal{JournalID: "journal-fx-001", Status: "POSTED", LegalEntityID: "le-1", FiscalPeriod: "2026-01",
		Lines: []domain.JournalLineInput{
			{AccountCode: "1000-Cash", DebitAmount: 10000},
			{AccountCode: "3000-OpeningEquity", CreditAmount: 9000},
			{AccountCode: "9999-Plug", CreditAmount: 1000},
		}}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched journal: want 422, got %d %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	mustDecode(t, rr.Body.Bytes(), &body)
	if body["error_code"] != "migration_reconciliation_failed" {
		t.Fatalf("want migration_reconciliation_failed, got %v", body)
	}
	b := s.migrationBatches[batchID]
	if b.Status != domain.MigrationBatchStatusQuarantined || b.QuarantineReason == nil {
		t.Fatalf("want QUARANTINED with a reason, got %s / %v", b.Status, b.QuarantineReason)
	}
	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/certify",
		domain.CertifyMigrationBatchRequest{Reason: "signed"}, "certifier-1"); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("certifying an unreconciled batch: want 422, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestCommitOpeningPosting_ReadBackUnavailable_StaysPostedAndRetryReconcilesWithoutReposting(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{getJournalErr: domain.ErrGLServiceUnavailable}
	r := newRouter(s, &stubAuthZ{}, cl)
	batchID := approvedMigrationBatch(t, s, cl, r)

	rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("read-back failure: want 503, got %d %s", rr.Code, rr.Body.String())
	}
	if got := s.migrationBatches[batchID].Status; got != domain.MigrationBatchStatusPosted {
		t.Fatalf("an unproven batch must stay POSTED, got %s", got)
	}

	cl.getJournalErr = nil
	retry := doReq(r, http.MethodPost, "/v1/migration-batches/"+batchID+"/commit", nil, "preparer-1")
	if retry.Code != http.StatusOK || s.migrationBatches[batchID].Status != domain.MigrationBatchStatusReconciled {
		t.Fatalf("retry: want 200 RECONCILED, got %d %s / %s", retry.Code, retry.Body.String(), s.migrationBatches[batchID].Status)
	}
	if cl.postMultiLineCalls != 1 {
		t.Fatalf("the retry must reconcile, not post again: %d journal posts", cl.postMultiLineCalls)
	}
}

// ── One chart read per command ───────────────────────────────────────────────

func TestValidateOpeningBalances_ReadsTheChartOnceForTheWholeBatch(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	createValidatedMigrationBatch(t, s, cl, r) // two entries, two accounts
	if cl.chartCalls != 1 {
		t.Fatalf("want one chart listing for the batch, got %d", cl.chartCalls)
	}
}

func TestValidateOpeningBalances_ChartUnavailable_Returns503WithoutQuarantine(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubAuthZ{}, cl)
	req := domain.CreateMigrationBatchRequest{
		LegalEntityID: "le-1", FiscalPeriod: "2026-01", SourceSystemName: "LegacyERP", SourceExtractHash: "sha256:abc",
		ExpectedRowCount: 2, ExpectedTotalDebits: 10000, ExpectedTotalCredits: 10000, Entries: balancedMigrationEntries(),
	}
	rr := doReq(r, http.MethodPost, "/v1/migration-batches/", req, "preparer-1")
	var batch domain.MigrationBatch
	mustDecode(t, rr.Body.Bytes(), &batch)

	cl.chartOfAccountsErr = domain.ErrGLServiceUnavailable
	if rr := doReq(r, http.MethodPost, "/v1/migration-batches/"+batch.BatchID+"/validate", nil, "preparer-1"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d %s", rr.Code, rr.Body.String())
	}
	if got := s.migrationBatches[batch.BatchID].Status; got != domain.MigrationBatchStatusLoaded {
		t.Fatalf("an outage is not a validation failure: want LOADED, got %s", got)
	}
}

// ── ACC-18 ───────────────────────────────────────────────────────────────────

func TestTraceJournalToSource_RequiresViewOnEveryEntityInTheTrace(t *testing.T) {
	s := newStubStore()
	s.lineageEdges = []domain.LineageEdge{
		{LegalEntityID: "le-1", FromType: "allocation_run", FromID: "run-1", ToType: "journal", ToID: "journal-1"},
		{LegalEntityID: "le-2", FromType: "allocation_run", FromID: "run-2", ToType: "journal", ToID: "journal-1"},
	}
	authz := &recordingAuthZ{onCheck: func(_, legalEntityID, _ string) error {
		if legalEntityID == "le-2" {
			return domain.ErrAuthorizationDenied
		}
		return nil
	}}
	r := newRouterWithAuthZ(s, authz, &stubClients{})

	for _, path := range []string{
		"/v1/lineage/journals/journal-1/source",
		"/v1/lineage/journals/journal-1/source/as-of?as_of=2099-01-01T00:00:00Z",
	} {
		if rr := doReq(r, http.MethodGet, path, nil, "auditor-1"); rr.Code != http.StatusForbidden {
			t.Fatalf("%s: le-2's edge was served to a caller authorized only for le-1 (%d %s)", path, rr.Code, rr.Body.String())
		}
	}
}

func TestRebuildLineageProjection_EdgeWriteFails_StaysDegraded(t *testing.T) {
	s := newStubStore()
	s.postedJournalRefs = []domain.PostedJournalRef{{FromType: "fx_revaluation_run", FromID: "run-1", JournalID: "journal-1"}}
	s.recordEdgeErr = errors.New("lineage_edges unavailable")
	r := newRouter(s, &stubAuthZ{}, &stubClients{})

	rr := doReq(r, http.MethodPost, "/v1/lineage/rebuild?legal_entity_id=le-1", nil, "preparer-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var status domain.LineageProjectionStatus
	mustDecode(t, rr.Body.Bytes(), &status)
	if status.Status != domain.LineageProjectionDegraded {
		t.Fatalf("a rebuild that could not record the missing edge reported %s", status.Status)
	}
}
