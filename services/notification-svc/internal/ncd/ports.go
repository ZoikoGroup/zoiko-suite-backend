package ncd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"zoiko.io/notification-svc/internal/events"
)

// ── errors ──────────────────────────────────────────────────────────────────

// Kind classifies an Error for the HTTP layer.
type Kind int

const (
	KindInvalid     Kind = iota + 1 // 400
	KindForbidden                   // 403
	KindNotFound                    // 404
	KindConflict                    // 409
	KindRefused                     // 422, carries a §10.3 reason code
	KindUnavailable                 // 503
)

// Error is every failure the plane returns to a caller.
type Error struct {
	Kind    Kind
	Code    string
	Detail  string
	Refusal *Refusal
}

func (e *Error) Error() string { return e.Code + ": " + e.Detail }

func Invalid(code, format string, a ...any) *Error {
	return &Error{Kind: KindInvalid, Code: code, Detail: fmt.Sprintf(format, a...)}
}
func Forbidden(code, format string, a ...any) *Error {
	return &Error{Kind: KindForbidden, Code: code, Detail: fmt.Sprintf(format, a...)}
}
func NotFound(code, format string, a ...any) *Error {
	return &Error{Kind: KindNotFound, Code: code, Detail: fmt.Sprintf(format, a...)}
}
func Conflict(code, format string, a ...any) *Error {
	return &Error{Kind: KindConflict, Code: code, Detail: fmt.Sprintf(format, a...)}
}
func Unavailable(code, format string, a ...any) *Error {
	return &Error{Kind: KindUnavailable, Code: code, Detail: fmt.Sprintf(format, a...)}
}
func Refused(r *Refusal) *Error {
	return &Error{Kind: KindRefused, Code: string(r.Code), Detail: r.Detail, Refusal: r}
}

// AsError extracts an *Error, wrapping anything else as unavailable: an
// unclassified failure is the store or a dependency, never the caller.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var r *Refusal
	if errors.As(err, &r) {
		return Refused(r)
	}
	return Unavailable("store_unavailable", "%v", err)
}

// Store sentinel errors, returned by Tx implementations.
var (
	ErrNotFound            = errors.New("not found")
	ErrUnknownOutstanding  = errors.New("an UNKNOWN attempt is outstanding")
	ErrProviderIDCollision = errors.New("provider message id already maps to another attempt")
	ErrDuplicateEvent      = errors.New("provider event already recorded")
	ErrStaleVersion        = errors.New("expected version does not match")
)

// ── store ports ─────────────────────────────────────────────────────────────

// WorkRef names one unit of cross-tenant work: an id and its tenant, nothing
// else — the same narrowness the retry worker's DueRetry keeps, so no content
// crosses the platform-scope hatch.
type WorkRef struct {
	ID       string
	TenantID string
}

// ReputationRow is one tenant × stream × binding window of outcomes (§7.4).
type ReputationRow struct {
	TenantID      string  `json:"tenant_id"`
	Stream        string  `json:"stream"`
	BindingID     string  `json:"binding_id"`
	Attempts      int     `json:"attempts"`
	HardBounces   int     `json:"hard_bounces"`
	Complaints    int     `json:"complaints"`
	Failures      int     `json:"failures"`
	BounceRate    float64 `json:"hard_bounce_rate"`
	ComplaintRate float64 `json:"complaint_rate"`
}

// Store is the persistence the plane needs outside a transaction.
type Store interface {
	// InTx runs fn in one tenant-scoped transaction. Everything that must be
	// atomic with a state change — the attempt, its evidence, its event —
	// goes through the Tx it passes.
	InTx(ctx context.Context, tenantID string, fn func(Tx) error) error

	// Cross-tenant discovery (SELECT-only platform scope).
	DueJobs(ctx context.Context, now time.Time, limit int) ([]WorkRef, error)
	UnknownPastDue(ctx context.Context, now time.Time, limit int) ([]WorkRef, error)
	StaleSubmitting(ctx context.Context, before time.Time, limit int) ([]WorkRef, error)
	NoticesDue(ctx context.Context, horizon time.Time, limit int) ([]WorkRef, error)
	FindAttempt(ctx context.Context, bindingID, token, providerMessageID string) (*WorkRef, error)
	Reputation(ctx context.Context, since time.Time) ([]ReputationRow, error)

	// Platform configuration.
	Bindings(ctx context.Context, now time.Time) ([]Binding, error)
	SetBindingHealth(ctx context.Context, bindingID, state, reason, actor string) error
	RecordBindingResult(ctx context.Context, bindingID string, success bool, threshold int) error
}

