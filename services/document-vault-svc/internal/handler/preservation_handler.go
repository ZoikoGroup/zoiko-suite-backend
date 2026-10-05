package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/document-vault-svc/internal/domain"
)

const (
	actionRenditionManage = "RENDITION_MANAGE"
	actionRenditionRead   = "RENDITION_READ"
	actionFixityManage    = "FIXITY_MANAGE"
	actionFixityRead      = "FIXITY_READ"
	actionRedactionManage = "REDACTION_MANAGE"
	actionRedactionRead   = "REDACTION_READ"
	actionExportManage    = "EXPORT_PACKAGE_MANAGE"
	actionExportRead      = "EXPORT_PACKAGE_READ"
)

// PreservationStore is DRC-04's own persistence contract — rendition,
// fixity, redaction and export-package management, kept separate from
// the existing Store/RecordsStore/RetentionStore interfaces.
type PreservationStore interface {
	CreateRendition(ctx context.Context, p domain.CreateRenditionParams) (*domain.Rendition, error)
	GetRendition(ctx context.Context, renditionID string) (*domain.Rendition, error)

	CreateFixityManifestForVersion(ctx context.Context, documentVersionID, sourceHash, principalID string) (*domain.FixityManifest, error)
	GetFixityManifest(ctx context.Context, manifestID string) (*domain.FixityManifest, error)
	GetFixityManifestByRendition(ctx context.Context, renditionID string) (*domain.FixityManifest, error)
	RecordVerification(ctx context.Context, p domain.RecordVerificationParams) (*domain.FixityManifest, error)
	StartRepair(ctx context.Context, p domain.StartRepairParams) (*domain.FixityManifest, error)

	CreateRedactionProfile(ctx context.Context, p domain.CreateRedactionProfileParams) (*domain.RedactionProfile, error)
	GetRedactionProfile(ctx context.Context, redactionID string) (*domain.RedactionProfile, error)

	CreateExportPackage(ctx context.Context, p domain.CreateExportPackageParams) (*domain.ExportPackage, error)
	GetExportPackage(ctx context.Context, packageID string) (*domain.ExportPackage, error)
}

func RegisterPreservationRoutes(r chi.Router, h *Handler, store PreservationStore) {
	ph := &preservationHandler{Handler: h, store: store}
	r.Route("/v1/renditions", func(r chi.Router) {
		r.Post("/", ph.CreateRendition)
		r.Get("/{rendition_id}", ph.GetRendition)
	})
	r.Route("/v1/fixity-manifests", func(r chi.Router) {
		r.Post("/", ph.CreateFixityManifestForVersion)
		r.Get("/{manifest_id}", ph.GetFixityManifest)
		r.Get("/by-rendition/{rendition_id}", ph.GetFixityManifestByRendition)
		r.Post("/{manifest_id}/verify", ph.RecordVerification)
		r.Post("/{manifest_id}/start-repair", ph.StartRepair)
	})
	r.Route("/v1/redaction-profiles", func(r chi.Router) {
		r.Post("/", ph.CreateRedactionProfile)
		r.Get("/{redaction_id}", ph.GetRedactionProfile)
	})
	r.Route("/v1/export-packages", func(r chi.Router) {
		r.Post("/", ph.CreateExportPackage)
		r.Get("/{package_id}", ph.GetExportPackage)
	})
}

type preservationHandler struct {
	*Handler
	store PreservationStore
}

// ── Renditions ───────────────────────────────────────────────────────────────

type createRenditionRequest struct {
	SourceDocumentVersionID string  `json:"source_document_version_id"`
	ParentRenditionID       *string `json:"parent_rendition_id,omitempty"`
	RenditionClass          string  `json:"rendition_class"`
	TransformationProfile   string  `json:"transformation_profile"`
	ChecksumSHA256          string  `json:"checksum_sha256"`
	StorageKey              string  `json:"storage_key"`
	SizeBytes               int64   `json:"size_bytes"`
	ContentType             string  `json:"content_type"`
}

func (h *preservationHandler) CreateRendition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createRenditionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SourceDocumentVersionID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "source_document_version_id")
		return
	}
	if !h.authorize(w, r, principalID, "", actionRenditionManage) {
		return
	}
	rd, err := h.store.CreateRendition(r.Context(), domain.CreateRenditionParams{
		SourceDocumentVersionID: req.SourceDocumentVersionID, ParentRenditionID: req.ParentRenditionID,
		RenditionClass: req.RenditionClass, TransformationProfile: req.TransformationProfile,
		ChecksumSHA256: req.ChecksumSHA256, StorageKey: req.StorageKey, SizeBytes: req.SizeBytes,
		ContentType: req.ContentType, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rd)
}

func (h *preservationHandler) GetRendition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRenditionRead) {
		return
	}
	rd, err := h.store.GetRendition(r.Context(), chi.URLParam(r, "rendition_id"))
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rd)
}

// ── Fixity Manifests ─────────────────────────────────────────────────────────

type createFixityManifestRequest struct {
	DocumentVersionID string `json:"document_version_id"`
	SourceHash        string `json:"source_hash"`
}

