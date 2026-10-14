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
	"zoiko.io/payroll-run-svc/internal/paydeps"
	"zoiko.io/payroll-run-svc/internal/store"
)

// ── stubs ────────────────────────────────────────────────────────────────────

type stubPayStore struct {
	ref       *domain.PayrollRunRef
	result    *domain.PayrollResult
	workers   []domain.PayrollResultWorker
	liability *domain.PayrollLiability
	reserved  string
	instr     *domain.PaymentInstruction

	created      *domain.PaymentInstruction
	createdNew   bool
	validated    *store.ValidationFacts
	approvedBy   string
	invalidated  string
	beginCalls   int
	outcome      [3]string // attempt, status, error
	released     string
	cancelledFor string
	err          error
}

func (s *stubPayStore) GetPayrollRunRef(context.Context, string) (*domain.PayrollRunRef, error) {
	if s.ref == nil {
		return nil, domain.ErrRunRefNotFound
	}
	return s.ref, nil
}
func (s *stubPayStore) GetPayrollResultByRef(context.Context, string) (*domain.PayrollResult, error) {
	if s.result == nil {
		return nil, domain.ErrResultNotFound
	}
	return s.result, nil
}
func (s *stubPayStore) ListResultWorkers(context.Context, string) ([]domain.PayrollResultWorker, error) {
	return s.workers, nil
}
func (s *stubPayStore) GetLiability(context.Context, string) (*domain.PayrollLiability, error) {
	if s.liability == nil {
		return nil, domain.ErrLiabilityNotFound
	}
	return s.liability, nil
}
func (s *stubPayStore) ReservedForLiability(context.Context, string) (string, error) {
	return s.reserved, nil
}
func (s *stubPayStore) CreatePaymentInstruction(_ context.Context, in *domain.PaymentInstruction, _ store.PaymentEventBuilder) (*domain.PaymentInstruction, bool, error) {
	s.created = in
	if s.err != nil {
		return nil, false, s.err
	}
	in.State, in.Version = domain.PayDraft, 1
	return in, s.createdNew, nil
}
func (s *stubPayStore) GetPaymentInstruction(context.Context, string) (*domain.PaymentInstruction, error) {
	if s.instr == nil {
		return nil, domain.ErrInstructionNotFound
	}
	cp := *s.instr
	return &cp, nil
}
func (s *stubPayStore) ListPaymentInstructions(context.Context, store.InstructionFilter) ([]domain.PaymentInstruction, error) {
	return []domain.PaymentInstruction{}, s.err
}
func (s *stubPayStore) ValidatePaymentInstruction(_ context.Context, _ string, _ int, f store.ValidationFacts) (*domain.PaymentInstruction, error) {
	s.validated = &f
	if s.err != nil {
		return nil, s.err
	}
	cp := *s.instr
	cp.State = domain.PayValidated
	cp.RequiredSignatures = f.RequiredSignatures
	return &cp, nil
}
func (s *stubPayStore) ApprovePaymentInstruction(_ context.Context, _, approver, _ string, _ store.PaymentEventBuilder) (*domain.PaymentInstruction, error) {
	s.approvedBy = approver
	if s.err != nil {
		return nil, s.err
	}
	cp := *s.instr
	cp.State = domain.PayApproved
	return &cp, nil
}
func (s *stubPayStore) InvalidatePaymentApproval(_ context.Context, _, reason string, _ store.PaymentEventBuilder) (*domain.PaymentInstruction, error) {
	s.invalidated = reason
	return s.instr, s.err
}
func (s *stubPayStore) CancelPaymentInstruction(_ context.Context, _ string, _ int, reason string, _ store.PaymentEventBuilder) (*domain.PaymentInstruction, error) {
	s.cancelledFor = reason
	return s.instr, s.err
}
func (s *stubPayStore) SupersedeInstructionsForRun(context.Context, string, store.PaymentEventBuilder) (*store.InstructionSupersedeOutcome, error) {
	return &store.InstructionSupersedeOutcome{}, s.err
}
func (s *stubPayStore) BeginSubmitToBanking(_ context.Context, _ string, _ int, _ string) (*domain.PaymentInstruction, bool, error) {
	s.beginCalls++
	if s.err != nil {
		return nil, false, s.err
	}
	cp := *s.instr
	cp.State = domain.PaySubmitted
	return &cp, s.instr.State == domain.PaySubmitted, nil
}
func (s *stubPayStore) RecordBankingOutcome(_ context.Context, _, attempt, status, bankErr string, _ store.PaymentEventBuilder) (*domain.PaymentInstruction, error) {
	s.outcome = [3]string{attempt, status, bankErr}
	cp := *s.instr
	cp.State, cp.BankingStatus = domain.PaySubmitted, &status
	if domain.HandedOffStatus(status) {
		cp.State = domain.PayHandedOff
	}
	return &cp, s.err
}
func (s *stubPayStore) ReleaseRefusedSubmission(_ context.Context, _, reason string) (*domain.PaymentInstruction, error) {
	s.released = reason
	return s.instr, nil
}
func (s *stubPayStore) PaymentInstructionFingerprint(context.Context, string) (string, string, error) {
	if s.instr == nil {
		return "", "", domain.ErrInstructionNotFound
	}
	fp := ""
	if s.instr.ApprovedFingerprint != nil {
		fp = *s.instr.ApprovedFingerprint
	}
	return s.instr.State, fp, nil
}

