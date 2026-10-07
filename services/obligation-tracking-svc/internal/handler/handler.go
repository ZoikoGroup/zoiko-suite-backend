package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligation-tracking-svc/internal/authz"
	"zoiko.io/obligation-tracking-svc/internal/domain"
	"zoiko.io/obligation-tracking-svc/internal/events"
	"zoiko.io/obligation-tracking-svc/internal/middleware"
	"zoiko.io/obligation-tracking-svc/internal/store"
)

const (
	actionObligationCreate     = "OBLIGATION_CREATE"
	actionObligationUpdate     = "OBLIGATION_UPDATE"
	actionObligationValidate   = "OBLIGATION_VALIDATE"
	actionObligationSchedule   = "OBLIGATION_SCHEDULE"
	actionObligationMarkDue    = "OBLIGATION_MARK_DUE"
	actionObligationMarkInProg = "OBLIGATION_MARK_IN_PROGRESS"
	actionObligationComplete   = "OBLIGATION_COMPLETE"
	actionObligationWaive      = "OBLIGATION_WAIVE"
	actionObligationBreach     = "OBLIGATION_RECORD_BREACH"
	actionObligationDispute    = "OBLIGATION_DISPUTE"
	actionObligationSupersede  = "OBLIGATION_SUPERSEDE"
)

// AuthZClient is the subset of authz.Client used by the handler, so tests can stub it.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthZClient
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "not authorized to perform this action")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

// writeLifecycleErr maps a store failure to the status it deserves, shared
// by every command in this handler.
func (h *Handler) writeLifecycleErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, domain.ErrTenantMissing):
		writeError(w, http.StatusUnauthorized, "tenant scope missing")
	case errors.Is(err, domain.ErrObligationNotFound):
		writeError(w, http.StatusNotFound, "obligation not found")
	case errors.Is(err, domain.ErrWrongStatus):
		writeError(w, http.StatusConflict, domain.ErrWrongStatus.Error())
	case errors.Is(err, domain.ErrAmbiguousDueDateBasis):
		writeError(w, http.StatusBadRequest, domain.ErrAmbiguousDueDateBasis.Error())
	case errors.Is(err, domain.ErrWaiverAuthorityRequired):
		writeError(w, http.StatusBadRequest, domain.ErrWaiverAuthorityRequired.Error())
	default:
		h.logger.Error(what, zap.Error(err))
		writeError(w, http.StatusInternalServerError, what)
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/obligations", func(r chi.Router) {
		r.Post("/", h.CreateObligation)
		r.Get("/", h.ListObligations)
		r.Get("/{id}", h.GetObligation)
		r.Put("/{id}", h.UpdateObligation)
		r.Post("/{id}/validate", h.ValidateExtractedObligation)
		r.Post("/{id}/schedule", h.Schedule)
		r.Post("/{id}/mark-due", h.MarkDue)
		r.Post("/{id}/mark-in-progress", h.MarkInProgress)
		r.Post("/{id}/complete", h.Complete)
		r.Post("/{id}/waive", h.Waive)
		r.Post("/{id}/record-breach", h.RecordBreach)
		r.Post("/{id}/dispute", h.Dispute)
		r.Post("/{id}/supersede", h.Supersede)
	})
}

func (h *Handler) CreateObligation(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.DueDate == "" || req.ObligationType == "" {
		writeError(w, http.StatusBadRequest, "title, due_date, and obligation_type are required")
		return
	}
	if req.ObligationType != domain.ObligationTypeContractual {
		writeError(w, http.StatusBadRequest, "obligation_type must be CONTRACTUAL — this service tracks contract-derived obligations only; statutory/regulatory/internal-policy obligations belong in obligations-svc")
		return
	}
	if req.SourceType != "" && !domain.SourceType(req.SourceType).Valid() {
		writeError(w, http.StatusBadRequest, "source_type must be CONTRACT or CLAUSE")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionObligationCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o := &domain.Obligation{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		SourceType:     req.SourceType,
		SourceID:       req.SourceID,
		Title:          req.Title,
		Description:    req.Description,
		ObligationType: req.ObligationType,
		RiskLevel:      req.RiskLevel,
		DueDate:        req.DueDate,
		AssignedTo:     req.AssignedTo,
		ExtractedByAI:  req.ExtractedByAI,
		EffectiveFrom:  req.EffectiveFrom,
		EffectiveTo:    req.EffectiveTo,
		CreatedBy:      req.CreatedBy,
	}

	if err := h.store.CreateObligation(r.Context(), o); err != nil {
		h.writeLifecycleErr(w, "failed to create obligation", err)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.created", ObligationID: o.ObligationID, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusCreated, o)
}

func (h *Handler) GetObligation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	o, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to get obligation", err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) ListObligations(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")
	sourceType := r.URL.Query().Get("source_type")
	obligations, err := h.store.ListObligations(r.Context(), legalEntityID, status, sourceType)
	if err != nil {
		h.writeLifecycleErr(w, "failed to list obligations", err)
		return
	}
	if obligations == nil {
		obligations = []domain.Obligation{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"obligations": obligations, "total": len(obligations)})
}

