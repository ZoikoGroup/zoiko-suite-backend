package context

import (
	"context"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	svcenvelope "zoiko.io/identity-context-svc/internal/envelope"
)

// channelsByPrincipalType is which §4 source channels each kind of principal can
// legitimately arrive on.
//
// This is the server-side half of resolving source_channel. It cannot tell a
// human on the web console from the same human on mobile — both are legitimate
// and only the edge knows which door was used — but it can refuse the
// assertions that are impossible: a human arriving as a "scheduled_job", or a
// service account arriving as "web". Those are the ones that matter, because
// §5 derives provenance class from the channel and import/integration impose
// source_system obligations.
var channelsByPrincipalType = map[domain.PrincipalType]map[svcenvelope.SourceChannel]bool{
	domain.PrincipalTypeHuman: {
		svcenvelope.ChannelWeb: true, svcenvelope.ChannelMobile: true, svcenvelope.ChannelAPI: true,
	},
	domain.PrincipalTypeAPIClient: {
		svcenvelope.ChannelAPI: true, svcenvelope.ChannelIntegration: true, svcenvelope.ChannelImport: true,
	},
	domain.PrincipalTypeServiceAccount: {
		svcenvelope.ChannelSystem: true, svcenvelope.ChannelScheduledJob: true,
		svcenvelope.ChannelIntegration: true, svcenvelope.ChannelImport: true,
	},
}

// defaultChannelByPrincipalType is the channel the server derives when none is
// asserted. Humans have none: web and mobile are equally likely, and guessing
// would put a fabricated fact into the evidence.
var defaultChannelByPrincipalType = map[domain.PrincipalType]svcenvelope.SourceChannel{
	domain.PrincipalTypeAPIClient:      svcenvelope.ChannelAPI,
	domain.PrincipalTypeServiceAccount: svcenvelope.ChannelSystem,
}

// resolveSourceChannel decides the source_channel recorded on the decision.
func resolveSourceChannel(p *domain.Principal, asserted string) (string, domain.SourceInputBasis) {
	def, hasDefault := defaultChannelByPrincipalType[p.PrincipalType]
	if asserted == "" {
		if hasDefault {
			return string(def), domain.BasisServerDerived
		}
		return "", domain.BasisNotPresented
	}
	if !channelsByPrincipalType[p.PrincipalType][svcenvelope.SourceChannel(asserted)] {
		return "", domain.BasisRejectedInconsistent
	}
	if hasDefault && string(def) == asserted {
		return asserted, domain.BasisServerDerived
	}
	return asserted, domain.BasisAssertedConsistent
}

// resolveWorkloadID decides the workload identity recorded on the decision.
//
// A service account or API client authenticates AS the workload, so its
// verified principal id is the workload identity — no header needed, and a
// header naming a different workload is a claim to be someone else. A human
// session may be driven by a workload (a console backend acting for the user),
// but nothing reaching this service can attest which one, so an asserted id is
// discarded rather than recorded as fact.
func resolveWorkloadID(p *domain.Principal, asserted string) (string, domain.SourceInputBasis) {
	switch p.PrincipalType {
	case domain.PrincipalTypeServiceAccount, domain.PrincipalTypeAPIClient:
		if asserted != "" && asserted != p.PrincipalID {
			return p.PrincipalID, domain.BasisRejectedInconsistent
		}
		return p.PrincipalID, domain.BasisVerifiedPrincipal
	default:
		if asserted == "" {
			return "", domain.BasisNotPresented
		}
		return "", domain.BasisUnverifiableDiscarded
	}
}

// EntitlementResolver resolves §4's entitlement context reference.
//
// The dependency §4 names is a "commercial entitlement read model". Its owner
// is COM-03 Entitlement (Commercial Platform spec §4.3, EntitlementSnapshot),
// which is not implemented anywhere in the estate, so there is deliberately no
// HTTP implementation of this interface: writing a client against an API that
// does not exist would mean guessing its paths and payloads. When COM-03 ships,
// an implementation is wired with Resolver.WithEntitlementResolver and nothing
// else changes — the column, the status and the explain output already exist.
type EntitlementResolver interface {
	ResolveEntitlementContext(ctx context.Context, tenantID, legalEntityID string) (*string, error)
}

// WithEntitlementResolver wires the entitlement read model.
func (r *Resolver) WithEntitlementResolver(e EntitlementResolver) *Resolver {
	r.entitlements = e
	return r
}

// resolveEntitlement never fails the resolution.
//
// Non-blocking on purpose: authentication must not depend on the commercial
// plane being up, and no consumer in the estate yet enforces on the reference.
// The status records which of the three outcomes happened, so a decision made
// without an entitlement context says so.
func (r *Resolver) resolveEntitlement(ctx context.Context, tenantID, legalEntityID string) (*string, domain.EntitlementContextStatus) {
	if r.entitlements == nil {
		return nil, domain.EntitlementUpstreamNotConfigured
	}
	ref, err := r.entitlements.ResolveEntitlementContext(ctx, tenantID, legalEntityID)
	if err != nil {
		r.log.Warn("entitlement context unavailable — recorded, not blocking",
			zap.String("tenant_id", tenantID), zap.Error(err))
		return nil, domain.EntitlementUpstreamUnavailable
	}
	return ref, domain.EntitlementResolved
}
