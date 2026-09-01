package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/supplier-recovery-svc/internal/authz"
	"zoiko.io/supplier-recovery-svc/internal/bankreconciliation"
	"zoiko.io/supplier-recovery-svc/internal/domain"
	"zoiko.io/supplier-recovery-svc/internal/events"
	"zoiko.io/supplier-recovery-svc/internal/handler"
	"zoiko.io/supplier-recovery-svc/internal/middleware"
	"zoiko.io/supplier-recovery-svc/internal/payableopenitem"
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
	deny     bool
	sodRules bool
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func (a *stubAuthz) CheckAllowedOwnObject(_ context.Context, principalID, _, _, resourceOwnerPrincipalID string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	if a.sodRules && principalID == resourceOwnerPrincipalID {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

// ── stub payable-open-item-svc (AP-08) client ───────────────────────────────

type stubPayables struct {
	payables    map[string]payableopenitem.Payable
	offsetCalls int
	offsetFail  bool
}

func newStubPayables() *stubPayables {
	return &stubPayables{payables: map[string]payableopenitem.Payable{}}
}

func (p *stubPayables) add(payableID, legalEntityID string, residual float64) {
	p.payables[payableID] = payableopenitem.Payable{PayableID: payableID, LegalEntityID: legalEntityID, ResidualAmount: residual, Status: "OPEN"}
}

func (p *stubPayables) GetPayable(_ context.Context, _, payableID string) (*payableopenitem.Payable, error) {
	pay, ok := p.payables[payableID]
	if !ok {
		return nil, domain.ErrCaseNotFound
	}
	return &pay, nil
}

func (p *stubPayables) ApplyRecovery(_ context.Context, _, _, payableID string, req payableopenitem.ApplyRecoveryRequest) (*payableopenitem.Payable, error) {
	p.offsetCalls++
	if p.offsetFail {
		return nil, domain.ErrOffsetFailedAtPayable
	}
	pay, ok := p.payables[payableID]
	if !ok {
		return nil, domain.ErrCaseNotFound
	}
	pay.ResidualAmount -= req.Amount
	p.payables[payableID] = pay
	return &pay, nil
}

var _ payableopenitem.Client = (*stubPayables)(nil)

// ── stub bank-reconciliation-svc client ─────────────────────────────────────

type stubBankRec struct {
	lines map[string]bankreconciliation.StatementLine
}

func newStubBankRec() *stubBankRec {
	return &stubBankRec{lines: map[string]bankreconciliation.StatementLine{}}
}

func (b *stubBankRec) add(id, legalEntityID, status string, amount float64) {
	b.lines[id] = bankreconciliation.StatementLine{StatementLineID: id, LegalEntityID: legalEntityID, Amount: amount, CurrencyCode: "USD", BankReference: "bref-" + id, Status: status}
}

func (b *stubBankRec) GetConfirmedInboundLine(_ context.Context, _, legalEntityID, statementLineID string) (*bankreconciliation.StatementLine, error) {
	line, ok := b.lines[statementLineID]
	if !ok {
		return nil, domain.ErrStatementLineNotFound
	}
	if line.LegalEntityID != legalEntityID || line.Amount <= 0 {
		return nil, domain.ErrStatementLineMismatch
	}
	if line.Status != "MATCHED" {
		return nil, domain.ErrStatementLineNotConfirmed
	}
	return &line, nil
}

var _ bankreconciliation.Client = (*stubBankRec)(nil)

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap12-1"
const testLegalEntity = "le-ap12-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, payables *stubPayables, bankrec *stubBankRec) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, payables, bankrec, logger)
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
	return doRequestAs(r, method, path, body, tenantID, "principal-case-owner")
}

func createCase(t *testing.T, r http.Handler, payableID string, amount float64) *domain.SupplierRecoveryCase {
	t.Helper()
	req := domain.CreateCaseRequest{
		LegalEntityID: testLegalEntity, SupplierRef: "vendor-1", RecoveryBasis: domain.BasisOverpayment,
		SourcePayableID: payableID, TotalAmount: amount, Currency: "USD", RecoveryReason: "duplicate payment identified",
	}
	w := doRequest(r, http.MethodPost, "/ap12/cases/", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createCase: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var c domain.SupplierRecoveryCase
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	return &c
}

func approveCase(t *testing.T, r http.Handler, caseID, ownerPrincipal string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAs(r, http.MethodPost, "/ap12/cases/"+caseID+"/approve", nil, testTenant, ownerPrincipal+"-checker")
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreateSupplierRecoveryCase(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-1", testLegalEntity, 500)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, payables, newStubBankRec())

	c := createCase(t, r, "payable-1", 500)
	if c.Status != domain.StatusOpen {
		t.Fatalf("expected OPEN, got %s", c.Status)
	}
}

func TestCreateSupplierRecoveryCase_UnknownPayable_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPayables(), newStubBankRec())
	w := doRequest(r, http.MethodPost, "/ap12/cases/", domain.CreateCaseRequest{
		LegalEntityID: testLegalEntity, SupplierRef: "vendor-1", RecoveryBasis: domain.BasisOverpayment,
		SourcePayableID: "does-not-exist", TotalAmount: 100, Currency: "USD",
	}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 unknown source payable, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveRecoveryPlan_SelfApproval_Denied(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-2", testLegalEntity, 200)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, payables, newStubBankRec())
	c := createCase(t, r, "payable-2", 200) // created by "principal-case-owner"

	w := doRequestAs(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/approve", nil, testTenant, "principal-case-owner")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval denied, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveRecoveryPlan_IndependentApprover_Succeeds(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-3", testLegalEntity, 200)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, payables, newStubBankRec())
	c := createCase(t, r, "payable-3", 200)

	w := approveCase(t, r, c.CaseID, "principal-case-owner")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 independent approval, got %d: %s", w.Code, w.Body.String())
	}
	var approved domain.SupplierRecoveryCase
	_ = json.Unmarshal(w.Body.Bytes(), &approved)
	if approved.Status != domain.StatusInRecovery {
		t.Fatalf("expected IN_RECOVERY, got %s", approved.Status)
	}
}

// TestApplyApprovedOffset_RealAP08Call is the first real caller of AP-08's
// ApplyRecovery anywhere in this codebase.
func TestApplyApprovedOffset_RealAP08Call(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-4", testLegalEntity, 500)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, payables, newStubBankRec())
	c := createCase(t, r, "payable-4", 300)
	approveCase(t, r, c.CaseID, "principal-case-owner")

	w := doRequestAs(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/apply-offset",
		domain.ApplyOffsetRequest{Amount: 300, RecoveryRef: "offset-ref-1"}, testTenant, "principal-checker")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if payables.offsetCalls != 1 {
		t.Fatalf("expected exactly one real ApplyRecovery call to AP-08, got %d", payables.offsetCalls)
	}
	var resp struct {
		Case domain.SupplierRecoveryCase `json:"case"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Case.Status != domain.StatusRecovered {
		t.Fatalf("expected RECOVERED, got %s", resp.Case.Status)
	}
}

func TestApplyApprovedOffset_SelfApproval_Denied(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-5", testLegalEntity, 500)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, payables, newStubBankRec())
	c := createCase(t, r, "payable-5", 300)
	approveCase(t, r, c.CaseID, "principal-case-owner")

	w := doRequestAs(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/apply-offset",
		domain.ApplyOffsetRequest{Amount: 300, RecoveryRef: "offset-ref-2"}, testTenant, "principal-case-owner")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval denied for offset, got %d: %s", w.Code, w.Body.String())
	}
	if payables.offsetCalls != 0 {
		t.Fatalf("expected AP-08 never called for a denied offset, got %d calls", payables.offsetCalls)
	}
}

// TestLinkConfirmedSupplierRefund_UnmatchedLine_Rejected is negative-path #1.
func TestLinkConfirmedSupplierRefund_UnmatchedLine_Rejected(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-6", testLegalEntity, 400)
	bankrec := newStubBankRec()
	bankrec.add("stmt-1", testLegalEntity, "UNMATCHED", 150)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, payables, bankrec)
	c := createCase(t, r, "payable-6", 150)
	approveCase(t, r, c.CaseID, "principal-case-owner")

	w := doRequest(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/link-refund", domain.LinkRefundRequest{StatementLineID: "stmt-1"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 unmatched statement line, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLinkConfirmedSupplierRefund_MatchedLine_Succeeds(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-7", testLegalEntity, 400)
	bankrec := newStubBankRec()
	bankrec.add("stmt-2", testLegalEntity, "MATCHED", 150)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, payables, bankrec)
	c := createCase(t, r, "payable-7", 150)
	approveCase(t, r, c.CaseID, "principal-case-owner")

	w := doRequest(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/link-refund", domain.LinkRefundRequest{StatementLineID: "stmt-2"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Case domain.SupplierRecoveryCase `json:"case"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Case.Status != domain.StatusRecovered {
		t.Fatalf("expected RECOVERED, got %s", resp.Case.Status)
	}
}

// TestCloseRecoveryCase_RequiresFullyRecovered is negative-path #4.
func TestCloseRecoveryCase_RequiresFullyRecovered(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-8", testLegalEntity, 400)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, payables, newStubBankRec())
	c := createCase(t, r, "payable-8", 300)
	approveCase(t, r, c.CaseID, "principal-case-owner")

	w := doRequest(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/close", domain.CloseCaseRequest{}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 not fully recovered, got %d: %s", w.Code, w.Body.String())
	}

	doRequestAs(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/apply-offset",
		domain.ApplyOffsetRequest{Amount: 300, RecoveryRef: "offset-ref-close"}, testTenant, "principal-checker")

	w = doRequest(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/close", domain.CloseCaseRequest{}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 closing fully recovered case, got %d: %s", w.Code, w.Body.String())
	}
}

