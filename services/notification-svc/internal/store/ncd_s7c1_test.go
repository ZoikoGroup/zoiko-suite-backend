package store_test

import (
	"context"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/ncd"
	"zoiko.io/notification-svc/internal/partycontacts"
)

type scriptedParties struct {
	contacts []partycontacts.Contact
	err      error
	asked    []string
}

func (s *scriptedParties) VerifiedEmails(_ context.Context, tenantID, _ string, partyID string, _ time.Time) ([]partycontacts.Contact, error) {
	s.asked = append(s.asked, tenantID+"/"+partyID)
	return s.contacts, s.err
}

func contact(id, value, purpose string) partycontacts.Contact {
	return partycontacts.Contact{ContactPointID: id, Value: value, Purpose: purpose, Status: "VERIFIED", Channel: "EMAIL"}
}

// S7-C1: an external contact is addressed through a party of the sender's own tenant, from its verified contact point.
func TestS7C1_APartyIsAddressedThroughItsVerifiedContactPoint(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil)
	parties := &scriptedParties{contacts: []partycontacts.Contact{contact("cp-1", "billing@customer.example", "BILLING")}}

	// No source wired: a party recipient cannot be resolved, and says why.
	_, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "party:p-1"})
	h.kind(err, ncd.KindUnavailable, "party_contacts_not_configured")

	h.svc.SetPartyContacts(parties)
	plan, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "party:p-1"})
	h.must(err)
	if len(plan.Endpoints) != 1 {
		t.Fatalf("one verified EMAIL endpoint: %+v", plan.Endpoints)
	}
	ep := plan.Endpoints[0]
	if ep.Channel != "EMAIL" || ep.Address != "billing@customer.example" || !ep.Verified || ep.Provenance != ncd.ProvenancePartyMaster ||
		ep.ProvenanceRef != "tenant-entity-registry-svc:party/p-1/contact-point/cp-1" {
		t.Fatalf("endpoint and provenance: %+v", ep)
	}
	if ep.EndpointMasked == "" || ep.EndpointMasked == ep.Address {
		t.Fatalf("the address is masked for display: %q", ep.EndpointMasked)
	}
	if len(parties.asked) != 1 || parties.asked[0] != h.tenant+"/p-1" {
		t.Fatalf("the registry is asked under the sender's own tenant: %v", parties.asked)
	}
	if plan.RecipientPrincipalID != "party:p-1" {
		t.Fatalf("the recipient is the party: %s", plan.RecipientPrincipalID)
	}
}

func TestS7C1_AContactIsUsedOnlyForThePurposeItWasGivenFor(t *testing.T) {
	h := newHarness(t)
	h.svc.SetPartyContacts(&scriptedParties{contacts: []partycontacts.Contact{contact("cp-legal", "legal@customer.example", "LEGAL_NOTICE")}})

	// A transactional intent does not use a legal-notice address.
	i := h.intent(nil)
	_, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "party:p-1"})
	h.refusal(err, ncd.NCD006RecipientUnresolved)

	// A regulated notice does.
	reg := h.regulatedIntent()
	plan, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: reg.IntentID, RecipientPrincipalID: "party:p-1"})
	h.must(err)
	if plan.Endpoints[0].Address != "legal@customer.example" {
		t.Fatalf("%+v", plan.Endpoints)
	}
}

func TestS7C1_APartyThatIsNotFoundOrUnreachableIsNeverGuessed(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil)
	parties := &scriptedParties{err: partycontacts.ErrNotFound}
	h.svc.SetPartyContacts(parties)
	_, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "party:other-tenants"})
	h.refusal(err, ncd.NCD006RecipientUnresolved)

	parties.err = partycontacts.ErrUnavailable
	_, err = h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "party:p-1"})
	h.kind(err, ncd.KindUnavailable, "party_registry_unavailable")

	parties.err, parties.contacts = nil, nil
	_, err = h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "party:p-1"})
	h.refusal(err, ncd.NCD006RecipientUnresolved)
}

func TestS7C1_APartyCannotBeCombinedWithFreeTextOrWithAnotherTenant(t *testing.T) {
	h := newHarness(t)
	i := h.intent(nil)
	h.svc.SetPartyContacts(&scriptedParties{contacts: []partycontacts.Contact{contact("cp-1", "a@customer.example", "GENERAL")}})

	_, err := h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "party:p-1",
		FreeTextEndpoint: &ncd.FreeTextEndpoint{Channel: ncd.ChannelEmail, Address: "other@example.com"}})
	h.kind(err, ncd.KindInvalid, "free_text_with_party")

	// Cross-tenant stays refused: no federation model exists.
	_, err = h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: i.IntentID, RecipientPrincipalID: "party:p-1", RecipientTenantID: "another-tenant"})
	h.refusal(err, ncd.NCD020CrossTenantRecipientBlock)

	// An intent that does not allow EMAIL cannot use a party (its contacts are email only).
	inApp := h.intent(func(in *ncd.IntentInput) { in.IntentCode, in.AllowedChannels = "inapp.only", []string{"IN_APP"} })
	_, err = h.svc.ResolveRecipient(h.ctx, h.author, ncd.RecipientInput{IntentID: inApp.IntentID, RecipientPrincipalID: "party:p-1"})
	h.refusal(err, ncd.NCD006RecipientUnresolved)
}
