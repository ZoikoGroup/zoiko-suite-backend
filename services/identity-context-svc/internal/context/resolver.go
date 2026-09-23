// Package context contains the core identity resolution orchestrator.
package context

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
	"zoiko.io/identity-context-svc/internal/telemetry"
)

// Sentinel errors — mapped to HTTP status codes in handler.go.
var (
	// ErrTokenInvalid is returned when the inbound bearer/SAML token fails verification.
	ErrTokenInvalid = errors.New("token invalid or unverifiable")
	// ErrPrincipalInactive is returned when the principal does not exist or is not ACTIVE.
	ErrPrincipalInactive = errors.New("principal inactive or not found")
	// ErrTenantInactive is returned when the tenant's lifecycle_state is not ACTIVE.
	ErrTenantInactive = errors.New("tenant inactive")
	// ErrEntityUnauthorized is returned when the principal is not authorized for the requested entity.
	ErrEntityUnauthorized = errors.New("principal not authorized for the requested legal entity")
	// ErrTrustPostureBlocked is returned when trust posture evaluates to BLOCKED.
	ErrTrustPostureBlocked = errors.New("session blocked by trust posture policy")
	// ErrUpstreamUnavailable is returned when any upstream Tier 0 service cannot be reached.
	// Callers receive HTTP 503 — fail closed, never fail silent.
	ErrUpstreamUnavailable = errors.New("upstream dependency unavailable")
	// ErrNoToken is returned when neither bearer_token nor saml_assertion is provided.
	ErrNoToken = errors.New("exactly one of bearer_token or saml_assertion must be provided")

	// ErrSAMLUnsupported is returned for a saml_assertion, which this service
	// does not process. No SAML identity provider is configured anywhere in the
	// estate and no assertion can be validated against one that does not exist.
	//
	// Distinct from ErrTokenInvalid, and a 400 rather than a 401, because the
	// caller's credential is not the problem: no assertion they could supply
	// would work. Reporting it as "token invalid" invites a retry that cannot
	// succeed, and buries an unimplemented feature as a routine auth failure.
	ErrSAMLUnsupported = errors.New("saml_assertion is not supported: no SAML identity provider is configured")
)

// ResolveResult is what a successful resolution produced.
//
// Resolve used to return a bare JWT string. The spec's envelope section
// requires an evidence_id on every material governance decision, and issuing a
// signed identity envelope is the most material decision this service makes —
// so the caller now gets the decision's evidence alongside the credential it
// granted.
type ResolveResult struct {
	EnvelopeJWT      string
	EvidenceID       string
	SessionContextID string
	ExpiresAt        time.Time
}

// Resolver orchestrates the six-dimension identity context resolution.
//
// HOT PATH RULES:
//  1. Resolve() targets P99 < 50ms end-to-end.
//  2. Risk score is read from RiskSignalCache ONLY — no live call to
//     Intelligence Plane or any Tier 2/3 service (Q3 resolution).
//  3. No downstream service may infer identity context independently
//     (03-microservices.md §09.1 critical constraint).
//  4. Partial envelopes are PROHIBITED. All six dimensions must resolve or
//     the service fails closed. Never return a zero-value envelope.
type Resolver struct {
	cfg *config.Config
	log *zap.Logger
	// wg tracks all in-flight fire-and-forget event publish goroutines.
	// Drain() blocks until every goroutine completes, allowing main.go to
	// call it after srv.Shutdown() for a clean graceful shutdown.
	// Gap 2 follow-up: add a context-aware bounded drain (see linked issue)
	// so the process is not fully dependent on orchestrator SIGKILL if a
	// goroutine truly hangs.
	wg          sync.WaitGroup
	principals  PrincipalStore
	sessions    SessionCache
	riskSignals RiskSignalCache
	upstream    UpstreamRegistry
	events      EventPublisher
	verifier    TokenVerifier
	signer      EnvelopeSigner
	siem        *siem.Client

	// ingress enforces the ingress-to-tenant binding (negative path #2).
	// Optional: a nil checker skips the check, which is what the resolver's
	// own tests want and what a deployment with no bindings table gets.
	ingress *IngressChecker

	// residency refuses a resolution whose entity is not servable from this
	// region. Also optional, for the same reason.
	residency *ResidencyPolicy

	// support verifies a client-asserted X-Support-Context-Id before the
	// resolution is attributed to it.
	//
	// Unlike ingress and residency, a nil verifier does NOT mean "skip". Those
	// two are nil when a deployment has no bindings table or no region to
	// check against — there is genuinely nothing to verify. This one is nil
	// when the support command family was never wired, and a caller asserting
	// an elevation that this process cannot check is refused, not quietly
	// admitted unelevated. Getting that backwards is how the original defect
	// would reappear.
	support SupportContextVerifier

	// retention decides disposition_due_at for the session evidence row.
	retention time.Duration

	// metrics is optional. A nil one means the resolver is running in a test
	// or a deployment without Prometheus, and every increment below is guarded
	// rather than the instrument being a required constructor argument.
	metrics *telemetry.GovMetrics
}

