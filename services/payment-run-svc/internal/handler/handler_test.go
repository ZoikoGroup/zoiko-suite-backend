package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payment-run-svc/internal/authz"
	"zoiko.io/payment-run-svc/internal/domain"
	"zoiko.io/payment-run-svc/internal/handler"
	"zoiko.io/payment-run-svc/internal/middleware"
	"zoiko.io/payment-run-svc/internal/payableopenitem"
	"zoiko.io/payment-run-svc/internal/paymentauthorization"
	"zoiko.io/payment-run-svc/internal/paymentproposal"
	"zoiko.io/payment-run-svc/internal/paymentstatus"
	"zoiko.io/payment-run-svc/internal/provideradapter"
	"zoiko.io/payment-run-svc/internal/store"
)

const testTenant = "tenant-ap11-1"
const testLegalEntity = "le-ap11-1"
const testAccount = "bank-acct-1"
const testMethod = "ACH"

var testValueDate = time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

// ── stub authz ───────────────────────────────────────────────────────────────

type stubAuthz struct{ deny bool }

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── stub payment-authorization-svc (AP-10) + payment-proposal-svc (AP-09) ───

type stubAuth struct {
	auths        map[string]paymentauthorization.Authorization
	validity     map[string]bool
	consumeFails map[string]bool
	consumeCalls map[string]int
}

func (a *stubAuth) GetApprovedAuthorization(_ context.Context, tenantID, legalEntityID, authorizationID string) (*paymentauthorization.Authorization, error) {
	auth, ok := a.auths[authorizationID]
	if !ok || (auth.TenantID != nil && *auth.TenantID != tenantID) || auth.LegalEntityID != legalEntityID || auth.Status != "APPROVED" {
		return nil, domain.ErrAuthorizationNotEligible
	}
	return &auth, nil
}

func (a *stubAuth) ValidateAuthorization(_ context.Context, _, authorizationID string) (bool, error) {
	return a.validity[authorizationID], nil
}

func (a *stubAuth) ConsumeAuthorization(_ context.Context, _, _, authorizationID string) error {
	a.consumeCalls[authorizationID]++
	if a.consumeFails[authorizationID] || a.consumeCalls[authorizationID] > 1 {
		return domain.ErrAuthorizationConsumeFailed // single-use, like the real AP-10
	}
	return nil
}

var _ paymentauthorization.Client = (*stubAuth)(nil)

type stubProposals struct {
	props map[string]*paymentproposal.Proposal
}

func (p *stubProposals) GetFrozenProposal(_ context.Context, _, _, proposalID string) (*paymentproposal.Proposal, error) {
	prop, ok := p.props[proposalID]
	if !ok {
		return nil, domain.ErrAuthorizedSubjectMismatch
	}
	cp := *prop
	return &cp, nil
}

var _ paymentproposal.Client = (*stubProposals)(nil)

// ── stub BNK-06 ─────────────────────────────────────────────────────────────

// stubProvider models BNK-06 faithfully enough to test the UNKNOWN paths:
// Prepare is idempotent on IdempotencyKey, Submit only works on PREPARED,
// Retry only on PENDING_UNKNOWN, and a submit "timeout" leaves the bank-side
// attempt PENDING_UNKNOWN while the caller gets an error.
type stubProvider struct {
	attempts     map[string]*provideradapter.Attempt
	byKey        map[string]string
	prepareErr   error
	submitErr    error
	submitStatus string
	retryErr     error
	retryStatus  string
	prepareCalls int
	submitCalls  int
	retryCalls   int
	prepared     []provideradapter.PrepareRequest
}

func newStubProvider() *stubProvider {
	return &stubProvider{attempts: map[string]*provideradapter.Attempt{}, byKey: map[string]string{}}
}

func (p *stubProvider) Prepare(_ context.Context, _, _ string, req provideradapter.PrepareRequest) (*provideradapter.Attempt, error) {
	p.prepareCalls++
	if p.prepareErr != nil {
		return nil, p.prepareErr
	}
	if id, ok := p.byKey[req.IdempotencyKey]; ok {
		cp := *p.attempts[id]
		return &cp, nil
	}
	p.prepared = append(p.prepared, req)
	a := &provideradapter.Attempt{AttemptID: "attempt-" + req.SourceReference, Status: provideradapter.AttemptPrepared}
	p.attempts[a.AttemptID] = a
	p.byKey[req.IdempotencyKey] = a.AttemptID
	cp := *a
	return &cp, nil
}

func (p *stubProvider) Submit(_ context.Context, _, _, attemptID string) (*provideradapter.Attempt, error) {
	p.submitCalls++
	a := p.attempts[attemptID]
	if a.Status != provideradapter.AttemptPrepared {
		return nil, domain.ErrBankingAttemptConflict
	}
	if p.submitErr != nil {
		a.Status = provideradapter.AttemptPendingUnknown
		return nil, p.submitErr
	}
	a.Status = provideradapter.AttemptSubmitted
	if p.submitStatus != "" {
		a.Status = p.submitStatus
	}
	a.ProviderRequestID = "preq-" + attemptID
	cp := *a
	return &cp, nil
}

