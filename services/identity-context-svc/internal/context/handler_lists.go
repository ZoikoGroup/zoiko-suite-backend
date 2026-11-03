package context

import (
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/store"
)

type principalsListResponse struct {
	Items      []store.PrincipalDetail `json:"principals"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

type authenticationEventsListResponse struct {
	Items      []domain.AuthenticationEvent `json:"events"`
	NextCursor string                       `json:"next_cursor,omitempty"`
}

// listLimit parses the limit query param, defaulting to 50 and capping at 200.
func listLimit(r *http.Request) int {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 200 {
				n = 200
			}
			limit = n
		}
	}
	return limit
}

// ListPrincipals is the console read model for the People tab: a page of the
// caller's tenant's principals. Authorized at IDENTITY_PRINCIPAL_READ.
func (h *Handler) ListPrincipals(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "list_principals", tenantID); !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityPrincipalRead) {
		return
	}

	limit := listLimit(r)
	items, _, err := h.principals.ListPrincipals(r.Context(), tenantID, "", "", 0, limit)
	if err != nil {
		h.log.Error("list principals failed", zap.String("tenant_id", tenantID), zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "principals list failed")
		return
	}
	if items == nil {
		items = []store.PrincipalDetail{}
	}
	writeJSON(w, http.StatusOK, principalsListResponse{Items: items})
}

// ListAuthenticationEvents is the console read model for the Sign-in events
// tab: a page of the caller's tenant's authentication events, newest first.
// Authorized at IDENTITY_AUTH_EVENTS_READ.
func (h *Handler) ListAuthenticationEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	callerPrincipalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.throttle(w, r, "list_authentication_events", tenantID); !ok {
		return
	}
	if !h.authorize(w, r, callerPrincipalID, tenantID, IdentityAuthEventsRead) {
		return
	}

	items, nextCursor, err := h.principals.ListAuthenticationEvents(r.Context(), tenantID, r.URL.Query().Get("cursor"), listLimit(r))
	if err != nil {
		h.log.Error("list authentication events failed", zap.String("tenant_id", tenantID), zap.Error(err))
		writeError(w, r, http.StatusInternalServerError, "authentication events list failed")
		return
	}
	if items == nil {
		items = []domain.AuthenticationEvent{}
	}
	writeJSON(w, http.StatusOK, authenticationEventsListResponse{Items: items, NextCursor: nextCursor})
}
