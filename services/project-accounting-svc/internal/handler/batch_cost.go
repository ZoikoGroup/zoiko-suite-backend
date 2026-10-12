package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// MaxCostIngestBatch caps one ingest call.
const MaxCostIngestBatch = 500

const (
	IngestStatusCaptured  = "CAPTURED"
	IngestStatusDuplicate = "DUPLICATE"
	IngestStatusRejected  = "REJECTED"
)

// CostIngestLineResult is the outcome of one line of a batch.
type CostIngestLineResult struct {
	Index   int    `json:"index"`
	Status  string `json:"status"`
	EntryID string `json:"entry_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type CostIngestResponse struct {
	Results    []CostIngestLineResult `json:"results"`
	Total      int                    `json:"total"`
	Captured   int                    `json:"captured"`
	Duplicates int                    `json:"duplicates"`
	Rejected   int                    `json:"rejected"`
}

// IngestProjectCosts: POST /v1/projects/{id}/costs/ingest
//
// Body is a JSON array of the existing capture-cost request shape. Each line
// is processed independently through captureCostLine — the same code path
// CaptureProjectCost uses — so (source_type, source_reference) idempotency,
// the project-must-be-ACTIVE guard and authz are reused, and nothing posts to
// the GL. HTTP 200 is returned even if some lines are rejected; partial
// success is explicit in the per-line results. A line's project_id may be
// omitted (defaults to {id}) but if present must equal {id}.
func (h *Handler) IngestProjectCosts(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "id")
	var lines []domain.CaptureProjectCostRequest
	if !decodeJSON(w, r, &lines) {
		return
	}
	if len(lines) == 0 {
		writeError(w, http.StatusBadRequest, "empty_batch", "request body must be a non-empty JSON array of cost lines")
		return
	}
	if len(lines) > MaxCostIngestBatch {
		writeError(w, http.StatusBadRequest, "batch_too_large", "a batch may contain at most 500 lines")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	resp := CostIngestResponse{Results: make([]CostIngestLineResult, 0, len(lines)), Total: len(lines)}
	for i, req := range lines {
		res := CostIngestLineResult{Index: i}
		if req.ProjectID == "" {
			req.ProjectID = projectID
		}
		if req.ProjectID != projectID {
			res.Status, res.Reason = IngestStatusRejected, "project_mismatch: line project_id does not match the batch project"
		} else if f := validateCaptureLine(req); f != nil {
			res.Status, res.Reason = IngestStatusRejected, f.Code+": "+f.Message
		} else {
			e, duplicate, p, f := h.captureCostLine(r.Context(), principalID, req, actionProjectCostCapture)
			switch {
			case f != nil:
				res.Status, res.Reason = IngestStatusRejected, f.Code
				if f.Message != "" {
					res.Reason += ": " + f.Message
				}
			case duplicate:
				res.Status, res.EntryID = IngestStatusDuplicate, e.EntryID
			default:
				res.Status, res.EntryID = IngestStatusCaptured, e.EntryID
				h.publisher.PublishProjectCostCaptured(r.Context(), getCorrelationID(r), principalID, p.TenantID, *e)
			}
		}
		switch res.Status {
		case IngestStatusCaptured:
			resp.Captured++
		case IngestStatusDuplicate:
			resp.Duplicates++
		default:
			resp.Rejected++
		}
		resp.Results = append(resp.Results, res)
	}
	writeJSON(w, http.StatusOK, resp)
}
