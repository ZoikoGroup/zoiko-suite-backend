package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/identity"
	"zoiko.io/notification-svc/internal/retry"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/templates"
)

type Store interface {
	CreateNotification(ctx context.Context, n *domain.Notification) (created bool, err error)
	GetNotification(ctx context.Context, id string) (*domain.Notification, error)
	ListNotifications(ctx context.Context, f domain.ListFilter) ([]domain.Notification, error)
	CompleteDelivery(ctx context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time) error
	ScheduleRetry(ctx context.Context, id, tenantID, failureReason string, attemptedAt, nextAttemptAt time.Time) error
	MarkRead(ctx context.Context, id, recipientPrincipalID string, readAt time.Time) error
	CountUnread(ctx context.Context, recipientPrincipalID string) (int, error)
}

// RecipientResolver turns a principal into the contact endpoint a message is
// delivered to. Satisfied by internal/identity.Client.
type RecipientResolver interface {
	ResolveEmail(ctx context.Context, tenantID, callerPrincipalID, recipientPrincipalID string) (string, error)
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
	// SMS is deliberately absent. The service used to accept it, resolve a
	// recipient for it, and then fail every one — the only channel that
	// advertised a capability the platform does not have. A caller now gets
	// the same clean 400 an unknown channel gets, at the request boundary,
	// instead of a FAILED record for an attempt no provider ever saw.
	//
	// This is a withdrawal, not a decision that SMS is unwanted: restoring it
	// means adding a provider to internal/deliver and putting "SMS" back here.
	// Existing SMS rows are untouched and still render — the schema's channel
	// CHECK still permits the value, because the register is the account of
	// what this service did, including what it did wrongly.
	"IN_APP":  true,
	"WEBHOOK": true,
}

// Deliverer hands a notification to the transport its channel names. It is an
// interface, not a function, so the FAILED path can be exercised by a test
// double that genuinely refuses.
//
// It returns a domain.DeliveryOutcome rather than the (bool, string) pair it
// used to. The pair could say "it worked" or "it did not, here is why", and
// could not say either of the two things that turned out to matter: what the
// provider gave back as evidence, and whether a refusal is worth attempting
// again. Both were consequently unrecorded, so a greylisted message and a
// nonexistent mailbox concluded identically.
//
// The implementation lives in internal/deliver. It used to be StubDeliverer,
// right here — an adapter that logged a line and reported success for every
// channel, because no provider was wired up. It was honestly documented and
// still had a real consequence: an EMAIL notification was recorded SENT,
// published notification.sent, and told the operator it had gone out, when
// nothing had ever been transmitted.
type Deliverer interface {
	Deliver(ctx context.Context, n domain.Notification) domain.DeliveryOutcome
}

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	deliverer Deliverer
	recipient RecipientResolver
	log       *zap.Logger

	// retryPolicy decides whether a first-attempt failure is scheduled for
	// another try. The same policy the worker uses, so the schedule a send
	// writes and the schedule the worker extends cannot disagree.
	retryPolicy retry.Policy
}

// Deps groups the handler's collaborators.
//
// A struct rather than a seventh positional parameter: the constructor already
// took five interfaces and a logger, all of them satisfied by more than one
// type in tests, and every one of them assignable to at least one other
// position. Transposing two arguments there compiles and fails at runtime,
// which is the same reason domain.ListFilter exists.
type Deps struct {
	Store       Store
	Publisher   Publisher
	AuthZ       AuthZClient
	Deliverer   Deliverer
	Recipient   RecipientResolver
	RetryPolicy retry.Policy
	Log         *zap.Logger
}

func New(d Deps) *Handler {
	return &Handler{
		store:       d.Store,
		publisher:   d.Publisher,
		authz:       d.AuthZ,
		deliverer:   d.Deliverer,
		recipient:   d.Recipient,
		retryPolicy: d.RetryPolicy.Normalize(),
		log:         d.Log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/notifications", func(r chi.Router) {
		r.Post("/", h.SendNotification)
		r.Get("/", h.ListNotifications)

		// Registered before "/{id}" for legibility only — chi matches a static
		// segment ahead of a wildcard regardless of declaration order, so
		// "unread-count" is never captured as a notification id.
		r.Get("/unread-count", h.UnreadCount)
		r.Get("/templates", h.ListTemplates)

		r.Get("/{id}", h.GetNotification)
		r.Post("/{id}/read", h.MarkRead)
	})
}

