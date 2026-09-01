package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/payment-proposal-svc/internal/accountspayable"
	authzpkg "zoiko.io/payment-proposal-svc/internal/authz"
	"zoiko.io/payment-proposal-svc/internal/domain"
	"zoiko.io/payment-proposal-svc/internal/events"
	"zoiko.io/payment-proposal-svc/internal/handler"
	"zoiko.io/payment-proposal-svc/internal/middleware"
	"zoiko.io/payment-proposal-svc/internal/payableopenitem"
	"zoiko.io/payment-proposal-svc/internal/supplierprofile"
	"zoiko.io/payment-proposal-svc/internal/tax"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub authz — including the own-object SoD layer ─────────────────────────

type stubAuthz struct {
	deny       bool
	sodRules   bool
	lastAction string
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, actionType string) error {
	a.lastAction = actionType
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func (a *stubAuthz) CheckAllowedOwnObject(_ context.Context, principalID, _, actionType, resourceOwnerPrincipalID string) error {
	a.lastAction = actionType
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	if a.sodRules && principalID == resourceOwnerPrincipalID {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── stub accounts-payable-svc client ────────────────────────────────────────

type stubAP struct {
	invoices map[string]accountspayable.Invoice
}

func newStubAP() *stubAP { return &stubAP{invoices: map[string]accountspayable.Invoice{}} }

func (a *stubAP) add(id, tenantID, legalEntityID, vendorID, status string, amount float64) {
	a.invoices[id] = accountspayable.Invoice{
		InvoiceID: id, TenantID: tenantID, LegalEntityID: legalEntityID, VendorID: vendorID,
		Amount: amount, CurrencyCode: "USD", DueDate: time.Now().UTC().Add(30 * 24 * time.Hour), Status: status,
	}
}

func (a *stubAP) GetEligibleInvoice(_ context.Context, tenantID, legalEntityID, invoiceID string) (*accountspayable.Invoice, error) {
	inv, ok := a.invoices[invoiceID]
	if !ok || inv.TenantID != tenantID || inv.LegalEntityID != legalEntityID || inv.Status != "APPROVED" {
		return nil, domain.ErrPayableNotEligible
	}
	return &inv, nil
}

var _ accountspayable.Client = (*stubAP)(nil)

// ── stub payable-open-item-svc (AP-08) client ───────────────────────────────

type stubPayables struct {
	payables map[string]payableopenitem.Payable
}

func newStubPayables() *stubPayables {
	return &stubPayables{payables: map[string]payableopenitem.Payable{}}
}

func (c *stubPayables) add(id, legalEntityID, payeeRef, status string, isHeld, isDisputed bool, residual float64) {
	c.payables[id] = payableopenitem.Payable{
		PayableID: "payable-" + id, LegalEntityID: legalEntityID, SourceReference: id, PayeeRef: payeeRef,
		Currency: "USD", Status: status, IsHeld: isHeld, IsDisputed: isDisputed, ResidualAmount: residual,
	}
}

func (c *stubPayables) GetEligiblePayable(_ context.Context, _, legalEntityID, claimID string) (*payableopenitem.Payable, error) {
	p, ok := c.payables[claimID]
	if !ok || p.LegalEntityID != legalEntityID || p.Status != "OPEN" && p.Status != "PARTIALLY_SETTLED" || p.ResidualAmount <= 0 {
		return nil, domain.ErrPayableNotEligible
	}
	return &p, nil
}

var _ payableopenitem.Client = (*stubPayables)(nil)

// ── stub supplier-financial-profile-svc client ──────────────────────────────

type stubSupplier struct {
	profiles map[string]supplierprofile.Profile
}

func newStubSupplier() *stubSupplier {
	return &stubSupplier{profiles: map[string]supplierprofile.Profile{}}
}

func (s *stubSupplier) set(legalEntityID, supplierRef, status string, updatedAt time.Time, taxWithholdingRef string) {
	s.profiles[legalEntityID+"|"+supplierRef] = supplierprofile.Profile{
		ProfileID: "profile-" + supplierRef, LegalEntityID: legalEntityID, SupplierRef: supplierRef,
		Status: status, UpdatedAt: updatedAt, TaxWithholdingRef: taxWithholdingRef,
	}
}

func (s *stubSupplier) FindActiveProfile(_ context.Context, _, legalEntityID, supplierRef string) (*supplierprofile.Profile, error) {
	p, ok := s.profiles[legalEntityID+"|"+supplierRef]
	if !ok {
		return nil, domain.ErrPayeeNotFound
	}
	return &p, nil
}

var _ supplierprofile.Client = (*stubSupplier)(nil)

// ── stub tax-determination-svc client ───────────────────────────────────────

type stubTax struct {
	fail  bool
	calls int
}

func (t *stubTax) Determine(_ context.Context, _ string, r tax.DetermineRequest) (*tax.Result, error) {
	t.calls++
	if t.fail {
		return nil, domain.ErrTaxDeterminationFailed
	}
	return &tax.Result{DeterminationID: "det-" + r.TransactionID, CalculatedTaxAmount: r.GrossAmount * 0.1}, nil
}

var _ tax.Client = (*stubTax)(nil)

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap09-1"
const testLegalEntity = "le-ap09-1"
const testPreparer = "principal-preparer"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, ap *stubAP, payables *stubPayables, sup *stubSupplier, tx *stubTax) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, ap, payables, sup, tx, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func doRequestAs(r http.Handler, method, path string, body interface{}, tenantID, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doRequest(r http.Handler, method, path string, body interface{}, tenantID string) *httptest.ResponseRecorder {
	return doRequestAs(r, method, path, body, tenantID, testPreparer)
}

func createProposal(t *testing.T, r http.Handler) *domain.PaymentProposal {
	t.Helper()
	req := domain.CreateProposalRequest{
		LegalEntityID: testLegalEntity, PayingBankAccountRef: "bank-acct-1", Currency: "USD",
		PaymentDate: time.Now().UTC().Add(7 * 24 * time.Hour), PaymentMethod: "ACH",
	}
	w := doRequest(r, http.MethodPost, "/ap09/proposals/", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createProposal: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var p domain.PaymentProposal
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return &p
}

func addAPInvoiceItem(t *testing.T, r http.Handler, proposalID string, req domain.AddEligiblePayableRequest) *domain.ProposalItem {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/ap09/proposals/"+proposalID+"/items", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("addItem: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var item domain.ProposalItem
	_ = json.Unmarshal(w.Body.Bytes(), &item)
	return &item
}

func recalcAndReview(t *testing.T, r http.Handler, proposalID string) {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/ap09/proposals/"+proposalID+"/recalculate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("recalculate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	w = doRequest(r, http.MethodPost, "/ap09/proposals/"+proposalID+"/submit-for-review", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("submit-for-review: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreateProposal_Draft(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubAP(), newStubPayables(), newStubSupplier(), &stubTax{})
	p := createProposal(t, r)
	if p.Status != domain.StatusDraft {
		t.Fatalf("expected DRAFT, got %s", p.Status)
	}
}

func TestAddEligiblePayable_APInvoice(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-1", testTenant, testLegalEntity, "vendor-1", "APPROVED", 1000)
	sup := newStubSupplier()
	sup.set(testLegalEntity, "vendor-1", "ACTIVE", time.Now().UTC(), "")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, ap, newStubPayables(), sup, &stubTax{})
	p := createProposal(t, r)

	item := addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-1"})
	if item.GrossAmount != 1000 || item.PayeeRef != "vendor-1" {
		t.Fatalf("unexpected item: %+v", item)
	}
}

func TestAddEligiblePayable_NotEligibleInvoice_Rejected(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-2", testTenant, testLegalEntity, "vendor-1", "RECEIVED", 1000) // not yet APPROVED
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, ap, newStubPayables(), newStubSupplier(), &stubTax{})
	p := createProposal(t, r)

	w := doRequest(r, http.MethodPost, "/ap09/proposals/"+p.ProposalID+"/items",
		domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-2"}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-eligible invoice, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAddEligiblePayable_OnHoldWithoutException_Rejected is negative-path
// scenario #1.
func TestAddEligiblePayable_OnHoldWithoutException_Rejected(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-3", testTenant, testLegalEntity, "vendor-held", "APPROVED", 500)
	sup := newStubSupplier()
	sup.set(testLegalEntity, "vendor-held", "ON_HOLD", time.Now().UTC(), "")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, ap, newStubPayables(), sup, &stubTax{})
	p := createProposal(t, r)

	w := doRequest(r, http.MethodPost, "/ap09/proposals/"+p.ProposalID+"/items",
		domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-3"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on-hold without exception, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAddEligiblePayable_OnHoldWithException_Succeeds(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-4", testTenant, testLegalEntity, "vendor-held-2", "APPROVED", 500)
	sup := newStubSupplier()
	sup.set(testLegalEntity, "vendor-held-2", "ON_HOLD", time.Now().UTC(), "")
	az := &stubAuthz{}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, ap, newStubPayables(), sup, &stubTax{})
	p := createProposal(t, r)

	item := addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{
		PayableSource: domain.SourceAPInvoice, PayableID: "inv-4", ExceptionRef: "manager-override-123",
	})
	if item.ExceptionRef == "" {
		t.Fatalf("expected exception_ref recorded on item")
	}
	if az.lastAction != handler.ProposalExceptionResolve {
		t.Fatalf("expected exception-override add to check %s, got %s", handler.ProposalExceptionResolve, az.lastAction)
	}
}

// TestAddEligiblePayable_DuplicatePayable_Rejected is negative-path #2.
func TestAddEligiblePayable_DuplicatePayable_Rejected(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-5", testTenant, testLegalEntity, "vendor-2", "APPROVED", 200)
	sup := newStubSupplier()
	sup.set(testLegalEntity, "vendor-2", "ACTIVE", time.Now().UTC(), "")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, ap, newStubPayables(), sup, &stubTax{})
	p1 := createProposal(t, r)
	addAPInvoiceItem(t, r, p1.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-5"})

	p2 := createProposal(t, r)
	w := doRequest(r, http.MethodPost, "/ap09/proposals/"+p2.ProposalID+"/items",
		domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-5"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 duplicate payable, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAddEligiblePayable_ExpenseClaim(t *testing.T) {
	payables := newStubPayables()
	payables.add("claim-1", testLegalEntity, "employee-1", "OPEN", false, false, 250)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubAP(), payables, newStubSupplier(), &stubTax{})
	p := createProposal(t, r)

	item := addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceExpenseClaim, PayableID: "claim-1"})
	if item.GrossAmount != 250 || item.PayeeRef != "employee-1" {
		t.Fatalf("unexpected item: %+v", item)
	}
}

