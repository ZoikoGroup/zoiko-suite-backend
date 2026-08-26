package context

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/session"
	"zoiko.io/identity-context-svc/internal/store"
)

// AuthZClient checks whether a principal is authorized to perform an action.
type AuthZClient interface {
	// CheckAllowed returns nil if principalID is authorized to perform
	// actionType within legalEntityID and tenantID. Returns domain.ErrAuthorizationDenied
	// on a DENIED decision, or domain.ErrAuthorizationServiceUnavailable if
	// no decision could be obtained — callers must fail closed on the latter.
	// tenantID is the caller's verified tenant scope (from X-Tenant-Id header).
	// Empty string means no tenant scope (global-only SoD rules).
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType, tenantID string) error
}

const actionPrincipalStatusManage = "PRINCIPAL_STATUS_MANAGE"

// ErrIdentityMissing is returned when X-Principal-Id header is absent.
var ErrIdentityMissing = errors.New("missing X-Principal-Id header")

// ErrTenantMissing is returned when X-Tenant-Id header is absent.
var ErrTenantMissing = errors.New("missing X-Tenant-Id header")

// Handler exposes the inbound REST endpoints defined in openapi.yaml.
type Handler struct {
	resolver   *Resolver
	auth       *Authenticator
	sessions   SessionCache
	principals PrincipalStore
	authz      AuthZClient
	log        *zap.Logger
}

func NewHandler(
	resolver *Resolver,
	auth *Authenticator,
	sessions SessionCache,
	principals PrincipalStore,
	authz AuthZClient,
	log *zap.Logger,
) *Handler {
	return &Handler{
		resolver:   resolver,
		auth:       auth,
		sessions:   sessions,
		principals: principals,
		authz:      authz,
		log:        log,
	}
}

// RegisterRoutes mounts all endpoints under a chi Router.
// All routes are under /v1/ per URI versioning strategy.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1", func(r chi.Router) {
		r.Post("/authenticate", h.Authenticate)
		r.Post("/context/resolve", h.ResolveContext)
		r.Get("/context/session/{sessionContextID}", h.GetSession)
		r.Post("/context/session/{sessionContextID}/invalidate", h.InvalidateSession)

		r.Get("/principals/{principalID}", h.GetPrincipal)
		r.Get("/principals/{principalID}/roles", h.GetPrincipalRoles)
		r.Get("/principals/{principalID}/delegations", h.GetPrincipalDelegations)
		r.Put("/principals/{principalID}/status", h.UpdatePrincipalStatus)
	})
}

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
		case errors.Is(err, ErrAuthRequestInvalid):
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

// ── POST /v1/context/resolve ─────────────────────────────────────────────────

