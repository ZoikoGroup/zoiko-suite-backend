package handler

import (
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/channeldecision"
	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/ledger"
)

// POST /v1/channel-decision (ZS-SVC-Y-001 NCD-02 section 5.5): which channels may this
// communication use for this recipient, in order, and why not the others. It sends
// nothing and records nothing. It is advisory: the delivery guard decides again
// immediately before the provider, because suppression, consent and preferences can
// change between the question and the send.
//
// Authorized as NOTIFICATION_SEND on the legal entity, since the answer reveals whether a
// person's address is suppressed or their channels muted.

type channelDecisionRequest struct {
	LegalEntityID        string   `json:"legal_entity_id"`
	RecipientPrincipalID string   `json:"recipient_principal_id"`
	IntentID             string   `json:"intent_id"`
	CommunicationClass   string   `json:"communication_class"`
	Channels             []string `json:"channels"`
}

// ChannelDecision answers the question.
func (h *Handler) ChannelDecision(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req channelDecisionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RecipientPrincipalID == "" || (req.LegalEntityID == "" && req.IntentID == "") {
		writeError(w, http.StatusBadRequest, "missing_fields", "recipient_principal_id and a legal_entity_id (or an intent_id) are required")
		return
	}

	facts := channeldecision.Facts{Class: req.CommunicationClass, Requested: req.Channels, Now: time.Now().UTC()}

	legalEntity := req.LegalEntityID
	if req.IntentID != "" {
		if h.intents == nil {
			writeError(w, http.StatusServiceUnavailable, "intent_registry_unavailable", "the communication intent registry is not configured")
			return
		}
		now := time.Now().UTC()
		iv, err := h.intents.EffectiveIntentVersion(r.Context(), req.IntentID, now, now)
		if err != nil {
			h.writeIntentError(w, err)
			return
		}
		if req.LegalEntityID != "" && req.LegalEntityID != iv.LegalEntityID {
			writeError(w, http.StatusBadRequest, "legal_entity_mismatch", "the intent belongs to a different legal entity")
			return
		}
		legalEntity = iv.LegalEntityID
		if req.CommunicationClass != "" && req.CommunicationClass != iv.PurposeClass {
			writeError(w, http.StatusBadRequest, "communication_class_conflict",
				"the communication intent decides the purpose class ("+iv.PurposeClass+"); do not supply a different one")
			return
		}
		facts.Class, facts.IntentChannels = iv.PurposeClass, iv.AllowedChannels
	}
	if facts.Class == "" {
		facts.Class = "T0" // how every direct send was treated before classes existed
	}
	if !domain.ValidDirectPathClass(facts.Class) {
		writeError(w, http.StatusUnprocessableEntity, "intent_class_not_direct",
			"class "+facts.Class+" is not available on the direct send path; marketing and lifecycle mail use the ledger pipeline")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntity, actionSend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if h.preferences != nil {
		p, err := h.preferences.GetPreferences(r.Context(), req.RecipientPrincipalID)
		switch {
		case err == nil:
			facts.Prefs = p
		case errors.Is(err, domain.ErrPreferencesNotFound):
		default:
			h.log.Error("channel decision: preferences unreadable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
	}

	h.emailFacts(r, tenantID, principalID, req.RecipientPrincipalID, &facts)

	writeJSON(w, http.StatusOK, channeldecision.Decide(facts))
}

// emailFacts resolves the recipient's email endpoint and its suppression state. Anything
// that cannot be established makes the email channel unavailable, never assumed fine.
func (h *Handler) emailFacts(r *http.Request, tenantID, callerID, recipientID string, f *channeldecision.Facts) {
	addr, _, err := h.resolveRecipient(r.Context(), tenantID, callerID,
		domain.SendNotificationRequest{RecipientPrincipalID: recipientID, Channel: domain.ChannelEmail})
	if err != nil {
		f.EmailUnavailable = err.Error()
		return
	}
	if h.suppressions == nil {
		f.EmailUnavailable = "suppression state is not available"
		return
	}
	stream, ok := ledger.StreamForDirectClass(ledger.CommunicationClass(f.Class))
	if !ok {
		f.EmailUnavailable = "no sender stream for class " + f.Class
		return
	}
	suppressed, reason, err := h.suppressions.IsEmailSuppressed(r.Context(), tenantID, addr, stream, ledger.CommunicationClass(f.Class))
	if err != nil {
		f.EmailUnavailable = "suppression state could not be read"
		return
	}
	if suppressed {
		if reason == "" {
			reason = "suppressed"
		}
		f.EmailSuppressed = reason
	}
}
