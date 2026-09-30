package context

import (
	// stdlib context, imported inside package context: the import name
	// shadows this package's own name within the file, which is never used to
	// qualify its own identifiers. Same pattern as interfaces.go.
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	svcenvelope "zoiko.io/identity-context-svc/internal/envelope"
	"zoiko.io/identity-context-svc/internal/session"
	"zoiko.io/identity-context-svc/internal/store"
)

// Action constants passed to authorization-svc as action_type.
//
// Scoped at the tenant: domain.Principal and domain.SessionContext carry
// TenantID and no legal entity, so there is no finer scope to pass. Naming
// the real one is better than inventing a legal entity that does not exist.
const (
	IdentitySessionRead       = "IDENTITY_SESSION_READ"
	IdentitySessionInvalidate = "IDENTITY_SESSION_INVALIDATE"
	IdentityPrincipalRead     = "IDENTITY_PRINCIPAL_READ"
	IdentityPrincipalStatus   = "IDENTITY_PRINCIPAL_STATUS_SET"
)

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	// tenantID is the caller's gateway-verified tenant scope, forwarded to
	// authorization-svc as X-Tenant-Id. Without it /v1/authorize answers 400
	// envelope_incomplete and this client fails closed, so every guarded route
	// below reports an outage instead of a decision.
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, tenantID string) error
}

// Handler exposes the inbound REST endpoints defined in openapi.yaml.
type Handler struct {
	resolver   *Resolver
	auth       *Authenticator
	sessions   SessionCache
	principals PrincipalStore
	authz      AuthzChecker
	log        *zap.Logger

	// environment is stamped on every TenantContextDecision. Held on the
	// handler rather than read from config at each call because it is a
	// deployment fact, not a request one.
	environment domain.Environment

	// support backs the AttachSupportContext command family. Nil when support
	// context is not wired, in which case those routes answer 501 rather than
	// panicking — an unconfigured privileged command should be unavailable,
	// not silently permissive.
	support *SupportService

	// cache backs RefreshTenantContextCache and InvalidateTenantContext.
	cache *ContextCacheService
}

func NewHandler(
	resolver *Resolver,
	auth *Authenticator,
	sessions SessionCache,
	principals PrincipalStore,
	authz AuthzChecker,
	log *zap.Logger,
) *Handler {
	return &Handler{
		resolver:    resolver,
		auth:        auth,
		sessions:    sessions,
		principals:  principals,
		authz:       authz,
		log:         log,
		environment: domain.EnvironmentLocal,
	}
}

// WithEnvironment sets the deployment tier recorded on every decision.
func (h *Handler) WithEnvironment(e domain.Environment) *Handler {
	if e.Valid() {
		h.environment = e
	}
	return h
}

// WithSupport wires the privileged support-context commands.
func (h *Handler) WithSupport(s *SupportService) *Handler {
	h.support = s
	return h
}

// WithContextCache wires RefreshTenantContextCache and InvalidateTenantContext.
func (h *Handler) WithContextCache(c *ContextCacheService) *Handler {
	h.cache = c
	return h
}

// ── What is guarded here, and the one route that cannot be ──────────────
//
// POST /v1/context/resolve has NO principal or authorization guard, and a
// guard there would be circular. It takes a token in the request body and
// returns a signed IdentityContextEnvelope JWT, answering 401 on
// ErrTokenInvalid / ErrNoToken. It IS the authentication endpoint — the
// thing that mints the envelope every other service trusts. Requiring a
// verified principal before it would mean needing an envelope in order to
// obtain one.
//
// That is the strongest instance of the row 84d pattern in the estate,
// stronger than carta's /evaluate (which scores a principal) or
// siem's /stream (which records an authentication failure). All three want
// SERVICE identity via mTLS, not a user principal.
// TestResolveStillWorksWithoutIdentityHeaders pins it.
//
// Everything else is guarded, and two of them were CROSS-TENANT before this
// change — not merely unauthorized:
//
//   - GET /context/session/{id} had no tenant check at all and returns
//     EnvelopeJWT, the signed envelope itself. Knowing a session id was
//     enough to obtain a working credential for that identity, in any
//     tenant. That is credential theft, not a data leak, and it is the most
//     severe defect found in Priority 2b.
//   - POST /context/session/{id}/invalidate had no tenant check either, so
//     any caller could force-logout any principal in any tenant — and the
//     actor recorded against it came from an unverified
//     X-Actor-Principal-ID header, so the audit trail named whoever the
//     caller claimed to be.
//
// The four principal routes were already tenant-scoped (they pass tenantID
// to the store); they lacked authorization only.