func (h *Handler) UpdateObligation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}

	var req domain.UpdateObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.ObligationType != "" {
		if req.ObligationType != domain.ObligationTypeContractual {
			writeError(w, http.StatusBadRequest, "obligation_type must be CONTRACTUAL — this service tracks contract-derived obligations only; statutory/regulatory/internal-policy obligations belong in obligations-svc")
			return
		}
		existing.ObligationType = req.ObligationType
	}
	if req.RiskLevel != "" {
		existing.RiskLevel = req.RiskLevel
	}
	if req.DueDate != "" {
		existing.DueDate = req.DueDate
	}
	if req.AssignedTo != "" {
		existing.AssignedTo = req.AssignedTo
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.UpdateObligation(r.Context(), existing); err != nil {
		h.writeLifecycleErr(w, "failed to update obligation", err)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.updated", ObligationID: id, TenantID: tenantID,
		LegalEntityID: existing.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: existing,
	})
	writeJSON(w, http.StatusOK, existing)
}

// ValidateExtractedObligation moves CANDIDATE -> PLANNED. LEG-07 §9.1: "AI
// extraction cannot activate obligation without validation" — this is that
// gate.
func (h *Handler) ValidateExtractedObligation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationValidate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.ValidateExtractedObligation(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to validate obligation", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.validated", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

// Schedule moves PLANNED -> ACTIVE and certifies the due date's provenance
// (source clause, trigger, calculation method — LEG-07 §9.1).
func (h *Handler) Schedule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.ScheduleObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationSchedule); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.Schedule(r.Context(), id, &req)
	if err != nil {
		h.writeLifecycleErr(w, "failed to schedule obligation", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.activated", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) MarkDue(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationMarkDue); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.MarkDue(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to mark obligation due", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.due", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) MarkInProgress(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationMarkInProg); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.MarkInProgress(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to mark obligation in progress", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.in_progress", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationComplete); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.Complete(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to complete obligation", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.satisfied", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

// Waive is distinct from satisfaction and requires documented authority
// (LEG-07 §9.1).
func (h *Handler) Waive(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.WaiveObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationWaive); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.Waive(r.Context(), id, &req)
	if err != nil {
		h.writeLifecycleErr(w, "failed to waive obligation", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.waived", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

// RecordBreach records the fact of breach only — LEG-07 §9.1: no automatic
// financial accrual, payment or legal remedy.
func (h *Handler) RecordBreach(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.RecordBreachRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationBreach); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.RecordBreach(r.Context(), id, &req)
	if err != nil {
		h.writeLifecycleErr(w, "failed to record breach", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.breached", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) Dispute(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.DisputeObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationDispute); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.Dispute(r.Context(), id, &req)
	if err != nil {
		h.writeLifecycleErr(w, "failed to dispute obligation", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.disputed", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) Supersede(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.SupersedeObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SupersededBy == "" {
		writeError(w, http.StatusBadRequest, "superseded_by is required")
		return
	}

	existing, err := h.store.GetObligation(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch obligation", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionObligationSupersede); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	o, err := h.store.Supersede(r.Context(), id, req.SupersededBy)
	if err != nil {
		h.writeLifecycleErr(w, "failed to supersede obligation", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "obligation.superseded", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
