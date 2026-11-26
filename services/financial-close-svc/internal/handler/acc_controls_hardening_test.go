package handler_test

// ZS-SVC-B-001 first survey (SPEC_DEVIATIONS.md B1-B8): maker-checker on
// ACC-07/08/09/10, command idempotency on the four aggregate creates, one
// reversal per FX run, and ACC-18's permission and rebuild corrections.
// ACC-17's migration-batch maker-checker, source-to-target reconciliation and
// one-chart-read-per-command coverage were retired along with its code —
// see SPEC_DEVIATIONS.md A9.

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

func TestApprove_PreparerCannotApproveOwnItem_ACC08_09_10(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{accountStatuses: map[string]string{"6100-Sales": "ACTIVE"}}
	r := newRouter(s, &stubAuthZ{}, cl)
	s.prepaymentSchedules["ppy-1"] = &domain.PrepaymentSchedule{ScheduleID: "ppy-1", LegalEntityID: "le-1",
		Status: domain.PrepaymentStatusDraft, CreatedByPrincipalID: "preparer-1"}
	s.allocationRules["rule-1"] = &domain.AllocationRule{RuleID: "rule-1", RuleVersionID: "rule-1", LegalEntityID: "le-1",
		Status: domain.AllocationRuleStatusDraft, CreatedByPrincipalID: "preparer-1",
		Drivers: []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}}}
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{CurrencyCode: testCurrency, RunID: "run-1", LegalEntityID: "le-1",
		Status: domain.FXRevaluationStatusReview, CreatedByPrincipalID: "preparer-1"}

	for _, path := range []string{
		"/v1/prepayments/ppy-1/approve",
		"/v1/allocation-rules/rule-1/approve",
		"/v1/fx-revaluations/run-1/approve",
	} {
		requireSelfApprovalRefused(t, doReq(r, http.MethodPost, path, nil, "preparer-1"), path)
		if rr := doReq(r, http.MethodPost, path, nil, "approver-1"); rr.Code != http.StatusOK {
			t.Fatalf("%s by an independent approver: want 200, got %d %s", path, rr.Code, rr.Body.String())
		}
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
	accrual := domain.CreateAccrualRequest{CurrencyCode: testCurrency, LegalEntityID: "le-1", Description: "audit fee", PolicyVersion: "v1", TotalAmount: 900,
		StartFiscalPeriod: "2026-01", PeriodCount: 3, DebitAccountCode: "6100-AuditFee", CreditAccountCode: "2100-Accrued"}
	prepayment := domain.CreatePrepaymentRequest{CurrencyCode: testCurrency, LegalEntityID: "le-1", Description: "insurance", TotalAmount: 1200,
		StartFiscalPeriod: "2026-01", PeriodCount: 12, DebitAccountCode: "6200-Insurance", CreditAccountCode: "1400-Prepaid"}
	rule := domain.CreateAllocationRuleRequest{CurrencyCode: testCurrency, LegalEntityID: "le-1", Name: "IT", SourceAccountCode: "5000-IT",
		Drivers: []domain.AllocationDriver{{RecipientAccountCode: "6100-Sales", WeightPercentage: 100}}}
	revaluation := domain.StartRevaluationRequest{CurrencyCode: testCurrency, LegalEntityID: "le-1", FiscalPeriod: "2026-01", FXGainLossAccountCode: "7900-FX",
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
	s.fxRuns["run-1"] = &domain.FXRevaluationRun{CurrencyCode: testCurrency, RunID: "run-1", LegalEntityID: "le-1", FiscalPeriod: "2026-01",
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
