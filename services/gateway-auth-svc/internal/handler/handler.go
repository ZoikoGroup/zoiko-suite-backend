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

	// metrics is never nil — New installs nopMetrics.
	metrics DomainMetrics
}

// DomainMetrics records the outcomes this service's HTTP metrics cannot
// express.
//
// /verify is a single route, so http_requests_total collapses every refusal
// into one bucket on one path: a missing bearer token (the gateway working as
// designed) is indistinguishable there from a JWKS outage (every login in the
// estate failing). Those need opposite responses, so the outcome has to be a
// label.
//
// Declared here, in the package that produces the events, rather than the
// handler importing internal/telemetry: that keeps handler tests free of a
// Prometheus registry, and stops a second registration of the same collector
// inside a test binary. internal/telemetry.Metrics satisfies it.
type DomainMetrics interface {
	// VerifyDecision records a terminal outcome of /verify. Values are
	// telemetry.Outcome* — see that package for what each one means.
	VerifyDecision(outcome string)
	// JWKSError records a key-retrieval failure by operation: "fetch"
	// (identity-context-svc unreachable) or "key_lookup" (kid not in the set).
	JWKSError(operation string)
	// TenantContext records a GOV-01 resolution outcome: resolved, stale,
	// denied, unavailable or disabled.
	TenantContext(outcome string)
	// CartaDecision records a continuous-risk decision, including the ALLOW
	// and STEP_UP_MFA ones that do not block.
	CartaDecision(decision string)
}

// nopMetrics is the default, so a Handler built without metrics — every
// handler unit test — behaves identically and needs no wiring.
type nopMetrics struct{}

func (nopMetrics) VerifyDecision(string) {}
func (nopMetrics) JWKSError(string)      {}
func (nopMetrics) TenantContext(string)  {}
func (nopMetrics) CartaDecision(string)  {}

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
		cfg:     cfg,
		jwks:    jwksClient,
		carta:   cartaClient,
		siem:    siemClient,
		tenant:  tenantResolver,
		log:     log,
		metrics: nopMetrics{},
	}
}

// UseMetrics attaches a domain metrics recorder. Separate from New so the
// existing constructor signature — and every caller and test of it — is
// unchanged.
func (h *Handler) UseMetrics(m DomainMetrics) *Handler {
	if m != nil {
		h.metrics = m
	}
	return h
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
		h.deny(w, "missing bearer token", outcomeNoToken)
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
		// A JWKS failure is NOT a bad token, and conflating them is how a
		// total identity-context-svc outage reads as a wave of users
		// presenting invalid credentials. The response stays 401 either way —
		// the caller genuinely cannot be verified — but the signal does not.
		switch {
		case errors.Is(err, jwks.ErrUnavailable):
			h.metrics.JWKSError("fetch")
		case errors.Is(err, jwks.ErrKeyNotFound):
			h.metrics.JWKSError("key_lookup")
		}
		h.log.Info("gateway rejected request",
			zap.Error(err),
			zap.String("forwarded_uri", r.Header.Get("X-Forwarded-Uri")),
		)
		h.deny(w, "invalid token", outcomeInvalidToken)
		return
	}

	if claims.Principal.PrincipalID == "" || claims.TenantID == "" {
		// The signature verified, so this is identity-context-svc minting an
		// envelope without a principal or a tenant — an upstream defect, not a
		// caller one, and worth its own label so it is not buried among
		// ordinary bad tokens.
		h.log.Error("verified token carries incomplete claims",
			zap.String("principal_id", claims.Principal.PrincipalID),
			zap.String("tenant_id", claims.TenantID),
			zap.String("forwarded_uri", r.Header.Get("X-Forwarded-Uri")),
		)
		h.deny(w, "invalid token", outcomeIncompleteClaims)
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
		// Streamed, not just logged. This is the only refusal in this service
		// that is evidence of an ATTACK rather than of a misconfiguration — a
		// validly-signed token for tenant A presented against tenant B's
		// hostname is token theft or replay, not a user mistake. It was
		// reaching a log line and nothing else, while strictly less serious
		// events (every CARTA decision, every tenant-context denial) were
		// already streaming to SIEM. Critical, because by construction the
		// token itself verified: whoever sent it holds valid credentials.
		h.siem.Stream(ctx, claims.TenantID, "auth.tenant_hostname_mismatch", siem.SeverityCritical,
			"token for tenant "+claims.TenantID+" presented against hostname-resolved tenant "+resolvedTenantID+
				" by principal "+claims.Principal.PrincipalID)
		h.denyMismatch(w, "token tenant does not match the hostname-resolved tenant")
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

	if assessment != nil {
		// Counted before the branch, so ALLOW and STEP_UP_MFA are visible too.
		h.metrics.CartaDecision(string(assessment.Decision))
	}

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

		// ISOLATE/DENY/STEP_UP_MFA are hard-blocked: they represent risk high
		// enough that this platform has no automated remediation for them.
		// STEP_UP_MFA was previously allowed through because there was no
		// step-up challenge flow downstream, but letting it pass silently
		// defeats the risk engine's intent — a request that needs step-up
		// must not proceed as if it were ALLOW.
		if assessment.Decision == carta.DecisionIsolate || assessment.Decision == carta.DecisionDeny || assessment.Decision == carta.DecisionStepUpMFA {
			h.metrics.VerifyDecision(outcomeCartaBlocked)
			h.denyCarta(w, "access denied by continuous risk assessment", string(assessment.Decision))
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
		if resolved.Stale {
			h.metrics.TenantContext("stale")
		} else {
			h.metrics.TenantContext("resolved")
		}
		setIfPresent(w, "X-Jurisdiction-Context", resolved.JurisdictionContext)
		setIfPresent(w, "X-Timezone", resolved.Timezone)
		setIfPresent(w, "X-Residency-Policy-Id", resolved.ResidencyPolicyID)
		if resolved.Stale {
			// Named on the response so a backend can tell that its context came
			// from a cache the registry could not confirm. Only ever set on
			// non-material reads — writes never reach here on a stale context.
			w.Header().Set("X-Tenant-Context-Stale", "true")
		}
	} else {
		// Resolution is switched off for this deployment (TENANT_REGISTRY_URL
		// unset). Recorded rather than left blank so "no tenant-context series
		// at all" cannot be misread as "the resolver is failing silently".
		h.metrics.TenantContext("disabled")
	}

	h.metrics.VerifyDecision(outcomeAllowed)
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
		h.metrics.TenantContext("unavailable")
		h.metrics.VerifyDecision(outcomeTenantContextUnresolved)
		// Fail closed, and say why: no fallback to a global or default tenant,
		// and no proceeding on a context nobody could confirm.
		w.Header().Set("X-Tenant-Context", "unresolved")
		w.Header().Set(denialReasonHeader, outcomeTenantContextUnresolved)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("tenant context could not be resolved"))
		return
	}

	h.metrics.TenantContext("denied")
	h.metrics.VerifyDecision(outcomeTenantContextDenied)
	h.siem.Stream(r.Context(), claims.TenantID, "tenant_context.denied", siem.SeverityHigh,
		"tenant context denied for principal "+claims.Principal.PrincipalID)
	w.Header().Set("X-Tenant-Context", "denied")
	w.Header().Set(denialReasonHeader, outcomeTenantContextDenied)
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

