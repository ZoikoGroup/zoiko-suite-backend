package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/ncd"
)

// ZS-SVC-Y-001 control-plane routes (NCD-01 … NCD-05 and §10.1).
//
// Every route follows the service's discipline: identity and tenant first,
// then the stored object is fetched, then the caller is authorized against
// THAT object's legal entity, then the command runs. Authorization reuses the
// six actions this service already defines — each is granted somewhere in the
// estate's RBAC seed — rather than inventing names nothing grants.

// PlatformScopeID is the legal-entity scope platform configuration (provider
// bindings) is authorized against — the same constant authorization-svc uses.
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// NCDHandler serves the control plane.
type NCDHandler struct {
	svc   *ncd.Service
	authz AuthZClient
	log   *zap.Logger
}

// NewNCDHandler wires the handler.
func NewNCDHandler(svc *ncd.Service, authz AuthZClient, log *zap.Logger) *NCDHandler {
	if log == nil {
		log = zap.NewNop()
	}
	return &NCDHandler{svc: svc, authz: authz, log: log}
}

// RegisterNCDRoutes mounts the plane under /v1.
func RegisterNCDRoutes(r chi.Router, h *NCDHandler) {
	r.Group(func(r chi.Router) {
		// NCD-01 (§4.5)
		r.Post("/v1/communication-intents", h.createIntent)
		r.Get("/v1/communication-intents/{id}", h.getIntent)
		r.Post("/v1/communication-intents/{id}/versions", h.createIntentVersion)
		r.Post("/v1/communication-intents/{id}/activate", h.activateIntent)
		r.Post("/v1/communication-intents/{id}/retire", h.retireIntent)
		r.Get("/v1/communication-intents/{id}/templates", h.listTemplates)
		r.Post("/v1/templates", h.createTemplate)
		r.Get("/v1/templates/{id}", h.getTemplate)
		r.Post("/v1/templates/{id}/validate", h.templateAction("validate"))
		r.Post("/v1/templates/{id}/approve", h.templateAction("approve"))
		r.Post("/v1/templates/{id}/reject", h.templateAction("reject"))
		r.Post("/v1/templates/{id}/publish", h.templateAction("publish"))
		r.Post("/v1/templates/{id}/retire", h.templateAction("retire"))
		r.Post("/v1/render-previews", h.renderPreview)
		r.Get("/v1/intents/{id}/effective", h.effective)

		// NCD-02 (§5.5)
		r.Post("/v1/recipient-resolution", h.recipientResolution)
		r.Post("/v1/channel-decision", h.channelDecision)
		r.Get("/v1/suppressions", h.listSuppressions)
		r.Post("/v1/suppressions", h.addSuppression)
		r.Post("/v1/suppressions/{id}/lift", h.liftSuppression)
		r.Post("/v1/preferences", h.setPreference)
		r.Get("/v1/preferences/me", h.getPreference)
		r.Post("/v1/revalidate", h.revalidate)

		// §10.1 communications
		r.Post("/v1/communications", h.createCommunication)
		r.Get("/v1/communications/{id}", h.getCommunication)
		r.Post("/v1/communications/{id}/prepare", h.prepare)
		r.Post("/v1/communications/{id}/dispatch", h.dispatch)
		r.Post("/v1/communications/{id}/cancel", h.cancel)
		r.Post("/v1/communications/{id}/resend", h.resend)
		r.Post("/v1/communications/{id}/attempts/{attempt_id}/resolve", h.resolveAttempt)
		r.Post("/v1/communications/{id}/misdelivery", h.misdelivery)
		r.Get("/v1/evidence/{id}", h.evidence)
		r.Post("/v1/acknowledgments", h.acknowledge)
		r.Post("/v1/provider-events/{binding}", h.providerEvents)

		// NCD-03 / NCD-04 operations
		r.Get("/v1/provider-bindings", h.listBindings)
		r.Post("/v1/provider-bindings/{id}/circuit", h.setCircuit)
		r.Post("/v1/streams/{stream}", h.setStream)
		r.Get("/v1/reputation", h.reputation)
		r.Get("/v1/exceptions", h.exceptions)
		r.Post("/v1/bulk-sends", h.previewBulk)
		r.Post("/v1/bulk-sends/{id}/dispatch", h.dispatchBulk)
		r.Get("/v1/approvals/{id}", h.getApproval)
		r.Post("/v1/approvals/{id}/approve", h.decideApproval(true))
		r.Post("/v1/approvals/{id}/reject", h.decideApproval(false))

		// NCD-05 (§8)
		r.Post("/v1/regulated-notices", h.createNotice)
		r.Get("/v1/regulated-notices/{id}", h.getNotice)
		r.Post("/v1/regulated-notices/{id}/manual-evidence", h.manualEvidence)
		r.Post("/v1/regulated-notices/{id}/record-declaration", h.declareRecord)
	})
}

