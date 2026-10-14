package ar06

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

	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
)

// testNow is the fixed clock of every test: 2026-10-12.
var testNow = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

const base = "/internal/v1/ar06/"

type fakeAuthz struct {
	grants map[string]map[string]bool
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

// fakeInvoices stands in for AR-04's kit.Invoices: issued invoices are registered per tenant; LinkCorrection writes to a
// test table in the caller's transaction (so a rollback visibly undoes it) when the table exists.
type fakeInvoices struct {
	mu      sync.Mutex
	issued  map[string]*kit.IssuedInvoice // tenant + "/" + id
	links   []string
	linkErr error
	useDB   bool
}

func newFakeInvoices() *fakeInvoices { return &fakeInvoices{issued: map[string]*kit.IssuedInvoice{}} }

func (f *fakeInvoices) put(tenant string, inv *kit.IssuedInvoice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued[tenant+"/"+inv.InvoiceID] = inv
}

func (f *fakeInvoices) GetIssued(_ context.Context, tenantID, invoiceID string) (*kit.IssuedInvoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.issued[tenantID+"/"+invoiceID]
	if !ok {
		return nil, kit.NotFound("invoice not found")
	}
	c := *in
	return &c, nil
}

func (f *fakeInvoices) LinkCorrection(ctx context.Context, tx pgx.Tx, _ kit.Caller, invoiceID, correctionID, kind string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linkErr != nil {
		return f.linkErr
	}
	if tx != nil && f.useDB {
		if _, err := tx.Exec(ctx, `INSERT INTO ar06_test_links (invoice_id, correction_id, kind) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, invoiceID, correctionID, kind); err != nil {
			return err
		}
	}
	f.links = append(f.links, invoiceID+"|"+correctionID+"|"+kind)
	return nil
}
func (f *fakeInvoices) SetEInvoiceState(context.Context, pgx.Tx, kit.Caller, string, string) error {
	return nil
}
func (f *fakeInvoices) SetSettlementState(context.Context, pgx.Tx, kit.Caller, string, string) error {
	return nil
}
func (f *fakeInvoices) SetDisputeState(context.Context, pgx.Tx, kit.Caller, string, string) error {
	return nil
}
func (f *fakeInvoices) setLinkErr(err error) {
	f.mu.Lock()
	f.linkErr = err
	f.mu.Unlock()
}
func (f *fakeInvoices) linkLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.links...)
}

// fakeOpenItems stands in for AR-07. Truth lives in test tables written in the caller's transaction so that a rollback
// visibly undoes the consequence.
type fakeOpenItems struct {
	mu    sync.Mutex
	err   error
	items map[string]*kit.OpenItemView // source document id -> item (invoices)
}

func newFakeOpenItems() *fakeOpenItems { return &fakeOpenItems{items: map[string]*kit.OpenItemView{}} }

func (f *fakeOpenItems) putInvoiceItem(tenant, entity, invoiceID, currency string, gross kit.Amount) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[tenant+"/"+invoiceID] = &kit.OpenItemView{OpenItemID: "OI-" + invoiceID, DocumentKind: kit.DocInvoice, SourceDocumentID: invoiceID, LegalEntityID: entity,
		Currency: currency, OriginalAmount: gross, Residual: gross}
}

func (f *fakeOpenItems) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeOpenItems) CreateFromDocument(ctx context.Context, tx pgx.Tx, _ kit.Caller, in kit.OpenItemFromDocument) (kit.OpenItemRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return kit.OpenItemRef{}, f.err
	}
	if tx != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO ar06_test_openitems (source_document_id, kind, amount, source_invoice_id, due) VALUES ($1,$2,$3::numeric,$4,$5) ON CONFLICT DO NOTHING`,
			in.SourceDocumentID, in.DocumentKind, in.OriginalAmount.String(), in.SourceInvoiceID, in.DueDate.Format("2006-01-02")); err != nil {
			return kit.OpenItemRef{}, err
		}
	}
	return kit.OpenItemRef{OpenItemID: "OI-" + in.SourceDocumentID, Version: 1}, nil
}

func (f *fakeOpenItems) ApplyAdjustment(ctx context.Context, tx pgx.Tx, _ kit.Caller, in kit.OpenItemAdjustment) (*kit.OpenItemView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var orig kit.Amount
	for _, it := range f.items {
		if it.OpenItemID == in.OpenItemID {
			orig = it.OriginalAmount
		}
	}
	if tx != nil {
		var done kit.Amount
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount),0) FROM ar06_test_adjustments WHERE open_item_id = $1`, in.OpenItemID).Scan(&done); err != nil {
			return nil, err
		}
		var dup int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ar06_test_adjustments WHERE event_key = $1`, in.EventKey).Scan(&dup); err != nil {
			return nil, err
		}
		if dup == 0 {
			if orig.Sub(done).Sub(in.Amount).IsNegative() {
				return nil, kit.Unprocessable(kit.CodeReceivableResidualInvalid, "the adjustment exceeds the residual")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ar06_test_adjustments (open_item_id, event_key, kind, amount, source_ref_type, source_ref_id) VALUES ($1,$2,$3,$4::numeric,$5,$6)`,
				in.OpenItemID, in.EventKey, in.Kind, in.Amount.String(), in.SourceRefType, in.SourceRefID); err != nil {
				return nil, err
			}
		}
	}
	return &kit.OpenItemView{OpenItemID: in.OpenItemID, OriginalAmount: orig}, nil
}

func (f *fakeOpenItems) GetByID(context.Context, string, string) (*kit.OpenItemView, error) {
	return nil, kit.NotFound("not found")
}

func (f *fakeOpenItems) GetBySourceDocument(_ context.Context, tenantID, sourceDocumentID string) (*kit.OpenItemView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[tenantID+"/"+sourceDocumentID]; ok {
		c := *it
		return &c, nil
	}
	return nil, kit.NotFound("open item not found")
}

func (f *fakeOpenItems) Exposure(context.Context, string, string, string, string) (kit.Amount, error) {
	return 0, nil
}
func (f *fakeOpenItems) CollectionBlocked(context.Context, string, string, string) (bool, error) {
	return false, nil
}

// fakePeriods stands in for the accounting-period gate.
type fakePeriods struct {
	mu    sync.Mutex
	err   error
	calls int
}

func (f *fakePeriods) PostingPeriod(_ context.Context, _ kit.Caller, _ string, _ time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "2026-10", nil
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

var _ ar04.PeriodGate = (*fakePeriods)(nil)

// fakeTax stands in for tax-determination-svc (the TAX domain computes; AR never does).
type fakeTax struct {
	mu          sync.Mutex
	ratePct     string
	requests    []taxdet.Request
	store       map[string]*taxdet.Determination
	detErr      error
	getErr      error
	notFound    map[string]bool
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
	rate := kit.Amount(int64(kit.MustAmount(f.ratePct)) / 100)
	tax := ar04.LineNet(req.GrossAmount, rate, req.Currency)
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

func (f *fakeTax) set(fn func()) {
	f.mu.Lock()
	fn()
	f.mu.Unlock()
}

const (
	pCreator  = "creator"
	pApprover = "approver"
	pIssuer   = "issuer"
	pLinker   = "linker"
	pBoss     = "boss"
	pNobody   = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pCreator:  {ActionCorrectionManage: true},
		pApprover: {ActionCorrectionApprove: true},
		pIssuer:   {ActionCorrectionIssue: true},
		pLinker:   {ActionCorrectionLink: true},
		pBoss:     {ActionCorrectionManage: true, ActionCorrectionApprove: true, ActionCorrectionIssue: true, ActionCorrectionLink: true},
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

func asCorrection(t *testing.T, body []byte) *Correction {
	t.Helper()
	var c Correction
	require.NoError(t, json.Unmarshal(body, &c), string(body))
	return &c
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

// sourceInvoice is the default issued source: two lines, net 1250.50, tax 250.10 (20%), gross 1500.60 USD.
func sourceInvoice(entity, party string) *kit.IssuedInvoice {
	return &kit.IssuedInvoice{InvoiceID: uuid.NewString(), LegalEntityID: entity, CustomerPartyID: party, InvoiceNumber: "INV-000001", Currency: "USD",
		Net: kit.MustAmount("1250.50"), Tax: kit.MustAmount("250.10"), Gross: kit.MustAmount("1500.60"),
		IssueDate: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), DueDate: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), ContentHash: "h",
		Lines: []kit.IssuedInvoiceLine{
			{LineNumber: 1, Description: "Platform licence", Quantity: kit.MustAmount("2"), UnitPrice: kit.MustAmount("500"), Net: kit.MustAmount("1000.00"), Tax: kit.MustAmount("200.00"), TaxCode: "STANDARD"},
			{LineNumber: 2, Description: "Onboarding", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("250.50"), Net: kit.MustAmount("250.50"), Tax: kit.MustAmount("50.10"), TaxCode: "STANDARD"},
		}}
}

// creditBody is a credit note correcting source line 1 by unit_price (quantity 1).
func creditBody(entity string, src *kit.IssuedInvoice, unitPrice string, extra map[string]any) map[string]any {
	b := map[string]any{"legal_entity_id": entity, "kind": "CREDIT_NOTE", "source_invoice_id": src.InvoiceID, "reason_code": "PRICE_ADJUSTMENT", "reason": "agreed discount",
		"supply_date": "2026-10-10", "tax_context": map[string]any{"jurisdiction_id": "GB", "supply_type": "B2B"},
		"lines": []map[string]any{{"source_line_no": 1, "description": "Licence price correction", "quantity": "1", "unit_price": unitPrice,
			"product_classification": "SOFTWARE", "supply_kind": "DIGITAL_SERVICES"}}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func debitBody(entity string, src *kit.IssuedInvoice, unitPrice string, extra map[string]any) map[string]any {
	b := creditBody(entity, src, unitPrice, nil)
	b["kind"] = "DEBIT_NOTE"
	b["reason_code"] = "UNDERBILLED"
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
