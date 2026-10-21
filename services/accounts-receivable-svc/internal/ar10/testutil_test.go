package ar10

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

var testNow = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

const base = "/internal/v1/ar10/"

type fakeAuthz struct{ grants map[string]map[string]bool }

func (f *fakeAuthz) CheckAllowed(_ context.Context, principal, _, action string) error {
	if f.grants[principal][action] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

type fakeEntities struct{}

func (fakeEntities) VerifyInTenant(context.Context, string, string) error { return nil }

// fakePeriods stands in for the accounting-period gate.
type fakePeriods struct {
	mu  sync.Mutex
	err error
}

var _ ar04.PeriodGate = (*fakePeriods)(nil)

func (f *fakePeriods) PostingPeriod(context.Context, kit.Caller, string, time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return "2026-10", nil
}
func (f *fakePeriods) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

// fakeDest stands in for payee-banking-identity-svc.
type fakeDest struct {
	mu sync.Mutex
	d  map[string]*DestinationInfo
}

func newFakeDest() *fakeDest { return &fakeDest{d: map[string]*DestinationInfo{}} }

func (f *fakeDest) put(ref, party, currency, fingerprint, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.d[ref] = &DestinationInfo{Ref: ref, PartyRef: party, Currency: currency, Fingerprint: fingerprint, Status: status,
		Evidence: map[string]any{"source": "fake", "status": status, "verification_evidence_ref": "ev-" + ref}}
}

func (f *fakeDest) mutate(ref string, fn func(*DestinationInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.d[ref])
}

func (f *fakeDest) Verify(_ context.Context, _ kit.Caller, ref string) (*DestinationInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.d[ref]
	if !ok {
		return nil, kit.NotFound("refund destination not found")
	}
	c := *d
	return &c, nil
}

// fakeGateway stands in for payment-initiation-adapter-svc + payment-status-svc and COUNTS external effects.
type fakeGateway struct {
	mu        sync.Mutex
	attempts  map[string]*Attempt // by attempt id
	byKey     map[string]string   // idempotency key -> attempt id
	payments  map[string]*PaymentState
	prepares  int // PrepareAttempt calls
	submits   int // SubmitAttempt calls (the external effect)
	registers int
	prepErr   error
	mode      string // submit outcome: "ok" | "timeout" | "reject" | "nonerror-unknown"
	subErr    error
	getErr    error
	payErr    error
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{attempts: map[string]*Attempt{}, byKey: map[string]string{}, payments: map[string]*PaymentState{}, mode: "ok"}
}

func (f *fakeGateway) PrepareAttempt(_ context.Context, _ kit.Caller, in PrepareInput) (*Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	if f.prepErr != nil {
		return nil, f.prepErr
	}
	if id, ok := f.byKey[in.IdempotencyKey]; ok {
		c := *f.attempts[id]
		return &c, nil
	}
	a := &Attempt{AttemptID: uuid.NewString(), Status: AttemptPrepared}
	f.attempts[a.AttemptID], f.byKey[in.IdempotencyKey] = a, a.AttemptID
	c := *a
	return &c, nil
}

func (f *fakeGateway) SubmitAttempt(_ context.Context, _ kit.Caller, id string) (*Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	a := f.attempts[id]
	if a == nil || a.Status != AttemptPrepared {
		return nil, ErrGatewayUnavailable // the adapter's 409 "not submittable"
	}
	if f.subErr != nil {
		return nil, f.subErr
	}
	switch f.mode {
	case "ok":
		a.Status, a.ProviderRequestID = AttemptSubmitted, "prov-"+id
	case "timeout": // the adapter swallows a provider timeout into PENDING_UNKNOWN and answers 200
		a.Status = AttemptPendingUnkn
	case "reject":
		a.Status, a.RejectionReason = AttemptRejectedPre, "provider rejected"
	}
	c := *a
	return &c, nil
}

func (f *fakeGateway) GetAttempt(_ context.Context, _ kit.Caller, id string) (*Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	a := f.attempts[id]
	if a == nil {
		return nil, ErrGatewayUnavailable
	}
	c := *a
	return &c, nil
}

func (f *fakeGateway) RegisterPayment(_ context.Context, _ kit.Caller, _, providerRequestID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers++
	if f.payErr != nil {
		return "", f.payErr
	}
	id := uuid.NewString()
	f.payments[id] = &PaymentState{PaymentID: id, Status: PaySubmitted, ProviderRequestID: providerRequestID}
	return id, nil
}

func (f *fakeGateway) GetPayment(_ context.Context, _ kit.Caller, id string) (*PaymentState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.payErr != nil {
		return nil, f.payErr
	}
	p := f.payments[id]
	if p == nil {
		return nil, ErrGatewayUnavailable
	}
	c := *p
	return &c, nil
}

// resolve simulates the adapter operator resolving an ambiguous attempt.
func (f *fakeGateway) resolve(attemptID, status, providerRequestID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[attemptID].Status, f.attempts[attemptID].ProviderRequestID = status, providerRequestID
}

func (f *fakeGateway) setAllPayments(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.payments {
		p.Status = status
	}
}

func (f *fakeGateway) counts() (prepares, submits, registers int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares, f.submits, f.registers
}

const (
	pRequester = "requester" // creates refunds and write-offs
	pApprover  = "approver"  // approves refunds
	pReleaser  = "releaser"  // submits + reconciles refunds
	pRefundAll = "refundall" // create + approve + submit + reconcile (to prove SoD, not roles, stops self-service)
	pTaxReview = "taxreview" // tax treatment decisions
	pTier1     = "tier1"     // write-off approval up to the tier-1 limit
	pTier2     = "tier2"
	pTier3     = "tier3"
	pTier4     = "tier4"
	pCommitter = "committer" // commits write-offs
	pWOAll     = "woall"     // create + tier3 + tax + commit (requester that holds approval authority)
	pCollector = "collector" // places holds / handles disputes in AR-07, holds tier 3
	pNobody    = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pRequester: {ActionRefundCreate: true, ActionWriteOffCreate: true},
		pApprover:  {ActionRefundApprove: true},
		pReleaser:  {ActionRefundSubmit: true, ActionRefundReconcile: true},
		pRefundAll: {ActionRefundCreate: true, ActionRefundApprove: true, ActionRefundSubmit: true, ActionRefundReconcile: true},
		pTaxReview: {ActionWriteOffTaxReview: true},
		pTier1:     {ActionWriteOffApproveT1: true},
		pTier2:     {ActionWriteOffApproveT2: true},
		pTier3:     {ActionWriteOffApproveT3: true},
		pTier4:     {ActionWriteOffApproveT4: true},
		pCommitter: {ActionWriteOffCommit: true},
		pWOAll:     {ActionWriteOffCreate: true, ActionWriteOffApproveT3: true, ActionWriteOffTaxReview: true, ActionWriteOffCommit: true},
		pCollector: {ActionWriteOffApproveT3: true},
		pNobody:    {},
	}
}

func testDeps(authz kit.Authz) kit.Deps {
	return kit.Deps{Log: zap.NewNop(), Authz: authz, Entities: fakeEntities{}, Clock: func() time.Time { return testNow }}
}

func testRouter(m *Module) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	m.Register(r)
	return r
}

func call(t *testing.T, h http.Handler, method, path, tenant, principal, key string, body any) (int, []byte, http.Header) {
	t.Helper()
	rdr := bytes.NewReader(nil)
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("X-Tenant-Id", tenant)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if method == http.MethodPost && key == "" {
		key = uuid.NewString()
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), rec.Header()
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(body, &e), string(body))
	return e.Code
}

// newReq builds a direct-handler request carrying the tenant context the gateway middleware would install.
func newReq(method, path, tenant, principal string, body any) *http.Request {
	rdr := bytes.NewReader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
		req = req.WithContext(svcmiddleware.WithTenant(req.Context(), tenant))
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	return req
}
