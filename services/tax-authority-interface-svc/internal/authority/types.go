// Package authority is the live tax-authority adapter of ZS-SVC-M-001 INT-08.
//
// It drives a certified authority profile (eventing/adapter) through a connector that INT-05 approved and a
// credential only the vault can release, around a durable filing attempt. The rules it enforces:
//
//   - the approved document is validated against the authority schema BEFORE anything is sent; a document that
//     fails is BLOCKED, never "fixed" to satisfy the authority (TAX owns the return facts);
//   - nothing leaves before the filing is durably SUBMITTING (compare-and-set), and the profile version, schema
//     rail and transformed payload are pinned then, so a retry is exactly the original;
//   - a timeout or outage is PENDING_UNKNOWN, and a retry first ASKS the authority what it has before it ever
//     sends again, so a timeout can never become a duplicate filing;
//   - transport success is not acceptance: SUBMITTED means the authority holds the filing; ACCEPTED and
//     REJECTED come only from the authority, by poll or callback, and a receipt that does not echo our reference
//     is a mismatch, never an acceptance;
//   - an expired certificate stops the send before it starts, and an outage near the filing deadline is
//     escalated, not left to wait quietly.
package authority

import (
	"context"
	"errors"
	"time"

	"zoiko.io/eventing/adapter"
)

// PlatformScopeID scopes platform reference data (authority profiles).
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// Filing statuses (tax_filing_submissions.status). PENDING is the legacy "recorded, never transmitted" state.
const (
	StatusPending        = "PENDING"
	StatusPrepared       = "PREPARED"
	StatusValidated      = "VALIDATED"
	StatusBlocked        = "BLOCKED"
	StatusSubmitting     = "SUBMITTING"
	StatusSubmitted      = "SUBMITTED"
	StatusPendingUnknown = "PENDING_UNKNOWN"
	StatusAccepted       = "ACCEPTED"
	StatusRejected       = "REJECTED"
	StatusSuperseded     = "SUPERSEDED"
	// StatusDelivered is an e-invoice network reporting the document reached its recipient (EINVOICE only).
	StatusDelivered = "DELIVERED"
)

// Adapter kinds: what an interface carries.
const (
	KindTax      = "TAX"
	KindEInvoice = "EINVOICE"
)

// Profile statuses.
const (
	ProfileDraft      = "DRAFT"
	ProfileCertified  = "CERTIFIED"
	ProfileDeprecated = "DEPRECATED"
	ProfileWithdrawn  = "WITHDRAWN"
)

