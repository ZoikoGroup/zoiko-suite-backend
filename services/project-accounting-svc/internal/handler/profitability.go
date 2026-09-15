package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// roundCents mirrors internal/store's own helper of the same name — money
// is NUMERIC(18,2), and a delta that doesn't land on a whole cent is
// wrong, not merely imprecise.
func roundCents(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ── POST /v1/profitability/projections/refresh, /rebuild ────────────────────

// RefreshProfitabilityProjection also backs /rebuild — see migration
// 000004's doc comment on why both spec commands collapse into one real
// recompute.
func (h *Handler) RefreshProfitabilityProjection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), req.ProjectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectProfitabilityRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	proj, err := h.store.RefreshProfitabilityProjection(r.Context(), req.ProjectID, principalID, time.Now().UTC())
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	h.publisher.PublishProjectProfitabilityRefreshed(r.Context(), getCorrelationID(r), principalID, tenantID, *proj)
	writeJSON(w, http.StatusOK, proj)
}

// ── GET /v1/profitability/projections?project_id= ────────────────────────────

// GetProjectProfitability also backs GetFreshness — the returned
// projection's own status field IS the freshness label, re-derived
// against live source data on every read.
func (h *Handler) GetProjectProfitability(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), projectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectProfitabilityRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	proj, err := h.store.GetProjectProfitability(r.Context(), projectID)
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proj)
}

// GetMarginBridge backs the spec's own query of the same name — the walk
// from the last certified baseline to the current live projection,
// showing what changed since certification. See
// internal/store/profitability_store.go's own doc comment on
// GetLatestCertifiedSnapshot.
func (h *Handler) GetMarginBridge(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), projectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectProfitabilityRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	current, err := h.store.GetProjectProfitability(r.Context(), projectID)
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	baseline, err := h.store.GetLatestCertifiedSnapshot(r.Context(), projectID)
	if errors.Is(err, domain.ErrSnapshotNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"project_id": projectID, "certified_baseline": nil, "current": current})
		return
	}
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": projectID, "certified_baseline": baseline, "current": current,
		"revenue_delta": roundCents(current.Revenue - baseline.Revenue),
		"cost_delta":    roundCents(current.Cost - baseline.Cost),
		"margin_delta":  roundCents(current.Margin - baseline.Margin),
	})
}

// GetCostRevenueDrilldown backs the spec's own query of the same name —
// the current profitability projection alongside the authoritative
// PRJ-02/PRJ-03 facts it was computed from, so a reader can drill from
// the read model down to source. Composed from existing reads — no new
// store method.
func (h *Handler) GetCostRevenueDrilldown(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), projectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectProfitabilityRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	proj, err := h.store.GetProjectProfitability(r.Context(), projectID)
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	costEntries, err := h.store.ListCostEntries(r.Context(), projectID, "")
	if err != nil {
		h.log.Error("GetCostRevenueDrilldown: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if costEntries == nil {
		costEntries = []domain.CostEntry{}
	}
	revenueRun, err := h.store.GetLatestRecognitionRunForProject(r.Context(), projectID)
	var revenueRunResult any
	if err != nil {
		if !errors.Is(err, domain.ErrRecognitionRunNotFound) {
			h.writeRecognitionErr(w, err)
			return
		}
		revenueRunResult = nil
	} else {
		revenueRunResult = revenueRun
	}
	writeJSON(w, http.StatusOK, map[string]any{"projection": proj, "cost_entries": costEntries, "revenue_run": revenueRunResult})
}

// GetProfitabilityAsOf backs the spec's own query of the same name — the
// certified snapshot that was current as of a past instant. See
// internal/store/profitability_store.go's own doc comment on
// GetProfitabilitySnapshotAsOf.
func (h *Handler) GetProfitabilityAsOf(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	atParam := r.URL.Query().Get("at")
	at := time.Now().UTC()
	if atParam != "" {
		parsed, err := time.Parse(time.RFC3339, atParam)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_at", "at must be RFC3339")
			return
		}
		at = parsed
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), projectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectProfitabilityRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	snap, err := h.store.GetProfitabilitySnapshotAsOf(r.Context(), projectID, at)
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// ── POST /v1/profitability/snapshots ──────────────────────────────────────────

// BuildProfitabilitySnapshot lands the new snapshot directly in
// RECONCILED — see migration 000004's doc comment on why DRAFT is
// unreachable in this v1. Refuses against a STALE projection — the real
// enforcement of negative path #1 at build time.
func (h *Handler) BuildProfitabilitySnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), req.ProjectID)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectProfitabilityRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	snap, err := h.store.BuildProfitabilitySnapshot(r.Context(), req.ProjectID, principalID, time.Now().UTC())
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, snap)
}

// ── GET /v1/profitability/snapshots/{id} ──────────────────────────────────────

func (h *Handler) GetProfitabilitySnapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	snap, err := h.store.GetProfitabilitySnapshot(r.Context(), id)
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionProjectProfitabilityRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// ── POST /v1/profitability/snapshots/{id}/certify ─────────────────────────────

// CertifyProfitabilitySnapshot does not refuse self-certification — see
// migration 000004's doc comment on why that SoD clause is not
// enforceable in this v1 (no metric-model-authorship registration exists
// anywhere on this platform).
func (h *Handler) CertifyProfitabilitySnapshot(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	snap, err := h.store.GetProfitabilitySnapshot(r.Context(), id)
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, snap.LegalEntityID, actionProjectProfitabilityCertify); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	certified, err := h.store.CertifyProfitabilitySnapshot(r.Context(), id, principalID, time.Now().UTC())
	if err != nil {
		h.writeProfitabilityErr(w, err)
		return
	}
	h.publisher.PublishProjectProfitabilitySnapshotCertified(r.Context(), getCorrelationID(r), principalID, tenantID, *certified)
	writeJSON(w, http.StatusOK, certified)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeProfitabilityErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrProjectionNotBuilt):
		writeError(w, http.StatusNotFound, "projection_not_built", err.Error())
	case errors.Is(err, domain.ErrProjectionStale):
		writeError(w, http.StatusUnprocessableEntity, "projection_stale", err.Error())
	case errors.Is(err, domain.ErrSnapshotNotFound):
		writeError(w, http.StatusNotFound, "snapshot_not_found", "")
	case errors.Is(err, domain.ErrInvalidSnapshotTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrSnapshotStaleAtCertification):
		writeError(w, http.StatusUnprocessableEntity, "snapshot_stale", err.Error())
	default:
		h.log.Error("profitability store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
