package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/corporate-tax-svc/internal/authz"
	"zoiko.io/corporate-tax-svc/internal/domain"
	"zoiko.io/corporate-tax-svc/internal/evidencereq"
	"zoiko.io/corporate-tax-svc/internal/middleware"
	"zoiko.io/corporate-tax-svc/internal/store"
)

// Action types passed to authorization-svc for each write endpoint.
const (
	ActionTaxReturnCreate   = "TAX_RETURN_CREATE"
	ActionTaxReturnUpdate   = "TAX_RETURN_UPDATE"
	ActionTaxReturnSubmit   = "TAX_RETURN_SUBMIT"
	ActionTaxReturnAssess   = "TAX_RETURN_ASSESS"
	ActionNexusRecordCreate = "NEXUS_RECORD_CREATE"
	ActionNexusRecordRead   = "NEXUS_RECORD_READ"
	ActionTaxReturnRead     = "TAX_RETURN_READ"
)

// Stable error code for a refused maker-checker step.
const codeSegregationOfDuties = "segregation_of_duties"

// AuthzChecker is the authorization-svc contract this handler depends on.
// authz.Client satisfies it; tests can substitute a stub.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

var _ AuthzChecker = (*authz.Client)(nil)

// Handler wires together the HTTP layer with the store. Domain events are
// written by the store to the transactional outbox inside the business
// transaction and delivered by internal/outbox.Relay.
type Handler struct {
	store    store.Store
	authz    AuthzChecker
	evidence EvidenceChecker
	logger   *zap.Logger
}

// EvidenceChecker is the evidence-requirements contract the handler depends
// on. A nil checker leaves the gate unwired (tests); production always wires
// it in main.
type EvidenceChecker interface {
	EvaluateSufficient(ctx context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, evidenceRefs []string) error
}

func New(st store.Store, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, logger: logger}
}

// WithEvidenceGate wires the evidence-requirements gate into return submission.
func (h *Handler) WithEvidenceGate(c EvidenceChecker) *Handler {
	h.evidence = c
	return h
}

func (h *Handler) checkEvidence(w http.ResponseWriter, r *http.Request, tenantID, legalEntityID, principalID string) bool {
	if h.evidence == nil {
		return true
	}
	err := h.evidence.EvaluateSufficient(r.Context(), tenantID, legalEntityID,
		"TAX", ActionTaxReturnSubmit, r.Header.Get("X-Correlation-ID"), principalID, evidenceRefsFromHeader(r.Header.Get("X-Evidence-Refs")))
	switch {
	case err == nil:
		return true
	case errors.Is(err, evidencereq.ErrEvidenceMissing):
		writeError(w, http.StatusUnprocessableEntity, "required evidence is missing")
	default:
		h.logger.Error("evidence gate unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "evidence service unavailable")
	}
	return false
}

func evidenceRefsFromHeader(raw string) []string {
	var refs []string
	for _, ref := range strings.Split(raw, ",") {
		if ref = strings.TrimSpace(ref); ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

// RegisterRoutes mounts all corporate-tax endpoints under a chi Router.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/corporate-tax-returns", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{id}", h.GetByID)
		r.Put("/{id}", h.Update)
		r.Post("/{id}/submit", h.Submit)
		r.Post("/{id}/assess", h.Assess)
	})
	r.Route("/v1/nexus-records", func(r chi.Router) {
		r.Post("/", h.CreateNexusRecord)
		r.Get("/", h.ListNexusRecords)
		r.Get("/{id}", h.GetNexusRecordByID)
	})
}

