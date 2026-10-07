package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
)

// fullAccount is the value proposeDestination submits. It must never appear on
// the event bus, in any event, in any form.
const fullAccount = "1234567890123456"

// TestDestinationEvents_NeverCarryFullAccount drives every lifecycle command
// that publishes (propose, approve, activate, suspend, supersede) and asserts,
// on the bytes that would be written to Kafka, that:
//   - the full account identifier is absent;
//   - the payload is the safe projection, not a PayeeDestination;
//   - the tenant is present on both the event and the payload.
//
// Masking the HTTP response is not an event control: events are serialized
// before the response is built, so this test inspects the event itself.
func TestDestinationEvents_NeverCarryFullAccount(t *testing.T) {
	party := newStubParty()
	party.add("party-ev", testLegalEntity)
	pub := &stubPublisher{}
	r := newTestRouter(newStubStore(), pub, &stubAuthz{sodRules: true}, party)

	// Activated destination → suspend.
	first := proposeDestination(t, r, "party-ev", domain.SourceSupplierPortal)
	verifyDestination(t, r, first.DestinationID, "OUTBOUND_CALL_TO_SUPPLIER")
	approveDestination(t, r, first.DestinationID)
	if w := doRequest(r, http.MethodPost, "/org10/destinations/"+first.DestinationID+"/activate", nil, testTenant); w.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", w.Code, w.Body.String())
	}
	if w := doRequest(r, http.MethodPost, "/org10/destinations/"+first.DestinationID+"/suspend",
		domain.SuspendDestinationRequest{Reason: "fraud review"}, testTenant); w.Code != http.StatusOK {
		t.Fatalf("suspend: %d %s", w.Code, w.Body.String())
	}

	// Second destination → supersede path.
	req := domain.ProposeDestinationRequest{
		LegalEntityID: testLegalEntity, PartyRef: "party-ev", FinancialInstitution: "Second Bank",
		AccountIdentifier: "9999888877776666", CountryCode: "US", Currency: "USD", PayeeName: "Acme Supplies", SourceType: domain.SourceSupplierPortal,
	}
	w := doRequest(r, http.MethodPost, "/org10/destinations/", req, testTenant)
	var second domain.PayeeDestination
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	verifyDestination(t, r, second.DestinationID, "OUTBOUND_CALL_TO_SUPPLIER") // only VERIFIED/APPROVAL_PENDING/ACTIVE can be superseded
	if w := doRequest(r, http.MethodPost, "/org10/destinations/"+second.DestinationID+"/supersede",
		domain.SupersedeDestinationRequest{Reason: "replaced"}, testTenant); w.Code != http.StatusOK {
		t.Fatalf("supersede: %d %s", w.Code, w.Body.String())
	}

	wantTypes := map[string]bool{
		domain.EventPayeeDestinationProposed: false, domain.EventPayeeDestinationApproved: false,
		domain.EventPayeeDestinationActivated: false, domain.EventPayeeDestinationSuspended: false,
		domain.EventPayeeDestinationSuperseded: false,
	}
	secrets := []string{fullAccount, "9999888877776666", "First National Bank", "Second Bank", "Acme Supplies"}

	for _, p := range pub.params {
		wantTypes[p.EventType] = true

		if p.TenantID != testTenant {
			t.Errorf("%s: event TenantID = %q, want %q", p.EventType, p.TenantID, testTenant)
		}
		payload, ok := p.Payload.(domain.DestinationEventPayload)
		if !ok {
			t.Fatalf("%s: payload is %T, want domain.DestinationEventPayload (never PayeeDestination)", p.EventType, p.Payload)
		}
		if payload.TenantID != testTenant {
			t.Errorf("%s: payload tenant_id = %q, want %q", p.EventType, payload.TenantID, testTenant)
		}
		if payload.DestinationID == "" || payload.AccountLast4 == "" || payload.Fingerprint == "" {
			t.Errorf("%s: projection lost its identifying fields: %+v", p.EventType, payload)
		}

		raw, err := json.Marshal(p.Payload)
		if err != nil {
			t.Fatalf("%s: marshal: %v", p.EventType, err)
		}
		for _, s := range secrets {
			if strings.Contains(string(raw), s) {
				t.Errorf("%s: event payload contains %q: %s", p.EventType, s, raw)
			}
		}
		if strings.Contains(strings.ToLower(string(raw)), "accountidentifier") || strings.Contains(strings.ToLower(string(raw)), "account_identifier") {
			t.Errorf("%s: event payload carries an account identifier field: %s", p.EventType, raw)
		}
	}
	for et, seen := range wantTypes {
		if !seen {
			t.Errorf("no %s event was published", et)
		}
	}
}

// TestDestinationEvents_NotAffectedByPrivilegedReader pins the reason the
// projection exists: the event is identical whether or not the acting
// principal may read the full account, because it never contains it.
func TestDestinationEvents_NotAffectedByPrivilegedReader(t *testing.T) {
	party := newStubParty()
	party.add("party-priv", testLegalEntity)
	pub := &stubPublisher{}
	r := newTestRouter(newStubStore(), pub, &stubAuthz{}, party) // privileged: HTTP response shows the full account

	d := proposeDestination(t, r, "party-priv", domain.SourceSupplierPortal)
	if d.AccountIdentifier != fullAccount {
		t.Fatalf("precondition: privileged caller should see the full account over HTTP, got %q", d.AccountIdentifier)
	}
	if len(pub.params) != 1 {
		t.Fatalf("expected 1 event, got %d", len(pub.params))
	}
	raw, _ := json.Marshal(pub.params[0].Payload)
	if strings.Contains(string(raw), fullAccount) {
		t.Fatalf("event leaked the full account even though HTTP masking is separate: %s", raw)
	}
}
