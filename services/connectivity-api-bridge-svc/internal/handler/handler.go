package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/connectivity-api-bridge-svc/internal/authz"
	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/events"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
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
	r.Route("/v1/bridges", func(r chi.Router) {
		r.Post("/", h.CreateBridge)
		r.Get("/", h.ListBridges)
		r.Get("/{id}", h.GetByID)
		r.Post("/{id}/ingest", h.IngestPayload)
		r.Get("/{id}/logs", h.ListLogs)
	})
}

func (h *Handler) CreateBridge(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateBridgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.BridgeName == "" || req.EndpointURL == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, bridge_name, and endpoint_url are required")
		return
	}

	b := &domain.ApiBridge{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		BridgeName:    req.BridgeName,
		Protocol:      req.Protocol,
		EndpointURL:   req.EndpointURL,
		AuthType:      req.AuthType,
		Status:        domain.StatusActive,
	}

	if err := h.store.CreateBridge(r.Context(), b); err != nil {
		h.logger.Error("failed to create bridge", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create bridge")
		return
	}

	_ = h.publisher.Publish(r.Context(), "bridge.created", b.BridgeID, tenantID, b)
	writeJSON(w, http.StatusCreated, b)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	b, err := h.store.GetBridgeByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrBridgeNotFound) {
			writeError(w, http.StatusNotFound, "bridge not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get bridge")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *Handler) ListBridges(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	bridges, err := h.store.ListBridges(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list bridges")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"bridges": bridges,
		"total":   len(bridges),
	})
}

func (h *Handler) IngestPayload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	b, err := h.store.GetBridgeByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrBridgeNotFound) {
			writeError(w, http.StatusNotFound, "bridge not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify bridge")
		return
	}

	var req domain.IngestPayloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid payload body")
		return
	}

	log := &domain.IngestionLog{
		BridgeID:        b.BridgeID,
		TenantID:        tenantID,
		PayloadSummary:  req.PayloadSummary,
		IngestionStatus: domain.IngestionSuccess,
	}

	if err := h.store.RecordIngestion(r.Context(), log); err != nil {
		h.logger.Error("failed to record ingestion log", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to process ingestion")
		return
	}

	_ = h.publisher.Publish(r.Context(), "bridge.ingested", log.LogID, tenantID, log)
	writeJSON(w, http.StatusOK, log)
}

func (h *Handler) ListLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	logs, err := h.store.ListIngestionLogs(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list ingestion logs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"logs":  logs,
		"total": len(logs),
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
