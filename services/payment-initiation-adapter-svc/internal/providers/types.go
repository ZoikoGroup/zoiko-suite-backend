// Package providers is the live bank/PSP adapter of BNK-06 (ZS-SVC-M-001 INT-07).
//
// It drives a certified provider profile (eventing/adapter) through a connector that INT-05 approved and a
// credential only the vault can release, around a durable attempt row. The rules it enforces:
//
//   - nothing leaves before the attempt is durably SUBMITTING (compare-and-set), and the profile version, rail,
//     connector and payload hash are pinned at that moment so a retry is exactly the original;
//   - an outcome where the request MAY have reached the bank (timeout, reset, 5xx, unreadable answer) is
//     PENDING_UNKNOWN, and a retry first ASKS the provider what it has before it ever sends again;
//   - a provider acknowledgement is SUBMITTED, which is not payment finality (payment-status-svc owns that);
//   - a rate limit is "not processed": the attempt goes back where it was with a not-before time.
package providers

import (
	"context"
	"errors"
	"time"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/adapter/ext"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
)

// PlatformScopeID scopes platform reference data (provider profiles), as in the other INT services.
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// Profile statuses.
const (
	ProfileDraft      = "DRAFT"
	ProfileCertified  = "CERTIFIED"
	ProfileDeprecated = "DEPRECATED"
	ProfileWithdrawn  = "WITHDRAWN"
)

