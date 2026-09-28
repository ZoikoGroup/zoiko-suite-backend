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
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Status  string `json:"status"` // PENDING, UNKNOWN, PROVIDER_ACCEPTED, DELIVERED, READ, SERVED, FAILED

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

	// IdempotencyKey is the purpose-scoped key derived from the originating
	// business event. Two different communications sharing a correlation ID
	// but with different purposes MUST NOT collide — §3.4.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// PurposeContext records the purpose the idempotency key was scoped to.
	// A caller must supply it on send; the handler refuses a send where the
	// same key exists for a different purpose. This is the §3.4 fix for
	// "two different communications sharing a correlation collide".
	PurposeContext string `json:"purpose_context,omitempty"`
}

// Retrying reports whether delivery has not concluded and another attempt is
// scheduled. Kept as a method so the console and the service agree on what the
// combination means rather than each re-deriving it.
func (n Notification) Retrying() bool {
	return n.Status == "PENDING" && n.NextAttemptAt != nil
}

// InFlight reports whether an attempt has been made but not yet concluded.
// This is the "UNKNOWN" state — the provider was called but we have not yet
// received confirmation of acceptance or final refusal.
func (n Notification) InFlight() bool {
	return n.Status == "PENDING" && n.NextAttemptAt == nil
}

// Concluded reports whether the notification has reached a terminal state.
func (n Notification) Concluded() bool {
	switch n.Status {
	case "PROVIDER_ACCEPTED", "DELIVERED", "READ", "SERVED", "FAILED":
		return true
	}
	return false
}

// InFlightDuration returns how long the notification has been in flight.
// Returns 0 if not in flight.
func (n Notification) InFlightDuration() time.Duration {
	if !n.InFlight() {
		return 0
	}
	from := n.LastAttemptAt
	if from == nil {
		from = &n.CreatedAt
	}
	return time.Since(*from)
}

// Status constants per ZS-SVC-Y-001 §3.3 — five distinct claims, never one
// generic "sent=true". The service never exposes SENT as a single status.
//
//   PENDING         → delivery not yet concluded (may be in flight or scheduled)
//   UNKNOWN         → provider was called but response not yet known (timeout)
//   PROVIDER_ACCEPTED → provider accepted the message (SMTP 250, queue id)
//   DELIVERED       → provider confirmed delivery to mailbox (DSN success)
//   READ            → recipient opened the message (in-app only, or read receipt)
//   SERVED          → legal service confirmed (acknowledgement chain complete)
//   FAILED          → terminal failure, no more attempts will be made
const (
	StatusPending          = "PENDING"
	StatusUnknown          = "UNKNOWN"
	StatusProviderAccepted = "PROVIDER_ACCEPTED"
	StatusDelivered        = "DELIVERED"
	StatusRead             = "READ"
	StatusServed           = "SERVED"
	StatusFailed           = "FAILED"
)

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

	// PurposeContext is REQUIRED. It scopes the idempotency key to the
	// business purpose of the communication. Two different communications
	// sharing a correlation_id but with different purposes MUST NOT collide
	// — ZS-SVC-Y-001 §3.4. A send missing this field is refused.
	PurposeContext string `json:"purpose_context"`

	// IdempotencyKey is the purpose-scoped key the caller derives from the
	// originating business event. If omitted, the handler derives one from
	// (tenant_id, legal_entity_id, correlation_id, purpose_context).
	//
	// A replay with the same idempotency_key returns the original notification
	// with its outcome, rather than sending a second time.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

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
	// The two forms are mutually exclusive: accepting both would leave it
	// ambiguous which one actually reached the recipient.
	Template  string            `json:"template,omitempty"`
	Variables map[string]string `json:"variables,omitempty"`
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

 	// Err preserves the original error for classification (e.g., timeout
 	// detection per §3.4). Not serialized — only used in-process.
 	Err error
 }

// DeliveryAttempt is one provider submission with its outcome and evidence.
// This replaces the simple counter (delivery_attempts) with a durable,
// auditable chain — ZS-SVC-Y-001 §3.4 requires "durable attempt_id per
// provider submission" and "resend carries an explicit reason and preserves
// the evidence chain".
type DeliveryAttempt struct {
	AttemptID        string     `json:"attempt_id"`
	NotificationID   string     `json:"notification_id"`
	AttemptNumber    int        `json:"attempt_number"`
	Channel          string     `json:"channel"`
	Provider         string     `json:"provider"`
	Status           string     `json:"status"` // PROVIDER_ACCEPTED, DELIVERED, READ, SERVED, FAILED, UNKNOWN
	ProviderResponse string     `json:"provider_response,omitempty"`
	FailureReason    string     `json:"failure_reason,omitempty"`
	Retryable        bool       `json:"retryable"`
	ResendReason     string     `json:"resend_reason,omitempty"` // Why this attempt was made (first_try, retry, reconcile, manual)
	CreatedAt        time.Time  `json:"created_at"`
	ConcludedAt      *time.Time `json:"concluded_at,omitempty"`
}

