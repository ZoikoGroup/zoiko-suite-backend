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
	"zoiko.io/expense-claim-svc/internal/configflag"
	"zoiko.io/expense-claim-svc/internal/documentvault"
	"zoiko.io/expense-claim-svc/internal/domain"
	"zoiko.io/expense-claim-svc/internal/employeemaster"
	"zoiko.io/expense-claim-svc/internal/handler"
	"zoiko.io/expense-claim-svc/internal/middleware"
	"zoiko.io/expense-claim-svc/internal/payableopenitem"
	"zoiko.io/expense-claim-svc/internal/policy"
	"zoiko.io/expense-claim-svc/internal/tax"
)

const (
	tenantA      = "tenant-ap07-a"
	tenantB      = "tenant-ap07-b"
	legalEntity  = "le-ap07-1"
	claimant     = "principal-claimant"
	approver     = "principal-approver"
	otherApprove = "principal-approver-2"
)

func ver(n int) *int { return &n }

// ── stubs ────────────────────────────────────────────────────────────────────

// stubAuthz stands in for authorization-svc: a blanket deny, a per-action deny,
// "down", and — with sodRules — the own-object SoD layer that denies a
// principal acting on an object they own.
type stubAuthz struct {
	deny        bool
	down        bool
	denyActions map[string]bool
	sodRules    bool
	actions     []string
}