// WithMetrics attaches the GOV-01 instruments.
func (r *Resolver) WithMetrics(m *telemetry.GovMetrics) *Resolver {
	r.metrics = m
	return r
}

// WithSupportVerifier wires support-context verification into resolution.
//
// Wired from cmd/server after SupportService is built, rather than taken by
// NewResolver, because SupportService depends on the publisher and SoD checker
// that are constructed later than the resolver.
func (r *Resolver) WithSupportVerifier(v SupportContextVerifier) *Resolver {
	r.support = v
	return r
}

// countFailure records a refusal by reason.
//
// The reason label is the point. An ingress/tenant mismatch and an expired
// token are both 401s in the HTTP metrics and could not be less alike
// operationally: one is somebody's session timing out, the other is a tenant
// boundary being crossed.
func (r *Resolver) countFailure(reason string) {
	if r.metrics != nil {
		r.metrics.ResolutionFailed.WithLabelValues(reason).Inc()
	}
}

// WithIngressChecker attaches the ingress-to-tenant binding check.
//
// Functional options rather than more constructor parameters: NewResolver
// already takes ten, and the three things added here are all independently
// optional — a test wants none of them, the local stack wants one, production
// wants all three.
func (r *Resolver) WithIngressChecker(c *IngressChecker) *Resolver {
	r.ingress = c
	return r
}

// WithResidencyPolicy attaches data-residency enforcement.
func (r *Resolver) WithResidencyPolicy(p *ResidencyPolicy) *Resolver {
	r.residency = p
	return r
}

// WithRetention sets how long session evidence is kept before disposition.
// Zero leaves disposition_due_at NULL, which the sweep reads as "never due".
func (r *Resolver) WithRetention(d time.Duration) *Resolver {
	r.retention = d
	return r
}

// NewResolver constructs a Resolver with all required dependencies injected.
func NewResolver(
	cfg *config.Config,
	log *zap.Logger,
	principals PrincipalStore,
	sessions SessionCache,
	riskSignals RiskSignalCache,
	upstream UpstreamRegistry,
	events EventPublisher,
	verifier TokenVerifier,
	signer EnvelopeSigner,
	siemClient *siem.Client,
) *Resolver {
	return &Resolver{
		cfg:         cfg,
		log:         log,
		principals:  principals,
		sessions:    sessions,
		riskSignals: riskSignals,
		upstream:    upstream,
		events:      events,
		verifier:    verifier,
		signer:      signer,
		siem:        siemClient,
	}
}

// Drain waits for in-flight event publish goroutines, bounded by ctx.
//
// Call it after srv.Shutdown() returns so events are not lost mid-flight on
// SIGTERM. It returns ctx.Err() if the budget expires with goroutines still
// running — the caller logs that and continues shutting down.
//
// The bound matters: this was previously an unconditional wg.Wait(), so one
// goroutine blocked on an unreachable broker hung the process until the
// orchestrator's SIGKILL. A stop that never completes is indistinguishable
// from a crash in every dashboard that watches for clean termination.
func (r *Resolver) Drain(ctx context.Context) error {
	return waitCtx(ctx, &r.wg)
}

