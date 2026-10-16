package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/ledger"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/store"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubAccStore struct {
	ref    *domain.PayrollRunRef
	result *domain.PayrollResult
	input  domain.ResultPostingInput
	bridge *domain.AccountingBridge
	all    []domain.AccountingBridge

	created     *domain.AccountingBridge
	createdNew  bool
	createCalls int
	built       []domain.OutboxEvent

	submitCalls, reverseCalls, withdrawCalls int
	reverseReason                            string
	storeErr                                 error
	recorded                                 string
	recordedDetail                           any
}

func (s *stubAccStore) GetPayrollRunRef(context.Context, string) (*domain.PayrollRunRef, error) {
	if s.ref == nil {
		return nil, domain.ErrRunRefNotFound
	}
	return s.ref, nil
}

func (s *stubAccStore) GetPayrollResultByRef(context.Context, string) (*domain.PayrollResult, error) {
	if s.result == nil {
		return nil, domain.ErrResultNotFound
	}
	return s.result, nil
}

func (s *stubAccStore) ResultPostingInputFor(context.Context, string) (domain.ResultPostingInput, *domain.PayrollResult, *domain.PayrollRunRef, error) {
	if s.ref == nil || s.result == nil {
		return domain.ResultPostingInput{}, nil, nil, domain.ErrRunRefNotFound
	}
	return s.input, s.result, s.ref, nil
}

func (s *stubAccStore) CreateAccountingBridge(_ context.Context, b *domain.AccountingBridge, mk store.AccountingEventBuilder) (*domain.AccountingBridge, bool, error) {
	s.createCalls++
	s.created = b
	if s.storeErr != nil {
		return nil, false, s.storeErr
	}
	b.State, b.Version = domain.BridgeEventCreated, 1
	if mk != nil {
		evs, err := mk(store.AccountingChange{Kind: store.AccChangeCreated, Bridge: b})
		if err != nil {
			return nil, false, err
		}
		s.built = evs
	}
	return b, s.createdNew, nil
}

func (s *stubAccStore) GetAccountingBridge(context.Context, string) (*domain.AccountingBridge, error) {
	if s.bridge == nil {
		return nil, domain.ErrBridgeNotFound
	}
	return s.bridge, nil
}

func (s *stubAccStore) ListAccountingBridges(context.Context, string) ([]domain.AccountingBridge, error) {
	return s.all, nil
}

func (s *stubAccStore) SubmitAccountingBridge(_ context.Context, _ string, _ int, actor string, mk store.AccountingEventBuilder) (*domain.AccountingBridge, error) {
	s.submitCalls++
	if s.storeErr != nil {
		return nil, s.storeErr
	}
	b := *s.bridge
	b.State, b.SubmittedBy = domain.BridgeSubmitted, &actor
	if mk != nil {
		evs, err := mk(store.AccountingChange{Kind: store.AccChangeSubmitted, Bridge: &b})
		if err != nil {
			return nil, err
		}
		s.built = evs
	}
	return &b, nil
}

func (s *stubAccStore) CreateReversalBridge(_ context.Context, id, reason, creator, _ string, mk store.AccountingEventBuilder) (*domain.AccountingBridge, bool, error) {
	s.reverseCalls++
	s.reverseReason = reason
	if s.storeErr != nil {
		return nil, false, s.storeErr
	}
	orig := *s.bridge
	rev := &domain.AccountingBridge{BridgeID: uuid.NewString(), PostingPurpose: domain.PurposeReversal, OriginalBridgeID: &id, ResultID: orig.ResultID,
		TenantID: orig.TenantID, LegalEntityID: orig.LegalEntityID, State: domain.BridgeEventCreated, EvidenceFlags: []string{}, CreatedBy: creator}
	if mk != nil {
		evs, err := mk(store.AccountingChange{Kind: store.AccChangeCreated, Bridge: rev, Original: &orig, Reason: reason})
		if err != nil {
			return nil, false, err
		}
		s.built = evs
	}
	return rev, s.createdNew, nil
}

