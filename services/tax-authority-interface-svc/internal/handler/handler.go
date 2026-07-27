package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tax-authority-interface-svc/internal/authz"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/events"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
	"zoiko.io/tax-authority-interface-svc/internal/store"
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
	r.Route("/v1/tax-authority", func(r chi.Router) {
		r.Post("/interfaces", h.CreateInterface)
		r.Get("/interfaces", h.ListInterfaces)
		r.Get("/interfaces/{id}", h.GetInterfaceByID)
		r.Post("/filings", h.SubmitTaxFiling)
		r.Get("/filings", h.ListSubmissions)
	})
}

func (h *Handler) CreateInterface(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateInterfaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.Jurisdiction == "" || req.AuthorityName == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction, and authority_name are required")
		return
	}

	tf := &domain.TaxInterface{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		Jurisdiction:  req.Jurisdiction,
		AuthorityName: req.AuthorityName,
		Protocol:      req.Protocol,
		Status:        "ACTIVE",
	}

	if err := h.store.CreateInterface(r.Context(), tf); err != nil {
		h.logger.Error("failed to create tax interface", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create tax interface")
		return
	}

	_ = h.publisher.Publish(r.Context(), "tax.interface.created", tf.InterfaceID, tenantID, tf)
	writeJSON(w, http.StatusCreated, tf)
}

func (h *Handler) GetInterfaceByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tf, err := h.store.GetInterfaceByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrInterfaceNotFound) {
			writeError(w, http.StatusNotFound, "tax interface not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get tax interface")
		return
	}
	writeJSON(w, http.StatusOK, tf)
}

func (h *Handler) ListInterfaces(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	interfaces, err := h.store.ListInterfaces(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tax interfaces")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"interfaces": interfaces,
		"total":      len(interfaces),
	})
}

func (h *Handler) SubmitTaxFiling(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.SubmitTaxFilingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.InterfaceID == "" || req.TaxPeriod == "" || req.FilingType == "" {
		writeError(w, http.StatusBadRequest, "interface_id, tax_period, and filing_type are required")
		return
	}

	if _, err := h.store.GetInterfaceByID(r.Context(), req.InterfaceID); err != nil {
		if errors.Is(err, domain.ErrInterfaceNotFound) {
			writeError(w, http.StatusNotFound, "tax interface not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify tax interface")
		return
	}

	sub := &domain.TaxFilingSubmission{
		InterfaceID:  req.InterfaceID,
		TenantID:     tenantID,
		TaxPeriod:    req.TaxPeriod,
		FilingType:   req.FilingType,
		TaxAmount:    req.TaxAmount,
		Status:       domain.TaxFilingSubmitted,
		AckReference: "TAX-ACK-991823",
	}

	if err := h.store.CreateSubmission(r.Context(), sub); err != nil {
		h.logger.Error("failed to create tax filing submission", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to submit tax filing")
		return
	}

	_ = h.publisher.Publish(r.Context(), "tax.filing.submitted", sub.SubmissionID, tenantID, sub)
	writeJSON(w, http.StatusCreated, sub)
}

func (h *Handler) ListSubmissions(w http.ResponseWriter, r *http.Request) {
	interfaceID := r.URL.Query().Get("interface_id")
	submissions, err := h.store.ListSubmissions(r.Context(), interfaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tax filing submissions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"submissions": submissions,
		"total":       len(submissions),
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
