package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
)

type Store interface {
	CreateNotification(ctx context.Context, n *domain.Notification) (created bool, err error)
	GetNotification(ctx context.Context, id string) (*domain.Notification, error)
	ListNotifications(ctx context.Context, legalEntityID, recipientPrincipalID, status string) ([]domain.Notification, error)
	CompleteDelivery(ctx context.Context, id, newStatus, failureReason string, sentAt *time.Time) error
}

type Publisher interface {
	PublishSent(ctx context.Context, correlationID string, n domain.Notification)
	PublishFailed(ctx context.Context, correlationID string, n domain.Notification, reason string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionSend = "NOTIFICATION_SEND"
	actionView = "NOTIFICATION_VIEW"
)

var supportedChannels = map[string]bool{
	"EMAIL":   true,
	"SMS":     true,
	"IN_APP":  true,
	"WEBHOOK": true,
}

// deliverStub is a documented stub delivery adapter — no real email/SMS/
// webhook provider is wired up on the platform yet. It "delivers" by logging,
// and always succeeds for a supported channel; an unsupported channel is the
// only way to exercise the FAILED path today. Replace with real provider
// integrations (SES, Twilio, etc.) when one exists — same "documented
// stub-first posture" convention used by accounts-payable-svc's authz client
// and vendor-due-diligence-svc's sanctions screening.
func deliverStub(log *zap.Logger, channel, recipient, subject string) (delivered bool, reason string) {
	if !supportedChannels[channel] {
		return false, "unsupported channel: " + channel
	}
	log.Info("stub delivery",
		zap.String("channel", channel),
		zap.String("recipient_principal_id", recipient),
		zap.String("subject", subject),
	)
	return true, "delivered via stub " + channel + " adapter"
}

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/notifications", func(r chi.Router) {
		r.Post("/", h.SendNotification)
		r.Get("/", h.ListNotifications)
		r.Get("/{id}", h.GetNotification)
	})
}

// ── POST /v1/notifications ────────────────────────────────────────────────────

// SendNotification records and "delivers" (via the stub adapter) a governed
// notification. Idempotent on (tenant_id, correlation_id): a retry replays
// the original delivery outcome rather than sending a second time.
//
// Delivery failure never surfaces as a 5xx to the caller — the critical
// constraint from the service's own spec (03-microservices.md §9.7) is that
// "notification failure must not collapse source operational workflows." A
// caller that failed to notify someone should see a normal 201 with
// status: FAILED, not an error that could make it treat its own — otherwise
// successful — operation as having failed too.
func (h *Handler) SendNotification(w http.ResponseWriter, r *http.Request) {
	var req domain.SendNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.RecipientPrincipalID == "" || req.LegalEntityID == "" || req.Channel == "" ||
		req.Subject == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"recipient_principal_id, legal_entity_id, channel, subject, correlation_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionSend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	correlationID := getCorrelationID(r)
	now := time.Now().UTC()

	notification := &domain.Notification{
		NotificationID:       uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		RecipientPrincipalID: req.RecipientPrincipalID,
		Channel:              req.Channel,
		Subject:              req.Subject,
		Body:                 req.Body,
		Status:               "PENDING",
		SourceEventType:      req.SourceEventType,
		SourceReference:      req.SourceReference,
		CorrelationID:        req.CorrelationID,
		CreatedByPrincipalID: principalID,
		CreatedAt:            now,
	}

	created, err := h.store.CreateNotification(r.Context(), notification)
	if err != nil {
		h.log.Error("failed to create notification", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if !created {
		// Replay: this correlation_id was already processed.
		writeJSON(w, http.StatusOK, notification)
		return
	}

	delivered, reason := deliverStub(h.log, req.Channel, req.RecipientPrincipalID, req.Subject)

	sentAt := time.Now().UTC()
	newStatus := "SENT"
	failureReason := ""
	if !delivered {
		newStatus = "FAILED"
		failureReason = reason
	}

	if err := h.store.CompleteDelivery(r.Context(), notification.NotificationID, newStatus, failureReason, &sentAt); err != nil {
		h.log.Error("failed to record delivery outcome", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	notification.Status = newStatus
	notification.FailureReason = failureReason
	notification.SentAt = &sentAt

	if delivered {
		h.publisher.PublishSent(r.Context(), correlationID, *notification)
	} else {
		h.publisher.PublishFailed(r.Context(), correlationID, *notification, reason)
	}

	writeJSON(w, http.StatusCreated, notification)
}

// ── GET /v1/notifications ─────────────────────────────────────────────────────

func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	recipientPrincipalID := r.URL.Query().Get("recipient_principal_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListNotifications(r.Context(), legalEntityID, recipientPrincipalID, status)
	if err != nil {
		h.log.Error("failed to list notifications", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.Notification{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/notifications/{id} ────────────────────────────────────────────────

func (h *Handler) GetNotification(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	notification, err := h.store.GetNotification(r.Context(), id)
	if errors.Is(err, domain.ErrNotificationNotFound) {
		writeError(w, http.StatusNotFound, "notification_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch notification", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, notification.LegalEntityID, actionView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, notification)
}

// ── Helpers ────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
