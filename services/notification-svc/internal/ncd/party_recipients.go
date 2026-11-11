package ncd

import (
	"context"
	"errors"
	"strings"
	"time"

	"zoiko.io/notification-svc/internal/partycontacts"
)

// S7-C1 (ZS-SVC-Y-001 NCD-02 s5.2, INV-16): an external contact that is not a platform principal is addressed through a
// party of the SENDER'S OWN tenant. The recipient is written `party:<party_id>` in recipient_principal_id, a reserved
// namespace, so it travels through plans, communications and re-resolution with no new stored field. The address comes
// from the party's VERIFIED contact points in tenant-entity-registry-svc; nothing the caller types is trusted, and a
// party of another tenant is simply not found there. Cross-tenant recipients stay refused (NCD-020): no federation
// model exists to authorize one.

// PartyRecipientPrefix marks a recipient that is a party, not a principal.
const PartyRecipientPrefix = "party:"

// PartyRecipient returns the party id of a `party:<id>` recipient.
func PartyRecipient(recipient string) (partyID string, ok bool) {
	if !strings.HasPrefix(recipient, PartyRecipientPrefix) {
		return "", false
	}
	id := strings.TrimSpace(strings.TrimPrefix(recipient, PartyRecipientPrefix))
	return id, id != ""
}

// ProvenancePartyMaster is the provenance of an address that came from a party's verified contact point.
const ProvenancePartyMaster = "PARTY_MASTER"

// PartyContactSource reads a party's verified, in-force email contact points. *partycontacts.Client implements it.
type PartyContactSource interface {
	VerifiedEmails(ctx context.Context, tenantID, correlationID, partyID string, at time.Time) ([]partycontacts.Contact, error)
}

// SetPartyContacts turns on addressing a communication to a party of the sender's tenant.
func (s *Service) SetPartyContacts(p PartyContactSource) { s.partyContacts = p }

// allowedContactPurposes are the contact-point purposes an intent may use. A legal-notice address is for legal
// notices, and promotion goes only to an address given for general contact.
func allowedContactPurposes(intent Intent) []string {
	switch intent.PurposeClass {
	case PurposeRegulated:
		return []string{"LEGAL_NOTICE"}
	case PurposeMarketing:
		return []string{"GENERAL"}
	default:
		return []string{"GENERAL", "BILLING"}
	}
}

// resolvePartyEndpoints resolves a `party:` recipient. Network I/O, so it runs outside any database transaction.
func (s *Service) resolvePartyEndpoints(ctx context.Context, a Actor, intent Intent, in RecipientInput, partyID string,
	evidence map[string]any, now time.Time) ([]Endpoint, map[string]any, error) {
	if in.FreeTextEndpoint != nil {
		return nil, evidence, Invalid("free_text_with_party", "a party recipient is addressed from its verified contact points; free_text_endpoint cannot be combined with it")
	}
	if !intent.Allows(ChannelEmail) {
		return nil, evidence, Refused(Refuse(NCD006RecipientUnresolved,
			"a party recipient has verified email contact points only, and the intent does not allow EMAIL"))
	}
	if s.partyContacts == nil {
		return nil, evidence, Unavailable("party_contacts_not_configured", "no party contact source is configured, so a party recipient cannot be resolved")
	}
	evidence["recipient_kind"] = "PARTY"
	contacts, err := s.partyContacts.VerifiedEmails(ctx, a.TenantID, a.CorrelationID, partyID, now)
	switch {
	case errors.Is(err, partycontacts.ErrNotFound):
		return nil, evidence, Refused(Refuse(NCD006RecipientUnresolved, "no party "+partyID+" in this tenant"))
	case err != nil:
		return nil, evidence, Unavailable("party_registry_unavailable", "party contact resolution could not reach the registry: %v", err)
	}
	purposes := allowedContactPurposes(intent)
	for _, c := range contacts {
		if !containsString(purposes, c.Purpose) {
			continue
		}
		evidence["party_contact"] = map[string]any{"contact_point_id": c.ContactPointID, "purpose": c.Purpose, "evidence_ref": c.EvidenceRef}
		return []Endpoint{{Channel: ChannelEmail, Address: c.Value, Provenance: ProvenancePartyMaster, Verified: true, ResolvedAt: now,
			ProvenanceRef: "tenant-entity-registry-svc:party/" + partyID + "/contact-point/" + c.ContactPointID}}, evidence, nil
	}
	evidence["party_contacts_considered"] = len(contacts)
	return nil, evidence, Refused(Refuse(NCD006RecipientUnresolved,
		"party "+partyID+" has no verified, in-force email contact point for the purpose this intent allows ("+strings.Join(purposes, ", ")+")"))
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
