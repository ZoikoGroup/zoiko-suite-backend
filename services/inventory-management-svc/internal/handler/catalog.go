package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
)

// ── POST /v1/catalog/offerings ──────────────────────────────────────────────

func (h *Handler) CreateOffering(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateOfferingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.SKUCode == "" || req.Category == "" || req.OwnerPrincipalID == "" || req.Description == "" || req.Unit == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, sku_code, category, owner_principal_id, description and unit are required")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCatalogManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	o := &domain.Offering{
		OfferingID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID,
		SKUCode: req.SKUCode, Category: req.Category, OwnerPrincipalID: req.OwnerPrincipalID,
		CreatedAt: now, CreatedByPrincipalID: principalID,
	}
	v := &domain.OfferingVersion{
		VersionID: uuid.NewString(), TenantID: tenantID, OfferingID: o.OfferingID, VersionNumber: 1,
		Description: req.Description, Unit: req.Unit, AvailabilityRules: req.AvailabilityRules,
		Status: domain.CatalogVersionStatusDraft, CreatedAt: now, CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateOffering(r.Context(), o, v, req.Variants); err != nil {
		if errors.Is(err, domain.ErrDuplicateOfferingSKU) {
			writeError(w, http.StatusUnprocessableEntity, "duplicate_sku", err.Error())
			return
		}
		h.log.Error("failed to create offering", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishOfferingCreated(r.Context(), getCorrelationID(r), principalID, *o, *v)
	writeJSON(w, http.StatusCreated, map[string]any{"offering": o, "version": v})
}

// ── GET /v1/catalog/offerings/{id} ──────────────────────────────────────────

func (h *Handler) GetOffering(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	o, err := h.store.GetOffering(r.Context(), id)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	v, err := h.store.GetCurrentOfferingVersion(r.Context(), id)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"offering": o, "current_version": v})
}

// ── GET /v1/catalog/offerings ────────────────────────────────────────────────

