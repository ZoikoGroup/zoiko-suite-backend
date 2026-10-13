package ar09

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

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

var testNow = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC) // a Monday

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
	pCollector = "collector" // owns cases: manage + send
	pManager   = "manager"   // a second manager (not the owner)
	pHold      = "holder"
	pHold2     = "holder2"
	pEscalate  = "escalator"
	pPolicy    = "policyadmin"
	pNobody    = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pCollector: {ActionCollectionsManage: true, ActionDunningSend: true},
		pManager:   {ActionCollectionsManage: true, ActionDunningSend: true, ActionCollectionsEscalate: true},
		pHold:      {ActionHoldManage: true},
		pHold2:     {ActionHoldManage: true},
		pEscalate:  {ActionCollectionsEscalate: true},
		pPolicy:    {ActionPolicyManage: true},
		pNobody:    {},
		// AR-07 actions for the integration tests that drive real AR-07 (dispute / hold) through its own routes.
		"ar07dispute": {"AR07_DISPUTE_MANAGE": true, "AR07_HOLD_MANAGE": true},
	}
}

// fakeProfiles stands in for AR-01's resolver.
type fakeProfiles struct {
	mu       sync.Mutex
	inactive bool
	profile  string
	err      error
}

func (f *fakeProfiles) ActiveAt(_ context.Context, _, entity, party string, _ time.Time) (*kit.ProfileView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.inactive {
		return nil, kit.Err(http.StatusConflict, kit.CodeCustomerProfileInactive, "no active profile")
	}
	p := f.profile
	if p == "" {
		p = "STD"
	}
	return &kit.ProfileView{CustomerPartyID: party, LegalEntityID: entity, DunningProfile: p, CreditHold: true /* irrelevant to collections */}, nil
}

func (f *fakeProfiles) setInactive(v bool) { f.mu.Lock(); f.inactive = v; f.mu.Unlock() }

// fakeRecv is an in-memory AR-07 read port that counts calls. It has no mutating method to count: the Receivables interface
// has none.
type fakeRecv struct {
	mu      sync.Mutex
	items   map[string]kit.OpenItemView
	blocked bool
}

func newFakeRecv() *fakeRecv { return &fakeRecv{items: map[string]kit.OpenItemView{}} }

func (f *fakeRecv) put(v kit.OpenItemView) { f.mu.Lock(); f.items[v.OpenItemID] = v; f.mu.Unlock() }

func (f *fakeRecv) GetByID(_ context.Context, _, id string) (*kit.OpenItemView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.items[id]
	if !ok {
		return nil, kit.NotFound("open item not found")
	}
	return &v, nil
}

func (f *fakeRecv) CollectionBlocked(context.Context, string, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blocked, nil
}

func (f *fakeRecv) ListOpenForCustomer(_ context.Context, _, entity, party, currency string) ([]kit.OpenItemView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []kit.OpenItemView{}
	for _, v := range f.items {
		if v.CustomerPartyID == party && v.LegalEntityID == entity && v.Currency == currency {
			out = append(out, v)
		}
	}
	return out, nil
}

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

const base = "/internal/v1/ar09/"

// testPolicy is the stage plan used by the tests: dunning at 5 / 15 / 30 days past due; promise grace 2 days.
func testPolicy() Policy {
	return Policy{DunningProfile: "STD", EffectiveFrom: "2026-01-01", PromiseGraceDays: 2, Stages: []Stage{
		{StageNo: 1, DaysPastDue: 5, Channel: "EMAIL", TemplateKey: "DUN1"},
		{StageNo: 2, DaysPastDue: 15, Channel: "EMAIL", TemplateKey: "DUN2"},
		{StageNo: 3, DaysPastDue: 30, GraceDays: 0, Channel: "LETTER", TemplateKey: "DUN3", EscalationThreshold: kit.MustAmount("100")},
	}}
}

func mkViews() []kit.OpenItemView {
	return []kit.OpenItemView{
		{OpenItemID: "late", Residual: kit.MustAmount("100"), DueDate: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{OpenItemID: "current", Residual: kit.MustAmount("100"), DueDate: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{OpenItemID: "paid", Residual: 0, DueDate: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	}
}
