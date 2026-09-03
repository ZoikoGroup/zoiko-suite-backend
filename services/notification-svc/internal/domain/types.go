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
}

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
}

// AddressSource values for Notification.RecipientAddressSource.
const (
	AddressSourceIdentityContext = "IDENTITY_CONTEXT"
	AddressSourceRequest         = "REQUEST"
)

// Channels this service accepts. IN_APP is terminal inside the platform; the
// other three hand off to a provider outside it.
const (
	ChannelEmail   = "EMAIL"
	// ChannelSMS is NOT accepted for new notifications — the handler's
	// supportedChannels omits it, so a send naming it is refused at the
	// request boundary. The constant remains because historical rows carry the
	// value and the delivery router still has to answer for them truthfully.
	ChannelSMS = "SMS"
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
)
