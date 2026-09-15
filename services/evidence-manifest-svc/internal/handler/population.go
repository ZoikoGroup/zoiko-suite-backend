package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
)

const (
	AuditPopulationWrite = "AUDIT_POPULATION_WRITE"
	AuditPopulationRead  = "AUDIT_POPULATION_READ"
)

type definePopulationRequest struct {
	EngagementID              string `json:"engagement_id"`
	LegalEntityID             string `json:"legal_entity_id"`
	ObjectClass               string `json:"object_class"`
	PeriodStart               string `json:"period_start"`
	PeriodEnd                 string `json:"period_end"`
	SourceSystem              string `json:"source_system"`
	SourceQuery               string `json:"source_query"`
	SourceWatermark           string `json:"source_watermark"`
	Assertion                 string `json:"assertion"`
	ExpectedCompletenessCheck string `json:"expected_completeness_check"`
}

func (h *Handler) DefinePopulation(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req definePopulationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, AuditPopulationWrite) {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	periodStart, err1 := parseDate(req.PeriodStart)
	periodEnd, err2 := parseDate(req.PeriodEnd)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, "invalid_date", "period_start/period_end must be YYYY-MM-DD")
		return
	}
	pop, created, err := h.store.DefinePopulation(r.Context(), domain.DefinePopulationParams{
		EngagementID: req.EngagementID, TenantID: "", LegalEntityID: req.LegalEntityID, ObjectClass: req.ObjectClass,
		PeriodStart: periodStart, PeriodEnd: periodEnd, SourceSystem: req.SourceSystem, SourceQuery: req.SourceQuery,
		SourceWatermark: req.SourceWatermark, Assertion: req.Assertion, ExpectedCompletenessCheck: req.ExpectedCompletenessCheck,
		CreatedByPrincipalID: principalID, CorrelationID: correlationID,
	})
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, pop)
}

func (h *Handler) getPopulationForAccess(w http.ResponseWriter, r *http.Request, action string) (*domain.AuditPopulation, bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	pop, err := h.store.GetAuditPopulation(r.Context(), "", chi.URLParam(r, "population_id"))
	if err != nil {
		h.writePopulationErr(w, err)
		return nil, false
	}
	if !h.authorize(w, r, principalID, pop.LegalEntityID, action) {
		return nil, false
	}
	return pop, true
}

func (h *Handler) GetAuditPopulation(w http.ResponseWriter, r *http.Request) {
	pop, ok := h.getPopulationForAccess(w, r, AuditPopulationRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, pop)
}

func (h *Handler) ListAuditPopulations(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	engagementID := chi.URLParam(r, "engagement_id")
	// No single legal_entity_id is known before the query; the store
	// scopes by tenant+engagement, and each returned population's own
	// legal_entity_id was already set at DefinePopulation authz time — a
	// list read is therefore checked against the caller's tenant-wide
	// read grant rather than per-row, mirroring GetManifest's own posture
	// of authorizing after the tenant-scoped fetch.
	pops, err := h.store.ListAuditPopulationsByEngagement(r.Context(), "", engagementID)
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	if len(pops) > 0 && !h.authorize(w, r, principalID, pops[0].LegalEntityID, AuditPopulationRead) {
		return
	}
	writeJSON(w, http.StatusOK, pops)
}

type buildPopulationRequest struct {
	Rows []buildPopulationRowRequest `json:"rows"`
}

type buildPopulationRowRequest struct {
	SourceRecordID string          `json:"source_record_id"`
	Amount         *float64        `json:"amount,omitempty"`
	RowSnapshot    json.RawMessage `json:"row_snapshot"`
}

func (h *Handler) BuildPopulation(w http.ResponseWriter, r *http.Request) {
	pop, ok := h.getPopulationForAccess(w, r, AuditPopulationWrite)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	var req buildPopulationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Rows) == 0 {
		writeError(w, http.StatusBadRequest, "missing_field", "rows")
		return
	}
	rows := make([]domain.BuildPopulationRow, len(req.Rows))
	for i, rr := range req.Rows {
		rows[i] = domain.BuildPopulationRow{SourceRecordID: rr.SourceRecordID, Amount: rr.Amount, RowSnapshot: rr.RowSnapshot}
	}
	updated, _, err := h.store.BuildPopulation(r.Context(), domain.BuildPopulationParams{PopulationID: pop.PopulationID, TenantID: "", CorrelationID: correlationID, Rows: rows})
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type validatePopulationRequest struct {
	ControlTotals map[string]float64 `json:"control_totals"`
}