// requireTenant returns the gateway-verified tenant, or refuses.
//
// Kept as 401 rather than the pre-existing 400: a request with no verified
// tenant is unauthenticated, not malformed. The three principal read routes
// previously answered 400 here, which is a smaller thing but wrong in a way
// that matters for a client deciding whether to retry or re-authenticate.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-Id")
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized,
			"X-Tenant-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	return tenantID, true
}

// requirePrincipal returns the gateway-verified calling principal.
//
// X-Actor-Principal-ID is deliberately NOT accepted as a source here. It was
// read directly by InvalidateSession and UpdatePrincipalStatus and recorded
// as the actor without ever being verified, which made the audit trail
// self-reported: a caller chose the name that would appear against a forced
// logout or a status change. The gateway-verified X-Principal-Id is the only
// identity this handler will attribute an action to.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized,
			"X-Principal-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	return principalID, true
}

// requireCorrelation returns the request's correlation id, or refuses.
//
// §4's Engineering Interaction Wireframe marks every GOV-01 QUERY "Read-only;
// scoped; correlation required". Scoped was enforced by requireTenant and
// requirePrincipal; correlation was not enforced by anything. The envelope
// middleware parses and REPORTS it, but its default mode is write-strict,
// which refuses material writes and admits reads — so a bare GET carrying no
// correlation succeeded and produced a decision nothing could later be traced
// to.
//
// 400 rather than 401: the caller is authenticated and scoped, the request is
// simply not traceable, which is a malformed request rather than an
// unauthenticated one.
func (h *Handler) requireCorrelation(w http.ResponseWriter, r *http.Request) (string, bool) {
	correlationID := strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
	if correlationID == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeContextUnresolved,
			"X-Correlation-ID is required on GOV-01 queries — a governance read must be traceable to the story that caused it")
		return "", false
	}
	return correlationID, true
}

// authorizeUnlessSelf permits an action on the caller's OWN principal or
// session without an authorization round-trip, and requires a grant
// otherwise.
//
// The exemption is load-bearing, not a convenience. Reading your own roles,
// your own delegations, or your own session is ordinary platform traffic; if
// it needed an explicit grant, every principal on the platform would need
// one and the check would be noise that everybody holds. What is privileged
// is reading or acting on SOMEONE ELSE'S — which is exactly the case that
// previously had no check at all.
//
// Note this is not applied to status changes: see UpdatePrincipalStatus.
func (h *Handler) authorizeUnlessSelf(w http.ResponseWriter, r *http.Request, callerPrincipalID, subjectPrincipalID, tenantID, action string) bool {
	if callerPrincipalID == subjectPrincipalID {
		return true
	}
	return h.authorize(w, r, callerPrincipalID, tenantID, action)
}

// authorize asks authorization-svc whether this principal may perform
// action within tenantID, and fails CLOSED.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, action, tenantID); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return false
		}
		h.log.Error("authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return false
	}
	return true
}