// ── plumbing ────────────────────────────────────────────────────────────────

func (h *NCDHandler) actor(w http.ResponseWriter, r *http.Request) (ncd.Actor, bool) {
	p := r.Header.Get("X-Principal-Id")
	if p == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return ncd.Actor{}, false
	}
	t := svcmiddleware.TenantFromContext(r.Context())
	if t == "" {
		writeError(w, http.StatusUnauthorized, "tenant_missing", "X-Tenant-Id is required")
		return ncd.Actor{}, false
	}
	return ncd.Actor{TenantID: t, PrincipalID: p, CorrelationID: getCorrelationID(r)}, true
}

func (h *NCDHandler) allow(w http.ResponseWriter, r *http.Request, a ncd.Actor, legalEntityID, action string) bool {
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_legal_entity", "a legal entity scope is required for this action")
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), a.PrincipalID, legalEntityID, action); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", err.Error())
		} else {
			writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		}
		return false
	}
	return true
}

// headerEntity is the legal entity a tenant-wide operation is authorized on:
// the query parameter, else the envelope's X-Legal-Entity-Id.
func headerEntity(r *http.Request) string {
	if le := r.URL.Query().Get("legal_entity_id"); le != "" {
		return le
	}
	return r.Header.Get("X-Legal-Entity-Id")
}

func (h *NCDHandler) fail(w http.ResponseWriter, err error) {
	e := ncd.AsError(err)
	status := http.StatusServiceUnavailable
	switch e.Kind {
	case ncd.KindInvalid:
		status = http.StatusBadRequest
	case ncd.KindForbidden:
		status = http.StatusForbidden
	case ncd.KindNotFound:
		status = http.StatusNotFound
	case ncd.KindConflict:
		status = http.StatusConflict
	case ncd.KindRefused:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error_code": e.Code, "error_message": e.Detail, "reason": e.Refusal.Name, "reason_code": e.Refusal.Code,
		})
		return
	}
	if status == http.StatusServiceUnavailable {
		h.log.Error("ncd operation failed", zap.Error(err))
	}
	writeError(w, status, e.Code, e.Detail)
}

func (h *NCDHandler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 && r.Body != nil {
		// An empty body is an empty command object.
		if b, _ := io.ReadAll(io.LimitReader(r.Body, 1)); len(b) == 0 {
			return true
		}
	}
	return decodeJSON(w, r, dst)
}

func (h *NCDHandler) intentEntity(w http.ResponseWriter, r *http.Request, a ncd.Actor, id string) (string, bool) {
	le, err := h.svc.IntentEntity(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return "", false
	}
	return le, true
}

func (h *NCDHandler) commFor(w http.ResponseWriter, r *http.Request, a ncd.Actor, id, action string) (*ncd.Communication, bool) {
	c, err := h.svc.Communication(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	return c, h.allow(w, r, a, c.LegalEntityID, action)
}

// ── NCD-01 ──────────────────────────────────────────────────────────────────

func (h *NCDHandler) createIntent(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.IntentInput
	if !decodeJSON(w, r, &in) || !h.allow(w, r, a, in.LegalEntityID, actionTemplateManage) {
		return
	}
	i, err := h.svc.CreateIntent(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, i)
}

func (h *NCDHandler) getIntent(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	le, ok := h.intentEntity(w, r, a, id)
	if !ok || !h.allow(w, r, a, le, actionTemplateManage) {
		return
	}
	v, _ := strconv.Atoi(r.URL.Query().Get("version"))
	i, all, err := h.svc.GetIntent(r.Context(), a, id, v)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"intent": i, "versions": all})
}

func (h *NCDHandler) createIntentVersion(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var in ncd.IntentInput
	if !decodeJSON(w, r, &in) {
		return
	}
	le, ok := h.intentEntity(w, r, a, id)
	if !ok || !h.allow(w, r, a, le, actionTemplateManage) {
		return
	}
	i, err := h.svc.CreateIntentVersion(r.Context(), a, id, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, i)
}

