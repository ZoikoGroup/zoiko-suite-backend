package handler

// HTTP surface for MDM-05 matching, merge, split and stewardship (ZS-SVC-U-001 s19).
//
//	POST /v1/resolution:match                 MatchParties: explainable, tenant-scoped, writes nothing
//	POST /v1/resolution/{case}:merge          MergeParties (201)
//	POST /v1/resolution/{case}:split          SplitParty (200)
//	GET  /v1/resolution-cases                 list stewardship cases
//	POST /v1/resolution-cases/{id}/decision   decide a case (not a merge)
//	GET  /v1/parties/{id}/merge-history       ExplainMergeHistory
//
// The merging and deciding principal is the verified caller. No body field names it.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// PartyResolutionService is the MDM-05 half of the service contract.
type PartyResolutionService interface {
	MatchParties(ctx context.Context, req domain.MatchPartiesRequest) (*domain.MatchPartiesResult, error)
	ListResolutionCases(ctx context.Context, status, caseType string, limit int, after string) ([]domain.ResolutionCase, string, error)
	DecideResolutionCase(ctx context.Context, caseID string, req domain.CaseDecisionRequest) (*registry.CaseDecisionOutcome, error)
	MergeParties(ctx context.Context, caseID string, req domain.MergePartiesRequest) (*registry.MergeOutcome, error)
	SplitParties(ctx context.Context, caseID string, req domain.SplitPartiesRequest) (*registry.SplitOutcome, error)
	ExplainMergeHistory(ctx context.Context, partyID string) (*domain.MergeHistory, error)
}

func registerPartyResolutionRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(PartyResolutionService); !ok {
		return
	}
	r.Post("/resolution:match", h.MatchParties)
	r.Post("/resolution/{caseID}:merge", h.MergeParties)
	r.Post("/resolution/{caseID}:split", h.SplitParties)
	r.Get("/resolution-cases", h.ListResolutionCases)
	r.Post("/resolution-cases/{caseID}/decision", h.DecideResolutionCase)
	r.Get("/parties/{partyID}/merge-history", h.ExplainMergeHistory)
}

func (h *Handler) resolution() PartyResolutionService {
	rs, _ := h.svc.(PartyResolutionService)
	return rs
}

// MatchParties answers what a create would do.
func (h *Handler) MatchParties(w http.ResponseWriter, r *http.Request) {
	var req domain.MatchPartiesRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := h.resolution().MatchParties(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// MergeParties commits a merge.
func (h *Handler) MergeParties(w http.ResponseWriter, r *http.Request) {
	var req domain.MergePartiesRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.resolution().MergeParties(r.Context(), chi.URLParam(r, "caseID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// SplitParties corrects a merge.
func (h *Handler) SplitParties(w http.ResponseWriter, r *http.Request) {
	var req domain.SplitPartiesRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.resolution().SplitParties(r.Context(), chi.URLParam(r, "caseID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListResolutionCases lists stewardship cases, oldest first.
func (h *Handler) ListResolutionCases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeErrJSON(w, r, http.StatusBadRequest, "limit must be an integer from 1 to 200", correlationID(r))
			return
		}
		limit = n
	}
	out, next, err := h.resolution().ListResolutionCases(r.Context(), q.Get("status"), q.Get("case_type"), limit, q.Get("cursor"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	if out == nil {
		out = []domain.ResolutionCase{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}

// DecideResolutionCase settles a case that is not a merge.
func (h *Handler) DecideResolutionCase(w http.ResponseWriter, r *http.Request) {
	var req domain.CaseDecisionRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.resolution().DecideResolutionCase(r.Context(), chi.URLParam(r, "caseID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ExplainMergeHistory explains where a party resolves and why.
func (h *Handler) ExplainMergeHistory(w http.ResponseWriter, r *http.Request) {
	out, err := h.resolution().ExplainMergeHistory(r.Context(), chi.URLParam(r, "partyID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var _ PartyResolutionService = (*registry.Service)(nil)