func (h *preservationHandler) CreateFixityManifestForVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createFixityManifestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DocumentVersionID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "document_version_id")
		return
	}
	if !h.authorize(w, r, principalID, "", actionFixityManage) {
		return
	}
	m, err := h.store.CreateFixityManifestForVersion(r.Context(), req.DocumentVersionID, req.SourceHash, principalID)
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *preservationHandler) GetFixityManifest(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionFixityRead) {
		return
	}
	m, err := h.store.GetFixityManifest(r.Context(), chi.URLParam(r, "manifest_id"))
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *preservationHandler) GetFixityManifestByRendition(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionFixityRead) {
		return
	}
	m, err := h.store.GetFixityManifestByRendition(r.Context(), chi.URLParam(r, "rendition_id"))
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type recordVerificationRequest struct {
	ObservedHash string `json:"observed_hash"`
	Outcome      string `json:"outcome"`
}

func (h *preservationHandler) RecordVerification(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req recordVerificationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !h.authorize(w, r, principalID, "", actionFixityManage) {
		return
	}
	m, err := h.store.RecordVerification(r.Context(), domain.RecordVerificationParams{
		ManifestID: chi.URLParam(r, "manifest_id"), ObservedHash: req.ObservedHash,
		ClaimedOutcome: req.Outcome, VerifiedByPrincipalID: principalID,
	})
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

type startRepairRequest struct {
	RepairSourceRef string `json:"repair_source_ref"`
}

func (h *preservationHandler) StartRepair(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req startRepairRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !h.authorize(w, r, principalID, "", actionFixityManage) {
		return
	}
	m, err := h.store.StartRepair(r.Context(), domain.StartRepairParams{
		ManifestID: chi.URLParam(r, "manifest_id"), RepairSourceRef: req.RepairSourceRef, StartedByPrincipalID: principalID,
	})
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// ── Redaction Profiles ───────────────────────────────────────────────────────

type createRedactionProfileRequest struct {
	RenditionID    string   `json:"rendition_id"`
	Purpose        string   `json:"purpose"`
	RecipientClass string   `json:"recipient_class"`
	FieldsRemoved  []string `json:"fields_removed"`
	LegalBasisRef  string   `json:"legal_basis_ref"`
}

func (h *preservationHandler) CreateRedactionProfile(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createRedactionProfileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RenditionID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "rendition_id")
		return
	}
	if !h.authorize(w, r, principalID, "", actionRedactionManage) {
		return
	}
	rp, err := h.store.CreateRedactionProfile(r.Context(), domain.CreateRedactionProfileParams{
		RenditionID: req.RenditionID, Purpose: req.Purpose, RecipientClass: req.RecipientClass,
		FieldsRemoved: req.FieldsRemoved, LegalBasisRef: req.LegalBasisRef, ApprovedByPrincipalID: principalID,
	})
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rp)
}

func (h *preservationHandler) GetRedactionProfile(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionRedactionRead) {
		return
	}
	rp, err := h.store.GetRedactionProfile(r.Context(), chi.URLParam(r, "redaction_id"))
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rp)
}

// ── Export Packages ──────────────────────────────────────────────────────────

type createExportPackageItemRequest struct {
	ItemType       string  `json:"item_type"`
	RefID          string  `json:"ref_id"`
	RedactionID    *string `json:"redaction_id,omitempty"`
	Omit           bool    `json:"omit"`
	OmissionReason string  `json:"omission_reason,omitempty"`
}

type createExportPackageRequest struct {
	Items []createExportPackageItemRequest `json:"items"`
}

func (h *preservationHandler) CreateExportPackage(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createExportPackageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !h.authorize(w, r, principalID, "", actionExportManage) {
		return
	}
	items := make([]domain.ExportPackageItemInput, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, domain.ExportPackageItemInput{
			ItemType: it.ItemType, RefID: it.RefID, RedactionID: it.RedactionID, Omit: it.Omit, OmissionReason: it.OmissionReason,
		})
	}
	pkg, err := h.store.CreateExportPackage(r.Context(), domain.CreateExportPackageParams{
		RequestedByPrincipalID: principalID, Items: items,
	})
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pkg)
}

func (h *preservationHandler) GetExportPackage(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, "", actionExportRead) {
		return
	}
	pkg, err := h.store.GetExportPackage(r.Context(), chi.URLParam(r, "package_id"))
	if err != nil {
		h.handlePreservationStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pkg)
}

func (h *preservationHandler) handlePreservationStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrRenditionNotFound), errors.Is(err, domain.ErrSourceDocumentVersionNotFound),
		errors.Is(err, domain.ErrParentRenditionNotFound), errors.Is(err, domain.ErrFixityManifestNotFound),
		errors.Is(err, domain.ErrExportPackageNotFound), errors.Is(err, domain.ErrExportItemRefNotFound),
		errors.Is(err, domain.ErrExportRedactionNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidRenditionClass), errors.Is(err, domain.ErrFixityInvalidOutcome),
		errors.Is(err, domain.ErrInvalidExportItemType), errors.Is(err, domain.ErrNoExportPackageItems),
		errors.Is(err, domain.ErrExportOmissionReasonRequired):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, domain.ErrFixityNotVerifiedOrRepairing), errors.Is(err, domain.ErrFixityNotFailed),
		errors.Is(err, domain.ErrFixityHashMismatchClaim), errors.Is(err, domain.ErrRenditionNotRedactedClass),
		errors.Is(err, domain.ErrRedactionProfileExists):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		h.handleStoreError(w, err)
	}
}
