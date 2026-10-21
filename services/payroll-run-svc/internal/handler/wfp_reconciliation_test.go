package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
	"zoiko.io/payroll-run-svc/internal/paymentstatus"
	"zoiko.io/payroll-run-svc/internal/store"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubReconStore struct {
	ref       *domain.PayrollRunRef
	result    *domain.PayrollResult
	run       *domain.ReconRun
	exc       *domain.ReconException
	instr     *domain.PaymentInstruction
	bridges   []domain.AccountingBridge
	err       error
	execErr   error
	waiveErr  error
	linkErr   error
	returnErr error

	executed *struct {
		RunRef, Actor string
		Reperform     bool
	}
	certified string
	waived    *struct{ ID, Actor, Reason, Evidence string }
	assigned  string
	withdrawn []string
	linked    string
	returned  *struct {
		Version int
		Reason  string
	}
	recorded    *domain.BankingObservation
	superseded  []string
	gate        *domain.CloseGate
	gateArgs    [2]string
	observation []domain.BankingObservation
}

func (s *stubReconStore) GetPayrollRunRef(context.Context, string) (*domain.PayrollRunRef, error) {
	if s.ref == nil {
		return nil, domain.ErrRunRefNotFound
	}
	return s.ref, nil
}
func (s *stubReconStore) GetPayrollResultByRef(context.Context, string) (*domain.PayrollResult, error) {
	if s.result == nil {
		return nil, domain.ErrResultNotFound
	}
	return s.result, nil
}
func (s *stubReconStore) GetRuleManifest(context.Context, string) (*domain.PayrollRuleManifest, error) {
	return &domain.PayrollRuleManifest{ManifestID: "m-1", ManifestVersion: "v1", ManifestDigest: "d"}, nil
}
func (s *stubReconStore) CorrectionChains(context.Context, string) ([]domain.PayrollCorrectionChain, []domain.PayrollCorrectionChain, error) {
	return nil, nil, nil
}
func (s *stubReconStore) ExecuteReconciliation(_ context.Context, runRef, actor, _ string, reperform bool, _ store.ReconEventBuilder) (*store.ReconOutcome, error) {
	s.executed = &struct {
		RunRef, Actor string
		Reperform     bool
	}{runRef, actor, reperform}
	if s.execErr != nil {
		return nil, s.execErr
	}
	return &store.ReconOutcome{Run: s.run, Created: []domain.ReconException{}, Resolved: []domain.ReconException{}, Open: []domain.ReconException{}}, nil
}
func (s *stubReconStore) CertifyReconRun(_ context.Context, id, actor string, _ store.ReconEventBuilder) (*domain.ReconRun, error) {
	s.certified = actor
	if s.err != nil {
		return nil, s.err
	}
	return s.run, nil
}
func (s *stubReconStore) GetReconRun(context.Context, string) (*domain.ReconRun, error) {
	if s.run == nil {
		return nil, domain.ErrReconRunNotFound
	}
	return s.run, nil
}
func (s *stubReconStore) ListReconRuns(context.Context, string) ([]domain.ReconRun, error) {
	return []domain.ReconRun{}, s.err
}
func (s *stubReconStore) ListReconExceptions(context.Context, string, string, string) ([]domain.ReconException, error) {
	return []domain.ReconException{}, s.err
}
func (s *stubReconStore) GetReconException(context.Context, string) (*domain.ReconException, error) {
	if s.exc == nil {
		return nil, domain.ErrReconExceptionNotFound
	}
	return s.exc, nil
}
func (s *stubReconStore) AssignReconException(_ context.Context, _, owner string) (*domain.ReconException, error) {
	s.assigned = owner
	return s.exc, s.err
}
func (s *stubReconStore) WaiveReconException(_ context.Context, id, actor, reason, ev string, _ store.ReconEventBuilder) (*domain.ReconException, error) {
	s.waived = &struct{ ID, Actor, Reason, Evidence string }{id, actor, reason, ev}
	if s.waiveErr != nil {
		return nil, s.waiveErr
	}
	return s.exc, nil
}
func (s *stubReconStore) ResolveExceptionsForSupersededResult(context.Context, string, store.ReconEventBuilder) ([]domain.ReconException, error) {
	return []domain.ReconException{{}, {}}, s.err
}
func (s *stubReconStore) CloseGateFor(_ context.Context, entity, period string) (*domain.CloseGate, error) {
	s.gateArgs = [2]string{entity, period}
	if s.err != nil {
		return nil, s.err
	}
	return s.gate, nil
}
func (s *stubReconStore) CorrectionCompletenessFor(context.Context, string) (*domain.CorrectionCompleteness, error) {
	return &domain.CorrectionCompleteness{Complete: false, Effects: []domain.CorrectionEffect{{Effect: "ACCOUNTING", Pending: 1}}}, s.err
}
func (s *stubReconStore) ListAccountingBridges(context.Context, string) ([]domain.AccountingBridge, error) {
	return s.bridges, s.err
}
func (s *stubReconStore) WithdrawAccountingBridge(_ context.Context, id string, _ int, _ string, _ store.AccountingEventBuilder) (*domain.AccountingBridge, error) {
	s.withdrawn = append(s.withdrawn, id)
	return &domain.AccountingBridge{BridgeID: id}, s.err
}
func (s *stubReconStore) SupersedeLiabilitiesForRun(context.Context, string, store.LiabilityEventBuilder) (*store.SupersedeOutcome, error) {
	s.superseded = append(s.superseded, "liabilities")
	return &store.SupersedeOutcome{}, s.err
}
func (s *stubReconStore) SupersedeInstructionsForRun(context.Context, string, store.PaymentEventBuilder) (*store.InstructionSupersedeOutcome, error) {
	s.superseded = append(s.superseded, "instructions")
	return &store.InstructionSupersedeOutcome{}, s.err
}
func (s *stubReconStore) ListLiabilities(context.Context, store.LiabilityFilter) ([]domain.PayrollLiability, error) {
	return []domain.PayrollLiability{}, s.err
}
func (s *stubReconStore) ListPaymentInstructions(context.Context, store.InstructionFilter) ([]domain.PaymentInstruction, error) {
	return []domain.PaymentInstruction{}, s.err
}
func (s *stubReconStore) GetPaymentInstruction(context.Context, string) (*domain.PaymentInstruction, error) {
	if s.instr == nil {
		return nil, domain.ErrInstructionNotFound
	}
	return s.instr, nil
}
func (s *stubReconStore) ListBankingObservations(context.Context, string) ([]domain.BankingObservation, error) {
	return s.observation, s.err
}
func (s *stubReconStore) RecordBankingObservation(_ context.Context, _ string, o *domain.BankingObservation, _ string, _ store.ReconEventBuilder) (*domain.BankingObservation, bool, error) {
	s.recorded = o
	return o, true, s.err
}
func (s *stubReconStore) LinkInstructionPayment(_ context.Context, _, paymentID string) (*domain.PaymentInstruction, error) {
	s.linked = paymentID
	if s.linkErr != nil {
		return nil, s.linkErr
	}
	return s.instr, nil
}
func (s *stubReconStore) MarkInstructionReturned(_ context.Context, _ string, v int, reason string, _ store.ReconEventBuilder) (*domain.PaymentInstruction, error) {
	s.returned = &struct {
		Version int
		Reason  string
	}{v, reason}
	if s.returnErr != nil {
		return nil, s.returnErr
	}
	return s.instr, nil
}