// RegisterRoutes mounts all endpoints under a chi Router.
// All routes are under /v1/ per URI versioning strategy.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1", func(r chi.Router) {
		r.Post("/authenticate", h.Authenticate)

		// ── GOV-01 queries ───────────────────────────────────────────────────
		// ResolveTenantContext
		r.Post("/context/resolve", h.ResolveContext)
		// GetEffectiveContext
		r.Get("/context/session/{sessionContextID}", h.GetSession)
		// ExplainContextResolution
		r.Get("/context/session/{sessionContextID}/explain", h.ExplainContext)

		// ── GOV-01 commands ──────────────────────────────────────────────────
		r.Post("/context/session/{sessionContextID}/invalidate", h.InvalidateSession)
		// RefreshTenantContextCache
		r.Post("/context/cache/refresh", h.RefreshContextCache)
		// InvalidateTenantContext — tenant-wide, distinct action from the
		// per-session invalidate above. See InvalidateTenantContext.
		r.Post("/context/tenant/invalidate", h.InvalidateTenantContext)

		// AttachSupportContext (privileged)
		r.Post("/context/support", h.AttachSupportContext)
		r.Get("/context/support/{supportContextID}", h.GetSupportContext)
		r.Delete("/context/support/{supportContextID}", h.RevokeSupportContext)
		// The review half of §1's "followed by reconciliation/review". The
		// reconciler goroutine reports what is pending; this closes one.
		r.Post("/context/support/{supportContextID}/review", h.ReviewSupportContext)

		r.Get("/principals/{principalID}", h.GetPrincipal)
		r.Get("/principals/{principalID}/roles", h.GetPrincipalRoles)
		r.Get("/principals/{principalID}/delegations", h.GetPrincipalDelegations)
		r.Put("/principals/{principalID}/status", h.UpdatePrincipalStatus)
	})
}

// ── POST /v1/context/resolve ─────────────────────────────────────────────────

func (h *Handler) ResolveContext(w http.ResponseWriter, r *http.Request) {
	var req domain.ResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeContextUnresolved, "invalid request body")
		return
	}

	// Server-resolved, never client-supplied. The fields carry json:"-" so a
	// body that names them is ignored, and they are filled here from what the
	// SERVER observed about the connection.
	req.IngressSource = CanonicalIngress(r)
	req.Environment = h.environment
	// A support-scoped resolve carries its grant so every session issued under
	// an elevation is attributable to it. Absent for ordinary traffic.
	//
	// This header is CLIENT-SUPPLIED and is not sanitized at the edge — the
	// gateway's authResponseHeaders list does not include it and neither does
	// the GTRM edge strip. Taking it here is therefore an assertion, not a
	// fact; the resolver verifies it against the grant register before any
	// session is attributed to it. See Resolver.verifySupportContext.
	if scID := r.Header.Get("X-Support-Context-Id"); scID != "" {
		req.SupportContextID = &scID
	}

	// Canonical envelope facts the middleware already parsed and validated.
	// Read from the resolved envelope rather than re-read from headers, so
	// there is exactly one place in the service that decides what these mean.
	if env, ok := svcenvelope.FromContext(r.Context()); ok {
		req.SourceChannel = string(env.SourceChannel)
		req.WorkloadID = env.WorkloadID
		req.CausationID = env.CausationID
	}

	result, err := h.resolver.Resolve(r.Context(), req)
	if err != nil {
		h.log.Warn("resolve failed",
			zap.Error(err),
			zap.String("ingress", req.IngressSource),
			zap.String("correlation_id", req.CorrelationID))
		switch {
		case errors.Is(err, domain.ErrIngressTenantMismatch):
			// 401, not 403. The caller is not forbidden from an action — the
			// context in which they claimed to act could not be established.
			writeCoded(w, http.StatusUnauthorized, domain.ErrCodeContextUnresolved,
				"context could not be resolved for this request")
		case errors.Is(err, domain.ErrResidencyDenied):
			writeCoded(w, http.StatusForbidden, domain.ErrCodeResidencyDenied, err.Error())
		case errors.Is(err, ErrSAMLUnsupported):
			writeCoded(w, http.StatusBadRequest, domain.ErrCodeUnsupported, err.Error())
		case errors.Is(err, ErrTrustPostureBlocked):
			writeCoded(w, http.StatusUnauthorized, domain.ErrCodeTrustPostureBlocked, err.Error())
		// A refused support elevation. 401 rather than 403 for the same reason
		// the ingress mismatch above is 401: the caller is not forbidden an
		// action, the context it claimed to act in could not be established.
		//
		// These two codes were declared for exactly this and, until now, were
		// reachable only from the support read/revoke routes.
		case errors.Is(err, domain.ErrSupportContextExpired):
			writeCoded(w, http.StatusUnauthorized, domain.ErrCodeBreakGlassExpired, err.Error())
		case errors.Is(err, domain.ErrSupportContextNotFound):
			writeCoded(w, http.StatusUnauthorized, domain.ErrCodeBreakGlassRequired, err.Error())
		// Not the caller's fault: support is unwired in this deployment, so
		// the assertion could not be checked either way. 503, not 401.
		case errors.Is(err, domain.ErrSupportContextUnverifiable):
			writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable, err.Error())
		case errors.Is(err, ErrTokenInvalid),
			errors.Is(err, ErrPrincipalInactive),
			errors.Is(err, ErrTenantInactive),
			errors.Is(err, ErrEntityUnauthorized),
			errors.Is(err, ErrNoToken):
			writeCoded(w, http.StatusUnauthorized, domain.ErrCodeContextUnresolved, err.Error())
		case errors.Is(err, ErrUpstreamUnavailable):
			writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable, err.Error())
		default:
			writeCoded(w, http.StatusInternalServerError, domain.ErrCodeContextUnresolved, "internal error")
		}
		return
	}

	// Tokens must never be cached by an intermediary. Same reasoning as
	// /v1/authenticate: what is returned here is a bearer credential.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, domain.ResolveResponseV2{
		EnvelopeJWT:      result.EnvelopeJWT,
		EvidenceID:       result.EvidenceID,
		SessionContextID: result.SessionContextID,
		ExpiresAt:        result.ExpiresAt.Unix(),
	})
}