// AttemptStatus constants
const (
	AttemptStatusUnknown          = "UNKNOWN"
	AttemptStatusProviderAccepted = "PROVIDER_ACCEPTED"
	AttemptStatusDelivered        = "DELIVERED"
	AttemptStatusRead             = "READ"
	AttemptStatusServed           = "SERVED"
	AttemptStatusFailed           = "FAILED"
)

// ResendReason constants — why an attempt was made
const (
	ResendReasonFirstTry  = "first_try"
	ResendReasonRetry     = "retry"
	ResendReasonReconcile = "reconcile"
	ResendReasonManual    = "manual"
)

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

	// Suppression/Preference errors
	ErrSuppressionNotFound   = errorString("suppression not found")
	ErrPreferenceNotFound    = errorString("preference not found")
	ErrChannelDecisionFailed = errorString("channel decision evaluation failed")
)

// ── NCD-02: Suppression ───────────────────────────────────────────────────────

// SuppressionReason enumerates why a channel is suppressed for a recipient.
type SuppressionReason string

const (
	SuppressionReasonUnsubscribe   SuppressionReason = "unsubscribe"
	SuppressionReasonBounce        SuppressionReason = "bounce"
	SuppressionReasonComplaint     SuppressionReason = "complaint"
	SuppressionReasonManual        SuppressionReason = "manual"
	SuppressionReasonLegalHold     SuppressionReason = "legal_hold"
)

// Suppression is a hard stop: the channel is blocked regardless of preference
// or permission. Per §5.4, suppression has highest precedence.
type Suppression struct {
	SuppressionID string             `json:"suppression_id"`
	TenantID      string             `json:"tenant_id"`
	PrincipalID   string             `json:"principal_id"`
	Channel       string             `json:"channel"` // EMAIL, SMS, IN_APP, WEBHOOK
	Reason        SuppressionReason  `json:"reason"`
	CreatedBy     string             `json:"created_by"` // principal who created it
	CreatedAt     time.Time          `json:"created_at"`
	ExpiresAt     *time.Time         `json:"expires_at,omitempty"` // optional: temporary suppression
}

// SuppressionFilter for listing suppressions.
type SuppressionFilter struct {
	TenantID     string
	PrincipalID  string
	Channel      string
	Reason       string
	ActiveOnly   bool
	Limit        int
	Offset       int
}

// ── NCD-02: Preference ────────────────────────────────────────────────────────