func (s *stubAccStore) WithdrawAccountingBridge(_ context.Context, _ string, _ int, reason string, mk store.AccountingEventBuilder) (*domain.AccountingBridge, error) {
	s.withdrawCalls++
	if s.storeErr != nil {
		return nil, s.storeErr
	}
	b := *s.bridge
	b.State = domain.BridgeCorrected
	kind := domain.CorrectionWithdrawn
	b.CorrectionKind = &kind
	return &b, nil
}

func (s *stubAccStore) RecordReconciliation(_ context.Context, _ string, state string, detail any) (*domain.AccountingBridge, error) {
	s.recorded, s.recordedDetail = state, detail
	b := *s.bridge
	b.ReconciliationState = state
	return &b, nil
}

type stubLedgerReader struct {
	journal *ledger.Journal
	found   bool
	err     error
	calls   int
	tenant  string
}

func (l *stubLedgerReader) GetJournal(_ context.Context, tenant, _, _, _ string) (*ledger.Journal, bool, error) {
	l.calls++
	l.tenant = tenant
	return l.journal, l.found, l.err
}

// ── harness ──────────────────────────────────────────────────────────────────

const accBase = "/v1/payroll/wfp/accounting/events"

func newAccRouter(st *stubAccStore, az handler.AuthZClient, lr *stubLedgerReader) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(rw, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPAccountingRoutes(r, handler.NewWFPAccounting(st, az, lr, "", zap.NewNop()))
	return r
}

func accDo(r chi.Router, method, path string, body any, principal string, headers ...string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func accCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var b map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &b))
	s, _ := b["error_code"].(string)
	return s
}

func acceptedFixture() *stubAccStore {
	d := func(s string) *big.Rat { r, _ := domain.ParseDecimal(s); return r }
	return &stubAccStore{
		ref: &domain.PayrollRunRef{
			RunRefID: uuid.NewString(), LegalEntityID: "le-OF-REF", SourceSystemID: "zp", SourceRunID: "RUN-7", SourceRunVersion: 2,
			PayrollPeriodID: "2026-09", PayDate: "2026-09-30", AcceptanceState: domain.AcceptanceAccepted,
		},
		result: &domain.PayrollResult{ResultID: uuid.NewString(), Outcome: domain.ResultAccepted, Currency: "GBP", ReceivedBy: "recv-1",
			EvidenceFlags: []string{"PLACEHOLDER_TAX_RATE", "EMPLOYER_LIABILITIES_NOT_COMPUTED"}},
		input: domain.ResultPostingInput{
			Gross: d("1000"), Net: d("800"),
			ByKind: map[string]*big.Rat{domain.KindEarning: d("1000"), domain.KindEmployeeTax: d("200")},
		},
		createdNew: true,
	}
}

func postedBridge(runRef string) *domain.AccountingBridge {
	j, e := "jrn-1", "exec-1"
	return &domain.AccountingBridge{
		BridgeID: uuid.NewString(), TenantID: "tenant-abc", RunRefID: runRef, ResultID: "res-1", LegalEntityID: "le-OF-BRIDGE",
		PostingPurpose: domain.PurposeResult, State: domain.BridgePosted, Version: 4, JournalID: &j, ExecutionID: &e,
		FiscalPeriod: "2026-09", TransactionCurrency: "GBP", LineCount: 3, TotalDebit: "1000.0000", TotalCredit: "1000.0000",
		SourceEventID: "wfp-payroll:res-1:PRIMARY:PAYROLL_RESULT", ReconciliationState: domain.ReconNotReconciled, EvidenceFlags: []string{},
	}
}

// ── identity & authorization ─────────────────────────────────────────────────