// waitCtx waits on wg until it completes or ctx is done.
func waitCtx(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Resolve assembles and signs the IdentityContextEnvelope from all six dimensions.
// Any single dimension failure causes a fail-closed rejection:
//   - Dimension failures (invalid token, inactive principal/tenant/entity) → ErrXxx (→ 401)
//   - Infrastructure failures (upstream unreachable)                        → ErrUpstreamUnavailable (→ 503)
func (r *Resolver) Resolve(ctx context.Context, req domain.ResolveRequest) (*ResolveResult, error) {
	// Validate mutual exclusivity of token inputs
	if req.BearerToken == "" && req.SAMLAssertion == "" {
		return nil, ErrNoToken
	}
	if req.BearerToken != "" && req.SAMLAssertion != "" {
		return nil, ErrNoToken
	}

	// ── Dimension 1: Verify inbound token → authenticated principal ─────────
	claims, err := r.verifyToken(ctx, req)
	if errors.Is(err, ErrSAMLUnsupported) {
		// Not wrapped in ErrTokenInvalid: the assertion was never assessed, so
		// calling it invalid would state a verification result that never
		// happened.
		return nil, err
	}
	if err != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			ctx, cancel := detach(ctx)
			defer cancel()
			r.countFailure("token_invalid")
			if err := r.events.PublishResolutionFailed(ctx, "unknown", req.CorrelationID, "token_invalid"); err != nil {
				r.log.Error("event publish failed",
					zap.String("event_type", "identity.context.resolution_failed"),
					zap.String("subject", "unknown"),
					zap.Error(err),
				)
			}
		}()
		return nil, fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}

	// ── Ingress binding (negative paths #1 and #2) ──────────────────────────
	//
	// Runs here, before anything is looked up, because the claim it checks is
	// the tenant — and every query below is scoped by that tenant. Checking it
	// afterwards would mean the cross-tenant read had already happened.
	//
	// The check can only ever REFUSE. It never supplies a tenant, so a forged
	// host header cannot select one; what it can do is catch a token being
	// presented on a hostname bound to somebody else.
	// ingressDecision carries the binding's source_version through to the
	// evidence row — §4's "policy/version references". Declared outside the
	// block so a deployment with no ingress checker simply records nothing
	// rather than the resolution having to branch later.
	var ingressDecision IngressDecision
	if r.ingress != nil {
		decision, err := r.ingress.Evaluate(ctx, req.IngressSource, claims.TenantID)
		ingressDecision = decision
		if err != nil {
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				ctx, cancel := detach(ctx)
				defer cancel()
				r.countFailure("ingress_tenant_mismatch")
				if err := r.events.PublishResolutionFailed(ctx, claims.Subject, req.CorrelationID, "ingress_tenant_mismatch"); err != nil {
					r.log.Error("event publish failed",
						zap.String("event_type", "identity.context.resolution_failed"),
						zap.String("subject", claims.Subject),
						zap.Error(err))
				}
				// An ingress mismatch is a cross-tenant attempt, which is the
				// highest-signal event this service can produce. Streamed at
				// CRITICAL rather than HIGH: a blocked trust posture is one
				// user having a bad day, this is a boundary being probed.
				r.siem.Stream(ctx, claims.TenantID, "identity.ingress_tenant_mismatch",
					siem.SeverityCritical,
					fmt.Sprintf("Request on ingress %q presented a token claiming tenant %s",
						req.IngressSource, claims.TenantID))
			}()
			return nil, err
		}
	}

	principal, err := r.principals.FindByIDPSubject(ctx, claims.Subject, claims.TenantID)
	if err != nil || principal == nil || principal.Status != domain.PrincipalStatusActive {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			ctx, cancel := detach(ctx)
			defer cancel()
			r.countFailure("principal_inactive_or_not_found")
			if err := r.events.PublishResolutionFailed(ctx, claims.Subject, req.CorrelationID, "principal_inactive_or_not_found"); err != nil {
				r.log.Error("event publish failed",
					zap.String("event_type", "identity.context.resolution_failed"),
					zap.String("subject", claims.Subject),
					zap.Error(err),
				)
			}
		}()
		return nil, ErrPrincipalInactive
	}

	// ── Dimension 2: Tenant validation ──────────────────────────────────────
	if err := r.validateTenant(ctx, principal.TenantID, req.CorrelationID); err != nil {
		return nil, err
	}

	// ── Support elevation, if one is asserted ───────────────────────────────
	//
	// Placed here for two reasons. It is the first point at which the tenant
	// and principal a grant must be checked against are both trusted; and a
	// refused elevation should not first cost an entity-scope lookup, three
	// upstream calls and a signature.
	if err := r.verifySupportContext(ctx, principal, req); err != nil {
		return nil, err
	}

	// ── Dimension 3: Legal entity scope validation ──────────────────────────
	entityScope, err := r.validateEntityScope(ctx, principal.PrincipalID, principal.TenantID, req.LegalEntityID, req.CorrelationID)
	if err != nil {
		return nil, err
	}

	// ── Residency enforcement ───────────────────────────────────────────────
	//
	// The entity's residency policy has been RECORDED on every session since
	// migration 000005, and until now nothing compared it against where this
	// process actually runs. A recorded-but-unenforced residency policy
	// produces a flawless audit trail of PII being served from the wrong
	// region, which is worse than no trail at all: it is evidence against you.
	if r.residency != nil {
		if err := r.residency.Check(entityScope.DataResidencyPolicyID, req.LegalEntityID); err != nil {
			r.wg.Add(1)
			go func() {
				defer r.wg.Done()
				ctx, cancel := detach(ctx)
				defer cancel()
				r.countFailure("residency_denied")
				if err := r.events.PublishResolutionFailed(ctx, principal.PrincipalID, req.CorrelationID, "residency_denied"); err != nil {
					r.log.Error("event publish failed",
						zap.String("event_type", "identity.context.resolution_failed"),
						zap.String("principal_id", principal.PrincipalID),
						zap.Error(err))
				}
				r.siem.Stream(ctx, principal.TenantID, "identity.residency_denied",
					siem.SeverityHigh,
					fmt.Sprintf("Entity %s residency policy %s is not servable from region %s",
						req.LegalEntityID, entityScope.DataResidencyPolicyID, r.residency.Region))
			}()
			return nil, err
		}
	}

	// ── Dimension 4: Role profile ───────────────────────────────────────────
	roleAssignments, err := r.principals.FindActiveRoleAssignments(ctx, principal.PrincipalID, principal.TenantID, &req.LegalEntityID)
	if err != nil {
		return nil, fmt.Errorf("%w: role assignments: %v", ErrUpstreamUnavailable, err)
	}
	roleIDs := make([]string, len(roleAssignments))
	for i, ra := range roleAssignments {
		roleIDs[i] = ra.RoleID
	}
	permBundleIDs, err := r.upstream.ResolvePermissionBundles(ctx, principal.TenantID, roleIDs)
	if err != nil {
		return nil, fmt.Errorf("%w: permission bundles: %v", ErrUpstreamUnavailable, err)
	}

	// ── Dimension 5: Delegated authority ────────────────────────────────────
	// Read from this service's own store, not an upstream. delegated_authorities
	// is owned here, and asking a stubbed "upstream" for it meant every envelope
	// carried an empty delegation list that looked authoritative.
	delegations, err := r.principals.FindActiveDelegations(ctx, principal.PrincipalID, principal.TenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: delegated authority: %v", ErrUpstreamUnavailable, err)
	}

	// ── Dimension 6: Session trust posture ──────────────────────────────────
	// Risk score is read from async cache ONLY — no live Intelligence Plane call (Q3).
	posture, riskScore, riskSource, err := r.resolveTrustPosture(ctx, principal.PrincipalID, claims.MFADone, req.CorrelationID)
	if err != nil {
		return nil, err
	}
	if posture == domain.TrustPostureBlocked {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			ctx, cancel := detach(ctx)
			defer cancel()
			r.countFailure("trust_posture_blocked")
			if err := r.events.PublishResolutionFailed(ctx, principal.PrincipalID, req.CorrelationID, "trust_posture_blocked"); err != nil {
				r.log.Error("event publish failed",
					zap.String("event_type", "identity.context.resolution_failed"),
					zap.String("principal_id", principal.PrincipalID),
					zap.Error(err),
				)
			}
			// Doc 05 §13.2 names "MFA/step-up events" as a required SIEM
			// signal. A blocked trust posture (which correlates strongly
			// with a missing or stale MFA attestation — see
			// resolveTrustPosture below) is the actionable case; every
			// successful resolution is not streamed, for the same
			// signal-vs-noise reason authorization-svc only streams DENIED.
			r.siem.Stream(ctx, principal.TenantID, "session.trust_posture_blocked",
				siem.SeverityHigh, fmt.Sprintf("Trust posture BLOCKED for principal %s (MFA verified: %t)", principal.PrincipalID, claims.MFADone))
		}()
		return nil, ErrTrustPostureBlocked
	}

	// ── Assemble signed IdentityContextEnvelope (Q2 — signed short-lived JWT) ──
	sessionContextID := ulid.Make().String()
	jti := ulid.Make().String()
	// The evidence object id for this decision. Returned to the caller so it
	// can cite the decision that granted its envelope, per the spec's envelope
	// section ("evidence_id returned for material governance decision").
	evidenceID := "ev-" + ulid.Make().String()
	now := time.Now().UTC()
	exp := now.Add(time.Duration(r.cfg.EnvelopeJWTTTLSeconds) * time.Second)

	roleClaims := make([]domain.RoleAssignmentClaim, len(roleAssignments))
	for i, ra := range roleAssignments {
		roleClaims[i] = domain.RoleAssignmentClaim{
			RoleID:        ra.RoleID,
			LegalEntityID: ra.LegalEntityID,
		}
	}

	// FindActiveDelegations returns everything the principal holds across the
	// tenant, so the session's own entity scope is applied here. A NULL
	// legal_entity_id is tenant-wide and travels with every session; one naming
	// a different entity must not, or an envelope issued for entity A would
	// carry an authority that only exists on entity B.
	activeDelegations := make([]domain.DelegatedAuthorityClaim, 0, len(delegations))
	for _, d := range delegations {
		if d.LegalEntityID != nil && *d.LegalEntityID != req.LegalEntityID {
			continue
		}
		if d.RevocationStatus == domain.RevocationStatusActive {
			activeDelegations = append(activeDelegations, domain.DelegatedAuthorityClaim{
				DelegatedAuthorityID: d.DelegatedAuthorityID,
				DelegatorPrincipalID: d.DelegatorPrincipalID,
				ScopeType:            d.ScopeType,
				LegalEntityID:        d.LegalEntityID,
				AuthorityLimitType:   d.AuthorityLimitType,
				AuthorityLimitValue:  d.AuthorityLimitValue,
			})
		}
	}

	envelope := &domain.IdentityContextEnvelope{
		JTI: jti,
		ISS: r.cfg.JWTIssuer,
		AUD: r.cfg.JWTAudienceInternal,
		IAT: now.Unix(),
		EXP: exp.Unix(),
		Principal: domain.PrincipalClaims{
			PrincipalID:   principal.PrincipalID,
			TenantID:      principal.TenantID,
			PrincipalType: principal.PrincipalType,
			DisplayName:   principal.DisplayName,
		},
		TenantID:      principal.TenantID,
		LegalEntityID: req.LegalEntityID,
		RoleProfile: domain.RoleProfileClaims{
			RoleAssignments:     roleClaims,
			PermissionBundleIDs: permBundleIDs,
		},
		DelegatedAuthority: activeDelegations,
		SessionTrustPosture: domain.SessionTrustClaims{
			Posture:           posture,
			MFAVerified:       claims.MFADone,
			AdaptiveRiskScore: riskScore,
			SessionContextID:  sessionContextID,
		},
		// Verified by verifySupportContext above — an unverified assertion
		// never reaches here, because it returns before this point. Carrying
		// it on the envelope is what lets a downstream service see that it is
		// serving support traffic; recording it only in session_contexts left
		// that fact inside this service's database.
		SupportContextID: req.SupportContextID,

		CorrelationID: req.CorrelationID,
		SchemaVersion: "1.0",
	}

	signedJWT, err := r.signer.Sign(envelope)
	if err != nil {
		return nil, fmt.Errorf("envelope signing failed: %w", err)
	}

	// ── Persist SessionContext (append-only evidence obligation) ─────────────
	sc := domain.SessionContext{
		SessionContextID: sessionContextID,
		PrincipalID:      principal.PrincipalID,
		TenantID:         principal.TenantID,
		LegalEntityID:    req.LegalEntityID,
		CorrelationID:    req.CorrelationID,
		TrustPosture:     posture,
		MFAVerified:      claims.MFADone,
		// nil, not 0. No device fingerprint reaches this service, and 0 is the
		// worst score rather than the absence of one — see DeviceTrustScore.
		DeviceTrustScore:   nil,
		AdaptiveRiskScore:  riskScore,
		RiskSignalSource:   riskSource,
		EnvelopeJWTJTI:     jti,
		IssuedAt:           now,
		ExpiresAt:          exp,
		InvalidatedAt:      nil,
		InvalidationReason: nil,
		// Resolved in Dimension 3 from the entity the session is scoped to. The
		// registry treats it as mandatory on every LegalEntity, and this record
		// is the evidence of which policy governed the session's PII.
		DataResidencyPolicyID: entityScope.DataResidencyPolicyID,
		SourceService:         "identity-context-svc",
		SchemaVersion:         "1.0",

		// TenantContextDecision fields (spec section 18).
		IngressSource:    ingressOrUnknown(req.IngressSource),
		Environment:      environmentOrLocal(req.Environment),
		EvidenceID:       evidenceID,
		SupportContextID: req.SupportContextID,
		RetentionClass:   domain.RetentionClassSessionEvidence,
		DispositionDueAt: r.dispositionDue(now),

		// §4 server-resolved context and required source inputs. These were
		// parsed off the canonical envelope by the middleware and then
		// discarded, so a decision could not afterwards be explained by the
		// channel it arrived on or the workload that made it.
		SourceChannel: req.SourceChannel,
		WorkloadID:    req.WorkloadID,
		CausationID:   req.CausationID,

		// §4 evidence/lineage: "policy/version references". Which revision of
		// the routing truth this decision was made against — the difference
		// between a decision that can be replayed and one that can be
		// reproduced.
		IngressBindingVersion: ingressDecision.SourceVersion,

		// EntitlementContextRef stays nil: §4 names it, and no service in the
		// estate resolves one. Left explicitly unset rather than filled with a
		// placeholder — see the field's own comment.
		EntitlementContextRef: nil,
	}

	// ── Evidence, atomically ────────────────────────────────────────────────
	//
	// The session row and its identity.context.resolved event are now written
	// in ONE transaction, and a failure FAILS THE RESOLUTION.
	//
	// That is a deliberate change of stance. This used to log-and-swallow, on
	// the reasoning that a resolution which succeeded on all six dimensions
	// should not be failed by an evidence-store hiccup. The reasoning was
	// sound when the evidence was a best-effort Redis write and the event went
	// to Kafka on a separate path — losing one did not lose the other.
	//
	// It is not sound now. Both halves live in the same Postgres transaction,
	// so a failure here means NO evidence exists anywhere: no session record,
	// no event, nothing for an audit to find. Invariant 9 requires material
	// decisions to retain evidence, and issuing a signed platform credential
	// with no record that it was issued is precisely the outcome the invariant
	// forbids. Postgres is already a hard dependency of this path — the
	// principal lookup three dimensions ago would have failed without it — so
	// this adds no new failure mode, it only stops one being hidden.
	if err := r.sessions.PersistSessionContextWithEvent(ctx, sc, domain.ContextResolvedEvent{
		PrincipalID:      principal.PrincipalID,
		TenantID:         principal.TenantID,
		LegalEntityID:    req.LegalEntityID,
		SessionContextID: sessionContextID,
		EvidenceID:       evidenceID,
		CorrelationID:    req.CorrelationID,
	}); err != nil {
		r.log.Error("session evidence could not be recorded — refusing to issue the envelope",
			zap.String("session_context_id", sessionContextID),
			zap.String("principal_id", principal.PrincipalID),
			zap.String("correlation_id", req.CorrelationID),
			zap.Error(err))
		return nil, fmt.Errorf("%w: session evidence: %v", ErrUpstreamUnavailable, err)
	}

	// Cache the signed JWT for P99 < 5ms re-validation. This one IS
	// best-effort: the envelope is self-contained and independently verifiable
	// from the JWKS, so a cache miss costs a re-resolve, not a lost session.
	if err := r.sessions.Put(ctx, sessionContextID, signedJWT); err != nil {
		r.log.Error("failed to cache envelope JWT", zap.String("session_context_id", sessionContextID), zap.Error(err))
	}

	r.log.Info("identity.context.resolved",
		zap.String("principal_id", principal.PrincipalID),
		zap.String("session_context_id", sessionContextID),
		zap.String("evidence_id", evidenceID),
		zap.String("trust_posture", string(posture)),
		zap.String("ingress_source", sc.IngressSource),
		zap.String("correlation_id", req.CorrelationID),
	)

	return &ResolveResult{
		EnvelopeJWT:      signedJWT,
		EvidenceID:       evidenceID,
		SessionContextID: sessionContextID,
		ExpiresAt:        exp,
	}, nil
}