func (h *Handler) ResolveContext(w http.ResponseWriter, r *http.Request) {
	var req domain.ResolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	jwt, err := h.resolver.Resolve(r.Context(), req)
	if err != nil {
		h.log.Warn("resolve failed", zap.Error(err), zap.String("correlation_id", req.CorrelationID))
		switch {
		case errors.Is(err, ErrTokenInvalid),
			errors.Is(err, ErrPrincipalInactive),
			errors.Is(err, ErrTenantInactive),
			errors.Is(err, ErrEntityUnauthorized),
			errors.Is(err, ErrTrustPostureBlocked),
			errors.Is(err, ErrNoToken):
			writeError(w, http.StatusUnauthorized, err.Error())
		case errors.Is(err, ErrUpstreamUnavailable):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	writeJSON(w, http.StatusOK, domain.ResolveResponse{EnvelopeJWT: jwt})
}

// ── GET /v1/context/session/:sessionContextID ────────────────────────────────

func (h *Handler) GetSession(w http.ResponseWriter, r *http.Request) {
	sessionContextID := chi.URLParam(r, "sessionContextID")
	jwt, err := h.sessions.Get(r.Context(), sessionContextID)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found or expired")
		return
	}
	writeJSON(w, http.StatusOK, domain.GetSessionResponse{EnvelopeJWT: jwt})
}

// ── POST /v1/context/session/:sessionContextID/invalidate ────────────────────

func (h *Handler) InvalidateSession(w http.ResponseWriter, r *http.Request) {
	sessionContextID := chi.URLParam(r, "sessionContextID")
	correlationID := r.Header.Get("X-Correlation-ID")
	actorPrincipalID := r.Header.Get("X-Actor-Principal-ID")

	var req domain.InvalidateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.resolver.InvalidateSession(
		r.Context(), sessionContextID, req.Reason, actorPrincipalID, correlationID,
	); err != nil {
		h.log.Error("invalidate session failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to invalidate session")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── PUT /v1/principals/:principalID/status ───────────────────────────────────
// Status transitions only. No soft-delete per doctrine (data-model §2.11).
// Idempotent — re-applying same status is a no-op at the DB level.
//
// Requires X-Principal-Id and X-Tenant-Id headers. Authorization is checked
// against PRINCIPAL_STATUS_MANAGE on the platform scope legal entity.
// req.Status must be a valid PrincipalStatus value.
//
// Response: 204 no content / 400 invalid status / 401 missing identity or tenant
// / 403 authorization denied / 404 principal not found / 503 unavailable.
func (h *Handler) UpdatePrincipalStatus(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	// Authorization: caller must hold PRINCIPAL_STATUS_MANAGE at platform scope.
	// The platform scope legal entity is configured via AUTHZ_PLATFORM_SCOPE_ENTITY_ID.
	// If not configured, the check fails closed.
	if err := h.authz.CheckAllowed(r.Context(), principalID, "", actionPrincipalStatusManage, tenantID); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	correlationID := r.Header.Get("X-Correlation-ID")
	actorPrincipalID := r.Header.Get("X-Actor-Principal-ID")

	var req domain.UpdateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate status against domain.PrincipalStatus values.
	if req.Status != domain.PrincipalStatusActive && req.Status != domain.PrincipalStatusSuspended && req.Status != domain.PrincipalStatusDisabled {
		writeError(w, http.StatusBadRequest, "invalid status: must be ACTIVE, SUSPENDED, or DISABLED")
		return
	}

	targetPrincipalID := chi.URLParam(r, "principalID")

	if err := h.principals.UpdateStatus(
		r.Context(), targetPrincipalID, tenantID, req.Status, actorPrincipalID, correlationID,
	); err != nil {
		h.writeStoreErr(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── GET /v1/principals/:principalID ─────────────────────────────────────────

func (h *Handler) GetPrincipal(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	p, err := h.principals.FindByID(r.Context(), chi.URLParam(r, "principalID"), tenantID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "principal not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── GET /v1/principals/:principalID/roles ────────────────────────────────────

func (h *Handler) GetPrincipalRoles(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	assignments, err := h.principals.FindActiveRoleAssignments(r.Context(), principalID, tenantID, nil)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, assignments)
}

// ── GET /v1/principals/:principalID/delegations ──────────────────────────────

func (h *Handler) GetPrincipalDelegations(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	delegations, err := h.principals.FindActiveDelegations(r.Context(), principalID, tenantID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, delegations)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// requirePrincipal reads the caller's verified principal from the
// X-Principal-Id header, rejecting the request if absent.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

// requireTenant reads the caller's verified tenant scope from the
// X-Tenant-Id header, rejecting the request if absent.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := r.Header.Get("X-Tenant-Id")
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, ErrTenantMissing.Error())
		return "", false
	}
	return tenantID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "authorization denied")
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz unavailable")
	}
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrPrincipalNotFound) {
		writeError(w, http.StatusNotFound, "principal not found")
		return
	}
	h.log.Error("principal store error", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "store unavailable")
}

type errorResponse struct {
	Error         string `json:"error"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// Ensure the interfaces defined in interfaces.go are satisfied at compile time.
// The concrete implementations live in their own packages.
var _ PrincipalStore = (*store.PgStore)(nil)
var _ SessionCache = (*session.Cache)(nil)
var _ RiskSignalCache = (*session.RiskSignalCache)(nil)
var _ CredentialStore = (*store.PgStore)(nil)
