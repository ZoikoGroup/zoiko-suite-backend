package ar08

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

const base = "/internal/v1/ar08/"

type fakeAuthz struct{ grants map[string]map[string]bool }

func (f *fakeAuthz) CheckAllowed(_ context.Context, principal, _, action string) error {
	if f.grants[principal][action] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

type fakeEntities struct{}

func (fakeEntities) VerifyInTenant(context.Context, string, string) error { return nil }

// fakeSource stands in for Banking (banking-connector-svc).
type fakeSource struct {
	mu    sync.Mutex
	txns  map[string]*BankTransaction
	err   error
	calls int
}

func newFakeSource() *fakeSource { return &fakeSource{txns: map[string]*BankTransaction{}} }

func (f *fakeSource) put(tenant, id, amount, currency, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txns[id] = &BankTransaction{TransactionID: id, TenantID: tenant, StatementLineID: uuid.NewString(), Currency: currency, Status: status,
		Counterparty: "ACME PAYER", Amount: kit.MustAmount(amount), TransactionDate: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeSource) setStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txns[id].Status = status
}

func (f *fakeSource) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *fakeSource) Get(_ context.Context, _ kit.Caller, id string) (*BankTransaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	t, ok := f.txns[id]
	if !ok {
		return nil, ErrBankTxnNotFound
	}
	c := *t
	return &c, nil
}

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

const (
	pRegistrar = "registrar" // registers receipts
	pClerk     = "clerk"     // applies / proposes
	pApprover  = "approver"  // approves manual matches
	pUnapplier = "unapplier"
	pReapplier = "reapplier"
	pClerkAppr = "clerkappr" // manage + approve (to prove proposer != approver)
	pRegAppr   = "regappr"   // register + approve (to prove registrant gains no override)
	pNobody    = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pRegistrar: {ActionReceiptRegister: true},
		pClerk:     {ActionApplicationManage: true},
		pApprover:  {ActionManualMatchApprove: true},
		pUnapplier: {ActionUnapply: true},
		pReapplier: {ActionReapply: true},
		pClerkAppr: {ActionApplicationManage: true, ActionManualMatchApprove: true},
		pRegAppr:   {ActionReceiptRegister: true, ActionManualMatchApprove: true},
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
