package ar03

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

// fakeProfiles stands in for AR-01's resolver.
type fakeProfiles struct {
	mu    sync.Mutex
	hold  bool
	limit kit.Amount
	curr  string
	err   error
	calls int
}

func (f *fakeProfiles) ActiveAt(_ context.Context, _, entity, party string, _ time.Time) (*kit.ProfileView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &kit.ProfileView{CustomerPartyID: party, LegalEntityID: entity, PaymentTermsCode: "NET30", PaymentTermsDays: 30, CreditHold: f.hold,
		CreditLimit: f.limit, CreditCurrency: f.curr}, nil
}

func (f *fakeProfiles) set(hold bool, limit kit.Amount, err error) {
	f.mu.Lock()
	f.hold, f.limit, f.err = hold, limit, err
	if limit.IsPositive() && f.curr == "" {
		f.curr = "USD"
	}
	f.mu.Unlock()
}

// fakeOpenItems stands in for AR-07; only Exposure is meaningful for AR-03.
type fakeOpenItems struct {
	mu       sync.Mutex
	exposure kit.Amount
	err      error
	calls    int
}

func (f *fakeOpenItems) Exposure(context.Context, string, string, string, string) (kit.Amount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.exposure, f.err
}
func (f *fakeOpenItems) CreateFromDocument(context.Context, pgx.Tx, kit.Caller, kit.OpenItemFromDocument) (kit.OpenItemRef, error) {
	return kit.OpenItemRef{}, nil
}
func (f *fakeOpenItems) ApplyAdjustment(context.Context, pgx.Tx, kit.Caller, kit.OpenItemAdjustment) (*kit.OpenItemView, error) {
	return nil, nil
}
func (f *fakeOpenItems) GetByID(context.Context, string, string) (*kit.OpenItemView, error) {
	return nil, nil
}
func (f *fakeOpenItems) GetBySourceDocument(context.Context, string, string) (*kit.OpenItemView, error) {
	return nil, nil
}
func (f *fakeOpenItems) CollectionBlocked(context.Context, string, string, string) (bool, error) {
	return false, nil
}

const (
	pCreator    = "creator"    // create/amend/submit/milestones/close
	pApprover   = "approver"   // approve (no credit authority)
	pConfirmer  = "confirmer"  // confirm
	pCreditAuth = "creditauth" // approve + confirm + credit exception authority + hold
	pCreditRel  = "creditrel"  // hold + credit exception authority (release authority)
	pHolder     = "holder"     // hold only
	pCanceller  = "canceller"  // cancel
	pBoss       = "boss"       // everything (to prove self-approval above threshold is refused)
	pNobody     = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pCreator:    {ActionOrderManage: true},
		pApprover:   {ActionOrderApprove: true},
		pConfirmer:  {ActionOrderConfirm: true},
		pCreditAuth: {ActionOrderApprove: true, ActionOrderConfirm: true, ActionCreditExceptionApprove: true, ActionOrderHold: true},
		pCreditRel:  {ActionOrderHold: true, ActionCreditExceptionApprove: true},
		pHolder:     {ActionOrderHold: true},
		pCanceller:  {ActionOrderCancel: true},
		pBoss: {ActionOrderManage: true, ActionOrderApprove: true, ActionOrderConfirm: true, ActionOrderHold: true, ActionOrderCancel: true,
			ActionCreditExceptionApprove: true},
		pNobody: {},
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

func asOrder(t *testing.T, body []byte) *Order {
	t.Helper()
	var o Order
	require.NoError(t, json.Unmarshal(body, &o), string(body))
	return &o
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// newReq builds a direct-handler request carrying the tenant context the gateway
// middleware would install.
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

func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }

func createBody(entity, party string) map[string]any {
	return map[string]any{
		"legal_entity_id": entity, "customer_party_id": party, "currency": "USD", "customer_po_ref": "PO-7781",
		"price_source_ref": "PRICELIST-2026-Q4", "catalog_version_ref": "CAT-14", "terms": "Net 30; delivery within 14 days",
		"lines": []map[string]any{
			{"description": "Platform licence", "product_ref": "SKU-LIC", "quantity": "2", "unit_price": "500.00"},
			{"description": "Onboarding", "product_ref": "SKU-ONB", "quantity": "1", "unit_price": "250.50"},
		},
	}
}
