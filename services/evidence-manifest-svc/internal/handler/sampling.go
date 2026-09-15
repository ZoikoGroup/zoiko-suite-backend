package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/evidence-manifest-svc/internal/domain"
)

const (
	SampleDesignWrite = "SAMPLE_DESIGN_WRITE"
	SampleDesignRead  = "SAMPLE_DESIGN_READ"
	SampleExecute     = "SAMPLE_EXECUTE"
)

type createParamSetRequest struct {
	LegalEntityID         string   `json:"legal_entity_id"`
	Approach              string   `json:"approach"`
	TolerableMisstatement *float64 `json:"tolerable_misstatement,omitempty"`
	ExpectedMisstatement  *float64 `json:"expected_misstatement,omitempty"`
	ConfidenceLevel       *float64 `json:"confidence_level,omitempty"`
	KeyItemThreshold      *float64 `json:"key_item_threshold,omitempty"`
}

// CreateSamplingParameterSet is not one of AUD-04's own named commands,
// but a SampleDesign cannot reference a parameter set that doesn't exist
// yet — this is the prerequisite creation endpoint.
func (h *Handler) CreateSamplingParameterSet(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req createParamSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, SampleDesignWrite) {
		return
	}
	switch req.Approach {
	case domain.SamplingApproachRandom, domain.SamplingApproachSystematic, domain.SamplingApproachJudgmental:
	default:
		writeError(w, http.StatusBadRequest, "invalid_field", "approach")
		return
	}
	ps, err := h.store.CreateSamplingParameterSet(r.Context(), domain.CreateSamplingParameterSetParams{
		Approach: req.Approach, TolerableMisstatement: req.TolerableMisstatement, ExpectedMisstatement: req.ExpectedMisstatement,
		ConfidenceLevel: req.ConfidenceLevel, KeyItemThreshold: req.KeyItemThreshold,
	})
	if err != nil {
		h.log.Error("CreateSamplingParameterSet: store unavailable")
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusCreated, ps)
}

type createSampleDesignRequest struct {
	PopulationID string `json:"population_id"`
	Objective    string `json:"objective"`
	ParamSetID   string `json:"param_set_id"`
	SampleSize   int    `json:"sample_size"`
}

func (h *Handler) CreateSampleDesign(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	var req createSampleDesignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.SampleSize <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_field", "sample_size must be > 0")
		return
	}
	pop, err := h.store.GetAuditPopulation(r.Context(), "", req.PopulationID)
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	if !h.authorize(w, r, principalID, pop.LegalEntityID, SampleDesignWrite) {
		return
	}
	design, created, err := h.store.CreateSampleDesign(r.Context(), domain.CreateSampleDesignParams{
		PopulationID: req.PopulationID, Objective: req.Objective, ParamSetID: req.ParamSetID, SampleSize: req.SampleSize,
		CreatedByPrincipalID: principalID, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, design)
}

func (h *Handler) getDesignForAccess(w http.ResponseWriter, r *http.Request, action string) (*domain.SampleDesign, bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	design, err := h.store.GetSampleDesign(r.Context(), "", chi.URLParam(r, "design_id"))
	if err != nil {
		h.writeSamplingErr(w, err)
		return nil, false
	}
	pop, err := h.store.GetAuditPopulation(r.Context(), "", design.PopulationID)
	if err != nil {
		h.writePopulationErr(w, err)
		return nil, false
	}
	if !h.authorize(w, r, principalID, pop.LegalEntityID, action) {
		return nil, false
	}
	return design, true
}

func (h *Handler) GetSampleDesign(w http.ResponseWriter, r *http.Request) {
	design, ok := h.getDesignForAccess(w, r, SampleDesignRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, design)
}

func (h *Handler) ApproveSampleDesign(w http.ResponseWriter, r *http.Request) {
	design, ok := h.getDesignForAccess(w, r, SampleDesignWrite)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	updated, _, err := h.store.ApproveSampleDesign(r.Context(), domain.ApproveSampleDesignParams{DesignID: design.DesignID, ActorPrincipalID: principalID, CorrelationID: correlationID})
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// SelectSample is the real, deterministic selection algorithm — see
// PgStore.SelectSample's own doc comment.
func (h *Handler) SelectSample(w http.ResponseWriter, r *http.Request) {
	design, ok := h.getDesignForAccess(w, r, SampleDesignWrite)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	sel, items, _, err := h.store.SelectSample(r.Context(), domain.SelectSampleParams{DesignID: design.DesignID, CorrelationID: correlationID})
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"selection": sel, "items": items})
}