// principalIDOrUnauthorized extracts the caller's principal from the
// X-Principal-Id header, writing a 401 via writeError if it is missing.
func principalIDOrUnauthorized(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// checkAuthorized calls authorization-svc and fails closed: any error other
// than an explicit denial is treated as a service-unavailable (503), and a
// denial is a 403. Returns true if the caller may proceed.
func (h *Handler) checkAuthorized(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	principalID, ok := principalIDOrUnauthorized(w, r)
	if !ok {
		return
	}

	var req domain.CreateTaxReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.FiscalYear == 0 {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, and fiscal_year are required")
		return
	}

	if !h.checkAuthorized(w, r, principalID, req.LegalEntityID, ActionTaxReturnCreate) {
		return
	}

	ret := &domain.TaxReturn{
		LegalEntityID:         req.LegalEntityID,
		JurisdictionID:        req.JurisdictionID,
		TaxRegistrationNumber: req.TaxRegistrationNumber,
		FiscalYear:            req.FiscalYear,
		AccountingPeriodStart: req.AccountingPeriodStart,
		AccountingPeriodEnd:   req.AccountingPeriodEnd,
		GrossRevenue:          req.GrossRevenue,
		AllowableDeductions:   req.AllowableDeductions,
		TaxRatePercent:        req.TaxRatePercent,
		TaxCredits:            req.TaxCredits,
		TaxAlreadyPaid:        req.TaxAlreadyPaid,
		Currency:              req.Currency,
		Notes:                 req.Notes,
		EffectiveFrom:         req.EffectiveFrom,
		CreatedBy:             principalID,
	}

	if err := h.store.Create(r.Context(), ret, eventMeta(r, principalID)); err != nil {
		if errors.Is(err, domain.ErrDuplicateReturn) {
			writeError(w, http.StatusConflict, domain.ErrDuplicateReturn.Error())
			return
		}
		h.logger.Error("create corporate tax return failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create corporate tax return")
		return
	}

	writeJSON(w, http.StatusCreated, ret)
}

// authorizeRead authorizes a read of action in scope (the record's legal
// entity, a list's legal_entity_id filter, or the tenant for a tenant-wide
// list). Reads used to need nothing beyond X-Tenant-Id. Now: no principal is
// 401, a denied principal is 403, an unreachable authorization-svc is 503.
func (h *Handler) authorizeRead(w http.ResponseWriter, r *http.Request, scope, action string) bool {
	principalID, ok := principalIDOrUnauthorized(w, r)
	if !ok {
		return false
	}
	return h.checkAuthorized(w, r, principalID, scope, action)
}

// listScope is the authorization scope of a list: its legal_entity_id
// filter when given, else the caller's tenant.
func listScope(r *http.Request) string {
	if le := r.URL.Query().Get("legal_entity_id"); le != "" {
		return le
	}
	return middleware.GetTenantID(r.Context())
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	if _, ok := principalIDOrUnauthorized(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	ret, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch corporate tax return")
		return
	}
	if !h.authorizeRead(w, r, ret.LegalEntityID, ActionTaxReturnRead) {
		return
	}
	writeJSON(w, http.StatusOK, ret)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeRead(w, r, listScope(r), ActionTaxReturnRead) {
		return
	}
	q := r.URL.Query()
	fiscalYear, _ := strconv.Atoi(q.Get("fiscal_year"))

	returns, err := h.store.List(r.Context(), q.Get("legal_entity_id"), q.Get("jurisdiction_id"), q.Get("status"), fiscalYear)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list corporate tax returns")
		return
	}
	if returns == nil {
		returns = []domain.TaxReturn{}
	}
	// Envelope, like every other tax list (tax-rules, determinations, vat
	// returns): {corporate_tax_returns, total}. A bare array here broke the
	// contract callers already parse.
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"corporate_tax_returns": returns,
		"total":                 len(returns),
	})
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := principalIDOrUnauthorized(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch corporate tax return")
		return
	}

	if !h.checkAuthorized(w, r, principalID, existing.LegalEntityID, ActionTaxReturnUpdate) {
		return
	}

	if existing.Status != domain.StatusDraft {
		writeError(w, http.StatusConflict, "cannot update a non-draft return")
		return
	}

	var req domain.CreateTaxReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing.GrossRevenue = req.GrossRevenue
	existing.AllowableDeductions = req.AllowableDeductions
	existing.TaxRatePercent = req.TaxRatePercent
	existing.TaxCredits = req.TaxCredits
	existing.TaxAlreadyPaid = req.TaxAlreadyPaid
	existing.Notes = req.Notes

	if err := h.store.Update(r.Context(), existing, eventMeta(r, principalID)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update corporate tax return")
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) Submit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := principalIDOrUnauthorized(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch corporate tax return")
		return
	}

	if !h.checkAuthorized(w, r, principalID, existing.LegalEntityID, ActionTaxReturnSubmit) {
		return
	}

	if !h.checkEvidence(w, r, tenantID, existing.LegalEntityID, principalID) {
		return
	}

	ret, err := h.store.Submit(r.Context(), id, principalID, eventMeta(r, principalID))
	if err != nil {
		if errors.Is(err, domain.ErrAlreadySubmitted) {
			writeError(w, http.StatusConflict, "return has already been submitted")
			return
		}
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to submit corporate tax return")
		return
	}

	writeJSON(w, http.StatusOK, ret)
}