// TestAddEligiblePayable_ExpenseClaim_HeldRequiresException verifies the
// real parity gap this AP-08 switch closes: an EXPENSE_CLAIM item now gets
// the same ExceptionRef hold-override AP_INVOICE items already had —
// expense-claim-svc itself never had a hold concept for this service to
// check before AP-08 existed.
func TestAddEligiblePayable_ExpenseClaim_HeldRequiresException(t *testing.T) {
	payables := newStubPayables()
	payables.add("claim-2", testLegalEntity, "employee-2", "OPEN", true, false, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubAP(), payables, newStubSupplier(), &stubTax{})
	p := createProposal(t, r)

	w := doRequest(r, http.MethodPost, "/ap09/proposals/"+p.ProposalID+"/items",
		domain.AddEligiblePayableRequest{PayableSource: domain.SourceExpenseClaim, PayableID: "claim-2"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 held payable without exception, got %d: %s", w.Code, w.Body.String())
	}

	item := addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{
		PayableSource: domain.SourceExpenseClaim, PayableID: "claim-2", ExceptionRef: "exception-ref-1",
	})
	if item.ExceptionRef != "exception-ref-1" {
		t.Fatalf("expected exception ref recorded, got %+v", item)
	}
}

func TestRecalculateAndSubmitForReview(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-6", testTenant, testLegalEntity, "vendor-3", "APPROVED", 300)
	sup := newStubSupplier()
	sup.set(testLegalEntity, "vendor-3", "ACTIVE", time.Now().UTC(), "")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, ap, newStubPayables(), sup, &stubTax{})
	p := createProposal(t, r)
	addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-6"})

	w := doRequest(r, http.MethodPost, "/ap09/proposals/"+p.ProposalID+"/recalculate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 recalculating, got %d: %s", w.Code, w.Body.String())
	}
	var calculated domain.PaymentProposal
	_ = json.Unmarshal(w.Body.Bytes(), &calculated)
	if calculated.Status != domain.StatusCalculated || calculated.GrossAmount != 300 || calculated.NetAmount != 300 {
		t.Fatalf("unexpected calculated proposal: %+v", calculated)
	}
}