type stubPayees struct {
	dest *domain.PayeeDestination
	err  error
	got  []string
}

func (p *stubPayees) ActiveDestination(_ context.Context, _, _, _, payeeRef string) (*domain.PayeeDestination, error) {
	p.got = append(p.got, payeeRef)
	return p.dest, p.err
}

type stubWorkers struct {
	ref string
	err error
}

func (w *stubWorkers) PayeeRefFor(context.Context, string, string, string, string, string) (string, error) {
	return w.ref, w.err
}

type stubPayers struct {
	err error
	got []string
}

func (p *stubPayers) Verify(_ context.Context, _, principal, _, account, _ string) error {
	p.got = append(p.got, principal+"/"+account)
	return p.err
}

type stubPolicy struct {
	result string
	err    error
	amount float64
}

func (p *stubPolicy) Evaluate(_ context.Context, _, _, _ string, amount float64) (string, string, error) {
	p.amount = amount
	if p.result == "" {
		return "WITHIN_THRESHOLD", "pol-1", p.err
	}
	return p.result, "pol-1", p.err
}

type stubBank struct {
	attempt *paydeps.Attempt
	err     error
	reqs    []paydeps.HandoffRequest
}

func (b *stubBank) Handoff(_ context.Context, r paydeps.HandoffRequest) (*paydeps.Attempt, error) {
	b.reqs = append(b.reqs, r)
	return b.attempt, b.err
}

const payBase = "/v1/payroll/wfp/payment-instructions"

type payRig struct {
	st      *stubPayStore
	payees  *stubPayees
	workers *stubWorkers
	payers  *stubPayers
	policy  *stubPolicy
	bank    *stubBank
	authz   handler.AuthZClient
	router  chi.Router
}