// Tx is one tenant-scoped transaction.
type Tx interface {
	TenantID() string
	// Enqueue writes an event into the outbox on THIS transaction. There is no
	// other way to emit, so no fact can be announced outside the transaction
	// that records it.
	Enqueue(out events.Outbound) error

	// NCD-01
	InsertIntent(i *Intent) error
	GetIntent(id string, version int) (*Intent, error) // version 0 = latest
	ListIntentVersions(id string) ([]Intent, error)
	ActivateIntent(id string, version int, approver string, activatedAt, effectiveFrom time.Time) error
	RetireIntent(id, actor string, at time.Time) (int, error)
	EffectiveIntent(id string, txTime, knownAt time.Time) (*Intent, error)
	NextTemplateSlot(intentID, channel, locale string) (templateID string, version int, err error)
	InsertTemplate(tv *TemplateVersion) error
	GetTemplate(id string) (*TemplateVersion, error)
	UpdateTemplate(tv *TemplateVersion) error
	ListTemplates(intentID string) ([]TemplateVersion, error)
	EffectiveTemplates(intentID string, txTime, knownAt time.Time) ([]TemplateVersion, error)

	// NCD-02
	GetPreference(principalID string) (*Preference, error) // nil, nil when none
	SavePreference(p *Preference, expectedVersion int) error
	InsertSuppression(s *Suppression) (created bool, err error)
	GetSuppression(id string) (*Suppression, error)
	LiftSuppression(id, liftedBy, evidenceRef, approvedBy string, at time.Time) error
	ListSuppressions(principalID, endpointHash string, activeOnly bool, limit int) ([]Suppression, error)
	// ActiveSuppressions returns every suppression that could apply to a
	// recipient or any of its endpoints — canonical rows plus the legacy
	// email_suppressions list, matched by raw address.
	ActiveSuppressions(principalID string, endpointHashes, emails []string, now time.Time) ([]Suppression, error)
	CountSoftBounces(endpointHash string, since time.Time) (int, error)
	InsertPlan(p *RecipientPlan) error
	GetPlan(id string) (*RecipientPlan, error)
	InsertDecision(d *ChannelDecision) error
	GetDecision(id string) (*ChannelDecision, error)
	ListDecisions(communicationID string) ([]ChannelDecision, error)
	GetStreamControl(stream string) (state, reason string, err error)
	SetStreamControl(stream, state, reason, actor string, automatic bool) error

	// Communications
	InsertCommunication(c *Communication) (created bool, err error) // replay fills c
	GetCommunication(id string, forUpdate bool) (*Communication, error)
	UpdateCommunication(c *Communication) error
	ListCommunications(recipientPrincipalID string, limit int) ([]Communication, error)
	InsertRender(r *RenderedContent) error
	ListRenders(communicationID string) ([]RenderedContent, error)

	// NCD-03
	InsertJob(j *DeliveryJob) error
	GetJob(id string) (*DeliveryJob, error)
	ClaimJob(id string, now time.Time, lease time.Duration) (*DeliveryJob, error) // nil when not claimable
	UpdateJob(j *DeliveryJob) error
	ListJobs(communicationID string) ([]DeliveryJob, error)
	HasUnknownAttempt(communicationID string) (bool, error)
	InsertAttempt(a *Attempt) error
	GetAttempt(id string) (*Attempt, error)
	UpdateAttempt(a *Attempt) error
	SetProviderMessageID(attemptID, providerMessageID string) error
	ListAttempts(communicationID string) ([]Attempt, error)
	CountAttempts(since time.Time, recipientPrincipalID, channel, intentID string) (int, error)

	// NCD-04
	InsertEvidence(e *Evidence) error
	ListEvidence(communicationID string) ([]Evidence, error)
	InsertException(x *Exception) (created bool, err error)
	ListExceptions(openOnly bool, limit int) ([]Exception, error)
	ResolveException(id, actor, resolution string, at time.Time) error

	// NCD-05
	InsertNotice(n *RegulatedNotice) error
	GetNotice(id string, version int) (*RegulatedNotice, error) // version 0 = latest
	GetNoticeByCommunication(communicationID string) (*RegulatedNotice, error)
	ListNoticeVersions(id string) ([]RegulatedNotice, error)
	UpdateNotice(n *RegulatedNotice) error
	InsertAck(a *Acknowledgment) error
	ListAcks(noticeID string) ([]Acknowledgment, error)

	// Maker-checker and bulk
	InsertApproval(a *Approval) error
	GetApproval(id string, forUpdate bool) (*Approval, error)
	UpdateApproval(a *Approval) error
	ListApprovals(targetID string) ([]Approval, error)
	InsertBulk(b *BulkSend) error
	GetBulk(id string, forUpdate bool) (*BulkSend, error)
	UpdateBulk(b *BulkSend) error
}

// ── collaborators ───────────────────────────────────────────────────────────

// RecipientResolver is the IAM contact authority (identity-context-svc).
type RecipientResolver interface {
	ResolveEmail(ctx context.Context, tenantID, callerPrincipalID, recipientPrincipalID string) (string, error)
}

// Message is one rendered submission handed to a transport.
type Message struct {
	TenantID             string
	LegalEntityID        string
	CommunicationID      string
	AttemptID            string
	IdempotencyToken     string
	Channel              string
	BindingID            string
	To                   string
	RecipientPrincipalID string
	Subject              string
	Body                 string
	CorrelationID        string
	SenderIdentity       string
}

// Transport submits one message through one binding.
type Transport interface {
	Submit(ctx context.Context, b Binding, m Message) SubmitOutcome
}
