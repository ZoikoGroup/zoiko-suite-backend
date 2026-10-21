package handler_test

import (
	"context"
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

type stubLiabStore struct {
	ref        *domain.PayrollRunRef
	result     *domain.PayrollResult
	components []domain.PayrollResultComponent
	classes    []domain.LiabilityClass
	liability  *domain.PayrollLiability
	err        error

	applied     bool
	appliedPlan domain.RecognitionPlan
	appliedRev  map[string]string
	appliedAsOf string
	registered  *domain.RegisterLiabilityClassRequest
	settledReq  *domain.SettleLiabilityRequest
	settledEv   store.SettlementEvidence
	settleReply *store.SettleOutcome
	scheduled   string
	adjusted    bool
}

func (s *stubLiabStore) GetPayrollRunRef(context.Context, string) (*domain.PayrollRunRef, error) {
	if s.ref == nil {
		return nil, domain.ErrRunRefNotFound
	}
	return s.ref, nil
}
func (s *stubLiabStore) GetPayrollResultByRef(context.Context, string) (*domain.PayrollResult, error) {
	if s.result == nil {
		return nil, domain.ErrResultNotFound
	}
	return s.result, nil
}
func (s *stubLiabStore) ListResultComponents(context.Context, string, string) ([]domain.PayrollResultComponent, error) {
	return s.components, nil
}
func (s *stubLiabStore) RegisterLiabilityClass(_ context.Context, req domain.RegisterLiabilityClassRequest, _ string) (*domain.LiabilityClass, error) {
	s.registered = &req
	if s.err != nil {
		return nil, s.err
	}
	return &domain.LiabilityClass{ClassCode: req.ClassCode, ClassVersion: 1}, nil
}
func (s *stubLiabStore) ListLiabilityClasses(context.Context, string) ([]domain.LiabilityClass, error) {
	return s.classes, nil
}
func (s *stubLiabStore) ApplyRecognition(_ context.Context, _ *domain.PayrollRunRef, _ *domain.PayrollResult, plan domain.RecognitionPlan,
	review map[string]string, _, _, asOf string, _ store.LiabilityEventBuilder) (*store.RecognitionOutcome, error) {
	s.applied, s.appliedPlan, s.appliedRev, s.appliedAsOf = true, plan, review, asOf
	if s.err != nil {
		return nil, s.err
	}
	return &store.RecognitionOutcome{NewCount: len(plan.Liabilities)}, nil
}
func (s *stubLiabStore) DueSweep(context.Context, string, string, store.LiabilityEventBuilder) ([]domain.PayrollLiability, error) {
	return nil, s.err
}
func (s *stubLiabStore) ListLiabilities(context.Context, store.LiabilityFilter) ([]domain.PayrollLiability, error) {
	return []domain.PayrollLiability{}, s.err
}
func (s *stubLiabStore) GetLiability(context.Context, string) (*domain.PayrollLiability, error) {
	if s.liability == nil {
		return nil, domain.ErrLiabilityNotFound
	}
	return s.liability, nil
}
func (s *stubLiabStore) ListLiabilityExceptions(context.Context, string, string, string) ([]domain.LiabilityException, error) {
	return []domain.LiabilityException{}, s.err
}
func (s *stubLiabStore) ScheduleLiability(_ context.Context, _ string, v int, date, _ string, _ store.LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	s.scheduled = date
	if s.err != nil {
		return nil, s.err
	}
	l := *s.liability
	l.State, l.Version = domain.LiabScheduled, v+1
	return &l, nil
}
func (s *stubLiabStore) SettleLiability(_ context.Context, _ string, req domain.SettleLiabilityRequest, ev store.SettlementEvidence, _ string, _ store.LiabilityEventBuilder) (*store.SettleOutcome, error) {
	s.settledReq, s.settledEv = &req, ev
	if s.err != nil {
		return nil, s.err
	}
	if s.settleReply != nil {
		return s.settleReply, nil
	}
	return &store.SettleOutcome{Liability: s.liability, Settlement: &domain.LiabilitySettlement{PaymentID: req.PaymentID}}, nil
}
func (s *stubLiabStore) AdjustLiability(context.Context, string, domain.AdjustLiabilityRequest, string, store.LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	s.adjusted = true
	return s.liability, s.err
}
func (s *stubLiabStore) DisputeLiability(context.Context, string, int, string, store.LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	return s.liability, s.err
}
func (s *stubLiabStore) ClearLiabilityDispute(context.Context, string, int, store.LiabilityEventBuilder) (*domain.PayrollLiability, error) {
	return s.liability, s.err
}
func (s *stubLiabStore) SupersedeLiabilitiesForRun(context.Context, string, store.LiabilityEventBuilder) (*store.SupersedeOutcome, error) {
	return &store.SupersedeOutcome{}, s.err
}
func (s *stubLiabStore) ReconcileLiabilities(context.Context, string, store.LiabilityEventBuilder) (*store.LiabilityReconciliation, error) {
	return &store.LiabilityReconciliation{}, s.err
}

type stubAuthorities struct {
	byID  map[string]error
	calls []string
	args  [][3]string
}

func (a *stubAuthorities) ValidateAuthority(_ context.Context, tenant, entity, id string) error {
	a.calls = append(a.calls, id)
	a.args = append(a.args, [3]string{tenant, entity, id})
	return a.byID[id]
}

type stubBanking struct {
	err   error
	calls [][3]string
	ev    paymentstatus.Evidence
}

func (b *stubBanking) VerifySettled(_ context.Context, tenant, entity, payment string) (*paymentstatus.Evidence, error) {
	b.calls = append(b.calls, [3]string{tenant, entity, payment})
	if b.err != nil {
		return nil, b.err
	}
	ev := b.ev
	if ev.Status == "" {
		ev.Status = "SETTLED"
	}
	return &ev, nil
}

const liabBase = "/v1/payroll/wfp/liabilities"

func newLiabRouter(st *stubLiabStore, az handler.AuthZClient, auth *stubAuthorities, bank *stubBanking) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(rw, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPLiabilityRoutes(r, handler.NewWFPLiability(st, az, auth, bank, zap.NewNop()))
	return r
}

func liabFixture() *stubLiabStore {
	id := uuid.NewString()
	return &stubLiabStore{
		ref: &domain.PayrollRunRef{RunRefID: uuid.NewString(), LegalEntityID: "le-OF-REF", PayDate: "2026-09-30", PayrollPeriodID: "2026-09",
			AcceptanceState: domain.AcceptanceAccepted},
		result: &domain.PayrollResult{ResultID: uuid.NewString(), Outcome: domain.ResultAccepted, Currency: "GBP"},
		components: []domain.PayrollResultComponent{
			{ComponentID: "1", Code: "TAX", Kind: domain.KindEmployeeTax, Amount: "600.10"},
			{ComponentID: "2", Code: "PEN", Kind: domain.KindEmployeePension, Amount: "100"},
			{ComponentID: "3", Code: "NI", Kind: domain.KindEmployerSocial, Amount: "250"},
		},
		classes: []domain.LiabilityClass{
			{ClassRowID: "r1", ComponentKind: domain.KindEmployeeTax, ComponentCode: "*", ClassCode: "PAYE", LiabilityType: "TAX", AuthorityCounterpartyID: "cp-hmrc",
				JurisdictionID: "GB", DueRuleKind: domain.DueDayOfFollowingMonth, DueRuleValue: 22, Active: true, EffectiveFrom: "2026-01-01"},
			{ClassRowID: "r2", ComponentKind: domain.KindEmployeePension, ComponentCode: "*", ClassCode: "PENSION", LiabilityType: "PENSION", AuthorityCounterpartyID: "cp-nest",
				JurisdictionID: "GB", DueRuleKind: domain.DueDaysAfterPayDate, DueRuleValue: 10, Active: true, EffectiveFrom: "2026-01-01"},
			{ClassRowID: "r3", ComponentKind: domain.KindEmployerSocial, ComponentCode: "*", ClassCode: "NI-ER", LiabilityType: "SOCIAL", AuthorityCounterpartyID: "cp-hmrc",
				JurisdictionID: "GB", DueRuleKind: domain.DueDayOfFollowingMonth, DueRuleValue: 22, Active: true, EffectiveFrom: "2026-01-01"},
		},
		liability: &domain.PayrollLiability{LiabilityID: id, TenantID: "tenant-abc", LegalEntityID: "le-OF-LIABILITY", State: domain.LiabDue, Version: 3,
			Amount: "600.1000", Currency: "GBP"},
	}
}

func recognizeBody(st *stubLiabStore) map[string]any {
	return map[string]any{"run_ref_id": st.ref.RunRefID}
}

// ── identity & authorization ─────────────────────────────────────────────────

func TestWFP06_IdentityAuthorizationAndForgedTenant(t *testing.T) {
	st := liabFixture()
	r := newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{})
	assert.Equal(t, http.StatusUnauthorized, accDo(r, "POST", liabBase+"/recognize", recognizeBody(st), "").Code)

	denied := newLiabRouter(st, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubAuthorities{}, &stubBanking{})
	assert.Equal(t, http.StatusForbidden, accDo(denied, "POST", liabBase+"/recognize", recognizeBody(st), "p-1").Code)
	down := newLiabRouter(st, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}, &stubAuthorities{}, &stubBanking{})
	assert.Equal(t, http.StatusServiceUnavailable, accDo(down, "POST", liabBase+"/recognize", recognizeBody(st), "p-1").Code, "fail closed")
	assert.False(t, st.applied)

	forged := map[string]any{"run_ref_id": st.ref.RunRefID, "tenant_id": "someone-elses"}
	rr := accDo(r, "POST", liabBase+"/recognize", forged, "p-1")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Equal(t, "invalid_json", accCode(t, rr))
}

