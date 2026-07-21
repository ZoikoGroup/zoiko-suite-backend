package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/counterparty-management-svc/internal/authz"
	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/events"
	"zoiko.io/counterparty-management-svc/internal/middleware"
	"zoiko.io/counterparty-management-svc/internal/store"
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
	r.Route("/v1/counterparties", func(r chi.Router) {
		r.Post("/", h.CreateCounterparty)
		r.Get("/", h.ListCounterparties)
		r.Get("/{id}", h.GetCounterparty)
		r.Put("/{id}", h.UpdateCounterparty)
		r.Post("/{id}/compliance", h.UpdateComplianceStatus)
	})
}

func (h *Handler) CreateCounterparty(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateCounterpartyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.CounterpartyType == "" || req.JurisdictionID == "" {
		writeError(w, http.StatusBadRequest, "name, counterparty_type, and jurisdiction_id are required")
		return
	}

	c := &domain.Counterparty{
		TenantID:           tenantID,
		LegalEntityID:      req.LegalEntityID,
		Name:               req.Name,
		CounterpartyType:   req.CounterpartyType,
		RegistrationNumber: req.RegistrationNumber,
		TaxID:              req.TaxID,
		JurisdictionID:     req.JurisdictionID,
		RiskCategory:       req.RiskCategory,
		ContactEmail:       req.ContactEmail,
		Phone:              req.Phone,
		Address:            req.Address,
		EffectiveFrom:      req.EffectiveFrom,
		CreatedBy:          req.CreatedBy,
	}

	if err := h.store.CreateCounterparty(r.Context(), c); err != nil {
		h.logger.Error("create counterparty failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create counterparty")
		return
	}

	_ = h.publisher.Publish(r.Context(), "counterparty.created", c.CounterpartyID, tenantID, c)
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetCounterparty(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, err := h.store.GetCounterparty(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrCounterpartyNotFound) {
			writeError(w, http.StatusNotFound, "counterparty not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get counterparty")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListCounterparties(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	counterpartyType := r.URL.Query().Get("counterparty_type")
	status := r.URL.Query().Get("status")
	counterparties, err := h.store.ListCounterparties(r.Context(), legalEntityID, counterpartyType, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list counterparties")
		return
	}
	if counterparties == nil {
		counterparties = []domain.Counterparty{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"counterparties": counterparties, "total": len(counterparties)})
}

func (h *Handler) UpdateCounterparty(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetCounterparty(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrCounterpartyNotFound) {
			writeError(w, http.StatusNotFound, "counterparty not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch counterparty")
		return
	}

	var req domain.UpdateCounterpartyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.CounterpartyType != "" {
		existing.CounterpartyType = req.CounterpartyType
	}
	if req.RegistrationNumber != "" {
		existing.RegistrationNumber = req.RegistrationNumber
	}
	if req.TaxID != "" {
		existing.TaxID = req.TaxID
	}
	if req.JurisdictionID != "" {
		existing.JurisdictionID = req.JurisdictionID
	}
	if req.RiskCategory != "" {
		existing.RiskCategory = req.RiskCategory
	}
	if req.Status != "" {
		existing.Status = req.Status
	}
	if req.ContactEmail != "" {
		existing.ContactEmail = req.ContactEmail
	}
	if req.Phone != "" {
		existing.Phone = req.Phone
	}
	if req.Address != "" {
		existing.Address = req.Address
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateCounterparty(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update counterparty")
		return
	}

	_ = h.publisher.Publish(r.Context(), "counterparty.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) UpdateComplianceStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.UpdateComplianceStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ComplianceStatus == "" {
		writeError(w, http.StatusBadRequest, "compliance_status is required")
		return
	}

	c, err := h.store.UpdateComplianceStatus(r.Context(), id, req.ComplianceStatus)
	if err != nil {
		if errors.Is(err, domain.ErrCounterpartyNotFound) {
			writeError(w, http.StatusNotFound, "counterparty not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update compliance status")
		return
	}

	_ = h.publisher.Publish(r.Context(), "counterparty.compliance_updated", id, tenantID, c)
	writeJSON(w, http.StatusOK, c)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
