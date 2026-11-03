package handler

// REF-01 ImportJurisdictionUpdate (ZS-SVC-I-001 s4.11): an externally sourced
// update to the jurisdiction registry, with a publisher and a different approver.
//
//	POST /v1/admin/jurisdiction-imports                      propose (ImportJurisdictionUpdate)
//	GET  /v1/admin/jurisdiction-imports?status=&limit=       list batches
//	GET  /v1/admin/jurisdiction-imports/{batch_id}           one batch, with its rows and their errors
//	POST /v1/admin/jurisdiction-imports/{batch_id}/approve   approve and apply, all rows or none
//	POST /v1/admin/jurisdiction-imports/{batch_id}/reject    reject, with a reason
//
// "ISO/source updates maker-checker" and "Publisher/approver separation for
// externally sourced code-list updates": proposing changes nothing; approval by
// the same principal is refused; a batch with a row that failed validation
// cannot be approved; and approval applies the whole batch in one transaction
// or none of it, announcing each change in the same transaction.

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// maxImportBody bounds a proposal: MaxImportItems rows with room to spare.
const maxImportBody = 4 << 20

type jurisdictionImportStore interface {
	ProposeJurisdictionImportWithQuerier(ctx context.Context, q store.Querier, p domain.ProposeImportParams) (*domain.ImportBatch, bool, error)
	ApproveJurisdictionImportWithQuerier(ctx context.Context, q store.Querier, p domain.DecideImportParams) (*domain.ImportBatch, []domain.AppliedChange, error)
	RejectJurisdictionImportWithQuerier(ctx context.Context, q store.Querier, p domain.DecideImportParams) (*domain.ImportBatch, error)
	GetJurisdictionImport(ctx context.Context, batchID string) (*domain.ImportBatch, error)
	ListJurisdictionImports(ctx context.Context, status string, limit int) ([]*domain.ImportBatch, error)
}

// ProposeImportRequest is the body of POST /v1/admin/jurisdiction-imports.
type ProposeImportRequest struct {
	SourceStandard string                   `json:"source_standard"`
	SourceVersion  string                   `json:"source_version"`
	EvidenceRef    string                   `json:"evidence_ref"`
	Summary        string                   `json:"summary"`
	Items          []domain.ImportItemInput `json:"items"`
}

// DecideImportRequest is the body of approve and reject. A reason is required
// to reject and optional to approve.
type DecideImportRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) importStore(w http.ResponseWriter, r *http.Request) (jurisdictionImportStore, bool) {
	s, ok := h.store.(jurisdictionImportStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "the store does not support jurisdiction imports")
	}
	return s, ok
}

// writeImportError answers the refusals the import surface has.
func (h *Handler) writeImportError(w http.ResponseWriter, r *http.Request, err error, correlationID string) {
	var rowErr *domain.ImportItemError
	switch {
	case errors.As(err, &rowErr):
		writeProblemWith(w, r, http.StatusUnprocessableEntity, "import_row_failed", rowErr.Error(),
			map[string]any{"item_no": rowErr.ItemNo, "message": rowErr.Message})
	case errors.Is(err, domain.ErrInvalidImport):
		writeError(w, r, http.StatusBadRequest, "invalid_import", err.Error())
	case errors.Is(err, domain.ErrImportNotFound):
		writeError(w, r, http.StatusNotFound, "import_not_found", err.Error())
	case errors.Is(err, domain.ErrImportDecided):
		writeError(w, r, http.StatusConflict, "import_already_decided", err.Error())
	case errors.Is(err, domain.ErrImportVersionConflict):
		writeError(w, r, http.StatusConflict, "import_version_conflict", err.Error())
	case errors.Is(err, domain.ErrImportHasInvalidRows):
		writeError(w, r, http.StatusUnprocessableEntity, "import_has_invalid_rows", err.Error())
	case errors.Is(err, domain.ErrNotIndependent):
		writeError(w, r, http.StatusForbidden, "segregation_of_duties",
			"the principal who proposed an import cannot approve it: a different reference-data steward must")
	default:
		h.writeStoreError(w, r, err, correlationID)
	}
}

func importSource(b *domain.ImportBatch) *events.ImportSource {
	return &events.ImportSource{
		BatchID: b.BatchID, SourceStandard: b.SourceStandard, SourceVersion: b.SourceVersion, ImportHash: b.ImportHash,
	}
}

// ImportJurisdictionUpdate handles POST /v1/admin/jurisdiction-imports.
//
//	201 → proposed; read invalid_count and each row's validation_errors before asking for approval
//	200 → this source version was already proposed with the same content (replay)
//	400 → the batch as a whole is not acceptable
//	409 → this source version was already proposed with different content
func (h *Handler) ImportJurisdictionUpdate(w http.ResponseWriter, r *http.Request) {
	s, ok := h.importStore(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "import_propose"); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBody)
	var req ProposeImportRequest
	if !decodeBody(w, r, &req) {
		return
	}

	var batch *domain.ImportBatch
	var created bool
	err := h.store.WithTransaction(r.Context(), func(q store.Querier) error {
		b, isNew, err := s.ProposeJurisdictionImportWithQuerier(r.Context(), q, domain.ProposeImportParams{
			SourceStandard: req.SourceStandard, SourceVersion: req.SourceVersion, EvidenceRef: req.EvidenceRef,
			Summary: req.Summary, Items: req.Items, ActorID: principalID,
		})
		batch, created = b, isNew
		return err
	})
	if err != nil {
		h.writeImportError(w, r, err, correlationID)
		return
	}
	h.log.Info("ImportJurisdictionUpdate", zap.String("batch_id", batch.BatchID), zap.Bool("created", created),
		zap.Int("items", batch.ItemCount), zap.Int("invalid", batch.InvalidCount),
		zap.String("principal_id", principalID), zap.String("correlation_id", correlationID))
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, batch)
}