// dispositionDue returns when this session's evidence becomes disposable, or
// nil when retention is unconfigured.
//
// A nil due date reads as "never due" to the sweep, which is the safe default:
// a misconfigured retention period should keep evidence too long, never delete
// it early. There is no recovering from the second.
func (r *Resolver) dispositionDue(issuedAt time.Time) *time.Time {
	if r.retention <= 0 {
		return nil
	}
	due := issuedAt.Add(r.retention)
	return &due
}

func ingressOrUnknown(v string) string {
	if v == "" {
		return domain.IngressUnknown
	}
	return v
}

func environmentOrLocal(e domain.Environment) domain.Environment {
	if !e.Valid() {
		return domain.EnvironmentLocal
	}
	return e
}

// InvalidateSession appends invalidated_at to the SessionContext record and
// evicts the JWT from cache. Fully idempotent — re-invalidating an
// already-invalidated session is a no-op.
//
// tenantID is the caller's verified scope. A session belonging to another
// tenant reads back as absent, so this is a no-op rather than a cross-tenant
// revocation.
func (r *Resolver) InvalidateSession(
	ctx context.Context,
	sessionContextID string,
	tenantID string,
	reason domain.InvalidationReason,
	actorPrincipalID string,
	correlationID string,
) error {
	existing, err := r.sessions.GetSessionContext(ctx, sessionContextID, tenantID)
	if err != nil || existing == nil {
		// Not found — idempotent no-op
		return nil
	}
	if existing.InvalidatedAt != nil {
		// Already invalidated — status-idempotent no-op
		r.log.Debug("session already invalidated — no-op",
			zap.String("session_context_id", sessionContextID),
		)
		return nil
	}

	now := time.Now().UTC()
	if err := r.sessions.Invalidate(ctx, sessionContextID, tenantID, reason, now); err != nil {
		return fmt.Errorf("invalidate session context: %w", err)
	}
	if err := r.sessions.Evict(ctx, sessionContextID); err != nil {
		r.log.Warn("failed to evict session JWT from cache", zap.String("session_context_id", sessionContextID), zap.Error(err))
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := detach(ctx)
		defer cancel()
		if err := r.events.PublishSessionInvalidated(ctx, sessionContextID, existing.PrincipalID, reason, correlationID); err != nil {
			r.log.Error("event publish failed",
				zap.String("event_type", "session.invalidated"),
				zap.String("session_context_id", sessionContextID),
				zap.Error(err),
			)
		}
	}()

	r.log.Info("session.invalidated",
		zap.String("session_context_id", sessionContextID),
		zap.String("reason", string(reason)),
		zap.String("actor", actorPrincipalID),
	)
	return nil
}

