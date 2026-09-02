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

	authzpkg "zoiko.io/supplier-financial-profile-svc/internal/authz"
	"zoiko.io/supplier-financial-profile-svc/internal/domain"
	"zoiko.io/supplier-financial-profile-svc/internal/events"
	"zoiko.io/supplier-financial-profile-svc/internal/handler"
	"zoiko.io/supplier-financial-profile-svc/internal/middleware"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub authz — including the own-object SoD layer ─────────────────────────

// stubAuthz is a real, working stand-in for authorization-svc's own
// behavior, not a canned mock: CheckAllowedOwnObject denies whenever the
// deciding principal equals the resource owner, exactly like
// authorization-svc's real dynamic SoD rule would for a principal
// deciding their own proposal.
type stubAuthz struct {
	deny     bool // deny CheckAllowed outright
	sodRules bool // enable own-object SoD enforcement in CheckAllowedOwnObject
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

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap01-1"
const testLegalEntity = "le-ap01-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, logger)
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
	return doRequestAs(r, method, path, body, tenantID, "principal-01")
}

func createProfile(t *testing.T, r http.Handler) *domain.SupplierFinancialProfile {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles", domain.CreateProfileRequest{
		LegalEntityID: testLegalEntity, SupplierRef: "supplier-acme-1",
	}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("createProfile: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var p domain.SupplierFinancialProfile
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	return &p
}

func createActiveProfile(t *testing.T, r http.Handler) *domain.SupplierFinancialProfile {
	t.Helper()
	p := createProfile(t, r)
	w := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/activate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("createActiveProfile: expected 200 activating, got %d: %s", w.Code, w.Body.String())
	}
	var active domain.SupplierFinancialProfile
	_ = json.Unmarshal(w.Body.Bytes(), &active)
	return &active
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestCreateProfile_Draft(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createProfile(t, r)
	if p.Status != domain.StatusDraft {
		t.Fatalf("expected DRAFT, got %s", p.Status)
	}
}

func TestActivateProfile(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createActiveProfile(t, r)
	if p.Status != domain.StatusActive {
		t.Fatalf("expected ACTIVE, got %s", p.Status)
	}
}

func TestActivateProfile_AlreadyActive_Conflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createActiveProfile(t, r)
	w := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/activate", nil, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 re-activating an ACTIVE profile, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHoldAndReleaseLifecycle(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createActiveProfile(t, r)

	w := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/hold",
		domain.PlaceHoldRequest{Reason: "compliance review"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 placing hold, got %d: %s", w.Code, w.Body.String())
	}
	var held domain.SupplierFinancialProfile
	_ = json.Unmarshal(w.Body.Bytes(), &held)
	if held.Status != domain.StatusOnHold {
		t.Fatalf("expected ON_HOLD, got %s", held.Status)
	}

	wRelease := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/release-hold", nil, testTenant)
	if wRelease.Code != http.StatusOK {
		t.Fatalf("expected 200 releasing hold, got %d: %s", wRelease.Code, wRelease.Body.String())
	}
	var released domain.SupplierFinancialProfile
	_ = json.Unmarshal(wRelease.Body.Bytes(), &released)
	if released.Status != domain.StatusActive {
		t.Fatalf("expected ACTIVE after release, got %s", released.Status)
	}
}

func TestHoldOnDraftProfile_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createProfile(t, r) // still DRAFT, never activated

	w := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/hold",
		domain.PlaceHoldRequest{Reason: "test"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 holding a DRAFT profile, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChangePaymentTerms_Overlap_Rejected is the regression test for
// AP-01's negative-path scenario #2.
func TestChangePaymentTerms_Overlap_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createActiveProfile(t, r)

	from1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	w1 := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_30", EffectiveFrom: from1, EffectiveTo: &to1}, testTenant)
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected 201 for the first period, got %d: %s", w1.Code, w1.Body.String())
	}

	// Overlaps: starts before the first period ends.
	from2 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	w2 := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_60", EffectiveFrom: from2}, testTenant)
	if w2.Code != http.StatusConflict {
		t.Fatalf("FABRICATION: expected 409 for an overlapping payment terms period, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestChangePaymentTerms_NonOverlapping_Succeeds(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	p := createActiveProfile(t, r)

	from1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_30", EffectiveFrom: from1, EffectiveTo: &to1}, testTenant)

	// Starts exactly when the first ends — [) semantics mean this must succeed.
	w2 := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/payment-terms",
		domain.ChangePaymentTermsRequest{TermsCode: "NET_60", EffectiveFrom: to1}, testTenant)
	if w2.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a period starting exactly when the prior one ends, got %d: %s", w2.Code, w2.Body.String())
	}
}

// ── high-risk change / own-object SoD ────────────────────────────────────────