func (h *NCDHandler) activateIntent(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var in struct {
		Version       int        `json:"version"`
		EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Version < 1 {
		writeError(w, http.StatusBadRequest, "missing_fields", "version is required")
		return
	}
	le, ok := h.intentEntity(w, r, a, id)
	if !ok || !h.allow(w, r, a, le, actionTemplateApprove) {
		return
	}
	i, err := h.svc.ActivateIntent(r.Context(), a, id, in.Version, in.EffectiveFrom)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, i)
}

func (h *NCDHandler) retireIntent(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	le, ok := h.intentEntity(w, r, a, id)
	if !ok || !h.allow(w, r, a, le, actionTemplateApprove) {
		return
	}
	i, err := h.svc.RetireIntent(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, i)
}

func (h *NCDHandler) listTemplates(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	le, ok := h.intentEntity(w, r, a, id)
	if !ok || !h.allow(w, r, a, le, actionTemplateManage) {
		return
	}
	ts, err := h.svc.ListTemplates(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": ts})
}

func (h *NCDHandler) createTemplate(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.TemplateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	le, ok := h.intentEntity(w, r, a, in.IntentID)
	if !ok || !h.allow(w, r, a, le, actionTemplateManage) {
		return
	}
	tv, err := h.svc.CreateTemplate(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, tv)
}

func (h *NCDHandler) getTemplate(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	tv, err := h.svc.GetTemplate(r.Context(), a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.allow(w, r, a, tv.LegalEntityID, actionTemplateManage) {
		return
	}
	writeJSON(w, http.StatusOK, tv)
}

func (h *NCDHandler) templateAction(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := h.actor(w, r)
		if !ok {
			return
		}
		id := chi.URLParam(r, "id")
		var in struct {
			EffectiveFrom *time.Time `json:"effective_from,omitempty"`
		}
		if !h.decode(w, r, &in) {
			return
		}
		le, err := h.svc.TemplateEntity(r.Context(), a, id)
		if err != nil {
			h.fail(w, err)
			return
		}
		action := actionTemplateApprove
		if kind == "validate" {
			action = actionTemplateManage
		}
		if !h.allow(w, r, a, le, action) {
			return
		}
		var tv *ncd.TemplateVersion
		switch kind {
		case "validate":
			tv, err = h.svc.ValidateTemplate(r.Context(), a, id)
		case "approve":
			tv, err = h.svc.ApproveTemplate(r.Context(), a, id)
		case "reject":
			tv, err = h.svc.RejectTemplate(r.Context(), a, id)
		case "publish":
			tv, err = h.svc.PublishTemplate(r.Context(), a, id, in.EffectiveFrom)
		case "retire":
			tv, err = h.svc.RetireTemplate(r.Context(), a, id)
		}
		if err != nil {
			h.fail(w, err)
			return
		}
		status := http.StatusOK
		if kind == "validate" && tv.ValidationReport != nil && !tv.ValidationReport.Passed {
			// The report is the answer; 422 so a caller cannot mistake a
			// failed validation for a passed one.
			status = http.StatusUnprocessableEntity
		}
		writeJSON(w, status, tv)
	}
}

func (h *NCDHandler) renderPreview(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.PreviewInput
	if !decodeJSON(w, r, &in) {
		return
	}
	le, err := h.svc.TemplateEntity(r.Context(), a, in.TemplateVersionID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.allow(w, r, a, le, actionTemplateManage) {
		return
	}
	p, err := h.svc.RenderPreview(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *NCDHandler) effective(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	now := time.Now().UTC()
	txTime, knownAt := now, now
	for name, dst := range map[string]*time.Time{"transaction_time": &txTime, "knowledge_time": &knownAt} {
		if raw := r.URL.Query().Get(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_time", name+" must be RFC 3339")
				return
			}
			*dst = t.UTC()
		}
	}
	le, err := h.svc.IntentEntity(r.Context(), a, id)
	if err != nil {
		var e *ncd.Error
		if errors.As(err, &e) && e.Kind == ncd.KindNotFound {
			h.fail(w, ncd.Refused(ncd.Refuse(ncd.NCD001IntentNotFound, "no intent "+id)))
			return
		}
		h.fail(w, err)
		return
	}
	if !h.allow(w, r, a, le, actionView) {
		return
	}
	set, err := h.svc.Effective(r.Context(), a, id, txTime, knownAt)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// ── NCD-02 ──────────────────────────────────────────────────────────────────

func (h *NCDHandler) recipientResolution(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.RecipientInput
	if !decodeJSON(w, r, &in) {
		return
	}
	le, ok := h.intentEntity(w, r, a, in.IntentID)
	if !ok || !h.allow(w, r, a, le, actionSend) {
		return
	}
	p, err := h.svc.ResolveRecipient(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *NCDHandler) channelDecision(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.DecisionRequest
	if !decodeJSON(w, r, &in) {
		return
	}
	p, err := h.svc.GetPlan(r.Context(), a, in.RecipientPlanID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.allow(w, r, a, p.LegalEntityID, actionSend) {
		return
	}
	d, err := h.svc.ChannelDecision(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *NCDHandler) listSuppressions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	q := ncd.SuppressionQuery{PrincipalID: r.URL.Query().Get("principal_id"), Channel: r.URL.Query().Get("channel"),
		Address: r.URL.Query().Get("endpoint"), Purpose: r.URL.Query().Get("purpose"), ActiveOnly: r.URL.Query().Get("active") != "false"}
	// A principal may read their own suppressions; anything else is the
	// operator list and needs the suppression grant.
	if !(q.PrincipalID == a.PrincipalID && q.Address == "") && !h.allow(w, r, a, headerEntity(r), actionSuppressionManage) {
		return
	}
	list, err := h.svc.ListSuppressions(r.Context(), a, q)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suppressions": list})
}

func (h *NCDHandler) addSuppression(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.SuppressionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if !in.SelfServiceSuppression(a.PrincipalID) && !h.allow(w, r, a, headerEntity(r), actionSuppressionManage) {
		return
	}
	s, created, err := h.svc.AddSuppression(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, s)
}

func (h *NCDHandler) liftSuppression(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.LiftInput
	if !decodeJSON(w, r, &in) || !h.allow(w, r, a, headerEntity(r), actionSuppressionManage) {
		return
	}
	s, appr, err := h.svc.LiftSuppression(r.Context(), a, chi.URLParam(r, "id"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	if appr != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"approval": appr, "suppression": s,
			"message": "reactivation of this suppression needs a second principal's approval (§7.3)"})
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *NCDHandler) setPreference(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.PreferenceInput
	if !decodeJSON(w, r, &in) {
		return
	}
	p, err := h.svc.SetPreference(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *NCDHandler) getPreference(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetPreference(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *NCDHandler) revalidate(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in struct {
		CommunicationID string `json:"communication_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if _, ok := h.commFor(w, r, a, in.CommunicationID, actionSend); !ok {
		return
	}
	c, d, err := h.svc.Revalidate(r.Context(), a, in.CommunicationID)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"communication": c, "channel_decision": d})
}

// ── §10.1 communications ────────────────────────────────────────────────────

func (h *NCDHandler) createCommunication(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.CommunicationInput
	if !decodeJSON(w, r, &in) || !h.allow(w, r, a, in.LegalEntityID, actionSend) {
		return
	}
	c, created, err := h.svc.CreateCommunication(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !created {
		// NP-21 / NCD-019: the same logical communication already exists.
		writeJSON(w, http.StatusOK, map[string]any{"communication": c, "replayed": true,
			"reason_code": ncd.NCD019DuplicateCommunication, "reason": ncd.ReasonNames[ncd.NCD019DuplicateCommunication]})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"communication": c, "replayed": false})
}

func (h *NCDHandler) getCommunication(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	c, err := h.svc.Communication(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if c.RecipientPrincipalID != a.PrincipalID && !h.allow(w, r, a, c.LegalEntityID, actionView) {
		return
	}
	v, err := h.svc.GetCommunication(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *NCDHandler) prepare(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, ok := h.commFor(w, r, a, id, actionSend); !ok {
		return
	}
	p, err := h.svc.Prepare(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *NCDHandler) dispatch(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, ok := h.commFor(w, r, a, id, actionSend); !ok {
		return
	}
	c, job, err := h.svc.Dispatch(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"communication": c, "job": job})
}

func (h *NCDHandler) cancel(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var in struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if _, ok := h.commFor(w, r, a, id, actionSend); !ok {
		return
	}
	c, n, err := h.svc.Cancel(r.Context(), a, id, in.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"communication": c, "jobs_cancelled": n})
}

func (h *NCDHandler) resend(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var in ncd.ResendInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if _, ok := h.commFor(w, r, a, id, actionSend); !ok {
		return
	}
	job, appr, err := h.svc.Resend(r.Context(), a, id, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	if appr != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"approval": appr,
			"message": "a resend of a regulated, mandatory or security communication needs a second principal's approval (§11.3)"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (h *NCDHandler) resolveAttempt(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var in ncd.ResolveInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if _, ok := h.commFor(w, r, a, id, actionResolveOutcome); !ok {
		return
	}
	att, err := h.svc.ResolveUnknown(r.Context(), a, id, chi.URLParam(r, "attempt_id"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, att)
}

func (h *NCDHandler) misdelivery(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	var in struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if _, ok := h.commFor(w, r, a, id, actionResolveOutcome); !ok {
		return
	}
	c, err := h.svc.Misdelivery(r.Context(), a, id, in.Reason)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *NCDHandler) evidence(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, ok := h.commFor(w, r, a, id, actionView); !ok {
		return
	}
	b, err := h.svc.Evidence(r.Context(), a, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *NCDHandler) acknowledge(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.AckInput
	if !decodeJSON(w, r, &in) {
		return
	}
	// No grant: the acknowledging actor is the authenticated recipient, and
	// the service refuses anyone else (INV-21).
	ack, err := h.svc.Acknowledge(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ack)
}

// providerEvents is POST /v1/provider-events/{binding}. It is exempt from the
// envelope middleware — a provider carries no ZoikoSuite identity — and is
// authenticated instead by the binding's HMAC signature (INV-27, NP-26).
func (h *NCDHandler) providerEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable_body", err.Error())
		return
	}
	binding := chi.URLParam(r, "binding")
	b, err := h.svc.VerifyCallback(r.Context(), binding, r.Header.Get("X-NCD-Timestamp"), r.Header.Get("X-NCD-Signature"), body)
	if err != nil {
		// Audited as a security event; nothing about delivery state changes.
		h.log.Warn("provider callback rejected", zap.String("binding", binding), zap.String("remote", r.RemoteAddr), zap.Error(err))
		e := ncd.AsError(err)
		status := http.StatusUnauthorized
		if e.Kind == ncd.KindNotFound {
			status = http.StatusNotFound
		} else if e.Kind == ncd.KindUnavailable {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, e.Code, e.Detail)
		return
	}
	var in struct {
		Events []ncd.ProviderEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &in); err != nil || len(in.Events) == 0 || len(in.Events) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_payload", "body must be {\"events\":[...]} with 1..500 events")
		return
	}
	results := h.svc.IngestProviderEvents(context.WithoutCancel(r.Context()), b, in.Events)
	status := http.StatusOK
	for _, res := range results {
		if res.Status == "RETRY" {
			// A transient store failure: ask the provider to redeliver.
			status = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, status, map[string]any{"results": results})
}

// ── NCD-03 / NCD-04 operations ──────────────────────────────────────────────

func (h *NCDHandler) listBindings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	bs, err := h.svc.ListBindings(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bs})
}

func (h *NCDHandler) setCircuit(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in struct {
		Open   bool   `json:"open"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) || !h.allow(w, r, a, PlatformScopeID, actionSuppressionManage) {
		return
	}
	if err := h.svc.SetCircuit(r.Context(), a, chi.URLParam(r, "id"), in.Open, in.Reason); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"binding_id": chi.URLParam(r, "id"), "circuit_open": in.Open})
}

func (h *NCDHandler) setStream(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) || !h.allow(w, r, a, headerEntity(r), actionSuppressionManage) {
		return
	}
	stream := chi.URLParam(r, "stream")
	if err := h.svc.SetStream(r.Context(), a, stream, in.State, in.Reason); err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stream": stream, "state": in.State})
}

func (h *NCDHandler) reputation(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok || !h.allow(w, r, a, headerEntity(r), actionView) {
		return
	}
	v, err := h.svc.Reputation(r.Context(), a)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *NCDHandler) exceptions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok || !h.allow(w, r, a, headerEntity(r), actionView) {
		return
	}
	xs, err := h.svc.ListExceptions(r.Context(), a, r.URL.Query().Get("open") != "false")
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"exceptions": xs})
}

func (h *NCDHandler) previewBulk(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.BulkInput
	if !decodeJSON(w, r, &in) || !h.allow(w, r, a, in.LegalEntityID, actionSend) {
		return
	}
	b, appr, err := h.svc.PreviewBulk(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"bulk_send": b, "approval": appr})
}