func TestWFP05_IdentityAuthorizationAndForgedTenant(t *testing.T) {
	st := acceptedFixture()
	r := newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{})
	body := map[string]any{"run_ref_id": st.ref.RunRefID}
	assert.Equal(t, http.StatusUnauthorized, accDo(r, "POST", accBase, body, "").Code)

	denied := newAccRouter(st, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubLedgerReader{})
	assert.Equal(t, http.StatusForbidden, accDo(denied, "POST", accBase, body, "p-1").Code)
	down := newAccRouter(st, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}, &stubLedgerReader{})
	assert.Equal(t, http.StatusServiceUnavailable, accDo(down, "POST", accBase, body, "p-1").Code, "fail closed")
	assert.Zero(t, st.createCalls)

	forged := map[string]any{"run_ref_id": st.ref.RunRefID, "tenant_id": "someone-elses"}
	rr := accDo(r, "POST", accBase, forged, "p-1")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_json", accCode(t, rr))
}

func TestWFP05_ActionsAreAuthorizedAgainstTheStoredEntityNotACallerHeader(t *testing.T) {
	st := acceptedFixture()
	st.bridge = postedBridge(st.ref.RunRefID)
	az := &entityAuthZ{}
	r := newAccRouter(st, az, &stubLedgerReader{})

	require.Equal(t, http.StatusCreated, accDo(r, "POST", accBase, map[string]any{"run_ref_id": st.ref.RunRefID}, "p-1", "X-Legal-Entity-Id", "le-CALLER-CHOSE").Code)
	require.Equal(t, http.StatusOK, accDo(r, "GET", accBase+"/"+st.bridge.BridgeID, nil, "p-1", "X-Legal-Entity-Id", "le-CALLER-CHOSE").Code)
	assert.Equal(t, []string{"le-OF-REF", "le-OF-BRIDGE"}, az.entities, "scope comes from the stored records")
	assert.Equal(t, []string{"WFP_ACCOUNTING_CREATE", "WFP_ACCOUNTING_VIEW"}, az.actions)
}

// ── create ───────────────────────────────────────────────────────────────────

func TestWFP05_CreateBuildsTheFrozenBalancedProposalFromTheAcceptedResult(t *testing.T) {
	st := acceptedFixture()
	lr := &stubLedgerReader{}
	r := newAccRouter(st, &stubAuthZ{}, lr)

	rr := accDo(r, "POST", accBase, map[string]any{"run_ref_id": st.ref.RunRefID}, "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	b := st.created
	require.NotNil(t, b)

	assert.Equal(t, domain.PurposeResult, b.PostingPurpose)
	assert.Equal(t, "PRIMARY", b.AccountingBookID, "the single default book")
	assert.Equal(t, domain.SourceEventID(st.result.ResultID, "PRIMARY", domain.PurposeResult), b.SourceEventID)
	assert.Equal(t, "2026-09", b.FiscalPeriod, "the ledger period comes from the pay date")
	assert.Equal(t, "2026-09-30", b.DocumentDate)
	assert.Equal(t, "GBP", b.TransactionCurrency)
	assert.Equal(t, "p-1", b.CreatedBy)
	assert.Equal(t, "tenant-abc", b.TenantID, "the subject is stamped with the AUTHENTICATED tenant")
	assert.Equal(t, 3, b.LineCount)
	assert.Equal(t, "1000.0000", b.TotalDebit)
	assert.Equal(t, b.TotalDebit, b.TotalCredit)
	assert.Equal(t, st.result.EvidenceFlags, b.EvidenceFlags)
	assert.Contains(t, b.Description, "RUN-7 v2")
	assert.Contains(t, b.Description, "PLACEHOLDER_TAX_RATE", "the evidence flags are visible to the controller on the journal itself")
	for _, l := range b.Lines {
		assert.Contains(t, domain.AllPayrollMappingKeys(), l.MappingKey)
	}
	assert.Zero(t, lr.calls, "creating a subject never touches the ledger")
}

func TestWFP05_CreateRefusesWhatCannotBePosted(t *testing.T) {
	cases := map[string]struct {
		mutate func(*stubAccStore)
		status int
		code   string
	}{
		"unknown run reference": {func(s *stubAccStore) { s.ref = nil }, http.StatusNotFound, "not_found"},
		"run is superseded":     {func(s *stubAccStore) { s.ref.AcceptanceState = domain.AcceptanceSuperseded }, http.StatusConflict, "RESULT_NOT_POSTABLE"},
		"run is rejected":       {func(s *stubAccStore) { s.ref.AcceptanceState = domain.AcceptanceRejected }, http.StatusConflict, "RESULT_NOT_POSTABLE"},
		"result is rejected":    {func(s *stubAccStore) { s.result.Outcome = domain.ResultRejected }, http.StatusConflict, "RESULT_NOT_POSTABLE"},
		"nothing to post": {func(s *stubAccStore) {
			z, _ := domain.ParseDecimal("0")
			s.input = domain.ResultPostingInput{Gross: z, Net: z, ByKind: map[string]*big.Rat{}}
		}, http.StatusUnprocessableEntity, "NOTHING_TO_POST"},
		"result that does not reconcile": {func(s *stubAccStore) {
			n, _ := domain.ParseDecimal("799")
			s.input.Net = n
		}, http.StatusUnprocessableEntity, "UNBALANCED_POSTING"},
	}
	for name, c := range cases {
		st := acceptedFixture()
		id := st.ref.RunRefID
		c.mutate(st)
		rr := accDo(newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{}), "POST", accBase, map[string]any{"run_ref_id": id}, "p-1")
		assert.Equal(t, c.status, rr.Code, name)
		assert.Equal(t, c.code, accCode(t, rr), name)
		assert.Zero(t, st.createCalls, "%s: nothing is stored for a result that cannot be posted", name)
	}
}