// ProfileRecord is a stored authority profile.
type ProfileRecord struct {
	ProviderKey string       `json:"provider_key"`
	Version     int          `json:"version"`
	Status      string       `json:"status"`
	Spec        adapter.Spec `json:"spec"`
	SpecHash    string       `json:"spec_hash"`
	CreatedBy   string       `json:"created_by"`
	CertifiedBy string       `json:"certified_by,omitempty"`
	CertifiedAt *time.Time   `json:"certified_at,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}

// Filing is a filing attempt with its interface context, read exactly (the amount as text).
type Filing struct {
	SubmissionID       string        `json:"submission_id"`
	TenantID           string        `json:"tenant_id"`
	InterfaceID        string        `json:"interface_id"`
	LegalEntityID      string        `json:"legal_entity_id"`
	Jurisdiction       string        `json:"jurisdiction"`
	ProviderKey        string        `json:"provider_key"`
	AdapterKind        string        `json:"adapter_kind"` // TAX or EINVOICE
	ConnectorID        string        `json:"connector_id"`
	CertificateRef     string        `json:"certificate_ref,omitempty"`
	CertificateNotAt   *time.Time    `json:"certificate_not_after,omitempty"`
	EscalateBefore     time.Duration `json:"-"`
	TaxPeriod          string        `json:"tax_period"`
	FilingType         string        `json:"filing_type"`
	AmountText         string        `json:"tax_amount"`
	SubjectRef         string        `json:"subject_ref"`
	SubjectFingerprint string        `json:"subject_fingerprint"`
	Document           []byte        `json:"-"`
	PayloadHash        string        `json:"payload_hash"`
	Rail               string        `json:"rail"`
	ProfileVersion     *int          `json:"profile_version,omitempty"`
	SentPayloadHash    string        `json:"sent_payload_hash,omitempty"`
	Status             string        `json:"status"`
	SendCount          int           `json:"send_count"`
	ValidatedAt        *time.Time    `json:"validated_at,omitempty"`
	SentAt             *time.Time    `json:"sent_at,omitempty"`
	InFlightSince      *time.Time    `json:"in_flight_since,omitempty"`
	NotBefore          *time.Time    `json:"not_before,omitempty"`
	NextPollAt         *time.Time    `json:"next_poll_at,omitempty"`
	DeadlineAt         *time.Time    `json:"deadline_at,omitempty"`
	AuthorityRef       string        `json:"authority_ref,omitempty"`
	AuthorityStatus    string        `json:"authority_status,omitempty"`
	ReceiptHash        string        `json:"receipt_hash,omitempty"`
	LastFailure        string        `json:"last_failure,omitempty"`
	EscalatedAt        *time.Time    `json:"escalated_at,omitempty"`
	SubmittedAt        time.Time     `json:"submitted_at"`
}

// CreateFiling is a new approved filing subject.
type CreateFiling struct {
	InterfaceID        string
	TaxPeriod          string
	FilingType         string
	AmountText         string
	SubjectRef         string
	SubjectFingerprint string
	Document           []byte
	PayloadHash        string
	Rail               string
	DeadlineAt         *time.Time
	Actor              string
	CorrelationID      string
}

// OutcomeKind is what to record after an authority call.
type OutcomeKind string

const (
	OutSubmitted OutcomeKind = "SUBMITTED"       // the authority holds the filing (received)
	OutAccepted  OutcomeKind = "ACCEPTED"        // the authority accepted it
	OutDelivered OutcomeKind = "DELIVERED"       // the network delivered it to its recipient (EINVOICE)
	OutRejected  OutcomeKind = "REJECTED"        // the authority refused it
	OutUnknown   OutcomeKind = "PENDING_UNKNOWN" // it may have reached the authority
	OutRelease   OutcomeKind = "RELEASE"         // provably not processed: back to where it was
	OutNone      OutcomeKind = "NONE"            // evidence only
)

// Outcome is the state change and evidence of one call, applied atomically.
type Outcome struct {
	Kind         OutcomeKind
	From         string // the status the filing must be in for this to apply
	ReleaseTo    string // OutRelease only
	AuthorityRef string
	Authority    string // the authority's own status word
	ReceiptHash  string
	Reason       string
	NotBefore    *time.Time
	NextPoll     *time.Time
	Failure      string
	Transport    TransportEntry
}

// TransportEntry is the evidence of one call: hashes and classes, never bodies or credentials.
type TransportEntry struct {
	LogID          string    `json:"log_id,omitempty"`
	SubmissionID   string    `json:"submission_id,omitempty"`
	ProviderKey    string    `json:"provider_key"`
	ProfileVersion int       `json:"profile_version"`
	Operation      string    `json:"operation"`
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

// Due is a filing a worker should look at.
type Due struct{ SubmissionID, TenantID string }

// Store is what the engine needs. The Postgres store implements it.
type Store interface {
	PutProfile(ctx context.Context, key string, spec adapter.Spec, createdBy string) (*ProfileRecord, error)
	GetProfile(ctx context.Context, key string, version int) (*ProfileRecord, error) // version 0: latest CERTIFIED
	CertifyProfile(ctx context.Context, key string, version int, by string) (*ProfileRecord, error)
	SetProfileStatus(ctx context.Context, key string, version int, to string) (*ProfileRecord, error)
	ListProfiles(ctx context.Context, key string) ([]ProfileRecord, error)

	// CreateFiling is idempotent on (tenant, subject_ref): the same subject returns the existing filing; the
	// same subject_ref with a different fingerprint is ErrSubjectConflict.
	CreateFiling(ctx context.Context, in CreateFiling) (f *Filing, created bool, err error)
	LoadFiling(ctx context.Context, id string) (*Filing, error)
	// MarkValidated records the validation AND pins the profile version it was validated under: a profile that
	// changes after approval never silently re-transforms an approved document.
	MarkValidated(ctx context.Context, id, actor string, profileVersion int) (*Filing, error)
	Block(ctx context.Context, id, reason, actor, correlationID string) (*Filing, error)
	BeginSend(ctx context.Context, id, from string, profileVersion int, sentPayloadHash, actor string) (*Filing, error)
	Finish(ctx context.Context, id string, o Outcome, actor, correlationID string) (*Filing, error)
	Escalate(ctx context.Context, id, reason, actor, correlationID string) error
	ListTransport(ctx context.Context, id string) ([]TransportEntry, error)
	RecentCalls(ctx context.Context, providerKey string, since time.Time, limit int) ([]bool, error)
	SweepStale(ctx context.Context, staleBefore time.Time, actor string) (int, error)
	DuePolls(ctx context.Context, now time.Time, limit int) ([]Due, error)
}

// Engine errors. Each maps to one stable HTTP response; none carries authority or credential content.
var (
	ErrFilingNotFound     = errors.New("tax filing submission not found")
	ErrProfileNotFound    = errors.New("no certified authority profile")
	ErrNotConfigured      = errors.New("the filing's interface is not bound to an authority")
	ErrBlocked            = errors.New("the filing was blocked: it does not satisfy the authority schema or profile")
	ErrCertificateExpired = errors.New("the taxpayer certificate has expired")
	ErrCircuitOpen        = errors.New("the authority is failing: sending is paused")
	ErrNotBefore          = errors.New("the authority asked to be called later")
	ErrInFlight           = errors.New("the filing is already being sent or has changed state")
	ErrPayloadChanged     = errors.New("the payload differs from the one first sent")
	ErrSubjectConflict    = errors.New("this submission subject was already approved with a different document")
	ErrUnresolved         = errors.New("the authority has not yet told us what happened; the filing stays pending")
	ErrNoQuery            = errors.New("the authority profile has no status query")
	ErrProfileSelfCertify = errors.New("an authority profile must be certified by someone other than its author")
	ErrProfileState       = errors.New("the authority profile is not in a state that allows that")
	ErrProfileExists      = errors.New("that authority profile version already exists")
	ErrStoreUnavailable   = errors.New("store unavailable")
)
