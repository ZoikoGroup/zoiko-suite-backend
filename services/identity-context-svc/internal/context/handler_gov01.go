package context

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/sod"
)

// This file carries the four GOV-01 contract operations that had no
// implementation at all:
//
//	ExplainContextResolution     GET  /v1/context/session/{id}/explain
//	RefreshTenantContextCache    POST /v1/context/cache/refresh
//	InvalidateTenantContext      POST /v1/context/tenant/invalidate
//	AttachSupportContext         POST /v1/context/support
//	                             DELETE /v1/context/support/{id}
//	                             GET  /v1/context/support/{id}
//
// The spec's illustrative contract surface puts these under
// /internal/v1/gov01/..., which is a naming convention for the whole control
// plane rather than a requirement on any one service. They are mounted under
// this service's existing /v1/context/... tree so the estate has one URI
// versioning scheme rather than two, and the operation names are preserved in
// the OpenAPI operationIds where a contract test can actually check them.

// ── ExplainContextResolution ─────────────────────────────────────────────────

// ActionExplainContext guards the explain query.
//
// Reading the account of somebody else's resolution reveals their tenant,
// entity, ingress and trust posture, so it is privileged — but reading your
// OWN is not, for the same reason reading your own roles is not: if it needed
// a grant, every principal would need one and the check would be noise.
const ActionExplainContext = "IDENTITY_CONTEXT_EXPLAIN"

// ExplainContext answers "why did this context resolve the way it did".
//
// The answer is reconstructed from the recorded decision, never re-derived —
// see Explain. An `as_of` query parameter reconstructs the decision's standing
// at an instant, which is what makes "was this session live at 14:05?" a
// question with an evidenced answer rather than an inference.
func (h *Handler) ExplainContext(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	sessionContextID := chi.URLParam(r, "sessionContextID")

	sc, err := h.sessions.GetSessionContext(r.Context(), sessionContextID, tenantID)
	if err != nil {
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable,
			"could not read the session record")
		return
	}
	if sc == nil || sc.TenantID != tenantID {
		// 404 rather than 403 for a foreign session — a distinct forbidden
		// would confirm the id exists, and session ids are what an attacker
		// probes for.
		writeCoded(w, http.StatusNotFound, domain.ErrCodeContextUnresolved, "session not found")
		return
	}
	if !h.authorizeUnlessSelf(w, r, callerPrincipalID, sc.PrincipalID, tenantID, ActionExplainContext) {
		return
	}

	asOf := time.Now().UTC()
	if raw := r.URL.Query().Get("as_of"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeCoded(w, http.StatusBadRequest, domain.ErrCodeContextUnresolved,
				"as_of must be an RFC3339 timestamp")
			return
		}
		asOf = parsed.UTC()
	}

	writeJSON(w, http.StatusOK, Explain(sc, asOf))
}

// ── RefreshTenantContextCache ────────────────────────────────────────────────

// RefreshContextCache drops this service's cached routing hints for the
// caller's tenant.
//
// Scoped to the caller's own verified tenant, with no "all tenants" form. A
// single caller invalidating the entire estate's context cache is a
// denial-of-service primitive wearing a maintenance command's clothes.
func (h *Handler) RefreshContextCache(w http.ResponseWriter, r *http.Request) {
	if h.cache == nil {
		writeCoded(w, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"context cache commands are not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, ActionRefreshContextCache) {
		return
	}

	var req domain.RefreshCacheRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeContextUnresolved, "invalid request body")
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = r.Header.Get("X-Correlation-ID")
	}

	resp, err := h.cache.Refresh(r.Context(), tenantID, req, callerPrincipalID)
	if err != nil {
		h.log.Error("refresh tenant context cache failed", zap.Error(err))
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable,
			"could not refresh the context cache")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── InvalidateTenantContext ──────────────────────────────────────────────────

