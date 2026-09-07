// Package handler implements the ForwardAuth endpoint Traefik calls before
// routing any gated request to a backend service.
package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"

	"zoiko.io/gateway-auth-svc/internal/carta"
	"zoiko.io/gateway-auth-svc/internal/config"
	"zoiko.io/gateway-auth-svc/internal/jwks"
	"zoiko.io/gateway-auth-svc/internal/siem"
	"zoiko.io/gateway-auth-svc/internal/tenantctx"
)

type Handler struct {
	cfg    *config.Config
	jwks   *jwks.Client
	carta  *carta.Client
	siem   *siem.Client
	tenant *tenantctx.Resolver
	log    *zap.Logger
}

// New builds the ForwardAuth handler. tenantResolver may be nil, which disables
// GOV-01 context resolution — see config.TenantRegistryURL.
func New(
	cfg *config.Config,
	jwksClient *jwks.Client,
	cartaClient *carta.Client,
	siemClient *siem.Client,
	tenantResolver *tenantctx.Resolver,
	log *zap.Logger,
) *Handler {
	return &Handler{
		cfg:    cfg,
		jwks:   jwksClient,
		carta:  cartaClient,
		siem:   siemClient,
		tenant: tenantResolver,
		log:    log,
	}
}

// envelopeClaims mirrors only the fields this gateway needs to propagate
// downstream. The full IdentityContextEnvelope shape is owned by
// identity-context-svc — this service reads a signed token, it never mints
// one.
type envelopeClaims struct {
	Principal struct {
		PrincipalID string `json:"principal_id"`
	} `json:"principal"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	CorrelationID string `json:"correlation_id"`
	jwt.RegisteredClaims
}

// Verify is called by Traefik's ForwardAuth middleware on every gated
// request. A 2xx response grants access and its headers are copied into the
// forwarded request (see authResponseHeaders in docker-compose.yml); any
// other status is returned to the client verbatim — fail-closed, the
// protected backend never sees an unverified request.
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	rawToken, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		h.deny(w, "missing bearer token")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	claims := &envelopeClaims{}
	_, err := jwt.ParseWithClaims(rawToken, claims, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, errors.New("unexpected signing method")
		}
		kid, ok := tok.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("token missing kid header")
		}
		return h.jwks.PublicKey(ctx, kid)
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(h.cfg.ExpectedIssuer),
		jwt.WithAudience(h.cfg.ExpectedAudience),
	)
	if err != nil {
		h.log.Info("gateway rejected request",
			zap.Error(err),
			zap.String("forwarded_uri", r.Header.Get("X-Forwarded-Uri")),
		)
		h.deny(w, "invalid token")
		return
	}

	if claims.Principal.PrincipalID == "" || claims.TenantID == "" {
		h.deny(w, "invalid token")
		return
	}

	// Token/hostname tenant mismatch defense — GTRM decision doc §6.2/§11
	// (acceptance test O), which explicitly assigns this check to the
	// application layer, not the router: "the app must reject a session
	// token whose tenant/workspace claim ≠ hostname-resolved tenant."
	//
	// X-Zoiko-Resolved-Tenant-Id is set by GTRM's per-tenant Traefik context
	// middleware AFTER stripping any externally-supplied copy of the same
	// header (see gtrm/compiler/emit.go's untrustedInboundHeaders) — a
	// client cannot forge it, only Traefik's own middleware chain can. When
	// present, it is the canonical tenant_id the caller's HOSTNAME resolved
	// to; if a validly-signed token nonetheless claims a different tenant,
	// this is exactly the spoofing shape doc §6.2 names (a token minted for
	// tenant A presented against tenant B's hostname) and must be rejected
	// even though the token itself verified correctly.
	//
	// The header is only present for requests GTRM's per-tenant routers have
	// resolved — today that's the GTRM Phase 1 proof slice, not yet every
	// backend service's router. Its absence is not itself suspicious: it
	// means this particular route hasn't been onboarded onto GTRM-resolved
	// routing yet, so no comparison is made (an honest, not a fabricated,
	// pass).
	if resolvedTenantID := r.Header.Get("X-Zoiko-Resolved-Tenant-Id"); resolvedTenantID != "" && resolvedTenantID != claims.TenantID {
		h.log.Warn("token/hostname tenant mismatch — rejecting",
			zap.String("token_tenant_id", claims.TenantID),
			zap.String("resolved_tenant_id", resolvedTenantID),
			zap.String("forwarded_uri", r.Header.Get("X-Forwarded-Uri")),
		)
		h.denyWithReason(w, "token tenant does not match the hostname-resolved tenant", "tenant_hostname_mismatch")
		return
	}

	// Continuous session-risk assessment (Doc 05 §3.11) — JWT verification
	// above only proves the token was validly issued; this asks whether THIS
	// request, right now, looks risky. A nil assessment means carta-svc is
	// unconfigured or unreachable: that degrades to "not scored," never to a
	// forced deny, since CARTA is an additive signal on top of the
	// already-fail-closed JWT check, not a replacement for it.
	assessment := h.carta.Evaluate(ctx,
		claims.Principal.PrincipalID, claims.TenantID, claims.LegalEntityID,
		clientIP(r), r.Method+" "+r.Header.Get("X-Forwarded-Uri"),
		// No resource-classification registry exists on this platform yet
		// (see docs/architecture/full-architecture-gap-analysis.md #16) —
		// MEDIUM is an explicit, honestly-labeled placeholder floor, not a
		// real per-resource sensitivity lookup.
		"MEDIUM",
	)

	if assessment != nil && assessment.Decision != carta.DecisionAllow {
		h.log.Warn("carta flagged this request",
			zap.String("principal_id", claims.Principal.PrincipalID),
			zap.String("decision", string(assessment.Decision)),
			zap.String("risk_level", assessment.RiskLevel),
			zap.Float64("trust_score", assessment.TrustScore),
			zap.Strings("risk_factors", assessment.RiskFactors),
		)
		h.siem.Stream(ctx, claims.TenantID, "session_risk."+strings.ToLower(string(assessment.Decision)),
			severityFor(assessment.Decision),
			"CARTA flagged principal "+claims.Principal.PrincipalID+": "+string(assessment.Decision))

		// ISOLATE/DENY are hard-blocked: they represent risk high enough
		// that this platform has no automated remediation for it. STEP_UP_MFA
		// is intentionally NOT blocked here — there is no step-up-MFA
		// challenge flow anywhere downstream to redirect to yet, and hard-
		// blocking with no path to recover would just be a silent lockout
		// wearing a different name. It is still logged and streamed to SIEM
		// above so the signal isn't lost, just not yet enforced.
		if assessment.Decision == carta.DecisionIsolate || assessment.Decision == carta.DecisionDeny {
			h.denyWithReason(w, "access denied by continuous risk assessment", string(assessment.Decision))
			return
		}
	}

	// GOV-01 (ZS-SVC-A-001 §4): resolve the authoritative tenant and operating
	// context before business processing begins. A valid token proves who is
	// asking; it does not prove the tenant may transact, nor that the entity
	// named in the token belongs to that tenant. The registry owns both facts.
	//
	// Skipped entirely when unconfigured — see config.TenantRegistryURL.
	resolved, err := h.tenant.Resolve(ctx,
		claims.TenantID, claims.LegalEntityID, materialWrite(originalMethod(r)))
	if err != nil {
		h.denyResolution(w, r, claims, err)
		return
	}

	w.Header().Set("X-Principal-Id", claims.Principal.PrincipalID)
	w.Header().Set("X-Tenant-Id", claims.TenantID)
	w.Header().Set("X-Legal-Entity-Id", claims.LegalEntityID)
	if claims.CorrelationID != "" {
		w.Header().Set("X-Correlation-Id", claims.CorrelationID)
	}

	// Server-resolved context, forwarded so backends read authoritative values
	// instead of the caller's assertions (§5 provenance class S: "client may
	// request context but cannot override result"). These names match the
	// envelope's own header constants, so a backend picks them up through the
	// parser it already runs — and Traefik overwrites whatever the client sent
	// under the same names, which is what makes the override real.
	//
	// Every header set here must also appear in the ForwardAuth middleware's
	// authResponseHeaders list, or Traefik drops it silently.
	if h.tenant.Enabled() {
		setIfPresent(w, "X-Jurisdiction-Context", resolved.JurisdictionContext)
		setIfPresent(w, "X-Timezone", resolved.Timezone)
		setIfPresent(w, "X-Residency-Policy-Id", resolved.ResidencyPolicyID)
		if resolved.Stale {
			// Named on the response so a backend can tell that its context came
			// from a cache the registry could not confirm. Only ever set on
			// non-material reads — writes never reach here on a stale context.
			w.Header().Set("X-Tenant-Context-Stale", "true")
		}
	}

	w.WriteHeader(http.StatusOK)
}

func setIfPresent(w http.ResponseWriter, header, value string) {
	if value != "" {
		w.Header().Set(header, value)
	}
}

// originalMethod returns the method of the request Traefik is authorising.
//
// ForwardAuth replays the client's method onto /verify, but also sets
// X-Forwarded-Method; the header is preferred because it survives any proxy in
// front that normalises the probe itself.
func originalMethod(r *http.Request) string {
	if m := r.Header.Get("X-Forwarded-Method"); m != "" {
		return m
	}
	return r.Method
}

// materialWrite mirrors the envelope contract's own definition, so the gateway
// and the backends agree on which requests are state-changing. PUT and DELETE
// are included: they are idempotent at the HTTP level, not at the business one.
func materialWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// denyResolution answers a failed GOV-01 resolution.
//
// The two outcomes are deliberately different statuses. ErrDenied is a decision
// the registry made — the tenant is unknown, suspended, or the entity is not
// its own — and re-authenticating will not change it, so 403. ErrUnavailable is
// the absence of a decision; answering 403 would tell the caller it had been
// refused when in fact nothing could be determined, so 503 with Retry-After.
func (h *Handler) denyResolution(w http.ResponseWriter, r *http.Request, claims *envelopeClaims, err error) {
	h.log.Warn("tenant context resolution failed",
		zap.Error(err),
		zap.String("principal_id", claims.Principal.PrincipalID),
		zap.String("tenant_id", claims.TenantID),
		zap.String("legal_entity_id", claims.LegalEntityID),
		zap.String("method", originalMethod(r)),
		zap.String("forwarded_uri", r.Header.Get("X-Forwarded-Uri")),
	)

	// Both branches name themselves on the response. Traefik returns an
	// unsuccessful ForwardAuth reply to the client verbatim — status, body and
	// headers — so this is what lets a console tell a GOV-01 refusal apart from
	// the backend service's own 403/503, which mean entirely different things
	// and have different fixes.
	if errors.Is(err, tenantctx.ErrUnavailable) {
		// Fail closed, and say why: no fallback to a global or default tenant,
		// and no proceeding on a context nobody could confirm.
		w.Header().Set("X-Tenant-Context", "unresolved")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("tenant context could not be resolved"))
		return
	}

	h.siem.Stream(r.Context(), claims.TenantID, "tenant_context.denied", siem.SeverityHigh,
		"tenant context denied for principal "+claims.Principal.PrincipalID)
	w.Header().Set("X-Tenant-Context", "denied")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("tenant context denied"))
}

func severityFor(d carta.Decision) siem.Severity {
	switch d {
	case carta.DecisionDeny:
		return siem.SeverityCritical
	case carta.DecisionIsolate:
		return siem.SeverityHigh
	case carta.DecisionStepUpMFA:
		return siem.SeverityMedium
	default:
		return siem.SeverityLow
	}
}

// clientIP prefers the value Traefik sets over the raw socket address, which
// would otherwise always be the gateway container's own address.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return r.RemoteAddr
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	tok := strings.TrimPrefix(header, prefix)
	if tok == "" {
		return "", false
	}
	return tok, true
}

func (h *Handler) deny(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(msg))
}

func (h *Handler) denyWithReason(w http.ResponseWriter, msg, reason string) {
	w.Header().Set("X-Carta-Decision", reason)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(msg))
}