// ── GET /v1/notifications/templates ─────────────────────────────────────────

// ListTemplates returns the template catalogue and each template's required
// variables.
//
// No authorization check, and no tenant scope. The catalogue is compiled into
// the binary — six transactional templates and the variable names they refuse
// to render without — and it is identical for every tenant and every caller.
// There is no tenant data in it to leak and nothing to authorize against: a
// NOTIFICATION_SEND grant is checked when a notification is actually sent, and
// gating discovery of what CAN be sent behind an entity grant would only mean
// a console cannot draw a form until the user picks a legal entity.
//
// It still requires a caller identity, so this is not an anonymous endpoint —
// the envelope middleware ahead of it refuses an unattributed request.
func (h *Handler) ListTemplates(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"templates": templates.Catalogue(),
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

	// A template renders subject and body; supplying both forms would leave it
	// ambiguous which one the recipient actually got.
	if req.Template != "" && (req.Subject != "" || req.Body != "") {
		writeError(w, http.StatusBadRequest, "conflicting_content",
			"supply either template (with variables) or subject and body, not both")
		return
	}

	if req.Template != "" {
		subject, body, err := templates.Render(req.Template, req.Variables)
		switch e := err.(type) {
		case nil:
			req.Subject, req.Body = subject, body
		case templates.ErrUnknownTemplate:
			writeError(w, http.StatusBadRequest, "unknown_template", e.Error())
			return
		case templates.ErrMissingVariables:
			// Refusing beats sending a message with a blank organization name
			// or an empty login link.
			writeError(w, http.StatusBadRequest, "missing_template_variables", e.Error())
			return
		default:
			h.log.Error("failed to render notification template",
				zap.String("template", req.Template), zap.Error(err))
			writeError(w, http.StatusInternalServerError, "template_render_failed", e.Error())
			return
		}
	}

	if req.RecipientPrincipalID == "" || req.LegalEntityID == "" || req.Channel == "" ||
		req.Subject == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"recipient_principal_id, legal_entity_id, channel, correlation_id are required, plus either subject or template")
		return
	}

	// An unrecognised channel is a caller mistake, not a delivery failure.
	if !supportedChannels[req.Channel] {
		writeError(w, http.StatusBadRequest, "unsupported_channel",
			"channel must be one of EMAIL, IN_APP, WEBHOOK")
		return
	}

	// A caller-supplied address is checked here, at the boundary, for the same
	// reason the channel is: it is a fact about the request, knowable without
	// calling anything, and wrong in a way the caller can fix. Left to the
	// provider it would come back as an SMTP rejection and be recorded as a
	// delivery failure — a permanent FAILED notification blaming the mail
	// server for a typo in the request that produced it.
	if req.RecipientAddress != "" {
		if !domain.ChannelNeedsAddress(req.Channel) {
			writeError(w, http.StatusBadRequest, "address_not_applicable",
				"recipient_address is only meaningful for EMAIL and SMS; "+
					"an IN_APP notice is delivered by being recorded, and WEBHOOK is not this service's channel")
			return
		}
		if req.Channel == domain.ChannelEmail {
			if _, err := mail.ParseAddress(req.RecipientAddress); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_recipient_address", err.Error())
				return
			}
		}
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

	// Resolve the endpoint before the record is written, so the address a
	// notification went to is part of the row from the moment it exists rather
	// than something patched in afterwards.
	//
	// A resolution failure does not abort the request. It is recorded as a
	// concluded FAILED notification, which is the same posture the rest of
	// this handler takes: 03-microservices.md §9.7 requires that "notification
	// failure must not collapse source operational workflows", and a payroll
	// run that finalized correctly must not be told it failed because an
	// employee has no email address on file.
	address, addressSource, resolveErr := h.resolveRecipient(r.Context(), tenantID, principalID, req)

	notification := &domain.Notification{
		NotificationID:         uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          req.LegalEntityID,
		RecipientPrincipalID:   req.RecipientPrincipalID,
		RecipientAddress:       address,
		RecipientAddressSource: addressSource,
		Channel:                req.Channel,
		Subject:                req.Subject,
		Body:                   req.Body,
		Status:                 "PENDING",
		SourceEventType:        req.SourceEventType,
		SourceReference:        req.SourceReference,
		CorrelationID:          req.CorrelationID,
		CreatedByPrincipalID:   principalID,
		CreatedAt:              now,
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

	// A notification whose recipient could not be resolved is never handed to
	// a provider. Attempting it would produce a second, misleading failure
	// from the transport ("empty To") on top of the real one, and the record
	// would name the mail server rather than the missing address.
	outcome := domain.DeliveryOutcome{}
	if resolveErr != nil {
		outcome.Reason = "recipient resolution failed: " + resolveErr.Error()
		outcome.Retryable = !identity.IsSettled(resolveErr)
	} else {
		outcome = h.deliverer.Deliver(r.Context(), *notification)
	}

	attemptedAt := time.Now().UTC()

	// A failure worth re-attempting does not conclude the notification. It
	// stays PENDING with a schedule on it, and internal/retry's worker picks
	// it up — which is the whole difference between classifying a failure and
	// doing something about it.
	if !outcome.Delivered && outcome.Retryable {
		if next, ok := h.retryPolicy.NextAttempt(attemptedAt, 1); ok {
			if err := h.store.ScheduleRetry(r.Context(), notification.NotificationID,
				tenantID, outcome.Reason, attemptedAt, next); err != nil {
				h.log.Error("failed to schedule delivery retry", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
				return
			}
			notification.Status = "PENDING"
			notification.FailureReason = outcome.Reason
			notification.DeliveryAttempts = 1
			notification.LastAttemptAt = &attemptedAt
			notification.NextAttemptAt = &next

			h.log.Warn("delivery failed on first attempt, scheduled for retry",
				zap.String("notification_id", notification.NotificationID),
				zap.Time("next_attempt_at", next),
				zap.String("reason", outcome.Reason))

			// No notification.failed event: nothing has failed yet. Publishing
			// one here and a notification.sent two minutes later would have
			// consumers act on an outcome that did not happen.
			writeJSON(w, http.StatusCreated, notification)
			return
		}
		// MaxAttempts of 1 — retry disabled by configuration. Fall through and
		// conclude, rather than sit PENDING with nothing scheduled to move it.
		outcome.Reason += " (retry is disabled by configuration)"
	}

	newStatus := "SENT"
	if !outcome.Delivered {
		newStatus = "FAILED"
	}

	if err := h.store.CompleteDelivery(r.Context(), notification.NotificationID,
		newStatus, outcome.Reason, outcome.ProviderResponse, &attemptedAt); err != nil {
		h.log.Error("failed to record delivery outcome", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	notification.Status = newStatus
	notification.FailureReason = outcome.Reason
	notification.ProviderResponse = outcome.ProviderResponse
	notification.SentAt = &attemptedAt
	notification.DeliveryAttempts = 1
	notification.LastAttemptAt = &attemptedAt

	if outcome.Delivered {
		h.publisher.PublishSent(r.Context(), correlationID, *notification)
	} else {
		h.publisher.PublishFailed(r.Context(), correlationID, *notification, outcome.Reason)
	}

	writeJSON(w, http.StatusCreated, notification)
}

// resolveRecipient determines the endpoint a notification is delivered to, and
// records where that endpoint came from.
//
// Channels that stay inside the platform get neither. IN_APP is delivered by
// being written to this register, so resolving an email address for one would
// make every in-app notice depend on identity-context-svc being reachable, to
// compute a value nothing reads.
func (h *Handler) resolveRecipient(ctx context.Context, tenantID, callerPrincipalID string, req domain.SendNotificationRequest) (address, source string, err error) {
	if !domain.ChannelNeedsAddress(req.Channel) {
		return "", "", nil
	}

	// An address supplied by the caller wins, and is recorded as such. The
	// override exists for recipients who are not yet established principals —
	// registration_received goes to somebody whose organization has not been
	// approved — and marking its provenance is what keeps it distinguishable
	// from an address the identity authority vouched for.
	if req.RecipientAddress != "" {
		return req.RecipientAddress, domain.AddressSourceRequest, nil
	}

	if h.recipient == nil {
		return "", "", errors.New("no recipient resolver is configured (IDENTITY_SERVICE_URL unset)")
	}

	addr, err := h.recipient.ResolveEmail(ctx, tenantID, callerPrincipalID, req.RecipientPrincipalID)
	if err != nil {
		h.log.Warn("recipient resolution failed",
			zap.String("recipient_principal_id", req.RecipientPrincipalID),
			zap.String("channel", req.Channel),
			zap.Bool("settled", identity.IsSettled(err)),
			zap.Error(err))
		return "", "", err
	}
	return addr, domain.AddressSourceIdentityContext, nil
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
		UnreadOnly:           r.URL.Query().Get("unread_only") == "true",
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

// MarkRead records that the recipient has opened an in-app notice.
// POST /v1/notifications/{id}/read
//
// Deliberately not authorized through NOTIFICATION_VIEW. That grant lets an
// administrator read the register for a legal entity, and read state is not a
// register read — it is the recipient's own assertion that they have seen
// their notice. An administrator opening the audit view must not silently
// clear somebody else's unread badge, so the only principal who can mark a
// notification read is the one it was addressed to.
//
// Idempotent: a second call is a 200 with the original read_at, not a
// conflict. Inboxes re-issue this on every render, and the first read is the
// fact worth keeping.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	// Fetched first so the three ways this can fail stay distinguishable. The
	// store's UPDATE matches on id, tenant, recipient and channel at once, so
	// on its own it can only report "no row changed" — one answer for an
	// unknown notification, somebody else's notification, and an email that
	// has no read state. Those are 404, 403 and 400 respectively.
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

	if notification.RecipientPrincipalID != principalID {
		writeError(w, http.StatusForbidden, "forbidden",
			"only the recipient may mark a notification read")
		return
	}
	if notification.Channel != domain.ChannelInApp {
		writeError(w, http.StatusBadRequest, "channel_has_no_read_state",
			"read state applies to IN_APP notifications only; this service cannot observe "+
				"whether a message delivered by an external provider was opened")
		return
	}

	readAt := time.Now().UTC()
	if err := h.store.MarkRead(r.Context(), id, principalID, readAt); err != nil {
		if errors.Is(err, domain.ErrNotificationNotFound) {
			writeError(w, http.StatusNotFound, "notification_not_found", "")
			return
		}
		h.log.Error("failed to mark notification read", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Re-read rather than assuming readAt was stored: the store keeps the
	// FIRST read, so on a repeat call the value written now is discarded and
	// returning it would report a read time that is not in the database.
	updated, err := h.store.GetNotification(r.Context(), id)
	if err != nil {
		h.log.Error("failed to re-read notification after marking read", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// UnreadCount answers how many in-app notices the calling principal has not
// opened — the number on the bell.
// GET /v1/notifications/unread-count
//
// Always the caller's own count, with no principal parameter to override it.
// A count is small but it is not nothing: a per-principal unread total that
// anyone could query would report on colleagues' attention, and asking for it
// is exactly the request that looks harmless in review.
func (h *Handler) UnreadCount(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	count, err := h.store.CountUnread(r.Context(), principalID)
	if err != nil {
		h.log.Error("failed to count unread notifications", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"recipient_principal_id": principalID,
		"unread_count":           count,
		// Named so the number cannot be mistaken for every unread message
		// across every channel. It counts what this service can actually
		// observe, which is in-app notices nobody has opened.
		"channel": domain.ChannelInApp,
	})
}

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
