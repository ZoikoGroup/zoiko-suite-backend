package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/events"
	"zoiko.io/counterparty-management-svc/internal/middleware"
	"zoiko.io/counterparty-management-svc/internal/store"
)

const (
	actionCRMCreate = "CRM_CREATE"
	actionCRMManage = "CRM_MANAGE"
	actionCRMClose  = "CRM_CLOSE"
)

// crmHandler embeds *Handler so it reuses requirePrincipal/writeAuthzErr
// without duplicating them — same composition pattern used for AUD-08's
// findingHandler and BIZ-05's taskHandler elsewhere in this build.
type crmHandler struct {
	*Handler
	store store.CRMStore
}

func RegisterCRMRoutes(r chi.Router, h *Handler, crmStore store.CRMStore) {
	ch := &crmHandler{Handler: h, store: crmStore}
	r.Route("/v1/relationships", func(r chi.Router) {
		r.Post("/", ch.CreateRelationship)
		r.Get("/{relationship_id}", ch.GetRelationship)
		r.Post("/{relationship_id}/confirm-party-link", ch.ConfirmPartyLink)
		r.Post("/{relationship_id}/interactions", ch.LogInteraction)
		r.Get("/{relationship_id}/timeline", ch.GetTimeline)
		r.Post("/{relationship_id}/opportunities", ch.CreateOpportunity)
	})
	r.Route("/v1/opportunities", func(r chi.Router) {
		r.Get("/{opportunity_id}", ch.GetOpportunity)
		r.Post("/{opportunity_id}/stage", ch.UpdateStage)
	})
}

type createRelationshipRequest struct {
	LegalEntityID           string `json:"legal_entity_id"`
	CandidateCounterpartyID string `json:"candidate_counterparty_id,omitempty"`
	Source                  string `json:"source,omitempty"`
	Channel                 string `json:"channel,omitempty"`
	OwnerPrincipalID        string `json:"owner_principal_id,omitempty"`
}

// CreateRelationship — BIZ-06's own CreateRelationship command.
func (h *crmHandler) CreateRelationship(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req createRelationshipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCRMCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	rel, err := h.store.CreateRelationship(r.Context(), domain.CreateRelationshipParams{
		TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: req.LegalEntityID,
		CandidateCounterpartyID: req.CandidateCounterpartyID, Source: req.Source, Channel: req.Channel,
		OwnerPrincipalID: req.OwnerPrincipalID, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "relationship.created", CounterpartyID: rel.RelationshipID, TenantID: rel.TenantID, LegalEntityID: rel.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: rel,
	}); err != nil {
		h.logger.Warn("failed to publish relationship.created event", zap.Error(err))
	}
	writeJSON(w, http.StatusCreated, rel)
}

