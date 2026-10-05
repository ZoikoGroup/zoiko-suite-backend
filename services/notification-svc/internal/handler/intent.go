package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

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

func registerIntentRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/communication-intents", func(r chi.Router) {
		r.Post("/", h.CreateCommunicationIntent)
		r.Post("/versions/{versionID}/validate", h.ValidateIntentVersion)
		r.Post("/versions/{versionID}/approve", h.ApproveIntentVersion)
		r.Post("/versions/{versionID}/publish", h.PublishIntentVersion)
		r.Get("/{intentID}", h.GetCommunicationIntent)
		r.Post("/{intentID}/versions", h.CreateIntentVersion)
		r.Post("/{intentID}/retire", h.RetireCommunicationIntent)
		r.Get("/{intentID}/effective", h.EffectiveIntent)
	})
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

// intentGate is the common front of every intent endpoint: a registry must be wired, and
// the caller must carry a principal and a tenant.
func (h *Handler) intentGate(w http.ResponseWriter, r *http.Request) (principalID string, ok bool) {
	if h.intents == nil {
		writeError(w, http.StatusServiceUnavailable, "intent_registry_unavailable", "the communication intent registry is not configured")
		return "", false
	}
	if principalID, ok = h.requirePrincipal(w, r); !ok {
		return "", false
	}
	if _, ok = h.requireTenant(w, r); !ok {
		return "", false
	}
	return principalID, true
}

type createIntentRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	IntentKey     string `json:"intent_key"`
	DisplayName   string `json:"display_name"`
	DomainOwner   string `json:"domain_owner"`
}

// CreateCommunicationIntent registers the stable identity of a purpose (NCD-01
// POST /communication-intents). The id is assigned by the server and never derived from
// the display name.
func (h *Handler) CreateCommunicationIntent(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.intentGate(w, r)
	if !ok {
		return
	}
	var req createIntentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.IntentKey == "" || req.DisplayName == "" || req.DomainOwner == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, intent_key, display_name and domain_owner are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	out, err := h.intents.CreateIntent(r.Context(), domain.CreateIntentParams{LegalEntityID: req.LegalEntityID, IntentKey: req.IntentKey,
		DisplayName: req.DisplayName, DomainOwner: req.DomainOwner, CreatedByPrincipalID: principalID})
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// authorizeIntent fetches the intent, then authorizes the action against its legal entity
// (fetch-then-authorize-then-use, as everywhere in this service).
func (h *Handler) authorizeIntent(w http.ResponseWriter, r *http.Request, principalID, intentID, action string) (*domain.CommunicationIntent, bool) {
	intent, err := h.intents.GetIntent(r.Context(), intentID)
	if err != nil {
		h.writeIntentError(w, err)
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, intent.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return nil, false
	}
	return intent, true
}

func (h *Handler) authorizeIntentVersion(w http.ResponseWriter, r *http.Request, principalID, versionID, action string) (*domain.IntentVersion, bool) {
	v, err := h.intents.GetIntentVersion(r.Context(), versionID)
	if err != nil {
		h.writeIntentError(w, err)
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, v.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return nil, false
	}
	return v, true
}

// GetCommunicationIntent reads one intent.
func (h *Handler) GetCommunicationIntent(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.intentGate(w, r)
	if !ok {
		return
	}
	intent, ok := h.authorizeIntent(w, r, principalID, chi.URLParam(r, "intentID"), actionView)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, intent)
}

type createIntentVersionRequest struct {
	PurposeClass      string                         `json:"purpose_class"`
	EvidenceClass     string                         `json:"evidence_class"`
	AllowedChannels   []string                       `json:"allowed_channels"`
	MarketingAllowed  bool                           `json:"marketing_allowed"`
	RecordRequirement bool                           `json:"record_requirement"`
	VariableContract  map[string]domain.VariableSpec `json:"variable_contract"`
}

// CreateIntentVersion writes a DRAFT version. A change of purpose class is simply a new
// version, because it changes what may block the message (INV-06).
func (h *Handler) CreateIntentVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.intentGate(w, r)
	if !ok {
		return
	}
	intentID := chi.URLParam(r, "intentID")
	if _, ok := h.authorizeIntent(w, r, principalID, intentID, actionTemplateManage); !ok {
		return
	}
	var req createIntentVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.intents.CreateIntentVersion(r.Context(), domain.CreateIntentVersionParams{IntentID: intentID, PurposeClass: req.PurposeClass,
		EvidenceClass: req.EvidenceClass, AllowedChannels: req.AllowedChannels, MarketingAllowed: req.MarketingAllowed,
		RecordRequirement: req.RecordRequirement, VariableContract: req.VariableContract, CreatedByPrincipalID: principalID})
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ValidateIntentVersion moves a DRAFT version to REVIEW.
func (h *Handler) ValidateIntentVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.intentGate(w, r)
	if !ok {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	if _, ok := h.authorizeIntentVersion(w, r, principalID, versionID, actionTemplateManage); !ok {
		return
	}
	out, err := h.intents.ValidateIntentVersion(r.Context(), versionID)
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ApproveIntentVersion approves a REVIEW version. The creator cannot (maker-checker).
func (h *Handler) ApproveIntentVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.intentGate(w, r)
	if !ok {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	if _, ok := h.authorizeIntentVersion(w, r, principalID, versionID, actionTemplateApprove); !ok {
		return
	}
	out, err := h.intents.ApproveIntentVersion(r.Context(), domain.ApproveIntentVersionParams{VersionID: versionID, ApprovedByPrincipalID: principalID})
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type publishIntentRequest struct {
	// EffectiveFrom is when the version starts to apply. Omitted means now. Never in the
	// past, and later than every version already published for the intent.
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
}

// PublishIntentVersion publishes an APPROVED version, effective-dated.
func (h *Handler) PublishIntentVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.intentGate(w, r)
	if !ok {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	if _, ok := h.authorizeIntentVersion(w, r, principalID, versionID, actionTemplateApprove); !ok {
		return
	}
	var req publishIntentRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.intents.PublishIntentVersion(r.Context(), domain.PublishIntentVersionParams{VersionID: versionID, PublishedByPrincipalID: principalID, EffectiveFrom: req.EffectiveFrom})
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// RetireCommunicationIntent retires an intent: no new versions, and nothing in force from
// then on. Earlier history stays resolvable.
func (h *Handler) RetireCommunicationIntent(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.intentGate(w, r)
	if !ok {
		return
	}
	intentID := chi.URLParam(r, "intentID")
	if _, ok := h.authorizeIntent(w, r, principalID, intentID, actionTemplateApprove); !ok {
		return
	}
	out, err := h.intents.RetireIntent(r.Context(), intentID, principalID)
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// EffectiveIntent resolves the version in force at transaction time `at`, as the platform
// knew it at `known_at` (NCD-01 GET /intents/{id}/effective). Both default to now.
func (h *Handler) EffectiveIntent(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.intentGate(w, r)
	if !ok {
		return
	}
	intentID := chi.URLParam(r, "intentID")
	if _, ok := h.authorizeIntent(w, r, principalID, intentID, actionView); !ok {
		return
	}
	now := time.Now().UTC()
	at, knownAt := now, now
	for name, dst := range map[string]*time.Time{"at": &at, "known_at": &knownAt} {
		if raw := r.URL.Query().Get(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_"+name, name+" must be an RFC3339 timestamp")
				return
			}
			*dst = t.UTC()
		}
	}
	out, err := h.intents.EffectiveIntentVersion(r.Context(), intentID, at, knownAt)
	if err != nil {
		h.writeIntentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
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