type stubLookup struct {
	snap *paymentstatus.Snapshot
	err  error
}

func (l *stubLookup) Snapshot(context.Context, string, string, string) (*paymentstatus.Snapshot, error) {
	return l.snap, l.err
}

type stubObserver struct {
	obs   *domain.BankingObservation
	calls int
}

func (o *stubObserver) Observe(context.Context, domain.PaymentInstruction) *domain.BankingObservation {
	o.calls++
	return o.obs
}

const reconBase = "/v1/payroll/wfp/reconciliation"
const bankBase = "/v1/payroll/wfp/banking-outcomes"

func reconFixture() *stubReconStore {
	return &stubReconStore{
		ref:    &domain.PayrollRunRef{RunRefID: uuid.NewString(), LegalEntityID: "le-OF-REF", AcceptanceState: domain.AcceptanceAccepted, RuleManifestID: "m-1"},
		result: &domain.PayrollResult{ResultID: uuid.NewString(), Outcome: domain.ResultAccepted, ResultFingerprint: "fp", Currency: "GBP", TotalGross: "1.0000", TotalNetPay: "1.0000"},
		run:    &domain.ReconRun{ReconRunID: uuid.NewString(), LegalEntityID: "le-OF-RUN", Outcome: domain.ReconOutcomeMatched, Assertions: []domain.Assertion{}},
		exc:    &domain.ReconException{ExceptionID: uuid.NewString(), LegalEntityID: "le-OF-EXC", State: domain.ReconStateOpen, RaisedBy: "p-3"},
		instr:  &domain.PaymentInstruction{InstructionID: uuid.NewString(), LegalEntityID: "le-OF-INSTR", State: domain.PayHandedOff, Version: 4},
		gate:   &domain.CloseGate{Open: true, Blockers: []domain.GateBlocker{}, Warnings: []domain.GateBlocker{}},
	}
}

