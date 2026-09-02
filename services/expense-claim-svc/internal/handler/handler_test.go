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

	authzpkg "zoiko.io/expense-claim-svc/internal/authz"
	"zoiko.io/expense-claim-svc/internal/documentvault"
	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/employeemaster"
	"zoiko.io/expense-claim-svc/internal/events"
	"zoiko.io/expense-claim-svc/internal/handler"
	"zoiko.io/expense-claim-svc/internal/middleware"
	"zoiko.io/expense-claim-svc/internal/payableopenitem"
	"zoiko.io/expense-claim-svc/internal/policy"
	"zoiko.io/expense-claim-svc/internal/tax"
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

// ── stub employee-master-svc client ─────────────────────────────────────────

type stubEmployee struct {
	active map[string]string // claimantID -> legalEntityID, if ACTIVE
}

func newStubEmployee() *stubEmployee { return &stubEmployee{active: map[string]string{}} }

func (e *stubEmployee) addActive(claimantID, legalEntityID string) {
	e.active[claimantID] = legalEntityID
}

func (e *stubEmployee) VerifyActiveClaimant(_ context.Context, _, legalEntityID, claimantID string) error {
	le, ok := e.active[claimantID]
	if !ok || le != legalEntityID {
		return domain.ErrClaimantNotEligible
	}
	return nil
}

var _ employeemaster.Client = (*stubEmployee)(nil)

// ── stub document-vault-svc client ──────────────────────────────────────────

type stubDoc struct {
	Status        string
	TenantID      string
	LegalEntityID string
}

type stubDocs struct{ docs map[string]stubDoc }

func newStubDocs() *stubDocs { return &stubDocs{docs: map[string]stubDoc{}} }

func (d *stubDocs) add(id, tenantID, legalEntityID, status string) {
	d.docs[id] = stubDoc{Status: status, TenantID: tenantID, LegalEntityID: legalEntityID}
}

func (d *stubDocs) VerifyReceipt(_ context.Context, _, tenantID, legalEntityID, documentID string) error {
	doc, ok := d.docs[documentID]
	if !ok {
		return domain.ErrDocumentNotFound
	}
	if doc.TenantID != tenantID || doc.LegalEntityID != legalEntityID {
		return domain.ErrDocumentMismatch
	}
	if doc.Status == "PURGE_PENDING" {
		return domain.ErrDocumentNotUsable
	}
	return nil
}

var _ documentvault.Client = (*stubDocs)(nil)

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
	return &tax.Result{DeterminationID: "det-" + r.TransactionID, TaxableAmount: r.GrossAmount, CalculatedTaxAmount: r.GrossAmount * 0.1}, nil
}

var _ tax.Client = (*stubTax)(nil)

// ── stub policy-svc client ──────────────────────────────────────────────────

type stubPolicy struct {
	result string // "" defaults to WITHIN_THRESHOLD
}

func (p *stubPolicy) EvaluateApprovalThreshold(_ context.Context, _, _, _ string, _ float64) (string, string, error) {
	if p.result == "" {
		return string(domain.PolicyWithinThreshold), "policy-version-1", nil
	}
	return p.result, "policy-version-1", nil
}

var _ policy.Client = (*stubPolicy)(nil)

// ── stub payable-open-item-svc (AP-08) client ───────────────────────────────

type stubPayable struct {
	fail  bool
	calls int
}

func (p *stubPayable) CreatePayableFromApprovedSource(_ context.Context, _, _ string, req payableopenitem.CreatePayableRequest) (*payableopenitem.PayableOpenItem, error) {
	p.calls++
	if p.fail {
		return nil, domain.ErrPayableServiceUnavailable
	}
	return &payableopenitem.PayableOpenItem{PayableID: "payable-" + req.SourceReference, Status: "OPEN"}, nil
}

var _ payableopenitem.Client = (*stubPayable)(nil)

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap07-1"
const testLegalEntity = "le-ap07-1"
const testClaimant = "principal-claimant"