func TestWFP06_ActionsAreAuthorizedAgainstTheStoredEntityNotACallerHeader(t *testing.T) {
	st := liabFixture()
	az := &entityAuthZ{}
	r := newLiabRouter(st, az, &stubAuthorities{}, &stubBanking{})
	hdr := []string{"X-Legal-Entity-Id", "le-CALLER-CHOSE"}

	require.Equal(t, http.StatusCreated, accDo(r, "POST", liabBase+"/recognize", recognizeBody(st), "p-1", hdr...).Code)
	require.Equal(t, http.StatusOK, accDo(r, "GET", liabBase+"/"+st.liability.LiabilityID, nil, "p-1", hdr...).Code)
	require.Equal(t, http.StatusOK, accDo(r, "POST", liabBase+"/reconcile", recognizeBody(st), "p-1", hdr...).Code)
	require.Equal(t, http.StatusOK, accDo(r, "GET", liabBase+"?run_ref_id="+st.ref.RunRefID, nil, "p-1", hdr...).Code)
	assert.Equal(t, []string{"le-OF-REF", "le-OF-LIABILITY", "le-OF-REF", "le-OF-REF"}, az.entities)
	assert.Equal(t, []string{"WFP_LIABILITY_RECOGNIZE", "WFP_LIABILITY_VIEW", "WFP_LIABILITY_RECONCILE", "WFP_LIABILITY_VIEW"}, az.actions)
}