// bearerToken extracts the credential from an Authorization header.
//
// The scheme is matched case-insensitively: RFC 7235 §2.1 defines auth-scheme
// as a case-insensitive token, and RFC 6750 inherits that. A strict
// HasPrefix("Bearer ") refused "bearer <token>" — a spelling several HTTP
// clients emit by default — with the same 401 as a forged credential, which is
// the least diagnosable way to fail a conformant caller.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// Outcome labels, mirroring internal/telemetry's exported constants.
//
// Duplicated rather than imported because the handler deliberately does not
// depend on internal/telemetry — see DomainMetrics. The audit script compares
// the two lists so they cannot drift apart unnoticed.
const (
	outcomeAllowed                 = "allowed"
	outcomeNoToken                 = "no_token"
	outcomeInvalidToken            = "invalid_token"
	outcomeIncompleteClaims        = "incomplete_claims"
	outcomeTenantHostnameMismatch  = "tenant_hostname_mismatch"
	outcomeCartaBlocked            = "carta_blocked"
	outcomeTenantContextDenied     = "tenant_context_denied"
	outcomeTenantContextUnresolved = "tenant_context_unresolved"
)

// denialReasonHeader names why a request was refused, on every refusal.
//
// Traefik returns an unsuccessful ForwardAuth reply to the client verbatim —
// status, body and headers — so this reaches the caller and is what lets a
// console tell the gateway's own refusals apart from the backend's.
const denialReasonHeader = "X-Auth-Denial-Reason"

// deny refuses with 401 and names the reason.
func (h *Handler) deny(w http.ResponseWriter, msg, outcome string) {
	h.metrics.VerifyDecision(outcome)
	w.Header().Set(denialReasonHeader, outcome)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(msg))
}

// denyCarta refuses with 403 because continuous risk assessment said so.
//
// X-Carta-Decision is set here and ONLY here. It used to be set by a shared
// helper that the tenant/hostname mismatch also called, so a token-spoofing
// rejection was reported to the caller as a decision the risk engine had made
// — attributing a refusal to a component that had not been consulted, and
// sending anyone reading the header to carta-svc's logs to look for a decision
// that was never there.
func (h *Handler) denyCarta(w http.ResponseWriter, msg, decision string) {
	w.Header().Set("X-Carta-Decision", decision)
	w.Header().Set(denialReasonHeader, outcomeCartaBlocked)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(msg))
}

// denyMismatch refuses with 403 because the token's tenant is not the tenant
// the caller's hostname resolved to.
func (h *Handler) denyMismatch(w http.ResponseWriter, msg string) {
	h.metrics.VerifyDecision(outcomeTenantHostnameMismatch)
	w.Header().Set(denialReasonHeader, outcomeTenantHostnameMismatch)
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(msg))
}
