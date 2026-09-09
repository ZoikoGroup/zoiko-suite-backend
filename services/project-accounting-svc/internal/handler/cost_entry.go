package handler

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/project-accounting-svc/internal/domain"
)

// ── POST /v1/cost-entries ─────────────────────────────────────────────────────

// CaptureProjectCost is the one real create path — see migration 000002's
// doc comment. AllocateSharedCostToProject is a thin wrapper that
// pre-sets SourceType to ALLOCATION and calls the same internal logic.
func (h *Handler) CaptureProjectCost(w http.ResponseWriter, r *http.Request) {
	var req domain.CaptureProjectCostRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	h.captureProjectCost(w, r, req, actionProjectCostCapture)
}

func (h *Handler) AllocateSharedCostToProject(w http.ResponseWriter, r *http.Request) {
	var req domain.CaptureProjectCostRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.SourceType = domain.CostSourceTypeAllocation
	h.captureProjectCost(w, r, req, actionProjectCostCapture)
}

func (h *Handler) captureProjectCost(w http.ResponseWriter, r *http.Request, req domain.CaptureProjectCostRequest, action string) {
	if req.ProjectID == "" || req.SourceReference == "" || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id, source_reference and currency are required")
		return
	}
	if req.SourceType == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", domain.ErrSourceTypeRequired.Error())
		return
	}
	switch req.SourceType {
	case domain.CostSourceTypeAP, domain.CostSourceTypePayroll, domain.CostSourceTypeInventory,
		domain.CostSourceTypeAsset, domain.CostSourceTypeAllocation, domain.CostSourceTypeManual:
	default:
		writeError(w, http.StatusBadRequest, "invalid_source_type", "source_type must be one of AP, PAYROLL, INVENTORY, ASSET, ALLOCATION, MANUAL")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	transactionDate := time.Now().UTC()
	if req.TransactionDate != nil {
		transactionDate = *req.TransactionDate
	}
	e := &domain.CostEntry{
		EntryID: uuid.NewString(), LegalEntityID: p.LegalEntityID, ProjectID: req.ProjectID,
		SourceType: req.SourceType, SourceReference: req.SourceReference, CostCategory: req.CostCategory,
		Quantity: req.Quantity, Amount: req.Amount, Currency: req.Currency, TransactionDate: transactionDate,
		Status: domain.CostEntryStatusCaptured, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if req.WBSID != "" {
		e.WBSID = &req.WBSID
	}
	if err := h.store.CaptureProjectCost(r.Context(), e); err != nil {
		h.log.Error("failed to capture project cost", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishProjectCostCaptured(r.Context(), getCorrelationID(r), principalID, p.TenantID, *e)
	writeJSON(w, http.StatusCreated, e)
}

// ── GET /v1/cost-entries/{id}, GET /v1/cost-entries ──────────────────────────

func (h *Handler) GetCostEntry(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	e, err := h.store.GetCostEntry(r.Context(), id)
	if err != nil {
		h.writeCostEntryErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionProjectCostRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// ListCostEntries backs the spec's own GetProjectCosts/GetCostByWBS
// queries — filtered by ?project_id= and optionally further by
// ?wbs_id=.
func (h *Handler) ListCostEntries(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "project_id is required")
		return
	}
	wbsID := r.URL.Query().Get("wbs_id")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectCostRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListCostEntries(r.Context(), projectID, wbsID)
	if err != nil {
		h.log.Error("ListCostEntries: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.CostEntry{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST /v1/cost-entries/{id}/validate ───────────────────────────────────────

func (h *Handler) ValidateProjectCost(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	e, err := h.store.GetCostEntry(r.Context(), id)
	if err != nil {
		h.writeCostEntryErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionProjectCostCapture); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.ValidateProjectCost(r.Context(), id, time.Now().UTC()); err != nil {
		h.writeCostEntryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"entry_id": id, "status": domain.CostEntryStatusAccepted})
}

// ── POST /v1/cost-entries/{id}/mark-billable ──────────────────────────────────

func (h *Handler) MarkBillableEligibility(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.MarkBillableEligibilityRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	e, err := h.store.GetCostEntry(r.Context(), id)
	if err != nil {
		h.writeCostEntryErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, e.LegalEntityID, actionProjectCostCapture); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.MarkBillableEligibility(r.Context(), id, req.Billable, req.Capitalizable); err != nil {
		h.writeCostEntryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry_id": id, "billable": req.Billable, "capitalizable": req.Capitalizable})
}

// ── POST /v1/cost-entries/{id}/reclassify, /reverse ───────────────────────────

// ReclassifyProjectCost/ReverseProjectCost never touch the original
// entry — migration 000002's own reject-mutation trigger makes that
// structurally impossible — each creates and returns a brand-new linked
// entry. Both refuse self-approval against the ORIGINAL entry's own
// creator, the spec's own SoD.
func (h *Handler) ReclassifyProjectCost(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ReclassifyProjectCostRequest
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
	original, err := h.store.GetCostEntry(r.Context(), id)
	if err != nil {
		h.writeCostEntryErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, original.LegalEntityID, actionProjectCostAdjust); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var costCategory *string
	if req.CostCategory != "" {
		costCategory = &req.CostCategory
	}
	linked, err := h.store.CreateLinkedCostEntry(r.Context(), id, principalID, req.Reason, false, uuid.NewString(), req.Amount, costCategory, req.Billable, req.Capitalizable, time.Now().UTC())
	if err != nil {
		if errors.Is(err, domain.ErrSelfApprovalNotPermittedReclassify) {
			writeError(w, http.StatusForbidden, "self_approval_not_permitted", err.Error())
			return
		}
		h.writeCostEntryErr(w, err)
		return
	}
	h.publisher.PublishProjectCostReclassified(r.Context(), getCorrelationID(r), principalID, tenantID, *linked)
	writeJSON(w, http.StatusCreated, linked)
}

func (h *Handler) ReverseProjectCost(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ReverseProjectCostRequest
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
	original, err := h.store.GetCostEntry(r.Context(), id)
	if err != nil {
		h.writeCostEntryErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, original.LegalEntityID, actionProjectCostAdjust); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	linked, err := h.store.CreateLinkedCostEntry(r.Context(), id, principalID, req.Reason, true, uuid.NewString(), nil, nil, nil, nil, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrSelfApprovalNotPermittedReversal):
			writeError(w, http.StatusForbidden, "self_approval_not_permitted", err.Error())
		case errors.Is(err, domain.ErrCostEntryAlreadyReversed):
			writeError(w, http.StatusUnprocessableEntity, "already_reversed", err.Error())
		default:
			h.writeCostEntryErr(w, err)
		}
		return
	}
	h.publisher.PublishProjectCostReversed(r.Context(), getCorrelationID(r), principalID, tenantID, *linked)
	writeJSON(w, http.StatusCreated, linked)
}

// ── POST /v1/projects/{id}/certify-costs ──────────────────────────────────────

func (h *Handler) CertifyCostPopulation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectCostCertify); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	cert, err := h.store.CertifyCostPopulation(r.Context(), id, principalID, time.Now().UTC())
	if err != nil {
		h.log.Error("failed to certify cost population", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishProjectCostPopulationCertified(r.Context(), getCorrelationID(r), principalID, tenantID, p.LegalEntityID, *cert)
	writeJSON(w, http.StatusCreated, cert)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) writeCostEntryErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrCostEntryNotFound):
		writeError(w, http.StatusNotFound, "cost_entry_not_found", "")
	case errors.Is(err, domain.ErrInvalidCostEntryTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrCostEntryAlreadyReversed):
		writeError(w, http.StatusUnprocessableEntity, "already_reversed", err.Error())
	default:
		h.log.Error("project cost store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}
