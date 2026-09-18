package context

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/siem"
)

// IngressBindingWriter is the refresh half of the routing-hint cache.
type IngressBindingWriter interface {
	TouchIngressBindings(ctx context.Context, tenantID string, identifiers []string) (int, error)
}

// TenantSessionRevoker revokes every live session in a tenant.
type TenantSessionRevoker interface {
	EvictAllForTenant(ctx context.Context, tenantID string, reason domain.InvalidationReason) (int, error)
}

// ContextCacheService implements GOV-01's two cache commands:
// RefreshTenantContextCache and InvalidateTenantContext.
//
// They are separate commands with separate authorization actions and very
// different blast radii, and conflating them would be a mistake:
//
//   - REFRESH drops routing hints so the next resolution re-reads them. It
//     revokes nothing and no user notices.
//   - INVALIDATE revokes every live session in a tenant. Every user of that
//     tenant is logged out immediately.
//
// A permission that granted both would mean anyone who could clear a cache
// could also log out a customer.
type ContextCacheService struct {
	bindings IngressBindingWriter
	sessions TenantSessionRevoker
	events   *events.Publisher
	siem     *siem.Client
	log      *zap.Logger
}

func NewContextCacheService(
	bindings IngressBindingWriter,
	sessions TenantSessionRevoker,
	publisher *events.Publisher,
	siemClient *siem.Client,
	log *zap.Logger,
) *ContextCacheService {
	return &ContextCacheService{
		bindings: bindings,
		sessions: sessions,
		events:   publisher,
		siem:     siemClient,
		log:      log,
	}
}

// Authorization actions for the two commands. Distinct on purpose — see the
// type's doc comment.
const (
	ActionRefreshContextCache     = "IDENTITY_CONTEXT_CACHE_REFRESH"
	ActionInvalidateTenantContext = "IDENTITY_CONTEXT_TENANT_INVALIDATE"
)

// Refresh marks a tenant's routing hints stale.
//
// It does NOT delete them. A deleted binding would make every request on that
// hostname fail closed until the registry sync ran again, which turns a cache
// refresh into an outage — and an operator reaching for "refresh the cache"
// during an incident is the last person who should be handed one.
func (s *ContextCacheService) Refresh(
	ctx context.Context,
	tenantID string,
	req domain.RefreshCacheRequest,
	actorPrincipalID string,
) (*domain.RefreshCacheResponse, error) {
	n, err := s.bindings.TouchIngressBindings(ctx, tenantID, req.IngressIdentifiers)
	if err != nil {
		return nil, fmt.Errorf("refresh tenant context cache: %w", err)
	}

	evidenceID := "ev-" + ulid.Make().String()

	// Announced so downstream holders of derived context caches — the
	// gateways, GOV-03 — drop theirs. A refresh that only cleared this
	// service's copy would leave the estate disagreeing with itself about a
	// tenant's routing for as long as the other caches live.
	if err := s.events.PublishTenantContextCacheInvalidated(
		ctx, tenantID, actorPrincipalID, req.Reason, req.CorrelationID, req.IngressIdentifiers,
	); err != nil {
		// The bindings ARE stale; only the announcement failed. Logged rather
		// than failed, because reporting an error here would invite a retry
		// that re-does work already done.
		s.log.Error("cache refreshed but the invalidation event could not be enqueued",
			zap.String("tenant_id", tenantID), zap.Error(err))
	}

	s.log.Info("tenant context cache refreshed",
		zap.String("tenant_id", tenantID),
		zap.Int("bindings_refreshed", n),
		zap.String("actor", actorPrincipalID),
		zap.String("reason", req.Reason),
		zap.String("evidence_id", evidenceID))

	return &domain.RefreshCacheResponse{BindingsRefreshed: n, EvidenceID: evidenceID}, nil
}

// InvalidateTenant revokes every live session in a tenant.
//
// The blast radius is every user of the tenant, so the justification is
// mandatory, the event carries it, and it is streamed to SIEM at CRITICAL. An
// operator who cannot say why in a sentence should not be running this.
func (s *ContextCacheService) InvalidateTenant(
	ctx context.Context,
	tenantID string,
	req domain.InvalidateTenantContextRequest,
	actorPrincipalID string,
) (*domain.InvalidateTenantContextResponse, error) {
	if req.Justification == "" {
		return nil, fmt.Errorf("%w: justification is required — this logs out every user in the tenant", ErrRequestInvalid)
	}
	reason := req.Reason
	if reason == "" {
		reason = domain.InvalidationReasonAdminRevoke
	}

	revoked, err := s.sessions.EvictAllForTenant(ctx, tenantID, reason)
	if err != nil {
		// A PARTIAL revocation is still reported, because the sessions that
		// were revoked stay revoked and a caller told only "error" would
		// reasonably assume none were.
		s.log.Error("tenant-wide invalidation incomplete",
			zap.String("tenant_id", tenantID),
			zap.Int("revoked_before_failure", revoked),
			zap.Error(err))
		return &domain.InvalidateTenantContextResponse{SessionsRevoked: revoked},
			fmt.Errorf("invalidate tenant context: %w", err)
	}

	evidenceID := "ev-" + ulid.Make().String()

	if err := s.events.PublishTenantContextInvalidated(
		ctx, tenantID, revoked, reason, req.Justification, actorPrincipalID, req.CorrelationID,
	); err != nil {
		s.log.Error("tenant invalidated but the event could not be enqueued",
			zap.String("tenant_id", tenantID), zap.Error(err))
	}

	s.siem.Stream(ctx, tenantID, "identity.context.tenant_invalidated",
		siem.SeverityCritical,
		fmt.Sprintf("All %d live sessions in tenant %s revoked by %s (%s): %s",
			revoked, tenantID, actorPrincipalID, reason, req.Justification))

	s.log.Warn("TENANT CONTEXT INVALIDATED",
		zap.String("tenant_id", tenantID),
		zap.Int("sessions_revoked", revoked),
		zap.String("actor", actorPrincipalID),
		zap.String("reason", string(reason)),
		zap.String("justification", req.Justification),
		zap.String("evidence_id", evidenceID))

	return &domain.InvalidateTenantContextResponse{
		SessionsRevoked: revoked,
		EvidenceID:      evidenceID,
	}, nil
}