// ── recognition ──────────────────────────────────────────────────────────────

func TestWFP06_RecognizePlansFromTheRegistryAndValidatesEachAuthorityOnce(t *testing.T) {
	st := liabFixture()
	auth := &stubAuthorities{}
	r := newLiabRouter(st, &stubAuthZ{}, auth, &stubBanking{})

	rr := accDo(r, "POST", liabBase+"/recognize", recognizeBody(st), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	require.True(t, st.applied)
	require.Len(t, st.appliedPlan.Liabilities, 3)
	assert.ElementsMatch(t, []string{"cp-hmrc", "cp-nest"}, auth.calls, "two authorities, three liabilities: hmrc is asked once")
	for _, a := range auth.args {
		assert.Equal(t, "tenant-abc", a[0], "the authenticated tenant")
		assert.Equal(t, "le-OF-REF", a[1], "the stored entity")
	}
	assert.Equal(t, "", st.appliedRev["cp-hmrc"])
	assert.Len(t, st.appliedAsOf, 10, "as-of is today's date")
}

func TestWFP06_AnUnreachableCounterpartyServiceStopsRecognitionAndAnInvalidOneMarksReview(t *testing.T) {
	st := liabFixture()
	r := newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{byID: map[string]error{"cp-nest": domain.ErrAuthorityUnavailable}}, &stubBanking{})
	rr := accDo(r, "POST", liabBase+"/recognize", recognizeBody(st), "p-1")
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, "unknown is not valid")
	assert.False(t, st.applied, "nothing is written on an unknown authority")

	st = liabFixture()
	r = newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{byID: map[string]error{"cp-nest": domain.ErrAuthorityInvalid}}, &stubBanking{})
	rr = accDo(r, "POST", liabBase+"/recognize", recognizeBody(st), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code)
	assert.NotEmpty(t, st.appliedRev["cp-nest"], "an invalid authority is recognized under review")
	assert.Empty(t, st.appliedRev["cp-hmrc"])
}