func (a *stubAuthz) check(action string) error {
	a.actions = append(a.actions, action)
	switch {
	case a.down:
		return authzpkg.ErrAuthzServiceUnavailable
	case a.deny || a.denyActions[action]:
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, action string) error {
	return a.check(action)
}

func (a *stubAuthz) CheckAllowedOwnObject(_ context.Context, principal, _, action, owner string) error {
	if err := a.check(action); err != nil {
		return err
	}
	if a.sodRules && principal == owner {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func (a *stubAuthz) last() string {
	if len(a.actions) == 0 {
		return ""
	}
	return a.actions[len(a.actions)-1]
}

type stubEmployee struct {
	active map[string]string
	down   bool
}

func (e *stubEmployee) VerifyActiveClaimant(_ context.Context, _, legalEntityID, claimantID string) error {
	if e.down {
		return domain.ErrClaimantServiceUnavailable
	}
	if le, ok := e.active[claimantID]; !ok || le != legalEntityID {
		return domain.ErrClaimantNotEligible
	}
	return nil
}

var _ employeemaster.Client = (*stubEmployee)(nil)

type stubDocs struct {
	docs map[string][3]string // id -> status, tenant, legal entity
	down bool
}

func (d *stubDocs) add(id, tenantID, le, status string) { d.docs[id] = [3]string{status, tenantID, le} }

func (d *stubDocs) VerifyReceipt(_ context.Context, _, tenantID, le, id string) error {
	if d.down {
		return domain.ErrDocumentServiceUnavailable
	}
	doc, ok := d.docs[id]
	if !ok {
		return domain.ErrDocumentNotFound
	}
	if doc[1] != tenantID || doc[2] != le {
		return domain.ErrDocumentMismatch
	}
	if doc[0] == "PURGE_PENDING" {
		return domain.ErrDocumentNotUsable
	}
	return nil
}

var _ documentvault.Client = (*stubDocs)(nil)

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

type stubPolicy struct {
	result string
	err    error
}

func (p *stubPolicy) EvaluateApprovalThreshold(context.Context, string, string, string, float64) (string, string, error) {
	if p.err != nil {
		return "", "", p.err
	}
	if p.result == "" {
		return string(domain.PolicyWithinThreshold), "policy-version-1", nil
	}
	return p.result, "policy-version-1", nil
}

var _ policy.Client = (*stubPolicy)(nil)

type stubPayable struct {
	status string // GetPayable status
	down   bool
	gets   int
}

func (p *stubPayable) CreatePayableFromApprovedSource(_ context.Context, _, _ string, req payableopenitem.CreatePayableRequest) (*payableopenitem.PayableOpenItem, error) {
	return &payableopenitem.PayableOpenItem{PayableID: "payable-" + req.SourceReference, Status: "OPEN"}, nil
}

func (p *stubPayable) GetPayable(_ context.Context, _, _, id string) (*payableopenitem.PayableOpenItem, error) {
	p.gets++
	if p.down {
		return nil, domain.ErrPayableServiceUnavailable
	}
	return &payableopenitem.PayableOpenItem{PayableID: id, Status: p.status}, nil
}

var _ payableopenitem.Client = (*stubPayable)(nil)

type stubConfigFlags struct {
	threshold float64
	found     bool
	err       error
	terms     float64
	termsSet  bool
	calls     int
}

func (c *stubConfigFlags) ResolveReceiptThreshold(context.Context, string, string) (float64, bool, error) {
	c.calls++
	return c.threshold, c.found, c.err
}

func (c *stubConfigFlags) ResolveReimbursementTermsDays(context.Context, string, string) (float64, bool, error) {
	return c.terms, c.termsSet, nil
}

var _ configflag.Client = (*stubConfigFlags)(nil)

// stubRelay emulates the payable relay's best-effort immediate hand-off by
// completing the claim's payable request in the store.
type stubRelay struct {
	store *stubStore
	calls []string
}

func (r *stubRelay) ProcessClaim(ctx context.Context, claimID string) {
	r.calls = append(r.calls, claimID)
	if req := r.store.payReqs[claimID]; req != nil {
		_ = r.store.CompletePayableRequest(ctx, *req, "payable-"+claimID, "payee-ref", "dest-1")
	}
}

// ── harness ──────────────────────────────────────────────────────────────────

type env struct {
	store    *stubStore
	authz    *stubAuthz
	emp      *stubEmployee
	docs     *stubDocs
	tax      *stubTax
	policy   *stubPolicy
	payable  *stubPayable
	cf       *stubConfigFlags
	relay    *stubRelay
	cfg      handler.Config
	useRelay bool
}

func newEnv() *env {
	e := &env{
		store: newStubStore(), authz: &stubAuthz{}, emp: &stubEmployee{active: map[string]string{claimant: legalEntity}},
		docs: &stubDocs{docs: map[string][3]string{}}, tax: &stubTax{}, policy: &stubPolicy{}, payable: &stubPayable{},
		cf:  &stubConfigFlags{},
		cfg: handler.Config{ReceiptRequiredThreshold: 25.0, Environment: "test"},
	}
	e.relay = &stubRelay{store: e.store}
	return e
}

func (e *env) router() chi.Router {
	d := handler.Deps{
		Store: e.store, Authz: e.authz, Employee: e.emp, Docs: e.docs, Tax: e.tax, Policy: e.policy,
		Payable: e.payable, ConfigFlags: e.cf, Config: e.cfg, Log: zap.NewNop(),
	}
	if e.useRelay {
		d.Relay = e.relay
	}
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(d))
	return r
}

type call struct {
	method, path string
	body         any
	principal    string
	tenant       string
	key          string
}

func (e *env) do(r chi.Router, c call) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if c.body != nil {
		_ = json.NewEncoder(&buf).Encode(c.body)
	}
	req := httptest.NewRequest(c.method, c.path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if c.principal != "" {
		req.Header.Set("X-Principal-Id", c.principal)
	}
	if c.tenant != "" {
		req.Header.Set("X-Tenant-Id", c.tenant)
	}
	if c.key != "" {
		req.Header.Set("Idempotency-Key", c.key)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func as(principal, method, path string, body any) call {
	return call{method: method, path: path, body: body, principal: principal, tenant: tenantA}
}

const base = "/ap07/expense-claims/"

type apiErr struct{ Error, Code string }

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Code
}

func decodeClaim(t *testing.T, rec *httptest.ResponseRecorder) domain.ExpenseClaim {
	t.Helper()
	var c domain.ExpenseClaim
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decoding claim: %v (%s)", err, rec.Body.String())
	}
	return c
}

func createReq() domain.CreateExpenseClaimRequest {
	return domain.CreateExpenseClaimRequest{
		LegalEntityID: legalEntity, ClaimantPrincipalID: claimant, Currency: "USD", BusinessPurpose: "client dinner", ProjectCostCenter: "CC-100",
	}
}

func lineReq(amount float64) domain.AddExpenseLineRequest {
	return domain.AddExpenseLineRequest{Merchant: "Acme Diner", ExpenseDate: time.Now().UTC(), Amount: amount, Currency: "USD", Category: "MEALS"}
}

func (e *env) claim(t *testing.T, r chi.Router) *domain.ExpenseClaim {
	t.Helper()
	rec := e.do(r, as(claimant, http.MethodPost, base, createReq()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	c := decodeClaim(t, rec)
	return &c
}

func (e *env) addLine(t *testing.T, r chi.Router, id string, req domain.AddExpenseLineRequest) *domain.ExpenseLine {
	t.Helper()
	rec := e.do(r, as(claimant, http.MethodPost, base+id+"/lines", req))
	if rec.Code != http.StatusCreated {
		t.Fatalf("addLine: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var l domain.ExpenseLine
	_ = json.Unmarshal(rec.Body.Bytes(), &l)
	return &l
}

func (e *env) submit(t *testing.T, r chi.Router, id string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(r, as(claimant, http.MethodPost, base+id+"/submit", nil))
}

// pending returns a claim routed to PENDING_APPROVAL with one line of amount.
func (e *env) pending(t *testing.T, r chi.Router, amount float64) *domain.ExpenseClaim {
	t.Helper()
	c := e.claim(t, r)
	e.addLine(t, r, c.ClaimID, lineReq(amount))
	if rec := e.submit(t, r, c.ClaimID); rec.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	if e.store.claims[c.ClaimID].Status != domain.StatusPendingApproval {
		t.Fatalf("expected PENDING_APPROVAL, got %s", e.store.claims[c.ClaimID].Status)
	}
	return c
}

func (e *env) approve(r chi.Router, id, principal, key string) *httptest.ResponseRecorder {
	c := as(principal, http.MethodPost, base+id+"/approve", domain.VersionedRequest{ExpectedVersion: ver(e.store.claims[id].Version)})
	c.key = key
	return e.do(r, c)
}

// ── CreateExpenseClaim ───────────────────────────────────────────────────────

func TestCreate_Draft_RecordsClaimantAndEvent(t *testing.T) {
	e := newEnv()
	c := e.claim(t, e.router())
	if c.Status != domain.StatusDraft || c.Version != 1 || c.ClaimantPrincipalID != claimant || c.TenantID == nil || *c.TenantID != tenantA {
		t.Fatalf("expected a DRAFT v1 for the claimant in the verified tenant, got %+v", c)
	}
	if len(e.store.events[c.ClaimID]) != 1 || e.store.events[c.ClaimID][0].EventType != domain.EventClaimCreated {
		t.Fatalf("expected the created event, got %v", e.store.events[c.ClaimID])
	}
	if len(e.store.outbox) != 2 || e.store.outbox[0] != "ExpenseClaimCreated" {
		t.Fatalf("expected the spec event and its alias in the outbox, got %v", e.store.outbox)
	}
}

func TestCreate_ClaimantNotEligible_Unavailable_AndValidation(t *testing.T) {
	e := newEnv()
	r := e.router()
	bad := createReq()
	bad.ClaimantPrincipalID = "nobody"
	if rec := e.do(r, as(claimant, http.MethodPost, base, bad)); rec.Code != http.StatusBadRequest || codeOf(t, rec) != "CLAIMANT_NOT_ELIGIBLE" {
		t.Fatalf("expected 400 CLAIMANT_NOT_ELIGIBLE, got %d %s", rec.Code, rec.Body.String())
	}
	e.emp.down = true
	if rec := e.do(r, as(claimant, http.MethodPost, base, createReq())); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unverifiable claimant must fail closed (503), got %d", rec.Code)
	}
	e.emp.down = false
	missing := createReq()
	missing.Currency = ""
	if rec := e.do(r, as(claimant, http.MethodPost, base, missing)); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing currency: expected 400, got %d", rec.Code)
	}
	if len(e.store.claims) != 0 {
		t.Fatal("a refused create must write nothing")
	}
}

func TestCreate_Auth_Tenant_AndIdentity(t *testing.T) {
	e := newEnv()
	r := e.router()
	e.authz.deny = true
	if rec := e.do(r, as(claimant, http.MethodPost, base, createReq())); rec.Code != http.StatusForbidden || codeOf(t, rec) != "FORBIDDEN" {
		t.Fatalf("expected 403 FORBIDDEN, got %d", rec.Code)
	}
	e.authz.deny, e.authz.down = false, true
	if rec := e.do(r, as(claimant, http.MethodPost, base, createReq())); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authorization-svc down must fail closed (503), got %d", rec.Code)
	}
	e.authz.down = false
	if rec := e.do(r, call{method: http.MethodPost, path: base, body: createReq(), tenant: tenantA}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no principal: expected 401, got %d", rec.Code)
	}
	if rec := e.do(r, call{method: http.MethodPost, path: base, body: createReq(), principal: claimant}); rec.Code != http.StatusBadRequest || codeOf(t, rec) != "VALIDATION_FAILED" {
		t.Fatalf("no tenant scope: expected 400 VALIDATION_FAILED, got %d", rec.Code)
	}
	if len(e.store.claims) != 0 {
		t.Fatal("refused creates must write nothing")
	}
}

func TestCreate_IdempotencyKey_ReplaysAndRejectsReuse(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := as(claimant, http.MethodPost, base, createReq())
	c.key = "key-1"
	first := e.do(r, c)
	second := e.do(r, c)
	if first.Code != http.StatusCreated || second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("expected 201 then a replay, got %d / replay=%q", first.Code, second.Header().Get("Idempotent-Replay"))
	}
	if len(e.store.claims) != 1 || decodeClaim(t, first).ClaimID != decodeClaim(t, second).ClaimID {
		t.Fatalf("a replay must resolve to the same single claim, got %d claims", len(e.store.claims))
	}
	other := createReq()
	other.BusinessPurpose = "something else"
	c2 := as(claimant, http.MethodPost, base, other)
	c2.key = "key-1"
	if rec := e.do(r, c2); rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("expected 422 IDEMPOTENCY_KEY_REUSED, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestGet_NotFound_And_CrossTenantIsAbsent(t *testing.T) {
	e := newEnv()
	r := e.router()
	if rec := e.do(r, as(approver, http.MethodGet, base+"does-not-exist", nil)); rec.Code != http.StatusNotFound || codeOf(t, rec) != "NOT_FOUND" {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	c := e.pending(t, r, 10)
	if rec := e.do(r, call{method: http.MethodGet, path: base + c.ClaimID, principal: approver, tenant: tenantB}); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant's claim must look absent, got %d", rec.Code)
	}
	if rec := e.do(r, call{method: http.MethodPost, path: base + c.ClaimID + "/cancel", body: domain.CancelClaimRequest{Reason: "x"}, principal: claimant, tenant: tenantB}); rec.Code != http.StatusNotFound {
		t.Fatalf("another tenant must not be able to command it, got %d", rec.Code)
	}
	if e.store.claims[c.ClaimID].Status != domain.StatusPendingApproval {
		t.Fatal("a cross-tenant command must change nothing")
	}
}

// ── AddExpenseLine / void ────────────────────────────────────────────────────

func TestAddLine_VerifiedReceipt_AndRefusals(t *testing.T) {
	e := newEnv()
	r := e.router()
	e.docs.add("doc-1", tenantA, legalEntity, "ACTIVE")
	e.docs.add("doc-other-tenant", tenantB, legalEntity, "ACTIVE")
	e.docs.add("doc-purged", tenantA, legalEntity, "PURGE_PENDING")
	c := e.claim(t, r)

	req := lineReq(50)
	req.ReceiptDocumentID = "doc-1"
	if l := e.addLine(t, r, c.ClaimID, req); l.ReceiptDocumentID != "doc-1" {
		t.Fatalf("expected the receipt attached, got %q", l.ReceiptDocumentID)
	}
	for doc, want := range map[string]int{"does-not-exist": 400, "doc-other-tenant": 403, "doc-purged": 409} {
		bad := lineReq(5)
		bad.ReceiptDocumentID = doc
		if rec := e.do(r, as(claimant, http.MethodPost, base+c.ClaimID+"/lines", bad)); rec.Code != want {
			t.Fatalf("%s: expected %d, got %d %s", doc, want, rec.Code, rec.Body.String())
		}
	}
	e.docs.down = true
	down := lineReq(5)
	down.ReceiptDocumentID = "doc-1"
	if rec := e.do(r, as(claimant, http.MethodPost, base+c.ClaimID+"/lines", down)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unverifiable receipt must fail closed (503), got %d", rec.Code)
	}
}

// Negative path 2: the same receipt used on two claims.
func TestAddLine_SameReceiptOnTwoClaims_Refused_UntilVoided(t *testing.T) {
	e := newEnv()
	r := e.router()
	e.docs.add("doc-shared", tenantA, legalEntity, "ACTIVE")
	c1, c2 := e.claim(t, r), e.claim(t, r)
	req := lineReq(50)
	req.ReceiptDocumentID = "doc-shared"
	l1 := e.addLine(t, r, c1.ClaimID, req)

	rec := e.do(r, as(claimant, http.MethodPost, base+c2.ClaimID+"/lines", req))
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "DUPLICATE_RISK" {
		t.Fatalf("expected 409 DUPLICATE_RISK, got %d %s", rec.Code, rec.Body.String())
	}
	assess := e.do(r, as(approver, http.MethodGet, "/ap07/receipts/doc-shared/duplicate-assessment?legal_entity_id="+legalEntity, nil))
	var a struct {
		InUse   bool   `json:"in_use"`
		ClaimID string `json:"claim_id"`
	}
	_ = json.Unmarshal(assess.Body.Bytes(), &a)
	if !a.InUse || a.ClaimID != c1.ClaimID {
		t.Fatalf("the duplicate assessment must name the claim holding the receipt, got %+v", a)
	}

	// Voiding (never deleting) the first line frees the receipt for a corrected claim.
	if rec := e.do(r, as(claimant, http.MethodPost, base+c1.ClaimID+"/lines/"+l1.LineID+"/void", domain.VoidLineRequest{Reason: "wrong claim"})); rec.Code != http.StatusOK {
		t.Fatalf("void: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(r, as(claimant, http.MethodPost, base+c2.ClaimID+"/lines", req)); rec.Code != http.StatusCreated {
		t.Fatalf("a voided line frees its receipt, got %d %s", rec.Code, rec.Body.String())
	}
	if len(e.store.lines[c1.ClaimID]) != 1 || e.store.lines[c1.ClaimID][0].VoidedAt == nil {
		t.Fatal("a voided line is kept as evidence, never deleted")
	}
	if rec := e.do(r, as(claimant, http.MethodPost, base+c1.ClaimID+"/lines/"+l1.LineID+"/void", domain.VoidLineRequest{})); rec.Code != http.StatusBadRequest {
		t.Fatalf("a void needs a reason, got %d", rec.Code)
	}
}

func TestAddLine_Validation_Currency_AndNotEditable(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.claim(t, r)
	for name, mod := range map[string]func(*domain.AddExpenseLineRequest){
		"no merchant":         func(l *domain.AddExpenseLineRequest) { l.Merchant = "" },
		"zero amount":         func(l *domain.AddExpenseLineRequest) { l.Amount = 0 },
		"no currency":         func(l *domain.AddExpenseLineRequest) { l.Currency = "" },
		"no date":             func(l *domain.AddExpenseLineRequest) { l.ExpenseDate = time.Time{} },
		"reclaim without tax": func(l *domain.AddExpenseLineRequest) { l.ClaimTaxRecovery = true },
		"currency != claim's": func(l *domain.AddExpenseLineRequest) { l.Currency = "EUR" },
	} {
		l := lineReq(10)
		mod(&l)
		if rec := e.do(r, as(claimant, http.MethodPost, base+c.ClaimID+"/lines", l)); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d: %s", name, rec.Code, rec.Body.String())
		}
	}
	p := e.pending(t, r, 10)
	if rec := e.do(r, as(claimant, http.MethodPost, base+p.ClaimID+"/lines", lineReq(5))); rec.Code != http.StatusConflict {
		t.Fatalf("a claim under review accepts no new lines, got %d", rec.Code)
	}
}

// ── SubmitExpenseClaim ───────────────────────────────────────────────────────

func TestSubmit_FreezesSnapshot_RoutesToPendingApproval_RecordsPolicy(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.pending(t, r, 10)
	got := e.store.claims[c.ClaimID]
	if got.PolicyAssessmentResult != domain.PolicyWithinThreshold || got.PolicyVersionID != "policy-version-1" || got.SubmittedVersion != 1 {
		t.Fatalf("expected the policy result and submission version recorded, got %+v", got)
	}
	subs := e.store.subs[c.ClaimID]
	if len(subs) != 1 || subs[0].VersionNo != 1 || len(subs[0].SnapshotHash) != 64 {
		t.Fatalf("expected one hash-addressed snapshot, got %+v", subs)
	}
}

func TestSubmit_Preconditions(t *testing.T) {
	e := newEnv()
	r := e.router()
	empty := e.claim(t, r)
	if rec := e.submit(t, r, empty.ClaimID); rec.Code != http.StatusBadRequest {
		t.Fatalf("a claim with no lines cannot be submitted, got %d", rec.Code)
	}
	req := createReq()
	req.BusinessPurpose = "  "
	rec := e.do(r, as(claimant, http.MethodPost, base, req))
	nop := decodeClaim(t, rec)
	e.addLine(t, r, nop.ClaimID, lineReq(10))
	if rec := e.submit(t, r, nop.ClaimID); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a claim with no business purpose cannot be submitted, got %d", rec.Code)
	}
	c := e.claim(t, r)
	e.addLine(t, r, c.ClaimID, lineReq(10))
	stale := as(claimant, http.MethodPost, base+c.ClaimID+"/submit", domain.VersionedRequest{ExpectedVersion: ver(9)})
	if rec := e.do(r, stale); rec.Code != http.StatusConflict || codeOf(t, rec) != "STALE_VERSION" {
		t.Fatalf("expected 409 STALE_VERSION, got %d", rec.Code)
	}
}

// Negative path 4: a tax reclaim is never inferred without a TAX result.
func TestSubmit_TaxRecoveryLine_UsesRealDetermination_AndFailureBlocks(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.claim(t, r)
	l := lineReq(100)
	l.ClaimTaxRecovery, l.Jurisdiction, l.TaxCategory = true, "US-CA", "STANDARD"
	line := e.addLine(t, r, c.ClaimID, l)

	e.tax.fail = true
	rec := e.submit(t, r, c.ClaimID)
	if rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "TAX_UNAVAILABLE" {
		t.Fatalf("expected 422 TAX_UNAVAILABLE, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.claims[c.ClaimID].Status != domain.StatusDraft || len(e.store.subs[c.ClaimID]) != 0 {
		t.Fatal("a failed determination must leave the claim a DRAFT with no snapshot")
	}

	e.tax.fail = false
	if rec := e.submit(t, r, c.ClaimID); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	got := e.store.lines[c.ClaimID][0]
	if e.tax.calls != 2 || got.TaxDeterminationID != "det-"+line.LineID || got.CalculatedTaxAmount != 10 {
		t.Fatalf("the reclaim figure must come from the determination, got %+v (calls %d)", got, e.tax.calls)
	}
}

func TestSubmit_ControlledCategory_NoPolicy_FailsClosed_AndResumes(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.claim(t, r)
	e.addLine(t, r, c.ClaimID, lineReq(10))

	e.policy.err = domain.ErrNoApplicablePolicy
	rec := e.submit(t, r, c.ClaimID)
	if rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "POLICY_UNAVAILABLE" {
		t.Fatalf("expected 422 POLICY_UNAVAILABLE, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.claims[c.ClaimID].Status != domain.StatusSubmitted {
		t.Fatalf("the claim must stay SUBMITTED so routing can resume, got %s", e.store.claims[c.ClaimID].Status)
	}
	e.policy.err = domain.ErrPolicyServiceUnavailable
	if rec := e.submit(t, r, c.ClaimID); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("policy-svc down on a controlled category must fail closed (503), got %d", rec.Code)
	}
	e.policy.err = nil
	if rec := e.submit(t, r, c.ClaimID); rec.Code != http.StatusOK || e.store.claims[c.ClaimID].Status != domain.StatusPendingApproval {
		t.Fatalf("resuming must route the claim, got %d / %s", rec.Code, e.store.claims[c.ClaimID].Status)
	}
	if len(e.store.subs[c.ClaimID]) != 1 {
		t.Fatalf("resuming must not write a second snapshot, got %d", len(e.store.subs[c.ClaimID]))
	}
}

func TestSubmit_NonControlledCategories_MayProceedUnassessed(t *testing.T) {
	e := newEnv()
	e.cfg.PolicyControlledCategories = []string{"TRAVEL"}
	e.policy.err = domain.ErrNoApplicablePolicy
	r := e.router()
	c := e.claim(t, r)
	e.addLine(t, r, c.ClaimID, lineReq(10)) // MEALS, not controlled
	if rec := e.submit(t, r, c.ClaimID); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	if got := e.store.claims[c.ClaimID]; got.Status != domain.StatusPendingApproval || got.PolicyAssessmentResult != domain.PolicyNotAssessed {
		t.Fatalf("expected PENDING_APPROVAL explicitly NOT_ASSESSED, got %+v", got)
	}
	e.policy.err = nil
}

// ── ApproveExpenseClaim ──────────────────────────────────────────────────────

func TestApprove_RequiresIdempotencyKey_AndExpectedVersion(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.pending(t, r, 10)
	if rec := e.approve(r, c.ClaimID, approver, ""); rec.Code != http.StatusBadRequest || codeOf(t, rec) != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("expected 400 IDEMPOTENCY_KEY_REQUIRED, got %d %s", rec.Code, rec.Body.String())
	}
	noVer := as(approver, http.MethodPost, base+c.ClaimID+"/approve", nil)
	noVer.key = "k"
	if rec := e.do(r, noVer); rec.Code != http.StatusBadRequest || codeOf(t, rec) != "VALIDATION_FAILED" {
		t.Fatalf("expected 400 VALIDATION_FAILED without expected_version, got %d", rec.Code)
	}
	stale := as(approver, http.MethodPost, base+c.ClaimID+"/approve", domain.VersionedRequest{ExpectedVersion: ver(99)})
	stale.key = "k2"
	if rec := e.do(r, stale); rec.Code != http.StatusConflict || codeOf(t, rec) != "STALE_VERSION" {
		t.Fatalf("expected 409 STALE_VERSION, got %d", rec.Code)
	}
	if e.store.claims[c.ClaimID].Status != domain.StatusPendingApproval {
		t.Fatal("refused approvals must change nothing")
	}
}

// Negative path 1: the claimant approves their own expense — refused locally
// (no authorization-svc needed) and again by its own-object layer.
func TestApprove_ClaimantCannotApproveOwnClaim(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.pending(t, r, 10)
	rec := e.approve(r, c.ClaimID, claimant, "k1")
	if rec.Code != http.StatusForbidden || codeOf(t, rec) != "SOD_CONFLICT" {
		t.Fatalf("expected 403 SOD_CONFLICT, got %d %s", rec.Code, rec.Body.String())
	}
	e.authz.sodRules = true
	e.store.claims[c.ClaimID].ClaimantPrincipalID = "someone-else" // local rule out of the picture; authz layer must still refuse
	if rec := e.approve(r, c.ClaimID, "someone-else", "k2"); rec.Code != http.StatusForbidden || codeOf(t, rec) != "SOD_CONFLICT" {
		t.Fatalf("authorization-svc's own-object layer must refuse too, got %d %s", rec.Code, rec.Body.String())
	}
	if e.store.claims[c.ClaimID].Status != domain.StatusPendingApproval || len(e.store.payReqs) != 0 || len(e.store.postings) != 0 {
		t.Fatal("a refused self-approval must have no financial consequence")
	}
}

func TestApprove_Independent_WritesPayableAndPostingRequestsAtomically(t *testing.T) {
	e := newEnv()
	e.cfg.Posting = domain.PostingConfig{ExpenseKey: "EXP", PayableKey: "PAY", TaxRecoverableKey: "TAXREC", FiscalPeriodLayout: "2006-01"}
	r := e.router()
	c := e.claim(t, r)
	e.addLine(t, r, c.ClaimID, lineReq(20))
	e.cf.threshold, e.cf.found = 1000, true // receipts not required in this test
	e.submit(t, r, c.ClaimID)

	rec := e.approve(r, c.ClaimID, approver, "approve-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := e.store.claims[c.ClaimID]
	if got.Status != domain.StatusApproved || got.ApprovedByPrincipalID == nil || *got.ApprovedByPrincipalID != approver || got.PayableState != domain.PayablePending {
		t.Fatalf("expected APPROVED by the approver with a PENDING payable, got %+v", got)
	}
	pr := e.store.payReqs[c.ClaimID]
	if pr == nil || pr.Amount != 20 || pr.Currency != "USD" || pr.State != domain.PayablePending {
		t.Fatalf("expected a durable AP-08 payable request for 20 USD, got %+v", pr)
	}
	if len(e.store.postings) != 1 || e.store.postings[0].SourceEventID != domain.ApprovalSourceEventID(c.ClaimID) || e.store.postings[0].Status != domain.PostingPending {
		t.Fatalf("expected one PENDING ACC-04 posting request keyed by the claim, got %+v", e.store.postings)
	}
	var p domain.GLPostingRequest
	_ = json.Unmarshal(e.store.postings[0].Payload, &p)
	var dr, cr float64
	for _, ln := range p.Lines {
		dr += ln.DebitAmount
		cr += ln.CreditAmount
	}
	if dr != 20 || cr != 20 || p.Lines[0].MappingKey != "EXP" || p.Lines[len(p.Lines)-1].MappingKey != "PAY" {
		t.Fatalf("expected a balanced posting using the configured mapping keys, got %+v", p)
	}
	seen := map[string]bool{}
	for _, ev := range e.store.outbox {
		seen[ev] = true
	}
	for _, want := range []string{"ExpenseClaimApproved", "ExpenseClaimPayableRequested", "accounting.event.requested"} {
		if !seen[want] {
			t.Fatalf("expected %s in the outbox, got %v", want, e.store.outbox)
		}
	}
}

func TestApprove_Replay_NeverDuplicatesTheFinancialConsequence(t *testing.T) {
	e := newEnv()
	e.cf.threshold, e.cf.found = 1000, true
	r := e.router()
	c := e.pending(t, r, 20)
	// The client sends the same request, key and version both times; a lost
	// response makes it retry exactly this.
	v := e.store.claims[c.ClaimID].Version
	send := func() *httptest.ResponseRecorder {
		x := as(approver, http.MethodPost, base+c.ClaimID+"/approve", domain.VersionedRequest{ExpectedVersion: ver(v)})
		x.key = "same-key"
		return e.do(r, x)
	}
	first := send()
	outboxAfter := len(e.store.outbox)
	second := send()
	if first.Code != http.StatusOK || second.Code != http.StatusOK || second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("expected 200 then a stored replay, got %d / %d replay=%q", first.Code, second.Code, second.Header().Get("Idempotent-Replay"))
	}
	if len(e.store.payReqs) != 1 || len(e.store.postings) != 1 || len(e.store.outbox) != outboxAfter {
		t.Fatalf("a replay must not duplicate payable/posting requests or events: %d / %d / %d vs %d",
			len(e.store.payReqs), len(e.store.postings), len(e.store.outbox), outboxAfter)
	}
}

func TestApprove_NotPending_Refused(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.claim(t, r)
	rec := e.do(r, func() call {
		x := as(approver, http.MethodPost, base+c.ClaimID+"/approve", domain.VersionedRequest{ExpectedVersion: ver(1)})
		x.key = "k"
		return x
	}())
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "INVALID_TRANSITION" {
		t.Fatalf("a DRAFT claim cannot be approved: expected 409 INVALID_TRANSITION, got %d", rec.Code)
	}
}

// Negative path 3: an expense over the receipt threshold approved without evidence.
func TestApprove_OverThresholdWithoutReceipt_Blocked_UntilReceiptOrException(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.pending(t, r, 100) // over the 25.0 default, no receipt
	rec := e.approve(r, c.ClaimID, approver, "k1")
	if rec.Code != http.StatusConflict || codeOf(t, rec) != "RECEIPT_REQUIRED" {
		t.Fatalf("expected 409 RECEIPT_REQUIRED, got %d %s", rec.Code, rec.Body.String())
	}
	if len(e.store.payReqs) != 0 || len(e.store.postings) != 0 {
		t.Fatal("a blocked approval must have no financial consequence")
	}

	// The delegated exception (a stronger authority) waives the requirement.
	e.authz.actions = nil
	ex := e.do(r, as(otherApprove, http.MethodPost, base+c.ClaimID+"/policy-exception",
		domain.RecordPolicyExceptionRequest{Reason: "receipt lost, manager approved", ExpectedVersion: ver(e.store.claims[c.ClaimID].Version)}))
	if ex.Code != http.StatusOK || e.authz.last() != handler.ExpenseExceptionApprove {
		t.Fatalf("expected 200 under %s, got %d (last action %s) %s", handler.ExpenseExceptionApprove, ex.Code, e.authz.last(), ex.Body.String())
	}
	if rec := e.approve(r, c.ClaimID, approver, "k2"); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after the exception, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestApprove_ReceiptThreshold_ComesFromTheConfigRegistry_FallsBackWhenUnavailable(t *testing.T) {
	cases := []struct {
		name  string
		cf    stubConfigFlags
		amt   float64
		want  int
		calls bool
	}{
		{"override lowers the threshold", stubConfigFlags{threshold: 10, found: true}, 15, http.StatusConflict, true},
		{"override raises the threshold", stubConfigFlags{threshold: 200, found: true}, 100, http.StatusOK, true},
		{"registry down falls back to the static 25", stubConfigFlags{err: configflag.ErrServiceUnavailable}, 100, http.StatusConflict, true},
		{"registry has no entry", stubConfigFlags{}, 15, http.StatusOK, true},
	}
	for _, tc := range cases {
		e := newEnv()
		cf := tc.cf
		e.cf = &cf
		r := e.router()
		c := e.pending(t, r, tc.amt)
		if rec := e.approve(r, c.ClaimID, approver, "k"); rec.Code != tc.want {
			t.Fatalf("%s: expected %d, got %d %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
		if tc.calls && e.cf.calls == 0 {
			t.Fatalf("%s: the registry must actually be consulted", tc.name)
		}
	}
}

func TestApprove_BusinessPurposeAndPolicyAssessment_AreRequired(t *testing.T) {
	e := newEnv()
	e.cf.threshold, e.cf.found = 1000, true
	r := e.router()
	c := e.pending(t, r, 10)
	e.store.claims[c.ClaimID].BusinessPurpose = ""
	if rec := e.approve(r, c.ClaimID, approver, "k1"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a missing business purpose blocks approval, got %d", rec.Code)
	}
	e.store.claims[c.ClaimID].BusinessPurpose = "dinner"
	e.store.claims[c.ClaimID].PolicyAssessmentResult = domain.PolicyNotAssessed
	rec := e.approve(r, c.ClaimID, approver, "k2")
	if rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "POLICY_UNAVAILABLE" {
		t.Fatalf("no policy assessment on a controlled category blocks approval, got %d %s", rec.Code, rec.Body.String())
	}
}

// Negative path 4, defence in depth at the approval boundary.
func TestApprove_TaxReclaimWithoutDetermination_Blocked(t *testing.T) {
	e := newEnv()
	e.cf.threshold, e.cf.found = 1000, true
	r := e.router()
	c := e.pending(t, r, 10)
	e.store.lines[c.ClaimID][0].ClaimTaxRecovery = true // reclaim flagged, determination id absent
	rec := e.approve(r, c.ClaimID, approver, "k")
	if rec.Code != http.StatusUnprocessableEntity || codeOf(t, rec) != "TAX_UNAVAILABLE" {
		t.Fatalf("expected 422 TAX_UNAVAILABLE, got %d %s", rec.Code, rec.Body.String())
	}
	if len(e.store.postings) != 0 {
		t.Fatal("no recoverable tax may be posted without a TAX result")
	}
}

func TestApprove_ApprovalRequired_NeedsTheStrongerExceptionAuthority(t *testing.T) {
	e := newEnv()
	e.policy.result = string(domain.PolicyApprovalRequired)
	e.cf.threshold, e.cf.found = 1000, true
	r := e.router()
	c := e.pending(t, r, 10)
	e.authz.denyActions = map[string]bool{handler.ExpenseExceptionApprove: true}
	if rec := e.approve(r, c.ClaimID, approver, "k1"); rec.Code != http.StatusForbidden {
		t.Fatalf("an APPROVAL_REQUIRED claim needs %s, got %d", handler.ExpenseExceptionApprove, rec.Code)
	}
	e.authz.denyActions = nil
	if rec := e.approve(r, c.ClaimID, approver, "k2"); rec.Code != http.StatusOK || e.authz.last() != handler.ExpenseExceptionApprove {
		t.Fatalf("expected 200 under the exception action, got %d (last %s)", rec.Code, e.authz.last())
	}
}

func TestApprove_ImmediateHandOff_IsBestEffort_AndPayableDueDateComesFromTerms(t *testing.T) {
	e := newEnv()
	e.useRelay = true
	e.cf.threshold, e.cf.found = 1000, true
	e.cf.terms, e.cf.termsSet = 30, true
	r := e.router()
	c := e.pending(t, r, 10)
	rec := e.approve(r, c.ClaimID, approver, "k")
	if rec.Code != http.StatusOK || len(e.relay.calls) != 1 || e.relay.calls[0] != c.ClaimID {
		t.Fatalf("expected the relay asked once to hand off immediately, got %d %v", rec.Code, e.relay.calls)
	}
	if got := decodeClaim(t, rec); got.Status != domain.StatusReimbursable {
		t.Fatalf("the response must reflect the hand-off outcome, got %s", got.Status)
	}
	due := e.store.payReqs[c.ClaimID].DueDate
	if d := time.Until(due); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("the payable's due date must come from the 30-day terms, never 'now': %v", due)
	}

	// Without the relay the approval still stands, with the payable PENDING.
	e2 := newEnv()
	e2.cf.threshold, e2.cf.found = 1000, true
	r2 := e2.router()
	c2 := e2.pending(t, r2, 10)
	if rec := e2.approve(r2, c2.ClaimID, approver, "k"); rec.Code != http.StatusOK || e2.store.claims[c2.ClaimID].Status != domain.StatusApproved {
		t.Fatalf("approval must stand without the immediate hand-off, got %d %s", rec.Code, e2.store.claims[c2.ClaimID].Status)
	}
}

// ── Reject / Return / Cancel / exception ─────────────────────────────────────

func TestReject_NeedsReasonVersionAndAnIndependentDecider(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.pending(t, r, 10)
	v := e.store.claims[c.ClaimID].Version
	if rec := e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/reject", domain.RejectClaimRequest{ExpectedVersion: ver(v)})); rec.Code != http.StatusBadRequest {
		t.Fatalf("a rejection needs a reason, got %d", rec.Code)
	}
	if rec := e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/reject", domain.RejectClaimRequest{Reason: "no"})); rec.Code != http.StatusBadRequest {
		t.Fatalf("a rejection needs expected_version, got %d", rec.Code)
	}
	if rec := e.do(r, as(claimant, http.MethodPost, base+c.ClaimID+"/reject", domain.RejectClaimRequest{Reason: "self", ExpectedVersion: ver(v)})); rec.Code != http.StatusForbidden || codeOf(t, rec) != "SOD_CONFLICT" {
		t.Fatalf("the claimant cannot reject their own claim, got %d", rec.Code)
	}
	rec := e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/reject", domain.RejectClaimRequest{Reason: "not a business expense", ExpectedVersion: ver(v)}))
	if rec.Code != http.StatusOK || e.store.claims[c.ClaimID].Status != domain.StatusRejected || e.store.claims[c.ClaimID].RejectionReason != "not a business expense" {
		t.Fatalf("expected REJECTED with the reason, got %d", rec.Code)
	}
	if len(e.store.payReqs) != 0 || len(e.store.postings) != 0 {
		t.Fatal("a rejection has no financial consequence")
	}
	if rec := e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/reject", domain.RejectClaimRequest{Reason: "again", ExpectedVersion: ver(v + 1)})); rec.Code != http.StatusConflict {
		t.Fatalf("a rejected claim is terminal, got %d", rec.Code)
	}
}

// Corrections preserve the prior submitted version and its evidence.
func TestReturnForCorrection_PreservesPriorSubmission_AndResubmitsAsVersion2(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.pending(t, r, 10)
	v1 := e.store.subs[c.ClaimID][0]

	rec := e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/return", domain.ReturnClaimRequest{Reason: "wrong cost center", ExpectedVersion: ver(e.store.claims[c.ClaimID].Version)}))
	if rec.Code != http.StatusOK || e.store.claims[c.ClaimID].Status != domain.StatusReturned {
		t.Fatalf("expected RETURNED, got %d", rec.Code)
	}

	// The returned claim is correctable: void the wrong line, add a corrected one.
	old := e.store.lines[c.ClaimID][0]
	if rec := e.do(r, as(claimant, http.MethodPost, base+c.ClaimID+"/lines/"+old.LineID+"/void", domain.VoidLineRequest{Reason: "wrong"})); rec.Code != http.StatusOK {
		t.Fatalf("void on a RETURNED claim: %d", rec.Code)
	}
	e.addLine(t, r, c.ClaimID, lineReq(12))
	if rec := e.submit(t, r, c.ClaimID); rec.Code != http.StatusOK {
		t.Fatalf("resubmit: %d %s", rec.Code, rec.Body.String())
	}
	subs := e.store.subs[c.ClaimID]
	if len(subs) != 2 || subs[0].SnapshotHash != v1.SnapshotHash || string(subs[0].Snapshot) != string(v1.Snapshot) || subs[1].VersionNo != 2 || subs[1].SnapshotHash == v1.SnapshotHash {
		t.Fatalf("version 1 must be untouched and version 2 added, got %+v", subs)
	}
	list := e.do(r, as(approver, http.MethodGet, base+c.ClaimID+"/submissions", nil))
	var resp struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(list.Body.Bytes(), &resp)
	if list.Code != http.StatusOK || resp.Count != 2 {
		t.Fatalf("expected both submission versions to be readable, got %d %s", list.Code, list.Body.String())
	}
}

func TestCancel_ByClaimant_AndNotAfterApproval(t *testing.T) {
	e := newEnv()
	e.cf.threshold, e.cf.found = 1000, true
	r := e.router()
	c := e.claim(t, r)
	if rec := e.do(r, as(claimant, http.MethodPost, base+c.ClaimID+"/cancel", domain.CancelClaimRequest{Reason: "duplicate entry"})); rec.Code != http.StatusOK || e.store.claims[c.ClaimID].Status != domain.StatusCancelled {
		t.Fatalf("expected CANCELLED, got %d", rec.Code)
	}
	p := e.pending(t, r, 10)
	e.approve(r, p.ClaimID, approver, "k")
	if rec := e.do(r, as(claimant, http.MethodPost, base+p.ClaimID+"/cancel", domain.CancelClaimRequest{Reason: "changed mind"})); rec.Code != http.StatusConflict {
		t.Fatalf("an approved claim can no longer be cancelled, got %d", rec.Code)
	}
}

// ── CloseExpenseClaim ────────────────────────────────────────────────────────

func TestClose_OnlyWhenAP08ReportsTheLivePayableSettled(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.pending(t, r, 10)
	reimb := func() { e.store.claims[c.ClaimID].Status = domain.StatusReimbursable }
	closeReq := func() *httptest.ResponseRecorder {
		return e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/close", domain.CloseClaimRequest{Reason: "settled"}))
	}

	if rec := closeReq(); rec.Code != http.StatusConflict || codeOf(t, rec) != "INVALID_TRANSITION" {
		t.Fatalf("only a reimbursable claim closes, got %d", rec.Code)
	}
	reimb()
	if rec := closeReq(); rec.Code != http.StatusConflict || codeOf(t, rec) != "PAYABLE_NOT_SETTLED" {
		t.Fatalf("no payable: expected 409 PAYABLE_NOT_SETTLED, got %d %s", rec.Code, rec.Body.String())
	}
	e.store.claims[c.ClaimID].PayableID = "payable-1"
	e.payable.status = "OPEN"
	if rec := closeReq(); rec.Code != http.StatusConflict || codeOf(t, rec) != "PAYABLE_NOT_SETTLED" {
		t.Fatalf("an unsettled payable cannot close the claim, got %d", rec.Code)
	}
	e.payable.down = true
	if rec := closeReq(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("AP-08 unreachable must fail closed, got %d", rec.Code)
	}
	e.payable.down, e.payable.status = false, payableopenitem.StatusSettled
	if rec := closeReq(); rec.Code != http.StatusOK || e.store.claims[c.ClaimID].Status != domain.StatusClosed {
		t.Fatalf("a SETTLED payable closes the claim, got %d", rec.Code)
	}
	if rec := e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/close", domain.CloseClaimRequest{})); rec.Code != http.StatusBadRequest {
		t.Fatalf("a close needs a reason, got %d", rec.Code)
	}
}

