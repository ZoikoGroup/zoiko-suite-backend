package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/esignature-integration-svc/internal/authz"
	"zoiko.io/esignature-integration-svc/internal/domain"
	"zoiko.io/esignature-integration-svc/internal/events"
	"zoiko.io/esignature-integration-svc/internal/middleware"
	"zoiko.io/esignature-integration-svc/internal/store"
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
	r.Route("/v1/esignature", func(r chi.Router) {
		r.Post("/envelopes", h.CreateEnvelope)
		r.Get("/envelopes", h.ListEnvelopes)
		r.Get("/envelopes/{id}", h.GetByID)
		r.Post("/envelopes/{id}/status", h.UpdateStatus)
		r.Patch("/envelopes/{id}/status", h.UpdateStatus)
	})
}

func (h *Handler) CreateEnvelope(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateEnvelopeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.DocumentTitle == "" || req.SignerEmail == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, document_title, and signer_email are required")
		return
	}

	env := &domain.SignatureEnvelope{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		Provider:      req.Provider,
		DocumentTitle: req.DocumentTitle,
		SignerEmail:   req.SignerEmail,
		SignerName:    req.SignerName,
		Status:        domain.EnvelopeSent,
	}

	if err := h.store.CreateEnvelope(r.Context(), env); err != nil {
		h.logger.Error("failed to create envelope", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create envelope")
		return
	}

	_ = h.publisher.Publish(r.Context(), "esignature.envelope.sent", env.EnvelopeID, tenantID, env)
	writeJSON(w, http.StatusCreated, env)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	env, err := h.store.GetEnvelopeByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrEnvelopeNotFound) {
			writeError(w, http.StatusNotFound, "envelope not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get envelope")
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (h *Handler) ListEnvelopes(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	envelopes, err := h.store.ListEnvelopes(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list envelopes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"envelopes": envelopes, "total": len(envelopes)})
}

func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.UpdateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Status == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}

	env, err := h.store.UpdateEnvelopeStatus(r.Context(), id, &req)
	if err != nil {
		if errors.Is(err, domain.ErrEnvelopeNotFound) {
			writeError(w, http.StatusNotFound, "envelope not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update envelope status")
		return
	}

	_ = h.publisher.Publish(r.Context(), "esignature.envelope."+req.Status, id, tenantID, env)
	writeJSON(w, http.StatusOK, env)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
