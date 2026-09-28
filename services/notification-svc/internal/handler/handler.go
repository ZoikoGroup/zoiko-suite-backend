package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/events"
	"zoiko.io/notification-svc/internal/identity"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/retry"
	"zoiko.io/notification-svc/internal/telemetry"
	"zoiko.io/notification-svc/internal/templates"
)

type Store interface {
	CreateNotification(ctx context.Context, n *domain.Notification) (created bool, err error)
	GetNotification(ctx context.Context, id string) (*domain.Notification, error)
	ListNotifications(ctx context.Context, f domain.ListFilter) ([]domain.Notification, error)
	// CompleteDelivery concludes a delivery AND enqueues the event describing
	// that conclusion, in one transaction. The event is a required argument
	// rather than something this handler publishes afterwards, which is what it
	// used to do — see the store method and migration 000005 for what that
	// cost.
	CompleteDelivery(ctx context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time, ev events.Outbound) error
	ScheduleRetry(ctx context.Context, id, tenantID, failureReason string, attemptedAt, nextAttemptAt time.Time) error
	MarkRead(ctx context.Context, id, recipientPrincipalID string, readAt time.Time) error
	CountUnread(ctx context.Context, recipientPrincipalID string) (int, error)

	// Attempt records — durable chain per §3.4
	CreateAttempt(ctx context.Context, a *domain.DeliveryAttempt) error
	UpdateAttempt(ctx context.Context, attemptID, status, failureReason, providerResponse string, concludedAt *time.Time) error
	GetAttempts(ctx context.Context, notificationID string) ([]domain.DeliveryAttempt, error)

	// Idempotency check by purpose-scoped key
	GetByIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (*domain.Notification, error)

	// Reconciliation: find notifications stuck in UNKNOWN (in flight) for too long
	FindStuckInFlight(ctx context.Context, staleBefore time.Time, limit int) ([]domain.DueRetry, error)

	// ── NCD-02: Suppression ───────────────────────────────────────────────────
	CreateSuppression(ctx context.Context, s *domain.Suppression) error
	GetSuppression(ctx context.Context, id string) (*domain.Suppression, error)
	ListSuppressions(ctx context.Context, f domain.SuppressionFilter) ([]domain.Suppression, error)
	DeleteSuppression(ctx context.Context, id string) error

	// ── NCD-02: Preference ────────────────────────────────────────────────────
	UpsertPreference(ctx context.Context, p *domain.Preference) error
	GetPreference(ctx context.Context, tenantID, principalID, channel string) (*domain.Preference, error)
	ListPreferences(ctx context.Context, f domain.PreferenceFilter) ([]domain.Preference, error)
	DeletePreference(ctx context.Context, tenantID, principalID, channel string) error

	// ── NCD-02: Channel Decision ──────────────────────────────────────────────
	// EvaluateChannel checks suppression, preference, quiet hours, and permission.
	// Returns the decision result and records it for audit.
	EvaluateChannel(ctx context.Context, tenantID, principalID, channel, permissionGrant string) (*domain.ChannelDecisionResult, error)
	ListChannelDecisions(ctx context.Context, f domain.ChannelDecisionFilter) ([]domain.ChannelDecision, error)

	// ── NCD-04: Bounce ─────────────────────────────────────────────────────────
	CreateBounceEvent(ctx context.Context, b *domain.BounceEvent) error
	GetBounceEvent(ctx context.Context, id string) (*domain.BounceEvent, error)
	ListBounceEvents(ctx context.Context, f domain.BounceEventFilter) ([]domain.BounceEvent, error)

	// ── NCD-04: Complaint ──────────────────────────────────────────────────────
	CreateComplaintEvent(ctx context.Context, c *domain.ComplaintEvent) error
	GetComplaintEvent(ctx context.Context, id string) (*domain.ComplaintEvent, error)
	ListComplaintEvents(ctx context.Context, f domain.ComplaintEventFilter) ([]domain.ComplaintEvent, error)

	// ── NCD-04: Channel Reputation ─────────────────────────────────────────────
	UpsertChannelReputation(ctx context.Context, r *domain.ChannelReputation) error
	ListChannelReputations(ctx context.Context, f domain.ChannelReputationFilter) ([]domain.ChannelReputation, error)

	// ── NCD-01: Communication Intent ──────────────────────────────────────────
	CreateCommunicationIntent(ctx context.Context, i *domain.CommunicationIntent) error
	GetCommunicationIntent(ctx context.Context, id string) (*domain.CommunicationIntent, error)
	ListCommunicationIntents(ctx context.Context, f domain.CommunicationIntentFilter) ([]domain.CommunicationIntent, error)
	UpdateCommunicationIntent(ctx context.Context, i *domain.CommunicationIntent) error
	DeleteCommunicationIntent(ctx context.Context, id string) error

	// ── NCD-01: Template ──────────────────────────────────────────────────────
	CreateTemplate(ctx context.Context, t *domain.Template) error
	GetTemplate(ctx context.Context, id string) (*domain.Template, error)
	GetTemplateByIntent(ctx context.Context, intentID, locale string, version int) (*domain.Template, error)
	GetEffectiveTemplate(ctx context.Context, tenantID, intentID, locale string, at time.Time) (*domain.Template, error)
	ListTemplates(ctx context.Context, f domain.TemplateFilter) ([]domain.Template, error)
	UpdateTemplate(ctx context.Context, t *domain.Template) error
	DeleteTemplate(ctx context.Context, id string) error

	// ── NCD-01: Template Approval (SoD) ───────────────────────────────────────
	CreateTemplateApproval(ctx context.Context, a *domain.TemplateApproval) error
	GetTemplateApproval(ctx context.Context, id string) (*domain.TemplateApproval, error)
	ListTemplateApprovals(ctx context.Context, f domain.TemplateApprovalFilter) ([]domain.TemplateApproval, error)
	DecideTemplateApproval(ctx context.Context, approvalID, approverID, status, reason string) error

	// ── NCD-01: Template Render/Preview ───────────────────────────────────────
	CreateTemplateRender(ctx context.Context, r *domain.TemplateRender) error
	GetTemplateRender(ctx context.Context, id string) (*domain.TemplateRender, error)
	ListTemplateRenders(ctx context.Context, templateID string, limit, offset int) ([]domain.TemplateRender, error)

	// ── NCD-05: Regulated Notice & Acknowledgment ─────────────────────────────
	CreateRegulatedNotice(ctx context.Context, n *domain.RegulatedNotice) error
	GetRegulatedNotice(ctx context.Context, id string) (*domain.RegulatedNotice, error)
	ListRegulatedNotices(ctx context.Context, f domain.RegulatedNoticeFilter) ([]domain.RegulatedNotice, error)
	UpdateRegulatedNotice(ctx context.Context, n *domain.RegulatedNotice) error
	CreateAcknowledgmentChainStep(ctx context.Context, step *domain.AcknowledgmentChainStep) error
	GetAcknowledgmentChain(ctx context.Context, regulatedNoticeID string) ([]domain.AcknowledgmentChainStep, error)
	AcknowledgeRegulatedNotice(ctx context.Context, req *domain.AcknowledgeRequest, actorID string) error
}

// RecipientResolver turns a principal into the contact endpoint a message is
// delivered to. Satisfied by internal/identity.Client.
type RecipientResolver interface {
	ResolveEmail(ctx context.Context, tenantID, callerPrincipalID, recipientPrincipalID string) (string, error)
}

