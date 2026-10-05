package domain

import "time"

type Notification struct {
	NotificationID       string `json:"notification_id"`
	TenantID             string `json:"tenant_id"`
	LegalEntityID        string `json:"legal_entity_id"`
	RecipientPrincipalID string `json:"recipient_principal_id"`

	// RecipientAddress is the endpoint a remote channel actually delivered to
	// — an email address for EMAIL — captured at send time and never
	// recomputed. It is a snapshot, deliberately: resolving it again at read
	// time would show today's address for a notice that went somewhere else
	// last month, and "which address did we actually use" is the whole
	// question when someone says they never received a notice.
	//
	// Empty for IN_APP, which has no endpoint outside the platform.
	RecipientAddress string `json:"recipient_address,omitempty"`

	// RecipientAddressSource records where that address came from. The NCD
	// spec (ZS-SVC-Y-001 §0.4) names "mandatory notices being sent to an
	// unverified or stale free-text address with no recipient provenance" as
	// a thing this control plane exists to prevent, so provenance is stored
	// rather than inferred.
	//
	//   IDENTITY_CONTEXT — resolved from identity-context-svc's principal record
	//   REQUEST          — supplied verbatim by the calling service
	RecipientAddressSource string `json:"recipient_address_source,omitempty"`

	Channel string `json:"channel"` // EMAIL, SMS, IN_APP, WEBHOOK
	From    string `json:"from,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Status  string `json:"status"` // PENDING, SENT, FAILED

	SourceEventType string `json:"source_event_type,omitempty"`
	SourceReference string `json:"source_reference,omitempty"`
	CorrelationID   string `json:"correlation_id"`
	FailureReason   string `json:"failure_reason,omitempty"`

	// ProviderResponse is what the delivery provider said when it accepted the
	// message — an SMTP queue id, or a provider message id. It is evidence of
	// acceptance and nothing more: §0.4 of the same spec forbids treating a
	// provider's "accepted" as proof that a person received, read or was
	// legally served with a notice, and SENT on this record means exactly
	// "a provider took it", never "it arrived".
	ProviderResponse string `json:"provider_response,omitempty"`

	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	CreatedAt            time.Time  `json:"created_at"`
	SentAt               *time.Time `json:"sent_at,omitempty"`

	// ReadAt is set when the recipient acknowledges an in-app notice. Nil
	// means unread. Only the recipient can set it, and it is never unset —
	// see Store.MarkRead.
	ReadAt *time.Time `json:"read_at,omitempty"`

	// DeliveryAttempts counts attempts actually made, so a notification that
	// eventually succeeded still records how much work that took.
	DeliveryAttempts int `json:"delivery_attempts"`

	// NextAttemptAt schedules the next try. It is meaningful only while
	// Status is PENDING, and the schema enforces that: a concluded
	// notification with a retry scheduled would have the worker re-sending a
	// message already delivered.
	//
	//   PENDING + NextAttemptAt set → will be attempted again
	//   PENDING + NextAttemptAt nil → in flight right now
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`

	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`

	// UnknownAt is set when this notification enters PENDING_UNKNOWN — the
	// attempt happened (SentAt is set too, same as any concluded attempt)
	// but the outcome is genuinely ambiguous. Nil for every other status.
	UnknownAt *time.Time `json:"unknown_at,omitempty"`

	// ResolvedAt/ResolvedByPrincipalID/ResolutionNote are
	// ResolveDeliveryOutcome's own evidence — who settled an ambiguous
	// attempt, when, and why. Set exactly once, moving Status to SENT or
	// FAILED.
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
	ResolvedByPrincipalID string     `json:"resolved_by_principal_id,omitempty"`
	ResolutionNote        string     `json:"resolution_note,omitempty"`

	// TemplateID/TemplateVersionID/RenderedContentHash are the doc's own
	// evidence/lineage requirement ("template/version, rendered hash"),
	// captured at send time. Empty for a notification sent from free-text
	// subject/body or the static catalogue (internal/templates), which
	// name no governed BIZ-03 template version to cite.
	TemplateID          string `json:"template_id,omitempty"`
	TemplateVersionID   string `json:"template_version_id,omitempty"`
	RenderedContentHash string `json:"rendered_content_hash,omitempty"`

	// PurposeContext and IdempotencyKey are the §3.4 purpose-scoped dedup key
	// (migration 000012). Both empty for a send that named no purpose, which is
	// then deduplicated on (tenant_id, correlation_id) exactly as before.
	PurposeContext string `json:"purpose_context,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// CommunicationClass is what KIND of message this is (S0 security, T0
	// transactional, A1 operational, L1 lifecycle, M1 marketing), fixed when the row
	// is created (migration 000019). Empty means the sender stated none and the
	// message is judged as T0.
	CommunicationClass string `json:"communication_class,omitempty"`

	// MessageIntentID is the ledger intent this notification was produced from, when
	// there is one (migration 000016); empty for a direct send. With the notification
	// id (the communication id) it is how one communication is recognised from both
	// send paths.
	MessageIntentID string `json:"message_intent_id,omitempty"`

	// IntentVersionID is the exact communication intent version the message was sent
	// under (migration 000021), fixed at creation. Empty when the send used no intent.
	IntentVersionID string `json:"intent_version_id,omitempty"`

	// Resend summary (migration 000014). The full reasoned chain is in the
	// attempt records; these say how often and why most recently.
	ResendCount             int        `json:"resend_count"`
	LastResendReason        string     `json:"last_resend_reason,omitempty"`
	LastResentAt            *time.Time `json:"last_resent_at,omitempty"`
	LastResentByPrincipalID string     `json:"last_resent_by_principal_id,omitempty"`
}