func (h *Handler) GetSelectionMethod(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.getDesignForAccess(w, r, SampleDesignRead); !ok {
		return
	}
	matches, err := h.store.ReproduceSelection(r.Context(), "", chi.URLParam(r, "selection_id"))
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reproducible": matches})
}

func (h *Handler) GetItemResults(w http.ResponseWriter, r *http.Request) {
	design, ok := h.getDesignForAccess(w, r, SampleDesignRead)
	if !ok {
		return
	}
	items, err := h.store.GetItemResults(r.Context(), "", design.DesignID)
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

type recordItemResultRequest struct {
	ObjectiveTested string   `json:"objective_tested"`
	Result          string   `json:"result"`
	ExceptionAmount *float64 `json:"exception_amount,omitempty"`
}

// RecordItemResult is AUD-NEG-013's own enforcement point — see
// PgStore.RecordItemResult's own doc comment.
func (h *Handler) RecordItemResult(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	var req recordItemResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ObjectiveTested == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "objective_tested")
		return
	}
	itemID := chi.URLParam(r, "item_id")
	item, _, err := h.store.RecordItemResult(r.Context(), domain.RecordItemResultParams{
		ItemID: itemID, ActorPrincipalID: principalID, ObjectiveTested: req.ObjectiveTested, Result: req.Result,
		ExceptionAmount: req.ExceptionAmount, CorrelationID: correlationID,
	})
	if errors.Is(err, domain.ErrObjectiveMismatch) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "objective_mismatch", "item": item})
		return
	}
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h *Handler) RecordNonresponse(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	item, _, err := h.store.RecordNonresponse(r.Context(), domain.RecordNonresponseParams{ItemID: chi.URLParam(r, "item_id"), ActorPrincipalID: principalID, CorrelationID: correlationID})
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

type addAlternativeProcedureRequest struct {
	ObjectiveTested string `json:"objective_tested"`
	Result          string `json:"result"`
}

func (h *Handler) AddAlternativeProcedure(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	var req addAlternativeProcedureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	item, _, err := h.store.AddAlternativeProcedure(r.Context(), domain.AddAlternativeProcedureParams{
		ItemID: chi.URLParam(r, "item_id"), ActorPrincipalID: principalID, ObjectiveTested: req.ObjectiveTested, Result: req.Result, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// EvaluateSample is AUD-NEG-014's own enforcement point — see
// PgStore.EvaluateSample's own doc comment.
func (h *Handler) EvaluateSample(w http.ResponseWriter, r *http.Request) {
	design, ok := h.getDesignForAccess(w, r, SampleDesignWrite)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	eval, _, err := h.store.EvaluateSample(r.Context(), domain.EvaluateSampleParams{DesignID: design.DesignID, CorrelationID: correlationID})
	if errors.Is(err, domain.ErrSampleEvaluationIncomplete) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "evaluation_incomplete", "evaluation": eval})
		return
	}
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, eval)
}

type supersedeSampleRequest struct {
	Reason        string `json:"reason"`
	NewParamSetID string `json:"new_param_set_id"`
}

// SupersedeSample is AUD-NEG-011's own enforcement point — see
// PgStore.SupersedeSample's own doc comment.
func (h *Handler) SupersedeSample(w http.ResponseWriter, r *http.Request) {
	design, ok := h.getDesignForAccess(w, r, SampleDesignWrite)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	var req supersedeSampleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reason == "" || req.NewParamSetID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason and new_param_set_id are required")
		return
	}
	newDesign, created, err := h.store.SupersedeSample(r.Context(), domain.SupersedeSampleParams{
		DesignID: design.DesignID, Reason: req.Reason, NewParamSetID: req.NewParamSetID, ActorPrincipalID: principalID, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeSamplingErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, newDesign)
}

func (h *Handler) writeSamplingErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrSampleDesignNotFound):
		writeError(w, http.StatusNotFound, "sample_design_not_found", "")
	case errors.Is(err, domain.ErrSampleDesignInvalidState):
		writeError(w, http.StatusUnprocessableEntity, "invalid_sample_design_state", "")
	case errors.Is(err, domain.ErrSampleItemNotFound):
		writeError(w, http.StatusNotFound, "sample_item_not_found", "")
	case errors.Is(err, domain.ErrSampleItemInvalidState):
		writeError(w, http.StatusUnprocessableEntity, "invalid_sample_item_state", "")
	case errors.Is(err, domain.ErrPopulationNotFrozen):
		writeError(w, http.StatusUnprocessableEntity, "population_not_frozen", "")
	case errors.Is(err, domain.ErrPopulationNotFound):
		writeError(w, http.StatusNotFound, "population_not_found", "")
	default:
		h.log.Error("sampling store error")
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}
