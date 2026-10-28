package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
)

// TestAllSixCommands_RequireIdempotencyKey is the header contract for every
// ORG-10 named command. The key is checked before the destination lookup or
// any store call.
func TestAllSixCommands_RequireIdempotencyKey(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, newStubParty())
	propose := domain.ProposeDestinationRequest{
		LegalEntityID: testLegalEntity, PartyRef: "party-any", FinancialInstitution: "Bank",
		AccountIdentifier: "1234567890123456", CountryCode: "US", Currency: "USD",
		PayeeName: "Acme", SourceType: domain.SourceSupplierPortal,
	}
	cases := []struct {
		name string
		path string
		body interface{}
	}{
		{"ProposePayeeDestination", "/org10/destinations/", propose},
		{"VerifyPayeeDestination", "/org10/destinations/x/verify", domain.VerifyDestinationRequest{VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "e"}},
		{"ApprovePayeeDestination", "/org10/destinations/x/approve", nil},
		{"ActivateDestination", "/org10/destinations/x/activate", nil},
		{"SuspendDestination", "/org10/destinations/x/suspend", domain.SuspendDestinationRequest{Reason: "fraud"}},
		{"SupersedeDestination", "/org10/destinations/x/supersede", domain.SupersedeDestinationRequest{Reason: "replaced"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequestAsWithHeaders(r, http.MethodPost, tc.path, tc.body, testTenant, "principal-proposer",
				map[string]string{"Idempotency-Key": ""})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 without Idempotency-Key, got %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "Idempotency-Key") {
				t.Fatalf("expected a clear missing-key error, got %s", w.Body.String())
			}
		})
	}
}

func TestIdempotencyKey_TooLongIsRejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, newStubParty())
	w := doRequestWithKey(r, http.MethodPost, "/org10/destinations/x/approve", nil, testTenant, "principal-checker", strings.Repeat("x", 256))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a 256-byte key, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "at most 255") {
		t.Fatalf("expected a length-specific error, got %s", w.Body.String())
	}
}

