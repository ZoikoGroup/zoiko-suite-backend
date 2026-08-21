package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
	ListNotifications(ctx context.Context, f domain.ListFilter) ([]domain.Notification, error)
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

// Deliverer hands a notification to a delivery provider. It is an interface,
// not a function, so the FAILED path can be exercised by a test double that
// genuinely refuses — see StubDeliverer for why that mattered.
type Deliverer interface {
	Deliver(ctx context.Context, n domain.Notification) (delivered bool, reason string)
}

// StubDeliverer is a documented stub delivery adapter — no real email/SMS/
// webhook provider is wired up on the platform yet. It "delivers" by logging
// and always succeeds. Replace with real provider integrations (SES, Twilio,
// etc.) when one exists — the same "documented stub-first posture" convention
// used by vendor-due-diligence-svc's sanctions screening.
//
// It used to double as the channel validator, returning FAILED for a channel
// it did not recognise. That made a caller's typo ("EMIAL") into a permanent
// FAILED delivery record plus a notification.failed event on the bus —
// evidence that a delivery was attempted and refused by a provider, for
// something no provider ever saw. Channel support is now checked at the
// request boundary and answers 400; this adapter only reports what an actual
// delivery attempt did.
type StubDeliverer struct{ Log *zap.Logger }

func (d StubDeliverer) Deliver(_ context.Context, n domain.Notification) (bool, string) {
	d.Log.Info("stub delivery",
		zap.String("channel", n.Channel),
		zap.String("recipient_principal_id", n.RecipientPrincipalID),
		zap.String("subject", n.Subject),
	)
	return true, "delivered via stub " + n.Channel + " adapter"
}

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	deliverer Deliverer
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, deliverer Deliverer, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, deliverer: deliverer, log: log}
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
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.RecipientPrincipalID == "" || req.LegalEntityID == "" || req.Channel == "" ||
		req.Subject == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"recipient_principal_id, legal_entity_id, channel, subject, correlation_id are required")
		return
	}

	// An unrecognised channel is a caller mistake, not a delivery failure.
	if !supportedChannels[req.Channel] {
		writeError(w, http.StatusBadRequest, "unsupported_channel",
			"channel must be one of EMAIL, SMS, IN_APP, WEBHOOK")
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionSend); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

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

	delivered, reason := h.deliverer.Deliver(r.Context(), *notification)

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

// ListNotifications returns the notifications the caller is entitled to read.
//
// The authorization used to be conditional on the filter: CheckAllowed ran
// only when legal_entity_id was supplied, so OMITTING the filter — the easier
// request to make — returned every notification in the tenant, across every
// legal entity, with subjects and bodies, to any principal holding no grant at
// all. A read is authorized by who is asking, never by which query parameters
// they happened to send.
//
// Two entitlements, both explicit:
//   - legal_entity_id supplied → NOTIFICATION_VIEW must be granted on it.
//   - legal_entity_id omitted  → the caller's own inbox. The recipient filter
//     is forced to the calling principal, and any recipient_principal_id they
//     asked for that is not themselves is refused rather than silently
//     rewritten.
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	filter := domain.ListFilter{
		LegalEntityID:        r.URL.Query().Get("legal_entity_id"),
		RecipientPrincipalID: r.URL.Query().Get("recipient_principal_id"),
		Status:               r.URL.Query().Get("status"),
	}

	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	filter.Limit, filter.Offset = limit, offset

	if filter.LegalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, filter.LegalEntityID, actionView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	} else {
		if filter.RecipientPrincipalID != "" && filter.RecipientPrincipalID != principalID {
			writeError(w, http.StatusForbidden, "forbidden",
				"reading another principal's notifications requires legal_entity_id and a NOTIFICATION_VIEW grant on it")
			return
		}
		filter.RecipientPrincipalID = principalID
	}

	list, err := h.store.ListNotifications(r.Context(), filter)
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
	if _, ok := h.requireTenant(w, r); !ok {
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

// requireTenant refuses a request that carries no tenant scope.
//
// Without this the store was the first thing to notice, returning
// ErrIdentityMissing, which the handlers reported as 503 store_unavailable —
// an outage status for a caller that simply forgot X-Tenant-Id, and one that
// sends whoever is on call to look at Postgres. Same fix as
// financial-close-svc.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_missing", "X-Tenant-Id is required")
		return "", false
	}
	return tenantID, true
}

// maxRequestBytes bounds a notification body. Subject is VARCHAR(255) and body
// is TEXT, so without a cap a single request could stream unbounded memory
// into the decoder before any validation ran.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a JSON request body with a size cap and no tolerance for
// unknown fields — a misspelled "subjekt" used to be accepted silently and
// stored as an empty subject, so the caller got a 201 for a notification that
// did not say what they wrote.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				"request body exceeds 256 KiB")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

const (
	defaultLimit = 100
	maxLimit     = 500
)

// parsePaging bounds a list read. An unbounded register grows without limit,
// and a discarded strconv error is the platform's recurring shape: limit=abc
// silently defaulted and offset=-1 reached Postgres and answered 503.
func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultLimit, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit",
				"limit must be an integer between 1 and "+strconv.Itoa(maxLimit))
			return 0, 0, false
		}
		limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_offset", "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
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