func TestWFP05_CreateValidatesTheBookAndReportsReplays(t *testing.T) {
	st := acceptedFixture()
	r := newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{})
	id := st.ref.RunRefID

	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", accBase, map[string]any{"run_ref_id": "nope"}, "p-1").Code)
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", accBase, map[string]any{"run_ref_id": id, "accounting_book_id": "bad book!"}, "p-1").Code)

	require.Equal(t, http.StatusCreated, accDo(r, "POST", accBase, map[string]any{"run_ref_id": id, "accounting_book_id": "STATUTORY"}, "p-1").Code)
	assert.Equal(t, "STATUTORY", st.created.AccountingBookID)
	assert.Contains(t, st.created.SourceEventID, ":STATUTORY:", "a different book is a different subject")
}

// ── submit / requeue ─────────────────────────────────────────────────────────

func TestWFP05_SubmitEnforcesSeparationOfDutiesBeforeTheStore(t *testing.T) {
	st := acceptedFixture()
	st.bridge = postedBridge(st.ref.RunRefID)
	st.bridge.State = domain.BridgeEventCreated
	st.bridge.RunRefID = st.ref.RunRefID
	r := newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{})
	path := accBase + "/" + st.bridge.BridgeID + "/submit"

	// The principal who received the result cannot submit its posting.
	rr := accDo(r, "POST", path, map[string]any{"expected_version": 4}, "recv-1")
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Equal(t, "SOD_SAME_PRINCIPAL", accCode(t, rr))
	assert.Zero(t, st.submitCalls)

	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", path, map[string]any{}, "p-2").Code, "a version is required")
	rr = accDo(r, "POST", path, map[string]any{"expected_version": 4}, "p-2")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, 1, st.submitCalls)
	require.Len(t, st.built, 1)
	assert.Equal(t, domain.EventPayrollAccountingSubmitted, st.built[0].EventType)

	// If-Match is honoured.
	assert.Equal(t, http.StatusOK, accDo(r, "POST", path, map[string]any{}, "p-2", "If-Match", `"4"`).Code)

	// Only an EVENT_CREATED subject can be submitted.
	st.bridge.State = domain.BridgePosted
	rr = accDo(r, "POST", path, map[string]any{"expected_version": 4}, "p-2")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "INVALID_STATE", accCode(t, rr))

	// The database's own refusals surface with the same precision.
	st.bridge.State = domain.BridgeEventCreated
	st.storeErr = domain.ErrSoDSamePrincipal
	assert.Equal(t, http.StatusForbidden, accDo(r, "POST", path, map[string]any{"expected_version": 4}, "p-2").Code)
	st.storeErr = domain.ErrResultNotPostable
	rr = accDo(r, "POST", path, map[string]any{"expected_version": 4}, "p-2")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "RESULT_NOT_POSTABLE", accCode(t, rr))
	st.storeErr = domain.ErrBridgeVersionConflict
	assert.Equal(t, "VERSION_CONFLICT", accCode(t, accDo(r, "POST", path, map[string]any{"expected_version": 1}, "p-2")))
}