// TestHighRiskChange_SelfApproval_Denied is the regression test for
// AP-01's SoD line: the proposer of a payee_reference change cannot also
// approve it. This exercises authorization-svc's real dynamic own-object
// SoD layer via the stub's faithful CheckAllowedOwnObject behavior.
func TestHighRiskChange_SelfApproval_Denied(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{sodRules: true})
	p := createActiveProfile(t, r)

	wPropose := doRequestAs(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/high-risk-changes",
		domain.ProposeHighRiskChangeRequest{Field: domain.FieldPayeeReference, NewValue: "payee-ref-999"}, testTenant, "principal-proposer")
	if wPropose.Code != http.StatusCreated {
		t.Fatalf("expected 201 proposing, got %d: %s", wPropose.Code, wPropose.Body.String())
	}
	var cr domain.HighRiskChangeRequest
	_ = json.Unmarshal(wPropose.Body.Bytes(), &cr)

	// The SAME principal attempts to approve their own proposal.
	wDecide := doRequestAs(r, http.MethodPost, "/ap01/high-risk-changes/"+cr.ChangeRequestID+"/decide",
		domain.DecideHighRiskChangeRequest{Approve: true}, testTenant, "principal-proposer")
	if wDecide.Code != http.StatusForbidden {
		t.Fatalf("SoD VIOLATION: expected 403 for self-approval of a high-risk change, got %d: %s", wDecide.Code, wDecide.Body.String())
	}

	// Confirm the change was NOT applied.
	updated, _ := st.FindProfile(context.Background(), p.ProfileID)
	if updated.PayeeReference == "payee-ref-999" {
		t.Fatalf("SoD VIOLATION: payee_reference was changed despite the denied self-approval")
	}
}

// TestHighRiskChange_IndependentApproval_Succeeds proves the SoD check
// is scoped to the SAME principal, not high-risk changes in general.
func TestHighRiskChange_IndependentApproval_Succeeds(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{sodRules: true})
	p := createActiveProfile(t, r)

	wPropose := doRequestAs(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/high-risk-changes",
		domain.ProposeHighRiskChangeRequest{Field: domain.FieldPayeeReference, NewValue: "payee-ref-999"}, testTenant, "principal-proposer")
	var cr domain.HighRiskChangeRequest
	_ = json.Unmarshal(wPropose.Body.Bytes(), &cr)

	wDecide := doRequestAs(r, http.MethodPost, "/ap01/high-risk-changes/"+cr.ChangeRequestID+"/decide",
		domain.DecideHighRiskChangeRequest{Approve: true}, testTenant, "principal-approver")
	if wDecide.Code != http.StatusOK {
		t.Fatalf("expected 200 for an independent approver, got %d: %s", wDecide.Code, wDecide.Body.String())
	}

	updated, _ := st.FindProfile(context.Background(), p.ProfileID)
	if updated.PayeeReference != "payee-ref-999" {
		t.Fatalf("expected payee_reference applied after independent approval, got %q", updated.PayeeReference)
	}
}

func TestHighRiskChange_Rejected_DoesNotApply(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{sodRules: true})
	p := createActiveProfile(t, r)

	wPropose := doRequestAs(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/high-risk-changes",
		domain.ProposeHighRiskChangeRequest{Field: domain.FieldPaymentMethodPreference, NewValue: "WIRE"}, testTenant, "principal-proposer")
	var cr domain.HighRiskChangeRequest
	_ = json.Unmarshal(wPropose.Body.Bytes(), &cr)

	wDecide := doRequestAs(r, http.MethodPost, "/ap01/high-risk-changes/"+cr.ChangeRequestID+"/decide",
		domain.DecideHighRiskChangeRequest{Approve: false, Reason: "insufficient evidence"}, testTenant, "principal-approver")
	if wDecide.Code != http.StatusOK {
		t.Fatalf("expected 200 for a valid rejection, got %d: %s", wDecide.Code, wDecide.Body.String())
	}

	updated, _ := st.FindProfile(context.Background(), p.ProfileID)
	if updated.PaymentMethodPreference == "WIRE" {
		t.Fatalf("expected a REJECTED change to never apply")
	}
}

func TestHighRiskChange_DoubleDecision_Conflict(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true})
	p := createActiveProfile(t, r)

	wPropose := doRequestAs(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/high-risk-changes",
		domain.ProposeHighRiskChangeRequest{Field: domain.FieldPayeeReference, NewValue: "payee-ref-999"}, testTenant, "principal-proposer")
	var cr domain.HighRiskChangeRequest
	_ = json.Unmarshal(wPropose.Body.Bytes(), &cr)

	doRequestAs(r, http.MethodPost, "/ap01/high-risk-changes/"+cr.ChangeRequestID+"/decide",
		domain.DecideHighRiskChangeRequest{Approve: true}, testTenant, "principal-approver")

	wSecond := doRequestAs(r, http.MethodPost, "/ap01/high-risk-changes/"+cr.ChangeRequestID+"/decide",
		domain.DecideHighRiskChangeRequest{Approve: true}, testTenant, "principal-another-approver")
	if wSecond.Code != http.StatusConflict {
		t.Fatalf("expected 409 deciding an already-decided change request, got %d: %s", wSecond.Code, wSecond.Body.String())
	}
}

func TestCreateProfile_AuthorizationDenied(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{deny: true})
	w := doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles", domain.CreateProfileRequest{
		LegalEntityID: testLegalEntity, SupplierRef: "supplier-acme-1",
	}, testTenant)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestGetProfile_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{})
	w := doRequest(r, http.MethodGet, "/ap01/supplier-financial-profiles/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListChangeEvents_TracksLifecycle(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{})
	p := createActiveProfile(t, r)
	doRequest(r, http.MethodPost, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/hold",
		domain.PlaceHoldRequest{Reason: "test"}, testTenant)

	w := doRequest(r, http.MethodGet, "/ap01/supplier-financial-profiles/"+p.ProfileID+"/change-events", nil, testTenant)
	var got struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Count != 3 { // created, activated, hold placed
		t.Fatalf("expected 3 change events, got %d", got.Count)
	}
}
