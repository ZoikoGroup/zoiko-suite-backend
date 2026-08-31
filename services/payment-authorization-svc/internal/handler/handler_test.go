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

	authzpkg "zoiko.io/payment-authorization-svc/internal/authz"
	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/events"
	"zoiko.io/payment-authorization-svc/internal/handler"
	"zoiko.io/payment-authorization-svc/internal/middleware"
	"zoiko.io/payment-authorization-svc/internal/paymentproposal"
	"zoiko.io/payment-authorization-svc/internal/policy"
	"zoiko.io/payment-authorization-svc/internal/supplierprofile"
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

// ── stub payment-proposal-svc client ────────────────────────────────────────

type stubProposal struct {
	proposals    map[string]paymentproposal.Proposal
	fingerprints map[string]string
}

func newStubProposal() *stubProposal {
	return &stubProposal{proposals: map[string]paymentproposal.Proposal{}, fingerprints: map[string]string{}}
}

func (p *stubProposal) add(id, tenantID, legalEntityID, status, preparer string, netAmount float64, items []paymentproposal.Item) {
	tid := tenantID
	p.proposals[id] = paymentproposal.Proposal{
		ProposalID: id, TenantID: &tid, LegalEntityID: legalEntityID, Status: status,
		NetAmount: netAmount, Currency: "USD", CreatedByPrincipalID: preparer, Items: items,
	}
	p.fingerprints[id] = "fp-" + id + "-v1"
}

func (p *stubProposal) GetProposal(_ context.Context, tenantID, proposalID string) (*paymentproposal.Proposal, error) {
	pr, ok := p.proposals[proposalID]
	if !ok || (pr.TenantID != nil && *pr.TenantID != tenantID) {
		return nil, domain.ErrProposalNotEligible
	}
	return &pr, nil
}

func (p *stubProposal) GetFingerprint(_ context.Context, _, proposalID string) (string, error) {
	fp, ok := p.fingerprints[proposalID]
	if !ok {
		return "", domain.ErrProposalNotEligible
	}
	return fp, nil
}

var _ paymentproposal.Client = (*stubProposal)(nil)

// ── stub supplier-financial-profile-svc client ──────────────────────────────

type stubSupplier struct {
	profiles map[string]supplierprofile.Profile
}

func newStubSupplier() *stubSupplier {
	return &stubSupplier{profiles: map[string]supplierprofile.Profile{}}
}

func (s *stubSupplier) set(legalEntityID, supplierRef string, updatedAt time.Time) {
	s.profiles[legalEntityID+"|"+supplierRef] = supplierprofile.Profile{
		ProfileID: "profile-" + supplierRef, LegalEntityID: legalEntityID, SupplierRef: supplierRef,
		Status: "ACTIVE", UpdatedAt: updatedAt,
	}
}

func (s *stubSupplier) FindActiveProfile(_ context.Context, _, legalEntityID, supplierRef string) (*supplierprofile.Profile, error) {
	p, ok := s.profiles[legalEntityID+"|"+supplierRef]
	if !ok {
		return nil, domain.ErrPayeeIdentityStale
	}
	return &p, nil
}

var _ supplierprofile.Client = (*stubSupplier)(nil)

// ── stub policy-svc client ──────────────────────────────────────────────────

type stubPolicy struct{ result string }

func (p *stubPolicy) EvaluateApprovalThreshold(_ context.Context, _, _, _ string, _ float64) (string, string, error) {
	if p.result == "" {
		return "WITHIN_THRESHOLD", "policy-version-1", nil
	}
	return p.result, "policy-version-1", nil
}

var _ policy.Client = (*stubPolicy)(nil)

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-ap10-1"
const testLegalEntity = "le-ap10-1"
const testPreparer = "principal-preparer"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, prop *stubProposal, sup *stubSupplier, pol *stubPolicy) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, prop, sup, pol, logger)
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
	return doRequestAs(r, method, path, body, tenantID, "principal-checker")
}