func TestWFP06_RecognizeRefusesAResultThatIsNotAccepted(t *testing.T) {
	for name, mutate := range map[string]func(*stubLiabStore){
		"superseded run":  func(s *stubLiabStore) { s.ref.AcceptanceState = domain.AcceptanceSuperseded },
		"rejected result": func(s *stubLiabStore) { s.result.Outcome = domain.ResultRejected },
	} {
		st := liabFixture()
		mutate(st)
		rr := accDo(newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{}), "POST", liabBase+"/recognize", recognizeBody(st), "p-1")
		assert.Equal(t, http.StatusConflict, rr.Code, name)
		assert.Equal(t, "INVALID_STATE", accCode(t, rr), name)
		assert.False(t, st.applied, name)
	}
	st := liabFixture()
	rr := accDo(newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{}), "POST", liabBase+"/recognize", map[string]any{"run_ref_id": "nope"}, "p-1")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	st.ref = nil
	rr = accDo(newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{}), "POST", liabBase+"/recognize", map[string]any{"run_ref_id": uuid.NewString()}, "p-1")
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestWFP06_AnUncoveredComponentReachesTheStoreAsAnExceptionInThePlan(t *testing.T) {
	st := liabFixture()
	st.classes = st.classes[:1] // only PAYE is registered
	rr := accDo(newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{}), "POST", liabBase+"/recognize", recognizeBody(st), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code)
	require.Len(t, st.appliedPlan.Liabilities, 1)
	var codes []string
	for _, e := range st.appliedPlan.Exceptions {
		codes = append(codes, e.Code+"/"+e.SubjectKey)
	}
	assert.Equal(t, []string{domain.ExcUnmappedClass + "/EMPLOYEE_PENSION:PEN", domain.ExcUnmappedClass + "/EMPLOYER_SOCIAL:NI"}, codes)
}

// ── registry ─────────────────────────────────────────────────────────────────

func TestWFP06_ClassRegistrationValidatesTheAuthorityFailClosed(t *testing.T) {
	body := map[string]any{
		"legal_entity_id": "le-1", "component_kind": "EMPLOYEE_TAX", "class_code": "PAYE", "liability_type": "TAX", "authority_counterparty_id": "cp-hmrc",
		"jurisdiction_id": "GB", "due_rule_kind": "DAY_OF_FOLLOWING_MONTH", "due_rule_value": 22, "rule_ref": "gov-1", "effective_from": "2026-01-01",
	}
	cases := map[string]struct {
		err    error
		status int
	}{"valid": {nil, http.StatusCreated}, "invalid authority": {domain.ErrAuthorityInvalid, http.StatusUnprocessableEntity},
		"service down": {domain.ErrAuthorityUnavailable, http.StatusServiceUnavailable}}
	for name, c := range cases {
		st := liabFixture()
		auth := &stubAuthorities{byID: map[string]error{"cp-hmrc": c.err}}
		rr := accDo(newLiabRouter(st, &stubAuthZ{}, auth, &stubBanking{}), "POST", liabBase+"/classes", body, "p-1")
		assert.Equal(t, c.status, rr.Code, name)
		assert.Equal(t, c.status == http.StatusCreated, st.registered != nil, "%s: stored only when the authority is real", name)
		assert.Equal(t, [3]string{"tenant-abc", "le-1", "cp-hmrc"}, auth.args[0])
	}

	st := liabFixture()
	bad := map[string]any{}
	for k, v := range body {
		bad[k] = v
	}
	bad["component_kind"] = "EARNING"
	rr := accDo(newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{}), "POST", liabBase+"/classes", bad, "p-1")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Nil(t, st.registered)
	st.err = domain.ErrLiabilityClassOrder
	rr = accDo(newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{}), "POST", liabBase+"/classes", body, "p-1")
	assert.Equal(t, http.StatusConflict, rr.Code)
}

// ── settlement ───────────────────────────────────────────────────────────────