func (h *NCDHandler) dispatchBulk(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in struct {
		AudienceHash string `json:"audience_hash"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	b, err := h.svc.GetBulk(r.Context(), a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.allow(w, r, a, b.LegalEntityID, actionSend) {
		return
	}
	b, results, err := h.svc.DispatchBulk(r.Context(), a, b.BulkID, in.AudienceHash)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"bulk_send": b, "results": results})
}

func approvalAction(kind string) string {
	switch kind {
	case ncd.ApprovalSuppressionLift:
		return actionSuppressionManage
	case ncd.ApprovalBulkSend:
		return actionTemplateApprove
	default:
		return actionResolveOutcome
	}
}

func (h *NCDHandler) approvalEntity(r *http.Request, ap *ncd.Approval) string {
	if ap.LegalEntityID == "-" || ap.LegalEntityID == "" {
		return headerEntity(r)
	}
	return ap.LegalEntityID
}

func (h *NCDHandler) getApproval(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	ap, err := h.svc.GetApproval(r.Context(), a, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if ap.RequestedBy != a.PrincipalID && !h.allow(w, r, a, h.approvalEntity(r, ap), approvalAction(ap.Kind)) {
		return
	}
	writeJSON(w, http.StatusOK, ap)
}

func (h *NCDHandler) decideApproval(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := h.actor(w, r)
		if !ok {
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		if !h.decode(w, r, &in) {
			return
		}
		ap, err := h.svc.GetApproval(r.Context(), a, chi.URLParam(r, "id"))
		if err != nil {
			h.fail(w, err)
			return
		}
		if !h.allow(w, r, a, h.approvalEntity(r, ap), approvalAction(ap.Kind)) {
			return
		}
		ap, result, err := h.svc.DecideApproval(r.Context(), a, ap.ApprovalID, approve, in.Note)
		if err != nil {
			h.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"approval": ap, "result": result})
	}
}

// ── NCD-05 ──────────────────────────────────────────────────────────────────

func (h *NCDHandler) createNotice(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.NoticeInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if _, ok := h.commFor(w, r, a, in.CommunicationID, actionSend); !ok {
		return
	}
	n, err := h.svc.CreateNotice(r.Context(), a, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

func (h *NCDHandler) getNotice(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	v, _ := strconv.Atoi(r.URL.Query().Get("version"))
	nv, c, err := h.svc.GetNotice(r.Context(), a, chi.URLParam(r, "id"), v)
	if err != nil {
		h.fail(w, err)
		return
	}
	if c.RecipientPrincipalID != a.PrincipalID && !h.allow(w, r, a, c.LegalEntityID, actionView) {
		return
	}
	writeJSON(w, http.StatusOK, nv)
}

func (h *NCDHandler) noticeComm(w http.ResponseWriter, r *http.Request, a ncd.Actor, action string) (string, bool) {
	id := chi.URLParam(r, "id")
	_, c, err := h.svc.GetNotice(r.Context(), a, id, 0)
	if err != nil {
		h.fail(w, err)
		return "", false
	}
	return id, h.allow(w, r, a, c.LegalEntityID, action)
}

func (h *NCDHandler) manualEvidence(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in ncd.ManualEvidenceInput
	if !decodeJSON(w, r, &in) {
		return
	}
	id, ok := h.noticeComm(w, r, a, actionResolveOutcome)
	if !ok {
		return
	}
	appr, err := h.svc.RequestManualEvidence(r.Context(), a, id, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"approval": appr,
		"message": "operator evidence applies only when a second principal approves it (NP-41)"})
}

func (h *NCDHandler) declareRecord(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in struct {
		DRCRecordRef string `json:"drc_record_ref"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	id, ok := h.noticeComm(w, r, a, actionSend)
	if !ok {
		return
	}
	n, err := h.svc.DeclareRecord(r.Context(), a, id, in.DRCRecordRef)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}
