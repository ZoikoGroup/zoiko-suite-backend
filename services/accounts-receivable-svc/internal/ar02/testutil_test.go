package ar02

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
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
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
	return &kit.ProfileView{CustomerPartyID: party, LegalEntityID: entity, PaymentTermsCode: "NET30", PaymentTermsDays: 30, CreditHold: f.hold}, nil
}

func (f *fakeProfiles) set(hold bool, err error) {
	f.mu.Lock()
	f.hold, f.err = hold, err
	f.mu.Unlock()
}

// fakeTax stands in for tax-determination-svc.
type fakeTax struct {
	mu    sync.Mutex
	det   *taxdet.Determination
	err   error
	calls int
	last  taxdet.Request
}

func (f *fakeTax) Determine(_ context.Context, _ svcclient.Caller, req taxdet.Request) (*taxdet.Determination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	return f.det, nil
}

func (f *fakeTax) Get(context.Context, svcclient.Caller, string) (*taxdet.Determination, error) {
	return nil, taxdet.ErrNotFound
}

func okDetermination() *taxdet.Determination {
	return &taxdet.Determination{DeterminationID: "TD-1", RuleID: "RULE-1", TaxLogicSnapshotID: "SNAP-7", Status: "CALCULATED",
		TaxableAmount: kit.MustAmount("100"), TaxRatePercentage: "20", Tax: kit.MustAmount("20"), GrossAmount: kit.MustAmount("120"), Currency: "USD", JurisdictionID: "GB"}
}

const (
	pPreparer     = "preparer"  // create/amend/submit/send/accept/reject/expire
	pApprover     = "approver"  // approve (no discount authority)
	pDiscApprover = "discount"  // approve + discount authority
	pBoss         = "boss"      // everything (to prove self-approval above threshold is refused)
	pConverter    = "converter" // convert
	pNobody       = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pPreparer:     {ActionQuoteManage: true},
		pApprover:     {ActionQuoteApprove: true},
		pDiscApprover: {ActionQuoteApprove: true, ActionQuoteDiscountApprove: true},
		pBoss:         {ActionQuoteManage: true, ActionQuoteApprove: true, ActionQuoteDiscountApprove: true, ActionQuoteConvert: true},
		pConverter:    {ActionQuoteConvert: true},
		pNobody:       {},
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

func asQuote(t *testing.T, body []byte) *Quote {
	t.Helper()
	var q Quote
	require.NoError(t, json.Unmarshal(body, &q), string(body))
	return &q
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// newReq builds a direct-handler request carrying the tenant context the
// gateway middleware would install.
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

func validityIn(days int) string { return time.Now().UTC().AddDate(0, 0, days).Format(dateLayout) }

func createBody(entity, party string) map[string]any {
	return map[string]any{
		"legal_entity_id": entity, "customer_party_id": party, "currency": "USD", "validity_date": validityIn(30),
		"price_source_ref": "PRICELIST-2026-Q4", "catalog_version_ref": "CAT-14", "terms": "Net 30; delivery within 14 days",
		"lines": []map[string]any{
			{"description": "Platform licence", "product_ref": "SKU-LIC", "quantity": "2", "unit_price": "500.00"},
			{"description": "Onboarding", "product_ref": "SKU-ONB", "quantity": "1", "unit_price": "250.50"},
		},
	}
}

// fakeOrders is the AR-03 stand-in: it creates a row in a test table inside the
// conversion transaction, idempotent on the source ref.
type fakeOrders struct {
	mu    sync.Mutex
	calls int
	err   error
	last  OrderFromQuote
}

func (f *fakeOrders) CreateFromQuote(ctx context.Context, tx pgx.Tx, c kit.Caller, in OrderFromQuote) (string, error) {
	f.mu.Lock()
	f.calls++
	f.last = in
	err := f.err
	f.mu.Unlock()
	var id string
	if e := tx.QueryRow(ctx, `INSERT INTO ar02_test_orders (order_id, quote_ref, tenant_id, net_total) VALUES ($1,$2,$3,$4::numeric)
		ON CONFLICT (quote_ref) DO UPDATE SET quote_ref = EXCLUDED.quote_ref RETURNING order_id`,
		"SO-"+uuid.NewString()[:8], in.Ref, c.TenantID, in.NetTotal.String()).Scan(&id); e != nil {
		return "", e
	}
	if err != nil { // simulated failure AFTER the order row was written: must roll back with the quote
		return "", err
	}
	return id, nil
}