func newReconRouter(st *stubReconStore, az handler.AuthZClient, pay handler.PaymentLookup, obs handler.BankingObserver) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(rw, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPReconRoutes(r, handler.NewWFPRecon(st, az, pay, obs, zap.NewNop()))
	return r
}

func stdRecon(st *stubReconStore) chi.Router {
	return newReconRouter(st, &stubAuthZ{}, &stubLookup{}, &stubObserver{})
}

// ── identity and authorization ───────────────────────────────────────────────

func TestWave8_IdentityAuthorizationAndForgedInput(t *testing.T) {
	st := reconFixture()
	body := map[string]any{"run_ref_id": st.ref.RunRefID}
	assert.Equal(t, http.StatusUnauthorized, accDo(stdRecon(st), "POST", reconBase+"/runs", body, "").Code)
	denied := newReconRouter(st, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubLookup{}, &stubObserver{})
	assert.Equal(t, http.StatusForbidden, accDo(denied, "POST", reconBase+"/runs", body, "p-3").Code)
	down := newReconRouter(st, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}, &stubLookup{}, &stubObserver{})
	assert.Equal(t, http.StatusServiceUnavailable, accDo(down, "POST", reconBase+"/runs", body, "p-3").Code, "fail closed")
	assert.Nil(t, st.executed)

	rr := accDo(stdRecon(st), "POST", reconBase+"/runs", map[string]any{"run_ref_id": st.ref.RunRefID, "tenant_id": "x"}, "p-3")
	assert.Equal(t, http.StatusBadRequest, rr.Code, "a tenant in the body is refused")
	assert.Equal(t, http.StatusBadRequest, accDo(stdRecon(st), "POST", reconBase+"/runs", map[string]any{"run_ref_id": "nope"}, "p-3").Code)
}

