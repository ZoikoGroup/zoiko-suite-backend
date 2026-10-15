package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/identity"
	"zoiko.io/notification-svc/internal/ledger"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/ncdcode"
	"zoiko.io/notification-svc/internal/quota"
	"zoiko.io/notification-svc/internal/retry"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/telemetry"
	"zoiko.io/notification-svc/internal/templates"
	"zoiko.io/notification-svc/internal/unsubscribe"
	"zoiko.io/notification-svc/internal/webhook"
)

type Store interface {
	CreateNotification(ctx context.Context, n *domain.Notification) (created bool, err error)
	GetNotification(ctx context.Context, id string) (*domain.Notification, error)
	ListNotifications(ctx context.Context, f domain.ListFilter) ([]domain.Notification, error)
	// CompleteDelivery, MarkOutcomeUnknown, ResolveDeliveryOutcome and the
	// template lifecycle writes each enqueue their domain event in the same
	// transaction as the transition (migration 000010, internal/outbox), which
	// is why none of them has a publish step after it and why they take the
	// correlation id the event carries.
	CompleteDelivery(ctx context.Context, id, newStatus, failureReason, providerResponse string, sentAt *time.Time, correlationID string, meta domain.AttemptMeta) error
	ScheduleRetry(ctx context.Context, id, tenantID, failureReason string, attemptedAt, nextAttemptAt time.Time, meta domain.AttemptMeta) error
	// MarkOutcomeUnknown/ResolveDeliveryOutcome back BIZ-10's own
	// PENDING_UNKNOWN handling — see their own doc comments in pg_store.go.
	MarkOutcomeUnknown(ctx context.Context, id, tenantID, reason string, attemptedAt time.Time, correlationID string, meta domain.AttemptMeta) error
	// ListAttempts backs GET /{id}/attempts — the durable per-attempt chain.
	ListAttempts(ctx context.Context, notificationID string) ([]domain.DeliveryAttempt, error)
	// CancelNotification withdraws a queued communication (ZS-SVC-Y-001 6.6).
	CancelNotification(ctx context.Context, id, tenantID, actor, reason string, at time.Time) (*domain.Notification, error)
	// BeginSubmission marks a notification as being handed to a provider,
	// committed before the call (migration 000013).
	BeginSubmission(ctx context.Context, id, tenantID string, at time.Time) error
	// BeginResend reopens a SENT/FAILED notification for one reasoned resend
	// (migration 000014); SetRecipientAddress fills an address the original
	// attempt never obtained.
	BeginResend(ctx context.Context, id, tenantID, actorPrincipalID, reason string, at time.Time) (*domain.Notification, error)
	SetRecipientAddress(ctx context.Context, id, tenantID, address, source string) error
	ResolveDeliveryOutcome(ctx context.Context, p domain.ResolveDeliveryOutcomeParams, resolvedAt time.Time) error
	MarkRead(ctx context.Context, id, recipientPrincipalID string, readAt time.Time) error
	CountUnread(ctx context.Context, recipientPrincipalID string) (int, error)

	CreateTemplate(ctx context.Context, p domain.CreateTemplateParams) (*domain.TemplateDefinition, error)
	GetTemplate(ctx context.Context, templateID string) (*domain.TemplateDefinition, error)
	CreateVersion(ctx context.Context, p domain.CreateVersionParams) (*domain.TemplateVersion, error)
	GetTemplateVersion(ctx context.Context, versionID string) (*domain.TemplateVersion, error)
	ValidateTemplate(ctx context.Context, versionID string) (*domain.TemplateVersion, error)
	ApproveTemplate(ctx context.Context, p domain.ApproveVersionParams) (*domain.TemplateVersion, error)
	PublishTemplate(ctx context.Context, p domain.PublishVersionParams) (*domain.TemplateVersion, error)
	GetPublishedVersion(ctx context.Context, templateID, locale string) (*domain.TemplateVersion, error)
	RetireTemplate(ctx context.Context, p domain.RetireTemplateParams) (*domain.TemplateDefinition, error)
	RenderPreview(ctx context.Context, p domain.RenderPreviewParams) (*domain.RenderPreviewResult, error)
	CompareVersions(ctx context.Context, versionIDA, versionIDB string) (*domain.CompareVersionsResult, error)
	ListLocales(ctx context.Context, templateID string) ([]domain.LocaleSummary, error)
}

// SuppressionStore is the persistence boundary for the email suppression list.
// Satisfied by *store.PgStore.
type SuppressionStore interface {
	AddSuppression(ctx context.Context, supp *ledger.EmailSuppression) error
	IsEmailSuppressed(ctx context.Context, tenantID, recipientEmail string, stream ledger.SenderStream, commClass ledger.CommunicationClass) (bool, string, error)
	ListSuppressions(ctx context.Context, tenantID string, limit, offset int) ([]*ledger.EmailSuppression, error)
}

// RecipientResolver turns a principal into the contact endpoint a message is
// delivered to. Satisfied by internal/identity.Client.
type RecipientResolver interface {
	ResolveEmail(ctx context.Context, tenantID, callerPrincipalID, recipientPrincipalID string) (string, error)
}