func newTestRouterWithPayable(st *stubStore, pub *stubPublisher, az *stubAuthz, emp *stubEmployee, docs *stubDocs, tx *stubTax, pol *stubPolicy, payable *stubPayable) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, emp, docs, tx, pol, payable, handler.Config{ReceiptRequiredThreshold: 25.0}, logger)
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, emp *stubEmployee, docs *stubDocs, tx *stubTax, pol *stubPolicy) chi.Router {
	return newTestRouterWithPayable(st, pub, az, emp, docs, tx, pol, &stubPayable{})
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
	return doRequestAs(r, method, path, body, tenantID, "principal-approver")
}

func createClaim(t *testing.T, r http.Handler, emp *stubEmployee) *domain.ExpenseClaim {
	t.Helper()
	emp.addActive(testClaimant, testLegalEntity)
	req := domain.CreateExpenseClaimRequest{
		LegalEntityID: testLegalEntity, ClaimantPrincipalID: testClaimant, Currency: "USD",
		BusinessPurpose: "client dinner", ProjectCostCenter: "CC-100",
	}
	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/", req, testTenant, testClaimant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createClaim: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var c domain.ExpenseClaim
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	return &c
}

func addLine(t *testing.T, r http.Handler, claimID string, req domain.AddExpenseLineRequest) *domain.ExpenseLine {
	t.Helper()
	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+claimID+"/lines", req, testTenant, testClaimant)
	if w.Code != http.StatusCreated {
		t.Fatalf("addLine: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var l domain.ExpenseLine
	_ = json.Unmarshal(w.Body.Bytes(), &l)
	return &l
}

func newLineReq(amount float64) domain.AddExpenseLineRequest {
	return domain.AddExpenseLineRequest{
		Merchant: "Acme Diner", ExpenseDate: time.Now().UTC(), Amount: amount, Currency: "USD", Category: "MEALS",
	}
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreateExpenseClaim_Draft(t *testing.T) {
	emp := newStubEmployee()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)
	if c.Status != domain.StatusDraft {
		t.Fatalf("expected DRAFT, got %s", c.Status)
	}
}

func TestCreateExpenseClaim_ClaimantNotEligible_Rejected(t *testing.T) {
	emp := newStubEmployee() // no active claimants registered
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	req := domain.CreateExpenseClaimRequest{LegalEntityID: testLegalEntity, ClaimantPrincipalID: "nobody", Currency: "USD"}
	w := doRequest(r, http.MethodPost, "/ap07/expense-claims/", req, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for ineligible claimant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAddExpenseLine_WithVerifiedReceipt(t *testing.T) {
	emp := newStubEmployee()
	docs := newStubDocs()
	docs.add("doc-1", testTenant, testLegalEntity, "ACTIVE")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, docs, &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)

	req := newLineReq(50)
	req.ReceiptDocumentID = "doc-1"
	line := addLine(t, r, c.ClaimID, req)
	if line.ReceiptDocumentID != "doc-1" {
		t.Fatalf("expected receipt attached, got %q", line.ReceiptDocumentID)
	}
}

func TestAddExpenseLine_UnknownReceipt_Rejected(t *testing.T) {
	emp := newStubEmployee()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)

	req := newLineReq(50)
	req.ReceiptDocumentID = "does-not-exist"
	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/lines", req, testTenant, testClaimant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown receipt, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAddExpenseLine_DuplicateReceipt_Rejected is negative-path scenario #2.
func TestAddExpenseLine_DuplicateReceipt_Rejected(t *testing.T) {
	emp := newStubEmployee()
	docs := newStubDocs()
	docs.add("doc-shared", testTenant, testLegalEntity, "ACTIVE")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, docs, &stubTax{}, &stubPolicy{})

	claim1 := createClaim(t, r, emp)
	req := newLineReq(50)
	req.ReceiptDocumentID = "doc-shared"
	addLine(t, r, claim1.ClaimID, req)

	claim2 := createClaim(t, r, emp)
	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+claim2.ClaimID+"/lines", req, testTenant, testClaimant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate receipt across claims, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitExpenseClaim_TaxRecoveryLine_CallsRealDetermination(t *testing.T) {
	emp := newStubEmployee()
	tx := &stubTax{}
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), tx, &stubPolicy{})
	c := createClaim(t, r, emp)

	req := newLineReq(100)
	req.ClaimTaxRecovery = true
	req.Jurisdiction = "US-CA"
	req.TaxCategory = "STANDARD"
	addLine(t, r, c.ClaimID, req)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 submitting, got %d: %s", w.Code, w.Body.String())
	}
	if tx.calls != 1 {
		t.Fatalf("expected exactly one real tax determination call, got %d", tx.calls)
	}
}