// ── GET /v1/context/session/:sessionContextID ────────────────────────────────

// GetSession returns the signed IdentityContextEnvelope for a session.
//
// This route had NO tenant check and NO principal check, and what it returns
// is the envelope JWT itself — the credential every other service on the
// platform trusts. Knowing a session id was therefore enough to obtain a
// working credential for that identity, in any tenant. That is credential
// theft rather than a data leak, which makes it the most severe defect found
// in Priority 2b.
//
// SessionCache.Get takes no tenant, so the scope has to come from the
// session record: GetSessionContext carries TenantID and PrincipalID, and
// both are checked before the JWT is handed over.
//
// A foreign session answers 404, never 403 — a distinct forbidden would let
// a caller enumerate valid session ids, and session ids are exactly what an
// attacker would be probing for here.
func (h *Handler) GetSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// §4: GOV-01 queries are "Read-only; scoped; correlation required".
	// Scoped was enforced here; correlation was enforced by nothing, because
	// the envelope's default write-strict mode admits reads.
	if _, ok := h.requireCorrelation(w, r); !ok {
		return
	}
	sessionContextID := chi.URLParam(r, "sessionContextID")

	sc, err := h.sessions.GetSessionContext(r.Context(), sessionContextID, tenantID)
	if err != nil || sc == nil {
		writeError(w, http.StatusNotFound, "session not found or expired")
		return
	}
	if sc.TenantID != tenantID {
		writeError(w, http.StatusNotFound, "session not found or expired")
		return
	}
	// Even within the tenant, handing one principal's envelope to another is
	// credential sharing. Own session needs no grant; anyone else's does.
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, sc.PrincipalID, tenantID, IdentitySessionRead) {
		return
	}

	jwt, err := h.sessions.Get(r.Context(), sessionContextID, tenantID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found or expired")
		return
	}
	writeJSON(w, http.StatusOK, domain.GetSessionResponse{EnvelopeJWT: jwt})
}