// There is no Publisher here. The handler used to publish each event to Kafka
// itself, after the store write had committed, with a broker error only
// logged — so a notice could conclude with no consumer ever told. Events are
// now enqueued by the store inside the transaction that records the fact, and
// internal/outbox's relay publishes them.

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionSend = "NOTIFICATION_SEND"
	actionView = "NOTIFICATION_VIEW"

	// actionTemplateManage gates authorship: CreateTemplate, CreateVersion,
	// ValidateTemplate. actionTemplateApprove gates the separate
	// governance actor's actions: ApproveTemplate, PublishTemplate,
	// RetireTemplate — the doc names two roles ("content owner" and
	// "policy/domain approver"), and every post-authorship lifecycle
	// transition belongs to the second one.
	actionTemplateManage  = "TEMPLATE_MANAGE"
	actionTemplateApprove = "TEMPLATE_APPROVE"

	// actionResolveOutcome gates ResolveDeliveryOutcome — deliberately
	// distinct from actionSend. Resolving an ambiguous attempt is a
	// reconciliation/operator action against a record that already
	// exists, not an act of originating a new notification.
	actionResolveOutcome = "NOTIFICATION_RESOLVE_OUTCOME"

	// actionSuppressionManage gates the admin suppression endpoints. A
	// platform operator editing the suppression list needs a more
	// privileged action than a regular NOTIFICATION_VIEW reader.
	actionSuppressionManage = "NOTIFICATION_SUPPRESS"
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
	store          Store
	authz          AuthZClient
	deliverer      Deliverer
	recipient      RecipientResolver
	log            *zap.Logger
	orchestrator   *ledger.Orchestrator
	ledgerStore    ledger.LedgerStore
	webhookHandler *webhook.Handler
	suppressions   SuppressionStore
	// unsubscribe opens one-click unsubscribe tokens. Nil means unsubscribe
	// is not configured, and the receiver refuses every request.
	unsubscribe *unsubscribe.Codec
	intents     IntentStore
	evidence    EvidenceStore
	// metrics may be nil (tests); every observation is nil-safe.
	metrics *telemetry.Domain

	// inAppOpened, when set, records the first read of an NCD-routed in-app
	// notice as OPENED evidence on its communication (ZS-SVC-Y-001 §7.1).
	inAppOpened InAppOpenedFunc

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
	Store          Store
	AuthZ          AuthZClient
	Deliverer      Deliverer
	Recipient      RecipientResolver
	RetryPolicy    retry.Policy
	Orchestrator   *ledger.Orchestrator
	LedgerStore    ledger.LedgerStore
	WebhookHandler *webhook.Handler
	Suppressions   SuppressionStore
	Unsubscribe    *unsubscribe.Codec
	InAppOpened    InAppOpenedFunc
	// Intents is the communication intent registry (NCD-01). Optional: without it the
	// registry routes answer 503 and a template bound to an intent cannot be sent
	// (fail closed).
	Intents IntentStore
	// Evidence reads normalized delivery evidence (NCD-04). Optional: without it the route answers 503.
	Evidence EvidenceStore
	Metrics  *telemetry.Domain
	Log      *zap.Logger
}

// InAppOpenedFunc records that a recipient opened an NCD in-app notice. The
// inbox row's id is the NCD attempt id; its source_reference the communication.
type InAppOpenedFunc func(ctx context.Context, tenantID, principalID, communicationID, attemptID string, at time.Time) error

