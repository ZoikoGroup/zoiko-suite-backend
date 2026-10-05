package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/telemetry"
)

type resendRequest struct {
	Reason string `json:"reason"`
}

// ResendNotification makes one explicit, reasoned resend of a concluded
// notification (ZS-SVC-Y-001 §3.4). POST /v1/notifications/{id}/resend
//
// §3.4: "User-initiated 'resend' creates an explicit resend reason and
// preserves the original attempt/evidence chain." So a resend is one more
// governed attempt on the SAME communication — recorded in the same attempt
// chain with origin "resend" and its reason — not a new, unrelated
// notification that would split the evidence in two.
//
// Refused while the notification is PENDING (still being attempted) or
// PENDING_UNKNOWN: an ambiguous attempt must be resolved against the provider
// first, because resending it is exactly the blind second material send §6.2
// forbids. The code returned for that case is the spec's own NCD-014.
//
// Authorized as NOTIFICATION_SEND on the notification's legal entity: a resend
// is an act of sending, by whoever may send for that entity.
func (h *Handler) ResendNotification(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req resendRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", domain.ErrResendReasonRequired.Error())
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

	switch existing.Status {
	case domain.StatusPendingUnknown:
		writeError(w, http.StatusConflict, "DELIVERY_OUTCOME_UNKNOWN",
			"NCD-014: the last attempt's outcome is ambiguous and must be resolved "+
				"(POST /{id}/resolve-delivery-outcome) before it may be resent")
		return
	case domain.StatusPending:
		writeError(w, http.StatusConflict, "delivery_in_progress",
			"this notification is still being attempted; it can be resent once it has concluded")
		return
	}

	n, err := h.store.BeginResend(r.Context(), id, tenantID, principalID, req.Reason, time.Now().UTC())
	if errors.Is(err, domain.ErrResendLedgerOwned) {
		writeError(w, http.StatusConflict, "ledger_communication_not_resendable", err.Error())
		return
	}
	if errors.Is(err, domain.ErrNotResendable) {
		// A race: another resend (or a resolution) moved it first.
		writeError(w, http.StatusConflict, "not_resendable", err.Error())
		return
	}
	if err != nil {
		h.log.Error("failed to reopen notification for resend", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// The address is a delivery snapshot and is reused as it is. Only an
	// address the original attempt never obtained is resolved now — filling
	// an absence, never rewriting where an earlier attempt went.
	var outcome domain.DeliveryOutcome
	if domain.ChannelNeedsAddress(n.Channel) && n.RecipientAddress == "" {
		addr, source, resolveErr := h.resolveRecipient(r.Context(), tenantID, principalID, domain.SendNotificationRequest{
			RecipientPrincipalID: n.RecipientPrincipalID, Channel: n.Channel,
		})
		if resolveErr != nil {
			outcome.Reason = "recipient resolution failed: " + resolveErr.Error()
		} else if err := h.store.SetRecipientAddress(r.Context(), n.NotificationID, tenantID, addr, source); err != nil {
			outcome.Reason = "resolved a recipient address but could not record it: " + err.Error()
		} else {
			n.RecipientAddress, n.RecipientAddressSource = addr, source
		}
	}
	if outcome.Reason == "" {
		var delivered bool
		if outcome, delivered = h.submitAndDeliver(w, r, n, tenantID, telemetry.OriginResend); !delivered {
			return
		}
	}

	h.recordAttemptOutcome(w, r, n, outcome, domain.AttemptMeta{
		Origin:           domain.AttemptOriginResend,
		ProviderName:     outcome.ProviderName,
		Retryable:        outcome.Retryable,
		ResendReason:     req.Reason,
		ActorPrincipalID: principalID,
	}, getCorrelationID(r), tenantID, http.StatusOK)
}
