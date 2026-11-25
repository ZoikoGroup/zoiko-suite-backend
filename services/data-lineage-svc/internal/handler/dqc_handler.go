package handler

import (
	"net/http"

	"zoiko.io/data-lineage-svc/internal/domain"
	customMiddleware "zoiko.io/data-lineage-svc/internal/middleware"
)

// GET /dqc/lineage (ZS-SVC-Z-001 section 11): the lineage graph as a read-only projection, with the gaps in
// its provenance listed (RB-03: identify the missing run or edge, never infer lineage).

type dqcLineageResponse struct {
	EntityID  string                       `json:"entity_id"`
	Direction string                       `json:"direction"`
	AsOf      string                       `json:"as_of,omitempty"`
	Graph     *domain.LineageGraphSnapshot `json:"graph"`
	// Complete is true only when the graph has no gap: every activity names its run, build, configuration
	// digest and transformation version, and every edge pins exact source and derived versions. It is
	// exactly the condition under which the graph could be sealed into a manifest.
	Complete bool                `json:"complete"`
	Gaps     []domain.LineageGap `json:"gaps"`
}

func (h *Handler) DQCLineage(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionLineageRead) {
		return
	}
	q := r.URL.Query()
	entityID := q.Get("entity_id")
	if entityID == "" {
		h.respondError(w, http.StatusBadRequest, "entity_id is required")
		return
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "upstream"
	}
	resp := dqcLineageResponse{EntityID: entityID, Direction: direction}
	var (
		snap *domain.LineageGraphSnapshot
		err  error
	)
	switch {
	case direction == "upstream" && q.Get("as_of") == "":
		snap, err = h.store.GetUpstreamLineage(r.Context(), tenantID, entityID)
	case direction == "upstream":
		asOf, perr := parseAsOf(q.Get("as_of"))
		if perr != nil {
			h.respondError(w, http.StatusBadRequest, "as_of must be a unix timestamp or RFC3339 datetime")
			return
		}
		resp.AsOf = asOf.UTC().Format("2006-01-02T15:04:05Z07:00")
		snap, err = h.store.GetAsOfLineage(r.Context(), tenantID, entityID, asOf)
	case direction == "downstream" && q.Get("as_of") == "":
		snap, err = h.store.GetDownstreamImpact(r.Context(), tenantID, entityID)
	case direction == "downstream":
		h.respondError(w, http.StatusBadRequest, "as_of is only supported for upstream lineage")
		return
	default:
		h.respondError(w, http.StatusBadRequest, "direction must be upstream or downstream")
		return
	}
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	resp.Graph = snap
	resp.Gaps = domain.LineageGaps(snap)
	resp.Complete = len(resp.Gaps) == 0
	h.respondJSON(w, http.StatusOK, resp)
}
