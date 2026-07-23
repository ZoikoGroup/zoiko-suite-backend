package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/external-data-feed-svc/internal/authz"
	"zoiko.io/external-data-feed-svc/internal/domain"
	"zoiko.io/external-data-feed-svc/internal/events"
	"zoiko.io/external-data-feed-svc/internal/middleware"
	"zoiko.io/external-data-feed-svc/internal/store"
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
	r.Route("/v1/external-data-feeds", func(r chi.Router) {
		r.Post("/subscriptions", h.CreateSubscription)
		r.Get("/subscriptions", h.ListSubscriptions)
		r.Get("/subscriptions/{id}", h.GetSubscriptionByID)
		r.Post("/events/ingest", h.IngestEvent)
		r.Get("/events", h.ListEvents)
	})
}

func (h *Handler) CreateSubscription(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.Provider == "" || req.FeedType == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, provider, and feed_type are required")
		return
	}

	sub := &domain.DataFeedSubscription{
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		Provider:      req.Provider,
		FeedType:      req.FeedType,
		Symbol:        req.Symbol,
		Status:        domain.FeedStatusActive,
	}

	if err := h.store.CreateSubscription(r.Context(), sub); err != nil {
		h.logger.Error("failed to create subscription", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create data feed subscription")
		return
	}

	_ = h.publisher.Publish(r.Context(), "external.feed.subscribed", sub.FeedID, tenantID, sub)
	writeJSON(w, http.StatusCreated, sub)
}

func (h *Handler) GetSubscriptionByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sub, err := h.store.GetSubscriptionByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrFeedNotFound) {
			writeError(w, http.StatusNotFound, "subscription not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get subscription")
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (h *Handler) ListSubscriptions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	subs, err := h.store.ListSubscriptions(r.Context(), legalEntityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list subscriptions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"subscriptions": subs, "total": len(subs)})
}

func (h *Handler) IngestEvent(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.IngestEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.FeedID == "" || req.EventType == "" {
		writeError(w, http.StatusBadRequest, "feed_id and event_type are required")
		return
	}

	// Verify subscription exists
	if _, err := h.store.GetSubscriptionByID(r.Context(), req.FeedID); err != nil {
		if errors.Is(err, domain.ErrFeedNotFound) {
			writeError(w, http.StatusNotFound, "feed subscription not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to verify feed subscription")
		return
	}

	ev := &domain.DataFeedEvent{
		FeedID:    req.FeedID,
		TenantID:  tenantID,
		EventType: req.EventType,
		Payload:   req.Payload,
	}

	if err := h.store.IngestEvent(r.Context(), ev); err != nil {
		h.logger.Error("failed to ingest event", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to ingest event")
		return
	}

	_ = h.publisher.Publish(r.Context(), "external.feed.event.ingested", ev.EventID, tenantID, ev)
	writeJSON(w, http.StatusCreated, ev)
}

func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	feedID := r.URL.Query().Get("feed_id")
	evts, err := h.store.ListEvents(r.Context(), feedID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"events": evts, "total": len(evts)})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
