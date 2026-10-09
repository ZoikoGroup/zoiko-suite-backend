package domain

// ── Source-input provenance (§4 server-resolved context) ─────────────────────
//
// §4 lists source channel and workload identity among the SERVER-resolved
// inputs. Both arrive as client headers (X-Source-Channel, X-Workload-Id) that
// the edge does not strip, so on their own they are assertions. Until
// 2026-09-28 they were recorded verbatim, which made the evidence row a record
// of whatever the caller said.
//
// The service now resolves each against the one server-side fact it holds
// about the caller — the verified principal's type — and records HOW each value
// was established alongside the value. A value that contradicts the principal
// is not recorded at all: §4's negative-path column permits "preserve UNKNOWN
// state", and an unknown is honest where a contradicted assertion is not.

// SourceInputBasis says how a recorded source input was established.
type SourceInputBasis string

const (
	// BasisServerDerived — decided by this service from the verified principal;
	// the client supplied nothing, or supplied the same thing.
	BasisServerDerived SourceInputBasis = "SERVER_DERIVED"

	// BasisVerifiedPrincipal — the workload IS the verified principal (a
	// service account or API client authenticates as itself).
	BasisVerifiedPrincipal SourceInputBasis = "VERIFIED_PRINCIPAL"

	// BasisVerifiedClient — derived from the verified IdP token's client
	// (azp) through the configured IDP_CLIENT_CHANNELS map: the web console's
	// client is "web", the mobile app's is "mobile". Settles what the principal
	// type alone cannot (web vs mobile for a human).
	BasisVerifiedClient SourceInputBasis = "VERIFIED_CLIENT"

	// BasisAssertedConsistent — asserted by the client and checked: it is one
	// of the values the principal's type can legitimately arrive on. What
	// cannot be checked here is WHICH of those values is true (web or mobile
	// for a human); that needs the edge to stamp the channel.
	BasisAssertedConsistent SourceInputBasis = "CLIENT_ASSERTED_CONSISTENT"

	// BasisRejectedInconsistent — asserted, contradicted by the principal, and
	// the ASSERTION is not recorded. A human claiming "system" leaves the
	// channel empty; a service account naming another workload has its own
	// verified id recorded in place of the claim. Either way the row carries
	// evidence that a contradicted assertion was made.
	BasisRejectedInconsistent SourceInputBasis = "REJECTED_INCONSISTENT"

	// BasisUnverifiableDiscarded — asserted, but nothing here can confirm or
	// refute it (a workload id presented on a human session), so NOT recorded.
	BasisUnverifiableDiscarded SourceInputBasis = "UNVERIFIABLE_DISCARDED"

	// BasisNotPresented — nothing asserted and nothing derivable.
	BasisNotPresented SourceInputBasis = "NOT_PRESENTED"
)

// EntitlementContextStatus says what became of §4's entitlement context
// reference, so "never resolved" and "resolved to nothing" stay distinct.
type EntitlementContextStatus string

const (
	EntitlementResolved EntitlementContextStatus = "RESOLVED"

	// EntitlementUpstreamNotConfigured — no entitlement read model is wired.
	// The state of every deployment today: COM-03 Entitlement, which owns the
	// EntitlementSnapshot the reference points at, is not implemented anywhere
	// in the estate.
	EntitlementUpstreamNotConfigured EntitlementContextStatus = "UPSTREAM_NOT_CONFIGURED"

	// EntitlementUpstreamUnavailable — wired, and the call failed.
	EntitlementUpstreamUnavailable EntitlementContextStatus = "UPSTREAM_UNAVAILABLE"
)

// SourceInputs is the provenance section of an explanation: every §4 source
// input the decision recorded, and how each was established.
//
// These were written to session_contexts from migration 000008 and never read
// back, so the explain endpoint could not show the channel, workload, causation
// or ingress version a decision was made against.
type SourceInputs struct {
	SourceChannel            string                   `json:"source_channel,omitempty"`
	SourceChannelBasis       SourceInputBasis         `json:"source_channel_basis,omitempty"`
	WorkloadID               string                   `json:"workload_id,omitempty"`
	WorkloadIDBasis          SourceInputBasis         `json:"workload_id_basis,omitempty"`
	CausationID              string                   `json:"causation_id,omitempty"`
	IngressBindingVersion    string                   `json:"ingress_binding_version,omitempty"`
	IngressCacheState        CacheFreshness           `json:"ingress_cache_state,omitempty"`
	EntitlementContextRef    *string                  `json:"entitlement_context_ref,omitempty"`
	EntitlementContextStatus EntitlementContextStatus `json:"entitlement_context_status,omitempty"`
}