// ListJurisdictionImports handles GET /v1/admin/jurisdiction-imports.
func (h *Handler) ListJurisdictionImports(w http.ResponseWriter, r *http.Request) {
	s, ok := h.importStore(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "import_view"); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	status := r.URL.Query().Get("status")
	switch status {
	case "", domain.ImportProposed, domain.ImportApproved, domain.ImportRejected:
	default:
		writeError(w, r, http.StatusBadRequest, "invalid_status", "status must be PROPOSED, APPROVED or REJECTED")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, r, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	out, err := s.ListJurisdictionImports(r.Context(), status, limit)
	if err != nil {
		h.writeImportError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imports": out})
}

// GetJurisdictionImport handles GET /v1/admin/jurisdiction-imports/{batch_id}.
func (h *Handler) GetJurisdictionImport(w http.ResponseWriter, r *http.Request) {
	s, ok := h.importStore(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "import_view"); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	b, err := s.GetJurisdictionImport(r.Context(), chi.URLParam(r, "batch_id"))
	if err != nil {
		h.writeImportError(w, r, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// ApproveJurisdictionImport handles POST /v1/admin/jurisdiction-imports/{batch_id}/approve.
//
//	200 → approved and applied; the counts reconcile with the rows
//	403 → the approver proposed it
//	409 → already decided
//	422 → a row failed validation, or a row could not be applied (nothing was)
func (h *Handler) ApproveJurisdictionImport(w http.ResponseWriter, r *http.Request) {
	s, ok := h.importStore(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "import_approve"); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	var req DecideImportRequest
	if !decodeBody(w, r, &req) {
		return
	}

	var batch *domain.ImportBatch
	err := h.store.WithTransaction(r.Context(), func(q store.Querier) error {
		ctx := events.WithBatch(r.Context())
		b, changes, err := s.ApproveJurisdictionImportWithQuerier(r.Context(), q, domain.DecideImportParams{
			BatchID: chi.URLParam(r, "batch_id"), ActorID: principalID, Reason: req.Reason,
		})
		if err != nil {
			return err
		}
		batch = b
		src := importSource(b)
		for _, c := range changes {
			if err := h.announceImportChange(ctx, c, src, correlationID); err != nil {
				return err
			}
		}
		return h.publisher.FlushPending(ctx, q)
	})
	if err != nil {
		h.writeImportError(w, r, err, correlationID)
		return
	}
	h.log.Info("ApproveJurisdictionImport", zap.String("batch_id", batch.BatchID),
		zap.Intp("added", batch.Added), zap.Intp("updated", batch.Updated), zap.Intp("retired", batch.Retired),
		zap.String("principal_id", principalID), zap.String("correlation_id", correlationID))
	writeJSON(w, http.StatusOK, batch)
}

// announceImportChange emits the events one applied row produces: created for an
// add, deactivated for a retirement, hierarchy.changed for a move and updated
// for a changed name or authority. A move that also renames emits both.
func (h *Handler) announceImportChange(ctx context.Context, c domain.AppliedChange, src *events.ImportSource, correlationID string) error {
	switch c.Outcome {
	case domain.OutcomeAdded:
		return h.publisher.PublishJurisdictionCreated(ctx, *c.Jurisdiction, correlationID)
	case domain.OutcomeRetired:
		return h.publisher.PublishJurisdictionDeactivated(ctx, *c.Jurisdiction, correlationID)
	case domain.OutcomeUpdated:
		if c.ParentChanged {
			if pub, ok := h.publisher.(events.JurisdictionHierarchyPublisher); ok {
				if err := pub.PublishJurisdictionHierarchyChanged(ctx, *c.Jurisdiction, c.Before.ParentJurisdictionID, src, correlationID); err != nil {
					return err
				}
			}
		}
		if c.AttributesChanged {
			return h.publishUpdated(ctx, c.Jurisdiction, events.JurisdictionChange{
				Type: events.ChangeAttributesChanged, Before: c.Before, Import: src,
			}, correlationID)
		}
	}
	return nil
}

// RejectJurisdictionImport handles POST /v1/admin/jurisdiction-imports/{batch_id}/reject.
func (h *Handler) RejectJurisdictionImport(w http.ResponseWriter, r *http.Request) {
	s, ok := h.importStore(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction", "import_approve"); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}
	var req DecideImportRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var batch *domain.ImportBatch
	err := h.store.WithTransaction(r.Context(), func(q store.Querier) error {
		b, err := s.RejectJurisdictionImportWithQuerier(r.Context(), q, domain.DecideImportParams{
			BatchID: chi.URLParam(r, "batch_id"), ActorID: principalID, Reason: req.Reason,
		})
		batch = b
		return err
	})
	if err != nil {
		h.writeImportError(w, r, err, correlationID)
		return
	}
	h.log.Info("RejectJurisdictionImport", zap.String("batch_id", batch.BatchID),
		zap.String("principal_id", principalID), zap.String("correlation_id", correlationID))
	writeJSON(w, http.StatusOK, batch)
}