func (h *Handler) Assess(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := principalIDOrUnauthorized(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch corporate tax return")
		return
	}

	if !h.checkAuthorized(w, r, principalID, existing.LegalEntityID, ActionTaxReturnAssess) {
		return
	}
	// Segregation of duties: whoever submitted the return may not also record
	// its assessment. Re-checked by the store under the row lock.
	if existing.SubmittedBy != nil && *existing.SubmittedBy == principalID {
		writeCodedError(w, http.StatusForbidden, codeSegregationOfDuties, domain.ErrSegregationOfDuties.Error())
		return
	}

	var req domain.AssessTaxReturnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AssessmentReference == "" {
		writeError(w, http.StatusBadRequest, "assessment_reference is required")
		return
	}

	ret, err := h.store.Assess(r.Context(), id, &req, principalID, eventMeta(r, principalID))
	if err != nil {
		if errors.Is(err, domain.ErrSegregationOfDuties) {
			writeCodedError(w, http.StatusForbidden, codeSegregationOfDuties, domain.ErrSegregationOfDuties.Error())
			return
		}
		if errors.Is(err, domain.ErrTaxReturnNotFound) {
			writeError(w, http.StatusNotFound, "corporate tax return not found")
			return
		}
		if errors.Is(err, domain.ErrInvalidStatusTransition) {
			writeError(w, http.StatusConflict, "only SUBMITTED corporate tax returns can be assessed")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to record assessment")
		return
	}
	writeJSON(w, http.StatusOK, ret)
}

func (h *Handler) CreateNexusRecord(w http.ResponseWriter, r *http.Request) {
	principalID, ok := principalIDOrUnauthorized(w, r)
	if !ok {
		return
	}

	var req domain.CreateNexusRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.NexusType == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, nexus_type, and effective_from are required")
		return
	}

	if !h.checkAuthorized(w, r, principalID, req.LegalEntityID, ActionNexusRecordCreate) {
		return
	}

	record := &domain.NexusRecord{
		LegalEntityID:   req.LegalEntityID,
		JurisdictionID:  req.JurisdictionID,
		NexusType:       req.NexusType,
		ThresholdAmount: req.ThresholdAmount,
		Currency:        req.Currency,
		EffectiveFrom:   req.EffectiveFrom,
		EffectiveTo:     req.EffectiveTo,
		CreatedBy:       principalID,
	}

	if err := h.store.CreateNexusRecord(r.Context(), record); err != nil {
		h.logger.Error("failed to create nexus record", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create nexus record")
		return
	}

	writeJSON(w, http.StatusCreated, record)
}

func (h *Handler) GetNexusRecordByID(w http.ResponseWriter, r *http.Request) {
	if _, ok := principalIDOrUnauthorized(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	record, err := h.store.GetNexusRecordByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNexusRecordNotFound) {
			writeError(w, http.StatusNotFound, "nexus record not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get nexus record")
		return
	}
	if !h.authorizeRead(w, r, record.LegalEntityID, ActionNexusRecordRead) {
		return
	}

	writeJSON(w, http.StatusOK, record)
}

func (h *Handler) ListNexusRecords(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeRead(w, r, listScope(r), ActionNexusRecordRead) {
		return
	}
	q := r.URL.Query()
	records, err := h.store.ListNexusRecords(r.Context(), q.Get("legal_entity_id"), q.Get("jurisdiction_id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list nexus records")
		return
	}
	if records == nil {
		records = []domain.NexusRecord{}
	}
	writeJSON(w, http.StatusOK, records)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeCodedError is writeError plus a stable machine-readable code.
func writeCodedError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// eventMeta is what the outbox event of a write records about the request.
func eventMeta(r *http.Request, principalID string) store.EventMeta {
	return store.EventMeta{ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID")}
}