// ── queries ──────────────────────────────────────────────────────────────────

func TestAvailableActions_FollowState(t *testing.T) {
	e := newEnv()
	r := e.router()
	actions := func(id string) map[string]bool {
		rec := e.do(r, as(approver, http.MethodGet, base+id+"/available-actions", nil))
		var resp struct {
			AvailableActions []string `json:"available_actions"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		m := map[string]bool{}
		for _, a := range resp.AvailableActions {
			m[a] = true
		}
		return m
	}
	c := e.claim(t, r)
	if a := actions(c.ClaimID); !a["AddExpenseLine"] || !a["SubmitExpenseClaim"] || !a["CancelExpenseClaim"] || a["ApproveExpenseClaim"] {
		t.Fatalf("DRAFT actions wrong: %v", a)
	}
	p := e.pending(t, r, 10)
	if a := actions(p.ClaimID); !a["ApproveExpenseClaim"] || !a["RejectExpenseClaim"] || !a["ReturnForCorrection"] || !a["RecordExpensePolicyException"] || a["AddExpenseLine"] {
		t.Fatalf("PENDING_APPROVAL actions wrong: %v", a)
	}
	e.store.claims[p.ClaimID].Status = domain.StatusReimbursable
	if a := actions(p.ClaimID); !a["CloseExpenseClaim"] || a["ApproveExpenseClaim"] {
		t.Fatalf("REIMBURSABLE actions wrong: %v", a)
	}
}

func TestHistory_PolicyAssessment_AndReadAuthorization(t *testing.T) {
	e := newEnv()
	r := e.router()
	c := e.pending(t, r, 10)
	rec := e.do(r, as(approver, http.MethodGet, base+c.ClaimID+"/history", nil))
	var hist struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &hist)
	if rec.Code != http.StatusOK || hist.Count < 3 {
		t.Fatalf("expected the created/submitted/routed trail, got %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(r, as(approver, http.MethodGet, base+c.ClaimID+"/policy-assessment", nil))
	var pa struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pa)
	if rec.Code != http.StatusOK || pa.Result != string(domain.PolicyWithinThreshold) {
		t.Fatalf("expected WITHIN_THRESHOLD, got %d %s", rec.Code, rec.Body.String())
	}
	e.authz.denyActions = map[string]bool{handler.ExpenseRead: true}
	if rec := e.do(r, as(approver, http.MethodGet, base+c.ClaimID+"/history", nil)); rec.Code != http.StatusForbidden {
		t.Fatalf("reads need %s, got %d", handler.ExpenseRead, rec.Code)
	}
	if rec := e.do(r, call{method: http.MethodGet, path: base + c.ClaimID + "/history", tenant: tenantA}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reads need a principal, got %d", rec.Code)
	}
	if rec := e.do(r, as(approver, http.MethodGet, "/ap07/receipts/x/duplicate-assessment", nil)); rec.Code != http.StatusBadRequest {
		t.Fatalf("the duplicate assessment needs legal_entity_id, got %d", rec.Code)
	}
}

// The accounting consequence is visible and recoverable, never silent.
func TestAccountingStatus_AndRequeue(t *testing.T) {
	e := newEnv()
	e.cf.threshold, e.cf.found = 1000, true
	r := e.router()
	c := e.pending(t, r, 20)
	status := func() (string, []any) {
		rec := e.do(r, as(approver, http.MethodGet, base+c.ClaimID+"/accounting-status", nil))
		var resp struct {
			Status   string `json:"status"`
			Postings []any  `json:"postings"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.Status, resp.Postings
	}
	if st, _ := status(); st != "NOT_APPLICABLE" {
		t.Fatalf("an unapproved claim has no accounting consequence, got %s", st)
	}
	e.approve(r, c.ClaimID, approver, "k")
	if st, p := status(); st != "PENDING" || len(p) != 1 {
		t.Fatalf("expected one PENDING posting, got %s %v", st, p)
	}

	e.store.postings[0].Status = domain.PostingQuarantined
	e.authz.denyActions = map[string]bool{handler.ExpenseExceptionApprove: true}
	if rec := e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/accounting/requeue", nil)); rec.Code != http.StatusForbidden {
		t.Fatalf("a requeue re-opens a financial consequence and needs the exception authority, got %d", rec.Code)
	}
	e.authz.denyActions = nil
	rec := e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/accounting/requeue", nil))
	var rq struct {
		Requeued int64 `json:"requeued"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rq)
	if rec.Code != http.StatusOK || rq.Requeued != 1 || e.store.postings[0].Status != domain.PostingPending {
		t.Fatalf("expected 1 requeued, got %d %+v", rec.Code, rq)
	}
	e.store.postings[0].Status = domain.PostingPosted
	rec = e.do(r, as(approver, http.MethodPost, base+c.ClaimID+"/accounting/requeue", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &rq)
	if rq.Requeued != 0 || e.store.postings[0].Status != domain.PostingPosted {
		t.Fatal("a POSTED request must never be requeued")
	}
}
