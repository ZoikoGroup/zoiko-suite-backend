package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
)

// EvidenceStore reads the normalized delivery evidence of a direct send (NCD-04 7.1).
type EvidenceStore interface {
	ListDeliveryEvidence(ctx context.Context, notificationID string) ([]domain.DeliveryEvidence, error)
}

// ListEvidence returns the normalized provider facts recorded for one notification, with
// each fact's strength and stated limits. GET /v1/notifications/{id}/evidence
//
// None of these facts means a person saw or acknowledged the message; the response says so,
// because the commonest misuse of delivery telemetry is reading it as proof of notice.
// Authorized as NOTIFICATION_VIEW on the notification's legal entity, like the attempts.
func (h *Handler) ListEvidence(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.evidence == nil {
		writeError(w, http.StatusServiceUnavailable, "evidence_unavailable", "delivery evidence is not configured")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	notification, err := h.store.GetNotification(r.Context(), id)
	if errors.Is(err, domain.ErrNotificationNotFound) {
		writeError(w, http.StatusNotFound, "notification_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch notification", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, notification.LegalEntityID, actionView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	facts, err := h.evidence.ListDeliveryEvidence(r.Context(), id)
	if err != nil {
		h.log.Error("failed to list delivery evidence", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"notification_id": id,
		"evidence":        facts,
		"notice":          "Provider telemetry establishes only what each fact states. None of it proves a person saw, read or acknowledged the message.",
	})
}
