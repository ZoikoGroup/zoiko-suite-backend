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
)

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
	cfg          *config.Config
	log          *zap.Logger
	// wg tracks all in-flight fire-and-forget event publish goroutines.
	// Drain() blocks until every goroutine completes, allowing main.go to
	// call it after srv.Shutdown() for a clean graceful shutdown.
	// Gap 2 follow-up: add a context-aware bounded drain (see linked issue)
	// so the process is not fully dependent on orchestrator SIGKILL if a
	// goroutine truly hangs.
	wg           sync.WaitGroup
	principals   PrincipalStore
	sessions     SessionCache
	riskSignals  RiskSignalCache
	upstream     UpstreamRegistry
	events       EventPublisher
	verifier     TokenVerifier
	signer       EnvelopeSigner
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
	}
}

// Drain blocks until all in-flight event publish goroutines have completed.
// Call this after srv.Shutdown() returns during graceful shutdown to avoid
// losing events mid-flight on SIGTERM.
//
// NOTE: Drain is unbounded — it blocks indefinitely if a goroutine hangs.
// A context-aware bounded drain with a configurable timeout is tracked as a
// follow-up (see linked GitHub issue) so the process is not fully dependent
// on the orchestrator's SIGKILL grace period in pathological cases.
func (r *Resolver) Drain() {
	r.wg.Wait()
}

// Resolve assembles and signs the IdentityContextEnvelope from all six dimensions.
// Any single dimension failure causes a fail-closed rejection:
//   - Dimension failures (invalid token, inactive principal/tenant/entity) → ErrXxx (→ 401)
//   - Infrastructure failures (upstream unreachable)                        → ErrUpstreamUnavailable (→ 503)
func (r *Resolver) Resolve(ctx context.Context, req domain.ResolveRequest) (string, error) {
	// Validate mutual exclusivity of token inputs
	if req.BearerToken == "" && req.SAMLAssertion == "" {
		return "", ErrNoToken
	}
	if req.BearerToken != "" && req.SAMLAssertion != "" {
		return "", ErrNoToken
	}

	// ── Dimension 1: Verify inbound token → authenticated principal ─────────
	claims, err := r.verifyToken(ctx, req)
	if err != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.events.PublishResolutionFailed(ctx, "unknown", req.CorrelationID, "token_invalid")
		}()
		return "", fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}

	principal, err := r.principals.FindByIDPSubject(ctx, claims.Subject, claims.TenantID)
	if err != nil || principal == nil || principal.Status != domain.PrincipalStatusActive {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.events.PublishResolutionFailed(ctx, claims.Subject, req.CorrelationID, "principal_inactive_or_not_found")
		}()
		return "", ErrPrincipalInactive
	}

	// ── Dimension 2: Tenant validation ──────────────────────────────────────
	if err := r.validateTenant(ctx, principal.TenantID, req.CorrelationID); err != nil {
		return "", err
	}

	// ── Dimension 3: Legal entity scope validation ──────────────────────────
	if err := r.validateEntityScope(ctx, principal.PrincipalID, req.LegalEntityID, req.CorrelationID); err != nil {
		return "", err
	}

	// ── Dimension 4: Role profile ───────────────────────────────────────────
	roleAssignments, err := r.principals.FindActiveRoleAssignments(ctx, principal.PrincipalID, &req.LegalEntityID)
	if err != nil {
		return "", fmt.Errorf("%w: role assignments: %v", ErrUpstreamUnavailable, err)
	}
	roleIDs := make([]string, len(roleAssignments))
	for i, ra := range roleAssignments {
		roleIDs[i] = ra.RoleID
	}
	permBundleIDs, err := r.upstream.ResolvePermissionBundles(ctx, roleIDs)
	if err != nil {
		return "", fmt.Errorf("%w: permission bundles: %v", ErrUpstreamUnavailable, err)
	}

	// ── Dimension 5: Delegated authority ────────────────────────────────────
	delegations, err := r.upstream.FetchActiveDelegations(ctx, principal.PrincipalID, req.LegalEntityID)
	if err != nil {
		return "", fmt.Errorf("%w: delegated authority: %v", ErrUpstreamUnavailable, err)
	}

	// ── Dimension 6: Session trust posture ──────────────────────────────────
	// Risk score is read from async cache ONLY — no live Intelligence Plane call (Q3).
	posture, riskScore, riskSource, err := r.resolveTrustPosture(ctx, principal.PrincipalID, claims.MFADone, req.CorrelationID)
	if err != nil {
		return "", err
	}
	if posture == domain.TrustPostureBlocked {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.events.PublishResolutionFailed(ctx, principal.PrincipalID, req.CorrelationID, "trust_posture_blocked")
		}()
		return "", ErrTrustPostureBlocked
	}

	// ── Assemble signed IdentityContextEnvelope (Q2 — signed short-lived JWT) ──
	sessionContextID := ulid.Make().String()
	jti := ulid.Make().String()
	now := time.Now().UTC()
	exp := now.Add(time.Duration(r.cfg.EnvelopeJWTTTLSeconds) * time.Second)

	roleClaims := make([]domain.RoleAssignmentClaim, len(roleAssignments))
	for i, ra := range roleAssignments {
		roleClaims[i] = domain.RoleAssignmentClaim{
			RoleID:        ra.RoleID,
			LegalEntityID: ra.LegalEntityID,
		}
	}

	activeDelegations := make([]domain.DelegatedAuthorityClaim, 0, len(delegations))
	for _, d := range delegations {
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
		CorrelationID: req.CorrelationID,
		SchemaVersion: "1.0",
	}

	signedJWT, err := r.signer.Sign(envelope)
	if err != nil {
		return "", fmt.Errorf("envelope signing failed: %w", err)
	}

	// ── Persist SessionContext (append-only evidence obligation) ─────────────
	sc := domain.SessionContext{
		SessionContextID:      sessionContextID,
		PrincipalID:           principal.PrincipalID,
		TenantID:              principal.TenantID,
		LegalEntityID:         req.LegalEntityID,
		CorrelationID:         req.CorrelationID,
		TrustPosture:          posture,
		MFAVerified:           claims.MFADone,
		DeviceTrustScore:      0, // TODO: derive from device fingerprint claim in IdP token
		AdaptiveRiskScore:     riskScore,
		RiskSignalSource:      riskSource,
		EnvelopeJWTJTI:        jti,
		IssuedAt:              now,
		ExpiresAt:             exp,
		InvalidatedAt:         nil,
		InvalidationReason:    nil,
		DataResidencyPolicyID: "", // TODO: resolve from Tenant & Entity Registry
		SourceService:         "identity-context-svc",
		SchemaVersion:         "1.0",
	}
	if err := r.sessions.PersistSessionContext(ctx, sc); err != nil {
		// Log but do not fail — outbox retry will eventually persist
		r.log.Error("failed to persist SessionContext", zap.String("session_context_id", sessionContextID), zap.Error(err))
	}

	// Cache the signed JWT for P99 < 5ms re-validation
	if err := r.sessions.Put(ctx, sessionContextID, signedJWT); err != nil {
		r.log.Error("failed to cache envelope JWT", zap.String("session_context_id", sessionContextID), zap.Error(err))
	}

	// Publish evidence event (non-blocking)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.events.PublishContextResolved(ctx, principal.PrincipalID, principal.TenantID, req.LegalEntityID, sessionContextID, req.CorrelationID)
	}()

	r.log.Info("identity.context.resolved",
		zap.String("principal_id", principal.PrincipalID),
		zap.String("session_context_id", sessionContextID),
		zap.String("trust_posture", string(posture)),
		zap.String("correlation_id", req.CorrelationID),
	)

	return signedJWT, nil
}

