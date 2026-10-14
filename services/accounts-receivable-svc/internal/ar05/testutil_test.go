package ar05

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// testNow is the fixed start of every test clock: 2026-10-12.
var testNow = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock { return &testClock{t: testNow} }
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

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

type fakeEntities struct{ err error }

func (f fakeEntities) VerifyInTenant(context.Context, string, string) error { return f.err }

const (
	pSubmitter  = "submitter"  // prepare / validate / submit / retry / cancel
	pReconciler = "reconciler" // reconcile by querying the provider
	pAdmin      = "admin"      // reconcile + adapter admin (manual evidence path)
	pAdmin2     = "admin2"     // a second admin (SoD)
	pNobody     = "nobody"
)

func standardGrants() map[string]map[string]bool {
	return map[string]map[string]bool{
		pSubmitter:  {ActionSubmit: true},
		pReconciler: {ActionReconcile: true},
		pAdmin:      {ActionReconcile: true, ActionAdapterAdmin: true},
		pAdmin2:     {ActionReconcile: true, ActionAdapterAdmin: true, ActionSubmit: true},
		pNobody:     {},
	}
}

// fakeInvoices stands in for AR-04's kit.Invoices. It records EVERY call, so a test can prove that a rejected e-invoice
// touched only the e-invoice dimension.
type fakeInvoices struct {
	mu       sync.Mutex
	issued   map[string]*kit.IssuedInvoice // tenant|id
	calls    []string                      // "SetEInvoiceState:<id>:<state>" and any other method
	dim      map[string]string
	failDim  error
	writeTx  bool // also write the dimension into ar05_test_dimension inside the caller's tx (atomicity proofs)
	original map[string]kit.IssuedInvoice
}

func newFakeInvoices() *fakeInvoices {
	return &fakeInvoices{issued: map[string]*kit.IssuedInvoice{}, dim: map[string]string{}, original: map[string]kit.IssuedInvoice{}}
}

func (f *fakeInvoices) put(tenant string, inv *kit.IssuedInvoice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued[tenant+"|"+inv.InvoiceID] = inv
	f.original[inv.InvoiceID] = *inv
}

func (f *fakeInvoices) GetIssued(_ context.Context, tenantID, invoiceID string) (*kit.IssuedInvoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv, ok := f.issued[tenantID+"|"+invoiceID]
	if !ok {
		return nil, kit.NotFound("invoice not found")
	}
	c := *inv
	c.Lines = append([]kit.IssuedInvoiceLine(nil), inv.Lines...)
	return &c, nil
}

func (f *fakeInvoices) LinkCorrection(context.Context, pgx.Tx, kit.Caller, string, string, string) error {
	f.record("LinkCorrection")
	return nil
}