func (h *Handler) SearchCatalog(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	category := r.URL.Query().Get("category")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCatalogRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.SearchCatalog(r.Context(), legalEntityID, category)
	if err != nil {
		h.log.Error("SearchCatalog: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.Offering{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/catalog/offerings/{id}/version-as-of ────────────────────────────

// GetVersionAsOf is the real implementation of the doc's own invariant:
// "product/service offering versions used by transactions SHALL be
// pinned; later catalog edits SHALL NOT rewrite historical transaction
// meaning." Returns CATALOG_VERSION_INVALID if no version was active at
// the requested instant.
func (h *Handler) GetVersionAsOf(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	atParam := r.URL.Query().Get("at")
	if atParam == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "at is required")
		return
	}
	at, err := time.Parse(time.RFC3339, atParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_at", "at must be an RFC3339 timestamp")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	o, err := h.store.GetOffering(r.Context(), id)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	v, err := h.store.GetVersionAsOf(r.Context(), id, at)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ── POST /v1/catalog/offerings/{id}/versions ────────────────────────────────

func (h *Handler) CreateCatalogVersion(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.CreateVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Description == "" || req.Unit == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "description and unit are required")
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
	o, err := h.store.GetOffering(r.Context(), id)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	v := &domain.OfferingVersion{
		VersionID: uuid.NewString(), TenantID: tenantID, OfferingID: id,
		Description: req.Description, Unit: req.Unit, AvailabilityRules: req.AvailabilityRules,
		Status: domain.CatalogVersionStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateVersion(r.Context(), v, req.Variants); err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// ── GET /v1/catalog/offerings/{id}/versions/{versionID} ────────────────────

func (h *Handler) GetOfferingVersion(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	v, err := h.store.GetOfferingVersion(r.Context(), versionID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	o, err := h.store.GetOffering(r.Context(), v.OfferingID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ── GET /v1/catalog/offerings/{id}/versions/{versionID}/variants ───────────

func (h *Handler) ListVariants(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	v, err := h.store.GetOfferingVersion(r.Context(), versionID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	o, err := h.store.GetOffering(r.Context(), v.OfferingID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListVariants(r.Context(), versionID)
	if err != nil {
		h.log.Error("ListVariants: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.CatalogVariant{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/catalog/offerings/{id}/versions/{versionID}/approve ───────────

func (h *Handler) ApproveOffering(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	v, err := h.store.GetOfferingVersion(r.Context(), versionID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	o, err := h.store.GetOffering(r.Context(), v.OfferingID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	// The doc's own authorization note: "Product/business owner +
	// specialist review for tax/accounting/legal mappings; no
	// self-activation when policy requires review" — approval is gated on
	// a distinct action from plain management.
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveOfferingVersion(r.Context(), versionID, principalID, now); err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	v.Status, v.ApprovedAt, v.ApprovedByPrincipalID = domain.CatalogVersionStatusApproved, &now, &principalID
	writeJSON(w, http.StatusOK, v)
}

// ── POST /v1/catalog/offerings/{id}/versions/{versionID}/activate ──────────

// ActivateOffering moves the version APPROVED -> ACTIVE and, in the same
// store transaction, forces any other currently-ACTIVE version of the
// same offering to SUPERSEDED — the real mechanism behind
// OfferingVersionSuperseded and "historical transactions pin the
// offering version."
func (h *Handler) ActivateOffering(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	versionID := chi.URLParam(r, "versionID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	v, err := h.store.GetOfferingVersion(r.Context(), versionID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	o, err := h.store.GetOffering(r.Context(), id)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	supersededID, err := h.store.ActivateOfferingVersion(r.Context(), id, versionID, principalID, now)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	v.Status, v.ActivatedAt, v.ActivatedByPrincipalID = domain.CatalogVersionStatusActive, &now, &principalID
	h.publisher.PublishOfferingActivated(r.Context(), getCorrelationID(r), principalID, tenantID, o.LegalEntityID, *v)
	if supersededID != nil {
		h.publisher.PublishOfferingVersionSuperseded(r.Context(), getCorrelationID(r), principalID, tenantID, o.LegalEntityID, id, *supersededID)
	}
	writeJSON(w, http.StatusOK, v)
}

// ── POST /v1/catalog/offerings/{id}/versions/{versionID}/suspend ───────────

func (h *Handler) SuspendOfferingVersion(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	var req domain.SuspendOfferingVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
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
	v, err := h.store.GetOfferingVersion(r.Context(), versionID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	o, err := h.store.GetOffering(r.Context(), v.OfferingID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.SuspendOfferingVersion(r.Context(), versionID, principalID, req.Reason, now); err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	v.Status, v.SuspendedAt, v.SuspendedByPrincipalID, v.SuspensionReason = domain.CatalogVersionStatusSuspended, &now, &principalID, &req.Reason
	h.publisher.PublishOfferingSuspended(r.Context(), getCorrelationID(r), principalID, tenantID, o.LegalEntityID, *v)
	writeJSON(w, http.StatusOK, v)
}

// ── POST /v1/catalog/offerings/{id}/versions/{versionID}/retire ────────────

func (h *Handler) RetireOfferingVersion(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	var req domain.RetireOfferingVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
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
	v, err := h.store.GetOfferingVersion(r.Context(), versionID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	o, err := h.store.GetOffering(r.Context(), v.OfferingID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.RetireOfferingVersion(r.Context(), versionID, principalID, req.Reason, now); err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	v.Status, v.RetiredAt, v.RetiredByPrincipalID, v.RetirementReason = domain.CatalogVersionStatusRetired, &now, &principalID, &req.Reason
	h.publisher.PublishOfferingRetired(r.Context(), getCorrelationID(r), principalID, tenantID, o.LegalEntityID, *v)
	writeJSON(w, http.StatusOK, v)
}

// ── POST /v1/catalog/offerings/{id}/versions/{versionID}/mappings ──────────

// LinkMapping never touches the version's own pinned content — see
// migration 000006's doc comment. The doc's own SoD note applies here:
// tax/accounting/product mappings require specialist review, gated on a
// distinct action from plain catalog management.
func (h *Handler) LinkMapping(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	var req domain.LinkMappingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !domain.ValidCatalogMappingType(req.MappingType) {
		writeError(w, http.StatusBadRequest, "invalid_mapping_type", domain.ErrInvalidMappingType.Error())
		return
	}
	if req.MappingRef == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "mapping_ref is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	v, err := h.store.GetOfferingVersion(r.Context(), versionID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	o, err := h.store.GetOffering(r.Context(), v.OfferingID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogMappingLink); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	m := &domain.CatalogMapping{
		MappingID: uuid.NewString(), VersionID: versionID, MappingType: req.MappingType, MappingRef: req.MappingRef,
		LinkedAt: time.Now().UTC(), LinkedByPrincipalID: principalID,
	}
	if err := h.store.LinkMapping(r.Context(), m); err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// ── GET /v1/catalog/offerings/{id}/versions/{versionID}/mappings ───────────

func (h *Handler) GetMappings(w http.ResponseWriter, r *http.Request) {
	versionID := chi.URLParam(r, "versionID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	v, err := h.store.GetOfferingVersion(r.Context(), versionID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	o, err := h.store.GetOffering(r.Context(), v.OfferingID)
	if err != nil {
		h.writeCatalogErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, actionCatalogRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.GetMappings(r.Context(), versionID)
	if err != nil {
		h.log.Error("GetMappings: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.CatalogMapping{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeCatalogErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrOfferingNotFound):
		writeError(w, http.StatusNotFound, "offering_not_found", "")
	case errors.Is(err, domain.ErrOfferingVersionNotFound):
		writeError(w, http.StatusNotFound, "offering_version_not_found", "")
	case errors.Is(err, domain.ErrInvalidCatalogVersionTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrCatalogVersionInvalid):
		writeError(w, http.StatusUnprocessableEntity, "CATALOG_VERSION_INVALID", err.Error())
	default:
		h.log.Error("catalog store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
