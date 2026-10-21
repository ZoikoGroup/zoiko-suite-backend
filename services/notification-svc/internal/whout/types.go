// Package whout is INT-04, tenant-configured OUTBOUND webhooks (ZS-SVC-M-001 §4.4):
// deliver approved events to approved destinations, with minimized payloads,
// cryptographic authenticity and bounded retry.
//
// It is not the inbound provider-receipt handling in internal/webhook, which receives
// events a provider sends about a notification. Here the service is the sender.
//
// What it owns: WebhookSubscription, its event/field filter, delivery attempts,
// destination response evidence and terminal delivery status. What it must not own:
// the recipient's business state (a 2xx means the HTTP exchange completed, nothing
// more), the recipient's credentials, source-domain facts, or the signing key, which
// stays in secret-vault-integration-svc and is only ever used there.
package whout

import "time"

// Status is a subscription's lifecycle state.
type Status string

const (
	StatusDraft           Status = "DRAFT"
	StatusPendingApproval Status = "PENDING_APPROVAL"
	StatusActive          Status = "ACTIVE"
	StatusSuspended       Status = "SUSPENDED" // an operator paused it; approval is intact
	StatusDisabled        Status = "DISABLED"  // the platform stopped it after repeated failure
	StatusRevoked         Status = "REVOKED"   // terminal
)

// Live reports whether the status is one that holds a valid approval.
func (s Status) Live() bool {
	return s == StatusActive || s == StatusSuspended || s == StatusDisabled
}

// Subscription is a tenant's outbound webhook configuration.
type Subscription struct {
	SubscriptionID         string
	TenantID               string
	LegalEntityID          string
	Name                   string
	DestinationURL         string
	EventTypes             []string
	AllowedFields          []string
	APIVersion             string
	SigningKeyPath         string
	PreviousSigningKeyPath string
	PreviousKeyValidUntil  *time.Time
	DestinationRegion      string
	MaxAttempts            int
	Status                 Status
	StatusReason           string
	Version                int
	ConfigFingerprint      string
	ApprovedFingerprint    string
	CreatedBy              string
	LastChangedBy          string
	ApprovedBy             string
	ApprovedAt             *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time

	ConsecutiveFailures    int
	ConsecutiveDeadLetters int
	BreakerOpenUntil       *time.Time
	LastSuccessAt          *time.Time
	LastFailureAt          *time.Time
}

// DeliveryStatus is a delivery's state.
type DeliveryStatus string

const (
	DeliveryPending      DeliveryStatus = "PENDING"
	DeliverySent         DeliveryStatus = "SENT" // an attempt is in flight
	DeliverySucceeded    DeliveryStatus = "SUCCEEDED"
	DeliveryRetrying     DeliveryStatus = "RETRYING"
	DeliveryDeadLettered DeliveryStatus = "DEAD_LETTERED"
)

// Dead-letter reasons.
const (
	ReasonRetriesExhausted    = "RETRIES_EXHAUSTED"
	ReasonDestinationGone     = "DESTINATION_GONE"
	ReasonMinimizationBlock   = "MINIMIZATION_BLOCKED"
	ReasonSubscriptionEnded   = "SUBSCRIPTION_REVOKED"
	ReasonEgressDenied        = "EGRESS_DENIED"
	ReasonSubscriptionNotLive = "SUBSCRIPTION_NOT_LIVE"
)

// Delivery is one event owed to one subscription.
type Delivery struct {
	DeliveryID       string
	TenantID         string
	SubscriptionID   string
	EventID          string
	EventType        string
	EventOccurredAt  time.Time
	Body             []byte
	BodyHash         string
	ScopeFingerprint string
	Status           DeliveryStatus
	AttemptCount     int
	NextAttemptAt    time.Time
	LockedUntil      *time.Time
	LastAttemptAt    *time.Time
	LastResponseCode *int
	LastErrorClass   string
	DeadLetterReason string
	DeadLetteredAt   *time.Time
	SucceededAt      *time.Time
	ReplayCount      int
	BudgetBase       int
	CorrelationID    string
	CreatedAt        time.Time
}

// AttemptOutcome is how one attempt ended.
type AttemptOutcome string

const (
	OutcomeInFlight      AttemptOutcome = "IN_FLIGHT"
	OutcomeSucceeded     AttemptOutcome = "SUCCEEDED"
	OutcomeHTTPError     AttemptOutcome = "HTTP_ERROR"
	OutcomeRateLimited   AttemptOutcome = "RATE_LIMITED"
	OutcomeTimeout       AttemptOutcome = "TIMEOUT"
	OutcomeTLSFailure    AttemptOutcome = "TLS_FAILURE"
	OutcomeNetworkError  AttemptOutcome = "NETWORK_ERROR"
	OutcomeEgressBlocked AttemptOutcome = "EGRESS_BLOCKED"
	OutcomeSigningFailed AttemptOutcome = "SIGNING_FAILED"
	OutcomeBlocked       AttemptOutcome = "BLOCKED"
	OutcomeUnknown       AttemptOutcome = "UNKNOWN"
)

// Attempt is the evidence of one delivery attempt (INT-04 evidence/lineage): the
// minimized payload hash, signature metadata, destination host, timestamps and the
// response code and a hash of the response body. The response body itself is not kept.
type Attempt struct {
	AttemptID          int64
	DeliveryID         string
	SubscriptionID     string
	AttemptNo          int
	StartedAt          time.Time
	FinishedAt         *time.Time
	Outcome            AttemptOutcome
	ResponseCode       *int
	ResponseBodyHash   string
	ResponseBodyBytes  *int
	ErrorClass         string
	DestinationHost    string
	PayloadHash        string
	SignatureProfile   string
	SignatureTimestamp *int64
	SignatureKeyPaths  []string
}

// LogKind names an entry in a subscription's append-only log.
type LogKind string

const (
	LogCreated         LogKind = "CREATED"
	LogUpdated         LogKind = "UPDATED"
	LogSubmitted       LogKind = "SUBMITTED"
	LogApproved        LogKind = "APPROVED"
	LogRejected        LogKind = "REJECTED"
	LogSuspended       LogKind = "SUSPENDED"
	LogResumed         LogKind = "RESUMED"
	LogDisabled        LogKind = "DISABLED"
	LogRevoked         LogKind = "REVOKED"
	LogKeyRotated      LogKind = "KEY_ROTATED"
	LogReplayRequested LogKind = "REPLAY_REQUESTED"
	LogGraceKeyRetired LogKind = "GRACE_KEY_RETIRED"
)

// LogEntry is one fact about a subscription.
type LogEntry struct {
	LogID          int64
	TenantID       string
	SubscriptionID string
	Kind           LogKind
	Actor          string
	Approver       string
	ApprovalRef    string
	FromStatus     Status
	ToStatus       Status
	Version        int
	Fingerprint    string
	DeliveryID     string
	Reason         string
	CorrelationID  string
	CreatedAt      time.Time
}

// Actor identifies who is acting, for authorship and separation of duties.
type Actor struct {
	TenantID      string
	PrincipalID   string
	CorrelationID string
}

// Health is a destination's current delivery health.
type Health struct {
	SubscriptionID         string
	Status                 Status
	BreakerOpen            bool
	BreakerOpenUntil       *time.Time
	ConsecutiveFailures    int
	ConsecutiveDeadLetters int
	LastSuccessAt          *time.Time
	LastFailureAt          *time.Time
	Pending                int
	Retrying               int
	DeadLettered           int
	Succeeded              int
}