// InvalidateSession appends invalidated_at to the SessionContext record and
// evicts the JWT from Redis cache. Fully idempotent — re-invalidating an
// already-invalidated session is a no-op.
func (r *Resolver) InvalidateSession(
	ctx context.Context,
	sessionContextID string,
	reason domain.InvalidationReason,
	actorPrincipalID string,
	correlationID string,
) error {
	existing, err := r.sessions.GetSessionContext(ctx, sessionContextID)
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
	if err := r.sessions.Invalidate(ctx, sessionContextID, reason, now); err != nil {
		return fmt.Errorf("invalidate session context: %w", err)
	}
	if err := r.sessions.Evict(ctx, sessionContextID); err != nil {
		r.log.Warn("failed to evict session JWT from cache", zap.String("session_context_id", sessionContextID), zap.Error(err))
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.events.PublishSessionInvalidated(ctx, sessionContextID, existing.PrincipalID, reason, correlationID)
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
	// SAML: TODO implement xmlsec1-backed SAML 2.0 assertion validation
	return nil, errors.New("SAML assertion processing not yet implemented")
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

func (r *Resolver) validateEntityScope(ctx context.Context, principalID, legalEntityID, correlationID string) error {
	authorized, err := r.upstream.IsPrincipalAuthorizedForEntity(ctx, principalID, legalEntityID)
	if err != nil {
		r.log.Error("entity registry unreachable — failing closed",
			zap.String("principal_id", principalID),
			zap.String("legal_entity_id", legalEntityID),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		return fmt.Errorf("%w: entity registry: %v", ErrUpstreamUnavailable, err)
	}
	if !authorized {
		return fmt.Errorf("%w: principal=%s entity=%s", ErrEntityUnauthorized, principalID, legalEntityID)
	}
	return nil
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
			r.events.PublishRiskSignalUnavailable(ctx, principalID, correlationID)
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
