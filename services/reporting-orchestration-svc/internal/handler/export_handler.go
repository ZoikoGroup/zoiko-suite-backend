package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/reporting-orchestration-svc/internal/archivestore"
	"zoiko.io/reporting-orchestration-svc/internal/domain"
	"zoiko.io/reporting-orchestration-svc/internal/middleware"
	"zoiko.io/reporting-orchestration-svc/internal/retention"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

const (
	AUDIT_EXPORT_REQUEST = "AUDIT_EXPORT_REQUEST"
	AUDIT_EXPORT_APPROVE = "AUDIT_EXPORT_APPROVE"
	AUDIT_EXPORT_DELIVER = "AUDIT_EXPORT_DELIVER"
)

// exportHandler is AUD-10's export/redact/deliver HTTP surface — kept as
// its own struct embedding *Handler (same pattern used for AUD-05/06/08's
// pbc/evidence/finding handlers in the other services this build touched)
// so its dependencies (the archive and retention clients) don't leak into
// the pre-existing report-definition/report-run handler.
type exportHandler struct {
	*Handler
	exports    store.ExportStore
	archives   archivestore.Client
	retentions retention.Client
}

// RegisterExportRoutes mounts AUD-10's export routes on r.
func RegisterExportRoutes(r chi.Router, h *Handler, exports store.ExportStore, archives archivestore.Client, retentions retention.Client) {
	eh := &exportHandler{Handler: h, exports: exports, archives: archives, retentions: retentions}
	r.With(middleware.TenantMiddleware).Route("/v1/audit-exports", func(r chi.Router) {
		r.Post("/", eh.createExport)
		r.Get("/{id}", eh.getExport)
		r.Post("/{id}/approve", eh.approveExport)
		r.Post("/{id}/build", eh.buildExport)
		r.Post("/{id}/redact", eh.redactExport)
		r.Post("/{id}/seal", eh.sealExport)
		r.Post("/{id}/deliver", eh.deliverExport)
		r.Post("/{id}/revoke", eh.revokeExport)
		r.Get("/{id}/manifest", eh.getManifest)
	})
}

type createExportRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	ArchiveID     string `json:"archive_id"`
	Purpose       string `json:"purpose"`
}