// TestSubmitExpenseClaim_TaxDeterminationFails_Blocked is negative-path
// scenario #4: never infer a tax reclaim without a real TAX result.
func TestSubmitExpenseClaim_TaxDeterminationFails_Blocked(t *testing.T) {
	emp := newStubEmployee()
	tx := &stubTax{fail: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), tx, &stubPolicy{})
	c := createClaim(t, r, emp)

	req := newLineReq(100)
	req.ClaimTaxRecovery = true
	req.Jurisdiction = "US-CA"
	req.TaxCategory = "STANDARD"
	addLine(t, r, c.ClaimID, req)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 blocked submission on tax determination failure, got %d: %s", w.Code, w.Body.String())
	}
}

// TestApproveExpenseClaim_SelfApproval_Denied is negative-path scenario #1.
func TestApproveExpenseClaim_SelfApproval_Denied(t *testing.T) {
	emp := newStubEmployee()
	az := &stubAuthz{sodRules: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(10))
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/approve", nil, testTenant, testClaimant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval denied, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveExpenseClaim_IndependentApprover_Succeeds(t *testing.T) {
	emp := newStubEmployee()
	az := &stubAuthz{sodRules: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(10))
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/approve", nil, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 independent approval, got %d: %s", w.Code, w.Body.String())
	}
	var approved domain.ExpenseClaim
	_ = json.Unmarshal(w.Body.Bytes(), &approved)
	if approved.Status != domain.StatusReimbursable {
		t.Fatalf("expected REIMBURSABLE, got %s", approved.Status)
	}
}

// TestApproveExpenseClaim_CreatesRealAP08Payable is the first real
// consumer expense-claim-svc has ever had for its approved claims —
// replacing the previously-unconsumed EXPENSE_CLAIM_PAYABLE_REQUESTED
// event with a real call to payable-open-item-svc (AP-08).
func TestApproveExpenseClaim_CreatesRealAP08Payable(t *testing.T) {
	emp := newStubEmployee()
	payable := &stubPayable{}
	r := newTestRouterWithPayable(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, emp, newStubDocs(), &stubTax{}, &stubPolicy{}, payable)
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(10))
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/approve", nil, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if payable.calls != 1 {
		t.Fatalf("expected exactly one real CreatePayableFromApprovedSource call to AP-08, got %d", payable.calls)
	}
}

// TestApproveExpenseClaim_AP08Unavailable_ApprovalStillStands verifies the
// AP-08 call is genuinely best-effort — mirroring goods-service-receipt-svc's
// own GRNI-posting doctrine — and never undoes an approval that already
// succeeded.
func TestApproveExpenseClaim_AP08Unavailable_ApprovalStillStands(t *testing.T) {
	emp := newStubEmployee()
	payable := &stubPayable{fail: true}
	r := newTestRouterWithPayable(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, emp, newStubDocs(), &stubTax{}, &stubPolicy{}, payable)
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(10))
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/approve", nil, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 approval to stand despite AP-08 failure, got %d: %s", w.Code, w.Body.String())
	}
	var approved domain.ExpenseClaim
	_ = json.Unmarshal(w.Body.Bytes(), &approved)
	if approved.Status != domain.StatusReimbursable {
		t.Fatalf("expected REIMBURSABLE despite AP-08 failure, got %s", approved.Status)
	}
}