// Preference is what the recipient wants (not a hard block like suppression).
// Per §5.3, preference is distinct from permission (legal basis / consent).
type Preference struct {
	PreferenceID     string     `json:"preference_id"`
	TenantID         string     `json:"tenant_id"`
	PrincipalID      string     `json:"principal_id"`
	Channel          string     `json:"channel"` // EMAIL, SMS, IN_APP, WEBHOOK
	Enabled          bool       `json:"enabled"`
	QuietHoursStart  *string    `json:"quiet_hours_start,omitempty"`  // HH:MM:SS format
	QuietHoursEnd    *string    `json:"quiet_hours_end,omitempty"`    // HH:MM:SS format
	Timezone         string     `json:"timezone,omitempty"`           // IANA timezone
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// PreferenceFilter for listing preferences.
type PreferenceFilter struct {
	TenantID    string
	PrincipalID string
	Channel     string
	EnabledOnly bool
	Limit       int
	Offset      int
}

// ── NCD-02: Channel Decision ──────────────────────────────────────────────────

// ChannelDecision records the outcome of a channel evaluation for audit.
type ChannelDecision struct {
	DecisionID     string     `json:"decision_id"`
	TenantID       string     `json:"tenant_id"`
	PrincipalID    string     `json:"principal_id"`
	Channel        string     `json:"channel"`
	Decision       string     `json:"decision"` // allowed, suppressed, quiet_hours, no_preference, no_permission
	SuppressionID  *string    `json:"suppression_id,omitempty"`
	PreferenceID   *string    `json:"preference_id,omitempty"`
	PermissionGrant string    `json:"permission_grant,omitempty"` // legal basis reference
	EvaluatedAt    time.Time  `json:"evaluated_at"`
}

// ChannelDecisionFilter for listing decisions.
type ChannelDecisionFilter struct {
	TenantID    string
	PrincipalID string
	Channel     string
	Decision    string
	Limit       int
	Offset      int
}

// ChannelDecisionResult is the outcome of evaluating a channel for a recipient.
type ChannelDecisionResult struct {
	Allowed          bool      `json:"allowed"`
	Decision         string    `json:"decision"` // allowed, suppressed, quiet_hours, no_preference, no_permission
	SuppressionID    *string   `json:"suppression_id,omitempty"`
	PreferenceID     *string   `json:"preference_id,omitempty"`
	PermissionGrant  *string   `json:"permission_grant,omitempty"`
}

// SuppressionReason values
const (
	ChannelDecisionAllowed         = "allowed"
	ChannelDecisionSuppressed      = "suppressed"
	ChannelDecisionQuietHours      = "quiet_hours"
	ChannelDecisionNoPreference    = "no_preference"
	ChannelDecisionNoPermission    = "no_permission"
)

// ── NCD-04: Bounce ────────────────────────────────────────────────────────────

type BounceType string

const (
	BounceTypeHard      BounceType = "hard"
	BounceTypeSoft      BounceType = "soft"
	BounceTypeTransient BounceType = "transient"
)

type BounceAction string

const (
	BounceActionSuppressionCreated BounceAction = "suppression_created"
	BounceActionRetryScheduled     BounceAction = "retry_scheduled"
	BounceActionIgnored            BounceAction = "ignored"
)

type BounceEvent struct {
	BounceID         string       `json:"bounce_id"`
	TenantID         string       `json:"tenant_id"`
	NotificationID   string       `json:"notification_id"`
	Provider         string       `json:"provider"`
	BounceType       BounceType   `json:"bounce_type"`
	BounceSubtype    string       `json:"bounce_subtype,omitempty"`
	DiagnosticCode   string       `json:"diagnostic_code,omitempty"`
	RecipientAddress string       `json:"recipient_address"`
	ReceivedAt       time.Time    `json:"received_at"`
	ProcessedAt      *time.Time   `json:"processed_at,omitempty"`
	ActionTaken      BounceAction `json:"action_taken,omitempty"`
	SuppressionID    *string      `json:"suppression_id,omitempty"`
}

type BounceEventFilter struct {
	TenantID       string
	NotificationID string
	RecipientAddr  string
	BounceType     string
	Limit          int
	Offset         int
}

// ── NCD-04: Complaint ─────────────────────────────────────────────────────────

type ComplaintAction string

const (
	ComplaintActionSuppressionCreated ComplaintAction = "suppression_created"
	ComplaintActionIgnored            ComplaintAction = "ignored"
)

type ComplaintEvent struct {
	ComplaintID      string           `json:"complaint_id"`
	TenantID         string           `json:"tenant_id"`
	NotificationID   *string          `json:"notification_id,omitempty"`
	Provider         string           `json:"provider"`
	ComplaintType    string           `json:"complaint_type"` // abuse, spam, unsubscribe_request
	RecipientAddress string           `json:"recipient_address"`
	ReceivedAt       time.Time        `json:"received_at"`
	ProcessedAt      *time.Time       `json:"processed_at,omitempty"`
	ActionTaken      ComplaintAction  `json:"action_taken,omitempty"`
	SuppressionID    *string          `json:"suppression_id,omitempty"`
}

type ComplaintEventFilter struct {
	TenantID       string
	NotificationID string
	RecipientAddr  string
	ComplaintType  string
	Limit          int
	Offset         int
}

// ── NCD-04: Channel Reputation ────────────────────────────────────────────────

type ChannelReputation struct {
	ReputationID    string     `json:"reputation_id"`
	TenantID        string     `json:"tenant_id"`
	Channel         string     `json:"channel"`
	Provider        string     `json:"provider"`
	WindowStart     time.Time  `json:"window_start"`
	WindowEnd       time.Time  `json:"window_end"`
	SentCount       int64      `json:"sent_count"`
	AcceptedCount   int64      `json:"accepted_count"`
	BouncedCount    int64      `json:"bounced_count"`
	ComplainedCount int64      `json:"complained_count"`
	DeliveredCount  int64      `json:"delivered_count"`
	ReadCount       int64      `json:"read_count"`
	ReputationScore *float64   `json:"reputation_score,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type ChannelReputationFilter struct {
	TenantID  string
	Channel   string
	Provider  string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
}

// Errors for NCD-04
var (
	ErrBounceNotFound     = errorString("bounce event not found")
	ErrComplaintNotFound  = errorString("complaint event not found")
	ErrReputationNotFound = errorString("channel reputation not found")
)

// ── NCD-01: Communication Intent ──────────────────────────────────────────────

type IntentCategory string

const (
	IntentCategoryTransactional IntentCategory = "transactional"
	IntentCategoryMarketing     IntentCategory = "marketing"
	IntentCategoryRegulatory    IntentCategory = "regulatory"
	IntentCategorySecurity      IntentCategory = "security"
)

type CommunicationIntent struct {
	IntentID       string         `json:"intent_id"`
	TenantID       string         `json:"tenant_id"`
	LegalEntityID  string         `json:"legal_entity_id"`
	Name           string         `json:"name"`
	Description    string         `json:"description,omitempty"`
	Category       IntentCategory `json:"category"`
	Channels       string         `json:"channels"` // comma-separated
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type CommunicationIntentFilter struct {
	TenantID      string
	LegalEntityID string
	Name          string
	Category      string
	Limit         int
	Offset        int
}

// ── NCD-01: Template ──────────────────────────────────────────────────────────

type TemplateStatus string

const (
	TemplateStatusDraft           TemplateStatus = "draft"
	TemplateStatusPendingApproval TemplateStatus = "pending_approval"
	TemplateStatusApproved        TemplateStatus = "approved"
	TemplateStatusPublished       TemplateStatus = "published"
	TemplateStatusArchived        TemplateStatus = "archived"
)

type Template struct {
	TemplateID      string         `json:"template_id"`
	IntentID        string         `json:"intent_id"`
	TenantID        string         `json:"tenant_id"`
	LegalEntityID   string         `json:"legal_entity_id"`
	Locale          string         `json:"locale"`
	Version         int            `json:"version"`
	SubjectTemplate string         `json:"subject_template"`
	BodyTemplate    string         `json:"body_template"`
	Variables       []string       `json:"variables"` // required variable names
	Status          TemplateStatus `json:"status"`
	ApprovedBy      string         `json:"approved_by,omitempty"`
	ApprovedAt      *time.Time     `json:"approved_at,omitempty"`
	PublishedAt     *time.Time     `json:"published_at,omitempty"`
	EffectiveFrom   *time.Time     `json:"effective_from,omitempty"`
	EffectiveTo     *time.Time     `json:"effective_to,omitempty"`
	CreatedBy       string         `json:"created_by"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

type TemplateFilter struct {
	TenantID      string
	LegalEntityID string
	IntentID      string
	Locale        string
	Status        string
	Limit         int
	Offset        int
}

// TemplateApproval for SoD workflow
type TemplateApproval struct {
	ApprovalID   string     `json:"approval_id"`
	TemplateID   string     `json:"template_id"`
	TenantID     string     `json:"tenant_id"`
	RequestedBy  string     `json:"requested_by"`
	ApprovedBy   string     `json:"approved_by,omitempty"`
	Status       string     `json:"status"` // pending, approved, rejected
	Reason       string     `json:"reason,omitempty"`
	RequestedAt  time.Time  `json:"requested_at"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
}

type TemplateApprovalFilter struct {
	TenantID   string
	TemplateID string
	Status     string
	Limit      int
	Offset     int
}

// TemplateRender for preview
type TemplateRender struct {
	RenderID       string     `json:"render_id"`
	TemplateID     string     `json:"template_id"`
	TenantID       string     `json:"tenant_id"`
	Variables      map[string]string `json:"variables"`
	RenderedSubject string    `json:"rendered_subject,omitempty"`
	RenderedBody   string     `json:"rendered_body,omitempty"`
	Error          string     `json:"error,omitempty"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
}

// TemplatePreviewRequest for render-previews endpoint
type TemplatePreviewRequest struct {
	TemplateID string            `json:"template_id,omitempty"`
	IntentID   string            `json:"intent_id,omitempty"`
	Locale     string            `json:"locale,omitempty"`
	Version    int               `json:"version,omitempty"`
	Variables  map[string]string `json:"variables"`
}

// Errors for NCD-01
var (
	ErrIntentNotFound      = errorString("communication intent not found")
	ErrTemplateNotFound    = errorString("template not found")
	ErrApprovalNotFound    = errorString("template approval not found")
	ErrSelfApproval        = errorString("creator cannot approve their own template")
	ErrInvalidTemplateVars = errorString("template variables validation failed")
)

// ── NCD-05: Regulated Notice ──────────────────────────────────────────────────

type RegulatedNoticeStatus string

const (
	RegulatedNoticeStatusPending       RegulatedNoticeStatus = "pending"
	RegulatedNoticeStatusSent          RegulatedNoticeStatus = "sent"
	RegulatedNoticeStatusAcknowledged  RegulatedNoticeStatus = "acknowledged"
	RegulatedNoticeStatusExpired       RegulatedNoticeStatus = "expired"
	RegulatedNoticeStatusFailed        RegulatedNoticeStatus = "failed"
)

type RegulatedNoticePriority string

const (
	RegulatedNoticePriorityNormal  RegulatedNoticePriority = "normal"
	RegulatedNoticePriorityHigh    RegulatedNoticePriority = "high"
	RegulatedNoticePriorityCritical RegulatedNoticePriority = "critical"
)

type AcknowledgmentMethod string

const (
	AcknowledgmentMethodClick            AcknowledgmentMethod = "click"
	AcknowledgmentMethodDigitalSignature AcknowledgmentMethod = "digital_signature"
	AcknowledgmentMethodWitness          AcknowledgmentMethod = "witness"
	AcknowledgmentMethodAPI              AcknowledgmentMethod = "api"
)

type RegulatedNotice struct {
	RegulatedNoticeID    string                 `json:"regulated_notice_id"`
	TenantID             string                 `json:"tenant_id"`
	LegalEntityID        string                 `json:"legal_entity_id"`
	IntentID             string                 `json:"intent_id,omitempty"`
	TemplateID           string                 `json:"template_id,omitempty"`
	RecipientPrincipalID string                 `json:"recipient_principal_id"`
	RecipientAddress     string                 `json:"recipient_address,omitempty"`
	Subject              string                 `json:"subject"`
	Body                 string                 `json:"body"`
	Variables            map[string]string      `json:"variables"`
	Channel              string                 `json:"channel"`
	Status               RegulatedNoticeStatus  `json:"status"`
	Priority             RegulatedNoticePriority `json:"priority"`
	ExpiresAt            *time.Time             `json:"expires_at,omitempty"`
	AcknowledgedAt       *time.Time             `json:"acknowledged_at,omitempty"`
	AcknowledgedBy       string                 `json:"acknowledged_by,omitempty"`
	AcknowledgmentMethod AcknowledgmentMethod   `json:"acknowledgment_method,omitempty"`
	AcknowledgmentChain  map[string]any         `json:"acknowledgment_chain,omitempty"`
	CreatedBy            string                 `json:"created_by"`
	CreatedAt            time.Time              `json:"created_at"`
	UpdatedAt            time.Time              `json:"updated_at"`
}

type RegulatedNoticeFilter struct {
	TenantID             string
	LegalEntityID        string
	RecipientPrincipalID string
	Status               string
	Priority             string
	Limit                int
	Offset               int
}

// AcknowledgmentChainStep records one step in the acknowledgment chain.
type AcknowledgmentChainStep struct {
	ChainID             string         `json:"chain_id"`
	RegulatedNoticeID   string         `json:"regulated_notice_id"`
	TenantID            string         `json:"tenant_id"`
	StepNumber          int            `json:"step_number"`
	Action              string         `json:"action"` // created, sent, delivered, opened, acknowledged, expired, failed
	Actor               string         `json:"actor,omitempty"`
	Method              string         `json:"method,omitempty"`
	Evidence            map[string]any `json:"evidence,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
}

type AcknowledgmentChainFilter struct {
	TenantID            string
	RegulatedNoticeID   string
	Action              string
	Limit               int
	Offset              int
}

// AcknowledgeRequest for POST /acknowledgements
type AcknowledgeRequest struct {
	RegulatedNoticeID   string               `json:"regulated_notice_id"`
	Method              AcknowledgmentMethod `json:"method"`
	Evidence            map[string]any       `json:"evidence,omitempty"`
	WitnessPrincipalID  string               `json:"witness_principal_id,omitempty"`
	DigitalSignature    string               `json:"digital_signature,omitempty"`
}

// Errors for NCD-05
var (
	ErrRegulatedNoticeNotFound = errorString("regulated notice not found")
	ErrChainStepNotFound       = errorString("acknowledgment chain step not found")
	ErrAlreadyAcknowledged     = errorString("notice already acknowledged")
	ErrNoticeExpired           = errorString("notice has expired")
)