// ── Private helpers ──────────────────────────────────────────────────────────

func (r *Resolver) verifyToken(ctx context.Context, req domain.ResolveRequest) (*domain.VerifiedClaims, error) {
	if req.BearerToken != "" {
		return r.verifier.VerifyBearer(ctx, req.BearerToken)
	}
	// Refused at the edge rather than half-attempted. Implementing this needs a
	// chosen IdP, its metadata and signing certificates, plus xmlsec1 or an
	// equivalent — none of which exist here, so there is nothing to validate
	// against and no way to test an implementation that claimed to.
	return nil, ErrSAMLUnsupported
}

// verifySupportContext refuses a resolution whose asserted support grant is
// not live, not this caller's, or not there at all.
//
// X-Support-Context-Id arrives from the client and, until this check existed,
// the resolver stamped it onto the session evidence unread. An expired,
// revoked, foreign-tenant or entirely fictional id became the grant the
// session was recorded against — the same self-reported-actor defect that
// requirePrincipal already refuses for X-Actor-Principal-ID one file away.
// SupportService.Verify had been written and was called by nothing on the
// request path, so the check existed and never ran.
//
// Refusal, not a silent downgrade. §4's negative-path column allows "deny
// /block/quarantine OR preserve UNKNOWN state"; deny is the reading taken,
// because a caller that names an elevation and receives an ordinary session
// believes it is acting under a grant that no evidence records — which is the
// condition NP3 exists to prevent rather than a safe fallback from it.
//
// subjectPrincipalID is empty here: the session being minted is the support
// operator's own, and which subject they may reach is decided per request by
// SupportContext.Covers, not at resolution time.
func (r *Resolver) verifySupportContext(ctx context.Context, principal *domain.Principal, req domain.ResolveRequest) error {
	if req.SupportContextID == nil {
		return nil
	}
	scID := *req.SupportContextID

	if r.support == nil {
		r.denySupport(ctx, principal, req, "support_context_unverifiable", scID)
		return domain.ErrSupportContextUnverifiable
	}

	// The tenant is the caller's own, verified in Dimension 1 from the token
	// and revalidated in Dimension 2 — never the header. That matches how
	// GetSupportContext and RevokeSupportContext scope their lookups, so a
	// grant is reachable from exactly one tenant throughout the service.
	if _, err := r.support.Verify(ctx, scID, principal.TenantID, principal.PrincipalID, ""); err != nil {
		switch {
		case errors.Is(err, domain.ErrSupportContextExpired):
			r.denySupport(ctx, principal, req, "support_context_expired", scID)
			return domain.ErrSupportContextExpired
		case errors.Is(err, domain.ErrSupportContextNotFound):
			// Absent, revoked, another principal's and another tenant's all
			// arrive here as one error, by Verify's own non-enumeration
			// design. The metric label keeps them together deliberately: a
			// caller must not be able to tell which of the four it hit.
			r.denySupport(ctx, principal, req, "support_context_not_found", scID)
			return domain.ErrSupportContextNotFound
		default:
			// A store failure is not a refusal of the caller. Still fail
			// closed, but say which kind of closed it is.
			return fmt.Errorf("%w: support context lookup: %v", ErrUpstreamUnavailable, err)
		}
	}
	return nil
}