func requestAuthorization(t *testing.T, r http.Handler, proposalID string) *domain.PaymentAuthorization {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/ap10/authorizations/", domain.RequestAuthorizationRequest{ProposalID: proposalID}, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("requestAuthorization: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var a domain.PaymentAuthorization
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	return &a
}

func setupFrozenProposal(prop *stubProposal, sup *stubSupplier, proposalID, payeeRef string, updatedAt time.Time, netAmount float64) {
	items := []paymentproposal.Item{{PayableSource: "AP_INVOICE", PayableID: "inv-1", PayeeRef: payeeRef, PayeeSnapshotAt: &updatedAt}}
	prop.add(proposalID, testTenant, testLegalEntity, "FROZEN", testPreparer, netAmount, items)
	sup.set(testLegalEntity, payeeRef, updatedAt)
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestRequestPaymentAuthorization_Pending(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	updatedAt := time.Now().UTC()
	setupFrozenProposal(prop, sup, "prop-1", "vendor-1", updatedAt, 1000)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})

	a := requestAuthorization(t, r, "prop-1")
	if a.Status != domain.StatusPending {
		t.Fatalf("expected PENDING, got %s", a.Status)
	}
	if a.ProposalFingerprint == "" {
		t.Fatalf("expected fingerprint captured at request time")
	}
}

func TestRequestPaymentAuthorization_NotFrozen_Rejected(t *testing.T) {
	prop := newStubProposal()
	prop.add("prop-2", testTenant, testLegalEntity, "DRAFT", testPreparer, 500, nil)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, newStubSupplier(), &stubPolicy{})

	w := doRequest(r, http.MethodPost, "/ap10/authorizations/", domain.RequestAuthorizationRequest{ProposalID: "prop-2"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 not frozen, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequestPaymentAuthorization_DuplicateActiveRequest_Rejected(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-3", "vendor-2", time.Now().UTC(), 200)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	requestAuthorization(t, r, "prop-3")

	w := doRequest(r, http.MethodPost, "/ap10/authorizations/", domain.RequestAuthorizationRequest{ProposalID: "prop-3"}, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 duplicate active request, got %d: %s", w.Code, w.Body.String())
	}
}

// TestApprovePayment_SelfApproval_Denied is negative-path scenario #3, the
// fifth reuse of the dynamic own-object SoD layer this session.
func TestApprovePayment_SelfApproval_Denied(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-4", "vendor-3", time.Now().UTC(), 100)
	az := &stubAuthz{sodRules: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-4") // requested by testPreparer, proposal also prepared by testPreparer

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, testPreparer)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval denied, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApprovePayment_IndependentApprover_Succeeds(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-5", "vendor-4", time.Now().UTC(), 100)
	az := &stubAuthz{sodRules: true}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-5")

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 independent approval, got %d: %s", w.Code, w.Body.String())
	}
	var approved domain.PaymentAuthorization
	_ = json.Unmarshal(w.Body.Bytes(), &approved)
	if approved.Status != domain.StatusApproved {
		t.Fatalf("expected APPROVED, got %s", approved.Status)
	}
}

// TestApprovePayment_ApprovalRequired_UsesHighValueAction is negative-path
// scenario #2 ("signer exceeds delegated limit").
func TestApprovePayment_ApprovalRequired_UsesHighValueAction(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-6", "vendor-5", time.Now().UTC(), 100)
	az := &stubAuthz{}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, prop, sup, &stubPolicy{result: "APPROVAL_REQUIRED"})
	a := requestAuthorization(t, r, "prop-6")

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if az.lastAction != handler.PaymentAuthorizeHighValue {
		t.Fatalf("expected APPROVAL_REQUIRED to check %s, got %s", handler.PaymentAuthorizeHighValue, az.lastAction)
	}
}

// TestApprovePayment_FingerprintMismatch_Invalidated is negative-path #1's
// fingerprint half.
func TestApprovePayment_FingerprintMismatch_Invalidated(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-7", "vendor-6", time.Now().UTC(), 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-7")

	prop.fingerprints["prop-7"] = "fp-prop-7-v2-changed"

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 fingerprint mismatch, got %d: %s", w.Code, w.Body.String())
	}

	fetched, _ := newStubStoreFind(t, r, a.AuthorizationID, testTenant)
	if fetched.Status != domain.StatusInvalidated {
		t.Fatalf("expected INVALIDATED, got %s", fetched.Status)
	}
}

