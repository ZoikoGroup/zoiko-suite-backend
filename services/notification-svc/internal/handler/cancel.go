package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
)

type cancelRequest struct {
	Reason string `json:"reason"`
}

// CancelNotification withdraws a queued communication before it is submitted (ZS-SVC-Y-001
// 6.6, "source workflow canceled before submit"). POST /v1/notifications/{id}/cancel
//
// Only a QUEUED communication can be withdrawn. One that has been handed to a provider, is
// being handed over now, has been claimed by a worker or has concluded is refused with 409:
// a cancellation must never describe a message that was in fact sent. The cancellation, its
// reason and who made it are kept on the record and announced, never deleted.
//
// Authorized as NOTIFICATION_SEND on the legal entity: whoever may send for it may withdraw.
func (h *Handler) CancelNotification(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req cancelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", domain.ErrCancelReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	existing, err := h.store.GetNotification(r.Context(), id)
	if errors.Is(err, domain.ErrNotificationNotFound) {
		writeError(w, http.StatusNotFound, "notification_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch notification", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionSend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	n, err := h.store.CancelNotification(r.Context(), id, tenantID, principalID, req.Reason, time.Now().UTC())
	switch {
	case errors.Is(err, domain.ErrCancelNotAllowed):
		writeError(w, http.StatusConflict, "not_cancellable", err.Error())
		return
	case errors.Is(err, domain.ErrNotificationNotFound):
		writeError(w, http.StatusNotFound, "notification_not_found", "")
		return
	case err != nil:
		h.log.Error("failed to cancel notification", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, n)
}