func TestWave8_EveryActionIsAuthorizedAgainstTheStoredObjectsEntity(t *testing.T) {
	st := reconFixture()
	az := &entityAuthZ{}
	r := newReconRouter(st, az, &stubLookup{snap: &paymentstatus.Snapshot{PaymentID: "pay-1", LegalEntityID: "le-OF-INSTR", SourceReference: st.instr.InstructionID}}, &stubObserver{obs: &domain.BankingObservation{}})
	hdr := []string{"X-Legal-Entity-Id", "le-CALLER-CHOSE"}
	ok := func(method, path string, body any) {
		t.Helper()
		rr := accDo(r, method, path, body, "p-3", hdr...)
		require.Less(t, rr.Code, 300, "%s %s: %s", method, path, rr.Body.String())
	}
	ok("POST", reconBase+"/runs", map[string]any{"run_ref_id": st.ref.RunRefID})
	ok("GET", reconBase+"/runs/"+st.run.ReconRunID, nil)
	ok("POST", reconBase+"/runs/"+st.run.ReconRunID+"/certify", nil)
	ok("GET", reconBase+"/exceptions/"+st.exc.ExceptionID, nil)
	ok("POST", reconBase+"/exceptions/"+st.exc.ExceptionID+"/assign", map[string]any{"owner": "team"})
	ok("POST", reconBase+"/exceptions/"+st.exc.ExceptionID+"/waive", map[string]any{"reason": "r", "evidence_ref": "e"})
	ok("GET", reconBase+"/exceptions?run_ref_id="+st.ref.RunRefID, nil)
	gateEntity := uuid.NewString()
	ok("GET", reconBase+"/close-gate?legal_entity_id="+gateEntity+"&period_id=2026-09", nil)
	ok("GET", bankBase+"/"+st.instr.InstructionID, nil)
	ok("POST", bankBase+"/"+st.instr.InstructionID+"/refresh", nil)
	ok("POST", bankBase+"/"+st.instr.InstructionID+"/link-payment", map[string]any{"payment_id": "pay-1"})
	ok("POST", bankBase+"/"+st.instr.InstructionID+"/mark-returned", map[string]any{"expected_version": 4, "reason": "returned"})

	assert.Equal(t, []string{"le-OF-REF", "le-OF-RUN", "le-OF-RUN", "le-OF-EXC", "le-OF-EXC", "le-OF-EXC", "le-OF-REF", gateEntity,
		"le-OF-INSTR", "le-OF-INSTR", "le-OF-INSTR", "le-OF-INSTR"}, az.entities)
	assert.Equal(t, []string{"WFP_RECON_RUN", "WFP_RECON_VIEW", "WFP_RECON_CERTIFY", "WFP_RECON_VIEW", "WFP_RECON_ASSIGN", "WFP_RECON_WAIVE", "WFP_RECON_VIEW",
		"WFP_CLOSE_GATE_VIEW", "WFP_BANKING_VIEW", "WFP_BANKING_REFRESH", "WFP_PAYMENT_LINK", "WFP_PAYMENT_RETURN"}, az.actions)
	assert.NotContains(t, az.entities, "le-CALLER-CHOSE")
}

// ── runs, certification, exceptions ──────────────────────────────────────────

func TestWave8_ExecuteUsesTheAuthenticatedPrincipalAndMapsRefusals(t *testing.T) {
	st := reconFixture()
	body := map[string]any{"run_ref_id": st.ref.RunRefID, "reperform": true}
	rr := accDo(stdRecon(st), "POST", reconBase+"/runs", body, "p-3")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	require.NotNil(t, st.executed)
	assert.Equal(t, "p-3", st.executed.Actor)
	assert.True(t, st.executed.Reperform)

	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"not accepted": {domain.ErrReconResultNotAccepted, http.StatusConflict},
		"not found":    {domain.ErrRunRefNotFound, http.StatusNotFound},
		"store down":   {context.DeadlineExceeded, http.StatusServiceUnavailable},
	} {
		st2 := reconFixture()
		st2.execErr = tc.err
		assert.Equal(t, tc.want, accDo(stdRecon(st2), "POST", reconBase+"/runs", map[string]any{"run_ref_id": st2.ref.RunRefID}, "p-3").Code, name)
	}
}