func settleBody(payment, amount string) map[string]any {
	return map[string]any{"payment_id": payment, "amount": amount, "currency": "GBP"}
}

func TestWFP06_SettlementNeedsBankingEvidenceAndFailsClosed(t *testing.T) {
	st := liabFixture()
	bank := &stubBanking{ev: paymentstatus.Evidence{Status: "SETTLED", FinalitySource: "PROVIDER_CALLBACK"}}
	r := newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, bank)
	path := liabBase + "/" + st.liability.LiabilityID + "/settlements"

	rr := accDo(r, "POST", path, settleBody("pay-1", "100.5"), "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	assert.Equal(t, [][3]string{{"tenant-abc", "le-OF-LIABILITY", "pay-1"}}, bank.calls, "verified for the liability's own entity")
	require.NotNil(t, st.settledReq)
	assert.Equal(t, "pay-1", st.settledReq.PaymentID)
	assert.Equal(t, "SETTLED", st.settledEv.BankingStatus)
	require.NotNil(t, st.settledEv.FinalitySource)
	assert.Equal(t, "PROVIDER_CALLBACK", *st.settledEv.FinalitySource)

	// A replay answers 200 and says so.
	st.settleReply = &store.SettleOutcome{Liability: st.liability, Settlement: &domain.LiabilitySettlement{PaymentID: "pay-1"}, Replay: true}
	rr = accDo(r, "POST", path, settleBody("pay-1", "100.5"), "p-1")
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"replay":true`)

	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"banking unreachable": {domain.ErrBankingUnavailable, http.StatusServiceUnavailable, "dependency_unavailable"},
		"not settled":         {domain.ErrPaymentNotSettled, http.StatusConflict, "PAYMENT_NOT_SETTLED"},
		"open conflict":       {domain.ErrPaymentInConflict, http.StatusConflict, "PAYMENT_NOT_SETTLED"},
		"unknown payment":     {domain.ErrPaymentNotFound, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		"other entity":        {domain.ErrPaymentEntityMismatch, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
	} {
		st := liabFixture()
		rr := accDo(newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{err: tc.err}), "POST", liabBase+"/"+st.liability.LiabilityID+"/settlements", settleBody("pay-1", "10"), "p-1")
		assert.Equal(t, tc.status, rr.Code, name)
		assert.Equal(t, tc.code, accCode(t, rr), name)
		assert.Nil(t, st.settledReq, "%s: nothing is settled without Banking evidence", name)
	}
}

func TestWFP06_SettlementRequestIsValidatedBeforeBankingIsAsked(t *testing.T) {
	st := liabFixture()
	bank := &stubBanking{}
	r := newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, bank)
	path := liabBase + "/" + st.liability.LiabilityID + "/settlements"
	for name, body := range map[string]any{
		"no payment":   settleBody("", "10"),
		"zero":         settleBody("p", "0"),
		"negative":     settleBody("p", "-5"),
		"too precise":  settleBody("p", "1.23456"),
		"lowercase":    map[string]any{"payment_id": "p", "amount": "1", "currency": "gbp"},
		"extra field":  map[string]any{"payment_id": "p", "amount": "1", "currency": "GBP", "mark_paid": true},
		"manual state": map[string]any{"state": "SETTLED"},
	} {
		rr := accDo(r, "POST", path, body, "p-1")
		assert.Equal(t, http.StatusBadRequest, rr.Code, name)
	}
	assert.Empty(t, bank.calls)
	assert.Nil(t, st.settledReq)
}

func TestWFP06_StoreRefusalsMapToStableCodes(t *testing.T) {
	cases := map[error]struct {
		status int
		code   string
	}{
		domain.ErrOverSettlement:           {http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		domain.ErrSettlementCurrency:       {http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		domain.ErrSettlementConflict:       {http.StatusConflict, "CONFLICT"},
		domain.ErrLiabilityState:           {http.StatusConflict, "INVALID_STATE"},
		domain.ErrLiabilityVersionConflict: {http.StatusConflict, "VERSION_CONFLICT"},
		domain.ErrLiabilityNotFound:        {http.StatusNotFound, "not_found"},
	}
	for storeErr, want := range cases {
		st := liabFixture()
		st.err = storeErr
		rr := accDo(newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{}), "POST", liabBase+"/"+st.liability.LiabilityID+"/settlements", settleBody("pay-1", "10"), "p-1")
		assert.Equal(t, want.status, rr.Code, storeErr.Error())
		assert.Equal(t, want.code, accCode(t, rr), storeErr.Error())
	}
}

// ── schedule / adjust / dispute ──────────────────────────────────────────────

func TestWFP06_ScheduleNeedsAVersionAndADate(t *testing.T) {
	st := liabFixture()
	r := newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{})
	path := liabBase + "/" + st.liability.LiabilityID + "/schedule"

	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", path, map[string]any{"scheduled_for": "2026-10-15"}, "p-1").Code, "no version")
	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", path, map[string]any{"expected_version": 3, "scheduled_for": "15/10/2026"}, "p-1").Code)
	assert.Empty(t, st.scheduled)

	rr := accDo(r, "POST", path, map[string]any{"expected_version": 3, "scheduled_for": "2026-10-15"}, "p-1")
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "2026-10-15", st.scheduled)
	rr = accDo(r, "POST", path, map[string]any{"scheduled_for": "2026-10-16"}, "p-1", "If-Match", `"3"`)
	assert.Equal(t, http.StatusOK, rr.Code, "If-Match is accepted as the version")

	st.err = domain.ErrLiabilityNeedsReview
	rr = accDo(r, "POST", path, map[string]any{"expected_version": 3, "scheduled_for": "2026-10-15"}, "p-1")
	assert.Equal(t, http.StatusConflict, rr.Code, "a liability under review cannot be scheduled")
}

func TestWFP06_AdjustAndDisputeRequireTheirEvidence(t *testing.T) {
	st := liabFixture()
	r := newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{})
	id := st.liability.LiabilityID

	for name, body := range map[string]any{
		"no reason":   map[string]any{"expected_version": 3, "delta": "5", "reason": "", "evidence_ref": "e"},
		"no evidence": map[string]any{"expected_version": 3, "delta": "5", "reason": "r", "evidence_ref": ""},
		"zero":        map[string]any{"expected_version": 3, "delta": "0", "reason": "r", "evidence_ref": "e"},
		"no version":  map[string]any{"delta": "5", "reason": "r", "evidence_ref": "e"},
	} {
		assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", liabBase+"/"+id+"/adjustments", body, "p-1").Code, name)
	}
	assert.False(t, st.adjusted)
	assert.Equal(t, http.StatusOK, accDo(r, "POST", liabBase+"/"+id+"/adjustments", map[string]any{"expected_version": 3, "delta": "-5", "reason": "r", "evidence_ref": "e"}, "p-1").Code)
	assert.True(t, st.adjusted)

	assert.Equal(t, http.StatusBadRequest, accDo(r, "POST", liabBase+"/"+id+"/dispute", map[string]any{"expected_version": 3}, "p-1").Code, "a dispute needs a reason")
	assert.Equal(t, http.StatusOK, accDo(r, "POST", liabBase+"/"+id+"/dispute", map[string]any{"expected_version": 3, "reason": "wrong amount"}, "p-1").Code)
	assert.Equal(t, http.StatusOK, accDo(r, "POST", liabBase+"/"+id+"/clear-dispute", map[string]any{"expected_version": 4}, "p-1").Code)
}

func TestWFP06_ReadsAreAuthorizedAndShaped(t *testing.T) {
	st := liabFixture()
	r := newLiabRouter(st, &stubAuthZ{}, &stubAuthorities{}, &stubBanking{})
	assert.Equal(t, http.StatusBadRequest, accDo(r, "GET", liabBase, nil, "p-1").Code, "a list needs an entity or a run reference")
	assert.Equal(t, http.StatusOK, accDo(r, "GET", liabBase+"?legal_entity_id=le-1&state=DUE", nil, "p-1").Code)
	assert.Equal(t, http.StatusBadRequest, accDo(r, "GET", liabBase+"?run_ref_id=bad", nil, "p-1").Code)
	assert.Equal(t, http.StatusOK, accDo(r, "GET", liabBase+"/exceptions?legal_entity_id=le-1", nil, "p-1").Code)
	assert.Equal(t, http.StatusOK, accDo(r, "GET", liabBase+"/classes?legal_entity_id=le-1", nil, "p-1").Code)
	assert.Equal(t, http.StatusBadRequest, accDo(r, "GET", liabBase+"/classes", nil, "p-1").Code)
}