// denySupport records a refused elevation: metric, resolution_failed event,
// SIEM stream.
//
// §4's negative-path column requires a stable error code AND evidence, so a
// rejected elevation has to leave a trail even though nothing was granted —
// an attempt to use a revoked break-glass grant is exactly the event a
// security team needs and is invisible if only the caller is told.
//
// Fire-and-forget on the resolver's WaitGroup, matching the residency and
// ingress denial paths, so a slow SIEM cannot add latency to a refusal.
func (r *Resolver) denySupport(ctx context.Context, principal *domain.Principal, req domain.ResolveRequest, reason, supportContextID string) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := detach(ctx)
		defer cancel()
		r.countFailure(reason)
		if err := r.events.PublishResolutionFailed(ctx, principal.PrincipalID, req.CorrelationID, reason); err != nil {
			r.log.Error("event publish failed",
				zap.String("event_type", "identity.context.resolution_failed"),
				zap.String("principal_id", principal.PrincipalID),
				zap.Error(err))
		}
		r.siem.Stream(ctx, principal.TenantID, "identity.support_context_rejected",
			siem.SeverityHigh,
			fmt.Sprintf("Principal %s asserted support context %s: %s",
				principal.PrincipalID, supportContextID, reason))
	}()
}

func (r *Resolver) validateTenant(ctx context.Context, tenantID, correlationID string) error {
	active, err := r.upstream.IsTenantActive(ctx, tenantID)
	if err != nil {
		r.log.Error("tenant registry unreachable — failing closed",
			zap.String("tenant_id", tenantID),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		return fmt.Errorf("%w: tenant registry: %v", ErrUpstreamUnavailable, err)
	}
	if !active {
		return fmt.Errorf("%w: tenant_id=%s", ErrTenantInactive, tenantID)
	}
	return nil
}

func (r *Resolver) validateEntityScope(ctx context.Context, principalID, tenantID, legalEntityID, correlationID string) (*domain.EntityScope, error) {
	scope, err := r.upstream.ResolveEntityScope(ctx, principalID, tenantID, legalEntityID)
	if err != nil {
		r.log.Error("entity registry unreachable — failing closed",
			zap.String("principal_id", principalID),
			zap.String("legal_entity_id", legalEntityID),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		return nil, fmt.Errorf("%w: entity registry: %v", ErrUpstreamUnavailable, err)
	}
	if scope == nil || !scope.Authorized {
		return nil, fmt.Errorf("%w: principal=%s entity=%s", ErrEntityUnauthorized, principalID, legalEntityID)
	}
	return scope, nil
}

// resolveTrustPosture reads from the async risk-signal cache (Q3).
// If the cache is unavailable, defaults to STANDARD posture and emits
// session.risk.changed with signal_source: UNAVAILABLE.
// Never falsely elevates trust on cache miss.
func (r *Resolver) resolveTrustPosture(
	ctx context.Context,
	principalID string,
	mfaVerified bool,
	correlationID string,
) (domain.TrustPosture, int, string, error) {
	signal, err := r.riskSignals.GetLatestSignal(ctx, principalID)

	riskScore := 0
	riskSource := "UNAVAILABLE"

	if err != nil || signal == nil {
		// Cache unavailable or empty — default STANDARD, never elevate (Q3)
		r.log.Warn("risk signal cache unavailable — defaulting to STANDARD posture",
			zap.String("principal_id", principalID),
			zap.String("correlation_id", correlationID),
		)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			ctx, cancel := detach(ctx)
			defer cancel()
			if r.metrics != nil {
				r.metrics.RiskSignalUnavailable.Inc()
			}
			if err := r.events.PublishRiskSignalUnavailable(ctx, principalID, correlationID); err != nil {
				r.log.Error("event publish failed",
					zap.String("event_type", "session.risk.changed"),
					zap.String("principal_id", principalID),
					zap.Error(err),
				)
			}
		}()
	} else {
		riskScore = signal.SignalValue
		riskSource = signal.SignalSource
	}

	var posture domain.TrustPosture
	switch {
	case riskScore >= 80:
		posture = domain.TrustPostureBlocked
	case riskScore >= 60:
		posture = domain.TrustPostureHighRisk
	case mfaVerified:
		posture = domain.TrustPostureMFAVerified
	default:
		posture = domain.TrustPostureStandard
	}

	// Clamp risk score to valid range
	riskScore = int(math.Min(float64(riskScore), 100))
	riskScore = int(math.Max(float64(riskScore), 0))

	return posture, riskScore, riskSource, nil
}

// detach returns a context that keeps ctx's values — trace span, correlation —
// but is immune to its cancellation.
//
// Every event publish below runs in a goroutine that outlives the HTTP handler
// that started it. Handing those the REQUEST context meant the response
// completing cancelled the publish mid-write: the observed failure was
// "kafka write: context canceled" on identity.authentication.succeeded, and the
// event simply never reached the topic.
//
// That is the evidence obligation failing silently. It is worse than a dropped
// log line, because the service reports the login as succeeded and no record of
// it exists anywhere downstream. A publish is given its own deadline instead, so
// it is bounded by how long a publish may reasonably take rather than by how
// quickly the client got its response.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), eventPublishTimeout)
}

// eventPublishTimeout bounds one fire-and-forget publish. Long enough for a
// broker round trip including a metadata refresh, short enough that Drain's
// budget is not consumed by one stuck write.
const eventPublishTimeout = 5 * time.Second