// Status values for Notification.Status. Named here so new code has a
// single source of truth to reference; existing call sites' raw string
// literals ("PENDING", "SENT", "FAILED") are left as they were — this
// does not rename anything already written.
const (
	StatusPending        = "PENDING"
	StatusSent           = "SENT"
	StatusFailed         = "FAILED"
	StatusPendingUnknown = "PENDING_UNKNOWN"
)

// Retrying reports whether delivery has not concluded and another attempt is
// scheduled. Kept as a method so the console and the service agree on what the
// combination means rather than each re-deriving it.
func (n Notification) Retrying() bool {
	return n.Status == "PENDING" && n.NextAttemptAt != nil
}

// DueRetry is one notification the retry worker has found waiting.
//
// It carries an id and a tenant and nothing else, on purpose. The claim query
// that produces it runs under the platform-scope hatch — the one connection in
// this service that can read across tenants — and the narrowness of this struct
// is what keeps message content, subjects and recipient addresses from crossing
// that boundary. Everything else is read afterwards, tenant-scoped.
type DueRetry struct {
	NotificationID string
	TenantID       string

	// Submitted is set only by FindStrandedDeliveries: the stranded row
	// carries a submitting_since marker (migration 000013), so it was handed
	// to a provider and its outcome was lost. It must become PENDING_UNKNOWN,
	// never be blindly re-sent (§6.2).
	Submitted bool
}

// ResolveDeliveryOutcomeParams — BIZ-10's own ResolveDeliveryOutcome
// command. ResolvedStatus must be StatusSent or StatusFailed — the two
// conclusions a real delivery attempt could have reached. ProviderResponse
// is optional (the resolver may have new acceptance evidence — a provider
// support ticket confirming the message DID go out — or none at all if
// resolving to FAILED).
type ResolveDeliveryOutcomeParams struct {
	NotificationID, TenantID, ActorPrincipalID string
	ResolvedStatus                             string
	ResolutionNote                             string
	ProviderResponse                           string
	// CorrelationID is carried onto the event the resolution enqueues.
	CorrelationID string
}

type SendNotificationRequest struct {
	RecipientPrincipalID string `json:"recipient_principal_id"`
	LegalEntityID        string `json:"legal_entity_id"`
	Channel              string `json:"channel"`
	Subject              string `json:"subject"`
	Body                 string `json:"body"`
	SourceEventType      string `json:"source_event_type,omitempty"`
	SourceReference      string `json:"source_reference,omitempty"`
	CorrelationID        string `json:"correlation_id"`

	// PurposeContext scopes deduplication to the business purpose of the
	// communication (§3.4). OPTIONAL: when absent (and the X-Purpose-Context
	// envelope header is absent too) a send is deduplicated on correlation_id
	// alone, exactly as before. When present, two communications that share a
	// correlation_id but differ in purpose, recipient or channel stay distinct.
	PurposeContext string `json:"purpose_context,omitempty"`

	// IdempotencyKey lets a caller supply the purpose-scoped key it derived
	// from the originating business event itself; otherwise one is derived.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// CommunicationClass states what KIND of message this is. The direct path
	// accepts S0 (security), T0 (transactional) and A1 (operational); marketing (M1)
	// and lifecycle (L1) mail must use the ledger pipeline, which carries the stream
	// sender identity and one-click unsubscribe headers this path does not. Empty is
	// treated as T0.
	CommunicationClass string `json:"communication_class,omitempty"`

	// RecipientAddress overrides recipient resolution for channels that need
	// an endpoint. Left empty — the normal case — the address is resolved from
	// identity-context-svc's record for RecipientPrincipalID, which is the
	// authoritative contact fact.
	//
	// The override exists because not every recipient is an established
	// principal with a stored address: the registration_received template goes
	// to someone whose organization has not been approved yet. Whichever path
	// supplied the address is recorded on the notification, so a stored
	// address and a caller-supplied one are never confusable after the fact.
	RecipientAddress string `json:"recipient_address,omitempty"`

	// Template names a catalogue template to render instead of supplying
	// subject and body directly. Variables fills its placeholders.
	//
	// TemplateID (with Locale) names a governed BIZ-03 template instead —
	// its currently PUBLISHED version for that locale is rendered as the
	// body. BIZ-03 templates carry no subject field (a document/form
	// template has no notion of one), so Subject is still supplied
	// directly even when TemplateID is used — only Body comes from the
	// render.
	//
	// Exactly one of (Template), (TemplateID+Locale), (Subject/Body) may
	// be used per send — accepting more than one would leave it ambiguous
	// which content actually reached the recipient.
	Template   string            `json:"template,omitempty"`
	TemplateID string            `json:"template_id,omitempty"`
	Locale     string            `json:"locale,omitempty"`
	Variables  map[string]string `json:"variables,omitempty"`
}