// InvalidateTenantContext revokes every live session in the caller's tenant.
//
// A separate route and a separate authorization action from the per-session
// invalidate, deliberately: "log one user out" and "log an entire tenant out"
// are not the same decision and must not share a permission.
//
// No self-exemption applies. Unlike reading your own session, there is no
// sense in which revoking a whole tenant is an ordinary self-service action.
func (h *Handler) InvalidateTenantContext(w http.ResponseWriter, r *http.Request) {
	if h.cache == nil {
		writeCoded(w, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"context cache commands are not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, ActionInvalidateTenantContext) {
		return
	}

	var req domain.InvalidateTenantContextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeContextUnresolved, "invalid request body")
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = r.Header.Get("X-Correlation-ID")
	}

	resp, err := h.cache.InvalidateTenant(r.Context(), tenantID, req, callerPrincipalID)
	if err != nil {
		if errors.Is(err, ErrRequestInvalid) {
			writeCoded(w, http.StatusBadRequest, domain.ErrCodeContextUnresolved, err.Error())
			return
		}
		h.log.Error("tenant context invalidation failed", zap.Error(err))
		// resp is non-nil on a PARTIAL revocation and carries the count that
		// did succeed. Returning it with a 207-equivalent 500 body is more
		// useful than a bare error: those sessions really are revoked, and a
		// caller who assumed otherwise would re-run a command that logs
		// everybody out twice.
		if resp != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":            "tenant invalidation incomplete",
				"error_code":       domain.ErrCodeUpstreamUnavailable,
				"sessions_revoked": resp.SessionsRevoked,
			})
			return
		}
		writeCoded(w, http.StatusInternalServerError, domain.ErrCodeUpstreamUnavailable,
			"tenant invalidation failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── AttachSupportContext (privileged) ────────────────────────────────────────

// AttachSupportContext grants a scoped, time-limited, independently-approved
// elevation into a customer tenant.
//
// The ONE privileged command GOV-01's contract names, and the one place in
// this service where a principal from outside a tenant is deliberately given
// standing inside it. Every control the spec's break-glass invariant lists is
// enforced before the write: scope, a bounded TTL, an independent approver, a
// reason code, a justification long enough to read, a ticket reference, an
// authorization grant, and a segregation-of-duties check.
//
// What it does NOT do is grant permissions. See SupportService.
func (h *Handler) AttachSupportContext(w http.ResponseWriter, r *http.Request) {
	if h.support == nil {
		// 501 rather than a permissive default. An unconfigured privileged
		// command should be unavailable, never silently open.
		writeCoded(w, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"support context is not configured in this deployment")
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.AttachSupportContextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeContextUnresolved, "invalid request body")
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = r.Header.Get("X-Correlation-ID")
	}

	// AUTHORIZATION IS SCOPED TO THE TARGET TENANT, not the caller's own.
	//
	// This is the one route where those differ and the difference is the whole
	// point: a support engineer lives in the support tenant and is asking for
	// standing in a customer's. Checking the grant against their own tenant
	// would ask whether they may attach support contexts AT HOME, which every
	// support engineer may, and would authorize an elevation into any tenant
	// on the platform.
	if req.TenantID == "" {
		writeCoded(w, http.StatusBadRequest, domain.ErrCodeContextUnresolved, "tenant_id is required")
		return
	}
	if !h.authorize(w, r, callerPrincipalID, req.TenantID, ActionAttachSupportContext) {
		return
	}

	sc, err := h.support.Attach(r.Context(), req, callerPrincipalID)
	if err != nil {
		h.writeSupportError(w, err, req.CorrelationID)
		return
	}

	writeJSON(w, http.StatusCreated, domain.AttachSupportContextResponse{
		SupportContextID: sc.SupportContextID,
		ExpiresAt:        sc.ExpiresAt,
		EvidenceID:       sc.EvidenceID,
	})
}

// GetSupportContext reads one grant, for the console that displays it and the
// reviewer who reconciles it.
func (h *Handler) GetSupportContext(w http.ResponseWriter, r *http.Request) {
	if h.support == nil {
		writeCoded(w, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"support context is not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, ActionAttachSupportContext) {
		return
	}

	supportContextID := chi.URLParam(r, "supportContextID")
	sc, err := h.support.store.FindSupportContext(r.Context(), supportContextID, tenantID)
	if err != nil {
		writeCoded(w, http.StatusServiceUnavailable, domain.ErrCodeUpstreamUnavailable,
			"could not read the support context")
		return
	}
	if sc == nil {
		writeCoded(w, http.StatusNotFound, domain.ErrCodeBreakGlassRequired, "support context not found")
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

// RevokeSupportContext ends an elevation early.
//
// Guarded by its own, weaker action. Revoking is de-escalation: requiring the
// same grant to END a support session as to START one means the person who
// notices a problem may be unable to stop it.
func (h *Handler) RevokeSupportContext(w http.ResponseWriter, r *http.Request) {
	if h.support == nil {
		writeCoded(w, http.StatusNotImplemented, domain.ErrCodeUpstreamUnavailable,
			"support context is not configured in this deployment")
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, ActionRevokeSupportContext) {
		return
	}

	supportContextID := chi.URLParam(r, "supportContextID")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req domain.RevokeSupportContextRequest
	// An empty body is fine here — a revocation with no stated reason still
	// beats one that did not happen because the caller forgot a payload.
	_ = json.NewDecoder(r.Body).Decode(&req)

	if err := h.support.Revoke(r.Context(), supportContextID, tenantID, req.Reason, callerPrincipalID, correlationID); err != nil {
		if errors.Is(err, domain.ErrSupportContextNotFound) {
			writeCoded(w, http.StatusNotFound, domain.ErrCodeBreakGlassRequired, "support context not found")
			return
		}
		h.log.Error("support context revocation failed", zap.Error(err))
		writeCoded(w, http.StatusInternalServerError, domain.ErrCodeUpstreamUnavailable,
			"could not revoke the support context")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSupportError maps the support service's failures to stable codes.
//
// The mapping is where the stable error catalogue earns its keep: a caller
// refused for a TTL that is too long, a missing approver and a segregation
// conflict all need different responses from the operator, and all three used
// to be indistinguishable message strings.
func (h *Handler) writeSupportError(w http.ResponseWriter, err error, correlationID string) {
	switch {
	case errors.Is(err, sod.ErrConflict):
		writeJSON(w, http.StatusConflict, errorResponse{
			Error:         err.Error(),
			ErrorCode:     domain.ErrCodeSoDConflict,
			CorrelationID: correlationID,
		})
	case errors.Is(err, sod.ErrUnavailable):
		// Fail closed, and say which control could not run. An operator told
		// only "service unavailable" will retry; one told the SoD service is
		// down will go and fix it.
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Error:         "segregation of duties could not be evaluated — refusing to grant",
			ErrorCode:     domain.ErrCodeUpstreamUnavailable,
			CorrelationID: correlationID,
		})
	case errors.Is(err, domain.ErrSupportSelfApproval),
		errors.Is(err, domain.ErrSupportTTLExceeded),
		errors.Is(err, ErrRequestInvalid):
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error:         err.Error(),
			ErrorCode:     domain.ErrCodeBreakGlassRequired,
			CorrelationID: correlationID,
		})
	default:
		h.log.Error("attach support context failed", zap.Error(err))
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error:         "could not attach the support context",
			ErrorCode:     domain.ErrCodeUpstreamUnavailable,
			CorrelationID: correlationID,
		})
	}
}
