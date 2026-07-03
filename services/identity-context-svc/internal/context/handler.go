package context

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/session"
	"zoiko.io/identity-context-svc/internal/store"
)

// Handler exposes the eight inbound REST endpoints defined in openapi.yaml.
type Handler struct {
	resolver   *Resolver
	sessions   SessionCache
	principals PrincipalStore
	log        *zap.Logger
}

func NewHandler(
	resolver *Resolver,
	sessions SessionCache,
	principals PrincipalStore,
	log *zap.Logger,
) *Handler {
	return &Handler{
		resolver:   resolver,
		sessions:   sessions,
		principals: principals,
		log:        log,
	}
}

// RegisterRoutes mounts all endpoints under a chi Router.
// All routes are under /v1/ per URI versioning strategy.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1", func(r chi.Router) {
		r.Post("/context/resolve", h.ResolveContext)
		r.Get("/context/session/{sessionContextID}", h.GetSession)
		r.Post("/context/session/{sessionContextID}/invalidate", h.InvalidateSession)

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

// ── GET /v1/principals/:principalID ─────────────────────────────────────────

func (h *Handler) GetPrincipal(w http.ResponseWriter, r *http.Request) {
	principalID := chi.URLParam(r, "principalID")
	p, err := h.principals.FindByID(r.Context(), principalID)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "principal not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── GET /v1/principals/:principalID/roles ────────────────────────────────────

func (h *Handler) GetPrincipalRoles(w http.ResponseWriter, r *http.Request) {
	principalID := chi.URLParam(r, "principalID")
	assignments, err := h.principals.FindActiveRoleAssignments(r.Context(), principalID, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retrieve role assignments")
		return
	}
	writeJSON(w, http.StatusOK, assignments)
}

// ── GET /v1/principals/:principalID/delegations ──────────────────────────────

func (h *Handler) GetPrincipalDelegations(w http.ResponseWriter, r *http.Request) {
	principalID := chi.URLParam(r, "principalID")
	delegations, err := h.principals.FindActiveDelegations(r.Context(), principalID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retrieve delegations")
		return
	}
	writeJSON(w, http.StatusOK, delegations)
}

// ── PUT /v1/principals/:principalID/status ───────────────────────────────────
// Status transitions only. No soft-delete per doctrine (data-model §2.11).
// Idempotent — re-applying same status is a no-op at the DB level.

func (h *Handler) UpdatePrincipalStatus(w http.ResponseWriter, r *http.Request) {
	principalID := chi.URLParam(r, "principalID")
	correlationID := r.Header.Get("X-Correlation-ID")
	actorPrincipalID := r.Header.Get("X-Actor-Principal-ID")

	var req domain.UpdateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.principals.UpdateStatus(
		r.Context(), principalID, req.Status, actorPrincipalID, correlationID,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update status")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

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