// ListFilter carries every constraint on a register read, including the
// paging bounds. Grouped into one struct rather than a growing parameter list
// so a caller cannot transpose two adjacent string arguments — the filters are
// all strings, and the compiler would not notice.
type ListFilter struct {
	LegalEntityID        string
	RecipientPrincipalID string
	Status               string

	// UnreadOnly restricts the read to notifications the recipient has not
	// acknowledged — what an inbox shows when the user filters to unread.
	UnreadOnly bool

	Limit  int
	Offset int
}

// DeliveryOutcome is what one delivery attempt actually achieved.
//
// It replaced a (bool, string) pair. The pair could say "it worked" or "it did
// not and here is why", and could not say the two things that matter most
// here: what the provider gave back as evidence of acceptance, and whether a
// refusal is worth attempting again. Both were therefore unrecorded — a
// greylisted message and a nonexistent mailbox concluded identically.
type DeliveryOutcome struct {
	// Delivered means a provider accepted the message. It does NOT mean the
	// recipient received, read or was legally served with it — ZS-SVC-Y-001
	// §0.4 forbids conflating those, and the notification's SENT status
	// carries exactly this weaker claim.
	Delivered bool

	// ProviderResponse is the acceptance evidence: which provider took it and
	// under what identifier, so a message can be traced into provider logs or
	// matched against the headers the recipient received.
	ProviderResponse string

	// Reason explains a refusal, and is written to failure_reason.
	Reason string

	// Retryable distinguishes "the provider says never" (unknown mailbox,
	// malformed address) from "not now" (connection refused, greylisting, an
	// SMTP 4xx). Nothing re-attempts on it yet; recording it is what makes a
	// retry worker possible without re-litigating every historical failure.
	Retryable bool

	// Unknown marks an outcome that is neither a confirmed acceptance nor a
	// safely-retryable or terminal failure — the message may or may not
	// have reached the provider, and guessing either way risks a duplicate
	// send (guessing "not sent") or a silently dropped notice (guessing
	// "sent"). Invariant #25: "ambiguous external outcomes SHALL remain
	// Pending/Unknown rather than being guessed." Mutually exclusive with
	// Delivered and Retryable — a caller sets at most one of the three.
	Unknown bool

	// ProviderName records the name of the provider that actually handled or refused
	// the attempt (e.g. "smtp-primary", "smtp-secondary", "ses").
	ProviderName string
}

// AddressSource values for Notification.RecipientAddressSource.
const (
	AddressSourceIdentityContext = "IDENTITY_CONTEXT"
	AddressSourceRequest         = "REQUEST"
)

// Channels this service accepts. IN_APP is terminal inside the platform; the
// other three hand off to a provider outside it.
const (
	ChannelEmail = "EMAIL"
	// ChannelSMS is NOT accepted for new notifications — the handler's
	// supportedChannels omits it, so a send naming it is refused at the
	// request boundary. The constant remains because historical rows carry the
	// value and the delivery router still has to answer for them truthfully.
	ChannelSMS     = "SMS"
	ChannelInApp   = "IN_APP"
	ChannelWebhook = "WEBHOOK"
)

