package ar04

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

// testNow is the fixed clock of every test: 2026-10-12.
var testNow = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

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
	err   error
	days  int
	calls int
	at    []time.Time
}

func (f *fakeProfiles) ActiveAt(_ context.Context, _, entity, party string, at time.Time) (*kit.ProfileView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.at = append(f.at, at)
	if f.err != nil {
		return nil, f.err
	}
	days := f.days
	if days == 0 {
		days = 30
	}
	return &kit.ProfileView{CustomerPartyID: party, LegalEntityID: entity, PaymentTermsCode: "NET30", PaymentTermsDays: days}, nil
}

func (f *fakeProfiles) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// fakeOpenItems stands in for AR-07.
type fakeOpenItems struct {
	mu      sync.Mutex
	err     error
	created []kit.OpenItemFromDocument
}

func (f *fakeOpenItems) CreateFromDocument(ctx context.Context, tx pgx.Tx, _ kit.Caller, in kit.OpenItemFromDocument) (kit.OpenItemRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return kit.OpenItemRef{}, f.err
	}
	// Integration tests give the fake a table in the caller's transaction, so a rollback visibly undoes the receivable.
	if tx != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO ar04_test_openitems (source_document_id) VALUES ($1)`, in.SourceDocumentID); err != nil {
			return kit.OpenItemRef{}, err
		}
	}
	f.created = append(f.created, in)
	return kit.OpenItemRef{OpenItemID: "OI-" + in.SourceDocumentID, Version: 1}, nil
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
func (f *fakeOpenItems) Exposure(context.Context, string, string, string, string) (kit.Amount, error) {
	return 0, nil
}
func (f *fakeOpenItems) CollectionBlocked(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (f *fakeOpenItems) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}
func (f *fakeOpenItems) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

// fakeOrders stands in for AR-03.
type fakeOrders struct {
	mu       sync.Mutex
	basis    *kit.BillingBasis
	eligErr  error
	invErr   error
	elig     int
	invoiced []string
}

func (f *fakeOrders) InvoiceEligibility(_ context.Context, _, orderID, milestoneID string) (*kit.BillingBasis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.elig++
	if f.eligErr != nil {
		return nil, f.eligErr
	}
	b := *f.basis
	b.OrderID, b.MilestoneID = orderID, milestoneID
	return &b, nil
}

func (f *fakeOrders) RecordInvoiced(ctx context.Context, tx pgx.Tx, _ kit.Caller, orderID, milestoneID, invoiceID string, net kit.Amount) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invErr != nil {
		return f.invErr
	}
	if tx != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO ar04_test_orders_log (invoice_id, entry) VALUES ($1, $2)`, invoiceID, orderID+"|"+milestoneID+"|"+invoiceID+"|"+net.String()); err != nil {
			return err
		}
	}
	f.invoiced = append(f.invoiced, orderID+"|"+milestoneID+"|"+invoiceID+"|"+net.String())
	return nil
}

// fakePeriods stands in for the accounting-period gate.
type fakePeriods struct {
	mu    sync.Mutex
	err   error
	key   string
	calls int
	dates []string
}

func (f *fakePeriods) PostingPeriod(_ context.Context, _ kit.Caller, _ string, date time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.dates = append(f.dates, day(date))
	if f.err != nil {
		return "", f.err
	}
	if f.key == "" {
		return "2026-10", nil
	}
	return f.key, nil
}

func (f *fakePeriods) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

type fakeEInvoice struct {
	required bool
	err      error
}

func (f fakeEInvoice) Required(context.Context, string, string, string, string) (bool, error) {
	return f.required, f.err
}

// fakeTax stands in for tax-determination-svc (the TAX domain computes; AR never does).
type fakeTax struct {
	mu          sync.Mutex
	ratePct     string // percent, e.g. "20"
	requests    []taxdet.Request
	store       map[string]*taxdet.Determination
	detErr      error
	getErr      error
	notFound    map[string]bool
	gets        int
	provenance  string
	overrideTax map[string]kit.Amount
}