// TestFreezePaymentProposal_SelfFreeze_Denied is the fourth reuse of the
// dynamic own-object SoD layer this session.
func TestFreezePaymentProposal_SelfFreeze_Denied(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-7", testTenant, testLegalEntity, "vendor-4", "APPROVED", 100)
	sup := newStubSupplier()
	sup.set(testLegalEntity, "vendor-4", "ACTIVE", time.Now().UTC(), "")
	az := &stubAuthz{sodRules: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, ap, newStubPayables(), sup, &stubTax{})
	p := createProposal(t, r) // created (prepared) by testPreparer
	addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-7"})
	recalcAndReview(t, r, p.ProposalID)

	w := doRequestAs(r, http.MethodPost, "/ap09/proposals/"+p.ProposalID+"/freeze", nil, testTenant, testPreparer)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-freeze denied, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFreezePaymentProposal_IndependentFreezer_Succeeds(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-8", testTenant, testLegalEntity, "vendor-5", "APPROVED", 100)
	sup := newStubSupplier()
	sup.set(testLegalEntity, "vendor-5", "ACTIVE", time.Now().UTC(), "")
	az := &stubAuthz{sodRules: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, ap, newStubPayables(), sup, &stubTax{})
	p := createProposal(t, r)
	addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-8"})
	recalcAndReview(t, r, p.ProposalID)

	w := doRequestAs(r, http.MethodPost, "/ap09/proposals/"+p.ProposalID+"/freeze", nil, testTenant, "principal-checker")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 independent freeze, got %d: %s", w.Code, w.Body.String())
	}
	var frozen domain.PaymentProposal
	_ = json.Unmarshal(w.Body.Bytes(), &frozen)
	if frozen.Status != domain.StatusFrozen {
		t.Fatalf("expected FROZEN, got %s", frozen.Status)
	}
}