func TestWFP05_RequeueOnlyForARejectedSubjectAndNeverBySameReceiver(t *testing.T) {
	st := acceptedFixture()
	st.bridge = postedBridge(st.ref.RunRefID)
	r := newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{})
	path := accBase + "/" + st.bridge.BridgeID + "/requeue"

	st.bridge.State = domain.BridgePosted
	assert.Equal(t, http.StatusConflict, accDo(r, "POST", path, map[string]any{"expected_version": 4}, "p-2").Code)

	st.bridge.State = domain.BridgeRejected
	assert.Equal(t, http.StatusForbidden, accDo(r, "POST", path, map[string]any{"expected_version": 4}, "recv-1").Code)
	assert.Equal(t, http.StatusOK, accDo(r, "POST", path, map[string]any{"expected_version": 4}, "p-2").Code)
}

// ── reverse / withdraw ───────────────────────────────────────────────────────

func TestWFP05_ReverseNeedsAPostedSubjectWhoseResultWasSuperseded(t *testing.T) {
	st := acceptedFixture()
	st.bridge = postedBridge(st.ref.RunRefID)
	r := newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{})
	path := accBase + "/" + st.bridge.BridgeID + "/reverse"

	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", path, map[string]any{}, "p-5").Code, "a reason is required")

	// The result is still ACCEPTED: nothing to reverse.
	rr := accDo(r, "POST", path, map[string]any{"reason": "x"}, "p-5")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "INVALID_STATE", accCode(t, rr))
	assert.Zero(t, st.reverseCalls)

	st.ref.AcceptanceState = domain.AcceptanceSuperseded
	rr = accDo(r, "POST", path, map[string]any{"reason": "retro correction"}, "p-5")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	assert.Equal(t, "retro correction", st.reverseReason)
	require.Len(t, st.built, 1)
	assert.Equal(t, domain.EventPayrollAccountingEventCreated, st.built[0].EventType)

	st.createdNew = false
	assert.Equal(t, http.StatusOK, accDo(r, "POST", path, map[string]any{"reason": "retro correction"}, "p-5").Code, "one reversal per original: a repeat is a replay")

	// A subject that has not posted has no journal to reverse.
	st.bridge.State = domain.BridgeSubmitted
	assert.Equal(t, http.StatusConflict, accDo(r, "POST", path, map[string]any{"reason": "x"}, "p-5").Code)
	// A reversal subject is not itself reversible.
	st.bridge.State, st.bridge.PostingPurpose = domain.BridgePosted, domain.PurposeReversal
	assert.Equal(t, http.StatusConflict, accDo(r, "POST", path, map[string]any{"reason": "x"}, "p-5").Code)
}