// ProfileRecord is a stored provider profile.
type ProfileRecord struct {
	ProviderKey string       `json:"provider_key"`
	Version     int          `json:"version"`
	Kind        string       `json:"kind"`
	Status      string       `json:"status"`
	Spec        adapter.Spec `json:"spec"`
	SpecHash    string       `json:"spec_hash"`
	CreatedBy   string       `json:"created_by"`
	CertifiedBy string       `json:"certified_by,omitempty"`
	CertifiedAt *time.Time   `json:"certified_at,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}

// Dispatch is an attempt together with its provider context, read exactly (the amount as text, never a float).
type Dispatch struct {
	AttemptID         string               `json:"attempt_id"`
	TenantID          string               `json:"tenant_id"`
	LegalEntityID     string               `json:"legal_entity_id"`
	Status            domain.AttemptStatus `json:"status"`
	SourceReference   string               `json:"source_reference"`
	IdempotencyKey    string               `json:"idempotency_key"`
	AmountText        string               `json:"amount"`
	Currency          string               `json:"currency"`
	PayerAccountRef   string               `json:"payer_account_ref"`
	PayeeRef          string               `json:"payee_ref"`
	PaymentReference  string               `json:"payment_reference"`
	ExecutionDate     time.Time            `json:"execution_date"`
	ProviderKey       string               `json:"provider_key"`
	ProfileVersion    *int                 `json:"profile_version,omitempty"`
	Rail              string               `json:"rail"`
	ConnectorID       string               `json:"connector_id"`
	SentPayloadHash   string               `json:"sent_payload_hash,omitempty"`
	SendCount         int                  `json:"send_count"`
	SentAt            *time.Time           `json:"sent_at,omitempty"`
	InFlightSince     *time.Time           `json:"in_flight_since,omitempty"`
	NotBefore         *time.Time           `json:"not_before,omitempty"`
	ProviderRequestID string               `json:"provider_request_id,omitempty"`
	ProviderStatus    string               `json:"provider_status,omitempty"`
	LastFailure       string               `json:"last_failure,omitempty"`
}

// OutcomeKind is what to record after a provider call.
type OutcomeKind string

const (
	OutSubmitted OutcomeKind = "SUBMITTED"       // the provider took it
	OutRejected  OutcomeKind = "REJECTED"        // the provider refused it
	OutUnknown   OutcomeKind = "PENDING_UNKNOWN" // it may have reached the provider
	OutRelease   OutcomeKind = "RELEASE"         // provably not processed: back to where it was
	OutNone      OutcomeKind = "NONE"            // evidence only (a query that settled nothing)
)

// SendOutcome is the state change and evidence of one provider call, applied atomically.
type SendOutcome struct {
	Kind        OutcomeKind
	From        domain.AttemptStatus // the status the attempt must be in for this to apply
	ReleaseTo   domain.AttemptStatus // OutRelease only
	ProviderRef string
	Provider    string // the provider's own status word
	Reason      string
	NotBefore   *time.Time
	Failure     string
	Transport   TransportEntry
}

// TransportEntry is the evidence of one call: hashes and classes, never bodies or credentials.
type TransportEntry struct {
	LogID          string    `json:"log_id,omitempty"`
	AttemptID      string    `json:"attempt_id,omitempty"`
	ProviderKey    string    `json:"provider_key"`
	ProfileVersion int       `json:"profile_version"`
	Operation      string    `json:"operation"` // SUBMIT | QUERY
	Disposition    string    `json:"disposition"`
	Class          string    `json:"class"`
	HTTPStatus     int       `json:"http_status"`
	RequestHash    string    `json:"request_hash,omitempty"`
	ResponseHash   string    `json:"response_hash,omitempty"`
	ProviderRef    string    `json:"provider_ref,omitempty"`
	ErrorCode      string    `json:"error_code,omitempty"`
	DurationMS     int       `json:"duration_ms"`
	RetryAfterS    int       `json:"retry_after_s,omitempty"`
	Actor          string    `json:"actor,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

// Store is what the engine needs. The Postgres store implements it.
type Store interface {
	// profiles (platform reference data)
	PutProfile(ctx context.Context, key string, spec adapter.Spec, createdBy string) (*ProfileRecord, error)
	GetProfile(ctx context.Context, key string, version int) (*ProfileRecord, error) // version 0: latest CERTIFIED
	CertifyProfile(ctx context.Context, key string, version int, by string) (*ProfileRecord, error)
	SetProfileStatus(ctx context.Context, key string, version int, to string) (*ProfileRecord, error)
	ListProfiles(ctx context.Context, key string) ([]ProfileRecord, error)

	// dispatch
	LoadDispatch(ctx context.Context, attemptID string) (*Dispatch, error)
	BeginSend(ctx context.Context, attemptID string, from domain.AttemptStatus, profileVersion int, payloadHash, actor string) (*Dispatch, error)
	FinishSend(ctx context.Context, attemptID string, o SendOutcome, actor, correlationID string) (*domain.PaymentInitiationAttempt, error)
	RejectUnsent(ctx context.Context, attemptID, reason, actor, correlationID string) (*domain.PaymentInitiationAttempt, error)
	ListTransport(ctx context.Context, attemptID string) ([]TransportEntry, error)
	RecentCalls(ctx context.Context, providerKey string, since time.Time, limit int) ([]bool, error) // true = healthy
	SweepStale(ctx context.Context, staleBefore time.Time, actor string) (int, error)
}

// Engine errors. Each maps to one stable HTTP response; none carries provider or credential content.
var (
	ErrProfileNotFound    = errors.New("no certified provider profile")
	ErrNotConfigured      = errors.New("the attempt is not bound to a provider")
	ErrConnectorRefused   = ext.ErrConnectorRefused
	ErrConnectorDown      = ext.ErrConnectorDown
	ErrCredentialRefused  = ext.ErrCredentialRefused
	ErrCredentialDown     = ext.ErrCredentialDown
	ErrCircuitOpen        = errors.New("the provider is failing: sending is paused")
	ErrNotBefore          = errors.New("the provider asked to be called later")
	ErrInFlight           = errors.New("the attempt is already being sent or has changed state")
	ErrPayloadChanged     = errors.New("the payload differs from the one first sent")
	ErrProfileMismatch    = errors.New("the provider profile cannot carry this payment")
	ErrUnresolved         = errors.New("the provider has not yet told us what happened; the attempt stays pending")
	ErrProfileSelfCertify = errors.New("a provider profile must be certified by someone other than its author")
	ErrProfileState       = errors.New("the provider profile is not in a state that allows that")
	ErrProfileExists      = errors.New("that provider profile version already exists")
)