func TestCommandIdempotency_ReplayAndConflict(t *testing.T) {
	party := newStubParty()
	party.add("party-idem", testLegalEntity)
	r := newTestRouter(newStubStore(), &stubAuthz{}, party)

	proposeReq := domain.ProposeDestinationRequest{
		LegalEntityID: testLegalEntity, PartyRef: "party-idem", FinancialInstitution: "Bank",
		AccountIdentifier: "1234567890123456", CountryCode: "US", Currency: "USD",
		PayeeName: "Acme", SourceType: domain.SourceSupplierPortal,
	}

	first := doRequestWithKey(r, http.MethodPost, "/org10/destinations/", proposeReq, testTenant, "principal-proposer", "k-propose")
	if first.Code != http.StatusCreated {
		t.Fatalf("expected 201 first propose, got %d: %s", first.Code, first.Body.String())
	}
	var created domain.PayeeDestination
	_ = json.Unmarshal(first.Body.Bytes(), &created)

	replay := doRequestWithKey(r, http.MethodPost, "/org10/destinations/", proposeReq, testTenant, "principal-proposer", "k-propose")
	if replay.Code != http.StatusOK {
		t.Fatalf("expected 200 replay, got %d: %s", replay.Code, replay.Body.String())
	}
	if replay.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("expected Idempotent-Replay: true, got %q", replay.Header().Get("Idempotent-Replay"))
	}
	var replayed domain.PayeeDestination
	_ = json.Unmarshal(replay.Body.Bytes(), &replayed)
	if replayed.DestinationID != created.DestinationID {
		t.Fatalf("replay returned %s, want %s", replayed.DestinationID, created.DestinationID)
	}

	// Reusing the propose key for a different command is a conflict, not a
	// second state transition.
	conflict := doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+created.DestinationID+"/verify",
		domain.VerifyDestinationRequest{VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "e"},
		testTenant, "principal-proposer", "k-propose")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("expected 409 key reuse across commands, got %d: %s", conflict.Code, conflict.Body.String())
	}

	verifyReq := domain.VerifyDestinationRequest{VerificationMethod: "INDEPENDENT_CALL", VerificationEvidenceRef: "e"}
	if w := doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+created.DestinationID+"/verify", verifyReq, testTenant, "principal-proposer", "k-verify"); w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
	if w := doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+created.DestinationID+"/approve", nil, testTenant, "principal-checker", "k-approve"); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}

	// A retried verify after the destination has moved to APPROVAL_PENDING
	// must replay its original result, not fail the current-state guard.
	w := doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+created.DestinationID+"/verify", verifyReq, testTenant, "principal-proposer", "k-verify")
	if w.Code != http.StatusOK || w.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("verify replay after approval: status %d replay %q body %s", w.Code, w.Header().Get("Idempotent-Replay"), w.Body.String())
	}
	var afterApprove domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &afterApprove)
	if afterApprove.Status != domain.StatusApprovalPending {
		t.Fatalf("verify replay should return the current row, got status %s", afterApprove.Status)
	}

	if w := doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+created.DestinationID+"/activate", nil, testTenant, "principal-activator", "k-activate"); w.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", w.Code, w.Body.String())
	}
	if w := doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+created.DestinationID+"/suspend", domain.SuspendDestinationRequest{Reason: "fraud"}, testTenant, "principal-suspender", "k-suspend"); w.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", w.Code, w.Body.String())
	}

	// Replay after a later state change returns the current SUSPENDED row
	// and must not reactivate it.
	w = doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+created.DestinationID+"/activate", nil, testTenant, "principal-activator", "k-activate")
	if w.Code != http.StatusOK || w.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("activate replay after suspend: status %d replay %q body %s", w.Code, w.Header().Get("Idempotent-Replay"), w.Body.String())
	}
	var afterSuspend domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &afterSuspend)
	if afterSuspend.Status != domain.StatusSuspended {
		t.Fatalf("replay must not reactivate; got %s", afterSuspend.Status)
	}

	// Explicit supersede is the sixth command; use a second destination.
	party.add("party-idem-2", testLegalEntity)
	secondReq := proposeReq
	secondReq.PartyRef = "party-idem-2"
	secondReq.AccountIdentifier = "9999999999999999"
	w = doRequestWithKey(r, http.MethodPost, "/org10/destinations/", secondReq, testTenant, "principal-proposer", "k-propose-2")
	if w.Code != http.StatusCreated {
		t.Fatalf("propose second: %d %s", w.Code, w.Body.String())
	}
	var second domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	if w := doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+second.DestinationID+"/verify", verifyReq, testTenant, "principal-proposer", "k-verify-2"); w.Code != http.StatusOK {
		t.Fatalf("verify second: %d %s", w.Code, w.Body.String())
	}
	if w := doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+second.DestinationID+"/supersede", domain.SupersedeDestinationRequest{Reason: "replaced"}, testTenant, "principal-admin", "k-supersede"); w.Code != http.StatusOK {
		t.Fatalf("supersede: %d %s", w.Code, w.Body.String())
	}
	w = doRequestWithKey(r, http.MethodPost, "/org10/destinations/"+second.DestinationID+"/supersede", domain.SupersedeDestinationRequest{Reason: "replaced"}, testTenant, "principal-admin", "k-supersede")
	if w.Code != http.StatusOK || w.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("supersede replay: status %d replay %q body %s", w.Code, w.Header().Get("Idempotent-Replay"), w.Body.String())
	}
}

func TestListPayeeVersions_PrivilegedCheckOncePerEntity(t *testing.T) {
	party := newStubParty()
	party.add("party-list", testLegalEntity)
	az := &stubAuthz{}
	r := newTestRouter(newStubStore(), az, party)

	for i, account := range []string{"1111111111111111", "2222222222222222", "3333333333333333"} {
		req := domain.ProposeDestinationRequest{
			LegalEntityID: testLegalEntity, PartyRef: "party-list", FinancialInstitution: "Bank",
			AccountIdentifier: account, CountryCode: "US", Currency: "USD",
			PayeeName: "Acme", SourceType: domain.SourceSupplierPortal,
		}
		if w := doRequest(r, http.MethodPost, "/org10/destinations/", req, testTenant); w.Code != http.StatusCreated {
			t.Fatalf("propose %d: %d %s", i, w.Code, w.Body.String())
		}
	}

	az.privilegedCalls = 0
	w := doRequest(r, http.MethodGet, "/org10/parties/party-list/versions", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("list versions: %d %s", w.Code, w.Body.String())
	}
	if az.privilegedCalls != 1 {
		t.Fatalf("expected one privileged-read check for three versions in one entity, got %d", az.privilegedCalls)
	}
	var body struct {
		Data []domain.PayeeDestination `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Data) != 3 {
		t.Fatalf("expected 3 versions, got %d", len(body.Data))
	}
	for _, d := range body.Data {
		if d.AccountIdentifier != "1111111111111111" && d.AccountIdentifier != "2222222222222222" && d.AccountIdentifier != "3333333333333333" {
			t.Fatalf("unexpected/masked account identifier %q", d.AccountIdentifier)
		}
	}
}
