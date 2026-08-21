package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/withholding-tax-svc/internal/authz"
	"zoiko.io/withholding-tax-svc/internal/domain"
	"zoiko.io/withholding-tax-svc/internal/events"
	"zoiko.io/withholding-tax-svc/internal/middleware"
	"zoiko.io/withholding-tax-svc/internal/store"
)

const (
	ActionWithholdingCalculate        = "WITHHOLDING_TAX_CALCULATE"
	ActionWithholdingObligationCreate = "WITHHOLDING_OBLIGATION_CREATE"
	ActionWithholdingObligationUpdate = "WITHHOLDING_OBLIGATION_UPDATE"
	ActionWithholdingObligationRemit  = "WITHHOLDING_OBLIGATION_REMIT"
	ActionWithholdingObligationCancel = "WITHHOLDING_OBLIGATION_CANCEL"
)

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/withholding-tax", func(r chi.Router) {
		r.Post("/calculate", h.Calculate)
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{id}", h.GetByID)
		r.Put("/{id}", h.Update)
		r.Post("/{id}/remit", h.Remit)
		r.Post("/{id}/cancel", h.Cancel)
	})
}

func (h *Handler) Calculate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.CalculateWithholdingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !h.authorize(w, r, principalID, req.LegalEntityID, ActionWithholdingCalculate) {
		return
	}

	if req.GrossPaymentAmount <= 0 {
		writeError(w, http.StatusBadRequest, "gross_payment_amount must be greater than 0")
		return
	}

	taxableBase := req.TaxableBaseAmount
	if taxableBase <= 0 {
		taxableBase = req.GrossPaymentAmount
	}

	var withheld float64
	treatyApplied := false
	if req.TaxTreatyExemption && req.ExemptionCertificateRef != "" {
		withheld = 0
		treatyApplied = true
	} else {
		withheld = taxableBase * (req.WithholdingRatePercent / 100.0)
	}

	res := domain.CalculateWithholdingResponse{
		GrossPaymentAmount:     req.GrossPaymentAmount,
		TaxableBaseAmount:      taxableBase,
		WithholdingRatePercent: req.WithholdingRatePercent,
		WithheldAmount:         withheld,
		Currency:               req.Currency,
		TaxTreatyApplied:       treatyApplied,
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "withholding_tax.calculated", ObligationID: req.PaymentReference, TenantID: tenantID,
		LegalEntityID: req.LegalEntityID, Jurisdiction: req.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: res,
	})
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.CreateObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.CounterpartyID == "" || req.PaymentReference == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, counterparty_id, and payment_reference are required")
		return
	}
	if req.GrossPaymentAmount <= 0 {
		writeError(w, http.StatusBadRequest, "gross_payment_amount must be greater than zero")
		return
	}

	if !h.authorize(w, r, principalID, req.LegalEntityID, ActionWithholdingObligationCreate) {
		return
	}

	o := &domain.WithholdingTaxObligation{
		TenantID:                tenantID,
		LegalEntityID:           req.LegalEntityID,
		JurisdictionID:          req.JurisdictionID,
		CounterpartyID:          req.CounterpartyID,
		PaymentReference:        req.PaymentReference,
		PaymentType:             req.PaymentType,
		GrossPaymentAmount:      req.GrossPaymentAmount,
		TaxableBaseAmount:       req.TaxableBaseAmount,
		WithholdingRatePercent:  req.WithholdingRatePercent,
		Currency:                req.Currency,
		TaxRuleID:               req.TaxRuleID,
		TaxTreatyExemption:      req.TaxTreatyExemption,
		ExemptionCertificateRef: req.ExemptionCertificateRef,
		Notes:                   req.Notes,
		EffectiveFrom:           req.EffectiveFrom,
		CreatedBy:               req.CreatedBy,
	}

	if err := h.store.Create(r.Context(), o); err != nil {
		h.logger.Error("create withholding tax obligation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create withholding tax obligation")
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "withholding_tax.created", ObligationID: o.ObligationID, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, Jurisdiction: o.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusCreated, o)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	o, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrObligationNotFound) {
			writeError(w, http.StatusNotFound, "withholding tax obligation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get withholding tax obligation")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	counterpartyID := r.URL.Query().Get("counterparty_id")
	status := r.URL.Query().Get("status")

	obligations, err := h.store.List(r.Context(), legalEntityID, jurisdictionID, counterpartyID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list withholding tax obligations")
		return
	}
	if obligations == nil {
		obligations = []domain.WithholdingTaxObligation{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"withholding_tax_obligations": obligations,
		"total":                       len(obligations),
	})
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrObligationNotFound) {
			writeError(w, http.StatusNotFound, "withholding tax obligation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch withholding tax obligation")
		return
	}

	if !h.authorize(w, r, principalID, existing.LegalEntityID, ActionWithholdingObligationUpdate) {
		return
	}

	var req domain.CreateObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existing.GrossPaymentAmount = req.GrossPaymentAmount
	existing.TaxableBaseAmount = req.TaxableBaseAmount
	existing.WithholdingRatePercent = req.WithholdingRatePercent
	existing.TaxTreatyExemption = req.TaxTreatyExemption
	existing.ExemptionCertificateRef = req.ExemptionCertificateRef
	if req.Notes != "" {
		existing.Notes = req.Notes
	}

	if err := h.store.Update(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update withholding tax obligation")
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "withholding_tax.updated", ObligationID: id, TenantID: tenantID,
		LegalEntityID: existing.LegalEntityID, Jurisdiction: existing.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: existing,
	})
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) Remit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrObligationNotFound) {
			writeError(w, http.StatusNotFound, "withholding tax obligation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch withholding tax obligation")
		return
	}

	if !h.authorize(w, r, principalID, existing.LegalEntityID, ActionWithholdingObligationRemit) {
		return
	}

	var req domain.RemitObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RemittanceReference == "" || req.RemittedBy == "" {
		writeError(w, http.StatusBadRequest, "remittance_reference and remitted_by are required")
		return
	}

	o, err := h.store.Remit(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeError(w, http.StatusNotFound, "withholding tax obligation not found")
		case errors.Is(err, domain.ErrAlreadyRemitted):
			writeError(w, http.StatusConflict, "withholding tax obligation is already remitted")
		case errors.Is(err, domain.ErrAlreadyCancelled):
			writeError(w, http.StatusConflict, "withholding tax obligation is cancelled")
		default:
			writeError(w, http.StatusInternalServerError, "failed to remit withholding tax obligation")
		}
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "withholding_tax.remitted", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, Jurisdiction: o.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrObligationNotFound) {
			writeError(w, http.StatusNotFound, "withholding tax obligation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch withholding tax obligation")
		return
	}

	if !h.authorize(w, r, principalID, existing.LegalEntityID, ActionWithholdingObligationCancel) {
		return
	}

	var req domain.CancelObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	o, err := h.store.Cancel(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeError(w, http.StatusNotFound, "withholding tax obligation not found")
		case errors.Is(err, domain.ErrAlreadyRemitted):
			writeError(w, http.StatusConflict, "cannot cancel remitted obligation")
		case errors.Is(err, domain.ErrAlreadyCancelled):
			writeError(w, http.StatusConflict, "obligation is already cancelled")
		default:
			writeError(w, http.StatusInternalServerError, "failed to cancel withholding tax obligation")
		}
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "withholding_tax.cancelled", ObligationID: id, TenantID: tenantID,
		LegalEntityID: o.LegalEntityID, Jurisdiction: o.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: o,
	})
	writeJSON(w, http.StatusOK, o)
}

// requirePrincipal extracts the caller's principal ID from the X-Principal-Id header.
// If missing, it writes a 401 response and returns ok=false.
func requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// authorize checks with authorization-svc whether principalID may perform actionType
// on legalEntityID. It writes the appropriate error response and returns ok=false if
// the action is not authorized (fail closed on any error, including unavailability).
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, actionType string) bool {
	err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionType)
	if err == nil {
		return true
	}
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "not authorized to perform this action")
		return false
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
	return false
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