func (p *stubProvider) Retry(_ context.Context, _, _, attemptID string) (*provideradapter.Attempt, error) {
	p.retryCalls++
	a := p.attempts[attemptID]
	if a.Status != provideradapter.AttemptPendingUnknown {
		return nil, domain.ErrBankingAttemptConflict
	}
	if p.retryErr != nil {
		return nil, p.retryErr
	}
	a.Status = provideradapter.AttemptSubmitted
	if p.retryStatus != "" {
		a.Status = p.retryStatus
	}
	a.ProviderRequestID = "preq-" + attemptID
	cp := *a
	return &cp, nil
}

func (p *stubProvider) GetAttempt(_ context.Context, _, _, attemptID string) (*provideradapter.Attempt, error) {
	a, ok := p.attempts[attemptID]
	if !ok {
		return nil, domain.ErrProviderAdapterUnavailable
	}
	cp := *a
	return &cp, nil
}

var _ provideradapter.Client = (*stubProvider)(nil)

// ── stub BNK-07 ─────────────────────────────────────────────────────────────

type stubStatus struct {
	fail      bool
	statuses  map[string]string // payment_id -> Status
	conflicts map[string]bool
	calls     int
}

func newStubStatus() *stubStatus {
	return &stubStatus{statuses: map[string]string{}, conflicts: map[string]bool{}}
}

func (s *stubStatus) RecordInitialStatus(_ context.Context, _, _ string, req paymentstatus.RecordInitialStatusRequest) (*paymentstatus.PaymentState, error) {
	s.calls++
	if s.fail {
		return nil, domain.ErrPaymentStatusUnavailable
	}
	id := "payment-" + req.SourceReference
	s.statuses[id] = "PREPARED"
	return &paymentstatus.PaymentState{PaymentID: id, Status: "PREPARED"}, nil
}

func (s *stubStatus) GetStatus(_ context.Context, _, paymentID string) (*paymentstatus.PaymentState, error) {
	status, ok := s.statuses[paymentID]
	if !ok {
		return nil, domain.ErrPaymentStatusUnavailable
	}
	return &paymentstatus.PaymentState{PaymentID: paymentID, Status: status, HasOpenConflict: s.conflicts[paymentID]}, nil
}

var _ paymentstatus.Client = (*stubStatus)(nil)

// ── stub AP-08 ──────────────────────────────────────────────────────────────

type stubPayables struct {
	fail    bool
	applied []payableopenitem.ApplyRequest
}

func (p *stubPayables) ApplyConfirmedPayment(_ context.Context, _, _ string, req payableopenitem.ApplyRequest) error {
	if p.fail {
		return domain.ErrPayableServiceUnavailable
	}
	p.applied = append(p.applied, req)
	return nil
}

var _ payableopenitem.Client = (*stubPayables)(nil)

// ── test harness ─────────────────────────────────────────────────────────────

type env struct {
	store     *stubStore
	authz     *stubAuthz
	auth      *stubAuth
	proposals *stubProposals
	provider  *stubProvider
	status    *stubStatus
	payables  *stubPayables
	router    chi.Router
}