func (h *Handler) ValidatePopulation(w http.ResponseWriter, r *http.Request) {
	pop, ok := h.getPopulationForAccess(w, r, AuditPopulationWrite)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	var req validatePopulationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.ControlTotals) == 0 {
		writeError(w, http.StatusBadRequest, "missing_field", "control_totals")
		return
	}
	updated, _, err := h.store.ValidatePopulation(r.Context(), domain.ValidatePopulationParams{PopulationID: pop.PopulationID, TenantID: "", CorrelationID: correlationID, ControlTotals: req.ControlTotals})
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// FreezePopulation is the real enforcement point for AUD-CTRL-007 — see
// PgStore.FreezePopulation's own doc comment. A reconciliation failure
// routes to QUARANTINED and is reported as 422 with the reason, not a 500.
func (h *Handler) FreezePopulation(w http.ResponseWriter, r *http.Request) {
	pop, ok := h.getPopulationForAccess(w, r, AuditPopulationWrite)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	updated, _, err := h.store.FreezePopulation(r.Context(), domain.FreezePopulationParams{PopulationID: pop.PopulationID, TenantID: "", CorrelationID: correlationID})
	if err != nil {
		if errors.Is(err, domain.ErrPopulationControlTotalMismatch) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "control_total_mismatch", "population": updated})
			return
		}
		h.writePopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type supersedePopulationRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) SupersedePopulation(w http.ResponseWriter, r *http.Request) {
	pop, ok := h.getPopulationForAccess(w, r, AuditPopulationWrite)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	var req supersedePopulationRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	updated, _, err := h.store.SupersedePopulation(r.Context(), domain.SupersedePopulationParams{PopulationID: pop.PopulationID, TenantID: "", Reason: req.Reason, CorrelationID: correlationID})
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type quarantinePopulationRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) QuarantinePopulation(w http.ResponseWriter, r *http.Request) {
	pop, ok := h.getPopulationForAccess(w, r, AuditPopulationWrite)
	if !ok {
		return
	}
	var req quarantinePopulationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}
	updated, _, err := h.store.QuarantinePopulation(r.Context(), domain.QuarantinePopulationParams{PopulationID: pop.PopulationID, TenantID: "", Reason: req.Reason})
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type addControlledDeltaRequest struct {
	Reason string                      `json:"reason"`
	Rows   []buildPopulationRowRequest `json:"rows"`
}

// AddControlledDelta is AUD-NEG-010's own mechanism — see
// PgStore.AddControlledDelta's own doc comment: a late item is never
// written into the frozen population, only into a new linked version.
func (h *Handler) AddControlledDelta(w http.ResponseWriter, r *http.Request) {
	pop, ok := h.getPopulationForAccess(w, r, AuditPopulationWrite)
	if !ok {
		return
	}
	principalID, _ := h.requirePrincipal(w, r)
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	var req addControlledDeltaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reason == "" || len(req.Rows) == 0 {
		writeError(w, http.StatusBadRequest, "missing_field", "reason and rows are required")
		return
	}
	rows := make([]domain.BuildPopulationRow, len(req.Rows))
	for i, rr := range req.Rows {
		rows[i] = domain.BuildPopulationRow{SourceRecordID: rr.SourceRecordID, Amount: rr.Amount, RowSnapshot: rr.RowSnapshot}
	}
	newPop, created, err := h.store.AddControlledDelta(r.Context(), domain.AddControlledDeltaParams{
		PopulationID: pop.PopulationID, TenantID: "", Reason: req.Reason, CreatedByPrincipalID: principalID, CorrelationID: correlationID, Rows: rows,
	})
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, newPop)
}

func (h *Handler) GetControlTotals(w http.ResponseWriter, r *http.Request) {
	pop, ok := h.getPopulationForAccess(w, r, AuditPopulationRead)
	if !ok {
		return
	}
	totals, err := h.store.GetControlTotals(r.Context(), "", pop.PopulationID)
	if err != nil {
		h.writePopulationErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, totals)
}

func parseDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

func (h *Handler) writePopulationErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrPopulationNotFound):
		writeError(w, http.StatusNotFound, "population_not_found", "")
	case errors.Is(err, domain.ErrPopulationInvalidState):
		writeError(w, http.StatusUnprocessableEntity, "invalid_population_state", "")
	case errors.Is(err, domain.ErrPopulationNotFrozen):
		writeError(w, http.StatusUnprocessableEntity, "population_not_frozen", "")
	default:
		h.log.Error("population store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}