func TestWFP05_WithdrawNeedsASupersededResult(t *testing.T) {
	st := acceptedFixture()
	st.bridge = postedBridge(st.ref.RunRefID)
	st.bridge.State = domain.BridgeEventCreated
	r := newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{})
	path := accBase + "/" + st.bridge.BridgeID + "/withdraw"

	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", path, map[string]any{"expected_version": 4}, "p-5").Code, "a reason is required")
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", path, map[string]any{"reason": "x"}, "p-5").Code, "a version is required")
	assert.Equal(t, http.StatusConflict, accDo(r, "POST", path, map[string]any{"expected_version": 4, "reason": "x"}, "p-5").Code, "the result is still ACCEPTED")
	assert.Zero(t, st.withdrawCalls)

	st.ref.AcceptanceState = domain.AcceptanceSuperseded
	rr := accDo(r, "POST", path, map[string]any{"expected_version": 4, "reason": "superseded by v3"}, "p-5")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "WITHDRAWN")
}

// ── reconcile ────────────────────────────────────────────────────────────────

func journal(status string, debit, credit float64, lines int) *ledger.Journal {
	j := &ledger.Journal{JournalID: "jrn-1", Status: status, LegalEntityID: "le-OF-BRIDGE", FiscalPeriod: "2026-09", CurrencyCode: "GBP"}
	for i := 0; i < lines; i++ {
		d, c := 0.0, 0.0
		if i == 0 {
			d = debit
		}
		if i == 1 {
			c = credit
		}
		j.Lines = append(j.Lines, struct {
			DebitAmount  float64 `json:"debit_amount"`
			CreditAmount float64 `json:"credit_amount"`
		}{d, c})
	}
	return j
}

func TestWFP05_ReconcileComparesExactlyAndRecordsTheOutcome(t *testing.T) {
	st := acceptedFixture()
	st.bridge = postedBridge(st.ref.RunRefID)
	lr := &stubLedgerReader{journal: journal("FINALIZED", 1000, 1000, 3), found: true}
	r := newAccRouter(st, &stubAuthZ{}, lr)
	path := accBase + "/" + st.bridge.BridgeID + "/reconcile"

	rr := accDo(r, "POST", path, nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, domain.ReconMatched, st.recorded)
	assert.Equal(t, "tenant-abc", lr.tenant, "the ledger is asked for the authenticated tenant")

	for name, c := range map[string]struct {
		j     *ledger.Journal
		found bool
		want  string
	}{
		"debits differ by a cent": {journal("FINALIZED", 1000.01, 1000, 3), true, "debits"},
		"credits differ":          {journal("FINALIZED", 1000, 999, 3), true, "credits"},
		"not finalized":           {journal("DRAFT", 1000, 1000, 3), true, "FINALIZED"},
		"wrong line count":        {journal("FINALIZED", 1000, 1000, 2), true, "lines"},
		"journal missing":         {nil, false, "no journal"},
	} {
		lr.journal, lr.found = c.j, c.found
		rr := accDo(r, "POST", path, nil, "p-1")
		require.Equal(t, http.StatusOK, rr.Code, name)
		assert.Equal(t, domain.ReconMismatch, st.recorded, name)
		assert.Contains(t, rr.Body.String(), c.want, name)
	}

	other := journal("FINALIZED", 1000, 1000, 3)
	other.CurrencyCode = "USD"
	other.FiscalPeriod = "2026-10"
	other.LegalEntityID = "le-X"
	lr.journal, lr.found = other, true
	rr = accDo(r, "POST", path, nil, "p-1")
	assert.Contains(t, rr.Body.String(), "currency")
	assert.Contains(t, rr.Body.String(), "period")
	assert.Contains(t, rr.Body.String(), "legal entity")
}