// GetRelationship — BIZ-06's own GetRelationship query.
func (h *crmHandler) GetRelationship(w http.ResponseWriter, r *http.Request) {
	rel, err := h.store.GetRelationship(r.Context(), chi.URLParam(r, "relationship_id"))
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

type confirmPartyLinkRequest struct {
	CounterpartyID string `json:"counterparty_id"`
}

// ConfirmPartyLink — a gap-fill (see internal/domain/crm.go's own doc
// comment on why). Fetched (read-only) BEFORE authorization and BEFORE
// the mutation, same fetch-then-authorize-then-mutate discipline as
// every handler in this platform.
func (h *crmHandler) ConfirmPartyLink(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req confirmPartyLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CounterpartyID == "" {
		writeError(w, http.StatusBadRequest, "counterparty_id is required")
		return
	}
	relationshipID := chi.URLParam(r, "relationship_id")
	existing, err := h.store.GetRelationship(r.Context(), relationshipID)
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionCRMManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	rel, err := h.store.ConfirmPartyLink(r.Context(), domain.ConfirmPartyLinkParams{
		RelationshipID: relationshipID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID, CounterpartyID: req.CounterpartyID,
	})
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

type logInteractionRequest struct {
	InteractionType string `json:"interaction_type"`
	Channel         string `json:"channel,omitempty"`
	Notes           string `json:"notes,omitempty"`
}

// LogInteraction — BIZ-06's own LogInteraction command.
func (h *crmHandler) LogInteraction(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req logInteractionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.InteractionType == "" {
		writeError(w, http.StatusBadRequest, "interaction_type is required")
		return
	}
	relationshipID := chi.URLParam(r, "relationship_id")
	existing, err := h.store.GetRelationship(r.Context(), relationshipID)
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionCRMManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	interaction, err := h.store.LogInteraction(r.Context(), domain.LogInteractionParams{
		RelationshipID: relationshipID, TenantID: middleware.GetTenantID(r.Context()), InteractionType: req.InteractionType,
		Channel: req.Channel, Notes: req.Notes, LoggedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "interaction.logged", CounterpartyID: relationshipID, TenantID: existing.TenantID, LegalEntityID: existing.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: interaction,
	}); err != nil {
		h.logger.Warn("failed to publish interaction.logged event", zap.Error(err))
	}
	writeJSON(w, http.StatusCreated, interaction)
}

// GetTimeline — BIZ-06's own GetTimeline query.
func (h *crmHandler) GetTimeline(w http.ResponseWriter, r *http.Request) {
	relationshipID := chi.URLParam(r, "relationship_id")
	timeline, err := h.store.GetTimeline(r.Context(), middleware.GetTenantID(r.Context()), relationshipID)
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if timeline == nil {
		timeline = []domain.Interaction{}
	}
	writeJSON(w, http.StatusOK, timeline)
}

type createOpportunityRequest struct {
	LegalEntityID    string  `json:"legal_entity_id"`
	Name             string  `json:"name"`
	Value            float64 `json:"value,omitempty"`
	Currency         string  `json:"currency,omitempty"`
	OwnerPrincipalID string  `json:"owner_principal_id,omitempty"`
}

// CreateOpportunity — BIZ-06's own CreateOpportunity command.
func (h *crmHandler) CreateOpportunity(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req createOpportunityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and name are required")
		return
	}
	relationshipID := chi.URLParam(r, "relationship_id")
	existing, err := h.store.GetRelationship(r.Context(), relationshipID)
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionCRMManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	opp, err := h.store.CreateOpportunity(r.Context(), domain.CreateOpportunityParams{
		RelationshipID: relationshipID, TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: req.LegalEntityID,
		Name: req.Name, Value: req.Value, Currency: req.Currency, OwnerPrincipalID: req.OwnerPrincipalID, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "opportunity.created", CounterpartyID: opp.OpportunityID, TenantID: opp.TenantID, LegalEntityID: opp.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: opp,
	}); err != nil {
		h.logger.Warn("failed to publish opportunity.created event", zap.Error(err))
	}
	writeJSON(w, http.StatusCreated, opp)
}

// GetOpportunity — BIZ-06's own GetOpportunity query.
func (h *crmHandler) GetOpportunity(w http.ResponseWriter, r *http.Request) {
	opp, err := h.store.GetOpportunity(r.Context(), chi.URLParam(r, "opportunity_id"))
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opp)
}

type updateStageRequest struct {
	Stage  domain.OpportunityStage `json:"stage"`
	Reason string                  `json:"reason,omitempty"`
}

// UpdateStage — BIZ-06's own UpdateStage command.
func (h *crmHandler) UpdateStage(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req updateStageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Stage == "" {
		writeError(w, http.StatusBadRequest, "stage is required")
		return
	}
	opportunityID := chi.URLParam(r, "opportunity_id")
	existing, err := h.store.GetOpportunity(r.Context(), opportunityID)
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionCRMManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	opp, err := h.store.UpdateStage(r.Context(), domain.UpdateStageParams{
		OpportunityID: opportunityID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID, Stage: req.Stage, Reason: req.Reason,
	})
	if err != nil {
		h.writeCRMErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "opportunity.stage_changed", CounterpartyID: opportunityID, TenantID: opp.TenantID, LegalEntityID: opp.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: opp,
	}); err != nil {
		h.logger.Warn("failed to publish opportunity.stage_changed event", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, opp)
}

func (h *crmHandler) writeCRMErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRelationshipNotFound):
		writeError(w, http.StatusNotFound, "relationship not found")
	case errors.Is(err, domain.ErrRelationshipInvalidState):
		writeError(w, http.StatusConflict, "relationship is not in a state that permits this action")
	case errors.Is(err, domain.ErrOpportunityNotFound):
		writeError(w, http.StatusNotFound, "opportunity not found")
	case errors.Is(err, domain.ErrOpportunityInvalidState):
		writeError(w, http.StatusConflict, "opportunity is not in a state that permits this action")
	case errors.Is(err, domain.ErrPartyLinkNotCandidate):
		writeError(w, http.StatusConflict, "relationship has no candidate party link to confirm")
	case errors.Is(err, domain.ErrInvalidStage):
		writeError(w, http.StatusBadRequest, "invalid opportunity stage")
	default:
		h.logger.Error("CRM store error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "CRM store unavailable")
	}
}