func TestWave8_CertificationRefusalsAreMappedHonestly(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"same principal": {domain.ErrReconSoD, http.StatusForbidden},
		"stale":          {domain.ErrReconRunStale, http.StatusConflict},
		"not matched":    {domain.ErrReconNotMatched, http.StatusConflict},
		"already":        {domain.ErrReconAlreadyCertified, http.StatusConflict},
	} {
		st := reconFixture()
		st.err = tc.err
		rr := accDo(stdRecon(st), "POST", reconBase+"/runs/"+st.run.ReconRunID+"/certify", nil, "p-4")
		assert.Equal(t, tc.want, rr.Code, name)
	}
	st := reconFixture()
	assert.Equal(t, http.StatusNotFound, accDo(stdRecon(&stubReconStore{}), "GET", reconBase+"/runs/"+uuid.NewString(), nil, "p-4").Code)
	assert.Equal(t, http.StatusOK, accDo(stdRecon(st), "POST", reconBase+"/runs/"+st.run.ReconRunID+"/certify", nil, "p-4").Code)
	assert.Equal(t, "p-4", st.certified)
}

func TestWave8_AWaiverNeedsAReasonAndEvidenceAndAnOwnerNeedsAName(t *testing.T) {
	st := reconFixture()
	r := stdRecon(st)
	w := reconBase + "/exceptions/" + st.exc.ExceptionID + "/waive"
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", w, map[string]any{"reason": "r"}, "p-5").Code)
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", w, map[string]any{"evidence_ref": "e"}, "p-5").Code)
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", w, map[string]any{"reason": " ", "evidence_ref": "e"}, "p-5").Code)
	assert.Nil(t, st.waived)
	assert.Equal(t, http.StatusOK, accDo(r, "POST", w, map[string]any{"reason": "legacy", "evidence_ref": "ticket-1"}, "p-5").Code)
	require.NotNil(t, st.waived)
	assert.Equal(t, "p-5", st.waived.Actor)
	assert.Equal(t, "ticket-1", st.waived.Evidence)

	st.waiveErr = domain.ErrReconSoD
	assert.Equal(t, http.StatusForbidden, accDo(r, "POST", w, map[string]any{"reason": "r", "evidence_ref": "e"}, "p-3").Code)
	st.waiveErr = domain.ErrReconExceptionClosed
	assert.Equal(t, http.StatusConflict, accDo(r, "POST", w, map[string]any{"reason": "r", "evidence_ref": "e"}, "p-5").Code)

	a := reconBase + "/exceptions/" + st.exc.ExceptionID + "/assign"
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", a, map[string]any{"owner": " "}, "p-5").Code)
	assert.Equal(t, http.StatusOK, accDo(r, "POST", a, map[string]any{"owner": " finance-team "}, "p-5").Code)
	assert.Equal(t, "finance-team", st.assigned)
}

// ── close gate ───────────────────────────────────────────────────────────────

func TestWave8_CloseGateValidatesItsQueryAndReportsTheGate(t *testing.T) {
	st := reconFixture()
	st.gate = &domain.CloseGate{Open: false, Blockers: []domain.GateBlocker{{Code: domain.GateNoReconRun}}, Warnings: []domain.GateBlocker{}}
	r := stdRecon(st)
	entity := uuid.NewString()
	assert.Equal(t, http.StatusBadRequest, accDo(r, "GET", reconBase+"/close-gate?legal_entity_id=nope&period_id=2026-09", nil, "p-1").Code)
	assert.Equal(t, http.StatusBadRequest, accDo(r, "GET", reconBase+"/close-gate?legal_entity_id="+entity, nil, "p-1").Code)
	rr := accDo(r, "GET", reconBase+"/close-gate?legal_entity_id="+entity+"&period_id=2026-09", nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, [2]string{entity, "2026-09"}, st.gateArgs)
	var g domain.CloseGate
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &g))
	assert.False(t, g.Open)
	assert.Equal(t, domain.GateNoReconRun, g.Blockers[0].Code)

	st.err = context.DeadlineExceeded
	assert.Equal(t, http.StatusServiceUnavailable, accDo(r, "GET", reconBase+"/close-gate?legal_entity_id="+entity+"&period_id=2026-09", nil, "p-1").Code,
		"a gate that cannot be computed is not an open gate")
}

