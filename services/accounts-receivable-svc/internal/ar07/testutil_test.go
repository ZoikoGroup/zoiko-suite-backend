package ar07

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
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

var testNow = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

type fakeAuthz struct{ grants map[string]map[string]bool }

func (f *fakeAuthz) CheckAllowed(_ context.Context, principal, _, action string) error {
	if f.grants[principal][action] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

type fakeEntities struct{}

func (fakeEntities) VerifyInTenant(context.Context, string, string) error { return nil }

const (
	pManager  = "manager"  // create / close
	pAdjuster = "adjuster" // privileged correction
	pDispute  = "dispute"  // dispute open/resolve
	pHold     = "hold"
	pBoss     = "boss"
	pNobody   = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pManager:  {ActionReceivableManage: true},
		pAdjuster: {ActionReceivableAdjust: true},
		pDispute:  {ActionDisputeManage: true},
		pHold:     {ActionHoldManage: true},
		pBoss:     {ActionReceivableManage: true, ActionReceivableAdjust: true, ActionDisputeManage: true, ActionHoldManage: true},
		pNobody:   {},
	}
}

// testClock is a mutable clock so ledger timestamps are distinct and deterministic.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func testDeps(authz kit.Authz, clock func() time.Time) kit.Deps {
	return kit.Deps{Log: zap.NewNop(), Authz: authz, Entities: fakeEntities{}, Clock: clock}
}

func testRouter(m *Module) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	m.Register(r)
	return r
}

// fakeInvoices records the dimension mirror calls (stands in for AR-04).
type fakeInvoices struct {
	mu         sync.Mutex
	settlement []string // "<invoiceID>:<state>"
	dispute    []string
	err        error
}

func (f *fakeInvoices) GetIssued(context.Context, string, string) (*kit.IssuedInvoice, error) {
	return nil, nil
}
func (f *fakeInvoices) LinkCorrection(context.Context, pgx.Tx, kit.Caller, string, string, string) error {
	return nil
}
func (f *fakeInvoices) SetEInvoiceState(context.Context, pgx.Tx, kit.Caller, string, string) error {
	return nil
}
func (f *fakeInvoices) SetSettlementState(_ context.Context, _ pgx.Tx, _ kit.Caller, id, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.settlement = append(f.settlement, id+":"+state)
	return nil
}
func (f *fakeInvoices) SetDisputeState(_ context.Context, _ pgx.Tx, _ kit.Caller, id, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.dispute = append(f.dispute, id+":"+state)
	return nil
}
func (f *fakeInvoices) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }
func (f *fakeInvoices) last() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, d := "", ""
	if n := len(f.settlement); n > 0 {
		s = f.settlement[n-1]
	}
	if n := len(f.dispute); n > 0 {
		d = f.dispute[n-1]
	}
	return s, d
}

func call(t *testing.T, h http.Handler, method, path, tenant, principal, key string, body any) (int, []byte) {
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
	return rec.Code, rec.Body.Bytes()
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

const base = "/internal/v1/ar07/"

func httpRec() *httptest.ResponseRecorder { return httptest.NewRecorder() }
