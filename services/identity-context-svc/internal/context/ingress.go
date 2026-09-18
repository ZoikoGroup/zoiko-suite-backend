package context

import (
	"context"
	"net"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// IngressBindingReader resolves a canonical ingress identifier to its tenant.
//
// A CACHE of a tenant-registry fact, never the master — see migration 000007.
// The asymmetry is load-bearing and is enforced in CheckIngressBinding below:
// a binding can CONTRADICT a token's tenant claim, but it can never SUPPLY a
// tenant the token did not claim.
type IngressBindingReader interface {
	FindIngressBinding(ctx context.Context, identifier string) (*domain.TenantIngressBinding, error)
}

// IngressPolicy decides how strict the ingress check is.
type IngressPolicy string

const (
	// IngressPolicyStrict refuses a request whose ingress identifier is not
	// bound to any tenant. Correct for production, where every hostname
	// serving the platform is provisioned and an unknown one is either a
	// misconfiguration or an attack.
	IngressPolicyStrict IngressPolicy = "strict"

	// IngressPolicyObserve records the ingress and enforces a MISMATCH, but
	// permits an unbound identifier. This is the default, and it is the
	// setting that lets the control be deployed before every environment has
	// its bindings seeded.
	//
	// The important half still runs: an identifier bound to tenant A carrying
	// a token for tenant B is refused under both policies. What observe mode
	// permits is the strictly weaker case of an identifier nobody has told us
	// about, which grants nothing on its own — the tenant still comes from the
	// verified token.
	IngressPolicyObserve IngressPolicy = "observe"
)

// CanonicalIngress extracts the ingress identifier a request arrived on.
//
// Order matters and is deliberate:
//
//  1. X-Canonical-Ingress, when the edge sets one. A platform that routes
//     through a gateway knows its own ingress name better than the Host header
//     that survived the hop.
//  2. X-Forwarded-Host, the first value. A proxy chain appends, so the FIRST
//     entry is the host the client actually asked for and the last is whatever
//     the nearest hop rewrote it to.
//  3. r.Host, for a direct connection.
//
// The port is stripped: api.example.com and api.example.com:443 are the same
// ingress, and a binding table that had to enumerate ports would miss on the
// first deployment that changed one.
//
// NOTE ON TRUST. None of these headers is trustworthy on its own, and this
// function does not pretend otherwise. What makes the check safe is that a
// forged value can only ever cause a REFUSAL — see CheckIngressBinding. An
// attacker who sets X-Forwarded-Host to a tenant they do not hold a token for
// gets a mismatch and is denied; one who sets it to the tenant they DO hold a
// token for changes nothing, because that is where the tenant came from
// anyway.
func CanonicalIngress(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Canonical-Ingress")); v != "" {
		return normalizeIngress(v)
	}
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		first := v
		if i := strings.IndexByte(v, ','); i >= 0 {
			first = v[:i]
		}
		if first = strings.TrimSpace(first); first != "" {
			return normalizeIngress(first)
		}
	}
	if h := strings.TrimSpace(r.Host); h != "" {
		return normalizeIngress(h)
	}
	return domain.IngressUnknown
}

// normalizeIngress lowercases and strips any port.
func normalizeIngress(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return domain.IngressUnknown
	}
	// SplitHostPort errors when there is no port, which is the common case, so
	// its failure is not an error condition here.
	if host, _, err := net.SplitHostPort(v); err == nil && host != "" {
		return host
	}
	return v
}

// IngressChecker enforces the ingress-to-tenant binding.
type IngressChecker struct {
	bindings IngressBindingReader
	policy   IngressPolicy
	log      *zap.Logger
}

func NewIngressChecker(bindings IngressBindingReader, policy IngressPolicy, log *zap.Logger) *IngressChecker {
	if policy != IngressPolicyStrict {
		policy = IngressPolicyObserve
	}
	return &IngressChecker{bindings: bindings, policy: policy, log: log}
}

