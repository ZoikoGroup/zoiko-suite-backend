package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
)

const (
	actionAuditPopulationManage = "AUDIT_POPULATION_MANAGE"
	actionAuditPopulationRead   = "AUDIT_POPULATION_READ"
)

type definePopulationRequest struct {
	SourceSystem string    `json:"source_system"`
	SourceObject string    `json:"source_object"`
	FilterSpec   string    `json:"filter_spec"`
	Watermark    string    `json:"watermark"`
	PeriodStart  time.Time `json:"period_start"`
	PeriodEnd    time.Time `json:"period_end"`
}

func (r definePopulationRequest) missing() string {
	switch {
	case r.SourceSystem == "":
		return "source_system"
	case r.SourceObject == "":
		return "source_object"
	case r.FilterSpec == "":
		return "filter_spec"
	case r.Watermark == "":
		return "watermark"
	case r.PeriodStart.IsZero():
		return "period_start"
	case r.PeriodEnd.IsZero():
		return "period_end"
	default:
		return ""
	}
}

// DefinePopulation handles POST /v1/audit/engagements/{engagement_id}/populations
// — AUD-03's entry point. The population starts empty (DEFINED); the extract
// itself is produced outside this service and recorded via BuildPopulation.
func (h *Handler) DefinePopulation(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPopulationManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	var req definePopulationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if missing := req.missing(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if req.PeriodEnd.Before(req.PeriodStart) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_period"})
		return
	}
	population, created, err := h.store.DefinePopulation(r.Context(), domain.DefinePopulationParams{
		EngagementID: engagement.EngagementID, TenantID: tenantID, SourceSystem: req.SourceSystem, SourceObject: req.SourceObject,
		FilterSpec: req.FilterSpec, Watermark: req.Watermark, PeriodStart: req.PeriodStart, PeriodEnd: req.PeriodEnd,
		CreatedByPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		h.publishAuditPopulationEvent(r, "audit.population.built", *population, actor)
	}
	writeJSON(w, status, population)
}

// getAuditPopulationForAccess resolves a population, checks authz against its
// owning engagement's legal entity, and returns both.
func (h *Handler) getAuditPopulationForAccess(w http.ResponseWriter, r *http.Request, action string) (*domain.AuditPopulation, *domain.AuditEngagement, bool) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return nil, nil, false
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, nil, false
	}
	population, err := h.store.GetAuditPopulation(r.Context(), tenantID, chi.URLParam(r, "population_id"))
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return nil, nil, false
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, population.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return nil, nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, action); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return nil, nil, false
	}
	return population, engagement, true
}

func (h *Handler) GetAuditPopulation(w http.ResponseWriter, r *http.Request) {
	population, _, ok := h.getAuditPopulationForAccess(w, r, actionAuditPopulationRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, population)
}

type buildPopulationRequest struct {
	ItemCount          int64   `json:"item_count"`
	ControlTotalAmount float64 `json:"control_total_amount"`
}

// BuildPopulation handles POST /v1/audit/populations/{population_id}/build.
// The item_count/control_total are the caller's own extract totals — this
// service records them as a candidate manifest, it does not compute them.
func (h *Handler) BuildPopulation(w http.ResponseWriter, r *http.Request) {
	population, _, ok := h.getAuditPopulationForAccess(w, r, actionAuditPopulationManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	var req buildPopulationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.ItemCount < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "item_count"})
		return
	}
	updated, _, err := h.store.BuildPopulation(r.Context(), domain.BuildPopulationParams{
		PopulationID: population.PopulationID, TenantID: population.TenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
		ItemCount: req.ItemCount, ControlTotalAmount: req.ControlTotalAmount,
	})
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type validatePopulationRequest struct {
	ExpectedItemCount          int64   `json:"expected_item_count"`
	ExpectedControlTotalAmount float64 `json:"expected_control_total_amount"`
}

// ValidatePopulation handles POST /v1/audit/populations/{population_id}/validate.
// A reconciliation mismatch (AUD-NEG-008) still returns 200 with the
// population's new QUARANTINED status — the request itself succeeded, it is
// the extract that failed.
func (h *Handler) ValidatePopulation(w http.ResponseWriter, r *http.Request) {
	population, _, ok := h.getAuditPopulationForAccess(w, r, actionAuditPopulationManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	var req validatePopulationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	updated, _, err := h.store.ValidatePopulation(r.Context(), domain.ValidatePopulationParams{
		PopulationID: population.PopulationID, TenantID: population.TenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
		ExpectedItemCount: req.ExpectedItemCount, ExpectedControlTotalAmount: req.ExpectedControlTotalAmount,
	})
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return
	}
	if updated.Status == domain.AuditPopulationQuarantined {
		h.publishAuditPopulationEvent(r, "audit.population.validation_failed", *updated, actor)
	}
	writeJSON(w, http.StatusOK, updated)
}