// TestApproveExpenseClaim_OverThresholdNoReceipt_Blocked is negative-path
// scenario #3.
func TestApproveExpenseClaim_OverThresholdNoReceipt_Blocked(t *testing.T) {
	emp := newStubEmployee()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(100)) // over the 25.0 threshold, no receipt
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/approve", nil, testTenant, "principal-approver")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 missing required receipt, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveExpenseClaim_PolicyExceptionWaivesReceiptRequirement(t *testing.T) {
	emp := newStubEmployee()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(100))
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/policy-exception",
		domain.RecordPolicyExceptionRequest{Reason: "receipt lost, manager approved"}, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 recording exception, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/approve", nil, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 approval after exception, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveExpenseClaim_ApprovalRequired_UsesExceptionAction(t *testing.T) {
	emp := newStubEmployee()
	az := &stubAuthz{}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, emp, newStubDocs(), &stubTax{}, &stubPolicy{result: string(domain.PolicyApprovalRequired)})
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(10))
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/approve", nil, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if az.lastAction != handler.ExpenseExceptionApprove {
		t.Fatalf("expected APPROVAL_REQUIRED claim to check %s, got %s", handler.ExpenseExceptionApprove, az.lastAction)
	}
}

func TestRejectExpenseClaim(t *testing.T) {
	emp := newStubEmployee()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(10))
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/reject",
		domain.RejectClaimRequest{Reason: "not a business expense"}, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 rejecting, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReturnForCorrection_ThenResubmit(t *testing.T) {
	emp := newStubEmployee()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)
	addLine(t, r, c.ClaimID, newLineReq(10))
	doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/return",
		domain.ReturnClaimRequest{Reason: "wrong cost center"}, testTenant, "principal-approver")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 returning, got %d: %s", w.Code, w.Body.String())
	}
	var returned domain.ExpenseClaim
	_ = json.Unmarshal(w.Body.Bytes(), &returned)
	if returned.Status != domain.StatusReturned {
		t.Fatalf("expected RETURNED, got %s", returned.Status)
	}

	w = doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/submit", nil, testTenant, testClaimant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 resubmitting, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCancelExpenseClaim(t *testing.T) {
	emp := newStubEmployee()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)

	w := doRequestAs(r, http.MethodPost, "/ap07/expense-claims/"+c.ClaimID+"/cancel",
		domain.CancelClaimRequest{Reason: "duplicate entry"}, testTenant, testClaimant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 cancelling, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetAvailableActions_Draft(t *testing.T) {
	emp := newStubEmployee()
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)

	w := doRequest(r, http.MethodGet, "/ap07/expense-claims/"+c.ClaimID+"/available-actions", nil, testTenant)
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
	if !found["AddExpenseLine"] || !found["SubmitExpenseClaim"] || !found["CancelExpenseClaim"] {
		t.Fatalf("expected DRAFT claim to allow add-line/submit/cancel, got %v", resp.AvailableActions)
	}
	if found["ApproveExpenseClaim"] {
		t.Fatalf("did not expect ApproveExpenseClaim available on a DRAFT claim, got %v", resp.AvailableActions)
	}
}

func TestGetDuplicateReceiptAssessment(t *testing.T) {
	emp := newStubEmployee()
	docs := newStubDocs()
	docs.add("doc-check", testTenant, testLegalEntity, "ACTIVE")
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, emp, docs, &stubTax{}, &stubPolicy{})
	c := createClaim(t, r, emp)
	req := newLineReq(30)
	req.ReceiptDocumentID = "doc-check"
	addLine(t, r, c.ClaimID, req)

	w := doRequest(r, http.MethodGet, "/ap07/receipts/doc-check/duplicate-assessment", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		InUse bool `json:"in_use"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.InUse {
		t.Fatalf("expected in_use=true for an already-attached receipt")
	}
}

func TestGetExpenseClaim_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubEmployee(), newStubDocs(), &stubTax{}, &stubPolicy{})
	w := doRequest(r, http.MethodGet, "/ap07/expense-claims/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateExpenseClaim_AuthorizationDenied(t *testing.T) {
	emp := newStubEmployee()
	emp.addActive(testClaimant, testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{deny: true}, emp, newStubDocs(), &stubTax{}, &stubPolicy{})
	req := domain.CreateExpenseClaimRequest{LegalEntityID: testLegalEntity, ClaimantPrincipalID: testClaimant, Currency: "USD"}
	w := doRequest(r, http.MethodPost, "/ap07/expense-claims/", req, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}