func New(d Deps) *Handler {
	return &Handler{
		store:          d.Store,
		authz:          d.AuthZ,
		deliverer:      d.Deliverer,
		recipient:      d.Recipient,
		retryPolicy:    d.RetryPolicy.Normalize(),
		orchestrator:   d.Orchestrator,
		ledgerStore:    d.LedgerStore,
		webhookHandler: d.WebhookHandler,
		suppressions:   d.Suppressions,
		unsubscribe:    d.Unsubscribe,
		intents:        d.Intents,
		evidence:       d.Evidence,
		metrics:        d.Metrics,
		inAppOpened:    d.InAppOpened,
		log:            d.Log,
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

		// Phase 1 Delivery Ledger & Event Ingestion routes
		r.Post("/events/ingest", h.IngestEvent)
		r.Get("/intents/{id}", h.GetIntent)

		// Phase 2 Step 5 Webhook routes
		if h.webhookHandler != nil {
			r.Post("/webhooks/{provider}", h.webhookHandler.HandleWebhook)
		}

		// Phase 3: Admin suppression management
		// POST   /v1/notifications/suppression         — add or update a suppression entry
		// GET    /v1/notifications/suppression         — list active suppressions
		//
		// There is deliberately no DELETE. A suppression is a durable fact (§7.3,
		// migration 000022): it is lifted on evidence through POST
		// /v1/suppressions/{id}/lift, which needs a second principal for a hard
		// bounce, complaint or operator hold, and the row is kept.
		r.Route("/suppression", func(r chi.Router) {
			r.Post("/", h.AddSuppression)
			r.Get("/", h.ListSuppressions)
		})

		// Phase 3: RFC 8058 one-click unsubscribe receiver.
		// This route is exempted from the envelope middleware in main.go because
		// the request originates from a mail client, not a ZoikoSuite service,
		// and carries no X-Principal-Id / X-Tenant-Id headers.
		// The action token in the body provides the identity and tenant context.
		r.Post("/unsubscribe", h.HandleUnsubscribe)

		r.Get("/{id}", h.GetNotification)
		r.Post("/{id}/read", h.MarkRead)
		r.Get("/{id}/delivery-status", h.GetDeliveryStatus)
		r.Get("/{id}/attempts", h.ListAttempts)
		r.Post("/{id}/cancel", h.CancelNotification)
		r.Get("/{id}/evidence", h.ListEvidence)
		r.Post("/{id}/resend", h.ResendNotification)
		r.Post("/{id}/resolve-delivery-outcome", h.ResolveDeliveryOutcome)
	})

	// The NCD control plane (RegisterNCDRoutes) owns /v1/communication-intents,
	// /v1/preferences, /v1/channel-decision and /v1/regulated-notices. The
	// parallel registry main carried on those paths was retired in the 7 Oct
	// merge; a governed template bound to an intent is still enforced on send
	// (enforceIntent).

	r.Route("/v1/document-templates", func(r chi.Router) {
		r.Post("/", h.CreateTemplate)
		r.Get("/{templateID}", h.GetTemplate)
		r.Post("/{templateID}/versions", h.CreateVersion)
		r.Get("/{templateID}/published", h.GetPublishedVersion)
		r.Post("/{templateID}/retire", h.RetireTemplate)
		r.Get("/{templateID}/locales", h.ListLocales)
		r.Get("/{templateID}/compare", h.CompareVersions)
		r.Post("/versions/{versionID}/validate", h.ValidateTemplate)
		r.Post("/versions/{versionID}/approve", h.ApproveTemplate)
		r.Post("/versions/{versionID}/publish", h.PublishTemplate)
		r.Post("/versions/{versionID}/preview", h.RenderPreview)
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

	// A template (static catalogue OR governed BIZ-03 template) renders
	// the body; supplying free-text alongside one would leave it
	// ambiguous which content the recipient actually got. TemplateID and
	// the static Template catalogue are themselves mutually exclusive for
	// the same reason.
	usingStaticTemplate := req.Template != ""
	usingGovernedTemplate := req.TemplateID != ""
	if usingStaticTemplate && usingGovernedTemplate {
		writeError(w, http.StatusBadRequest, "conflicting_content",
			"supply either template or template_id, not both")
		return
	}
	if (usingStaticTemplate || usingGovernedTemplate) && req.Body != "" {
		writeError(w, http.StatusBadRequest, "conflicting_content",
			"supply either a template (with variables) or subject and body, not both")
		return
	}

	if usingStaticTemplate {
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
		case templates.ErrUnexpectedVariables:
			writeError(w, http.StatusBadRequest, "unexpected_template_variables", e.Error())
			return
		default:
			h.log.Error("failed to render notification template",
				zap.String("template", req.Template), zap.Error(err))
			writeError(w, http.StatusInternalServerError, "template_render_failed", e.Error())
			return
		}
	}

	// A governed BIZ-03 template renders only the BODY — templates carry
	// no subject field (a document/form template has no notion of one),
	// so Subject is still required from the caller in this path, checked
	// below alongside every other required field. The actual fetch+render
	// happens further down, AFTER authorization — see that block's own
	// comment on why it cannot happen here.
	if usingGovernedTemplate && req.Locale == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "locale is required when template_id is set")
		return
	}

	if req.RecipientPrincipalID == "" || req.LegalEntityID == "" || req.Channel == "" ||
		(req.Subject == "" && !usingGovernedTemplate) || req.CorrelationID == "" {
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

	// The class decides which suppressions and preferences may block the message
	// (INV-06), so it is stated, validated and fixed at creation. Marketing and
	// lifecycle mail are not a direct send: they need the ledger pipeline's stream
	// identity and one-click unsubscribe (INV-07).
	if req.CommunicationClass != "" && !domain.ValidDirectPathClass(req.CommunicationClass) {
		writeError(w, http.StatusBadRequest, "invalid_communication_class",
			"communication_class must be one of "+strings.Join(domain.DirectPathClasses, ", ")+
				" on the direct send path; marketing (M1) and lifecycle (L1) mail use the ledger pipeline")
		return
	}
	if err := domain.ValidateSchedule(req.Channel, req.NotBefore, req.ExpiresAt, time.Now().UTC()); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
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

	// The governed-template fetch happens here, not earlier: GetPublishedVersion
	// and RenderPreview touch real tenant data (a template's content is itself
	// something a legal entity may not want disclosed to a caller who is not
	// authorized to send for it), so it must not run before the actionSend
	// authorization check above. This is the same fetch-then-authorize-then-use
	// discipline as every mutating handler in this platform, applied to a read
	// that also needs gating.
	// templateVersionID/renderedHash carry BIZ-10's own evidence/lineage
	// requirement ("template/version, rendered hash") onto the notification
	// row — empty for free-text/static-catalogue sends, which cite no
	// governed version.
	var templateVersionID, renderedHash, intentVersionID string
	if usingGovernedTemplate {
		published, err := h.store.GetPublishedVersion(r.Context(), req.TemplateID, req.Locale)
		if err != nil {
			// A template that is published in another locale but not this one is an unapproved
			// LOCALE (NCD-005), never silently replaced by another language; one with nothing
			// published is simply not published (NCD-003).
			if errors.Is(err, domain.ErrTemplateVersionNotFound) && h.otherLocalePublished(r.Context(), req.TemplateID, req.Locale) {
				writeError(w, http.StatusUnprocessableEntity, "locale_not_approved",
					"NCD-005 LOCALE_NOT_APPROVED: the template has no approved version in locale "+req.Locale+"; another language is never substituted")
				return
			}
			h.handleTemplateError(w, err)
			return
		}
		// A template published for a different legal entity than the one this
		// notification is being sent under is refused rather than used — using
		// it anyway would let a caller borrow another entity's approved wording
		// under this send's own legal_entity_id.
		if published.LegalEntityID != req.LegalEntityID {
			writeError(w, http.StatusBadRequest, "template_legal_entity_mismatch",
				"the published template belongs to a different legal_entity_id than this notification is being sent under")
			return
		}
		// A template bound to a communication intent is sent under the intent version in
		// force NOW, and under nothing else (NCD-01, INV-04): the purpose class comes from
		// the intent, the channel must be one it allows, every variable must satisfy its
		// typed contract, and the message is pinned to the exact version.
		tmplDef, derr := h.store.GetTemplate(r.Context(), req.TemplateID)
		if derr != nil {
			h.handleTemplateError(w, derr)
			return
		}
		if tmplDef.IntentID != nil {
			iv, ok := h.enforceIntent(w, r, *tmplDef.IntentID, published, &req)
			if !ok {
				return
			}
			intentVersionID = iv.VersionID
		}
		// A version that carries a subject OWNS it: the subject was reviewed and
		// frozen with the body, and a caller-supplied one would send reviewed
		// wording under unreviewed text (F-05). A version without one is legacy and
		// still needs the caller's.
		if published.Subject != nil && req.Subject != "" {
			writeError(w, http.StatusBadRequest, "conflicting_content",
				"this template version defines its own subject; do not supply one")
			return
		}
		if published.Subject == nil && req.Subject == "" {
			writeError(w, http.StatusBadRequest, "missing_fields",
				"subject is required: this template version does not define one")
			return
		}
		rendered, err := h.store.RenderPreview(r.Context(), domain.RenderPreviewParams{
			VersionID: published.VersionID, Variables: req.Variables,
		})
		if err != nil {
			h.handleTemplateError(w, err)
			return
		}
		if published.Subject != nil {
			req.Subject = rendered.RenderedSubject
		}
		req.Body = rendered.RenderedContent
		templateVersionID = published.VersionID
		sum := sha256.Sum256([]byte(rendered.RenderedContent))
		renderedHash = hex.EncodeToString(sum[:])
	}

	correlationID := getCorrelationID(r)

	// Purpose-scoped idempotency (§3.4, migration 000012). Optional: a send
	// that names no purpose and no key is deduplicated on correlation_id alone,
	// exactly as before.
	purpose := req.PurposeContext
	if purpose == "" {
		purpose = r.Header.Get("X-Purpose-Context")
	}
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" && purpose != "" {
		idempotencyKey = purposeScopedKey(tenantID, req, purpose)
	}
	if len(purpose) > 255 || len(idempotencyKey) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency",
			"purpose_context may be at most 255 characters and idempotency_key at most 500")
		return
	}
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
	queued := domain.Queued(req.NotBefore, now)
	var address, addressSource string
	var resolveErr error
	if !queued {
		// A queued send resolves its recipient when it is sent, so the endpoint it goes to is the
		// current one (6.6), not whatever it was days before.
		address, addressSource, resolveErr = h.resolveRecipient(r.Context(), tenantID, principalID, req)
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
		Status:                 "PENDING",
		SourceEventType:        req.SourceEventType,
		SourceReference:        req.SourceReference,
		CorrelationID:          req.CorrelationID,
		CreatedByPrincipalID:   principalID,
		CreatedAt:              now,
		TemplateID:             req.TemplateID,
		TemplateVersionID:      templateVersionID,
		RenderedContentHash:    renderedHash,
		PurposeContext:         purpose,
		IdempotencyKey:         idempotencyKey,
		CommunicationClass:     req.CommunicationClass,
		IntentVersionID:        intentVersionID,
		NotBefore:              req.NotBefore,
		ExpiresAt:              req.ExpiresAt,
	}
	if queued {
		notification.NextAttemptAt = req.NotBefore
	}

	// Counted against the send quotas (ZS-SVC-Y-001 6.5) in the same transaction that creates it.
	created, err := h.store.CreateNotification(quota.WithCounting(r.Context()), notification)
	var exceeded *quota.ExceededError
	if errors.As(err, &exceeded) {
		h.log.Warn("send refused: quota exceeded",
			zap.String("tenant_id", tenantID), zap.String("dimension", exceeded.Dimension), zap.Int("limit", exceeded.Limit))
		w.Header().Set("Retry-After", strconv.Itoa(int(exceeded.RetryAfter.Seconds())))
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", exceeded.Error())
		return
	}
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

	// A send queued for a future not_before is created and left for the worker, which sends it
	// when due through the same guarded path as any other attempt. Nothing is sent now.
	if queued {
		writeJSON(w, http.StatusCreated, notification)
		return
	}

	// A notification whose recipient could not be resolved is never handed to
	// a provider. Attempting it would produce a second, misleading failure
	// from the transport ("empty To") on top of the real one, and the record
	// would name the mail server rather than the missing address.
	var outcome domain.DeliveryOutcome
	if resolveErr != nil {
		outcome.Reason = ncdcode.Format(ncdcode.RecipientUnresolved) + ": recipient resolution failed: " + resolveErr.Error()
		outcome.Retryable = !identity.IsSettled(resolveErr)
	} else {
		var ok bool
		if outcome, ok = h.submitAndDeliver(w, r, notification, tenantID, telemetry.OriginRequest); !ok {
			return
		}
	}

	h.recordAttemptOutcome(w, r, notification, outcome, domain.AttemptMeta{
		Origin:            domain.AttemptOriginRequest,
		ProviderName:      outcome.ProviderName,
		Retryable:         outcome.Retryable,
		PrivacyDecisionID: outcome.PrivacyDecisionID,
		PrivacyResult:     outcome.PrivacyResult,
		BlockCode:         outcome.BlockCode,
		ActorPrincipalID:  principalID,
	}, correlationID, tenantID, http.StatusCreated)
}