func newEnv() *env {
	e := &env{
		store: newStubStore(),
		authz: &stubAuthz{},
		auth: &stubAuth{auths: map[string]paymentauthorization.Authorization{}, validity: map[string]bool{},
			consumeFails: map[string]bool{}, consumeCalls: map[string]int{}},
		proposals: &stubProposals{props: map[string]*paymentproposal.Proposal{}},
		provider:  newStubProvider(),
		status:    newStubStatus(),
		payables:  &stubPayables{},
	}
	h := handler.New(e.store, e.authz, handler.Clients{
		Authorization: e.auth, Proposal: e.proposals, Payables: e.payables, Provider: e.provider, Status: e.status,
	}, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	e.router = r
	return e
}

// payeeLine is one AP-09 proposal item: payee, net and withholding.
type payeeLine struct {
	payee       string
	net         float64
	withholding float64
}

// addAuthorization registers an APPROVED AP-10 authorization over a FROZEN
// AP-09 proposal containing lines, with matching fingerprint and totals.
func (e *env) addAuthorization(id, tenantID string, lines ...payeeLine) {
	tid := tenantID
	var total float64
	prop := &paymentproposal.Proposal{
		ProposalID: "prop-" + id, LegalEntityID: testLegalEntity, PayingBankAccountRef: testAccount,
		Currency: "USD", PaymentDate: testValueDate, PaymentMethod: testMethod, Status: "FROZEN",
		Fingerprint: "fp-" + id,
	}
	for n, l := range lines {
		prop.Items = append(prop.Items, paymentproposal.Item{
			PayableSource: "AP_INVOICE", PayableID: id + "-inv-" + string(rune('a'+n)), PayeeRef: l.payee,
			GrossAmount: l.net + l.withholding, WithholdingAmount: l.withholding, NetAmount: l.net,
			Currency: "USD", IsActive: true,
		})
		total += l.net
	}
	prop.NetAmount = total
	e.proposals.props[prop.ProposalID] = prop
	e.auth.auths[id] = paymentauthorization.Authorization{
		AuthorizationID: id, TenantID: &tid, LegalEntityID: testLegalEntity, ProposalID: prop.ProposalID,
		NetAmount: total, Currency: "USD", Status: "APPROVED", ProposalFingerprint: "fp-" + id,
	}
	e.auth.validity[id] = true
}

// add is the common single-payee case.
func (e *env) add(id string, net float64) {
	e.addAuthorization(id, testTenant, payeeLine{payee: "payee-" + id, net: net})
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "principal-operator")
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func runRequest(authIDs ...string) domain.CreateRunRequest {
	return domain.CreateRunRequest{
		LegalEntityID: testLegalEntity, PayingBankAccountRef: testAccount, Currency: "USD",
		ValueDate: testValueDate, PaymentMethod: testMethod, AuthorizationIDs: authIDs,
	}
}

func (e *env) createRun(t *testing.T, authIDs ...string) *domain.PaymentRun {
	t.Helper()
	w := doRequest(e.router, http.MethodPost, "/ap11/runs/", runRequest(authIDs...), testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createRun: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Run domain.PaymentRun `json:"run"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return &resp.Run
}

func (e *env) validateAndLock(t *testing.T, runID string) *httptest.ResponseRecorder {
	t.Helper()
	w := doRequest(e.router, http.MethodPost, "/ap11/runs/"+runID+"/validate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	return doRequest(e.router, http.MethodPost, "/ap11/runs/"+runID+"/lock", nil, testTenant)
}

func (e *env) submit(runID, key string) *httptest.ResponseRecorder {
	return doRequest(e.router, http.MethodPost, "/ap11/runs/"+runID+"/submit", domain.SubmitRunRequest{IdempotencyKey: key}, testTenant)
}

// lockedRun creates, validates and locks a run over the given authorizations.
func (e *env) lockedRun(t *testing.T, authIDs ...string) *domain.PaymentRun {
	t.Helper()
	run := e.createRun(t, authIDs...)
	if w := e.validateAndLock(t, run.RunID); w.Code != http.StatusOK {
		t.Fatalf("lock: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	return run
}

func (e *env) runStatus(t *testing.T, runID string) domain.RunStatus {
	t.Helper()
	w := doRequest(e.router, http.MethodGet, "/ap11/runs/"+runID, nil, testTenant)
	var resp struct {
		Run domain.PaymentRun `json:"run"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Run.Status
}

func (e *env) instructions(t *testing.T, runID string) []domain.RunInstruction {
	t.Helper()
	w := doRequest(e.router, http.MethodGet, "/ap11/runs/"+runID+"/instructions", nil, testTenant)
	var resp struct {
		Data []domain.RunInstruction `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) == 0 {
		t.Fatalf("expected at least one instruction")
	}
	return resp.Data
}

func (e *env) poll(t *testing.T, instructionID string) map[string]interface{} {
	t.Helper()
	w := doRequest(e.router, http.MethodPost, "/ap11/instructions/"+instructionID+"/poll", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("poll: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp
}

// ── creation ─────────────────────────────────────────────────────────────────

func TestCreateRun_Draft(t *testing.T) {
	e := newEnv()
	e.add("auth-1", 500)
	if run := e.createRun(t, "auth-1"); run.Status != domain.StatusDraft {
		t.Fatalf("expected DRAFT, got %s", run.Status)
	}
}

// TestCreateRun_MultiPayeeProposal_OneInstructionPerPayee is the fix for the
// whole proposal being sent to its first payee: each payee gets its own
// instruction with its own net total and payables.
func TestCreateRun_MultiPayeeProposal_OneInstructionPerPayee(t *testing.T) {
	e := newEnv()
	e.addAuthorization("auth-mp", testTenant,
		payeeLine{payee: "supplier-a", net: 100},
		payeeLine{payee: "supplier-b", net: 200, withholding: 20},
		payeeLine{payee: "supplier-a", net: 50.25},
	)
	run := e.createRun(t, "auth-mp")

	ins := e.instructions(t, run.RunID)
	if len(ins) != 2 {
		t.Fatalf("expected one instruction per payee (2), got %d", len(ins))
	}
	got := map[string]float64{}
	payables := map[string]int{}
	for _, i := range ins {
		got[i.PayeeRef] = i.NetAmount
		payables[i.PayeeRef] = len(i.Payables)
	}
	if got["supplier-a"] != 150.25 || got["supplier-b"] != 200 {
		t.Fatalf("expected supplier-a 150.25 and supplier-b 200, got %v", got)
	}
	if payables["supplier-a"] != 2 || payables["supplier-b"] != 1 {
		t.Fatalf("expected payables split 2/1, got %v", payables)
	}

	if w := e.validateAndLock(t, run.RunID); w.Code != http.StatusOK {
		t.Fatalf("lock: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if e.auth.consumeCalls["auth-mp"] != 1 {
		t.Fatalf("expected the shared authorization consumed exactly once, got %d", e.auth.consumeCalls["auth-mp"])
	}

	if w := e.submit(run.RunID, "idem-mp"); w.Code != http.StatusOK {
		t.Fatalf("submit: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	sent := map[string]float64{}
	for _, p := range e.provider.prepared {
		sent[p.PayeeRef] = p.Amount
	}
	if len(sent) != 2 || sent["supplier-a"] != 150.25 || sent["supplier-b"] != 200 {
		t.Fatalf("expected Banking to receive one payment per payee with its own amount, got %v", sent)
	}
}

func TestCreateRun_SubjectMismatch_Rejected(t *testing.T) {
	cases := map[string]func(e *env){
		"fingerprint differs": func(e *env) { e.proposals.props["prop-auth-x"].Fingerprint = "fp-changed" },
		"proposal not frozen": func(e *env) { e.proposals.props["prop-auth-x"].Status = "REVIEW" },
		"net total differs": func(e *env) {
			a := e.auth.auths["auth-x"]
			a.NetAmount = 999
			e.auth.auths["auth-x"] = a
		},
		"run changes paying account": func(e *env) { e.proposals.props["prop-auth-x"].PayingBankAccountRef = "other-acct" },
		"run changes value date":     func(e *env) { e.proposals.props["prop-auth-x"].PaymentDate = testValueDate.Add(48 * time.Hour) },
		"run changes method":         func(e *env) { e.proposals.props["prop-auth-x"].PaymentMethod = "WIRE" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			e.add("auth-x", 100)
			mutate(e)
			w := doRequest(e.router, http.MethodPost, "/ap11/runs/", runRequest("auth-x"), testTenant)
			if w.Code != http.StatusConflict {
				t.Fatalf("expected 409 subject mismatch, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestCreateRun_CrossTenantAuthorization_Rejected is negative-path #4.
func TestCreateRun_CrossTenantAuthorization_Rejected(t *testing.T) {
	e := newEnv()
	e.addAuthorization("auth-2", "some-other-tenant", payeeLine{payee: "p", net: 200})
	w := doRequest(e.router, http.MethodPost, "/ap11/runs/", runRequest("auth-2"), testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 cross-tenant rejected, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateRun_AuthorizationAlreadyInAnotherRun_Rejected(t *testing.T) {
	e := newEnv()
	e.add("auth-3", 300)
	e.createRun(t, "auth-3")
	w := doRequest(e.router, http.MethodPost, "/ap11/runs/", runRequest("auth-3"), testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 already in another run, got %d: %s", w.Code, w.Body.String())
	}
}

// ── lifecycle ────────────────────────────────────────────────────────────────

func TestValidatePaymentRun_NoLongerValid_Blocked(t *testing.T) {
	e := newEnv()
	e.add("auth-4", 100)
	e.auth.validity["auth-4"] = false
	run := e.createRun(t, "auth-4")
	w := doRequest(e.router, http.MethodPost, "/ap11/runs/"+run.RunID+"/validate", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 no longer valid, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLockPaymentRun_ConsumesAuthorizations(t *testing.T) {
	e := newEnv()
	e.add("auth-5", 100)
	run := e.lockedRun(t, "auth-5")
	if e.auth.consumeCalls["auth-5"] != 1 {
		t.Fatalf("expected exactly one Consume call, got %d", e.auth.consumeCalls["auth-5"])
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusLocked {
		t.Fatalf("expected LOCKED, got %s", s)
	}
}

func TestLockPaymentRun_ConsumeFails_RunMovesToException(t *testing.T) {
	e := newEnv()
	e.add("auth-6", 100)
	e.auth.consumeFails["auth-6"] = true
	run := e.createRun(t, "auth-6")
	if w := e.validateAndLock(t, run.RunID); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 consume failed, got %d: %s", w.Code, w.Body.String())
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusException {
		t.Fatalf("expected EXCEPTION after failed consume, got %s", s)
	}
}

func TestCancelUnsubmittedRun(t *testing.T) {
	e := newEnv()
	e.add("auth-11", 100)
	run := e.createRun(t, "auth-11")
	w := doRequest(e.router, http.MethodPost, "/ap11/runs/"+run.RunID+"/cancel", domain.CancelRunRequest{Reason: "duplicate run"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 cancelling, got %d: %s", w.Code, w.Body.String())
	}
}

// ── submission & UNKNOWN handling ────────────────────────────────────────────

func TestSubmitPaymentRun_HandsInstructionToBanking(t *testing.T) {
	e := newEnv()
	e.add("auth-14", 100)
	run := e.lockedRun(t, "auth-14")

	if w := e.submit(run.RunID, "idem-14"); w.Code != http.StatusOK {
		t.Fatalf("expected 200 submitting, got %d: %s", w.Code, w.Body.String())
	}
	if e.provider.prepareCalls != 1 || e.provider.submitCalls != 1 || e.status.calls != 1 {
		t.Fatalf("expected one prepare, one submit, one BNK-07 record; got %d/%d/%d", e.provider.prepareCalls, e.provider.submitCalls, e.status.calls)
	}
	ins := e.instructions(t, run.RunID)[0]
	if ins.ProviderAttemptID == "" || ins.Bnk07PaymentID == "" {
		t.Fatalf("expected Banking correlation ids, got %+v", ins)
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusSubmitted {
		t.Fatalf("expected SUBMITTED, got %s", s)
	}
}

func TestSubmitPaymentRun_CarriesAuthorizationFingerprintToBanking(t *testing.T) {
	e := newEnv()
	e.add("auth-fp-1", 100)
	run := e.lockedRun(t, "auth-fp-1")
	if w := e.submit(run.RunID, "idem-fp-1"); w.Code != http.StatusOK {
		t.Fatalf("expected 200 submitting, got %d: %s", w.Code, w.Body.String())
	}
	req := e.provider.prepared[0]
	if req.AuthorizationID != "auth-fp-1" || req.AuthorizationFingerprint != "fp-auth-fp-1" || req.AuthorizationSource != "PAYMENT_AUTHORIZATION_SVC" {
		t.Fatalf("expected the AP-10 fingerprint to reach BNK-06 unchanged, got %+v", req)
	}
}

// TestSubmitPaymentRun_ReplaySameKey_Idempotent is negative-path #1.
func TestSubmitPaymentRun_ReplaySameKey_Idempotent(t *testing.T) {
	e := newEnv()
	e.add("auth-7", 100)
	run := e.lockedRun(t, "auth-7")
	if w := e.submit(run.RunID, "idem-key-1"); w.Code != http.StatusOK {
		t.Fatalf("expected 200 first submit, got %d: %s", w.Code, w.Body.String())
	}
	if w := e.submit(run.RunID, "idem-key-1"); w.Code != http.StatusOK {
		t.Fatalf("expected 200 idempotent replay, got %d: %s", w.Code, w.Body.String())
	}
	if e.provider.submitCalls != 1 {
		t.Fatalf("expected the replay to send nothing new, got %d submits", e.provider.submitCalls)
	}
}

func TestSubmitPaymentRun_ReplayDifferentKey_Rejected(t *testing.T) {
	e := newEnv()
	e.add("auth-8", 100)
	run := e.lockedRun(t, "auth-8")
	e.submit(run.RunID, "idem-key-a")
	if w := e.submit(run.RunID, "idem-key-b"); w.Code != http.StatusConflict {
		t.Fatalf("expected 409 different key rejected, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSubmitPaymentRun_SubmitTimeout_PendingUnknownNotFailed is negative
// paths #32/#33: a provider timeout is UNKNOWN, not FAILED, and a replay
// never re-initiates the payment.
func TestSubmitPaymentRun_SubmitTimeout_PendingUnknownNotFailed(t *testing.T) {
	e := newEnv()
	e.add("auth-to", 100)
	e.provider.submitErr = domain.ErrProviderAdapterUnavailable
	run := e.lockedRun(t, "auth-to")

	if w := e.submit(run.RunID, "idem-to"); w.Code != http.StatusOK {
		t.Fatalf("expected 200 (outcome recorded as unknown), got %d: %s", w.Code, w.Body.String())
	}
	ins := e.instructions(t, run.RunID)[0]
	if ins.Status != domain.InstructionPendingUnknown || ins.ProviderAttemptID == "" {
		t.Fatalf("expected PENDING_UNKNOWN with the attempt id kept, got %+v", ins)
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusPendingUnknown {
		t.Fatalf("expected run PENDING_UNKNOWN, never EXCEPTION/FAILED, got %s", s)
	}

	e.provider.submitErr = nil
	if w := e.submit(run.RunID, "idem-to"); w.Code != http.StatusOK {
		t.Fatalf("expected 200 replay, got %d: %s", w.Code, w.Body.String())
	}
	if e.provider.submitCalls != 1 || len(e.provider.prepared) != 1 {
		t.Fatalf("replay after timeout must not re-initiate: %d submits, %d attempts", e.provider.submitCalls, len(e.provider.prepared))
	}
}

// TestSubmitPaymentRun_PrepareUnavailable_NothingSentRunStaysLocked: when
// BNK-06 cannot even prepare, nothing reached the bank, so the run stays
// LOCKED and the same submit can be replayed.
func TestSubmitPaymentRun_PrepareUnavailable_NothingSentRunStaysLocked(t *testing.T) {
	e := newEnv()
	e.add("auth-15", 100)
	e.provider.prepareErr = domain.ErrProviderAdapterUnavailable
	run := e.lockedRun(t, "auth-15")

	if w := e.submit(run.RunID, "idem-15"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusLocked {
		t.Fatalf("expected run to stay LOCKED, got %s", s)
	}
	if e.provider.submitCalls != 0 {
		t.Fatalf("expected nothing submitted, got %d", e.provider.submitCalls)
	}

	e.provider.prepareErr = nil
	if w := e.submit(run.RunID, "idem-15"); w.Code != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d: %s", w.Code, w.Body.String())
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusSubmitted {
		t.Fatalf("expected SUBMITTED after replay, got %s", s)
	}
}

func TestSubmitPaymentRun_PrepareRejected_InstructionException(t *testing.T) {
	e := newEnv()
	e.add("auth-rj", 100)
	e.provider.prepareErr = domain.ErrBankingPrepareRejected
	run := e.lockedRun(t, "auth-rj")

	if w := e.submit(run.RunID, "idem-rj"); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	ins := e.instructions(t, run.RunID)[0]
	if ins.Status != domain.InstructionException || ins.StatusReason == "" {
		t.Fatalf("expected EXCEPTION with a reason, got %+v", ins)
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusException {
		t.Fatalf("expected run EXCEPTION, got %s", s)
	}
}

// ── retry ────────────────────────────────────────────────────────────────────

func TestRetrySafeInstruction_ReusesSameAttempt(t *testing.T) {
	e := newEnv()
	e.add("auth-12", 100)
	e.provider.submitErr = domain.ErrProviderAdapterUnavailable
	run := e.lockedRun(t, "auth-12")
	e.submit(run.RunID, "idem-12")
	ins := e.instructions(t, run.RunID)[0]

	w := doRequest(e.router, http.MethodPost, "/ap11/instructions/"+ins.InstructionID+"/retry", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 retrying, got %d: %s", w.Code, w.Body.String())
	}
	if e.provider.retryCalls != 1 || len(e.provider.prepared) != 1 {
		t.Fatalf("expected one BNK-06 retry of the same attempt, got %d retries / %d attempts", e.provider.retryCalls, len(e.provider.prepared))
	}
	ins = e.instructions(t, run.RunID)[0]
	if ins.Status != domain.InstructionPending || ins.Bnk07PaymentID == "" {
		t.Fatalf("expected the retried attempt submitted and recorded with BNK-07, got %+v", ins)
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusSubmitted {
		t.Fatalf("expected run back to SUBMITTED, got %s", s)
	}
}

func TestRetrySafeInstruction_NotUnknown_Rejected(t *testing.T) {
	e := newEnv()
	e.add("auth-12b", 100)
	run := e.lockedRun(t, "auth-12b")
	e.submit(run.RunID, "idem-12b")
	ins := e.instructions(t, run.RunID)[0]

	w := doRequest(e.router, http.MethodPost, "/ap11/instructions/"+ins.InstructionID+"/retry", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a non-unknown instruction, got %d: %s", w.Code, w.Body.String())
	}
	if e.provider.retryCalls != 0 {
		t.Fatalf("expected no Banking retry, got %d", e.provider.retryCalls)
	}
}

// ── manual reconciliation ────────────────────────────────────────────────────

// TestReconcile_ManualSettled_Refused is negative-path #35: no settlement
// without Banking authority.
func TestReconcile_ManualSettled_Refused(t *testing.T) {
	for _, status := range []domain.InstructionStatus{domain.InstructionSettled, domain.InstructionAccepted, domain.InstructionRejected} {
		e := newEnv()
		e.add("auth-9", 100)
		run := e.lockedRun(t, "auth-9")
		e.submit(run.RunID, "idem-9")
		ins := e.instructions(t, run.RunID)[0]

		w := doRequest(e.router, http.MethodPost, "/ap11/instructions/"+ins.InstructionID+"/reconcile",
			domain.ReconcileInstructionRequest{ExternalStatus: status, ProviderEventRef: "evt-1", Reason: "operator says so"}, testTenant)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", status, w.Code, w.Body.String())
		}
		if len(e.payables.applied) != 0 {
			t.Fatalf("%s: no payable may be settled from a manual call", status)
		}
	}
}

func TestReconcile_ManualException_FlagsInstruction(t *testing.T) {
	e := newEnv()
	e.add("auth-10", 100)
	run := e.lockedRun(t, "auth-10")
	e.submit(run.RunID, "idem-10")
	ins := e.instructions(t, run.RunID)[0]

	req := domain.ReconcileInstructionRequest{ExternalStatus: domain.InstructionException, ProviderEventRef: "ticket-42", Reason: "bank says beneficiary name mismatch"}
	if w := doRequest(e.router, http.MethodPost, "/ap11/instructions/"+ins.InstructionID+"/reconcile", req, testTenant); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusException {
		t.Fatalf("expected run EXCEPTION, got %s", s)
	}

	w := doRequest(e.router, http.MethodPost, "/ap11/instructions/"+ins.InstructionID+"/reconcile", req, testTenant)
	var resp struct {
		Applied bool `json:"applied"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Applied {
		t.Fatalf("expected idempotent replay (200, applied=false), got %d %v", w.Code, resp.Applied)
	}

	missingReason := domain.ReconcileInstructionRequest{ExternalStatus: domain.InstructionException, ProviderEventRef: "ticket-43"}
	if w := doRequest(e.router, http.MethodPost, "/ap11/instructions/"+ins.InstructionID+"/reconcile", missingReason, testTenant); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a reason, got %d", w.Code)
	}
}

// ── polling & AP-08 settlement ───────────────────────────────────────────────

// TestPoll_Settled_SettlesPayablesInAP08 is the BNK-07 → AP-11 → AP-08 chain:
// a SETTLED payment settles each payable it pays, net plus withholding,
// keyed by BNK-07's payment id; a second poll applies nothing twice.
func TestPoll_Settled_SettlesPayablesInAP08(t *testing.T) {
	e := newEnv()
	e.addAuthorization("auth-16", testTenant,
		payeeLine{payee: "supplier-a", net: 90, withholding: 10},
		payeeLine{payee: "supplier-a", net: 40},
	)
	run := e.lockedRun(t, "auth-16")
	e.submit(run.RunID, "idem-16")
	ins := e.instructions(t, run.RunID)[0]
	e.status.statuses[ins.Bnk07PaymentID] = "SETTLED"

	resp := e.poll(t, ins.InstructionID)
	if resp["payables_pending_settlement"].(float64) != 0 {
		t.Fatalf("expected every payable settled in AP-08, got %v", resp)
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusSettled {
		t.Fatalf("expected run SETTLED, got %s", s)
	}
	if len(e.payables.applied) != 2 {
		t.Fatalf("expected 2 AP-08 applications, got %d", len(e.payables.applied))
	}
	sort.Slice(e.payables.applied, func(i, j int) bool { return e.payables.applied[i].NetAmount > e.payables.applied[j].NetAmount })
	first := e.payables.applied[0]
	if first.NetAmount != 90 || first.WithholdingAmount != 10 || first.Bnk07PaymentID != ins.Bnk07PaymentID {
		t.Fatalf("expected net 90 + withholding 10 keyed by the BNK-07 payment, got %+v", first)
	}

	e.poll(t, ins.InstructionID)
	if len(e.payables.applied) != 2 {
		t.Fatalf("expected no re-application on a second poll, got %d", len(e.payables.applied))
	}
}

func TestPoll_Settled_AP08Down_RetriedOnNextPoll(t *testing.T) {
	e := newEnv()
	e.add("auth-17", 100)
	run := e.lockedRun(t, "auth-17")
	e.submit(run.RunID, "idem-17")
	ins := e.instructions(t, run.RunID)[0]
	e.status.statuses[ins.Bnk07PaymentID] = "SETTLED"

	e.payables.fail = true
	if resp := e.poll(t, ins.InstructionID); resp["payables_pending_settlement"].(float64) != 1 {
		t.Fatalf("expected 1 payable pending while AP-08 is down, got %v", resp)
	}
	e.payables.fail = false
	if resp := e.poll(t, ins.InstructionID); resp["payables_pending_settlement"].(float64) != 0 {
		t.Fatalf("expected the payable settled on the next poll, got %v", resp)
	}
	if len(e.payables.applied) != 1 {
		t.Fatalf("expected exactly one AP-08 application, got %d", len(e.payables.applied))
	}
}

// settledRun drives a single-payee run to a bank-confirmed SETTLED instruction.
func (e *env) settledRun(t *testing.T, authID string) (*domain.PaymentRun, domain.RunInstruction) {
	t.Helper()
	e.add(authID, 100)
	run := e.lockedRun(t, authID)
	e.submit(run.RunID, "idem-"+authID)
	ins := e.instructions(t, run.RunID)[0]
	e.status.statuses[ins.Bnk07PaymentID] = "SETTLED"
	e.poll(t, ins.InstructionID)
	return run, ins
}

func (e *env) accountingStatus(t *testing.T, runID string) map[string]interface{} {
	t.Helper()
	w := doRequest(e.router, http.MethodGet, "/ap11/runs/"+runID+"/accounting-status", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("accounting-status: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp
}

// A bank-confirmed settlement records exactly one ACC-04 posting request, and
// polling again never records a second.
func TestAccountingStatus_SettlementRecordsOnePostingRequest(t *testing.T) {
	e := newEnv()
	run, ins := e.settledRun(t, "auth-acc-1")
	e.poll(t, ins.InstructionID)

	resp := e.accountingStatus(t, run.RunID)
	postings, _ := resp["postings"].([]interface{})
	if len(postings) != 1 {
		t.Fatalf("expected exactly one posting request, got %v", resp["postings"])
	}
	p := postings[0].(map[string]interface{})
	if p["status"] != "PENDING" || p["source_event_id"] != "ap11:settle:"+ins.InstructionID {
		t.Fatalf("unexpected posting request %v", p)
	}
	if resp["posting_counts"].(map[string]interface{})["PENDING"].(float64) != 1 {
		t.Fatalf("expected posting_counts PENDING=1, got %v", resp["posting_counts"])
	}
}

func TestRequeueAccounting_RequeuesOnlyFailedAndQuarantined(t *testing.T) {
	e := newEnv()
	run, _ := e.settledRun(t, "auth-acc-2")
	reqs := e.store.accounting[run.RunID]
	reqs[0].Status = "QUARANTINED"
	posted := &store.AccountingRequest{RequestID: "posted-1", SourceEventID: "ap11:settle:other", Status: "POSTED"}
	e.store.accounting[run.RunID] = append(reqs, posted)

	w := doRequest(e.router, http.MethodPost, "/ap11/runs/"+run.RunID+"/accounting/requeue", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("requeue: %d %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["requeued"].(float64) != 1 {
		t.Fatalf("expected 1 requeued, got %v", resp)
	}
	if reqs[0].Status != "PENDING" || posted.Status != "POSTED" {
		t.Fatalf("requeue must reset QUARANTINED and never touch POSTED: %s / %s", reqs[0].Status, posted.Status)
	}
}

func TestRequeueAccounting_RequiresAuthorization(t *testing.T) {
	e := newEnv()
	run, _ := e.settledRun(t, "auth-acc-3")
	e.store.accounting[run.RunID][0].Status = "FAILED"
	e.authz.deny = true

	w := doRequest(e.router, http.MethodPost, "/ap11/runs/"+run.RunID+"/accounting/requeue", nil, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when not authorized, got %d", w.Code)
	}
	if e.store.accounting[run.RunID][0].Status != "FAILED" {
		t.Fatal("a refused requeue must not change the request")
	}
}

func TestRequeueAccounting_UnknownRun_404(t *testing.T) {
	e := newEnv()
	w := doRequest(e.router, http.MethodPost, "/ap11/runs/does-not-exist/accounting/requeue", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPoll_NoNewsYet_NotApplied(t *testing.T) {
	e := newEnv()
	e.add("auth-18", 100)
	run := e.lockedRun(t, "auth-18")
	e.submit(run.RunID, "idem-18")
	ins := e.instructions(t, run.RunID)[0]

	if resp := e.poll(t, ins.InstructionID); resp["applied"].(bool) {
		t.Fatalf("expected applied=false while BNK-07 still reports PREPARED, got %v", resp)
	}
}

func TestPoll_OpenConflict_NotApplied(t *testing.T) {
	e := newEnv()
	e.add("auth-19", 100)
	run := e.lockedRun(t, "auth-19")
	e.submit(run.RunID, "idem-19")
	ins := e.instructions(t, run.RunID)[0]
	e.status.statuses[ins.Bnk07PaymentID] = "SETTLED"
	e.status.conflicts[ins.Bnk07PaymentID] = true

	e.poll(t, ins.InstructionID)
	if s := e.runStatus(t, run.RunID); s == domain.StatusSettled || len(e.payables.applied) != 0 {
		t.Fatalf("a conflicted BNK-07 status must not settle anything (run %s, %d applications)", s, len(e.payables.applied))
	}
}

// TestPoll_ResolvesUnknownFromSameAttempt: after a submit timeout the bank
// did accept the attempt; polling reads that same attempt, records BNK-07
// and returns the instruction to normal tracking — no re-submission.
func TestPoll_ResolvesUnknownFromSameAttempt(t *testing.T) {
	e := newEnv()
	e.add("auth-20", 100)
	e.provider.submitErr = domain.ErrProviderAdapterUnavailable
	run := e.lockedRun(t, "auth-20")
	e.submit(run.RunID, "idem-20")
	ins := e.instructions(t, run.RunID)[0]

	e.provider.attempts[ins.ProviderAttemptID].Status = provideradapter.AttemptSubmitted
	e.poll(t, ins.InstructionID)

	ins = e.instructions(t, run.RunID)[0]
	if ins.Status != domain.InstructionPending || ins.Bnk07PaymentID == "" {
		t.Fatalf("expected the unknown resolved to PENDING with a BNK-07 record, got %+v", ins)
	}
	if e.provider.submitCalls != 1 || e.provider.retryCalls != 0 {
		t.Fatalf("poll must not re-send: %d submits, %d retries", e.provider.submitCalls, e.provider.retryCalls)
	}
	if s := e.runStatus(t, run.RunID); s != domain.StatusSubmitted {
		t.Fatalf("expected run SUBMITTED, got %s", s)
	}
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestGetAvailableActions_Draft(t *testing.T) {
	e := newEnv()
	e.add("auth-13", 100)
	run := e.createRun(t, "auth-13")
	w := doRequest(e.router, http.MethodGet, "/ap11/runs/"+run.RunID+"/available-actions", nil, testTenant)
	var resp struct {
		AvailableActions []string `json:"available_actions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	found := map[string]bool{}
	for _, act := range resp.AvailableActions {
		found[act] = true
	}
	if !found["ValidatePaymentRun"] || !found["CancelUnsubmittedRun"] || found["SubmitPaymentRun"] {
		t.Fatalf("unexpected DRAFT actions %v", resp.AvailableActions)
	}
}

func TestGetRun_NotFound(t *testing.T) {
	e := newEnv()
	if w := doRequest(e.router, http.MethodGet, "/ap11/runs/does-not-exist", nil, testTenant); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