func TestWFP05_ReconcileNeverReportsAMatchItCouldNotCheck(t *testing.T) {
	st := acceptedFixture()
	st.bridge = postedBridge(st.ref.RunRefID)
	lr := &stubLedgerReader{err: domain.ErrLedgerUnavailable}
	r := newAccRouter(st, &stubAuthZ{}, lr)
	path := accBase + "/" + st.bridge.BridgeID + "/reconcile"

	rr := accDo(r, "POST", path, nil, "p-1")
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "ledger_unavailable", accCode(t, rr))
	assert.Empty(t, st.recorded, "an unreachable ledger is unknown, not a match and not a mismatch")

	st.bridge.JournalID = nil
	st.bridge.State = domain.BridgeSubmitted
	rr = accDo(r, "POST", path, nil, "p-1")
	assert.Equal(t, http.StatusConflict, rr.Code, "nothing has posted, so there is nothing to reconcile")
	assert.Equal(t, 1, lr.calls, "and the ledger was not asked")
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestWFP05_Queries(t *testing.T) {
	st := acceptedFixture()
	st.bridge = postedBridge(st.ref.RunRefID)
	st.all = []domain.AccountingBridge{*st.bridge}
	r := newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{})
	id := st.bridge.BridgeID

	rr := accDo(r, "GET", accBase+"/"+id+"/posting-status", nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"waiting_for_requeue":false`)
	st.bridge.State, st.bridge.Attempts = domain.BridgeSubmitted, domain.MaxPostingAttempts
	rr = accDo(r, "GET", accBase+"/"+id+"/posting-status", nil, "p-1")
	assert.Contains(t, rr.Body.String(), `"waiting_for_requeue":true`, "an exhausted subject is visible, not silently stuck")

	rr = accDo(r, "GET", accBase+"/"+id+"/journal-references", nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "jrn-1")
	assert.Contains(t, rr.Body.String(), "exec-1")

	rr = accDo(r, "GET", accBase+"/"+id+"/lineage", nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	for _, part := range []string{`"run_ref"`, `"result"`, `"subjects"`, "RUN-7", "jrn-1"} {
		assert.Contains(t, rr.Body.String(), part)
	}

	assert.Equal(t, http.StatusBadRequest, accDo(r, "GET", accBase, nil, "p-1").Code, "list needs a run_ref_id")
	assert.Equal(t, http.StatusOK, accDo(r, "GET", accBase+"?run_ref_id="+st.ref.RunRefID, nil, "p-1").Code)

	assert.Equal(t, http.StatusBadRequest, accDo(r, "GET", accBase+"/mapping-keys", nil, "p-1").Code, "an entity scope is required")
	rr = accDo(r, "GET", accBase+"/mapping-keys", nil, "p-1", "X-Legal-Entity-Id", "le-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "PAYROLL_NET_PAY_PAYABLE")
	assert.Contains(t, rr.Body.String(), "PAYROLL_EMPLOYER_SOCIAL_EXPENSE")

	st.bridge = nil
	assert.Equal(t, http.StatusNotFound, accDo(r, "GET", accBase+"/"+id, nil, "p-1").Code)
}

func TestWFP05_EventsCarryNoAmountsOrLineDetail(t *testing.T) {
	st := acceptedFixture()
	r := newAccRouter(st, &stubAuthZ{}, &stubLedgerReader{})
	require.Equal(t, http.StatusCreated, accDo(r, "POST", accBase, map[string]any{"run_ref_id": st.ref.RunRefID}, "p-1").Code)

	require.Len(t, st.built, 1)
	ev := st.built[0]
	assert.Equal(t, domain.EventPayrollAccountingEventCreated, ev.EventType)
	assert.Equal(t, "tenant-abc", ev.TenantID)
	assert.Equal(t, st.result.ResultID, ev.ObjectID, "keyed by result so everything about one result's accounting stays in order")
	for _, secret := range []string{"1000.0000", "800.0000", "200.0000", "debit", "credit", "PAYROLL_GROSS_EXPENSE"} {
		assert.NotContains(t, string(ev.Payload), secret, "no amount or posting line travels on the bus")
	}
	assert.Contains(t, string(ev.Payload), "PLACEHOLDER_TAX_RATE", "the evidence flags do travel: they are not amounts")
}