// ── correction cascade ───────────────────────────────────────────────────────

func TestWave8_CascadeWithdrawsUnpostedReportsPostedAndRefusesAnAcceptedResult(t *testing.T) {
	st := reconFixture()
	r := stdRecon(st)
	path := reconBase + "/corrections/" + st.ref.RunRefID + "/cascade"
	assert.Equal(t, http.StatusConflict, accDo(r, "POST", path, nil, "p-6").Code, "an accepted result has no correction to cascade")

	st.ref.AcceptanceState = domain.AcceptanceSuperseded
	st.bridges = []domain.AccountingBridge{
		{BridgeID: "b-created", PostingPurpose: domain.PurposeResult, State: domain.BridgeEventCreated, Version: 1},
		{BridgeID: "b-posted", PostingPurpose: domain.PurposeResult, State: domain.BridgePosted, Version: 3},
		{BridgeID: "b-rejected", PostingPurpose: domain.PurposeResult, State: domain.BridgeRejected, Version: 2},
		{BridgeID: "b-reversal", PostingPurpose: domain.PurposeReversal, State: domain.BridgeSubmitted, Version: 1},
		{BridgeID: "b-done", PostingPurpose: domain.PurposeResult, State: domain.BridgeCorrected, Version: 4},
	}
	rr := accDo(r, "POST", path, nil, "p-6")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Equal(t, []string{"b-created", "b-rejected"}, st.withdrawn, "only unposted result subjects are withdrawn")
	assert.Equal(t, []string{"liabilities", "instructions"}, st.superseded)
	var out handler.CascadeOutcome
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	assert.Equal(t, []string{"b-posted"}, out.AccountingNeedsReversal, "a posted journal needs its own reversal; the cascade does not do that")
	assert.Equal(t, 2, out.ExceptionsResolved)
	require.NotNil(t, out.Completeness)
	assert.False(t, out.Completeness.Complete)
}

// ── Banking commands ─────────────────────────────────────────────────────────

func TestWave8_LinkPaymentRequiresTheSamePaymentSourceAndEntity(t *testing.T) {
	st := reconFixture()
	path := bankBase + "/" + st.instr.InstructionID + "/link-payment"
	body := map[string]any{"payment_id": "pay-1"}
	mk := func(snap *paymentstatus.Snapshot, err error) chi.Router {
		return newReconRouter(st, &stubAuthZ{}, &stubLookup{snap: snap, err: err}, &stubObserver{})
	}
	good := &paymentstatus.Snapshot{PaymentID: "pay-1", LegalEntityID: "le-OF-INSTR", SourceReference: st.instr.InstructionID}

	assert.Equal(t, http.StatusBadRequest, accDo(mk(good, nil), "POST", path, map[string]any{}, "p-8").Code)
	assert.Equal(t, http.StatusServiceUnavailable, accDo(mk(nil, domain.ErrBankingUnavailable), "POST", path, body, "p-8").Code, "unreachable is unknown")
	assert.Equal(t, http.StatusNotFound, accDo(mk(nil, domain.ErrPaymentNotFound), "POST", path, body, "p-8").Code)
	assert.Equal(t, http.StatusUnprocessableEntity, accDo(mk(&paymentstatus.Snapshot{PaymentID: "pay-1", LegalEntityID: "le-OTHER", SourceReference: st.instr.InstructionID}, nil), "POST", path, body, "p-8").Code)
	assert.Equal(t, http.StatusConflict, accDo(mk(&paymentstatus.Snapshot{PaymentID: "pay-1", LegalEntityID: "le-OF-INSTR", SourceReference: "another-instruction"}, nil), "POST", path, body, "p-8").Code)
	assert.Empty(t, st.linked, "nothing is linked until every check passes")

	assert.Equal(t, http.StatusOK, accDo(mk(good, nil), "POST", path, body, "p-8").Code)
	assert.Equal(t, "pay-1", st.linked)
}