func newPayRig(st *stubPayStore, az handler.AuthZClient) *payRig {
	g := &payRig{
		st: st, payees: &stubPayees{dest: goodDest()}, workers: &stubWorkers{ref: "payee-1"}, payers: &stubPayers{}, policy: &stubPolicy{},
		bank: &stubBank{attempt: &paydeps.Attempt{AttemptID: "att-1", Status: "SUBMITTED"}}, authz: az,
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(rw, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterWFPPaymentRoutes(r, handler.NewWFPPayment(st, az, handler.PaymentDeps{
		Payees: g.payees, Workers: g.workers, Payers: g.payers, Policy: g.policy, Banking: g.bank, HighValueSi: 2,
	}, zap.NewNop()))
	g.router = r
	return g
}

func goodDest() *domain.PayeeDestination {
	return &domain.PayeeDestination{PayeeRef: "payee-1", DestinationID: "dest-1", Version: "v1", Fingerprint: "bank-fp", LegalEntityID: "le-OF-REF", Currency: "GBP"}
}

func payFixture() *stubPayStore {
	w := uuid.NewString()
	runRef := uuid.NewString()
	return &stubPayStore{
		ref:     &domain.PayrollRunRef{RunRefID: runRef, LegalEntityID: "le-OF-REF", PayDate: "2026-09-30", PayrollPeriodID: "2026-09", AcceptanceState: domain.AcceptanceAccepted},
		result:  &domain.PayrollResult{ResultID: uuid.NewString(), Outcome: domain.ResultAccepted, Currency: "GBP", ReceivedBy: "recv-1"},
		workers: []domain.PayrollResultWorker{{WorkerID: w, NetPay: "2400.4000", Currency: "GBP"}},
		liability: &domain.PayrollLiability{LiabilityID: uuid.NewString(), RunRefID: runRef, ClassCode: "PAYE", Currency: "GBP", AuthorityCounterpartyID: "cp-hmrc",
			OutstandingAmount: "600.1000", ExceptionState: domain.LiabExcNone},
		reserved: "100.0000", createdNew: true,
	}
}

// instrFor builds an instruction of the fixture in the given state, pinned to goodDest.
func instrFor(st *stubPayStore, state string) *domain.PaymentInstruction {
	w := st.workers[0].WorkerID
	i := &domain.PaymentInstruction{
		InstructionID: uuid.NewString(), TenantID: "tenant-abc", LegalEntityID: "le-OF-REF", InstructionType: domain.PayTypeNetPay, RunRefID: st.ref.RunRefID,
		ResultID: st.result.ResultID, WorkerID: &w, PayeeRef: "payee-1", DestinationID: "dest-1", DestinationVersion: "v1", DestinationFingerprint: "bank-fp",
		Amount: "2400.4000", Currency: "GBP", ValueDate: "2026-09-30", PayerAccountRef: "acct-1", PaymentMethod: domain.PayMethodBank,
		PaymentReference: "Payroll 2026-09", State: state, Version: 3, RequiredSignatures: 1, CreatedBy: "prep-1",
	}
	i.Fingerprint = domain.ComputeInstructionFingerprint(i)
	if state == domain.PayApproved || state == domain.PaySubmitted {
		fp := i.Fingerprint
		i.ApprovedFingerprint = &fp
	}
	st.instr = i
	return i
}

func payCreateBody(st *stubPayStore) map[string]any {
	return map[string]any{"run_ref_id": st.ref.RunRefID, "instruction_type": "NET_PAY", "worker_id": st.workers[0].WorkerID, "payer_account_ref": "acct-1", "value_date": "2026-09-30"}
}

// ── identity & authorization ─────────────────────────────────────────────────

func TestWFP07_IdentityAuthorizationAndForgedInput(t *testing.T) {
	st := payFixture()
	r := newPayRig(st, &stubAuthZ{})
	assert.Equal(t, http.StatusUnauthorized, accDo(r.router, "POST", payBase, payCreateBody(st), "").Code)
	denied := newPayRig(st, &stubAuthZ{err: domain.ErrAuthorizationDenied})
	assert.Equal(t, http.StatusForbidden, accDo(denied.router, "POST", payBase, payCreateBody(st), "p-1").Code)
	down := newPayRig(st, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable})
	assert.Equal(t, http.StatusServiceUnavailable, accDo(down.router, "POST", payBase, payCreateBody(st), "p-1").Code, "fail closed")
	assert.Nil(t, st.created)

	// Bank details and a forged tenant are not part of the contract.
	for _, extra := range []map[string]any{{"account_number": "12345678"}, {"payee_account": "x"}, {"tenant_id": "other"}, {"amount": "1"}} {
		body := payCreateBody(st)
		for k, v := range extra {
			body[k] = v
		}
		rr := accDo(r.router, "POST", payBase, body, "p-1")
		assert.Equal(t, http.StatusBadRequest, rr.Code, "%v", extra)
	}
	assert.Nil(t, st.created)
}

func TestWFP07_ActionsAreAuthorizedAgainstTheStoredEntity(t *testing.T) {
	st := payFixture()
	az := &entityAuthZ{}
	r := newPayRig(st, az)
	hdr := []string{"X-Legal-Entity-Id", "le-CALLER-CHOSE"}
	require.Equal(t, http.StatusCreated, accDo(r.router, "POST", payBase, payCreateBody(st), "p-1", hdr...).Code)
	instrFor(st, domain.PayDraft).LegalEntityID = "le-OF-INSTRUCTION"
	require.Equal(t, http.StatusOK, accDo(r.router, "GET", payBase+"/"+st.instr.InstructionID, nil, "p-1", hdr...).Code)
	assert.Equal(t, []string{"le-OF-REF", "le-OF-INSTRUCTION"}, az.entities)
	assert.Equal(t, []string{"WFP_PAYMENT_CREATE", "WFP_PAYMENT_VIEW"}, az.actions)
}

// ── create ───────────────────────────────────────────────────────────────────

func TestWFP07_CreateNetPayResolvesEverythingFromItsAuthorities(t *testing.T) {
	st := payFixture()
	r := newPayRig(st, &stubAuthZ{})
	rr := accDo(r.router, "POST", payBase, payCreateBody(st), "p-1", "X-Correlation-ID", "corr-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	in := st.created
	require.NotNil(t, in)
	assert.Equal(t, domain.PayTypeNetPay, in.InstructionType)
	assert.Equal(t, "2400.4000", in.Amount, "the accepted net pay, never a caller figure")
	assert.Equal(t, "GBP", in.Currency)
	assert.Equal(t, "payee-1", in.PayeeRef, "from the worker's verified financial profile")
	assert.Equal(t, "dest-1", in.DestinationID)
	assert.Equal(t, "v1", in.DestinationVersion)
	assert.Equal(t, "bank-fp", in.DestinationFingerprint)
	assert.Equal(t, "le-OF-REF", in.LegalEntityID, "the entity of the stored run reference")
	assert.Equal(t, "tenant-abc", in.TenantID, "the authenticated tenant")
	assert.Equal(t, "p-1", in.CreatedBy)
	assert.Equal(t, "Payroll 2026-09", in.PaymentReference)
	assert.Equal(t, domain.ComputeInstructionFingerprint(in), in.Fingerprint)
	assert.Equal(t, []string{"p-1/acct-1"}, r.payers.got, "the paying account was verified with the caller's identity")
	assert.Equal(t, []string{"payee-1"}, r.payees.got)

	// An existing instruction (the same worker's net pay) answers 200.
	st.createdNew = false
	assert.Equal(t, http.StatusOK, accDo(r.router, "POST", payBase, payCreateBody(st), "p-1").Code)
}

func TestWFP07_CreateLiabilityDefaultsToWhatIsStillUnreserved(t *testing.T) {
	st := payFixture()
	r := newPayRig(st, &stubAuthZ{})
	body := map[string]any{"run_ref_id": st.ref.RunRefID, "instruction_type": "LIABILITY", "liability_id": st.liability.LiabilityID, "payer_account_ref": "acct-1", "value_date": "2026-10-20"}
	rr := accDo(r.router, "POST", payBase, body, "p-1")
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	assert.Equal(t, "500.1000", st.created.Amount, "outstanding 600.10 less 100.00 already covered by live instructions")
	assert.Equal(t, []string{"cp-hmrc"}, r.payees.got, "the beneficiary is the authority counterparty's destination")
	assert.Equal(t, "Payroll liability PAYE 2026-09", st.created.PaymentReference)

	body["amount"] = "50"
	require.Equal(t, http.StatusCreated, accDo(r.router, "POST", payBase, body, "p-1").Code)
	assert.Equal(t, "50", st.created.Amount, "an explicit partial amount")

	st.reserved = "600.1000"
	delete(body, "amount")
	rr = accDo(r.router, "POST", payBase, body, "p-1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "nothing left to instruct")
	assert.Equal(t, "UNPROCESSABLE", accCode(t, rr))

	st.reserved = "0.0000"
	st.liability.RunRefID = uuid.NewString()
	assert.Equal(t, http.StatusConflict, accDo(r.router, "POST", payBase, body, "p-1").Code, "another result's liability")
}

func TestWFP07_CreateRefusesWhatCannotBePaid(t *testing.T) {
	cases := map[string]struct {
		mutate func(*payRig)
		status int
		code   string
	}{
		"run superseded":     {func(g *payRig) { g.st.ref.AcceptanceState = domain.AcceptanceSuperseded }, http.StatusConflict, "INVALID_STATE"},
		"result rejected":    {func(g *payRig) { g.st.result.Outcome = domain.ResultRejected }, http.StatusConflict, "INVALID_STATE"},
		"unknown run":        {func(g *payRig) { g.st.ref = nil }, http.StatusNotFound, "not_found"},
		"worker not in run":  {func(g *payRig) { g.st.workers = nil }, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		"zero net pay":       {func(g *payRig) { g.st.workers[0].NetPay = "0.0000" }, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		"no verified payee":  {func(g *payRig) { g.workers.err = domain.ErrPayeeMissing }, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		"payee master down":  {func(g *payRig) { g.payees.err = domain.ErrPayeeUnavailable }, http.StatusServiceUnavailable, "dependency_unavailable"},
		"no active payee":    {func(g *payRig) { g.payees.err = domain.ErrNoActivePayee }, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		"payee currency":     {func(g *payRig) { g.payees.dest.Currency = "USD" }, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		"payee entity":       {func(g *payRig) { g.payees.dest.LegalEntityID = "le-OTHER" }, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		"profile service":    {func(g *payRig) { g.workers.err = domain.ErrWorkerProfileUnavailable }, http.StatusServiceUnavailable, "dependency_unavailable"},
		"bad paying account": {func(g *payRig) { g.payers.err = domain.ErrPayerAccountInvalid }, http.StatusUnprocessableEntity, "UNPROCESSABLE"},
		"treasury down":      {func(g *payRig) { g.payers.err = domain.ErrPayerUnavailable }, http.StatusServiceUnavailable, "dependency_unavailable"},
	}
	for name, c := range cases {
		st := payFixture()
		g := newPayRig(st, &stubAuthZ{})
		body := payCreateBody(st)
		c.mutate(g)
		rr := accDo(g.router, "POST", payBase, body, "p-1")
		assert.Equal(t, c.status, rr.Code, name)
		assert.Equal(t, c.code, accCode(t, rr), name)
		assert.Nil(t, g.st.created, "%s: nothing is stored", name)
	}
}

// ── validate ─────────────────────────────────────────────────────────────────

func TestWFP07_ValidateRepinsThePayeeAndAsksPolicyForTheQuorum(t *testing.T) {
	st := payFixture()
	instrFor(st, domain.PayDraft)
	g := newPayRig(st, &stubAuthZ{})
	path := payBase + "/" + st.instr.InstructionID + "/validate"

	assert.Equal(t, http.StatusBadRequest, accDo(g.router, "POST", path, map[string]any{}, "p-1").Code, "a version is required")
	g.payees.dest.Version = "v2" // the payee's destination changed since creation
	rr := accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-1")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NotNil(t, st.validated)
	assert.Equal(t, "v2", st.validated.Payee.Version, "validation pins the destination as it is now")
	assert.Equal(t, 1, st.validated.RequiredSignatures)
	assert.Equal(t, 2400.4, g.policy.amount)

	g.policy.result = domain.PolicyApproval
	rr = accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 2, st.validated.RequiredSignatures, "high value: the configured quorum")
	assert.Equal(t, domain.PolicyApproval, st.validated.PolicyResult)

	g.policy.err = domain.ErrPolicyUnavailable
	st.validated = nil
	assert.Equal(t, http.StatusServiceUnavailable, accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-1").Code, "an unreachable policy blocks validation")
	assert.Nil(t, st.validated)

	g.policy.err = nil
	st.instr.State = domain.PayValidated
	assert.Equal(t, http.StatusConflict, accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-1").Code, "only a DRAFT validates")
	st.instr.State = domain.PayDraft
	st.ref.AcceptanceState = domain.AcceptanceSuperseded
	assert.Equal(t, http.StatusConflict, accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-1").Code)
	st.ref.AcceptanceState = domain.AcceptanceAccepted
	g.payers.err = domain.ErrPayerAccountInvalid
	assert.Equal(t, http.StatusUnprocessableEntity, accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-1").Code)
}

// ── approve ──────────────────────────────────────────────────────────────────

func TestWFP07_ApproveUsesTheHighValueActionWhenPolicySaysSo(t *testing.T) {
	st := payFixture()
	i := instrFor(st, domain.PayValidated)
	az := &entityAuthZ{}
	g := newPayRig(st, az)
	path := payBase + "/" + i.InstructionID + "/approve"

	require.Equal(t, http.StatusOK, accDo(g.router, "POST", path, map[string]any{"fingerprint": i.Fingerprint}, "approver-1").Code)
	assert.Equal(t, "approver-1", st.approvedBy)
	assert.Equal(t, []string{"WFP_PAYMENT_APPROVE"}, az.actions)

	hv := domain.PolicyApproval
	i.PolicyResult = &hv
	az.actions = nil
	require.Equal(t, http.StatusOK, accDo(g.router, "POST", path, map[string]any{"fingerprint": i.Fingerprint}, "approver-1").Code)
	assert.Equal(t, []string{"WFP_PAYMENT_APPROVE_HIGHVALUE"}, az.actions, "a high-value payment needs the high-value signing authority")
}

func TestWFP07_ApproveEnforcesSeparationOfDutiesAndWhatTheApproverSaw(t *testing.T) {
	st := payFixture()
	i := instrFor(st, domain.PayValidated)
	g := newPayRig(st, &stubAuthZ{})
	path := payBase + "/" + i.InstructionID + "/approve"
	body := map[string]any{"fingerprint": i.Fingerprint}

	assert.Equal(t, http.StatusBadRequest, accDo(g.router, "POST", path, map[string]any{}, "a-1").Code, "approve what you saw")
	for _, who := range []string{"prep-1", "recv-1"} {
		rr := accDo(g.router, "POST", path, body, who)
		assert.Equal(t, http.StatusForbidden, rr.Code, who)
		assert.Equal(t, "SOD_VIOLATION", accCode(t, rr), who)
	}
	g.payees.dest.ChangedBy = []string{"payee-maker"}
	rr := accDo(g.router, "POST", path, body, "payee-maker")
	assert.Equal(t, http.StatusForbidden, rr.Code, "someone who changed the payee's bank destination cannot approve a payment to it")
	assert.Equal(t, "SOD_VIOLATION", accCode(t, rr))
	assert.Equal(t, "", st.approvedBy)

	rr = accDo(g.router, "POST", path, map[string]any{"fingerprint": "sha256:stale"}, "a-1")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "APPROVAL_INVALIDATED", accCode(t, rr))

	st.instr.State = domain.PayDraft
	assert.Equal(t, http.StatusConflict, accDo(g.router, "POST", path, body, "a-1").Code, "only a VALIDATED instruction is approved")
	assert.Equal(t, "", st.approvedBy)
}

func TestWFP07_ApproveInvalidatesInsteadWhenTheProtectedSubjectChanged(t *testing.T) {
	for name, mutate := range map[string]func(*payRig){
		"payee destination version": func(g *payRig) { g.payees.dest.Version = "v2" },
		"bank details digest":       func(g *payRig) { g.payees.dest.Fingerprint = "other" },
		"destination replaced":      func(g *payRig) { g.payees.dest.DestinationID = "dest-2" },
		"worker payee changed":      func(g *payRig) { g.workers.ref = "payee-2"; g.payees.dest.PayeeRef = "payee-2" },
		"paying account unusable":   func(g *payRig) { g.payers.err = domain.ErrPayerAccountInvalid },
	} {
		st := payFixture()
		i := instrFor(st, domain.PayValidated)
		g := newPayRig(st, &stubAuthZ{})
		mutate(g)
		rr := accDo(g.router, "POST", payBase+"/"+i.InstructionID+"/approve", map[string]any{"fingerprint": i.Fingerprint}, "a-1")
		assert.Equal(t, http.StatusConflict, rr.Code, name)
		assert.Equal(t, "APPROVAL_INVALIDATED", accCode(t, rr), name)
		assert.Equal(t, "PROTECTED_SUBJECT_CHANGED", st.invalidated, name)
		assert.Equal(t, "", st.approvedBy, "%s: nothing is approved", name)
	}

	// A dependency that cannot answer is not "unchanged".
	st := payFixture()
	i := instrFor(st, domain.PayValidated)
	g := newPayRig(st, &stubAuthZ{})
	g.payees.err = domain.ErrPayeeUnavailable
	rr := accDo(g.router, "POST", payBase+"/"+i.InstructionID+"/approve", map[string]any{"fingerprint": i.Fingerprint}, "a-1")
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "", st.invalidated, "unknown is neither approved nor invalidated")
	assert.Equal(t, "", st.approvedBy)
}

func TestWFP07_ADisputedLiabilityIsNotApprovedOrSubmitted(t *testing.T) {
	st := payFixture()
	i := instrFor(st, domain.PayValidated)
	lid := st.liability.LiabilityID
	i.InstructionType, i.WorkerID, i.LiabilityID = domain.PayTypeLiab, nil, &lid
	i.Fingerprint = domain.ComputeInstructionFingerprint(i)
	st.liability.ExceptionState = domain.LiabExcDisputed
	g := newPayRig(st, &stubAuthZ{})
	g.payees.dest.PayeeRef = "cp-hmrc"
	i.PayeeRef = "cp-hmrc"
	i.Fingerprint = domain.ComputeInstructionFingerprint(i)
	rr := accDo(g.router, "POST", payBase+"/"+i.InstructionID+"/approve", map[string]any{"fingerprint": i.Fingerprint}, "a-1")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "", st.approvedBy)
}

// ── submit ───────────────────────────────────────────────────────────────────

func TestWFP07_SubmitHandsAnApprovedInstructionToBankingAndRecordsTheAttempt(t *testing.T) {
	st := payFixture()
	i := instrFor(st, domain.PayApproved)
	g := newPayRig(st, &stubAuthZ{})
	path := payBase + "/" + i.InstructionID + "/submit"

	assert.Equal(t, http.StatusBadRequest, accDo(g.router, "POST", path, map[string]any{}, "p-9").Code, "a version is required")
	rr := accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-9")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Len(t, g.bank.reqs, 1)
	req := g.bank.reqs[0]
	assert.Equal(t, i.InstructionID, req.InstructionID)
	assert.Equal(t, *i.ApprovedFingerprint, req.Fingerprint, "Banking is handed the APPROVED fingerprint")
	assert.Equal(t, "payee-1", req.PayeeRef)
	assert.Equal(t, 2400.4, req.Amount)
	assert.Equal(t, "p-9", req.PrincipalID)
	assert.Equal(t, [3]string{"att-1", "SUBMITTED", ""}, st.outcome)
	assert.Contains(t, rr.Body.String(), domain.PayHandedOff)
}

func TestWFP07_ABankingTimeoutLeavesTheInstructionPendingNotFailed(t *testing.T) {
	st := payFixture()
	i := instrFor(st, domain.PayApproved)
	g := newPayRig(st, &stubAuthZ{})
	path := payBase + "/" + i.InstructionID + "/submit"

	g.bank.attempt, g.bank.err = nil, domain.ErrHandoffUnavailable
	rr := accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-9")
	assert.Equal(t, http.StatusAccepted, rr.Code)
	assert.Equal(t, "UNKNOWN", st.outcome[1], "unknown, never failed and never paid")
	assert.Contains(t, rr.Body.String(), domain.PaySubmitted)
	assert.Equal(t, "", st.released)

	// Banking has the attempt but it is not confirmed submitted: still pending.
	g.bank.attempt, g.bank.err = &paydeps.Attempt{AttemptID: "att-1", Status: "PENDING_UNKNOWN"}, nil
	rr = accDo(g.router, "POST", path, map[string]any{"expected_version": 3}, "p-9")
	assert.Equal(t, http.StatusAccepted, rr.Code)
	assert.Equal(t, [3]string{"att-1", "PENDING_UNKNOWN", ""}, st.outcome)
}

func TestWFP07_ARefusalBeforeAnAttemptReleasesTheInstruction(t *testing.T) {
	st := payFixture()
	i := instrFor(st, domain.PayApproved)
	g := newPayRig(st, &stubAuthZ{})
	g.bank.attempt, g.bank.err = nil, &paydeps.ErrRefused{Reason: "payer account is not ACTIVE"}
	rr := accDo(g.router, "POST", payBase+"/"+i.InstructionID+"/submit", map[string]any{"expected_version": 3}, "p-9")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
	assert.Equal(t, "BANKING_REFUSED", accCode(t, rr))
	assert.Equal(t, "payer account is not ACTIVE", st.released)
}

func TestWFP07_SubmitChecksTheApprovalStillAppliesAndRetriesSafely(t *testing.T) {
	// The payee changed after approval: the approval is invalidated and nothing is sent.
	st := payFixture()
	i := instrFor(st, domain.PayApproved)
	g := newPayRig(st, &stubAuthZ{})
	g.payees.dest.Version = "v2"
	rr := accDo(g.router, "POST", payBase+"/"+i.InstructionID+"/submit", map[string]any{"expected_version": 3}, "p-9")
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Equal(t, "APPROVAL_INVALIDATED", accCode(t, rr))
	assert.Equal(t, "PROTECTED_SUBJECT_CHANGED", st.invalidated)
	assert.Empty(t, g.bank.reqs)
	assert.Zero(t, st.beginCalls)

	// Not approved: refused.
	st = payFixture()
	i = instrFor(st, domain.PayValidated)
	g = newPayRig(st, &stubAuthZ{})
	assert.Equal(t, http.StatusConflict, accDo(g.router, "POST", payBase+"/"+i.InstructionID+"/submit", map[string]any{"expected_version": 3}, "p-9").Code)
	assert.Empty(t, g.bank.reqs)

	// A retry of an already-submitted instruction needs no version, skips the drift check, and reuses the attempt.
	st = payFixture()
	i = instrFor(st, domain.PaySubmitted)
	g = newPayRig(st, &stubAuthZ{})
	g.payees.dest.Version = "v-later"
	rr = accDo(g.router, "POST", payBase+"/"+i.InstructionID+"/submit", map[string]any{}, "p-9")
	assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Len(t, g.bank.reqs, 1)
	assert.Equal(t, i.InstructionID, g.bank.reqs[0].InstructionID, "same instruction, same deterministic Banking key")

	// Already handed off: answered as is, Banking is not called again.
	st = payFixture()
	i = instrFor(st, domain.PayHandedOff)
	g = newPayRig(st, &stubAuthZ{})
	assert.Equal(t, http.StatusOK, accDo(g.router, "POST", payBase+"/"+i.InstructionID+"/submit", map[string]any{}, "p-9").Code)
	assert.Empty(t, g.bank.reqs)
}

// ── invalidate / cancel / supersede / reads ──────────────────────────────────

func TestWFP07_InvalidateCancelAndSupersedeRequireTheirReasons(t *testing.T) {
	st := payFixture()
	i := instrFor(st, domain.PayApproved)
	g := newPayRig(st, &stubAuthZ{})
	base := payBase + "/" + i.InstructionID

	assert.Equal(t, http.StatusBadRequest, accDo(g.router, "POST", base+"/invalidate", map[string]any{}, "p-1").Code)
	require.Equal(t, http.StatusOK, accDo(g.router, "POST", base+"/invalidate", map[string]any{"reason": "payee called"}, "p-1").Code)
	assert.Equal(t, "payee called", st.invalidated)

	assert.Equal(t, http.StatusBadRequest, accDo(g.router, "POST", base+"/cancel", map[string]any{"expected_version": 3}, "p-1").Code, "a reason is required")
	assert.Equal(t, http.StatusBadRequest, accDo(g.router, "POST", base+"/cancel", map[string]any{"reason": "x"}, "p-1").Code, "and a version")
	require.Equal(t, http.StatusOK, accDo(g.router, "POST", base+"/cancel", map[string]any{"expected_version": 3, "reason": "duplicate"}, "p-1").Code)
	assert.Equal(t, "duplicate", st.cancelledFor)

	st.err = domain.ErrInstructionState
	assert.Equal(t, http.StatusConflict, accDo(g.router, "POST", base+"/cancel", map[string]any{"expected_version": 3, "reason": "x"}, "p-1").Code)
	st.err = nil

	assert.Equal(t, http.StatusBadRequest, accDo(g.router, "POST", payBase+"/supersede", map[string]any{"run_ref_id": "bad"}, "p-1").Code)
	assert.Equal(t, http.StatusOK, accDo(g.router, "POST", payBase+"/supersede", map[string]any{"run_ref_id": st.ref.RunRefID}, "p-1").Code)
}

func TestWFP07_ReadsAndTheAdaptersFingerprintLookup(t *testing.T) {
	st := payFixture()
	i := instrFor(st, domain.PayApproved)
	g := newPayRig(st, &stubAuthZ{})

	assert.Equal(t, http.StatusBadRequest, accDo(g.router, "GET", payBase, nil, "p-1").Code, "a list needs an entity or run reference")
	assert.Equal(t, http.StatusOK, accDo(g.router, "GET", payBase+"?legal_entity_id=le-1&state=APPROVED", nil, "p-1").Code)
	assert.Equal(t, http.StatusOK, accDo(g.router, "GET", payBase+"/pending?run_ref_id="+st.ref.RunRefID, nil, "p-1").Code)
	assert.Equal(t, http.StatusOK, accDo(g.router, "GET", payBase+"/"+i.InstructionID+"/approval-fingerprint", nil, "p-1").Code)
	rr := accDo(g.router, "GET", payBase+"/"+i.InstructionID+"/banking-status", nil, "p-1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "payment-status-svc", "settlement and finality are not read from this record")

	// The adapter's lookup is service to service: tenant only, no principal, read-only.
	rr = accDo(g.router, "GET", payBase+"/"+i.InstructionID+"/fingerprint", nil, "")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), *i.ApprovedFingerprint)
	assert.Contains(t, rr.Body.String(), domain.PayApproved)
	st.instr.State, st.instr.ApprovedFingerprint = domain.PayValidated, nil
	rr = accDo(g.router, "GET", payBase+"/"+i.InstructionID+"/fingerprint", nil, "")
	assert.Contains(t, rr.Body.String(), `"fingerprint":""`, "an unapproved instruction exposes no approved fingerprint")
	st.instr = nil
	assert.Equal(t, http.StatusNotFound, accDo(g.router, "GET", payBase+"/"+uuid.NewString()+"/fingerprint", nil, "").Code)
}