// TestWriteOffRecovery_SelfApproval_Denied is negative-path #3.
func TestWriteOffRecovery_SelfApproval_Denied(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-9", testLegalEntity, 400)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, payables, newStubBankRec())
	c := createCase(t, r, "payable-9", 300)

	w := doRequestAs(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/write-off", domain.WriteOffRequest{Reason: "uncollectible"}, testTenant, "principal-case-owner")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approved write-off denied, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWriteOffRecovery_IndependentApprover_Succeeds(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-10", testLegalEntity, 400)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, payables, newStubBankRec())
	c := createCase(t, r, "payable-10", 300)

	w := doRequestAs(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/write-off", domain.WriteOffRequest{Reason: "uncollectible"}, testTenant, "principal-checker")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// TestApplyApprovedOffset_ReplayedRef_Idempotent verifies a replayed offset
// application is a genuine no-op and never calls AP-08 a second time.
func TestApplyApprovedOffset_ReplayedRef_Idempotent(t *testing.T) {
	payables := newStubPayables()
	payables.add("payable-11", testLegalEntity, 500)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, payables, newStubBankRec())
	c := createCase(t, r, "payable-11", 300)
	approveCase(t, r, c.CaseID, "principal-case-owner")

	req := domain.ApplyOffsetRequest{Amount: 100, RecoveryRef: "offset-ref-dup"}
	doRequestAs(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/apply-offset", req, testTenant, "principal-checker")

	w := doRequestAs(r, http.MethodPost, "/ap12/cases/"+c.CaseID+"/apply-offset", req, testTenant, "principal-checker")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Applied bool `json:"applied"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Applied {
		t.Fatalf("expected applied=false for a replayed offset reference")
	}
	if payables.offsetCalls != 1 {
		t.Fatalf("expected AP-08 called exactly once despite the replay, got %d", payables.offsetCalls)
	}
}

func TestGetRecoveryCase_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPayables(), newStubBankRec())
	w := doRequest(r, http.MethodGet, "/ap12/cases/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