// FreezePopulation handles POST /v1/audit/populations/{population_id}/freeze.
func (h *Handler) FreezePopulation(w http.ResponseWriter, r *http.Request) {
	population, _, ok := h.getAuditPopulationForAccess(w, r, actionAuditPopulationManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	updated, changed, err := h.store.FreezePopulation(r.Context(), domain.FreezePopulationParams{
		PopulationID: population.PopulationID, TenantID: population.TenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return
	}
	if changed {
		h.publishAuditPopulationEvent(r, "audit.population.frozen", *updated, actor)
	}
	writeJSON(w, http.StatusOK, updated)
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

// SupersedePopulation handles POST /v1/audit/populations/{population_id}/supersede.
func (h *Handler) SupersedePopulation(w http.ResponseWriter, r *http.Request) {
	population, _, ok := h.getAuditPopulationForAccess(w, r, actionAuditPopulationManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	var req reasonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reason"})
		return
	}
	updated, _, err := h.store.SupersedePopulation(r.Context(), domain.SupersedePopulationParams{
		PopulationID: population.PopulationID, TenantID: population.TenantID, ActorPrincipalID: actor, CorrelationID: correlationID, Reason: req.Reason,
	})
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// QuarantinePopulation handles POST /v1/audit/populations/{population_id}/quarantine.
func (h *Handler) QuarantinePopulation(w http.ResponseWriter, r *http.Request) {
	population, _, ok := h.getAuditPopulationForAccess(w, r, actionAuditPopulationManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	var req reasonRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reason"})
		return
	}
	updated, _, err := h.store.QuarantinePopulation(r.Context(), domain.QuarantinePopulationParams{
		PopulationID: population.PopulationID, TenantID: population.TenantID, ActorPrincipalID: actor, CorrelationID: correlationID, Reason: req.Reason,
	})
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type addControlledDeltaRequest struct {
	Reason                  string  `json:"reason"`
	DeltaItemCount          int64   `json:"delta_item_count"`
	DeltaControlTotalAmount float64 `json:"delta_control_total_amount"`
}

// AddControlledDelta handles POST /v1/audit/populations/{population_id}/delta
// — AUD-NEG-010: a late item after freeze never mutates the original;
// this creates a new successor version and supersedes the original.
func (h *Handler) AddControlledDelta(w http.ResponseWriter, r *http.Request) {
	population, _, ok := h.getAuditPopulationForAccess(w, r, actionAuditPopulationManage)
	if !ok {
		return
	}
	actor, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	var req addControlledDeltaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reason"})
		return
	}
	successor, created, err := h.store.AddControlledDelta(r.Context(), domain.AddControlledDeltaParams{
		PopulationID: population.PopulationID, TenantID: population.TenantID, ActorPrincipalID: actor, CorrelationID: correlationID,
		Reason: req.Reason, DeltaItemCount: req.DeltaItemCount, DeltaControlTotalAmount: req.DeltaControlTotalAmount,
	})
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		h.publishAuditPopulationEvent(r, "audit.population.delta_added", *successor, actor)
	}
	writeJSON(w, status, successor)
}

func (h *Handler) ListAuditPopulations(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPopulationRead); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	populations, err := h.store.ListAuditPopulationsByEngagement(r.Context(), tenantID, engagement.EngagementID)
	if err != nil {
		h.writeAuditPopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, populations)
}

func (h *Handler) publishAuditPopulationEvent(r *http.Request, eventType string, population domain.AuditPopulation, actor string) {
	if err := h.publisher.PublishAuditPopulationEvent(r.Context(), eventType, population, actor, r.Header.Get("X-Correlation-ID")); err != nil {
		h.log.Error("failed to publish audit population event", zap.String("event_type", eventType), zap.String("population_id", population.PopulationID), zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
	}
}

func (h *Handler) writeAuditPopulationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuditPopulationNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "audit_population_not_found"})
	case errors.Is(err, domain.ErrAuditPopulationInvalidState):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_population_state"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}