// ── POST /v1/context/session/:sessionContextID/invalidate ────────────────────

// InvalidateSession ends a session.
//
// This route also had no tenant check, so any caller could force-logout any
// principal in any tenant by session id. Two things were wrong at once, and
// the second is easy to miss: the actor recorded against the invalidation
// came from an UNVERIFIED X-Actor-Principal-ID header, so the audit trail
// named whoever the caller claimed to be. A forced logout attributed to an
// arbitrary name is worse than an unattributed one — it implicates someone.
//
// Self-invalidation (logging yourself out) needs no grant; ending someone
// else's session does.
func (h *Handler) InvalidateSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	sessionContextID := chi.URLParam(r, "sessionContextID")
	correlationID := r.Header.Get("X-Correlation-ID")

	sc, err := h.sessions.GetSessionContext(r.Context(), sessionContextID, tenantID)
	if err != nil || sc == nil {
		writeError(w, http.StatusNotFound, "session not found or expired")
		return
	}
	if sc.TenantID != tenantID {
		writeError(w, http.StatusNotFound, "session not found or expired")
		return
	}
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, sc.PrincipalID, tenantID, IdentitySessionInvalidate) {
		return
	}

	var req domain.InvalidateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// VALIDATE THE REASON HERE, not in Postgres.
	//
	// It used to travel unchecked to the store, where the
	// session_contexts_invalidation_reason_check constraint refused it and the
	// caller got 500 "failed to invalidate session". A request that names no
	// reason is a bad request, and saying so is the difference between "you
	// left a field out" and "this service is broken".
	//
	// The reason is not decoration: it is what the evidence record says about
	// why a session ended, and it is what a reviewer groups by.
	if !domain.ValidInvalidationReason(req.Reason) {
		writeError(w, http.StatusBadRequest,
			`reason is required and must be one of LOGOUT, ADMIN_REVOKE, RISK_ESCALATION, DELEGATION_REVOKED`)
		return
	}

	// The VERIFIED caller is recorded as the actor, not the self-reported
	// header this used to trust.
	if err := h.resolver.InvalidateSession(
		r.Context(), sessionContextID, tenantID, req.Reason, callerPrincipalID, correlationID,
	); err != nil {
		h.log.Error("invalidate session failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to invalidate session")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── GET /v1/principals/:principalID ─────────────────────────────────────────

func (h *Handler) GetPrincipal(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	principalID := chi.URLParam(r, "principalID")
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, principalID, tenantID, IdentityPrincipalRead) {
		return
	}
	p, err := h.principals.FindByID(r.Context(), principalID, tenantID)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "principal not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── GET /v1/principals/:principalID/roles ────────────────────────────────────

func (h *Handler) GetPrincipalRoles(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	principalID := chi.URLParam(r, "principalID")
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, principalID, tenantID, IdentityPrincipalRead) {
		return
	}
	assignments, err := h.principals.FindActiveRoleAssignments(r.Context(), principalID, tenantID, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retrieve role assignments")
		return
	}
	writeJSON(w, http.StatusOK, assignments)
}

// ── GET /v1/principals/:principalID/delegations ──────────────────────────────

func (h *Handler) GetPrincipalDelegations(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	principalID := chi.URLParam(r, "principalID")
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, principalID, tenantID, IdentityPrincipalRead) {
		return
	}
	delegations, err := h.principals.FindActiveDelegations(r.Context(), principalID, tenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retrieve delegations")
		return
	}
	writeJSON(w, http.StatusOK, delegations)
}

// ── PUT /v1/principals/:principalID/status ───────────────────────────────────
// Status transitions only. No soft-delete per doctrine (data-model §2.11).
// Idempotent — re-applying same status is a no-op at the DB level.