func (f *fakeInvoices) SetEInvoiceState(ctx context.Context, tx pgx.Tx, _ kit.Caller, invoiceID, state string) error {
	f.mu.Lock()
	if f.failDim != nil {
		err := f.failDim
		f.mu.Unlock()
		return err
	}
	f.calls = append(f.calls, "SetEInvoiceState:"+invoiceID+":"+state)
	f.dim[invoiceID] = state
	wtx := f.writeTx
	f.mu.Unlock()
	if wtx && tx != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO ar05_test_dimension (invoice_id, state) VALUES ($1,$2)`, invoiceID, state); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeInvoices) SetSettlementState(context.Context, pgx.Tx, kit.Caller, string, string) error {
	f.record("SetSettlementState")
	return nil
}
func (f *fakeInvoices) SetDisputeState(context.Context, pgx.Tx, kit.Caller, string, string) error {
	f.record("SetDisputeState")
	return nil
}

func (f *fakeInvoices) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeInvoices) states(invoiceID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	prefix := "SetEInvoiceState:" + invoiceID + ":"
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c[len(prefix):])
		}
	}
	return out
}

func (f *fakeInvoices) otherCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "SetEInvoiceState:") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeInvoices) current(invoiceID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dim[invoiceID]
}

// issuedUntouched proves the issued representation is byte-for-byte what it was.
func (f *fakeInvoices) issuedUntouched(t *testing.T, tenant, invoiceID string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, f.original[invoiceID], *f.issued[tenant+"|"+invoiceID], "the issued invoice must never change")
}

// fakeProfiles stands in for the jurisdiction-rules profile resolver.
type fakeProfiles struct {
	mu      sync.Mutex
	profile Profile
	err     error
	calls   int
	last    ProfileQuery
}

func (f *fakeProfiles) Resolve(_ context.Context, _ kit.Caller, q ProfileQuery) (*Profile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = q
	if f.err != nil {
		return nil, f.err
	}
	p := f.profile
	p.Jurisdiction, p.EffectiveAt = q.Jurisdiction, q.At.UTC()
	return &p, nil
}

func (f *fakeProfiles) set(fn func(*Profile)) {
	f.mu.Lock()
	fn(&f.profile)
	f.mu.Unlock()
}

func (f *fakeProfiles) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func defaultProfile() Profile {
	to := "2026-12-31"
	_ = to
	return Profile{ProfileCode: "PEPPOL_INVOICE", Network: "PEPPOL", DocumentType: DocInvoice, ProfileVersion: "1.0", Syntax: SyntaxJSON, Mode: "EXCHANGE",
		Corrections: []string{"CREDIT_NOTE"},
		Dependencies: []Dependency{
			{Kind: "SCHEMA", Ref: "peppol.pint.json", Version: "2026.1", ValidFrom: "2026-01-01"},
			{Kind: "CODE_LIST", Ref: "iso.4217", Version: "2026.06", ValidFrom: "2026-06-01"},
		},
		SemanticMapping: []MappingEntry{
			{From: "invoice.number", To: "cbc:ID"}, {From: "invoice.issue_date", To: "cbc:IssueDate"}, {From: "invoice.due_date", To: "cbc:DueDate"},
			{From: "invoice.currency", To: "cbc:DocumentCurrencyCode"}, {From: "invoice.net_total", To: "cbc:TaxExclusiveAmount"},
			{From: "invoice.tax_total", To: "cbc:TaxAmount"}, {From: "invoice.gross_total", To: "cbc:PayableAmount"},
			{From: "invoice.seller.legal_entity_id", To: "cac:AccountingSupplierParty"}, {From: "invoice.buyer.party_id", To: "cac:AccountingCustomerParty"},
			{From: "invoice.line.number", To: "cbc:LineID"}, {From: "invoice.line.description", To: "cbc:Description"}, {From: "invoice.line.net", To: "cbc:LineExtensionAmount"},
		},
		LifecycleStates: []string{"SUBMITTED", "PROCESSING", "ACCEPTED", "REJECTED"},
		PackRef:         "pack-gb", PackVersion: "2026.3", PackVersionID: "pv-1", PackDigest: "sha256:aa", RuleID: "rule-1", RuleDigest: "sha256:bb", DecisionID: "dec-1", RuleJurisdiction: "GB"}
}

type fakeEntJur struct {
	mu   sync.Mutex
	code string
	err  error
}

func (f *fakeEntJur) PrimaryJurisdiction(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.code, f.err
}

// fakeRegistry stands in for jurisdiction-rules-svc's regulatory submission registry.
type fakeRegistry struct {
	mu       sync.Mutex
	err      error
	regs     []RegistryRegistration
	events   []RegistryEvent
	eventErr error
}

func (f *fakeRegistry) Register(_ context.Context, _ kit.Caller, r RegistryRegistration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.regs = append(f.regs, r)
	return "reg-" + r.Key, nil
}

func (f *fakeRegistry) RecordEvent(_ context.Context, _ kit.Caller, _ string, e RegistryEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.eventErr != nil {
		return f.eventErr
	}
	f.events = append(f.events, e)
	return nil
}

// ── http plumbing ────────────────────────────────────────────────────────────

func testRouter(m *Module) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	m.Register(r)
	return r
}

func testDeps(authz kit.Authz, clk func() time.Time) kit.Deps {
	return kit.Deps{Log: zap.NewNop(), Authz: authz, Entities: fakeEntities{}, Clock: clk}
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

type respBody struct {
	Submission          *Submission     `json:"submission"`
	AvailableActions    []string        `json:"available_actions"`
	Existing            bool            `json:"existing"`
	DuplicateSuppressed bool            `json:"duplicate_suppressed"`
	Outcome             string          `json:"outcome"`
	Code                string          `json:"code"`
	ExternalAttemptID   string          `json:"external_attempt_id"`
	Retried             *bool           `json:"retried"`
	Reconciliation      *Reconciliation `json:"reconciliation"`
}

func asResp(t *testing.T, body []byte) respBody {
	t.Helper()
	var r respBody
	require.NoError(t, json.Unmarshal(body, &r), string(body))
	return r
}

// sampleInvoice is an issued invoice: 2 x 500.00 + 1 x 250.50 = net 1250.50, tax 20% = 250.10, gross 1500.60 USD.
func sampleInvoice(entity string) *kit.IssuedInvoice {
	return &kit.IssuedInvoice{InvoiceID: uuid.NewString(), LegalEntityID: entity, CustomerPartyID: uuid.NewString(), InvoiceNumber: "INV-000042", Currency: "USD",
		Net: kit.MustAmount("1250.50"), Tax: kit.MustAmount("250.10"), Gross: kit.MustAmount("1500.60"),
		IssueDate: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), DueDate: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
		TaxDeterminationID: "det-1", TaxRulePackVersion: "snap-2026.1", ContentHash: "c0ffee" + uuid.NewString()[:8],
		Lines: []kit.IssuedInvoiceLine{
			{LineNumber: 1, Description: "Platform licence", Quantity: kit.MustAmount("2"), UnitPrice: kit.MustAmount("500.00"), Net: kit.MustAmount("1000.00"), Tax: kit.MustAmount("200.00"), TaxCode: "STANDARD"},
			{LineNumber: 2, Description: "Onboarding", Quantity: kit.MustAmount("1"), UnitPrice: kit.MustAmount("250.50"), Net: kit.MustAmount("250.50"), Tax: kit.MustAmount("50.10"), TaxCode: "STANDARD"},
		}}
}

func (f *fakeInvoices) statesCount(invoiceID, state string) int {
	n := 0
	for _, s := range f.states(invoiceID) {
		if s == state {
			n++
		}
	}
	return n
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
