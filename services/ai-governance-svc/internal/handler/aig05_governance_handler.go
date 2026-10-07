package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/ai-governance-svc/internal/domain"
	"zoiko.io/ai-governance-svc/internal/events"
)

// ─────────────────────────────────────────────────────────────────────────────
// AIG-05: Evaluation, Monitoring, Incident & Change Governance
// ─────────────────────────────────────────────────────────────────────────────

func (h *Handler) CreateEvaluation(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateEvaluationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ModelReleaseID == "" || req.Dimension == "" || req.DatasetVersion == "" || req.Result == "" {
		writeError(w, http.StatusBadRequest, "model_release_id, dimension, dataset_version and result are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, EvaluationCreate) {
		return
	}
	e, err := h.store.CreateEvaluation(r.Context(), req, principalID)
	if err != nil {
		h.respondGovernanceError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "ai.evaluation.recorded", EntityID: e.EvaluationID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: e,
	})
	writeJSON(w, http.StatusCreated, e)
}

func (h *Handler) GetEvaluation(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, EvaluationRead) {
		return
	}
	e, err := h.store.GetEvaluation(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.respondGovernanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ReportIncident files a new AI incident. For AI-P0/AI-P1, the store
// layer quarantines/restricts the named model release in the same
// transaction — the doc's "immediate kill switch" requirement, not a
// follow-up step this handler has to remember to call.
func (h *Handler) ReportIncident(w http.ResponseWriter, r *http.Request) {
	var req domain.ReportIncidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Severity == "" || req.Description == "" {
		writeError(w, http.StatusBadRequest, "severity and description are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, IncidentReport) {
		return
	}
	in, err := h.store.ReportIncident(r.Context(), req, principalID)
	if err != nil {
		h.respondGovernanceError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "ai.incident.state_changed", EntityID: in.IncidentID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: in,
	})
	writeJSON(w, http.StatusCreated, in)
}

func (h *Handler) GetIncident(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, IncidentRead) {
		return
	}
	in, err := h.store.GetIncident(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.respondGovernanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) ContainIncident(w http.ResponseWriter, r *http.Request) {
	var req domain.ContainIncidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, IncidentContain) {
		return
	}
	in, err := h.store.ContainIncident(r.Context(), chi.URLParam(r, "id"), req)
	if err != nil {
		h.respondGovernanceError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "ai.incident.state_changed", EntityID: in.IncidentID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: in,
	})
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) ResolveIncident(w http.ResponseWriter, r *http.Request) {
	var req domain.ResolveIncidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, IncidentResolve) {
		return
	}
	in, err := h.store.ResolveIncident(r.Context(), chi.URLParam(r, "id"), req)
	if err != nil {
		h.respondGovernanceError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "ai.incident.state_changed", EntityID: in.IncidentID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: in,
	})
	writeJSON(w, http.StatusOK, in)
}

func (h *Handler) CloseIncident(w http.ResponseWriter, r *http.Request) {
	var req domain.CloseIncidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, IncidentClose) {
		return
	}
	in, err := h.store.CloseIncident(r.Context(), chi.URLParam(r, "id"), req, principalID)
	if err != nil {
		h.respondGovernanceError(w, err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "ai.incident.state_changed", EntityID: in.IncidentID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: in,
	})
	writeJSON(w, http.StatusOK, in)
}

// ReactivateRelease is AIG-05's governed reactivation gate — a
// QUARANTINED model release may only return to ACTIVE through this
// endpoint, which validates root-cause and re-evaluation evidence at
// the store layer before the transition is attempted.
func (h *Handler) ReactivateRelease(w http.ResponseWriter, r *http.Request) {
	var req domain.ReactivateReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IncidentID == "" {
		writeError(w, http.StatusBadRequest, "incident_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, ReleaseReactivate) {
		return
	}
	m, err := h.store.ReactivateRelease(r.Context(), chi.URLParam(r, "id"), req, principalID)
	if err != nil {
		h.respondGovernanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) respondGovernanceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrEvaluationNotFound), errors.Is(err, domain.ErrIncidentNotFound),
		errors.Is(err, domain.ErrModelReleaseNotFound), errors.Is(err, domain.ErrReactivationIncidentNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrInvalidEvaluationDimension), errors.Is(err, domain.ErrInvalidEvaluationResult),
		errors.Is(err, domain.ErrInvalidIncidentSeverity), errors.Is(err, domain.ErrIncidentMissingScope):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrIncidentNotOpen), errors.Is(err, domain.ErrIncidentNotContained),
		errors.Is(err, domain.ErrIncidentNotResolved), errors.Is(err, domain.ErrInvalidReleaseTransition),
		errors.Is(err, domain.ErrReactivationIncidentScopeMismatch), errors.Is(err, domain.ErrReactivationIncidentNotClosedOrResolved),
		errors.Is(err, domain.ErrReactivationNoPassingReevaluation):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("ai governance request failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "ai governance request failed")
	}
}
