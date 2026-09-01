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

	authzpkg "zoiko.io/payee-banking-identity-svc/internal/authz"
	"zoiko.io/payee-banking-identity-svc/internal/counterparty"
	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/events"
	"zoiko.io/payee-banking-identity-svc/internal/handler"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
)

// ── stub publisher ───────────────────────────────────────────────────────────

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

// ── stub authz — including the own-object SoD layer and privileged-read gate ─

type stubAuthz struct {
	deny           bool
	sodRules       bool
	denyPrivileged bool
}

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, actionType string) error {
	if a.deny {
		return authzpkg.ErrAuthorizationDenied
	}
	if actionType == handler.PayeeMasterPrivilegedRead && a.denyPrivileged {
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

// ── stub counterparty-management-svc (ORG-07) client ────────────────────────

type stubParty struct {
	parties map[string]counterparty.Counterparty
}

func newStubParty() *stubParty { return &stubParty{parties: map[string]counterparty.Counterparty{}} }

func (p *stubParty) add(id, legalEntityID string) {
	p.parties[id] = counterparty.Counterparty{CounterpartyID: id, LegalEntityID: legalEntityID, Status: "ACTIVE", ComplianceStatus: "VERIFIED"}
}

func (p *stubParty) GetParty(_ context.Context, _, legalEntityID, partyRef string) (*counterparty.Counterparty, error) {
	cp, ok := p.parties[partyRef]
	if !ok || cp.LegalEntityID != legalEntityID {
		return nil, domain.ErrPartyNotFound
	}
	return &cp, nil
}

var _ counterparty.Client = (*stubParty)(nil)

// ── test harness ─────────────────────────────────────────────────────────────

const testTenant = "tenant-org10-1"
const testLegalEntity = "le-org10-1"

func newTestRouter(st *stubStore, pub *stubPublisher, az *stubAuthz, party *stubParty) chi.Router {
	logger := zap.NewNop()
	h := handler.New(st, pub, az, party, logger)
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
	return doRequestAs(r, method, path, body, tenantID, "principal-proposer")
}

func proposeDestination(t *testing.T, r http.Handler, partyRef string, source domain.SourceType) *domain.PayeeDestination {
	t.Helper()
	req := domain.ProposeDestinationRequest{
		LegalEntityID: testLegalEntity, PartyRef: partyRef, FinancialInstitution: "First National Bank",
		AccountIdentifier: "1234567890123456", CountryCode: "US", Currency: "USD", PayeeName: "Acme Supplies", SourceType: source,
	}
	w := doRequest(r, http.MethodPost, "/org10/destinations/", req, testTenant)
	if w.Code != http.StatusCreated {
		t.Fatalf("proposeDestination: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var d domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return &d
}

func verifyDestination(t *testing.T, r http.Handler, destinationID, method string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(r, http.MethodPost, "/org10/destinations/"+destinationID+"/verify",
		domain.VerifyDestinationRequest{VerificationMethod: method, VerificationEvidenceRef: "evidence-ref-1"}, testTenant)
}

func approveDestination(t *testing.T, r http.Handler, destinationID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAs(r, http.MethodPost, "/org10/destinations/"+destinationID+"/approve", nil, testTenant, "principal-checker")
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestProposePayeeDestination(t *testing.T) {
	party := newStubParty()
	party.add("party-1", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, party)

	d := proposeDestination(t, r, "party-1", domain.SourceSupplierPortal)
	if d.Status != domain.StatusCandidate {
		t.Fatalf("expected CANDIDATE, got %s", d.Status)
	}
	if d.AccountLast4 != "3456" {
		t.Fatalf("expected masked last4 3456, got %q", d.AccountLast4)
	}
}

func TestProposePayeeDestination_UnknownParty_Rejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubParty())
	w := doRequest(r, http.MethodPost, "/org10/destinations/", domain.ProposeDestinationRequest{
		LegalEntityID: testLegalEntity, PartyRef: "does-not-exist", FinancialInstitution: "Bank", AccountIdentifier: "12345",
		CountryCode: "US", Currency: "USD", PayeeName: "Acme", SourceType: domain.SourceSupplierPortal,
	}, testTenant)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 unknown party, got %d: %s", w.Code, w.Body.String())
	}
}

// TestVerifyPayeeDestination_InvoiceSourced_RequiresIndependentMethod is
// negative-path acceptance #1 ("invoice-supplied bank data never activates
// destination").
func TestVerifyPayeeDestination_InvoiceSourced_RequiresIndependentMethod(t *testing.T) {
	party := newStubParty()
	party.add("party-2", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, party)
	d := proposeDestination(t, r, "party-2", domain.SourceInvoiceOCR)

	w := verifyDestination(t, r, d.DestinationID, "INVOICE_OCR")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 non-independent verification, got %d: %s", w.Code, w.Body.String())
	}

	w = verifyDestination(t, r, d.DestinationID, "OUTBOUND_CALL_TO_SUPPLIER")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with a genuinely independent method, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApprovePayeeDestination_SelfApproval_Denied(t *testing.T) {
	party := newStubParty()
	party.add("party-3", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, party)
	d := proposeDestination(t, r, "party-3", domain.SourceSupplierPortal) // proposed by "principal-proposer"
	verifyDestination(t, r, d.DestinationID, "OUTBOUND_CALL_TO_SUPPLIER")

	w := doRequestAs(r, http.MethodPost, "/org10/destinations/"+d.DestinationID+"/approve", nil, testTenant, "principal-proposer")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 self-approval denied, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApprovePayeeDestination_IndependentApprover_Succeeds(t *testing.T) {
	party := newStubParty()
	party.add("party-4", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, party)
	d := proposeDestination(t, r, "party-4", domain.SourceSupplierPortal)
	verifyDestination(t, r, d.DestinationID, "OUTBOUND_CALL_TO_SUPPLIER")

	w := approveDestination(t, r, d.DestinationID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 independent approval, got %d: %s", w.Code, w.Body.String())
	}
	var approved domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &approved)
	if approved.Status != domain.StatusApprovalPending {
		t.Fatalf("expected APPROVAL_PENDING, got %s", approved.Status)
	}
}

// TestActivateDestination_SupersedesPriorActive is the literal enforcement
// of "only one active version per party/scope."
func TestActivateDestination_SupersedesPriorActive(t *testing.T) {
	party := newStubParty()
	party.add("party-5", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{sodRules: true}, party)

	first := proposeDestination(t, r, "party-5", domain.SourceSupplierPortal)
	verifyDestination(t, r, first.DestinationID, "OUTBOUND_CALL_TO_SUPPLIER")
	approveDestination(t, r, first.DestinationID)
	w := doRequest(r, http.MethodPost, "/org10/destinations/"+first.DestinationID+"/activate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 activating first destination, got %d: %s", w.Code, w.Body.String())
	}

	// A second, later destination for the same party supersedes the first.
	req := domain.ProposeDestinationRequest{
		LegalEntityID: testLegalEntity, PartyRef: "party-5", FinancialInstitution: "Second Bank",
		AccountIdentifier: "9999999999999999", CountryCode: "US", Currency: "USD", PayeeName: "Acme Supplies", SourceType: domain.SourceSupplierPortal,
	}
	w = doRequest(r, http.MethodPost, "/org10/destinations/", req, testTenant)
	var second domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	verifyDestination(t, r, second.DestinationID, "OUTBOUND_CALL_TO_SUPPLIER")
	approveDestination(t, r, second.DestinationID)
	w = doRequest(r, http.MethodPost, "/org10/destinations/"+second.DestinationID+"/activate", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 activating second destination, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequest(r, http.MethodGet, "/org10/destinations/"+first.DestinationID, nil, testTenant)
	var reFetchedFirst domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &reFetchedFirst)
	if reFetchedFirst.Status != domain.StatusSuperseded {
		t.Fatalf("expected first destination SUPERSEDED after second activated, got %s", reFetchedFirst.Status)
	}

	w = doRequest(r, http.MethodGet, "/org10/parties/party-5/active", nil, testTenant)
	var active domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &active)
	if active.DestinationID != second.DestinationID {
		t.Fatalf("expected the second destination to be the active one, got %s", active.DestinationID)
	}
}