// ── ExplainContextResolution ─────────────────────────────────────────────────

// Explain reconstructs the account of one resolution from the recorded
// decision.
//
// RECONSTRUCTED, NOT RE-DERIVED, and the distinction is the whole value of the
// endpoint. Re-deriving would read today's risk signals, role assignments and
// entity state against a session issued last week, and would confidently
// report a decision that was never made. Every field here comes from the
// frozen session_contexts row, which is append-only and therefore already an
// as-of record of itself.
//
// This is DoD gate 6 ("historical version/as-of reconstruction demonstrated")
// and the ExplainContextResolution query GOV-01's contract names.
func Explain(sc *domain.SessionContext, asOf time.Time) *domain.ContextExplanation {
	outcome := "RESOLVED"
	switch {
	case sc.DisposedAt != nil:
		// The skeleton of the decision survives disposition; the personal data
		// does not. Saying so is more honest than presenting redacted fields
		// as though they were the original values.
		outcome = "RESOLVED_EVIDENCE_DISPOSED"
	case sc.InvalidatedAt != nil && !asOf.Before(*sc.InvalidatedAt):
		outcome = "INVALIDATED"
	case !asOf.Before(sc.ExpiresAt):
		outcome = "EXPIRED"
	}

	ex := &domain.ContextExplanation{
		SessionContextID: sc.SessionContextID,
		DecisionID:       sc.SessionContextID,
		EvidenceID:       sc.EvidenceID,
		Outcome:          outcome,
		PrincipalID:      sc.PrincipalID,
		TenantID:         sc.TenantID,
		LegalEntityID:    sc.LegalEntityID,
		Environment:      sc.Environment,
		IngressSource:    sc.IngressSource,
		Dimensions: []domain.DimensionOutcome{
			{
				Dimension: 1, Name: "authenticated_principal",
				Result: sc.PrincipalID, Source: "verified_idp_token",
				Detail: "principal resolved from the token's subject claim within its tenant",
			},
			{
				Dimension: 2, Name: "tenant",
				Result: sc.TenantID, Source: "tenant_registry",
				Detail: "tenant lifecycle_state was ACTIVE at issue time",
			},
			{
				Dimension: 3, Name: "legal_entity_scope",
				Result: sc.LegalEntityID, Source: "entity_registry",
				Detail: "residency policy " + orNone(sc.DataResidencyPolicyID),
			},
			{
				Dimension: 4, Name: "role_profile",
				Result: "recorded_in_envelope", Source: "identity_store + access_control",
				Detail: "role assignments and permission bundles were frozen into envelope " + sc.EnvelopeJWTJTI,
			},
			{
				Dimension: 5, Name: "delegated_authority",
				Result: "recorded_in_envelope", Source: "identity_store",
				Detail: "delegations active at issue time, scoped to this entity",
			},
			{
				Dimension: 6, Name: "session_trust_posture",
				Result: string(sc.TrustPosture), Source: sc.RiskSignalSource,
				Detail: fmt.Sprintf("mfa_verified=%t adaptive_risk_score=%d", sc.MFAVerified, sc.AdaptiveRiskScore),
			},
		},
		IssuedAt:          sc.IssuedAt,
		ExpiresAt:         sc.ExpiresAt,
		InvalidatedAt:     sc.InvalidatedAt,
		SupportContextID:  sc.SupportContextID,
		AsOf:              asOf,
		ReconstructedFrom: "session_contexts (append-only evidence record)",
		CorrelationID:     sc.CorrelationID,
		SchemaVersion:     "1.0",
	}
	if sc.InvalidationReason != nil {
		reason := string(*sc.InvalidationReason)
		ex.InvalidationReason = &reason
	}
	return ex
}

func orNone(v string) string {
	if v == "" {
		return "(none recorded)"
	}
	return v
}