// Metrics is the domain-metric surface this handler records against.
//
// It exists because every interesting failure in this service answers 2xx: a
// FAILED delivery is a 201 by design (§9.7 — notification failure must not
// collapse the source workflow), and so is one rescheduled after a transient
// failure. http_requests_total is therefore flat and healthy across both, and
// without these counters no number anywhere moves when the platform stops
// delivering.
//
// A nil Metrics is safe and means "do not record", so tests need not build one.
type Metrics interface {
	ObserveAttempt(channel, outcome, origin string, seconds float64)
	ObserveConclusion(channel, status string)
	ObserveRetryScheduled(channel string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionSend = "NOTIFICATION_SEND"
	actionView = "NOTIFICATION_VIEW"
)

var supportedChannels = map[string]bool{
	"EMAIL": true,
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
	metrics   Metrics
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
	Metrics     Metrics
	AuthZ       AuthZClient
	Deliverer   Deliverer
	Recipient   RecipientResolver
	RetryPolicy retry.Policy
	Log         *zap.Logger
}

func New(d Deps) *Handler {
	return &Handler{
		store:       d.Store,
		metrics:     d.Metrics,
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

	// ── NCD-02: Suppression ───────────────────────────────────────────────────
	r.Route("/v1/suppressions", func(r chi.Router) {
		r.Post("/", h.CreateSuppression)
		r.Get("/", h.ListSuppressions)
		r.Get("/{id}", h.GetSuppression)
		r.Delete("/{id}", h.DeleteSuppression)
	})

	// ── NCD-02: Preference ────────────────────────────────────────────────────
	r.Route("/v1/preferences", func(r chi.Router) {
		r.Post("/", h.UpsertPreference)
		r.Get("/", h.ListPreferences)
		r.Get("/{principal_id}/{channel}", h.GetPreference)
		r.Delete("/{principal_id}/{channel}", h.DeletePreference)
	})

	// ── NCD-02: Channel Decision ──────────────────────────────────────────────
	r.Route("/v1/channel-decision", func(r chi.Router) {
		r.Post("/evaluate", h.EvaluateChannel)
		r.Get("/", h.ListChannelDecisions)
	})

	// ── NCD-04: Bounce ─────────────────────────────────────────────────────────
	r.Route("/v1/bounces", func(r chi.Router) {
		r.Post("/", h.CreateBounceEvent)
		r.Get("/", h.ListBounceEvents)
		r.Get("/{id}", h.GetBounceEvent)
	})

	// ── NCD-04: Complaint ──────────────────────────────────────────────────────
	r.Route("/v1/complaints", func(r chi.Router) {
		r.Post("/", h.CreateComplaintEvent)
		r.Get("/", h.ListComplaintEvents)
		r.Get("/{id}", h.GetComplaintEvent)
	})

	// ── NCD-04: Channel Reputation ─────────────────────────────────────────────
	r.Route("/v1/reputation", func(r chi.Router) {
		r.Post("/", h.UpsertChannelReputation)
		r.Get("/", h.ListChannelReputations)
	})

	// ── NCD-01: Communication Intent ───────────────────────────────────────────
	r.Route("/v1/communication-intents", func(r chi.Router) {
		r.Post("/", h.CreateCommunicationIntent)
		r.Get("/", h.ListCommunicationIntents)
		r.Get("/{id}", h.GetCommunicationIntent)
		r.Patch("/{id}", h.UpdateCommunicationIntent)
		r.Delete("/{id}", h.DeleteCommunicationIntent)
	})

	// ── NCD-01: Template ───────────────────────────────────────────────────────
	r.Route("/v1/templates", func(r chi.Router) {
		r.Post("/", h.CreateTemplate)
		r.Get("/", h.ListTemplatesRegistry)
		r.Get("/effective", h.GetEffectiveTemplate)
		r.Post("/preview", h.RenderTemplatePreview)
		r.Get("/{id}", h.GetTemplate)
		r.Patch("/{id}", h.UpdateTemplate)
		r.Delete("/{id}", h.DeleteTemplate)
		r.Post("/{id}/validate", h.ValidateTemplate)
		r.Post("/{id}/approve", h.RequestTemplateApproval)
		r.Post("/{id}/publish", h.PublishTemplate)
	})

	// ── NCD-01: Template Approvals ─────────────────────────────────────────────
	r.Route("/v1/template-approvals", func(r chi.Router) {
		r.Get("/", h.ListTemplateApprovals)
		r.Get("/{id}", h.GetTemplateApproval)
		r.Post("/{id}/decide", h.DecideTemplateApproval)
	})

	// ── NCD-05: Regulated Notice & Acknowledgment ─────────────────────────────
	r.Route("/v1/regulated-notices", func(r chi.Router) {
		r.Post("/", h.CreateRegulatedNotice)
		r.Get("/", h.ListRegulatedNotices)
		r.Get("/{id}", h.GetRegulatedNotice)
		r.Patch("/{id}", h.UpdateRegulatedNotice)
	})
	r.Route("/v1/acknowledgements", func(r chi.Router) {
		r.Post("/", h.AcknowledgeRegulatedNotice)
		r.Get("/", h.ListAcknowledgements)
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
// It still requires a caller identity, so this is not an anonymous endpoint.
//
// That identity is checked HERE, not left to the envelope middleware. This
// comment used to say the middleware refused an unattributed request, and it
// did not: enforcement runs in write-strict mode (ZS_ENVELOPE_ENFORCEMENT's
// default), where a read's envelope is parsed and REPORTED but the request is
// admitted. Measured against the running service on 2026-09-22 — a bare
//
//	curl http://localhost:8133/v1/notifications/templates
//
// with no tenant, no principal and no headers at all returned 200 and the whole
// catalogue. The one route on this service that documented itself as
// authenticated was the one route that was not.
//
// The disclosure is small — the catalogue is compiled into the binary, is
// identical for every tenant and holds no tenant data — which is precisely why
// it survived: nothing downstream of it could go wrong in a way anyone would
// notice. A control that reads as present and does nothing is worse than no
// control, and every other handler here fails closed the same way.
func (h *Handler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
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
		req.Subject == "" || req.CorrelationID == "" || req.PurposeContext == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"recipient_principal_id, legal_entity_id, channel, correlation_id, purpose_context are required, plus either subject or template")
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

	// Derive or use the provided idempotency key (purpose-scoped per §3.4)
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("%s|%s|%s|%s", tenantID, req.LegalEntityID, req.CorrelationID, req.PurposeContext)
	}

	// Check for existing notification by idempotency key BEFORE creating
	existing, err := h.store.GetByIdempotencyKey(r.Context(), tenantID, idempotencyKey)
	if err != nil {
		h.log.Error("failed to check idempotency key", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if existing != nil {
		// Replay: this idempotency key was already processed.
		writeJSON(w, http.StatusOK, existing)
		return
	}

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
		Status:                 domain.StatusPending,
		SourceEventType:        req.SourceEventType,
		SourceReference:        req.SourceReference,
		CorrelationID:          req.CorrelationID,
		CreatedByPrincipalID:   principalID,
		CreatedAt:              now,
		IdempotencyKey:         idempotencyKey,
		PurposeContext:         req.PurposeContext,
	}

	created, err := h.store.CreateNotification(r.Context(), notification)
	if err != nil {
		h.log.Error("failed to create notification", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if !created {
		// Should not happen since we checked above, but defense in depth
		writeJSON(w, http.StatusOK, notification)
		return
	}

	// A notification whose recipient could not be resolved is never handed to
	// a provider. Attempting it would produce a second, misleading failure
	// from the transport ("empty To") on top of the real one, and the record
	// would name the mail server rather than the missing address.
	outcome := domain.DeliveryOutcome{}
	attemptStarted := time.Now()
	if resolveErr != nil {
		outcome.Reason = "recipient resolution failed: " + resolveErr.Error()
		outcome.Retryable = !identity.IsSettled(resolveErr)
	} else {
		outcome = h.deliverer.Deliver(r.Context(), *notification)
	}

	attemptedAt := time.Now().UTC()

	// Create the first attempt record (durable attempt_id per §3.4)
	attempt := &domain.DeliveryAttempt{
		AttemptID:        uuid.NewString(),
		NotificationID:   notification.NotificationID,
		AttemptNumber:    1,
		Channel:          notification.Channel,
		Provider:         "smtp", // TODO: extract from deliverer
		Status:           domain.AttemptStatusUnknown, // Start as UNKNOWN, reconcile later
		ProviderResponse: outcome.ProviderResponse,
		FailureReason:    outcome.Reason,
		Retryable:        outcome.Retryable,
		ResendReason:     domain.ResendReasonFirstTry,
		CreatedAt:        attemptedAt,
	}
	if err := h.store.CreateAttempt(r.Context(), attempt); err != nil {
		h.log.Error("failed to create attempt record", zap.Error(err))
	}

	// Recorded for every attempt, delivered or not — including the resolution
	// failures above, which never reach a provider. Excluding those would make
	// the attempt count disagree with delivery_attempts on the row, and an
	// unresolvable recipient is a delivery attempt that failed, not one that
	// did not happen.
	if h.metrics != nil {
		outcomeLabel := telemetry.OutcomeFailed
		if outcome.Delivered {
			outcomeLabel = telemetry.OutcomeDelivered
		}
		h.metrics.ObserveAttempt(notification.Channel, outcomeLabel,
			telemetry.OriginRequest, time.Since(attemptStarted).Seconds())
	}

	// The OUTCOME of an attempt already made is recorded on a context that
	// outlives the request, not on r.Context().
	//
	// WHY. Once the provider has been called, what happened is a fact about
	// the outside world, and the caller hanging up does not un-send an email.
	// r.Context() is cancelled when the response is written — and sooner if
	// the client disconnects or the server's 15s WriteTimeout fires — so the
	// two statements below could fail for no reason but the request ending,
	// leaving the notification PENDING with nothing scheduled: in flight
	// forever, which is the stranded state internal/retry's sweep exists to
	// repair. Five rows on the dev stack were in exactly that state for six
	// days.
	//
	// The sweep is the backstop; this is the fix. It matters most in the worst
	// case — a message that WAS delivered and whose success was never written
	// — because there the sweep would reasonably re-send it and the recipient
	// would get the notice twice. Keeping the write alive is what makes that
	// rare rather than routine.
	//
	// The tenant is carried over explicitly: the store reads it from the
	// context, and a bare context.Background() would have no tenant installed
	// and be refused by row-level security.
	outcomeCtx, cancelOutcome := context.WithTimeout(
		svcmiddleware.WithTenant(context.WithoutCancel(r.Context()), tenantID), 10*time.Second)
	defer cancelOutcome()

	// Check if the failure was a post-submit timeout (context deadline exceeded).
	// Per ZS-SVC-Y-001 §3.4: "Timeout after submit becomes UNKNOWN, not FAILED;
	// reconcile before re-attempting". We detect this by checking if the error
	// is a context deadline exceeded, which means the provider call timed out
	// but we don't know if it actually accepted the message.
	isTimeout := false
	if !outcome.Delivered && outcome.Retryable && outcome.Err != nil {
		// The deliverer preserves the original error in outcome.Err
		if errors.Is(outcome.Err, context.DeadlineExceeded) {
			isTimeout = true
		}
		// Also check for timeout via the Timeout() method (for net.Error)
		var timeoutErr interface{ Timeout() bool }
		if !isTimeout && errors.As(outcome.Err, &timeoutErr) && timeoutErr.Timeout() {
			isTimeout = true
		}
	}

	// If the provider accepted the message, transition to PROVIDER_ACCEPTED
	// (not SENT). If the outcome is a post-submit timeout, mark as UNKNOWN.
	// If the outcome is a failure worth re-attempting, stay PENDING with a
	// schedule. If it's a settled failure, conclude as FAILED.
	if outcome.Delivered {
		// Provider accepted — mark as PROVIDER_ACCEPTED (not SENT).
		// The reconciliation worker will later confirm delivery and advance
		// to DELIVERED/READ/SERVED as appropriate.
		notification.Status = domain.StatusProviderAccepted
		notification.ProviderResponse = outcome.ProviderResponse
		notification.SentAt = &attemptedAt
		notification.DeliveryAttempts = 1
		notification.LastAttemptAt = &attemptedAt

		// Update attempt record to PROVIDER_ACCEPTED
		attempt.Status = domain.AttemptStatusProviderAccepted
		attempt.ConcludedAt = &attemptedAt
		if err := h.store.UpdateAttempt(outcomeCtx, attempt.AttemptID, attempt.Status, attempt.FailureReason, attempt.ProviderResponse, &attemptedAt); err != nil {
			h.log.Error("failed to update attempt record", zap.Error(err))
		}
	} else if isTimeout {
		// Post-submit timeout: mark as UNKNOWN per §3.4.
		// Do not schedule a retry; the reconciliation worker will check
		// attempt records and advance to PROVIDER_ACCEPTED/DELIVERED/FAILED.
		notification.Status = domain.StatusUnknown
		notification.FailureReason = outcome.Reason
		notification.DeliveryAttempts = 1
		notification.LastAttemptAt = &attemptedAt
		// NextAttemptAt remains nil — this is "in flight" (UNKNOWN)

		// Update attempt record to UNKNOWN
		attempt.Status = domain.AttemptStatusUnknown
		// ConcludedAt stays nil — not concluded, awaiting reconciliation
		if err := h.store.UpdateAttempt(outcomeCtx, attempt.AttemptID, attempt.Status, attempt.FailureReason, attempt.ProviderResponse, nil); err != nil {
			h.log.Error("failed to update attempt record", zap.Error(err))
		}

		h.log.Warn("delivery timed out after submit, marked UNKNOWN for reconciliation",
			zap.String("notification_id", notification.NotificationID),
			zap.String("reason", outcome.Reason))
	} else if outcome.Retryable {
		// Failure worth re-attempting: schedule a retry, stay PENDING
		if next, ok := h.retryPolicy.NextAttempt(attemptedAt, 1); ok {
			if err := h.store.ScheduleRetry(outcomeCtx, notification.NotificationID,
				tenantID, outcome.Reason, attemptedAt, next); err != nil {
				h.log.Error("failed to schedule delivery retry", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
				return
			}
			notification.Status = domain.StatusPending
			notification.FailureReason = outcome.Reason
			notification.DeliveryAttempts = 1
			notification.LastAttemptAt = &attemptedAt
			notification.NextAttemptAt = &next

			// Update attempt record to FAILED (retryable)
			attempt.Status = domain.AttemptStatusFailed
			attempt.ConcludedAt = &attemptedAt
			if err := h.store.UpdateAttempt(outcomeCtx, attempt.AttemptID, attempt.Status, attempt.FailureReason, attempt.ProviderResponse, &attemptedAt); err != nil {
				h.log.Error("failed to update attempt record", zap.Error(err))
			}

			h.log.Warn("delivery failed on first attempt, scheduled for retry",
				zap.String("notification_id", notification.NotificationID),
				zap.Time("next_attempt_at", next),
				zap.String("reason", outcome.Reason))

			if h.metrics != nil {
				h.metrics.ObserveRetryScheduled(notification.Channel)
			}

			// No notification.failed event, and nothing enqueued: nothing has
			// failed yet. Emitting one here and a notification.sent two minutes
			// later would have consumers act on an outcome that did not happen.
			writeJSON(w, http.StatusCreated, notification)
			return
		}
		// MaxAttempts of 1 — retry disabled by configuration. Fall through and
		// conclude as FAILED, rather than sit PENDING with nothing scheduled.
		outcome.Reason += " (retry is disabled by configuration)"
	}

	// Terminal failure: conclude as FAILED
	if notification.Status == domain.StatusPending {
		notification.Status = domain.StatusFailed
		notification.FailureReason = outcome.Reason
		notification.ProviderResponse = outcome.ProviderResponse
		notification.SentAt = &attemptedAt
		notification.DeliveryAttempts = 1
		notification.LastAttemptAt = &attemptedAt

		attempt.Status = domain.AttemptStatusFailed
		attempt.ConcludedAt = &attemptedAt
		if err := h.store.UpdateAttempt(outcomeCtx, attempt.AttemptID, attempt.Status, attempt.FailureReason, attempt.ProviderResponse, &attemptedAt); err != nil {
			h.log.Error("failed to update attempt record", zap.Error(err))
		}
	}

	newStatus := notification.Status

	ev, err := sealConclusion(correlationID, *notification, outcome)
	if err != nil {
		h.log.Error("failed to seal the delivery event", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "event_seal_failed", err.Error())
		return
	}

	// One transaction: the conclusion and the event announcing it. A broker
	// outage can no longer lose the announcement, because the broker is not
	// involved — internal/outbox delivers it later and retries until it lands.
	if err := h.store.CompleteDelivery(outcomeCtx, notification.NotificationID,
		newStatus, outcome.Reason, outcome.ProviderResponse, &attemptedAt, ev); err != nil {
		h.log.Error("failed to record delivery outcome", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if h.metrics != nil {
		h.metrics.ObserveConclusion(notification.Channel, newStatus)
	}

	writeJSON(w, http.StatusCreated, notification)
}

// sealConclusion builds the one event a concluded notification emits.
//
// One function rather than an if/else at the call site, and shared in spirit
// with internal/retry's identical choice, because the mapping from outcome to
// event type is the thing that must not drift: a delivered notification that
// emitted notification.failed, or the reverse, would be a consumer acting on
// the opposite of what happened. The status written to the row and the event
// type are derived from the same domain.DeliveryOutcome.Delivered here.
func sealConclusion(correlationID string, n domain.Notification, outcome domain.DeliveryOutcome) (events.Outbound, error) {
	if outcome.Delivered {
		return events.Sent(correlationID, n)
	}
	return events.Failed(correlationID, n, outcome.Reason)
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

// ── NCD-02: Suppression ───────────────────────────────────────────────────────

// CreateSuppression records a new suppression for a principal/channel.
// POST /v1/suppressions
func (h *Handler) CreateSuppression(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req struct {
		PrincipalID string             `json:"principal_id"`
		Channel     string             `json:"channel"`
		Reason      domain.SuppressionReason `json:"reason"`
		ExpiresAt   *time.Time         `json:"expires_at,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.PrincipalID == "" || req.Channel == "" || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "principal_id, channel, reason are required")
		return
	}

	// Only the recipient themselves or an admin with NOTIFICATION_VIEW on the entity can create a suppression
	// For now, allow any authenticated principal to suppress themselves
	if req.PrincipalID != principalID {
		// Would need NOTIFICATION_VIEW grant to suppress another principal
		writeError(w, http.StatusForbidden, "forbidden", "cannot create suppression for another principal without NOTIFICATION_VIEW")
		return
	}

	sup := &domain.Suppression{
		SuppressionID: uuid.NewString(),
		TenantID:      tenantID,
		PrincipalID:   req.PrincipalID,
		Channel:       req.Channel,
		Reason:        req.Reason,
		CreatedBy:     principalID,
		CreatedAt:     time.Now().UTC(),
		ExpiresAt:     req.ExpiresAt,
	}

	if err := h.store.CreateSuppression(r.Context(), sup); err != nil {
		h.log.Error("failed to create suppression", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, sup)
}

// GetSuppression returns a suppression by id.
// GET /v1/suppressions/{id}
func (h *Handler) GetSuppression(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	sup, err := h.store.GetSuppression(r.Context(), id)
	if errors.Is(err, domain.ErrSuppressionNotFound) {
		writeError(w, http.StatusNotFound, "suppression_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get suppression", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, sup)
}

// ListSuppressions returns suppressions for the tenant.
// GET /v1/suppressions
func (h *Handler) ListSuppressions(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.SuppressionFilter{
		TenantID:    tenantID,
		PrincipalID: r.URL.Query().Get("principal_id"),
		Channel:     r.URL.Query().Get("channel"),
		Reason:      r.URL.Query().Get("reason"),
		ActiveOnly:  r.URL.Query().Get("active_only") == "true",
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListSuppressions(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list suppressions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.Suppression{}
	}
	writeJSON(w, http.StatusOK, list)
}

// DeleteSuppression removes a suppression.
// DELETE /v1/suppressions/{id}
func (h *Handler) DeleteSuppression(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.store.DeleteSuppression(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrSuppressionNotFound) {
			writeError(w, http.StatusNotFound, "suppression_not_found", "")
			return
		}
		h.log.Error("failed to delete suppression", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── NCD-02: Preference ────────────────────────────────────────────────────────

// UpsertPreference creates or updates a preference.
// POST /v1/preferences
func (h *Handler) UpsertPreference(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.Preference
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.PrincipalID == "" || req.Channel == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "principal_id, channel are required")
		return
	}

	// Only the recipient themselves can set their preferences
	if req.PrincipalID != principalID {
		writeError(w, http.StatusForbidden, "forbidden", "cannot set preference for another principal")
		return
	}

	req.TenantID = tenantID
	if req.PreferenceID == "" {
		req.PreferenceID = uuid.NewString()
	}
	req.CreatedAt = time.Now().UTC()
	req.UpdatedAt = time.Now().UTC()

	if err := h.store.UpsertPreference(r.Context(), &req); err != nil {
		h.log.Error("failed to upsert preference", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, req)
}

// GetPreference returns a preference for a principal/channel.
// GET /v1/preferences/{principal_id}/{channel}
func (h *Handler) GetPreference(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	targetPrincipalID := chi.URLParam(r, "principal_id")
	channel := chi.URLParam(r, "channel")

	// Only the recipient themselves can read their preferences
	if targetPrincipalID != principalID {
		writeError(w, http.StatusForbidden, "forbidden", "cannot read preference for another principal")
		return
	}

	pref, err := h.store.GetPreference(r.Context(), tenantID, targetPrincipalID, channel)
	if errors.Is(err, domain.ErrPreferenceNotFound) {
		writeError(w, http.StatusNotFound, "preference_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get preference", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, pref)
}

// ListPreferences returns preferences for the tenant.
// GET /v1/preferences
func (h *Handler) ListPreferences(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.PreferenceFilter{
		TenantID:    tenantID,
		PrincipalID: r.URL.Query().Get("principal_id"),
		Channel:     r.URL.Query().Get("channel"),
		EnabledOnly: r.URL.Query().Get("enabled_only") == "true",
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListPreferences(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list preferences", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.Preference{}
	}
	writeJSON(w, http.StatusOK, list)
}

// DeletePreference removes a preference.
// DELETE /v1/preferences/{principal_id}/{channel}
func (h *Handler) DeletePreference(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	targetPrincipalID := chi.URLParam(r, "principal_id")
	channel := chi.URLParam(r, "channel")

	// Only the recipient themselves can delete their preferences
	if targetPrincipalID != principalID {
		writeError(w, http.StatusForbidden, "forbidden", "cannot delete preference for another principal")
		return
	}

	if err := h.store.DeletePreference(r.Context(), tenantID, targetPrincipalID, channel); err != nil {
		if errors.Is(err, domain.ErrPreferenceNotFound) {
			writeError(w, http.StatusNotFound, "preference_not_found", "")
			return
		}
		h.log.Error("failed to delete preference", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── NCD-02: Channel Decision ──────────────────────────────────────────────────

// EvaluateChannel evaluates whether a channel is allowed for a principal.
// POST /v1/channel-decision/evaluate
func (h *Handler) EvaluateChannel(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req struct {
		PrincipalID     string `json:"principal_id"`
		Channel         string `json:"channel"`
		PermissionGrant string `json:"permission_grant,omitempty"` // legal basis reference
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.PrincipalID == "" || req.Channel == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "principal_id, channel are required")
		return
	}

	// Only the recipient themselves or an admin can evaluate
	if req.PrincipalID != principalID {
		writeError(w, http.StatusForbidden, "forbidden", "cannot evaluate channel for another principal without NOTIFICATION_VIEW")
		return
	}

	result, err := h.store.EvaluateChannel(r.Context(), tenantID, req.PrincipalID, req.Channel, req.PermissionGrant)
	if err != nil {
		h.log.Error("failed to evaluate channel", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// ListChannelDecisions returns the channel decision audit log.
// GET /v1/channel-decision
func (h *Handler) ListChannelDecisions(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.ChannelDecisionFilter{
		TenantID:    tenantID,
		PrincipalID: r.URL.Query().Get("principal_id"),
		Channel:     r.URL.Query().Get("channel"),
		Decision:    r.URL.Query().Get("decision"),
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListChannelDecisions(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list channel decisions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ChannelDecision{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── NCD-04: Bounce ────────────────────────────────────────────────────────────

// CreateBounceEvent records a bounce event from a provider webhook.
// POST /v1/bounces
func (h *Handler) CreateBounceEvent(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.BounceEvent
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.NotificationID == "" || req.Provider == "" || req.BounceType == "" || req.RecipientAddress == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "notification_id, provider, bounce_type, recipient_address are required")
		return
	}

	req.TenantID = tenantID
	if req.BounceID == "" {
		req.BounceID = uuid.NewString()
	}
	if req.ReceivedAt.IsZero() {
		req.ReceivedAt = time.Now().UTC()
	}

	if err := h.store.CreateBounceEvent(r.Context(), &req); err != nil {
		h.log.Error("failed to create bounce event", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Auto-create suppression for hard bounces
	if req.BounceType == domain.BounceTypeHard {
		sup := &domain.Suppression{
			SuppressionID: uuid.NewString(),
			TenantID:      tenantID,
			PrincipalID:   "", // Would need to resolve from address
			Channel:       domain.ChannelEmail,
			Reason:        domain.SuppressionReasonBounce,
			CreatedBy:     "system",
			CreatedAt:     time.Now().UTC(),
		}
		// Note: In production, you'd resolve the principal from the address
		// For now we just record the bounce
		_ = sup
	}

	writeJSON(w, http.StatusCreated, req)
}

// GetBounceEvent returns a bounce event by id.
// GET /v1/bounces/{id}
func (h *Handler) GetBounceEvent(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	b, err := h.store.GetBounceEvent(r.Context(), id)
	if errors.Is(err, domain.ErrBounceNotFound) {
		writeError(w, http.StatusNotFound, "bounce_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get bounce event", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, b)
}

// ListBounceEvents returns bounce events for the tenant.
// GET /v1/bounces
func (h *Handler) ListBounceEvents(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.BounceEventFilter{
		TenantID:       tenantID,
		NotificationID: r.URL.Query().Get("notification_id"),
		RecipientAddr:  r.URL.Query().Get("recipient_address"),
		BounceType:     r.URL.Query().Get("bounce_type"),
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListBounceEvents(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list bounce events", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.BounceEvent{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── NCD-04: Complaint ─────────────────────────────────────────────────────────

// CreateComplaintEvent records a complaint event from a provider feedback loop.
// POST /v1/complaints
func (h *Handler) CreateComplaintEvent(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.ComplaintEvent
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Provider == "" || req.ComplaintType == "" || req.RecipientAddress == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "provider, complaint_type, recipient_address are required")
		return
	}

	req.TenantID = tenantID
	if req.ComplaintID == "" {
		req.ComplaintID = uuid.NewString()
	}
	if req.ReceivedAt.IsZero() {
		req.ReceivedAt = time.Now().UTC()
	}

	if err := h.store.CreateComplaintEvent(r.Context(), &req); err != nil {
		h.log.Error("failed to create complaint event", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, req)
}

// GetComplaintEvent returns a complaint event by id.
// GET /v1/complaints/{id}
func (h *Handler) GetComplaintEvent(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	c, err := h.store.GetComplaintEvent(r.Context(), id)
	if errors.Is(err, domain.ErrComplaintNotFound) {
		writeError(w, http.StatusNotFound, "complaint_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get complaint event", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, c)
}

// ListComplaintEvents returns complaint events for the tenant.
// GET /v1/complaints
func (h *Handler) ListComplaintEvents(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.ComplaintEventFilter{
		TenantID:       tenantID,
		NotificationID: r.URL.Query().Get("notification_id"),
		RecipientAddr:  r.URL.Query().Get("recipient_address"),
		ComplaintType:  r.URL.Query().Get("complaint_type"),
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListComplaintEvents(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list complaint events", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ComplaintEvent{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── NCD-04: Channel Reputation ────────────────────────────────────────────────

// UpsertChannelReputation creates or updates a channel reputation window.
// POST /v1/reputation
func (h *Handler) UpsertChannelReputation(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.ChannelReputation
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Channel == "" || req.Provider == "" || req.WindowStart.IsZero() || req.WindowEnd.IsZero() {
		writeError(w, http.StatusBadRequest, "missing_fields", "channel, provider, window_start, window_end are required")
		return
	}

	req.TenantID = tenantID
	if req.ReputationID == "" {
		req.ReputationID = uuid.NewString()
	}
	req.CreatedAt = time.Now().UTC()
	req.UpdatedAt = time.Now().UTC()

	if err := h.store.UpsertChannelReputation(r.Context(), &req); err != nil {
		h.log.Error("failed to upsert channel reputation", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, req)
}

// ListChannelReputations returns channel reputation for the tenant.
// GET /v1/reputation
func (h *Handler) ListChannelReputations(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var since, until time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t
		}
	}
	if u := r.URL.Query().Get("until"); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			until = t
		}
	}

	filter := domain.ChannelReputationFilter{
		TenantID: tenantID,
		Channel:  r.URL.Query().Get("channel"),
		Provider: r.URL.Query().Get("provider"),
		Since:    since,
		Until:    until,
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListChannelReputations(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list channel reputations", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ChannelReputation{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── NCD-01: Communication Intent ──────────────────────────────────────────────

// CreateCommunicationIntent creates a new communication intent.
// POST /v1/communication-intents
func (h *Handler) CreateCommunicationIntent(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.CommunicationIntent
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Name == "" || req.Category == "" || req.Channels == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "name, category, channels are required")
		return
	}

	req.TenantID = tenantID
	req.LegalEntityID = r.URL.Query().Get("legal_entity_id")
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id query parameter is required")
		return
	}
	req.IntentID = uuid.NewString()
	req.CreatedBy = principalID
	req.CreatedAt = time.Now().UTC()
	req.UpdatedAt = time.Now().UTC()

	if err := h.store.CreateCommunicationIntent(r.Context(), &req); err != nil {
		h.log.Error("failed to create communication intent", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, req)
}

// GetCommunicationIntent returns a communication intent by id.
// GET /v1/communication-intents/{id}
func (h *Handler) GetCommunicationIntent(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	intent, err := h.store.GetCommunicationIntent(r.Context(), id)
	if errors.Is(err, domain.ErrIntentNotFound) {
		writeError(w, http.StatusNotFound, "intent_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get communication intent", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, intent)
}

// ListCommunicationIntents returns communication intents for the tenant.
// GET /v1/communication-intents
func (h *Handler) ListCommunicationIntents(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.CommunicationIntentFilter{
		TenantID:      tenantID,
		LegalEntityID: r.URL.Query().Get("legal_entity_id"),
		Name:          r.URL.Query().Get("name"),
		Category:      r.URL.Query().Get("category"),
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListCommunicationIntents(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list communication intents", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.CommunicationIntent{}
	}
	writeJSON(w, http.StatusOK, list)
}

// UpdateCommunicationIntent updates a communication intent.
// PATCH /v1/communication-intents/{id}
func (h *Handler) UpdateCommunicationIntent(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	var req domain.CommunicationIntent
	if !decodeJSON(w, r, &req) {
		return
	}

	req.TenantID = tenantID
	req.IntentID = id
	req.UpdatedAt = time.Now().UTC()

	if err := h.store.UpdateCommunicationIntent(r.Context(), &req); err != nil {
		h.log.Error("failed to update communication intent", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, req)
}

// DeleteCommunicationIntent deletes a communication intent.
// DELETE /v1/communication-intents/{id}
func (h *Handler) DeleteCommunicationIntent(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.store.DeleteCommunicationIntent(r.Context(), id); err != nil {
		h.log.Error("failed to delete communication intent", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── NCD-01: Template ──────────────────────────────────────────────────────────

// CreateTemplate creates a new template version (draft).
// POST /v1/templates
func (h *Handler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.Template
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.IntentID == "" || req.SubjectTemplate == "" || req.BodyTemplate == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "intent_id, subject_template, body_template are required")
		return
	}

	req.TenantID = tenantID
	req.LegalEntityID = r.URL.Query().Get("legal_entity_id")
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id query parameter is required")
		return
	}
	if req.Locale == "" {
		req.Locale = "en"
	}
	req.TemplateID = uuid.NewString()
	req.Version = 1
	req.Status = domain.TemplateStatusDraft
	req.CreatedBy = principalID
	req.CreatedAt = time.Now().UTC()
	req.UpdatedAt = time.Now().UTC()

	if err := h.store.CreateTemplate(r.Context(), &req); err != nil {
		h.log.Error("failed to create template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, req)
}

// GetTemplate returns a template by id.
// GET /v1/templates/{id}
func (h *Handler) GetTemplate(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	t, err := h.store.GetTemplate(r.Context(), id)
	if errors.Is(err, domain.ErrTemplateNotFound) {
		writeError(w, http.StatusNotFound, "template_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, t)
}

// ListTemplatesRegistry returns templates for the tenant (registry view, not the catalogue).
// GET /v1/templates
func (h *Handler) ListTemplatesRegistry(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.TemplateFilter{
		TenantID:      tenantID,
		LegalEntityID: r.URL.Query().Get("legal_entity_id"),
		IntentID:      r.URL.Query().Get("intent_id"),
		Locale:        r.URL.Query().Get("locale"),
		Status:        r.URL.Query().Get("status"),
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListTemplates(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list templates", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.Template{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GetEffectiveTemplate returns the effective template for an intent/locale at a given time.
// GET /v1/templates/effective?intent_id=...&locale=...&at=...
func (h *Handler) GetEffectiveTemplate(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	intentID := r.URL.Query().Get("intent_id")
	locale := r.URL.Query().Get("locale")
	atStr := r.URL.Query().Get("at")

	if intentID == "" || locale == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "intent_id, locale are required")
		return
	}

	var at time.Time
	if atStr != "" {
		if t, err := time.Parse(time.RFC3339, atStr); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_time", "at must be RFC3339 format")
			return
		} else {
			at = t
		}
	} else {
		at = time.Now().UTC()
	}

	t, err := h.store.GetEffectiveTemplate(r.Context(), tenantID, intentID, locale, at)
	if errors.Is(err, domain.ErrTemplateNotFound) {
		writeError(w, http.StatusNotFound, "template_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get effective template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, t)
}

// RenderTemplatePreview renders a template preview with provided variables.
// POST /v1/templates/preview
func (h *Handler) RenderTemplatePreview(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.TemplatePreviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if len(req.Variables) == 0 {
		writeError(w, http.StatusBadRequest, "missing_fields", "variables are required")
		return
	}

	// Resolve template: either by template_id, or by intent_id + locale + version
	var t *domain.Template
	var err error

	if req.TemplateID != "" {
		t, err = h.store.GetTemplate(r.Context(), req.TemplateID)
	} else if req.IntentID != "" {
		if req.Version > 0 {
			t, err = h.store.GetTemplateByIntent(r.Context(), req.IntentID, req.Locale, req.Version)
		} else {
			at := time.Now().UTC()
			t, err = h.store.GetEffectiveTemplate(r.Context(), tenantID, req.IntentID, req.Locale, at)
		}
	} else {
		writeError(w, http.StatusBadRequest, "missing_fields", "template_id or intent_id is required")
		return
	}

	if errors.Is(err, domain.ErrTemplateNotFound) {
		writeError(w, http.StatusNotFound, "template_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to resolve template for preview", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Render using the existing template engine
	subject, body, err := templates.Render("custom", req.Variables)
	_ = subject // custom template not in catalogue
	_ = body
	// For preview, we render the template's own subject/body templates
	renderedSubject, err := renderTemplate(t.SubjectTemplate, req.Variables)
	if err != nil {
		writeError(w, http.StatusBadRequest, "render_failed", err.Error())
		return
	}
	renderedBody, err := renderTemplate(t.BodyTemplate, req.Variables)
	if err != nil {
		writeError(w, http.StatusBadRequest, "render_failed", err.Error())
		return
	}

	render := &domain.TemplateRender{
		RenderID:        uuid.NewString(),
		TemplateID:      t.TemplateID,
		TenantID:        tenantID,
		Variables:       req.Variables,
		RenderedSubject: renderedSubject,
		RenderedBody:    renderedBody,
		CreatedBy:       principalID,
		CreatedAt:       time.Now().UTC(),
	}

	if err := h.store.CreateTemplateRender(r.Context(), render); err != nil {
		h.log.Error("failed to save template render", zap.Error(err))
	}

	resp := map[string]any{
		"template_id":       t.TemplateID,
		"rendered_subject":  renderedSubject,
		"rendered_body":     renderedBody,
		"missing_variables": findMissingVars(t.Variables, req.Variables),
	}
	writeJSON(w, http.StatusOK, resp)
}

// UpdateTemplate updates a template (creates new version if published).
// PATCH /v1/templates/{id}
func (h *Handler) UpdateTemplate(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	var req domain.Template
	if !decodeJSON(w, r, &req) {
		return
	}

	req.TenantID = tenantID
	req.TemplateID = id
	req.UpdatedAt = time.Now().UTC()

	if err := h.store.UpdateTemplate(r.Context(), &req); err != nil {
		h.log.Error("failed to update template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, req)
}

// DeleteTemplate deletes a template.
// DELETE /v1/templates/{id}
func (h *Handler) DeleteTemplate(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	if err := h.store.DeleteTemplate(r.Context(), id); err != nil {
		h.log.Error("failed to delete template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ValidateTemplate validates a template's variables.
// POST /v1/templates/{id}/validate
func (h *Handler) ValidateTemplate(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	t, err := h.store.GetTemplate(r.Context(), id)
	if errors.Is(err, domain.ErrTemplateNotFound) {
		writeError(w, http.StatusNotFound, "template_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Validation: check that required variables are present in body/subject
	missing := findMissingVarsInTemplate(t.SubjectTemplate, t.BodyTemplate, t.Variables)
	if len(missing) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"valid":              false,
			"missing_variables":  missing,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

// RequestTemplateApproval requests approval for a template (SoD: creator cannot approve).
// POST /v1/templates/{id}/approve
func (h *Handler) RequestTemplateApproval(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	t, err := h.store.GetTemplate(r.Context(), id)
	if errors.Is(err, domain.ErrTemplateNotFound) {
		writeError(w, http.StatusNotFound, "template_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if t.Status != domain.TemplateStatusDraft {
		writeError(w, http.StatusBadRequest, "invalid_status", "only draft templates can be submitted for approval")
		return
	}

	approval := &domain.TemplateApproval{
		ApprovalID:  uuid.NewString(),
		TemplateID:  id,
		TenantID:    tenantID,
		RequestedBy: principalID,
		Status:      "pending",
		RequestedAt: time.Now().UTC(),
	}

	if err := h.store.CreateTemplateApproval(r.Context(), approval); err != nil {
		h.log.Error("failed to create template approval", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Update template status to pending_approval
	t.Status = domain.TemplateStatusPendingApproval
	t.UpdatedAt = time.Now().UTC()
	if err := h.store.UpdateTemplate(r.Context(), t); err != nil {
		h.log.Error("failed to update template status", zap.Error(err))
	}

	writeJSON(w, http.StatusCreated, approval)
}

// PublishTemplate publishes an approved template with effective dates.
// POST /v1/templates/{id}/publish
func (h *Handler) PublishTemplate(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	t, err := h.store.GetTemplate(r.Context(), id)
	if errors.Is(err, domain.ErrTemplateNotFound) {
		writeError(w, http.StatusNotFound, "template_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if t.Status != domain.TemplateStatusApproved {
		writeError(w, http.StatusBadRequest, "invalid_status", "only approved templates can be published")
		return
	}

	var req struct {
		EffectiveFrom *time.Time `json:"effective_from,omitempty"`
		EffectiveTo   *time.Time `json:"effective_to,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	now := time.Now().UTC()
	t.Status = domain.TemplateStatusPublished
	t.PublishedAt = &now
	t.EffectiveFrom = req.EffectiveFrom
	t.EffectiveTo = req.EffectiveTo
	t.UpdatedAt = now

	if err := h.store.UpdateTemplate(r.Context(), t); err != nil {
		h.log.Error("failed to publish template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, t)
}

// ── NCD-01: Template Approvals ────────────────────────────────────────────────

// ListTemplateApprovals returns template approvals for the tenant.
// GET /v1/template-approvals
func (h *Handler) ListTemplateApprovals(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.TemplateApprovalFilter{
		TenantID:   tenantID,
		TemplateID: r.URL.Query().Get("template_id"),
		Status:     r.URL.Query().Get("status"),
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListTemplateApprovals(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list template approvals", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.TemplateApproval{}
	}
	writeJSON(w, http.StatusOK, list)
}

// GetTemplateApproval returns a template approval by id.
// GET /v1/template-approvals/{id}
func (h *Handler) GetTemplateApproval(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	a, err := h.store.GetTemplateApproval(r.Context(), id)
	if errors.Is(err, domain.ErrApprovalNotFound) {
		writeError(w, http.StatusNotFound, "approval_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get template approval", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, a)
}

// DecideTemplateApproval decides (approves/rejects) a template approval.
// POST /v1/template-approvals/{id}/decide
func (h *Handler) DecideTemplateApproval(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	var req struct {
		Status string `json:"status"` // approved, rejected
		Reason string `json:"reason,omitempty"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Status != "approved" && req.Status != "rejected" {
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be approved or rejected")
		return
	}

	// SoD check: approver cannot be the requester
	approval, err := h.store.GetTemplateApproval(r.Context(), id)
	if errors.Is(err, domain.ErrApprovalNotFound) {
		writeError(w, http.StatusNotFound, "approval_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get template approval", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if approval.RequestedBy == principalID {
		writeError(w, http.StatusForbidden, "self_approval", string(domain.ErrSelfApproval))
		return
	}

	if err := h.store.DecideTemplateApproval(r.Context(), id, principalID, req.Status, req.Reason); err != nil {
		h.log.Error("failed to decide template approval", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// If approved, update template status
	if req.Status == "approved" {
		t, err := h.store.GetTemplate(r.Context(), approval.TemplateID)
		if err == nil {
			now := time.Now().UTC()
			t.Status = domain.TemplateStatusApproved
			t.ApprovedBy = principalID
			t.ApprovedAt = &now
			t.UpdatedAt = now
			_ = h.store.UpdateTemplate(r.Context(), t)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"approval_id": id,
		"status":      req.Status,
		"decided_by":  principalID,
		"decided_at":  time.Now().UTC(),
	})
}

// ── NCD-05: Regulated Notice & Acknowledgment ──────────────────────────────────

// CreateRegulatedNotice creates a new regulated notice.
// POST /v1/regulated-notices
func (h *Handler) CreateRegulatedNotice(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.RegulatedNotice
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.RecipientPrincipalID == "" || req.Subject == "" || req.Body == "" || req.Channel == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "recipient_principal_id, subject, body, channel are required")
		return
	}

	req.TenantID = tenantID
	req.LegalEntityID = r.URL.Query().Get("legal_entity_id")
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id query parameter is required")
		return
	}
	if req.RegulatedNoticeID == "" {
		req.RegulatedNoticeID = uuid.NewString()
	}
	if req.Status == "" {
		req.Status = domain.RegulatedNoticeStatusPending
	}
	if req.Priority == "" {
		req.Priority = domain.RegulatedNoticePriorityNormal
	}
	req.CreatedBy = principalID
	req.CreatedAt = time.Now().UTC()
	req.UpdatedAt = time.Now().UTC()

	if err := h.store.CreateRegulatedNotice(r.Context(), &req); err != nil {
		h.log.Error("failed to create regulated notice", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Add initial chain step
	chainStep := &domain.AcknowledgmentChainStep{
		ChainID:           uuid.NewString(),
		RegulatedNoticeID: req.RegulatedNoticeID,
		TenantID:          tenantID,
		StepNumber:        1,
		Action:            "created",
		Actor:             principalID,
		Metadata:          map[string]any{"intent_id": req.IntentID, "template_id": req.TemplateID},
		CreatedAt:         time.Now().UTC(),
	}
	_ = h.store.CreateAcknowledgmentChainStep(r.Context(), chainStep)

	writeJSON(w, http.StatusCreated, req)
}

// GetRegulatedNotice returns a regulated notice by id.
// GET /v1/regulated-notices/{id}
func (h *Handler) GetRegulatedNotice(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	notice, err := h.store.GetRegulatedNotice(r.Context(), id)
	if errors.Is(err, domain.ErrRegulatedNoticeNotFound) {
		writeError(w, http.StatusNotFound, "regulated_notice_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get regulated notice", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, notice)
}

// ListRegulatedNotices returns regulated notices for the tenant.
// GET /v1/regulated-notices
func (h *Handler) ListRegulatedNotices(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	filter := domain.RegulatedNoticeFilter{
		TenantID:             tenantID,
		LegalEntityID:        r.URL.Query().Get("legal_entity_id"),
		RecipientPrincipalID: r.URL.Query().Get("recipient_principal_id"),
		Status:               r.URL.Query().Get("status"),
		Priority:             r.URL.Query().Get("priority"),
	}
	filter.Limit, filter.Offset, ok = parsePaging(w, r)
	if !ok {
		return
	}

	list, err := h.store.ListRegulatedNotices(r.Context(), filter)
	if err != nil {
		h.log.Error("failed to list regulated notices", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.RegulatedNotice{}
	}
	writeJSON(w, http.StatusOK, list)
}

// UpdateRegulatedNotice updates a regulated notice.
// PATCH /v1/regulated-notices/{id}
func (h *Handler) UpdateRegulatedNotice(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	id := chi.URLParam(r, "id")
	var req domain.RegulatedNotice
	if !decodeJSON(w, r, &req) {
		return
	}

	req.TenantID = tenantID
	req.RegulatedNoticeID = id
	req.UpdatedAt = time.Now().UTC()

	if err := h.store.UpdateRegulatedNotice(r.Context(), &req); err != nil {
		h.log.Error("failed to update regulated notice", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, req)
}

// AcknowledgeRegulatedNotice records an acknowledgment for a regulated notice.
// POST /v1/acknowledgements
func (h *Handler) AcknowledgeRegulatedNotice(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req domain.AcknowledgeRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.RegulatedNoticeID == "" || req.Method == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "regulated_notice_id, method are required")
		return
	}

	// Verify notice exists and is not already acknowledged
	notice, err := h.store.GetRegulatedNotice(r.Context(), req.RegulatedNoticeID)
	if errors.Is(err, domain.ErrRegulatedNoticeNotFound) {
		writeError(w, http.StatusNotFound, "regulated_notice_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get regulated notice", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if notice.Status == domain.RegulatedNoticeStatusAcknowledged {
		writeError(w, http.StatusConflict, "already_acknowledged", string(domain.ErrAlreadyAcknowledged))
		return
	}

	if notice.ExpiresAt != nil && time.Now().UTC().After(*notice.ExpiresAt) {
		writeError(w, http.StatusConflict, "notice_expired", string(domain.ErrNoticeExpired))
		return
	}

	// Update notice status
	now := time.Now().UTC()
	notice.Status = domain.RegulatedNoticeStatusAcknowledged
	notice.AcknowledgedAt = &now
	notice.AcknowledgedBy = principalID
	notice.AcknowledgmentMethod = req.Method
	notice.UpdatedAt = now

	// Build acknowledgment chain evidence
	evidence := req.Evidence
	if evidence == nil {
		evidence = make(map[string]any)
	}
	evidence["ip"] = r.RemoteAddr
	evidence["user_agent"] = r.UserAgent()
	if req.WitnessPrincipalID != "" {
		evidence["witness"] = req.WitnessPrincipalID
	}
	if req.DigitalSignature != "" {
		evidence["digital_signature"] = req.DigitalSignature
	}

	// Add to acknowledgment chain
	chain := notice.AcknowledgmentChain
	if chain == nil {
		chain = make(map[string]any)
	}
	chain[fmt.Sprintf("step_%d", len(chain)+1)] = map[string]any{
		"action":      "acknowledged",
		"actor":       principalID,
		"method":      req.Method,
		"evidence":    evidence,
		"timestamp":   now,
	}
	notice.AcknowledgmentChain = chain

	if err := h.store.UpdateRegulatedNotice(r.Context(), notice); err != nil {
		h.log.Error("failed to update regulated notice after acknowledgment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Add chain step
	chainStep := &domain.AcknowledgmentChainStep{
		ChainID:           uuid.NewString(),
		RegulatedNoticeID: req.RegulatedNoticeID,
		TenantID:          tenantID,
		StepNumber:        len(chain) + 1,
		Action:            "acknowledged",
		Actor:             principalID,
		Method:            string(req.Method),
		Evidence:          evidence,
		CreatedAt:         now,
	}
	_ = h.store.CreateAcknowledgmentChainStep(r.Context(), chainStep)

	writeJSON(w, http.StatusOK, map[string]any{
		"regulated_notice_id": req.RegulatedNoticeID,
		"status":              notice.Status,
		"acknowledged_at":     notice.AcknowledgedAt,
		"acknowledged_by":     notice.AcknowledgedBy,
		"method":              notice.AcknowledgmentMethod,
	})
}

// ListAcknowledgements returns acknowledgment chain for a notice.
// GET /v1/acknowledgements?notice_id=...
func (h *Handler) ListAcknowledgements(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	noticeID := r.URL.Query().Get("notice_id")
	if noticeID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "notice_id query parameter is required")
		return
	}

	chain, err := h.store.GetAcknowledgmentChain(r.Context(), noticeID)
	if err != nil {
		h.log.Error("failed to get acknowledgment chain", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if chain == nil {
		chain = []domain.AcknowledgmentChainStep{}
	}
	writeJSON(w, http.StatusOK, chain)
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

// renderTemplate performs simple variable substitution in a template string.
// Variables are in {{variable_name}} format.
func renderTemplate(tmpl string, vars map[string]string) (string, error) {
	result := tmpl
	for key, value := range vars {
		placeholder := "{{" + key + "}}"
		result = strings.ReplaceAll(result, placeholder, value)
	}
	return result, nil
}

// findMissingVars returns variables that are in required but not in provided.
func findMissingVars(required []string, provided map[string]string) []string {
	var missing []string
	for _, req := range required {
		if _, ok := provided[req]; !ok {
			missing = append(missing, req)
		}
	}
	return missing
}

// findMissingVarsInTemplate finds variables used in template but not declared.
func findMissingVarsInTemplate(subject, body string, declared []string) []string {
	declaredSet := make(map[string]bool)
	for _, v := range declared {
		declaredSet[v] = true
	}

	var missing []string
	// Find {{variable}} patterns
	re := regexp.MustCompile(`\{\{(\w+)\}\}`)
	matches := re.FindAllStringSubmatch(subject+" "+body, -1)
	for _, match := range matches {
		if len(match) > 1 {
			v := match[1]
			if !declaredSet[v] {
				missing = append(missing, v)
			}
		}
	}
	return missing
}