// TestProposePayeeDestination_DuplicateFingerprint_Rejected verifies the
// spec's own named "destination candidate fingerprint detects duplicates."
func TestProposePayeeDestination_DuplicateFingerprint_Rejected(t *testing.T) {
	party := newStubParty()
	party.add("party-6", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, party)
	proposeDestination(t, r, "party-6", domain.SourceSupplierPortal)

	req := domain.ProposeDestinationRequest{
		LegalEntityID: testLegalEntity, PartyRef: "party-6", FinancialInstitution: "First National Bank",
		AccountIdentifier: "1234567890123456", CountryCode: "US", Currency: "USD", PayeeName: "Acme Supplies", SourceType: domain.SourceSupplierPortal,
	}
	w := doRequest(r, http.MethodPost, "/org10/destinations/", req, testTenant)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 duplicate destination, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGetPayeeDestination_PrivilegedRead_ReturnsFullAccount is the other
// half of "full account never overexposed."
func TestGetPayeeDestination_PrivilegedRead_ReturnsFullAccount(t *testing.T) {
	party := newStubParty()
	party.add("party-7", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, party)
	d := proposeDestination(t, r, "party-7", domain.SourceSupplierPortal)

	w := doRequest(r, http.MethodGet, "/org10/destinations/"+d.DestinationID, nil, testTenant)
	var privileged domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &privileged)
	if privileged.AccountIdentifier != "1234567890123456" {
		t.Fatalf("expected privileged caller to see the full account identifier, got %q", privileged.AccountIdentifier)
	}
}

func TestGetPayeeDestination_UnprivilegedRead_Masked(t *testing.T) {
	party := newStubParty()
	party.add("party-8", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{denyPrivileged: true}, party)
	d := proposeDestination(t, r, "party-8", domain.SourceSupplierPortal)

	w := doRequest(r, http.MethodGet, "/org10/destinations/"+d.DestinationID, nil, testTenant)
	var masked domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &masked)
	if masked.AccountIdentifier != "" {
		t.Fatalf("expected unprivileged caller to see a masked account identifier, got %q", masked.AccountIdentifier)
	}
	if masked.AccountLast4 == "" {
		t.Fatalf("expected the masked last4 to still be visible")
	}
}

func TestGetPayeeDestination_NotFound(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubParty())
	w := doRequest(r, http.MethodGet, "/org10/destinations/does-not-exist", nil, testTenant)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