// Check verifies that the ingress a request arrived on is consistent with the
// tenant its token claims.
//
// This is negative path #2 of GOV-01's minimum acceptance set — "unknown
// hostname cannot fall back to another tenant" — and negative path #1's second
// half. Three outcomes:
//
//	ingress unknown/absent   → permitted under observe, refused under strict.
//	                           Grants nothing either way: the tenant still
//	                           comes from the verified token.
//	ingress bound, matches   → permitted.
//	ingress bound, DIFFERENT → refused, always, under every policy. There is
//	                           no legitimate request that arrives on tenant
//	                           A's hostname bearing tenant B's token.
//
// The one thing this never does is TAKE the tenant from the binding. That
// would make a hostname sufficient to select a tenant, which is exactly the
// "no trusted client context" refinement the spec opens with.
func (c *IngressChecker) Check(ctx context.Context, ingress, claimedTenantID string) error {
	if ingress == "" || ingress == domain.IngressUnknown {
		if c.policy == IngressPolicyStrict {
			c.log.Warn("request presented no ingress identifier — refused under strict ingress policy",
				zap.String("tenant_id", claimedTenantID))
			return domain.ErrIngressTenantMismatch
		}
		return nil
	}

	binding, err := c.bindings.FindIngressBinding(ctx, ingress)
	if err != nil {
		// Fail closed. A binding table we cannot read is a control that did
		// not run, and this one exists to stop cross-tenant resolution.
		c.log.Error("ingress binding lookup failed — failing closed",
			zap.String("ingress", ingress), zap.Error(err))
		return ErrUpstreamUnavailable
	}

	if binding == nil {
		if c.policy == IngressPolicyStrict {
			c.log.Warn("unknown ingress identifier — refused, no fallback tenant",
				zap.String("ingress", ingress),
				zap.String("claimed_tenant_id", claimedTenantID))
			return domain.ErrIngressTenantMismatch
		}
		c.log.Debug("ingress identifier not bound — permitted under observe policy",
			zap.String("ingress", ingress))
		return nil
	}

	if binding.TenantID != claimedTenantID {
		// The severe case, and the one both policies refuse. Logged at ERROR
		// with both tenants named, because this is either a routing
		// misconfiguration serving one customer's hostname to another's
		// backend, or someone actively trying to cross the boundary. Neither
		// should be discovered from a metric.
		c.log.Error("INGRESS/TENANT MISMATCH — refusing resolution",
			zap.String("ingress", ingress),
			zap.String("ingress_bound_tenant", binding.TenantID),
			zap.String("token_claimed_tenant", claimedTenantID))
		return domain.ErrIngressTenantMismatch
	}
	return nil
}

// ── Residency ────────────────────────────────────────────────────────────────

// ResidencyPolicy decides which data residency policies this deployment may
// serve.
//
// data_residency_policy_id has been recorded on every SessionContext since
// migration 000005, but nothing ever COMPARED it against where the process is
// actually running. A residency policy that is recorded and not enforced
// documents a breach rather than preventing one — it produces a perfect audit
// trail of PII being served from the wrong region.
type ResidencyPolicy struct {
	// Region is this deployment's region identifier.
	Region string
	// Allowed is the set of residency policy ids servable here. Empty means
	// enforcement is OFF, which is the default and is correct for a local
	// stack where no residency policies exist to check against.
	Allowed map[string]struct{}
	log     *zap.Logger
}

// NewResidencyPolicy builds the check from a configured allow-list.
//
// An empty list disables enforcement rather than refusing everything. That is
// the right default for a control being introduced into a running estate:
// refusing every resolution until somebody populates a list would take the
// platform down, and a control that takes the platform down on deployment gets
// reverted rather than fixed.
func NewResidencyPolicy(region string, allowed []string, log *zap.Logger) *ResidencyPolicy {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		if a = strings.TrimSpace(a); a != "" {
			set[a] = struct{}{}
		}
	}
	if len(set) == 0 {
		log.Warn("data residency enforcement is DISABLED — no allowed policies configured",
			zap.String("region", region))
	}
	return &ResidencyPolicy{Region: region, Allowed: set, log: log}
}

// Enforced reports whether the check is active.
func (p *ResidencyPolicy) Enforced() bool { return len(p.Allowed) > 0 }

// Check refuses a resolution whose entity carries a residency policy this
// region may not serve.
//
// An entity with NO residency policy is permitted when enforcement is on. The
// registry treats the field as mandatory, so an empty one means the registry
// did not return it — a registry problem, not a residency violation, and
// refusing every login over it would be the wrong response to the wrong
// diagnosis. It is logged at WARN so it is visible.
func (p *ResidencyPolicy) Check(policyID, legalEntityID string) error {
	if !p.Enforced() {
		return nil
	}
	if policyID == "" {
		p.log.Warn("legal entity carries no data residency policy — permitted, but the registry should supply one",
			zap.String("legal_entity_id", legalEntityID),
			zap.String("region", p.Region))
		return nil
	}
	if _, ok := p.Allowed[policyID]; !ok {
		p.log.Error("RESIDENCY DENIED — entity's data residency policy is not servable from this region",
			zap.String("legal_entity_id", legalEntityID),
			zap.String("data_residency_policy_id", policyID),
			zap.String("region", p.Region))
		return domain.ErrResidencyDenied
	}
	return nil
}
