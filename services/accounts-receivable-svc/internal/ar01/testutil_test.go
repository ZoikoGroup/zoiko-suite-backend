package ar01

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
)

type fakeAuthz struct {
	grants map[string]map[string]bool // principal -> action -> allowed
	err    error
}

func (f *fakeAuthz) CheckAllowed(_ context.Context, principal, _, action string) error {
	if f.err != nil {
		return f.err
	}
	if f.grants[principal][action] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

type fakeEntities struct{}

func (fakeEntities) VerifyInTenant(context.Context, string, string) error { return nil }

type fakeParties struct {
	party *Party
	err   error
	calls int
}

func (f *fakeParties) GetParty(_ context.Context, _ kit.Caller, id string) (*Party, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	p := *f.party
	p.PartyID = id
	return &p, nil
}

func activeCustomer() *Party { return &Party{Type: "CUSTOMER", Status: "ACTIVE"} }

type fakeOpen struct {
	kit.OpenItems
	exposure kit.Amount
	err      error
}

func (f fakeOpen) Exposure(context.Context, string, string, string, string) (kit.Amount, error) {
	return f.exposure, f.err
}

const (
	pMaker    = "maker"    // profile manage, limit manage, hold manage
	pApprover = "approver" // limit approve
	pBoth     = "both"     // manage + approve (to prove self-approval is refused)
	pNobody   = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pMaker:    {ActionProfileManage: true, ActionLimitManage: true, ActionHoldManage: true},
		pApprover: {ActionLimitApprove: true},
		pBoth:     {ActionProfileManage: true, ActionLimitManage: true, ActionLimitApprove: true, ActionHoldManage: true},
		pNobody:   {},
	}
}

func testRouter(m *Module) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	m.Register(r)
	return r
}

func testDeps(authz kit.Authz) kit.Deps {
	return kit.Deps{Log: zap.NewNop(), Authz: authz, Entities: fakeEntities{}}
}

// call performs one request; commands get a fresh Idempotency-Key unless key is given.
func call(t *testing.T, h http.Handler, method, path, tenant, principal, key string, body any) (int, []byte, http.Header) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
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

func asProfile(t *testing.T, body []byte) *Profile {
	t.Helper()
	var p Profile
	require.NoError(t, json.Unmarshal(body, &p), string(body))
	return &p
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// newReq builds a direct-handler request carrying the tenant context the
// gateway middleware would install.
func newReq(method, path, tenant, principal string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
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

func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }
