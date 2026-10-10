package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/supplier-financial-profile-svc/internal/authz"
	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	"zoiko.io/supplier-financial-profile-svc/internal/handler"
	"zoiko.io/supplier-financial-profile-svc/internal/middleware"
	"zoiko.io/supplier-financial-profile-svc/internal/payee"
)

// ── stub authz — including the own-object SoD layer ─────────────────────────

// stubAuthz stands in for authorization-svc: CheckAllowed can deny everything
// or specific actions; CheckAllowedOwnObject additionally denies when the
// deciding principal equals the resource owner (dynamic own-object SoD).
type stubAuthz struct {
	deny        bool
	denyActions map[string]bool
	sodRules    bool
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, action string) error {
	if a.deny || a.denyActions[action] {
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

// stubPayee is a controllable ORG-10.
type stubPayee struct {
	dest *payee.Destination
	err  error
	last string // party ref asked for
}

func (p *stubPayee) GetActiveDestination(_ context.Context, _, _, _, partyRef string) (*payee.Destination, error) {
	p.last = partyRef
	if p.err != nil {
		return nil, p.err
	}
	return p.dest, nil
}

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap01-1"
const testLegalEntity = "le-ap01-1"
const base = "/ap01/supplier-financial-profiles"

var keySeq int64

func newKey() string { return fmt.Sprintf("key-%d", atomic.AddInt64(&keySeq, 1)) }

type env struct {
	r     chi.Router
	st    *stubStore
	az    *stubAuthz
	payee *stubPayee
}

func newEnv(az *stubAuthz) *env {
	st := newStubStore()
	pe := &stubPayee{}
	h := handler.New(st, az, pe, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return &env{r: r, st: st, az: az, payee: pe}
}

func newTestRouter(az *stubAuthz) chi.Router { return newEnv(az).r }

type reqOpts struct {
	tenant    string
	principal string
	key       string
}

func do(r http.Handler, method, path string, body interface{}, o reqOpts) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if o.principal != "" {
		req.Header.Set("X-Principal-Id", o.principal)
	}
	if o.tenant != "" {
		req.Header.Set("X-Tenant-Id", o.tenant)
	}
	if o.key != "" {
		req.Header.Set("Idempotency-Key", o.key)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func as(principal string) reqOpts { return reqOpts{tenant: testTenant, principal: principal} }
func asKey(principal string) reqOpts {
	return reqOpts{tenant: testTenant, principal: principal, key: newKey()}
}

var maker = as("principal-01")

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

func errCode(w *httptest.ResponseRecorder) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Code
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("expected %d, got %d: %s", status, w.Code, w.Body.String())
	}
	if code != "" && errCode(w) != code {
		t.Fatalf("expected error code %q, got %q (%s)", code, errCode(w), w.Body.String())
	}
}

func createProfile(t *testing.T, r http.Handler) *domain.SupplierFinancialProfile {
	t.Helper()
	w := do(r, http.MethodPost, base, domain.CreateProfileRequest{LegalEntityID: testLegalEntity, SupplierRef: "supplier-acme-1"}, maker)
	expect(t, w, http.StatusCreated, "")
	p := decode[domain.SupplierFinancialProfile](t, w)
	return &p
}

func createActiveProfile(t *testing.T, r http.Handler) *domain.SupplierFinancialProfile {
	t.Helper()
	p := createProfile(t, r)
	w := do(r, http.MethodPost, base+"/"+p.ProfileID+"/activate", nil, maker)
	expect(t, w, http.StatusOK, "")
	a := decode[domain.SupplierFinancialProfile](t, w)
	return &a
}

func getProfile(t *testing.T, r http.Handler, id string) domain.SupplierFinancialProfile {
	t.Helper()
	w := do(r, http.MethodGet, base+"/"+id, nil, maker)
	expect(t, w, http.StatusOK, "")
	return decode[domain.SupplierFinancialProfile](t, w)
}

func outboxHas(st *stubStore, eventType string) bool {
	for _, e := range st.outbox {
		if e == eventType {
			return true
		}
	}
	return false
}

// ── lifecycle ────────────────────────────────────────────────────────────────

func TestCreateProfile_DraftVersion1(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createProfile(t, e.r)
	if p.Status != domain.StatusDraft || p.Version != 1 {
		t.Fatalf("expected DRAFT v1, got %s v%d", p.Status, p.Version)
	}
	if !outboxHas(e.st, "SupplierFinancialProfileCreated") || !outboxHas(e.st, "supplier_financial_profile.created") {
		t.Fatalf("expected spec event and legacy alias in outbox, got %v", e.st.outbox)
	}
}

func TestCreateProfile_DuplicateLiveProfile_Conflict(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	createProfile(t, r)
	w := do(r, http.MethodPost, base, domain.CreateProfileRequest{LegalEntityID: testLegalEntity, SupplierRef: "supplier-acme-1"}, maker)
	expect(t, w, http.StatusConflict, "VALIDATION_FAILED")
}

func TestCreateProfile_AuthorizationDenied(t *testing.T) {
	r := newTestRouter(&stubAuthz{deny: true})
	w := do(r, http.MethodPost, base, domain.CreateProfileRequest{LegalEntityID: testLegalEntity, SupplierRef: "s"}, maker)
	expect(t, w, http.StatusForbidden, "FORBIDDEN")
}

func TestCreateProfile_TenantMismatchAndMissing(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	w := do(r, http.MethodPost, base, domain.CreateProfileRequest{TenantID: "other", LegalEntityID: testLegalEntity, SupplierRef: "s"}, maker)
	expect(t, w, http.StatusForbidden, "FORBIDDEN")
	w = do(r, http.MethodPost, base, domain.CreateProfileRequest{LegalEntityID: testLegalEntity, SupplierRef: "s"}, reqOpts{principal: "p"})
	expect(t, w, http.StatusBadRequest, "VALIDATION_FAILED")
}

func TestActivateProfile(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)
	if p.Status != domain.StatusActive || p.Version != 2 {
		t.Fatalf("expected ACTIVE v2, got %s v%d", p.Status, p.Version)
	}
	if !outboxHas(e.st, "SupplierFinancialProfileChanged") {
		t.Fatalf("activate must emit SupplierFinancialProfileChanged, got %v", e.st.outbox)
	}
}

func TestActivateProfile_AlreadyActive_Conflict(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	w := do(r, http.MethodPost, base+"/"+p.ProfileID+"/activate", nil, maker)
	expect(t, w, http.StatusConflict, "VALIDATION_FAILED")
}

func TestHoldAndReleaseLifecycle(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)

	w := do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/hold", domain.PlaceHoldRequest{Reason: "compliance review"}, maker)
	expect(t, w, http.StatusOK, "")
	held := decode[domain.SupplierFinancialProfile](t, w)
	if held.Status != domain.StatusOnHold || held.HoldReason != "compliance review" {
		t.Fatalf("expected ON_HOLD with reason, got %s %q", held.Status, held.HoldReason)
	}
	if !outboxHas(e.st, "SupplierHoldPlaced") || !outboxHas(e.st, "supplier_hold.placed") {
		t.Fatalf("hold must emit SupplierHoldPlaced + legacy alias, got %v", e.st.outbox)
	}

	w = do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/release-hold", nil, maker)
	expect(t, w, http.StatusOK, "")
	if r := decode[domain.SupplierFinancialProfile](t, w); r.Status != domain.StatusActive || r.HoldReason != "" {
		t.Fatalf("expected ACTIVE after release, got %s", r.Status)
	}
	if !outboxHas(e.st, "SupplierHoldReleased") || !outboxHas(e.st, "supplier_hold.released") {
		t.Fatalf("release must emit SupplierHoldReleased + legacy alias, got %v", e.st.outbox)
	}
}

func TestHold_RequiresReasonAndActiveState(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	draft := createProfile(t, r)
	w := do(r, http.MethodPost, base+"/"+draft.ProfileID+"/hold", domain.PlaceHoldRequest{Reason: "x"}, maker)
	expect(t, w, http.StatusConflict, "VALIDATION_FAILED")

	r2 := newTestRouter(&stubAuthz{})
	a := createActiveProfile(t, r2)
	w = do(r2, http.MethodPost, base+"/"+a.ProfileID+"/hold", domain.PlaceHoldRequest{}, maker)
	expect(t, w, http.StatusBadRequest, "VALIDATION_FAILED")
}

func TestSuspendAndUnsuspend(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)

	w := do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/suspend", nil, maker)
	expect(t, w, http.StatusBadRequest, "VALIDATION_FAILED") // reason required

	w = do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/suspend", domain.TransitionRequest{Reason: "sanctions screening"}, maker)
	expect(t, w, http.StatusOK, "")
	if s := decode[domain.SupplierFinancialProfile](t, w); s.Status != domain.StatusSuspended {
		t.Fatalf("expected SUSPENDED, got %s", s.Status)
	}
	if !outboxHas(e.st, "SupplierFinancialProfileChanged") {
		t.Fatalf("suspend must emit SupplierFinancialProfileChanged")
	}
	// Suspended is not eligible.
	w = do(e.r, http.MethodGet, base+"/eligibility?legal_entity_id="+testLegalEntity+"&supplier_ref=supplier-acme-1", nil, maker)
	expect(t, w, http.StatusOK, "")
	if el := decode[domain.Eligibility](t, w); el.EligibleForNewCommitments || el.Status != domain.StatusSuspended {
		t.Fatalf("suspended supplier must be ineligible: %+v", el)
	}

	w = do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/unsuspend", nil, maker)
	expect(t, w, http.StatusOK, "")
	if s := decode[domain.SupplierFinancialProfile](t, w); s.Status != domain.StatusActive {
		t.Fatalf("expected ACTIVE after unsuspend, got %s", s.Status)
	}
	// Unsuspending an ACTIVE profile is invalid.
	w = do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/unsuspend", nil, maker)
	expect(t, w, http.StatusConflict, "VALIDATION_FAILED")
}

func TestSuspend_RequiresHoldManagePermission(t *testing.T) {
	r := newTestRouter(&stubAuthz{denyActions: map[string]bool{handler.SupplierHoldManage: true}})
	p := createActiveProfile(t, r)
	w := do(r, http.MethodPost, base+"/"+p.ProfileID+"/suspend", domain.TransitionRequest{Reason: "x"}, maker)
	expect(t, w, http.StatusForbidden, "FORBIDDEN")
	w = do(r, http.MethodPost, base+"/"+p.ProfileID+"/hold", domain.PlaceHoldRequest{Reason: "x"}, maker)
	expect(t, w, http.StatusForbidden, "FORBIDDEN")
}

func TestRetire_FromAnyLiveStateAndTerminal(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	w := do(r, http.MethodPost, base+"/"+p.ProfileID+"/retire", domain.RetireProfileRequest{Reason: "offboarded"}, maker)
	expect(t, w, http.StatusOK, "")
	w = do(r, http.MethodPost, base+"/"+p.ProfileID+"/retire", nil, maker)
	expect(t, w, http.StatusConflict, "VALIDATION_FAILED")
}

// ── payment terms (negative path #2) ─────────────────────────────────────────

func TestChangePaymentTerms_Overlap_Rejected(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)

	from1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	w1 := do(r, http.MethodPost, base+"/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_30", EffectiveFrom: from1, EffectiveTo: &to1}, maker)
	expect(t, w1, http.StatusCreated, "")

	from2 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	w2 := do(r, http.MethodPost, base+"/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_60", EffectiveFrom: from2}, maker)
	expect(t, w2, http.StatusConflict, "VALIDATION_FAILED")
}

func TestChangePaymentTerms_NonOverlapping_Succeeds_AndBumpsVersion(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)

	from1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_30", EffectiveFrom: from1, EffectiveTo: &to1}, maker)
	// Starts exactly when the first ends — [) semantics.
	w2 := do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_60", EffectiveFrom: to1}, maker)
	expect(t, w2, http.StatusCreated, "")
	if got := getProfile(t, e.r, p.ProfileID); got.Version != p.Version+2 {
		t.Fatalf("expected two terms changes to bump version by 2, got %d (was %d)", got.Version, p.Version)
	}
	if !outboxHas(e.st, "SupplierPaymentTermsChanged") || !outboxHas(e.st, "supplier_payment_terms.changed") {
		t.Fatalf("terms change must emit the spec event + legacy alias, got %v", e.st.outbox)
	}
}

func TestChangePaymentTerms_RequiresTermsManage(t *testing.T) {
	r := newTestRouter(&stubAuthz{denyActions: map[string]bool{handler.SupplierTermsManage: true}})
	p := createActiveProfile(t, r)
	w := do(r, http.MethodPost, base+"/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_30", EffectiveFrom: time.Now()}, maker)
	expect(t, w, http.StatusForbidden, "FORBIDDEN")
}

// ── high-risk change / maker-checker (SoD) ───────────────────────────────────

func propose(t *testing.T, r http.Handler, profileID, principal string, field domain.HighRiskField, value string) *domain.HighRiskChangeRequest {
	t.Helper()
	w := do(r, http.MethodPost, base+"/"+profileID+"/high-risk-changes",
		domain.ProposeHighRiskChangeRequest{Field: field, NewValue: value}, asKey(principal))
	expect(t, w, http.StatusCreated, "")
	cr := decode[domain.HighRiskChangeRequest](t, w)
	return &cr
}

func decide(r http.Handler, crID, principal string, version int, approve bool) *httptest.ResponseRecorder {
	return do(r, http.MethodPost, "/ap01/high-risk-changes/"+crID+"/decide",
		domain.DecideHighRiskChangeRequest{ExpectedVersion: &version, Approve: approve}, asKey(principal))
}

func TestHighRiskChange_SelfApproval_Denied_SoDConflict(t *testing.T) {
	// Even with a permissive authorization-svc the local maker != checker
	// guard refuses self-approval.
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)
	cr := propose(t, e.r, p.ProfileID, "principal-proposer", domain.FieldPayeeReference, "payee-ref-999")

	w := decide(e.r, cr.ChangeRequestID, "principal-proposer", p.Version, true)
	expect(t, w, http.StatusForbidden, "SOD_CONFLICT")
	if got := getProfile(t, e.r, p.ProfileID); got.PayeeReference == "payee-ref-999" || got.Version != p.Version {
		t.Fatalf("SoD VIOLATION: change applied despite denied self-approval: %+v", got)
	}
}

func TestHighRiskChange_ApproverDeniedByAuthorizationSvc(t *testing.T) {
	az := &stubAuthz{sodRules: true}
	e := newEnv(az)
	p := createActiveProfile(t, e.r)
	cr := propose(t, e.r, p.ProfileID, "principal-proposer", domain.FieldPaymentMethodPreference, "WIRE")
	az.deny = true // authorization-svc refuses the approver outright
	w := decide(e.r, cr.ChangeRequestID, "principal-approver", p.Version, true)
	expect(t, w, http.StatusForbidden, "FORBIDDEN")
}

func TestHighRiskChange_IndependentApproval_AppliesAndBumpsVersion(t *testing.T) {
	e := newEnv(&stubAuthz{sodRules: true})
	p := createActiveProfile(t, e.r)
	cr := propose(t, e.r, p.ProfileID, "principal-proposer", domain.FieldPayeeReference, "payee-ref-999")

	w := decide(e.r, cr.ChangeRequestID, "principal-approver", p.Version, true)
	expect(t, w, http.StatusOK, "")
	got := getProfile(t, e.r, p.ProfileID)
	if got.PayeeReference != "payee-ref-999" || got.Version != p.Version+1 {
		t.Fatalf("expected payee_reference applied and version bumped, got %+v", got)
	}
	if !outboxHas(e.st, "SupplierFinancialProfileChanged") || !outboxHas(e.st, domain.LegacyHighRiskDecided) {
		t.Fatalf("expected Changed + legacy decided events, got %v", e.st.outbox)
	}
}

func TestHighRiskChange_Rejected_DoesNotApply(t *testing.T) {
	e := newEnv(&stubAuthz{sodRules: true})
	p := createActiveProfile(t, e.r)
	cr := propose(t, e.r, p.ProfileID, "principal-proposer", domain.FieldPaymentMethodPreference, "WIRE")

	w := decide(e.r, cr.ChangeRequestID, "principal-approver", p.Version, false)
	expect(t, w, http.StatusOK, "")
	if got := getProfile(t, e.r, p.ProfileID); got.PaymentMethodPreference == "WIRE" || got.Version != p.Version {
		t.Fatalf("a REJECTED change must never apply: %+v", got)
	}
}

func TestHighRiskChange_DoubleDecision_Conflict(t *testing.T) {
	r := newTestRouter(&stubAuthz{sodRules: true})
	p := createActiveProfile(t, r)
	cr := propose(t, r, p.ProfileID, "principal-proposer", domain.FieldPayeeReference, "payee-ref-999")
	decide(r, cr.ChangeRequestID, "principal-approver", p.Version, true)
	w := decide(r, cr.ChangeRequestID, "principal-another-approver", p.Version+1, true)
	expect(t, w, http.StatusConflict, "VALIDATION_FAILED")
}

func TestHighRiskChange_Decide_RequiresExpectedVersionAndKey(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	cr := propose(t, r, p.ProfileID, "principal-proposer", domain.FieldPayeeReference, "payee-ref-999")

	w := do(r, http.MethodPost, "/ap01/high-risk-changes/"+cr.ChangeRequestID+"/decide",
		domain.DecideHighRiskChangeRequest{Approve: true}, asKey("principal-approver"))
	expect(t, w, http.StatusBadRequest, "VALIDATION_FAILED")

	v := p.Version
	w = do(r, http.MethodPost, "/ap01/high-risk-changes/"+cr.ChangeRequestID+"/decide",
		domain.DecideHighRiskChangeRequest{ExpectedVersion: &v, Approve: true}, as("principal-approver"))
	expect(t, w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")
}

func TestHighRiskChange_Decide_StaleVersion(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	cr := propose(t, r, p.ProfileID, "principal-proposer", domain.FieldPayeeReference, "payee-ref-999")
	w := decide(r, cr.ChangeRequestID, "principal-approver", p.Version+7, true)
	expect(t, w, http.StatusConflict, "STALE_VERSION")
}

func TestHighRiskChange_Propose_RequiresIdempotencyKey(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	w := do(r, http.MethodPost, base+"/"+p.ProfileID+"/high-risk-changes",
		domain.ProposeHighRiskChangeRequest{Field: domain.FieldPayeeReference, NewValue: "x"}, as("principal-proposer"))
	expect(t, w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")
}

func TestChangePaymentMethodPreference_MakerChecker(t *testing.T) {
	e := newEnv(&stubAuthz{sodRules: true})
	p := createActiveProfile(t, e.r)

	// Key required.
	w := do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/payment-method-preference",
		domain.ChangePaymentMethodPreferenceRequest{NewValue: "WIRE"}, as("principal-proposer"))
	expect(t, w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")

	w = do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/payment-method-preference",
		domain.ChangePaymentMethodPreferenceRequest{NewValue: "WIRE", Reason: "supplier request"}, asKey("principal-proposer"))
	expect(t, w, http.StatusCreated, "")
	cr := decode[domain.HighRiskChangeRequest](t, w)
	if cr.Field != domain.FieldPaymentMethodPreference || cr.Status != domain.ChangeRequestPending {
		t.Fatalf("unexpected change request: %+v", cr)
	}
	// Not applied until approved by someone else.
	if got := getProfile(t, e.r, p.ProfileID); got.PaymentMethodPreference != "" {
		t.Fatalf("preference applied before approval: %q", got.PaymentMethodPreference)
	}
	expect(t, decide(e.r, cr.ChangeRequestID, "principal-proposer", p.Version, true), http.StatusForbidden, "SOD_CONFLICT")
	expect(t, decide(e.r, cr.ChangeRequestID, "principal-approver", p.Version, true), http.StatusOK, "")
	if got := getProfile(t, e.r, p.ProfileID); got.PaymentMethodPreference != "WIRE" {
		t.Fatalf("expected WIRE after approval, got %q", got.PaymentMethodPreference)
	}
}

// ── amend ────────────────────────────────────────────────────────────────────

func TestAmend_LowRiskAppliedImmediately(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)
	cat, ch := "IT_SERVICES", "EDI"
	refs := []string{"cat-1", "cat-2"}
	w := do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/amend",
		domain.AmendProfileRequest{Category: &cat, InvoiceChannel: &ch, ProcurementCategoryRefs: &refs, Reason: "onboarding"}, maker)
	expect(t, w, http.StatusOK, "")
	got := decode[domain.SupplierFinancialProfile](t, w)
	if got.Category != cat || got.InvoiceChannel != ch || len(got.ProcurementCategoryRefs) != 2 || got.Version != p.Version+1 {
		t.Fatalf("unexpected amended profile: %+v", got)
	}
	if !outboxHas(e.st, "SupplierFinancialProfileChanged") {
		t.Fatalf("amend must emit SupplierFinancialProfileChanged")
	}
}

func TestAmend_HighRiskFieldsNeedIndependentApproval(t *testing.T) {
	e := newEnv(&stubAuthz{sodRules: true})
	p := createActiveProfile(t, e.r)
	policy := "STRICT_3WAY"
	flags := []string{"WATCHLIST"}
	taxRefs := []string{"WHT-15"}
	cat := "SERVICES"

	w := do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{
		Category: &cat, APAccountPolicy: &policy, RiskControlFlags: &flags, TaxClassificationRefs: &taxRefs,
	}, as("principal-maker"))
	expect(t, w, http.StatusAccepted, "")
	res := decode[domain.AmendResult](t, w)
	if len(res.PendingChanges) != 3 {
		t.Fatalf("expected 3 pending high-risk requests, got %d", len(res.PendingChanges))
	}
	cur := getProfile(t, e.r, p.ProfileID)
	if cur.Category != cat || cur.APAccountPolicy != "" || len(cur.RiskControlFlags) != 0 {
		t.Fatalf("only the low-risk field may apply immediately: %+v", cur)
	}

	// The maker cannot approve their own high-risk change.
	for _, pc := range res.PendingChanges {
		expect(t, decide(e.r, pc.ChangeRequestID, "principal-maker", cur.Version, true), http.StatusForbidden, "SOD_CONFLICT")
	}
	// A checker can; each applied change bumps the version, so re-read it.
	for _, pc := range res.PendingChanges {
		v := getProfile(t, e.r, p.ProfileID).Version
		expect(t, decide(e.r, pc.ChangeRequestID, "principal-checker", v, true), http.StatusOK, "")
	}
	got := getProfile(t, e.r, p.ProfileID)
	if got.APAccountPolicy != policy || len(got.RiskControlFlags) != 1 || len(got.TaxClassificationRefs) != 1 {
		t.Fatalf("approved high-risk fields not applied: %+v", got)
	}
}

func TestAmend_NoChangesAndStaleVersionAndRetired(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{Reason: "x"}, maker), http.StatusBadRequest, "VALIDATION_FAILED")

	cat := "X"
	stale := p.Version + 5
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{ExpectedVersion: &stale, Category: &cat}, maker), http.StatusConflict, "STALE_VERSION")

	do(r, http.MethodPost, base+"/"+p.ProfileID+"/retire", nil, maker)
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{Category: &cat}, maker), http.StatusConflict, "VALIDATION_FAILED")
}

// ── versioning / as-of / history ─────────────────────────────────────────────

func TestAsOfHistoryAndRevisions(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createProfile(t, e.r)
	t0 := e.st.clock // creation instant
	do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/activate", nil, maker)
	tActive := e.st.clock
	cat := "LOGISTICS"
	do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{Category: &cat}, maker)

	asOf := func(at time.Time) *httptest.ResponseRecorder {
		return do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/as-of?at="+at.Format(time.RFC3339), nil, maker)
	}
	// As of creation: DRAFT v1 with no category.
	w := asOf(t0)
	expect(t, w, http.StatusOK, "")
	got := decode[domain.ProfileAsOf](t, w)
	if got.Profile.Status != domain.StatusDraft || got.Profile.Version != 1 || got.Profile.Category != "" {
		t.Fatalf("as-of creation wrong: %+v", got.Profile)
	}
	// As of activation: ACTIVE v2, category still empty.
	got = decode[domain.ProfileAsOf](t, asOf(tActive))
	if got.Profile.Status != domain.StatusActive || got.Profile.Version != 2 || got.Profile.Category != "" {
		t.Fatalf("as-of activation wrong: %+v", got.Profile)
	}
	// Now: v3 with category.
	got = decode[domain.ProfileAsOf](t, asOf(e.st.clock.Add(time.Hour)))
	if got.Profile.Version != 3 || got.Profile.Category != cat {
		t.Fatalf("as-of now wrong: %+v", got.Profile)
	}
	// Before the profile existed.
	expect(t, asOf(t0.Add(-24*time.Hour)), http.StatusNotFound, "NOT_FOUND")
	// Bad timestamp.
	expect(t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/as-of?at=yesterday", nil, maker), http.StatusBadRequest, "VALIDATION_FAILED")

	// History: one revision per change, with prior snapshot and actor.
	w = do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/history", nil, maker)
	expect(t, w, http.StatusOK, "")
	hist := decode[struct {
		Data  []domain.ProfileRevision `json:"data"`
		Count int                      `json:"count"`
	}](t, w)
	if hist.Count != 3 || hist.Data[0].PriorSnapshot != nil || hist.Data[2].PriorSnapshot == nil ||
		hist.Data[2].PriorSnapshot.Version != 2 || hist.Data[2].ActorPrincipalID != "principal-01" || hist.Data[0].EffectiveTo == nil {
		t.Fatalf("unexpected history: %+v", hist)
	}
}

func TestAsOf_IncludesPaymentTermsInForce(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/payment-terms", domain.ChangePaymentTermsRequest{TermsCode: "NET_45", EffectiveFrom: from}, maker)
	w := do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/as-of?at="+e.st.clock.Format(time.RFC3339), nil, maker)
	expect(t, w, http.StatusOK, "")
	if got := decode[domain.ProfileAsOf](t, w); got.PaymentTerms == nil || got.PaymentTerms.TermsCode != "NET_45" {
		t.Fatalf("expected NET_45 in force, got %+v", got.PaymentTerms)
	}
}

func TestListChangeEvents_TracksLifecycle(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	do(r, http.MethodPost, base+"/"+p.ProfileID+"/hold", domain.PlaceHoldRequest{Reason: "test"}, maker)
	w := do(r, http.MethodGet, base+"/"+p.ProfileID+"/change-events", nil, maker)
	got := decode[struct {
		Count int `json:"count"`
	}](t, w)
	if got.Count != 3 { // created, activated, hold placed
		t.Fatalf("expected 3 change events, got %d", got.Count)
	}
}

// ── idempotency ──────────────────────────────────────────────────────────────

func TestIdempotency_ReplaySameKeySameBody(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)
	cat := "A"
	o := asKey("principal-01")
	body := domain.AmendProfileRequest{Category: &cat}

	w1 := do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/amend", body, o)
	expect(t, w1, http.StatusOK, "")
	if w1.Header().Get("Idempotent-Replay") != "" {
		t.Fatalf("first call must not be flagged as a replay")
	}
	w2 := do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/amend", body, o)
	expect(t, w2, http.StatusOK, "")
	if w2.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("expected Idempotent-Replay: true on replay")
	}
	if strings.TrimSpace(w1.Body.String()) != strings.TrimSpace(w2.Body.String()) {
		t.Fatalf("replay body differs:\n%s\n%s", w1.Body.String(), w2.Body.String())
	}
	if got := getProfile(t, e.r, p.ProfileID); got.Version != p.Version+1 {
		t.Fatalf("replay must not apply the change twice: version %d", got.Version)
	}
}

func TestIdempotency_SameKeyDifferentBody_Rejected(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	a, b := "A", "B"
	o := asKey("principal-01")
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{Category: &a}, o), http.StatusOK, "")
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{Category: &b}, o), http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED")
}

func TestIdempotency_CreateReplay(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	o := asKey("principal-01")
	req := domain.CreateProfileRequest{LegalEntityID: testLegalEntity, SupplierRef: "s-1"}
	w1 := do(r, http.MethodPost, base, req, o)
	expect(t, w1, http.StatusCreated, "")
	w2 := do(r, http.MethodPost, base, req, o)
	expect(t, w2, http.StatusCreated, "") // stored status, not a duplicate-profile 409
	if w2.Header().Get("Idempotent-Replay") != "true" || w1.Body.String() != w2.Body.String() {
		t.Fatalf("expected an identical replay, got %q vs %q", w1.Body.String(), w2.Body.String())
	}
}

// ── eligibility (negative path #4) ───────────────────────────────────────────

func eligibility(t *testing.T, r http.Handler, o reqOpts) (*httptest.ResponseRecorder, domain.Eligibility) {
	t.Helper()
	w := do(r, http.MethodGet, base+"/eligibility?legal_entity_id="+testLegalEntity+"&supplier_ref=supplier-acme-1", nil, o)
	var el domain.Eligibility
	_ = json.Unmarshal(w.Body.Bytes(), &el)
	return w, el
}

func TestEligibility_OnlyActiveAndNotHeld(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createProfile(t, r)

	_, el := eligibility(t, r, maker)
	if el.EligibleForNewCommitments || el.Status != domain.StatusDraft || el.Reason == "" || el.ProfileID != p.ProfileID {
		t.Fatalf("DRAFT supplier must be ineligible: %+v", el)
	}

	do(r, http.MethodPost, base+"/"+p.ProfileID+"/activate", nil, maker)
	_, el = eligibility(t, r, maker)
	if !el.EligibleForNewCommitments || el.IsOnHold || el.Reason != "" || el.Version != 2 || el.LegalEntityID != testLegalEntity || el.SupplierRef != "supplier-acme-1" {
		t.Fatalf("ACTIVE supplier must be eligible: %+v", el)
	}

	do(r, http.MethodPost, base+"/"+p.ProfileID+"/hold", domain.PlaceHoldRequest{Reason: "dispute"}, maker)
	_, el = eligibility(t, r, maker)
	if el.EligibleForNewCommitments || !el.IsOnHold || !strings.Contains(el.Reason, "dispute") {
		t.Fatalf("held supplier must be ineligible with the hold reason: %+v", el)
	}

	do(r, http.MethodPost, base+"/"+p.ProfileID+"/release-hold", nil, maker)
	do(r, http.MethodPost, base+"/"+p.ProfileID+"/retire", nil, maker)
	_, el = eligibility(t, r, maker)
	if el.EligibleForNewCommitments || el.Status != domain.StatusRetired {
		t.Fatalf("retired supplier must be ineligible: %+v", el)
	}
}

func TestEligibility_NotFoundTenantAndParams(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	createActiveProfile(t, r)

	w, _ := eligibility(t, r, reqOpts{tenant: "other-tenant", principal: "p"})
	expect(t, w, http.StatusNotFound, "NOT_FOUND")
	w = do(r, http.MethodGet, base+"/eligibility?legal_entity_id="+testLegalEntity+"&supplier_ref=unknown", nil, maker)
	expect(t, w, http.StatusNotFound, "NOT_FOUND")
	w = do(r, http.MethodGet, base+"/eligibility?legal_entity_id="+testLegalEntity, nil, maker)
	expect(t, w, http.StatusBadRequest, "VALIDATION_FAILED")
	w = do(r, http.MethodGet, base+"/eligibility?legal_entity_id="+testLegalEntity+"&supplier_ref=x", nil, reqOpts{principal: "p"})
	expect(t, w, http.StatusBadRequest, "VALIDATION_FAILED") // tenant required
}

func TestEligibility_ServiceReadWithoutPrincipal_AndAuthorizedWhenPrincipalGiven(t *testing.T) {
	az := &stubAuthz{}
	e := newEnv(az)
	createActiveProfile(t, e.r)

	// Internal service caller (no principal) is served tenant-scoped.
	w, el := eligibility(t, e.r, reqOpts{tenant: testTenant})
	expect(t, w, http.StatusOK, "")
	if !el.EligibleForNewCommitments {
		t.Fatalf("expected eligible: %+v", el)
	}
	// A principal that lacks read permission is refused.
	az.denyActions = map[string]bool{handler.SupplierFinancialRead: true}
	w, _ = eligibility(t, e.r, as("principal-x"))
	expect(t, w, http.StatusForbidden, "FORBIDDEN")
}

func TestEligibility_PrincipalRequiredWhenServiceReadsDisabled(t *testing.T) {
	st := newStubStore()
	h := handler.New(st, &stubAuthz{}, &stubPayee{}, zap.NewNop())
	h.AllowServiceReads = false
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	w, _ := eligibility(t, r, reqOpts{tenant: testTenant})
	expect(t, w, http.StatusUnauthorized, "UNAUTHENTICATED")
}

// ── reads: authorization + tenant ────────────────────────────────────────────

func TestReads_AreAuthorizedAndTenantVerified(t *testing.T) {
	az := &stubAuthz{}
	e := newEnv(az)
	p := createActiveProfile(t, e.r)

	paths := []string{
		base + "/" + p.ProfileID,
		base + "/" + p.ProfileID + "/as-of?at=" + time.Now().Add(24*time.Hour).Format(time.RFC3339),
		base + "/" + p.ProfileID + "/history",
		base + "/" + p.ProfileID + "/payment-terms",
		base + "/" + p.ProfileID + "/change-events",
		base + "/" + p.ProfileID + "/available-actions",
		base + "/" + p.ProfileID + "/payee-reference",
		base + "/" + p.ProfileID + "/last-payee-change",
	}
	for _, path := range paths {
		// No principal.
		expect(t, do(e.r, http.MethodGet, path, nil, reqOpts{tenant: testTenant}), http.StatusUnauthorized, "UNAUTHENTICATED")
		// No tenant.
		expect(t, do(e.r, http.MethodGet, path, nil, reqOpts{principal: "p"}), http.StatusBadRequest, "VALIDATION_FAILED")
		// Another tenant cannot see the profile at all.
		expect(t, do(e.r, http.MethodGet, path, nil, reqOpts{tenant: "tenant-other", principal: "p"}), http.StatusNotFound, "NOT_FOUND")
	}
	az.denyActions = map[string]bool{handler.SupplierFinancialRead: true}
	for _, path := range paths {
		expect(t, do(e.r, http.MethodGet, path, nil, maker), http.StatusForbidden, "FORBIDDEN")
	}
}

func TestCommands_RejectCrossTenantProfile(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	other := reqOpts{tenant: "tenant-other", principal: "p"}
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/hold", domain.PlaceHoldRequest{Reason: "x"}, other), http.StatusNotFound, "NOT_FOUND")
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/retire", nil, other), http.StatusNotFound, "NOT_FOUND")
	cat := "x"
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{Category: &cat}, other), http.StatusNotFound, "NOT_FOUND")
	if got := getProfile(t, r, p.ProfileID); got.Status != domain.StatusActive {
		t.Fatalf("cross-tenant command changed the profile: %+v", got)
	}
}

func TestListProfiles_ShapeAndTenantScoping(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	w := do(r, http.MethodGet, base+"/", nil, reqOpts{tenant: testTenant}) // service caller: no principal
	expect(t, w, http.StatusOK, "")
	var got struct {
		Data  []domain.SupplierFinancialProfile `json:"data"`
		Count int                               `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Count != 1 || got.Data[0].ProfileID != p.ProfileID || got.Data[0].UpdatedAt.IsZero() || got.Data[0].Status != domain.StatusActive {
		t.Fatalf("list shape changed: %s", w.Body.String())
	}
	w = do(r, http.MethodGet, base+"/", nil, reqOpts{tenant: "tenant-other", principal: "p"})
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Count != 0 {
		t.Fatalf("another tenant must see nothing, got %d", got.Count)
	}
}

func TestGetProfile_NotFound(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	expect(t, do(r, http.MethodGet, base+"/does-not-exist", nil, maker), http.StatusNotFound, "NOT_FOUND")
}

// ── GetPayeeReference (negative path #1) ─────────────────────────────────────

func TestGetPayeeReference_ReturnsOnlyORG10Value(t *testing.T) {
	e := newEnv(&stubAuthz{sodRules: true})
	p := createActiveProfile(t, e.r)
	// Store a (now stale) payee_reference on the profile via maker-checker.
	cr := propose(t, e.r, p.ProfileID, "principal-proposer", domain.FieldPayeeReference, "stale-stored-ref")
	expect(t, decide(e.r, cr.ChangeRequestID, "principal-approver", p.Version, true), http.StatusOK, "")

	updated := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)
	e.payee.dest = &payee.Destination{DestinationID: "org10-dest-42", LegalEntityID: testLegalEntity, PartyRef: "stale-stored-ref", Status: "ACTIVE", UpdatedAt: updated}

	w := do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/payee-reference", nil, maker)
	expect(t, w, http.StatusOK, "")
	got := decode[map[string]any](t, w)
	if got["payee_reference"] != "org10-dest-42" || got["destination_id"] != "org10-dest-42" || got["source"] != "ORG-10" {
		t.Fatalf("payee reference must be ORG-10's value: %v", got)
	}
	if got["destination_version"] != updated.Format(time.RFC3339Nano) {
		t.Fatalf("expected ORG-10 destination version, got %v", got["destination_version"])
	}
	if strings.Contains(w.Body.String(), "stale-stored-ref") && got["payee_reference"] != "org10-dest-42" {
		t.Fatalf("stored value leaked as payee reference: %s", w.Body.String())
	}
	if e.payee.last != "stale-stored-ref" {
		t.Fatalf("expected the profile's controlled payee_reference to be the ORG-10 lookup key, got %q", e.payee.last)
	}
}

func TestGetPayeeReference_FailsClosed_NeverFallsBack(t *testing.T) {
	e := newEnv(&stubAuthz{sodRules: true})
	p := createActiveProfile(t, e.r)
	cr := propose(t, e.r, p.ProfileID, "principal-proposer", domain.FieldPayeeReference, "stored-ref-1")
	decide(e.r, cr.ChangeRequestID, "principal-approver", p.Version, true)

	// ORG-10 down -> 503, no payee reference in the body.
	e.payee.err = payee.ErrUnavailable
	w := do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/payee-reference", nil, maker)
	expect(t, w, http.StatusServiceUnavailable, "PAYEE_SERVICE_UNAVAILABLE")
	if strings.Contains(w.Body.String(), "stored-ref-1") || strings.Contains(w.Body.String(), "payee_reference") {
		t.Fatalf("must not fall back to the stored value: %s", w.Body.String())
	}

	// ORG-10 has no active destination -> 404, again nothing stored is returned.
	e.payee.err = payee.ErrNoActiveDestination
	w = do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/payee-reference", nil, maker)
	expect(t, w, http.StatusNotFound, "NO_ACTIVE_PAYEE_DESTINATION")
	if strings.Contains(w.Body.String(), "stored-ref-1") {
		t.Fatalf("must not fall back to the stored value: %s", w.Body.String())
	}
}

func TestGetPayeeReference_UsesSupplierRefWhenNoControlledReference(t *testing.T) {
	e := newEnv(&stubAuthz{})
	p := createActiveProfile(t, e.r)
	e.payee.dest = &payee.Destination{DestinationID: "d1", LegalEntityID: testLegalEntity, UpdatedAt: time.Now()}
	expect(t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/payee-reference", nil, maker), http.StatusOK, "")
	if e.payee.last != "supplier-acme-1" {
		t.Fatalf("expected supplier_ref as the ORG-10 party, got %q", e.payee.last)
	}
}

// ── last-payee-change (negative path #3: bank changer cannot authorize payment) ─

func TestLastPayeeChange_ExposesChangerAndApprover(t *testing.T) {
	e := newEnv(&stubAuthz{sodRules: true})
	p := createActiveProfile(t, e.r)

	expect(t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/last-payee-change", nil, maker), http.StatusNotFound, "NOT_FOUND")

	cr := propose(t, e.r, p.ProfileID, "principal-bank-changer", domain.FieldPayeeReference, "payee-ref-7")
	// Pending proposals are not yet a change.
	expect(t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/last-payee-change", nil, maker), http.StatusNotFound, "NOT_FOUND")

	expect(t, decide(e.r, cr.ChangeRequestID, "principal-approver", p.Version, true), http.StatusOK, "")
	w := do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/last-payee-change", nil, maker)
	expect(t, w, http.StatusOK, "")
	got := decode[domain.LastPayeeChange](t, w)
	if got.PrincipalID != "principal-bank-changer" || got.ApproverPrincipalID != "principal-approver" || got.Version != p.Version+1 || got.ChangedAt.IsZero() {
		t.Fatalf("unexpected last payee change: %+v", got)
	}

	// A non-payee change (category) does not move it.
	cat := "X"
	do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/amend", domain.AmendProfileRequest{Category: &cat}, as("someone-else"))
	got2 := decode[domain.LastPayeeChange](t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/last-payee-change", nil, maker))
	if got2.PrincipalID != "principal-bank-changer" || got2.Version != got.Version {
		t.Fatalf("non-payee change must not affect last-payee-change: %+v", got2)
	}
}

// ── available actions ────────────────────────────────────────────────────────

func actionNames(t *testing.T, w *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	got := decode[struct {
		Actions []struct {
			Action string `json:"action"`
		} `json:"actions"`
	}](t, w)
	m := map[string]bool{}
	for _, a := range got.Actions {
		m[a.Action] = true
	}
	return m
}

func TestAvailableActions_ByStateAndPermission(t *testing.T) {
	az := &stubAuthz{}
	e := newEnv(az)
	p := createProfile(t, e.r)

	a := actionNames(t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/available-actions", nil, maker))
	if !a["activate"] || a["place_hold"] || a["release_hold"] {
		t.Fatalf("DRAFT actions wrong: %v", a)
	}
	do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/activate", nil, maker)
	a = actionNames(t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/available-actions", nil, maker))
	if a["activate"] || !a["place_hold"] || !a["suspend"] || !a["amend"] || !a["change_payment_terms"] {
		t.Fatalf("ACTIVE actions wrong: %v", a)
	}
	// Permission filter: without hold.manage the hold actions disappear.
	az.denyActions = map[string]bool{handler.SupplierHoldManage: true}
	a = actionNames(t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/available-actions", nil, maker))
	if a["place_hold"] || a["suspend"] || !a["amend"] {
		t.Fatalf("permission filter wrong: %v", a)
	}
	az.denyActions = nil
	do(e.r, http.MethodPost, base+"/"+p.ProfileID+"/retire", nil, maker)
	a = actionNames(t, do(e.r, http.MethodGet, base+"/"+p.ProfileID+"/available-actions", nil, maker))
	if len(a) != 0 {
		t.Fatalf("RETIRED must offer no actions: %v", a)
	}
}

// ── validation ───────────────────────────────────────────────────────────────

func TestProposeHighRiskChange_Validation(t *testing.T) {
	r := newTestRouter(&stubAuthz{})
	p := createActiveProfile(t, r)
	o := asKey("principal-proposer")
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/high-risk-changes", domain.ProposeHighRiskChangeRequest{Field: "BOGUS", NewValue: "x"}, o), http.StatusBadRequest, "VALIDATION_FAILED")
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/high-risk-changes", domain.ProposeHighRiskChangeRequest{Field: domain.FieldPayeeReference}, asKey("p")), http.StatusBadRequest, "VALIDATION_FAILED")
	expect(t, do(r, http.MethodPost, base+"/"+p.ProfileID+"/high-risk-changes", domain.ProposeHighRiskChangeRequest{Field: domain.FieldRiskControlFlags, NewValue: "not-json"}, asKey("p")), http.StatusBadRequest, "VALIDATION_FAILED")
}
