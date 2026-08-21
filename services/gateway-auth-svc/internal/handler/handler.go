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
)

type Handler struct {
	cfg   *config.Config
	jwks  *jwks.Client
	carta *carta.Client
	siem  *siem.Client
	log   *zap.Logger
}

func New(cfg *config.Config, jwksClient *jwks.Client, cartaClient *carta.Client, siemClient *siem.Client, log *zap.Logger) *Handler {
	return &Handler{cfg: cfg, jwks: jwksClient, carta: cartaClient, siem: siemClient, log: log}
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

	w.Header().Set("X-Principal-Id", claims.Principal.PrincipalID)
	w.Header().Set("X-Tenant-Id", claims.TenantID)
	w.Header().Set("X-Legal-Entity-Id", claims.LegalEntityID)
	if claims.CorrelationID != "" {
		w.Header().Set("X-Correlation-Id", claims.CorrelationID)
	}
	w.WriteHeader(http.StatusOK)
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