func TestWave8_RefreshRecordsWhatBankingSaysAndNeverInventsAnAnswer(t *testing.T) {
	st := reconFixture()
	path := bankBase + "/" + st.instr.InstructionID + "/refresh"
	obs := &stubObserver{}
	r := newReconRouter(st, &stubAuthZ{}, &stubLookup{}, obs)
	assert.Equal(t, http.StatusServiceUnavailable, accDo(r, "POST", path, nil, "p-8").Code, "Banking could not be asked")
	assert.Nil(t, st.recorded)

	obs.obs = &domain.BankingObservation{Source: domain.ObsSourceAttempt, RawStatus: "PENDING_UNKNOWN", Outcome: domain.OutcomeUnresolved}
	rr := accDo(r, "POST", path, nil, "p-8")
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotNil(t, st.recorded)
	assert.Equal(t, domain.OutcomeUnresolved, st.recorded.Outcome)

	st.instr.State = domain.PayApproved
	assert.Equal(t, http.StatusConflict, accDo(r, "POST", path, nil, "p-8").Code, "an instruction that never reached Banking has nothing to refresh")
	assert.Equal(t, 2, obs.calls)
}

func TestWave8_MarkReturnedNeedsAVersionAReasonAndMapsRefusals(t *testing.T) {
	st := reconFixture()
	path := bankBase + "/" + st.instr.InstructionID + "/mark-returned"
	r := stdRecon(st)
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", path, map[string]any{"reason": "r"}, "p-8").Code)
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", path, map[string]any{"expected_version": 4}, "p-8").Code)
	assert.Nil(t, st.returned)
	assert.Equal(t, http.StatusOK, accDo(r, "POST", path, map[string]any{"expected_version": 4, "reason": " account closed "}, "p-8").Code)
	require.NotNil(t, st.returned)
	assert.Equal(t, 4, st.returned.Version)
	assert.Equal(t, "account closed", st.returned.Reason)

	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"banking did not say returned": {domain.ErrInstructionNotReturned, http.StatusConflict},
		"after settlement":             {domain.ErrReturnedAfterSettlement, http.StatusConflict},
		"stale version":                {domain.ErrInstructionVersionConflict, http.StatusConflict},
	} {
		st.returnErr = tc.err
		assert.Equal(t, tc.want, accDo(r, "POST", path, map[string]any{"expected_version": 4, "reason": "r"}, "p-8").Code, name)
	}
}

// ── evidence package ─────────────────────────────────────────────────────────

func TestWave8_EvidencePackageHashesTheManifestAndCarriesNoBankDetail(t *testing.T) {
	st := reconFixture()
	r := stdRecon(st)
	path := reconBase + "/evidence/" + st.ref.RunRefID
	get := func() (hash string, raw string) {
		rr := accDo(r, "GET", path, nil, "p-1")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var out struct {
			Hash string `json:"manifest_hash"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		return out.Hash, rr.Body.String()
	}
	h1, raw := get()
	assert.Len(t, h1, 64)
	for _, forbidden := range []string{"payee_ref", "destination_id", "payer_account", "account_number", "iban", "sort_code"} {
		assert.NotContains(t, raw, forbidden)
	}
	h2, _ := get()
	assert.Equal(t, h1, h2, "the same evidence hashes the same, whenever it is read")
	st.result.ResultFingerprint = "changed"
	h3, _ := get()
	assert.NotEqual(t, h1, h3, "any difference in the evidence changes the hash")

	denied := newReconRouter(st, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubLookup{}, &stubObserver{})
	assert.Equal(t, http.StatusForbidden, accDo(denied, "GET", path, nil, "p-1").Code)
}