// TestFreezePaymentProposal_StalePayeeIdentity_Blocked is negative-path #4.
func TestFreezePaymentProposal_StalePayeeIdentity_Blocked(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-9", testTenant, testLegalEntity, "vendor-6", "APPROVED", 100)
	sup := newStubSupplier()
	initialUpdatedAt := time.Now().UTC()
	sup.set(testLegalEntity, "vendor-6", "ACTIVE", initialUpdatedAt, "")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, ap, newStubPayables(), sup, &stubTax{})
	p := createProposal(t, r)
	addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-9"})
	recalcAndReview(t, r, p.ProposalID)

	// Supplier profile changes after the item was added.
	sup.set(testLegalEntity, "vendor-6", "ACTIVE", initialUpdatedAt.Add(time.Hour), "")

	w := doRequestAs(r, http.MethodPost, "/ap09/proposals/"+p.ProposalID+"/freeze", nil, testTenant, "principal-checker")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 stale payee identity, got %d: %s", w.Code, w.Body.String())
	}
}

// TestFreezePaymentProposal_WentOnHoldSinceAdd_Blocked is negative-path #1's
// defense-in-depth half.
func TestFreezePaymentProposal_WentOnHoldSinceAdd_Blocked(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-10", testTenant, testLegalEntity, "vendor-7", "APPROVED", 100)
	sup := newStubSupplier()
	updatedAt := time.Now().UTC()
	sup.set(testLegalEntity, "vendor-7", "ACTIVE", updatedAt, "")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, ap, newStubPayables(), sup, &stubTax{})
	p := createProposal(t, r)
	addAPInvoiceItem(t, r, p.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-10"})
	recalcAndReview(t, r, p.ProposalID)

	// Same updated_at (so it's not "stale" by timestamp) but now ON_HOLD —
	// a value that couldn't have been set without bumping updated_at in
	// reality, but exercised here to isolate the hold check specifically.
	sup.set(testLegalEntity, "vendor-7", "ON_HOLD", updatedAt, "")

	w := doRequestAs(r, http.MethodPost, "/ap09/proposals/"+p.ProposalID+"/freeze", nil, testTenant, "principal-checker")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on-hold since add, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCancelProposal_FreesPayableForReselection(t *testing.T) {
	ap := newStubAP()
	ap.add("inv-11", testTenant, testLegalEntity, "vendor-8", "APPROVED", 150)
	sup := newStubSupplier()
	sup.set(testLegalEntity, "vendor-8", "ACTIVE", time.Now().UTC(), "")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, ap, newStubPayables(), sup, &stubTax{})
	p1 := createProposal(t, r)
	addAPInvoiceItem(t, r, p1.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-11"})

	w := doRequest(r, http.MethodPost, "/ap09/proposals/"+p1.ProposalID+"/cancel", domain.CancelProposalRequest{Reason: "duplicate entry"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 cancelling, got %d: %s", w.Code, w.Body.String())
	}

	p2 := createProposal(t, r)
	item := addAPInvoiceItem(t, r, p2.ProposalID, domain.AddEligiblePayableRequest{PayableSource: domain.SourceAPInvoice, PayableID: "inv-11"})
	if item.PayableID != "inv-11" {
		t.Fatalf("expected reselection to succeed after cancellation")
	}
}

func TestGetAvailableActions_Draft(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubAP(), newStubPayables(), newStubSupplier(), &stubTax{})
	p := createProposal(t, r)

	w := doRequest(r, http.MethodGet, "/ap09/proposals/"+p.ProposalID+"/available-actions", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		AvailableActions []string `json:"available_actions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	found := map[string]bool{}
	for _, a := range resp.AvailableActions {
		found[a] = true
	}
	if !found["AddEligiblePayable"] || !found["RecalculatePaymentProposal"] {
		t.Fatalf("expected DRAFT proposal to allow add/recalculate, got %v", resp.AvailableActions)
	}
	if found["FreezePaymentProposal"] {
		t.Fatalf("did not expect FreezePaymentProposal available on DRAFT, got %v", resp.AvailableActions)
	}
}

func TestGetFingerprint(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubAP(), newStubPayables(), newStubSupplier(), &stubTax{})
	p := createProposal(t, r)

	w := doRequest(r, http.MethodGet, "/ap09/proposals/"+p.ProposalID+"/fingerprint", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Fingerprint string `json:"fingerprint"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Fingerprint == "" {
		t.Fatalf("expected a non-empty fingerprint")
	}
}

func TestGetProposal_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubAP(), newStubPayables(), newStubSupplier(), &stubTax{})
	w := doRequest(r, http.MethodGet, "/ap09/proposals/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateProposal_AuthorizationDenied(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{deny: true}, newStubAP(), newStubPayables(), newStubSupplier(), &stubTax{})
	req := domain.CreateProposalRequest{LegalEntityID: testLegalEntity, PayingBankAccountRef: "acct", Currency: "USD", PaymentMethod: "ACH", PaymentDate: time.Now().UTC()}
	w := doRequest(r, http.MethodPost, "/ap09/proposals/", req, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}