// submitAndDeliver marks the submission and hands the notification to its
// transport. Returns false, having written the response, if the mark could not
// be recorded — in which case the provider was NOT called.
//
// The mark is committed before the provider is called (migration 000013): if
// this process dies mid-attempt, the stranded sweep then knows the provider may
// have the message and does not blindly send it again (§6.2). If the mark
// cannot be written, the row stays unmarked in flight and the sweep revives
// it, which duplicates nothing. IN_APP is never marked: the register row IS
// the delivery, so it cannot be ambiguous.
func (h *Handler) submitAndDeliver(w http.ResponseWriter, r *http.Request, n *domain.Notification, tenantID, origin string) (domain.DeliveryOutcome, bool) {
	if n.Channel != domain.ChannelInApp {
		if err := h.store.BeginSubmission(r.Context(), n.NotificationID, tenantID, time.Now().UTC()); err != nil {
			h.log.Error("failed to mark the submission; the provider was not called", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return domain.DeliveryOutcome{}, false
		}
	}
	started := time.Now()
	outcome := h.deliverer.Deliver(r.Context(), *n)
	h.metrics.ObserveAttempt(n.Channel, retry.AttemptOutcome(outcome), origin, time.Since(started).Seconds())
	return outcome, true
}

// recordAttemptOutcome records what one synchronous attempt achieved —
// ambiguous, rescheduled, or concluded — and writes the response with
// successCode. Shared by the first attempt (SendNotification) and an explicit
// resend (ResendNotification); meta says which it was, and is written as the
// attempt's durable row by whichever transition records its effect
// (migration 000011).
func (h *Handler) recordAttemptOutcome(w http.ResponseWriter, r *http.Request, notification *domain.Notification,
	outcome domain.DeliveryOutcome, meta domain.AttemptMeta, correlationID, tenantID string, successCode int) {
	attemptedAt := time.Now().UTC()
	attemptNumber := notification.DeliveryAttempts + 1

	// The OUTCOME of an attempt already made is recorded on a context that
	// outlives the request, not on r.Context().
	//
	// WHY. Once the provider has been called, what happened is a fact about
	// the outside world, and the caller hanging up does not un-send an email.
	// r.Context() is cancelled when the response is written — and sooner if
	// the client disconnects or the server's 15s WriteTimeout fires — so the
	// statements below could fail for no reason but the request ending,
	// leaving the notification PENDING with nothing scheduled: in flight
	// forever, which is the stranded state internal/retry's sweep exists to
	// repair. Five rows on the dev stack were in exactly that state for six
	// days.
	//
	// The tenant is carried over explicitly: the store reads it from the
	// context, and a bare context.Background() would have no tenant installed
	// and be refused by row-level security.
	outcomeCtx, cancelOutcome := context.WithTimeout(
		svcmiddleware.WithTenant(context.WithoutCancel(r.Context()), tenantID), 10*time.Second)
	defer cancelOutcome()

	// An ambiguous outcome is neither a success nor a settled failure — see
	// domain.DeliveryOutcome.Unknown's own doc comment. It is checked before
	// Retryable so an outcome that is genuinely unknown can never also be
	// silently retried (which risks a duplicate if the message did go out).
	if outcome.Unknown {
		// notification.outcome_unknown is enqueued by the store in the same
		// transaction as the transition — see PgStore.MarkOutcomeUnknown.
		if err := h.store.MarkOutcomeUnknown(outcomeCtx, notification.NotificationID, tenantID, outcome.Reason, attemptedAt, correlationID, meta); err != nil {
			h.log.Error("failed to record ambiguous delivery outcome", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
		h.metrics.ObserveConclusion(notification.Channel, domain.StatusPendingUnknown)
		notification.Status = domain.StatusPendingUnknown
		notification.FailureReason = outcome.Reason
		notification.DeliveryAttempts = attemptNumber
		notification.LastAttemptAt = &attemptedAt
		notification.SentAt = &attemptedAt
		notification.UnknownAt = &attemptedAt

		h.log.Warn("delivery outcome ambiguous",
			zap.String("notification_id", notification.NotificationID),
			zap.String("origin", meta.Origin),
			zap.String("reason", outcome.Reason))

		writeJSON(w, successCode, notification)
		return
	}

	// A failure worth re-attempting does not conclude the notification. It
	// stays PENDING with a schedule on it, and internal/retry's worker picks
	// it up — which is the whole difference between classifying a failure and
	// doing something about it.
	if !outcome.Delivered && outcome.Retryable {
		if next, ok := h.retryPolicy.NextAttemptFor(attemptedAt, attemptNumber, outcome.DeferUntil); ok {
			if err := h.store.ScheduleRetry(outcomeCtx, notification.NotificationID,
				tenantID, outcome.Reason, attemptedAt, next, meta); err != nil {
				h.log.Error("failed to schedule delivery retry", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
				return
			}
			h.metrics.ObserveRetryScheduled(notification.Channel)
			notification.Status = domain.StatusPending
			notification.FailureReason = outcome.Reason
			notification.DeliveryAttempts = attemptNumber
			notification.LastAttemptAt = &attemptedAt
			notification.NextAttemptAt = &next

			h.log.Warn("delivery failed, scheduled for retry",
				zap.String("notification_id", notification.NotificationID),
				zap.String("origin", meta.Origin),
				zap.Time("next_attempt_at", next),
				zap.String("reason", outcome.Reason))

			// No notification.failed event: nothing has failed yet. Publishing
			// one here and a notification.sent two minutes later would have
			// consumers act on an outcome that did not happen.
			writeJSON(w, successCode, notification)
			return
		}
		// Retry budget exhausted or disabled by configuration. Conclude,
		// rather than sit PENDING with nothing scheduled to move it.
		outcome.Reason += " (retry is disabled by configuration)"
	}

	newStatus := domain.StatusSent
	if !outcome.Delivered {
		newStatus = domain.StatusFailed
	}

	// notification.sent / notification.failed is enqueued by the store in the
	// same transaction as the conclusion — see PgStore.CompleteDelivery. The
	// handler never publishes: a publish after the commit could be lost with
	// the conclusion already recorded, which is the defect the outbox removes.
	if err := h.store.CompleteDelivery(outcomeCtx, notification.NotificationID,
		newStatus, outcome.Reason, outcome.ProviderResponse, &attemptedAt, correlationID, meta); err != nil {
		h.log.Error("failed to record delivery outcome", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.metrics.ObserveConclusion(notification.Channel, newStatus)
	notification.Status = newStatus
	notification.FailureReason = outcome.Reason
	notification.ProviderResponse = outcome.ProviderResponse
	notification.SentAt = &attemptedAt
	notification.DeliveryAttempts = attemptNumber
	notification.LastAttemptAt = &attemptedAt

	writeJSON(w, successCode, notification)
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
		// Either vocabulary: the precise §3.3 state or the stored value.
		Status:     domain.StoredStatusFor(r.URL.Query().Get("status")),
		UnreadOnly: r.URL.Query().Get("unread_only") == "true",
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

	if h.inAppOpened != nil && notification.ReadAt == nil && notification.SourceEventType == "ncd.communication" {
		if err := h.inAppOpened(r.Context(), notification.TenantID, principalID, notification.SourceReference,
			notification.NotificationID, readAt); err != nil {
			// The read itself is recorded; the evidence write is logged
			// rather than turning a successful read into an error.
			h.log.Warn("failed to record NCD in-app OPENED evidence", zap.Error(err))
		}
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

// deliveryStatusResponse is GetDeliveryStatus's own response shape — the
// doc's own query, a narrower view than the full notification record.
// ErrorCode surfaces the doc's own named stable error,
// DELIVERY_OUTCOME_UNKNOWN, describing the notification's own state — a
// 200 response, not a request failure.
type deliveryStatusResponse struct {
	NotificationID string     `json:"notification_id"`
	Status         string     `json:"status"`
	ErrorCode      string     `json:"error_code,omitempty"`
	FailureReason  string     `json:"failure_reason,omitempty"`
	SentAt         *time.Time `json:"sent_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}

// GetDeliveryStatus — BIZ-10's own GetDeliveryStatus query.
// GET /v1/notifications/{id}/delivery-status
func (h *Handler) GetDeliveryStatus(w http.ResponseWriter, r *http.Request) {
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

	resp := deliveryStatusResponse{
		NotificationID: notification.NotificationID, Status: domain.DeliveryStateOf(*notification),
		FailureReason: notification.FailureReason, SentAt: notification.SentAt, ResolvedAt: notification.ResolvedAt,
	}
	if notification.Status == domain.StatusPendingUnknown {
		resp.ErrorCode = "DELIVERY_OUTCOME_UNKNOWN"
	}
	writeJSON(w, http.StatusOK, resp)
}

type resolveDeliveryOutcomeRequest struct {
	ResolvedStatus   string `json:"resolved_status"`
	ResolutionNote   string `json:"resolution_note"`
	ProviderResponse string `json:"provider_response,omitempty"`
}

// ResolveDeliveryOutcome — BIZ-10's own ResolveDeliveryOutcome command.
// POST /v1/notifications/{id}/resolve-delivery-outcome
//
// Gated on actionResolveOutcome, not actionSend — see that constant's own
// doc comment. Fetched first so a wrong-status target gets its own
// distinguishable error rather than the store's generic "no row
// changed" — same discipline as MarkRead above.
func (h *Handler) ResolveDeliveryOutcome(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req resolveDeliveryOutcomeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ResolvedStatus != domain.StatusSent && req.ResolvedStatus != domain.StatusFailed {
		writeError(w, http.StatusBadRequest, "invalid_resolved_status", domain.ErrInvalidResolvedStatus.Error())
		return
	}
	if req.ResolutionNote == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", domain.ErrResolutionNoteRequired.Error())
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, notification.LegalEntityID, actionResolveOutcome); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if notification.Status != domain.StatusPendingUnknown {
		writeError(w, http.StatusConflict, "not_pending_unknown", domain.ErrNotPendingUnknown.Error())
		return
	}

	resolvedAt := time.Now().UTC()
	err = h.store.ResolveDeliveryOutcome(r.Context(), domain.ResolveDeliveryOutcomeParams{
		NotificationID: id, TenantID: tenantID, ActorPrincipalID: principalID,
		ResolvedStatus: req.ResolvedStatus, ResolutionNote: req.ResolutionNote, ProviderResponse: req.ProviderResponse,
		// The event the original attempt would have emitted is enqueued by the
		// store with the resolution — the spec names no separate "resolved"
		// event, only "resolve the original attempt".
		CorrelationID: getCorrelationID(r),
	}, resolvedAt)
	if err != nil {
		if errors.Is(err, domain.ErrNotificationNotFound) {
			// A race: the row moved (or vanished under a replay) between
			// the fetch above and this call. 409 rather than 404 — the
			// notification exists, it just stopped being PENDING_UNKNOWN.
			writeError(w, http.StatusConflict, "not_pending_unknown", domain.ErrNotPendingUnknown.Error())
			return
		}
		h.log.Error("failed to resolve delivery outcome", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	updated, err := h.store.GetNotification(r.Context(), id)
	if err != nil {
		h.log.Error("failed to re-read notification after resolving outcome", zap.Error(err))
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

// ── BIZ-03 Template ──────────────────────────────────────────────────────────

type createTemplateRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	Name            string `json:"name"`
	BusinessPurpose string `json:"business_purpose"`
	// IntentID optionally binds the template to a communication intent, for good (NCD-01).
	IntentID string `json:"intent_id,omitempty"`
}

// CreateTemplate creates a new template definition — BIZ-03's own
// CreateTemplate command. The caller becomes the owner.
func (h *Handler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createTemplateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.Name == "" || req.BusinessPurpose == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, name and business_purpose are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tmpl, err := h.store.CreateTemplate(r.Context(), domain.CreateTemplateParams{
		LegalEntityID: req.LegalEntityID, Name: req.Name, BusinessPurpose: req.BusinessPurpose, OwnerPrincipalID: principalID, IntentID: req.IntentID,
		CorrelationID: getCorrelationID(r),
	})
	if err != nil {
		if isIntentBindingError(err) {
			h.writeIntentError(w, err)
			return
		}
		h.log.Error("failed to create template", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, tmpl)
}

// GetTemplate — BIZ-03's own GetTemplate query.
func (h *Handler) GetTemplate(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	templateID := chi.URLParam(r, "templateID")
	tmpl, err := h.store.GetTemplate(r.Context(), templateID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tmpl.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tmpl)
}

type createVersionRequest struct {
	Locale         string   `json:"locale"`
	Content        string   `json:"content"`
	VariableSchema []string `json:"variable_schema,omitempty"`
	// Subject is the reviewed subject text, placeholders only; SubjectVariables are
	// the variables the author declares safe to appear in it (a subset of
	// variable_schema). Omit both to leave the subject to the sender (legacy).
	Subject               string   `json:"subject,omitempty"`
	SubjectVariables      []string `json:"subject_variables,omitempty"`
	BrandingMetadata      string   `json:"branding_metadata,omitempty"`
	AccessibilityMetadata string   `json:"accessibility_metadata,omitempty"`
}

// CreateVersion — BIZ-03's own CreateVersion command. Lands DRAFT;
// requires ValidateTemplate then ApproveTemplate then PublishTemplate
// before it governs anything a caller can render.
func (h *Handler) CreateVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req createVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Locale == "" || req.Content == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "locale and content are required")
		return
	}
	templateID := chi.URLParam(r, "templateID")
	tmpl, err := h.store.GetTemplate(r.Context(), templateID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tmpl.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	version, err := h.store.CreateVersion(r.Context(), domain.CreateVersionParams{
		TemplateID: templateID, Locale: req.Locale, Content: req.Content, VariableSchema: req.VariableSchema,
		Subject: req.Subject, SubjectVariables: req.SubjectVariables,
		BrandingMetadata: req.BrandingMetadata, AccessibilityMetadata: req.AccessibilityMetadata,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, version)
}

// ValidateTemplate — BIZ-03's own ValidateTemplate command. Moves a
// DRAFT version to REVIEW once its content parses and a variable schema
// is present.
func (h *Handler) ValidateTemplate(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	existing, err := h.store.GetTemplateVersion(r.Context(), versionID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	version, err := h.store.ValidateTemplate(r.Context(), versionID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// ApproveTemplate — BIZ-03's own ApproveTemplate command. Fetched
// (read-only) BEFORE authorization and BEFORE the mutation, same
// fetch-then-authorize-then-mutate discipline as every other handler in
// this platform, so a denied caller can never cause the approval to
// actually run before being refused.
func (h *Handler) ApproveTemplate(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	existing, err := h.store.GetTemplateVersion(r.Context(), versionID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTemplateApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	version, err := h.store.ApproveTemplate(r.Context(), domain.ApproveVersionParams{
		VersionID: versionID, ApprovedByPrincipalID: principalID, CorrelationID: getCorrelationID(r),
	})
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// PublishTemplate — BIZ-03's own PublishTemplate command.
func (h *Handler) PublishTemplate(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	existing, err := h.store.GetTemplateVersion(r.Context(), versionID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTemplateApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	version, err := h.store.PublishTemplate(r.Context(), domain.PublishVersionParams{
		VersionID: versionID, PublishedByPrincipalID: principalID, CorrelationID: getCorrelationID(r),
	})
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// GetPublishedVersion — BIZ-03's own GetPublishedVersion query.
func (h *Handler) GetPublishedVersion(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	templateID := chi.URLParam(r, "templateID")
	locale := r.URL.Query().Get("locale")
	if locale == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "locale query parameter is required")
		return
	}
	tmpl, err := h.store.GetTemplate(r.Context(), templateID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tmpl.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	version, err := h.store.GetPublishedVersion(r.Context(), templateID, locale)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

// RetireTemplate — BIZ-03's own RetireTemplate command.
func (h *Handler) RetireTemplate(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	templateID := chi.URLParam(r, "templateID")
	tmpl, err := h.store.GetTemplate(r.Context(), templateID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tmpl.LegalEntityID, actionTemplateApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	retired, err := h.store.RetireTemplate(r.Context(), domain.RetireTemplateParams{
		TemplateID: templateID, RetiredByPrincipalID: principalID, CorrelationID: getCorrelationID(r),
	})
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, retired)
}

type renderPreviewRequest struct {
	Variables map[string]string `json:"variables,omitempty"`
}

// RenderPreview — BIZ-03's own RenderPreview query. Renders any
// version's content, regardless of status, so a reviewer can see a
// DRAFT/REVIEW version before it is ever published.
func (h *Handler) RenderPreview(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req renderPreviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	versionID := chi.URLParam(r, "versionID")
	existing, err := h.store.GetTemplateVersion(r.Context(), versionID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	result, err := h.store.RenderPreview(r.Context(), domain.RenderPreviewParams{VersionID: versionID, Variables: req.Variables, Placeholders: true})
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// CompareVersions — BIZ-03's own CompareVersions query. Both version ids
// are query parameters, not path segments — this reads two versions,
// neither of which "owns" the comparison route.
func (h *Handler) CompareVersions(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	versionA := r.URL.Query().Get("version_a")
	versionB := r.URL.Query().Get("version_b")
	if versionA == "" || versionB == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "version_a and version_b query parameters are required")
		return
	}
	templateID := chi.URLParam(r, "templateID")
	tmpl, err := h.store.GetTemplate(r.Context(), templateID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tmpl.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	result, err := h.store.CompareVersions(r.Context(), versionA, versionB)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ListLocales — BIZ-03's own ListLocales query.
func (h *Handler) ListLocales(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	templateID := chi.URLParam(r, "templateID")
	tmpl, err := h.store.GetTemplate(r.Context(), templateID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tmpl.LegalEntityID, actionTemplateManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	locales, err := h.store.ListLocales(r.Context(), templateID)
	if err != nil {
		h.handleTemplateError(w, err)
		return
	}
	if locales == nil {
		locales = []domain.LocaleSummary{}
	}
	writeJSON(w, http.StatusOK, locales)
}

func (h *Handler) handleTemplateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrTemplateNotFound):
		writeError(w, http.StatusNotFound, "template_not_found", "")
	case errors.Is(err, domain.ErrTemplateVersionNotFound):
		writeError(w, http.StatusNotFound, "template_version_not_found", "")
	case errors.Is(err, domain.ErrTemplateRetired):
		writeError(w, http.StatusConflict, "template_retired", err.Error())
	case errors.Is(err, domain.ErrTemplateAlreadyRetired):
		writeError(w, http.StatusConflict, "already_retired", err.Error())
	case errors.Is(err, domain.ErrTemplateVersionNotDraft):
		writeError(w, http.StatusConflict, "not_draft", err.Error())
	case errors.Is(err, domain.ErrTemplateVersionNotReview):
		writeError(w, http.StatusConflict, "not_review", err.Error())
	case errors.Is(err, domain.ErrTemplateVersionNotApproved):
		writeError(w, http.StatusConflict, "not_approved", err.Error())
	case errors.Is(err, domain.ErrTemplateVersionSelfApproval):
		writeError(w, http.StatusForbidden, "self_approval_forbidden", err.Error())
	case errors.Is(err, domain.ErrTemplateContentInvalid):
		writeError(w, http.StatusBadRequest, "invalid_content", err.Error())
	case errors.Is(err, domain.ErrSubjectInvalid):
		writeError(w, http.StatusBadRequest, "invalid_subject", err.Error())
	case isIntentBindingError(err):
		h.writeIntentError(w, err)
	case errors.Is(err, domain.ErrTemplateLocaleRequired):
		writeError(w, http.StatusBadRequest, "missing_fields", err.Error())
	case errors.Is(err, domain.ErrTemplateVersionsBelongToDifferentTemplates):
		writeError(w, http.StatusBadRequest, "version_template_mismatch", err.Error())
	default:
		var missing domain.ErrTemplateVariablesMissing
		if errors.As(err, &missing) {
			writeError(w, http.StatusBadRequest, "missing_variables", missing.Error())
			return
		}
		var unexpected domain.ErrTemplateVariablesUnexpected
		if errors.As(err, &unexpected) {
			writeError(w, http.StatusBadRequest, "unexpected_variables", unexpected.Error())
			return
		}
		h.log.Error("template store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
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
	body := map[string]string{
		"error_code":    code,
		"error_message": msg,
	}
	// Where the condition is exactly one the standard names, the stable code (section 10.3)
	// rides beside the service's own. Added, never substituted: existing callers keep working.
	if stable, ok := ncdcode.ForAPIError(code); ok {
		body["reason_code"] = stable
		body["reason"] = ncdcode.Names[stable]
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── POST /v1/notifications/events/ingest ────────────────────────────────────

// IngestEvent ingests a business domain event and coordinates the Phase 1 communications
// pipeline: deduplication, recipient resolution, kill-switch check, template integrity,
// deterministic render, ledger recording, and delivery dispatch.
func (h *Handler) IngestEvent(w http.ResponseWriter, r *http.Request) {
	if h.orchestrator == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "orchestrator not configured")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	_, ok = h.requireTenant(w, r)
	if !ok {
		return
	}

	var req ledger.EventIngestRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Default correlation and causation from request envelope headers if omitted from body
	if req.CorrelationID == "" {
		req.CorrelationID = getCorrelationID(r)
	}
	if req.CausationID == nil {
		if cid := r.Header.Get("X-Causation-Id"); cid != "" {
			req.CausationID = &cid
		}
	}
	if req.LegalEntityID == "" {
		req.LegalEntityID = r.Header.Get("X-Legal-Entity-Id")
	}

	// Strictly validate mandatory request attributes
	if req.EventID == "" || req.EventType == "" || req.RecipientPrincipalID == "" ||
		req.TemplateKey == "" || req.LegalEntityID == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"event_id, event_type, recipient_principal_id, template_key, legal_entity_id, correlation_id are required")
		return
	}

	// Authorization check
	if h.authz != nil {
		if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionSend); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	res, err := h.orchestrator.IngestEvent(r.Context(), req, principalID)
	if err != nil {
		switch {
		case errors.Is(err, ledger.ErrMissingTenantContext):
			writeError(w, http.StatusUnauthorized, "tenant_missing", err.Error())
		case errors.Is(err, ledger.ErrInvalidIngestRequest):
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		case errors.Is(err, ledger.ErrTemplateNotFound):
			writeError(w, http.StatusBadRequest, "unknown_template", err.Error())
		case errors.Is(err, ledger.ErrMissingVariables):
			writeError(w, http.StatusBadRequest, "missing_template_variables", err.Error())
		case errors.Is(err, ledger.ErrHashMismatch):
			h.log.Error("template integrity verification failed", zap.String("template_key", req.TemplateKey), zap.Error(err))
			writeError(w, http.StatusInternalServerError, "template_integrity_failure", "template hash integrity check failed")
		case errors.Is(err, ledger.ErrRecipientEmailUnresolved):
			writeError(w, http.StatusUnprocessableEntity, "recipient_unresolved", err.Error())
		case errors.Is(err, ledger.ErrUnsubscribeUnavailable):
			writeError(w, http.StatusServiceUnavailable, "unsubscribe_unavailable", err.Error())
		default:
			h.log.Error("event orchestration failed", zap.String("event_id", req.EventID), zap.Error(err))
			writeError(w, http.StatusInternalServerError, "orchestration_failed", err.Error())
		}
		return
	}

	status := http.StatusCreated
	if res.IsReplay {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

// ── GET /v1/notifications/intents/{id} ──────────────────────────────────────

type IntentDetailResponse struct {
	*ledger.MessageIntent
	Render *ledger.MessageRender `json:"render,omitempty"`
}

// GetIntent retrieves a message intent and associated render under strict tenant RLS.
func (h *Handler) GetIntent(w http.ResponseWriter, r *http.Request) {
	if h.ledgerStore == nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "ledger store not configured")
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

	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must be a valid UUID")
		return
	}

	intent, err := h.ledgerStore.GetMessageIntent(r.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, store.ErrIntentNotFound) {
			writeError(w, http.StatusNotFound, "intent_not_found", "message intent not found")
			return
		}
		h.log.Error("failed to get message intent", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if h.authz != nil && intent.LegalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, intent.LegalEntityID, actionView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	render, rErr := h.ledgerStore.GetRenderByIntent(r.Context(), tenantID, id)
	if rErr != nil && !errors.Is(rErr, store.ErrRenderNotFound) {
		h.log.Warn("failed to fetch render for intent", zap.String("intent_id", id), zap.Error(rErr))
	}

	writeJSON(w, http.StatusOK, IntentDetailResponse{
		MessageIntent: intent,
		Render:        render,
	})
}

// ListAttempts — every durable provider submission for one notification, oldest
// first (ZS-SVC-Y-001 §3.4: "every provider submission has a separate durable
// attempt_id"). GET /v1/notifications/{id}/attempts
//
// Authorized exactly like GetNotification: the attempt chain is part of the
// notification's record, readable by whoever may read the record.
func (h *Handler) ListAttempts(w http.ResponseWriter, r *http.Request) {
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

	attempts, err := h.store.ListAttempts(r.Context(), id)
	if err != nil {
		h.log.Error("failed to list delivery attempts", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if attempts == nil {
		attempts = []domain.DeliveryAttempt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"notification_id": id,
		"attempts":        attempts,
	})
}

// purposeScopedKey derives the §3.4 idempotency key for a send that named a
// purpose but supplied no key of its own: stable across retries of the same
// request, distinct for a different purpose, recipient or channel raised by
// the same business event. Hashed so the key has a fixed length and carries
// no free text into an index.
func purposeScopedKey(tenantID string, req domain.SendNotificationRequest, purpose string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x1f" + req.LegalEntityID + "\x1f" + req.CorrelationID + "\x1f" +
		purpose + "\x1f" + req.RecipientPrincipalID + "\x1f" + req.Channel))
	return "psk-" + hex.EncodeToString(sum[:])
}

// otherLocalePublished reports whether the template has an approved, published version in some
// locale other than the one asked for, which is what makes a missing locale an unapproved
// locale rather than an unpublished template.
func (h *Handler) otherLocalePublished(ctx context.Context, templateID, locale string) bool {
	locales, err := h.store.ListLocales(ctx, templateID)
	if err != nil {
		return false
	}
	for _, l := range locales {
		if l.Locale != locale && l.PublishedVersionID != nil {
			return true
		}
	}
	return false
}
