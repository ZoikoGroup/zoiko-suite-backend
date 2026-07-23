package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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

	// Route alias for Postman compatibility
	r.Route("/v1/api-bridge/connections", func(r chi.Router) {
		r.Post("/", h.CreateBridge)
		r.Get("/", h.ListBridges)
		r.Get("/{id}", h.GetByID)
	})
}

func populateAliases(b *domain.ApiBridge) {
	if b == nil {
		return
	}
	b.ConnectionID = b.BridgeID
	if b.ProviderName == "" {
		b.ProviderName = b.BridgeName
	}
	if b.BaseURL == "" {
		b.BaseURL = b.EndpointURL
	}
}

func (h *Handler) CreateBridge(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateBridgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	bridgeName := req.BridgeName
	if bridgeName == "" {
		bridgeName = req.ProviderName
	}
	endpointURL := req.EndpointURL
	if endpointURL == "" {
		endpointURL = req.BaseURL
	}

	if req.LegalEntityID == "" || bridgeName == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and bridge_name/provider_name are required")
		return
	}

	bridge := &domain.ApiBridge{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		BridgeName:    bridgeName,
		ProviderName:  bridgeName,
		Protocol:      req.Protocol,
		EndpointURL:   endpointURL,
		BaseURL:       endpointURL,
		AuthType:      req.AuthType,
		Status:        domain.StatusActive,
	}

	if err := h.store.CreateBridge(r.Context(), bridge); err != nil {
		h.logger.Error("failed to create bridge endpoint", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create API bridge connection")
		return
	}

	populateAliases(bridge)
	_ = h.publisher.Publish(r.Context(), "connectivity.bridge.created", bridge.BridgeID, tenantID, bridge)
	writeJSON(w, http.StatusCreated, bridge)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	bridge, err := h.store.GetBridgeByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrBridgeNotFound) {
			writeError(w, http.StatusNotFound, "bridge endpoint not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch bridge endpoint")
		return
	}
	populateAliases(bridge)
	writeJSON(w, http.StatusOK, bridge)
}

func (h *Handler) ListBridges(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	bridges, err := h.store.ListBridges(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list bridge endpoints")
		return
	}
	for i := range bridges {
		populateAliases(&bridges[i])
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"bridges":     bridges,
		"connections": bridges,
		"total":       len(bridges),
	})
}

func (h *Handler) IngestPayload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.IngestPayloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.BridgeID = id

	log := &domain.IngestionLog{
		LogID:           uuid.New().String(),
		BridgeID:        req.BridgeID,
		TenantID:        tenantID,
		PayloadSummary:  req.PayloadSummary,
		IngestionStatus: domain.IngestionSuccess,
		IngestedAt:      time.Now().UTC(),
	}

	if err := h.store.RecordIngestion(r.Context(), log); err != nil {
		h.logger.Error("failed to log ingestion", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to ingest payload")
		return
	}

	_ = h.publisher.Publish(r.Context(), "connectivity.bridge.ingested", log.LogID, tenantID, log)
	writeJSON(w, http.StatusOK, log)
}

func (h *Handler) ListLogs(w http.ResponseWriter, r *http.Request) {
	bridgeID := chi.URLParam(r, "id")
	logs, err := h.store.ListIngestionLogs(r.Context(), bridgeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list ingestion logs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"logs": logs, "total": len(logs)})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