// ChannelNeedsAddress reports whether a channel delivers to an endpoint
// outside the platform and therefore needs a resolved recipient address.
//
// IN_APP does not: the row in this register IS the delivery, so there is
// nothing to resolve and nowhere to send. Asking identity-context-svc for an
// email address in order to write a database row would make every in-app
// notice depend on another service being reachable, for a value never used.
func ChannelNeedsAddress(channel string) bool {
	// EMAIL alone. SMS also needs an endpoint in principle, but it is no
	// longer accepted for new sends, and resolving an address for a legacy
	// SMS row would make a retry depend on identity-context-svc to compute a
	// value no provider will ever use.
	return channel == ChannelEmail
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrNotificationNotFound    = errorString("notification not found")
	ErrAuthorizationDenied     = errorString("authorization denied for notification action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("notification store unavailable")

	// ErrPrincipalNotFound / ErrPrincipalHasNoAddress are recipient-resolution
	// outcomes that are settled facts: asking again will return the same
	// answer, so the notification is recorded FAILED rather than left for a
	// retry that cannot succeed.
	ErrPrincipalNotFound     = errorString("recipient principal not found in identity-context-svc")
	ErrPrincipalHasNoAddress = errorString("recipient principal has no email address on record")

	// ErrIdentityServiceUnavailable is the opposite case — the answer is
	// unknown, not absent. Kept distinct so the failure_reason on the record
	// says which of the two happened, and so a retry worker can tell a
	// notification worth re-attempting from one that never will be.
	ErrIdentityServiceUnavailable = errorString("identity-context-svc unavailable")

	// ErrNotPendingUnknown is ResolveDeliveryOutcome's own guard — only a
	// notification actually in PENDING_UNKNOWN has an ambiguous attempt to
	// resolve.
	ErrNotPendingUnknown = errorString("notification is not in PENDING_UNKNOWN — there is no ambiguous outcome to resolve")

	// ErrInvalidResolvedStatus guards ResolveDeliveryOutcome's own target
	// status — an ambiguous attempt resolves to exactly the two conclusions
	// a real attempt could have reached, SENT or FAILED.
	ErrInvalidResolvedStatus = errorString("resolved_status must be SENT or FAILED")

	ErrResolutionNoteRequired = errorString("resolution_note is required")

	// ErrDeliveryOutcomeUnknown is the doc's own named stable error,
	// DELIVERY_OUTCOME_UNKNOWN: "Provider state ambiguous; original
	// notification attempt must be resolved." Surfaced on
	// GetDeliveryStatus for a PENDING_UNKNOWN notification — not a request
	// failure, a reportable code describing the notification's own state.
	ErrDeliveryOutcomeUnknown = errorString("provider state ambiguous; original notification attempt must be resolved")

	// ErrResendReasonRequired — §3.4: a user-initiated resend carries an
	// explicit reason.
	ErrResendReasonRequired = errorString("a resend requires a reason")

	// ErrNotResendable — only a SENT or FAILED notification can be resent. A
	// PENDING one is still being attempted; a PENDING_UNKNOWN one must be
	// resolved first (§6.2: UNKNOWN never authorizes a blind second send).
	ErrNotResendable = errorString("only a SENT or FAILED notification can be resent")
)

// ── Durable delivery attempts (ZS-SVC-Y-001 §3.4, §6.2; migration 000011) ──

// Attempt origins.
const (
	AttemptOriginRequest = "request" // the synchronous attempt inside POST /v1/notifications
	AttemptOriginRetry   = "retry"   // a later attempt by the retry worker
	AttemptOriginResend  = "resend"  // an explicit, reasoned resend (POST /{id}/resend)
)

// Attempt outcomes — what one attempt achieved, not the notification's status.
const (
	AttemptOutcomeAccepted = "ACCEPTED" // a provider accepted it (never "delivered", §3.3)
	AttemptOutcomeFailed   = "FAILED"   // refused, and no further attempt follows
	AttemptOutcomeRetrying = "RETRYING" // refused, another attempt is scheduled
	AttemptOutcomeUnknown  = "UNKNOWN"  // genuinely ambiguous; must be resolved, never blindly re-sent
)

// AttemptMeta is what the caller knows about an attempt that the notification
// row does not: where it was made, which provider handled it, and — for a
// resend — why. The transition that records the attempt's effect writes it,
// in the same transaction.
type AttemptMeta struct {
	Origin           string
	ProviderName     string
	Retryable        bool
	ResendReason     string
	ActorPrincipalID string
}

// DeliveryAttempt is one durable provider submission on the direct-send path.
type DeliveryAttempt struct {
	AttemptID        string    `json:"attempt_id"`
	TenantID         string    `json:"tenant_id"`
	NotificationID   string    `json:"notification_id"`
	AttemptNumber    int       `json:"attempt_number"`
	Origin           string    `json:"origin"`
	Channel          string    `json:"channel"`
	ProviderName     string    `json:"provider_name,omitempty"`
	Outcome          string    `json:"outcome"`
	ProviderResponse string    `json:"provider_response,omitempty"`
	FailureReason    string    `json:"failure_reason,omitempty"`
	Retryable        bool      `json:"retryable"`
	ResendReason     string    `json:"resend_reason,omitempty"`
	ActorPrincipalID string    `json:"actor_principal_id,omitempty"`
	AttemptedAt      time.Time `json:"attempted_at"`
	RecordedAt       time.Time `json:"recorded_at"`
}