// TestApprovePayment_PayeeIdentityChanged_Invalidated is negative-path #1's
// payee-identity half.
func TestApprovePayment_PayeeIdentityChanged_Invalidated(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	initial := time.Now().UTC()
	setupFrozenProposal(prop, sup, "prop-8", "vendor-7", initial, 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-8")

	sup.set(testLegalEntity, "vendor-7", initial.Add(time.Hour))

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 payee identity changed, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRejectPayment(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-9", "vendor-8", time.Now().UTC(), 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-9")

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/reject",
		domain.RejectPaymentRequest{Reason: "wrong amount"}, testTenant, "principal-checker")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 rejecting, got %d: %s", w.Code, w.Body.String())
	}
}

// TestConsumePaymentAuthorization_ReplayBlocked is negative-path scenario
// #4.
func TestConsumePaymentAuthorization_ReplayBlocked(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-10", "vendor-9", time.Now().UTC(), 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-10")
	doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")

	w := doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/consume", nil, testTenant, "principal-executor")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 first consume, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/consume", nil, testTenant, "principal-executor")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 replay blocked, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRevokePaymentAuthorization(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-11", "vendor-10", time.Now().UTC(), 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-11")

	w := doRequest(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/revoke", domain.RevokeAuthorizationRequest{Reason: "duplicate request"}, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 revoking, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExpirePaymentAuthorization(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-12", "vendor-11", time.Now().UTC(), 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-12")

	w := doRequest(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/expire", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 expiring, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetAvailableActions_Pending(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-13", "vendor-12", time.Now().UTC(), 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-13")

	w := doRequest(r, http.MethodGet, "/ap10/authorizations/"+a.AuthorizationID+"/available-actions", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		AvailableActions []string `json:"available_actions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	found := map[string]bool{}
	for _, act := range resp.AvailableActions {
		found[act] = true
	}
	if !found["ApprovePayment"] || !found["RejectPayment"] || !found["RevokePaymentAuthorization"] {
		t.Fatalf("expected PENDING to allow approve/reject/revoke, got %v", resp.AvailableActions)
	}
	if found["ConsumePaymentAuthorization"] {
		t.Fatalf("did not expect Consume available on PENDING, got %v", resp.AvailableActions)
	}
}

func TestValidateAuthorization_ApprovedAndUnchanged(t *testing.T) {
	prop := newStubProposal()
	sup := newStubSupplier()
	setupFrozenProposal(prop, sup, "prop-14", "vendor-13", time.Now().UTC(), 100)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, prop, sup, &stubPolicy{})
	a := requestAuthorization(t, r, "prop-14")
	doRequestAs(r, http.MethodPost, "/ap10/authorizations/"+a.AuthorizationID+"/approve", nil, testTenant, "principal-checker")

	w := doRequest(r, http.MethodGet, "/ap10/authorizations/"+a.AuthorizationID+"/validate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Valid bool `json:"valid"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Valid {
		t.Fatalf("expected valid=true for an unchanged approved authorization")
	}
}

func TestGetPaymentAuthorization_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubProposal(), newStubSupplier(), &stubPolicy{})
	w := doRequest(r, http.MethodGet, "/ap10/authorizations/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// newStubStoreFind is a small test helper that re-fetches an authorization
// through the HTTP layer (not the store directly) so it exercises the same
// path the handler itself uses.
func newStubStoreFind(t *testing.T, r http.Handler, authorizationID, tenantID string) (*domain.PaymentAuthorization, *httptest.ResponseRecorder) {
	t.Helper()
	w := doRequest(r, http.MethodGet, "/ap10/authorizations/"+authorizationID, nil, tenantID)
	var resp struct {
		Authorization domain.PaymentAuthorization `json:"authorization"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return &resp.Authorization, w
}