func newFakeTax() *fakeTax {
	return &fakeTax{ratePct: "20", store: map[string]*taxdet.Determination{}, notFound: map[string]bool{}, provenance: "snap-2026.1", overrideTax: map[string]kit.Amount{}}
}

func (f *fakeTax) Determine(_ context.Context, _ svcclient.Caller, req taxdet.Request) (*taxdet.Determination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.detErr != nil {
		return nil, f.detErr
	}
	tax := mulRound(req.GrossAmount, kit.MustAmount(f.ratePct), 100*10000, req.Currency)
	d := &taxdet.Determination{DeterminationID: "det-" + uuid.NewString(), LegalEntityID: req.LegalEntityID, RuleID: "rule-1", TaxLogicSnapshotID: f.provenance,
		Status: "CALCULATED", TaxableAmount: req.GrossAmount, TaxRatePercentage: f.ratePct, Tax: tax, GrossAmount: req.GrossAmount.Add(tax), Currency: req.Currency,
		JurisdictionID: req.JurisdictionID, SupplyDate: req.SupplyDate}
	f.store[d.DeterminationID] = d
	c := *d
	return &c, nil
}

func (f *fakeTax) Get(_ context.Context, _ svcclient.Caller, id string) (*taxdet.Determination, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.notFound[id] {
		return nil, taxdet.ErrNotFound
	}
	d, ok := f.store[id]
	if !ok {
		return nil, taxdet.ErrNotFound
	}
	c := *d
	if t, ok := f.overrideTax[id]; ok {
		c.Tax = t
	}
	return &c, nil
}

func (f *fakeTax) reset() {
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
}

const (
	pCreator   = "creator"   // create / amend / submit / cancel unissued
	pApprover  = "approver"  // approve only
	pIssuer    = "issuer"    // issue only
	pBoss      = "boss"      // every action (proves self-approval above the threshold is refused)
	pDelivery  = "delivery"  // record delivery results
	pCancelReq = "cancelreq" // request a legal cancellation
	pCancelApp = "cancelapp" // decide a legal cancellation
	pLinker    = "linker"    // link correction documents
	pNobody    = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pCreator:   {ActionInvoiceManage: true},
		pApprover:  {ActionInvoiceApprove: true},
		pIssuer:    {ActionInvoiceIssue: true},
		pDelivery:  {ActionInvoiceDeliveryRecord: true},
		pCancelReq: {ActionInvoiceCancelRequest: true},
		pCancelApp: {ActionInvoiceCancelApprove: true},
		pLinker:    {ActionInvoiceCorrectionLink: true},
		pBoss: {ActionInvoiceManage: true, ActionInvoiceApprove: true, ActionInvoiceIssue: true, ActionInvoiceCancelRequest: true, ActionInvoiceCancelApprove: true,
			ActionInvoiceCorrectionLink: true, ActionInvoiceDeliveryRecord: true},
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
	return kit.Deps{Log: zap.NewNop(), Authz: authz, Entities: fakeEntities{}, Clock: func() time.Time { return testNow }}
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

func asInvoice(t *testing.T, body []byte) *Invoice {
	t.Helper()
	var inv Invoice
	require.NoError(t, json.Unmarshal(body, &inv), string(body))
	return &inv
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

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

func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }

// createBody is a valid draft: two tax groups (SOFTWARE digital services, SERVICES), net 1250.50 USD.
func createBody(entity, party string) map[string]any {
	return map[string]any{
		"legal_entity_id": entity, "customer_party_id": party, "currency": "USD", "supply_date": "2026-10-10", "customer_reference": "PO-7781",
		"notes": "Q4 licence", "tax_context": map[string]any{"jurisdiction_id": "GB", "supply_type": "B2B"},
		"lines": []map[string]any{
			{"description": "Platform licence", "product_ref": "SKU-LIC", "quantity": "2", "unit_price": "500.00", "tax_category": "STANDARD",
				"product_classification": "SOFTWARE", "supply_kind": "DIGITAL_SERVICES"},
			{"description": "Onboarding", "product_ref": "SKU-ONB", "quantity": "1", "unit_price": "250.50", "tax_category": "STANDARD",
				"product_classification": "CONSULTING", "supply_kind": "SERVICES"},
		},
	}
}