func (h *exportHandler) createExport(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req createExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ArchiveID == "" || req.LegalEntityID == "" {
		h.errJSON(w, http.StatusBadRequest, "legal_entity_id and archive_id are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, AUDIT_EXPORT_REQUEST); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	e, err := h.exports.CreateExportRequest(r.Context(), domain.CreateExportRequestParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, ArchiveID: req.ArchiveID,
		Purpose: req.Purpose, RequestedByPrincipalID: principalID,
	})
	if err != nil {
		h.logger.Error("failed to create audit export request", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to create audit export request")
		return
	}
	h.okJSON(w, http.StatusCreated, e)
}

func (h *exportHandler) getExport(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	e, err := h.exports.GetExportRequest(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	h.okJSON(w, http.StatusOK, e)
}

func (h *exportHandler) approveExport(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	exportID := chi.URLParam(r, "id")
	existing, err := h.exports.GetExportRequest(r.Context(), tenantID, exportID)
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, AUDIT_EXPORT_APPROVE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	e, err := h.exports.ApproveExport(r.Context(), domain.ApproveExportParams{
		ExportID: exportID, TenantID: tenantID, ApprovedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	h.okJSON(w, http.StatusOK, e)
}

// buildExport is BuildExportPackage: it fetches the referenced archive's
// REAL record from audit-event-store-svc and records it as this export's
// manifest entry. If the archive cannot be fetched, the export is marked
// FAILED rather than sealed around fabricated content — see
// internal/domain/export.go's own package doc.
func (h *exportHandler) buildExport(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	exportID := chi.URLParam(r, "id")
	existing, err := h.exports.GetExportRequest(r.Context(), tenantID, exportID)
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	if err := h.exports.MarkExportBuilding(r.Context(), tenantID, exportID); err != nil {
		h.writeExportErr(w, err)
		return
	}

	archive, err := h.archives.GetArchive(r.Context(), existing.ArchiveID, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		_ = h.exports.MarkExportFailed(r.Context(), tenantID, exportID, err.Error())
		h.errJSON(w, http.StatusFailedDependency, domain.ErrArchiveFetchFailed.Error())
		return
	}

	if _, err := h.exports.RecordManifestEntry(r.Context(), tenantID, exportID, "audit-archive-"+archive.ArchiveID, archive.ArchiveDigest); err != nil {
		h.logger.Error("failed to record export manifest entry", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "failed to record export manifest entry")
		return
	}
	e, err := h.exports.GetExportRequest(r.Context(), tenantID, exportID)
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	h.okJSON(w, http.StatusOK, e)
}

type redactRequest struct {
	FieldOrScope string `json:"field_or_scope"`
	Reason       string `json:"reason"`
}

func (h *exportHandler) redactExport(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req redactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FieldOrScope == "" || req.Reason == "" {
		h.errJSON(w, http.StatusBadRequest, "field_or_scope and reason are required")
		return
	}
	d, err := h.exports.RecordRedaction(r.Context(), domain.RecordRedactionParams{
		ExportID: chi.URLParam(r, "id"), TenantID: tenantID,
		FieldOrScope: req.FieldOrScope, Reason: req.Reason, DecidedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	h.okJSON(w, http.StatusCreated, d)
}

func (h *exportHandler) sealExport(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	e, err := h.exports.SealExport(r.Context(), domain.SealExportParams{ExportID: chi.URLParam(r, "id"), TenantID: tenantID})
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	h.okJSON(w, http.StatusOK, e)
}

type deliverRequest struct {
	DeliveredToPrincipalID string `json:"delivered_to_principal_id"`
	DeliveryChannel        string `json:"delivery_channel"`
}

// deliverExport is the delivery gate: legal-hold check against
// retention-registry-svc (fail closed) then recipient authorization, both
// before DeliverExport ever runs.
func (h *exportHandler) deliverExport(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	exportID := chi.URLParam(r, "id")
	existing, err := h.exports.GetExportRequest(r.Context(), tenantID, exportID)
	if err != nil {
		h.writeExportErr(w, err)
		return
	}

	blocked, err := h.retentions.IsBlocked(r.Context(), tenantID, "AUDIT_EXPORT", existing.ArchiveID, principalID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.logger.Error("retention-registry-svc unavailable during export delivery", zap.Error(err))
		h.errJSON(w, http.StatusServiceUnavailable, domain.ErrRetentionServiceDown.Error())
		return
	}
	if blocked {
		h.errJSON(w, http.StatusConflict, domain.ErrLegalHoldActive.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, AUDIT_EXPORT_DELIVER); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	var req deliverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeliveredToPrincipalID == "" || req.DeliveryChannel == "" {
		h.errJSON(w, http.StatusBadRequest, "delivered_to_principal_id and delivery_channel are required")
		return
	}
	e, err := h.exports.DeliverExport(r.Context(), domain.DeliverExportParams{
		ExportID: exportID, TenantID: tenantID,
		DeliveredToPrincipalID: req.DeliveredToPrincipalID, DeliveryChannel: req.DeliveryChannel,
	})
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	h.okJSON(w, http.StatusOK, e)
}

func (h *exportHandler) revokeExport(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	e, err := h.exports.RevokeExport(r.Context(), domain.RevokeExportParams{ExportID: chi.URLParam(r, "id"), TenantID: tenantID})
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	h.okJSON(w, http.StatusOK, e)
}

func (h *exportHandler) getManifest(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())
	list, err := h.exports.ListManifest(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		h.writeExportErr(w, err)
		return
	}
	h.okJSON(w, http.StatusOK, list)
}

func (h *exportHandler) writeExportErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrExportNotFound):
		h.errJSON(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrExportInvalidState):
		h.errJSON(w, http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrExportSelfApproval):
		h.errJSON(w, http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrManifestRequired):
		h.errJSON(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("audit export operation failed", zap.Error(err))
		h.errJSON(w, http.StatusInternalServerError, "an unexpected error occurred")
	}
}
