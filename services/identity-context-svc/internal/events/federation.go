package events

// INT-10 identity federation events. They are enqueued by the store inside the same transaction as the fact they
// attest (see store/pg_federation.go and store/pg_provisioning.go), and carry identifiers and decisions, never a
// token, a secret or an email address.
const (
	// Federation connections and their trust configuration.
	EventFederationVersionProposed = "identity.federation.version_proposed"
	EventFederationVersionApproved = "identity.federation.version_approved"
	EventFederationVersionRejected = "identity.federation.version_rejected"
	EventFederationActivated       = "identity.federation.activated"
	EventFederationSuspended       = "identity.federation.suspended"
	EventFederationResumed         = "identity.federation.resumed"
	EventFederationRevoked         = "identity.federation.revoked"
	EventFederationDegraded        = "identity.federation.degraded"
	EventFederationRecovered       = "identity.federation.recovered"

	// Federated login.
	EventFederationLoginSucceeded = "identity.federation.login_succeeded"
	EventFederationLoginDenied    = "identity.federation.login_denied"

	// Provisioning.
	EventFederationSCIMTokenIssued    = "identity.federation.scim_token_issued"
	EventFederationSCIMTokenRevoked   = "identity.federation.scim_token_revoked"
	EventFederationMappingProposed    = "identity.federation.mapping_proposed"
	EventFederationMappingApproved    = "identity.federation.mapping_approved"
	EventFederationMappingRejected    = "identity.federation.mapping_rejected"
	EventFederationDeprovisionApplied = "identity.federation.deprovision_applied"
	EventFederationDeprovisionLate    = "identity.federation.deprovision_escalated"

	EventIdentityLinkCreated  = "identity.link.created"
	EventProvisioningApplied  = "identity.provisioning.applied"
	EventProvisioningRejected = "identity.provisioning.rejected"
)

// FederationEventTypes lists every INT-10 event, for the contract test.
func FederationEventTypes() []string {
	return []string{
		EventFederationVersionProposed, EventFederationVersionApproved, EventFederationVersionRejected, EventFederationActivated,
		EventFederationSuspended, EventFederationResumed, EventFederationRevoked, EventFederationDegraded, EventFederationRecovered,
		EventFederationLoginSucceeded, EventFederationLoginDenied,
		EventFederationSCIMTokenIssued, EventFederationSCIMTokenRevoked, EventFederationMappingProposed, EventFederationMappingApproved,
		EventFederationMappingRejected, EventFederationDeprovisionApplied, EventFederationDeprovisionLate,
		EventIdentityLinkCreated, EventProvisioningApplied, EventProvisioningRejected,
	}
}
