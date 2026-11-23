package handler

// HTTP surface for ORG-9 master imports and the API-5 operations that carry them.
//
//	POST /v1/master-imports                    upload a file of party rows: 202, Location /v1/operations/{id}. Idempotency-Key
//	                                           makes a retry the same operation, never a second import
//	GET  /v1/master-imports/{id}               the import: status, counts, reconciliation
//	GET  /v1/master-imports/{id}/rows          each row's verdict, errors and outcome (never the row as received)
//	POST /v1/master-imports/{id}:approve       approve a previewed import, by someone other than its uploader: 202 and an operation
//	GET  /v1/operations/{id}                   poll an operation: queued, running, succeeded, failed, cancel_requested, cancelled
//	POST /v1/operations/{id}:cancel            ask an operation to stop; 409 once past its point of no return
//
// Mounted only when the service satisfies MasterImportService, which *registry.Service does.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// MaxImportBody bounds an upload.
const MaxImportBody = 64 << 20

// MasterImportService is the ORG-9 half of the service contract.
type MasterImportService interface {
	CreateMasterImport(ctx context.Context, req domain.CreateMasterImportRequest, idempotencyKey string) (*registry.MasterImportOutcome, error)
	ApproveMasterImport(ctx context.Context, importID string, req domain.ApproveMasterImportRequest, idempotencyKey string) (*registry.MasterImportOutcome, error)
	GetMasterImport(ctx context.Context, importID string) (*domain.MasterImport, error)
	ListImportRows(ctx context.Context, importID string, q domain.ImportRowQuery) ([]domain.ImportRow, error)
	GetOperation(ctx context.Context, operationID string) (apicontract.Operation, error)
	CancelOperation(ctx context.Context, operationID string) (apicontract.Operation, error)
}

func registerMasterImportRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(MasterImportService); !ok {
		return
	}
	r.Post("/master-imports", h.CreateMasterImport)
	r.Get("/master-imports/{importID}", h.GetMasterImport)
	r.Get("/master-imports/{importID}/rows", h.ListImportRows)
	r.Post("/master-imports/{importID}:approve", h.ApproveMasterImport)
	r.Get("/operations/{operationID}", h.GetOperation)
	r.Post("/operations/{operationID}:cancel", h.CancelOperation)
}

func (h *Handler) imports() MasterImportService {
	ms, _ := h.svc.(MasterImportService)
	return ms
}

// importAccepted answers 202 with the operation and where to poll it.
func importAccepted(w http.ResponseWriter, out *registry.MasterImportOutcome) {
	apicontract.WriteOperation(w, http.StatusAccepted, out.Operation)
}

// CreateMasterImport uploads a file of party rows.
func (h *Handler) CreateMasterImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxImportBody)
	var req domain.CreateMasterImportRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.imports().CreateMasterImport(r.Context(), req, r.Header.Get("Idempotency-Key"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	importAccepted(w, out)
}

// ApproveMasterImport approves a previewed import.
func (h *Handler) ApproveMasterImport(w http.ResponseWriter, r *http.Request) {
	var req domain.ApproveMasterImportRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.imports().ApproveMasterImport(r.Context(), chi.URLParam(r, "importID"), req, r.Header.Get("Idempotency-Key"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	importAccepted(w, out)
}

// GetMasterImport returns an import.
func (h *Handler) GetMasterImport(w http.ResponseWriter, r *http.Request) {
	out, err := h.imports().GetMasterImport(r.Context(), chi.URLParam(r, "importID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListImportRows returns the rows of an import, a page at a time by row number.
func (h *Handler) ListImportRows(w http.ResponseWriter, r *http.Request) {
	q := domain.ImportRowQuery{Status: r.URL.Query().Get("status")}
	for name, dst := range map[string]*int{"after_row": &q.AfterRow, "limit": &q.Limit} {
		if v := r.URL.Query().Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeErrJSON(w, r, http.StatusBadRequest, name+" must be a whole number", correlationID(r))
				return
			}
			*dst = n
		}
	}
	rows, err := h.imports().ListImportRows(r.Context(), chi.URLParam(r, "importID"), q)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	resp := map[string]any{"items": rows}
	limit := q.Limit
	if limit == 0 {
		limit = 50
	}
	if len(rows) > 0 && len(rows) == limit {
		resp["next_after_row"] = rows[len(rows)-1].RowNo
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetOperation returns an operation.
func (h *Handler) GetOperation(w http.ResponseWriter, r *http.Request) {
	op, err := h.imports().GetOperation(r.Context(), chi.URLParam(r, "operationID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	apicontract.WriteOperation(w, http.StatusOK, op)
}

// CancelOperation asks an operation to stop.
func (h *Handler) CancelOperation(w http.ResponseWriter, r *http.Request) {
	op, err := h.imports().CancelOperation(r.Context(), chi.URLParam(r, "operationID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	apicontract.WriteOperation(w, http.StatusAccepted, op)
}

// The production service offers master imports; if it ever stops, the routes would silently vanish.
var _ MasterImportService = (*registry.Service)(nil)
