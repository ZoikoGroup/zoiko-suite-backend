package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"zoiko.io/notification-svc/internal/domain"
)

// ZS-SVC-Y-001 NCD-01 API (section 4.5): the communication intent registry.
//
//	POST /v1/communication-intents                        create the stable identity
//	GET  /v1/communication-intents/{intentID}             read it
//	POST /v1/communication-intents/{intentID}/versions    write a DRAFT version
//	POST /v1/communication-intents/versions/{id}/validate | approve | publish
//	POST /v1/communication-intents/{intentID}/retire
//	GET  /v1/communication-intents/{intentID}/effective?at=&known_at=
//
// Authorization reuses the template roles: the people who own wording (TEMPLATE_MANAGE)
// and the independent approvers (TEMPLATE_APPROVE) are the same two roles the intent
// registry needs, and a new action would have to be granted everywhere for no gain.

// IntentStore is the intent registry's persistence boundary.
type IntentStore interface {
	CreateIntent(ctx context.Context, p domain.CreateIntentParams) (*domain.CommunicationIntent, error)
	GetIntent(ctx context.Context, intentID string) (*domain.CommunicationIntent, error)
	CreateIntentVersion(ctx context.Context, p domain.CreateIntentVersionParams) (*domain.IntentVersion, error)
	GetIntentVersion(ctx context.Context, versionID string) (*domain.IntentVersion, error)
	ValidateIntentVersion(ctx context.Context, versionID string) (*domain.IntentVersion, error)
	ApproveIntentVersion(ctx context.Context, p domain.ApproveIntentVersionParams) (*domain.IntentVersion, error)
	PublishIntentVersion(ctx context.Context, p domain.PublishIntentVersionParams) (*domain.IntentVersion, error)
	RetireIntent(ctx context.Context, intentID, actor string) (*domain.CommunicationIntent, error)
	EffectiveIntentVersion(ctx context.Context, intentID string, at, knownAt time.Time) (*domain.IntentVersion, error)
}

func (h *Handler) writeIntentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrIntentNotFound):
		writeError(w, http.StatusNotFound, "intent_not_found", err.Error())
	case errors.Is(err, domain.ErrIntentVersionNotFound):
		writeError(w, http.StatusNotFound, "intent_version_not_found", err.Error())
	case errors.Is(err, domain.ErrIntentNotEffective):
		writeError(w, http.StatusConflict, "intent_not_effective", err.Error())
	case errors.Is(err, domain.ErrIntentKeyTaken):
		writeError(w, http.StatusConflict, "intent_key_taken", err.Error())
	case errors.Is(err, domain.ErrIntentRetired):
		writeError(w, http.StatusConflict, "intent_retired", err.Error())
	case errors.Is(err, domain.ErrIntentVersionNotDraft):
		writeError(w, http.StatusConflict, "not_draft", err.Error())
	case errors.Is(err, domain.ErrIntentVersionNotReview):
		writeError(w, http.StatusConflict, "not_review", err.Error())
	case errors.Is(err, domain.ErrIntentVersionNotApproved):
		writeError(w, http.StatusConflict, "not_approved", err.Error())
	case errors.Is(err, domain.ErrIntentVersionSelfApproval):
		writeError(w, http.StatusForbidden, "self_approval_forbidden", err.Error())
	case errors.Is(err, domain.ErrIntentEffectiveFromInvalid):
		writeError(w, http.StatusBadRequest, "invalid_effective_from", err.Error())
	case errors.Is(err, domain.ErrIntentInvalid):
		writeError(w, http.StatusBadRequest, "invalid_intent", err.Error())
	case errors.Is(err, domain.ErrSubjectInvalid):
		writeError(w, http.StatusBadRequest, "invalid_subject", err.Error())
	case errors.Is(err, domain.ErrTemplateVariableInvalid):
		writeError(w, http.StatusBadRequest, "template_variable_invalid", err.Error())
	default:
		h.log.Error("intent store error")
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// isIntentBindingError reports the refusals that come from a template's binding to an
// intent: the intent is missing, retired, not yet in force, or the wording does not
// conform to its contract. They are the caller's to fix, not an outage.
func isIntentBindingError(err error) bool {
	return errors.Is(err, domain.ErrIntentNotFound) || errors.Is(err, domain.ErrIntentRetired) ||
		errors.Is(err, domain.ErrIntentInvalid) || errors.Is(err, domain.ErrIntentNotEffective) ||
		errors.Is(err, domain.ErrTemplateVariableInvalid)
}

// enforceIntent applies the intent version in force now to a send that uses a template
// bound to that intent. It writes the refusal itself and reports whether to continue.
//
//   - no registry wired, or no version in force: the send is refused (fail closed);
//   - the channel must be one the intent allows (NCD-011);
//   - the purpose class comes from the intent, the sender cannot choose a different one,
//     and a class the direct path does not offer (marketing, lifecycle) is refused here
//     rather than sent without its stream identity and unsubscribe headers;
//   - the template must still conform to the contract, and every variable supplied must
//     satisfy its declared type, length and required-ness (NCD-004).
//
// Only the variables the template USES are held to the contract: a contract may govern
// more than one template.
func (h *Handler) enforceIntent(w http.ResponseWriter, r *http.Request, intentID string, published *domain.TemplateVersion,
	req *domain.SendNotificationRequest) (*domain.IntentVersion, bool) {

	if h.intents == nil {
		writeError(w, http.StatusServiceUnavailable, "intent_registry_unavailable",
			"this template is bound to a communication intent, and the registry is not configured")
		return nil, false
	}
	now := time.Now().UTC()
	iv, err := h.intents.EffectiveIntentVersion(r.Context(), intentID, now, now)
	if err != nil {
		h.writeIntentError(w, err)
		return nil, false
	}
	if !iv.ChannelAllowed(req.Channel) {
		writeError(w, http.StatusUnprocessableEntity, "no_compliant_channel",
			"NCD-011 NO_COMPLIANT_CHANNEL: the communication intent does not allow the "+req.Channel+" channel")
		return nil, false
	}
	if req.CommunicationClass != "" && req.CommunicationClass != iv.PurposeClass {
		writeError(w, http.StatusBadRequest, "communication_class_conflict",
			"the communication intent decides the purpose class ("+iv.PurposeClass+"); do not supply a different one")
		return nil, false
	}
	if !domain.ValidDirectPathClass(iv.PurposeClass) {
		writeError(w, http.StatusUnprocessableEntity, "intent_class_not_direct",
			"the intent's purpose class "+iv.PurposeClass+" is not available on the direct send path; marketing and lifecycle mail use the ledger pipeline")
		return nil, false
	}
	if err := domain.CheckTemplateVariables(published.VariableSchema, iv.VariableContract); err != nil {
		h.writeIntentError(w, err)
		return nil, false
	}
	used := make(map[string]domain.VariableSpec, len(published.VariableSchema))
	for _, name := range published.VariableSchema {
		used[name] = iv.VariableContract[name]
	}
	if err := domain.CheckVariables(used, req.Variables); err != nil {
		h.writeIntentError(w, err)
		return nil, false
	}
	req.CommunicationClass = iv.PurposeClass
	return iv, true
}