// UpdatePrincipalStatus suspends or reactivates a principal.
//
// This is the only route that gets NO self-exemption, deliberately. The
// authorizeUnlessSelf shortcut is right for reads — everyone legitimately
// reads their own roles — but wrong here in both directions: a suspended
// principal reactivating itself defeats the suspension, and letting anyone
// change their own status makes the control meaningless. Every status change
// requires an explicit grant, including on yourself.
//
// The actor recorded against the change is now the VERIFIED caller. It was
// previously taken from an unverified X-Actor-Principal-ID header, so the
// record of who suspended an account named whoever the caller claimed.
func (h *Handler) UpdatePrincipalStatus(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	principalID := chi.URLParam(r, "principalID")
	correlationID := r.Header.Get("X-Correlation-ID")

	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityPrincipalStatus) {
		return
	}

	var req domain.UpdateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.principals.UpdateStatus(
		r.Context(), principalID, tenantID, req.Status, callerPrincipalID, correlationID,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update status")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// errorResponse is the refusal body.
//
// ErrorCode carries one of the spec's stable error classes (section 16). The
// message is for a human reading a log; the CODE is what a client branches on,
// and it is the half that must not change with a refactor. Before this, every
// refusal from this service was an untyped message string, so a caller could
// only distinguish "wrong password" from "residency denied" by matching prose.
type errorResponse struct {
	Error         string `json:"error"`
	ErrorCode     string `json:"error_code,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	EvidenceID    string `json:"evidence_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// writeCoded is writeError with one of the spec's stable error classes.
func writeCoded(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorResponse{Error: msg, ErrorCode: code})
}

// Ensure the interfaces defined in interfaces.go are satisfied at compile time.
// The concrete implementations live in their own packages.
var _ PrincipalStore = (*store.PgStore)(nil)

// DurableCache, not the legacy Redis-only Cache: cmd/server/main.go wires
// session.NewDurableCache, and it is the one that carries the tenant scope and
// EvictAllForPrincipal the interface now requires.
var _ SessionCache = (*session.DurableCache)(nil)
var _ RiskSignalCache = (*session.RiskSignalCache)(nil)

// ── POST /v1/authenticate ────────────────────────────────────────────────────
//
// The one endpoint on this service reachable without an identity, and the
// entry point to the whole platform: it exchanges a human's password for the
// bearer token /v1/context/resolve accepts.
//
// It is exempt from the canonical input contract (see EXEMPT_PATHS in
// services/_contract/rollout.sh). The contract's unconditionally mandatory
// fields include X-Tenant-Id and an actor header, and gateway-auth-svc sets
// those only after verifying a signed envelope — which is what this endpoint
// exists to make obtainable. Demanding them here would be circular, and
// satisfiable only by a caller asserting the identity it has not yet proven.
//
// The tenant is therefore taken from the request BODY, not a header. That is
// not a weakening: naming a tenant selects which tenant's principals to search
// and confers nothing. A caller naming a tenant it has no credential in gets
// the same rejection as any other wrong password.
//
// Response: 200 with a bearer token / 400 missing field / 401 invalid
// credentials / 503 unavailable.
func (h *Handler) Authenticate(w http.ResponseWriter, r *http.Request) {
	var req domain.AuthenticateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = r.Header.Get("X-Correlation-ID")
	}

	resp, err := h.auth.Authenticate(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, ErrRequestInvalid):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrInvalidCredentials):
			// One message for every rejection reason. See ErrInvalidCredentials
			// — the reason is in access_decision_log and the event stream, not
			// on the wire, so this response cannot be used to enumerate
			// accounts or probe lockout state.
			writeError(w, http.StatusUnauthorized, "invalid credentials")
		case errors.Is(err, ErrAuthUnavailable):
			// 503, not 401: the attempt could not be decided. Collapsing the
			// two would report an outage as a wrong password.
			h.log.Error("authentication unavailable", zap.Error(err),
				zap.String("correlation_id", req.CorrelationID))
			writeError(w, http.StatusServiceUnavailable, "authentication unavailable")
		default:
			h.log.Error("unexpected authentication error", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Tokens must never be cached by an intermediary or written to a shared
	// store. Set explicitly rather than relying on any default.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}
