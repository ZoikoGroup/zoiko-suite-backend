// Package rollout is Wave 8 of XIC (ZS-SVC-T-001 §18.2, §18.3): staged production rollout of a connection and the
// migration of ad-hoc connectors into the generic exchange history.
//
// A production connection does not go from certified to carrying everything in one step. It is PLANNED, then runs a
// PILOT (an operation allowlist and a daily budget) until it has soaked long enough with enough clean exchanges, then
// goes GENERAL. Each step forward is a change request with the readiness evidence of §18.2 attached and is decided by
// someone other than the requester; the step back (ROLLED_BACK) is always immediate and a pilot that misbehaves rolls
// itself back. The stage is enforced where exchanges start (xicconn.Operable), so no path around it exists.
//
// An ad-hoc connector (a bank or tax integration that predates the registry) is brought in through an inventory
// entry that moves DISCOVERED -> MAPPED -> SHADOW -> CUTOVER -> RETIRED. In SHADOW the owning service registers its
// attempts into the generic history (append-only, never edited) and a parity run compares them with what the
// platform holds; cutover needs a passed run and a second person, and can be reversed.
package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// Rollout stages.
const (
	StagePlanned    = "PLANNED"
	StagePilot      = "PILOT"
	StageGeneral    = "GENERAL"
	StageRolledBack = "ROLLED_BACK"
)

// Legacy connector states.
const (
	LegacyDiscovered = "DISCOVERED"
	LegacyMapped     = "MAPPED"
	LegacyShadow     = "SHADOW"
	LegacyCutover    = "CUTOVER"
	LegacyRetired    = "RETIRED"
)

// Subjects of a change request.
const (
	KindRollout = "ROLLOUT"
	KindLegacy  = "LEGACY"
)

// Request statuses.
const (
	ReqPending  = "PENDING"
	ReqApproved = "APPROVED"
	ReqRejected = "REJECTED"
	ReqExpired  = "EXPIRED"
	ReqUsed     = "USED"
)

// Legacy exchange outcomes and directions.
const (
	OutPending = "PENDING"
	OutSettled = "SETTLED"
	OutFailed  = "FAILED"
	OutUnknown = "UNKNOWN"

	DirInbound  = "INBOUND"
	DirOutbound = "OUTBOUND"
	DirBatch    = "BATCH"
)

// Attestations of §18.2 "Production": what a person vouches for outside the platform's reach.
const (
	AttSLOAlerts = "SLO_ALERTS"
	AttRunbooks  = "RUNBOOKS"
	AttOnCall    = "ONCALL"
	AttNoP0P1    = "NO_P0_P1_DEFECTS"
)

// Attestations lists every attestation.
var Attestations = []string{AttSLOAlerts, AttRunbooks, AttOnCall, AttNoP0P1}

// Errors, stable so the HTTP layer maps them without parsing text.
var (
	ErrNotFound        = errors.New("not found")
	ErrBadRequest      = errors.New("bad request")
	ErrExists          = errors.New("a live rollout already exists for this connection")
	ErrState           = errors.New("the object is not in a state that allows this")
	ErrSelfAction      = errors.New("that step must be taken by someone other than the person who requested it")
	ErrNotReady        = errors.New("the readiness evidence does not allow this step")
	ErrVersionConflict = errors.New("version conflict: the object changed since it was read")
	ErrConflict        = errors.New("a registration with this reference already exists with different content")
	ErrDependency      = errors.New("a dependency needed for this is unavailable")
)

// Rollout is one connection's staged go-live.
type Rollout struct {
	RolloutID      string    `json:"rollout_id"`
	TenantID       string    `json:"tenant_id"`
	ConnectionID   string    `json:"connection_id"`
	ConnectorID    string    `json:"connector_id"`
	Stage          string    `json:"stage"`
	PilotOps       []string  `json:"pilot_operations"`
	PilotDailyCap  int       `json:"pilot_daily_cap"`
	MinSoakHours   int       `json:"min_soak_hours"`
	MinSuccess     int       `json:"min_success"`
	MaxFailurePct  int       `json:"max_failure_pct"`
	StageSince     time.Time `json:"stage_since"`
	AdvanceRequest string    `json:"-"`
	RollbackReason string    `json:"rollback_reason,omitempty"`
	RolledBackBy   string    `json:"rolled_back_by,omitempty"`
	CreatedBy      string    `json:"created_by"`
	Version        int64     `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Terms are what a rollout is created with.
type Terms struct {
	PilotOperations []string `json:"pilot_operations"`
	PilotDailyCap   int      `json:"pilot_daily_cap"`
	MinSoakHours    int      `json:"min_soak_hours"`
	MinSuccess      int      `json:"min_success"`
	MaxFailurePct   int      `json:"max_failure_pct"`
}

// Request is a change request for one step forward of a rollout or legacy connector.
type Request struct {
	RequestID    string          `json:"request_id"`
	TenantID     string          `json:"tenant_id"`
	SubjectKind  string          `json:"subject_kind"`
	SubjectID    string          `json:"subject_id"`
	FromState    string          `json:"from_state"`
	ToState      string          `json:"to_state"`
	Status       string          `json:"status"`
	Attestations []string        `json:"attestations"`
	Readiness    json.RawMessage `json:"readiness,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	RequestedBy  string          `json:"requested_by"`
	DecidedBy    string          `json:"decided_by,omitempty"`
	DecisionNote string          `json:"decision_note,omitempty"`
	ExpiresAt    time.Time       `json:"expires_at"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	Version      int64           `json:"version"`
}

// Check is one line of a readiness report.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Readiness is the evidence for a step forward. It is recomputed at the decision, never trusted from the request.
type Readiness struct {
	Ready  bool    `json:"ready"`
	Checks []Check `json:"checks"`
}

// Legacy is an ad-hoc connector in the migration inventory.
type Legacy struct {
	LegacyID       string    `json:"legacy_id"`
	TenantID       string    `json:"tenant_id"`
	Name           string    `json:"name"`
	OwningService  string    `json:"owning_service"`
	Direction      string    `json:"direction"`
	Description    string    `json:"description,omitempty"`
	State          string    `json:"state"`
	ConnectionID   string    `json:"connection_id,omitempty"`
	StateSince     time.Time `json:"state_since"`
	AdvanceRequest string    `json:"-"`
	CreatedBy      string    `json:"created_by"`
	Version        int64     `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// LegacyExchange is one attempt the owning service registers into the generic history. It carries references and a
// hash of the content, never the content.
type LegacyExchange struct {
	EntryID          string    `json:"entry_id,omitempty"`
	TenantID         string    `json:"tenant_id,omitempty"`
	LegacyID         string    `json:"legacy_id,omitempty"`
	ExternalRef      string    `json:"external_ref"`
	DomainRequestRef string    `json:"domain_request_ref,omitempty"`
	Operation        string    `json:"operation,omitempty"`
	Outcome          string    `json:"outcome"`
	PayloadHash      string    `json:"payload_hash"`
	OccurredAt       time.Time `json:"occurred_at"`
	RegisteredBy     string    `json:"registered_by,omitempty"`
	RegisteredAt     time.Time `json:"registered_at,omitempty"`
}

// ParityRun compares the registered legacy exchanges of a window with what the platform holds.
type ParityRun struct {
	ParityID       string          `json:"parity_id"`
	TenantID       string          `json:"tenant_id"`
	LegacyID       string          `json:"legacy_id"`
	WindowFrom     time.Time       `json:"window_from"`
	WindowTo       time.Time       `json:"window_to"`
	Compared       int             `json:"compared"`
	Matched        int             `json:"matched"`
	MissingInXIC   int             `json:"missing_in_xic"`
	OutcomeDiffers int             `json:"outcome_differs"`
	XICOnly        int             `json:"xic_only"`
	Samples        json.RawMessage `json:"samples"`
	Passed         bool            `json:"passed"`
	RunBy          string          `json:"run_by"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Event is append-only evidence.
type Event struct {
	SubjectKind string          `json:"subject_kind"`
	SubjectID   string          `json:"subject_id"`
	Kind        string          `json:"kind"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	Actor       string          `json:"actor"`
	At          time.Time       `json:"at"`
}

// OutEvent is a domain event for the transactional outbox.
type OutEvent struct {
	Type          string
	AggregateType string
	AggregateID   string
	ActorID       string
	CorrelationID string
	Data          map[string]any
}

// Actor is who is acting.
type Actor struct {
	PrincipalID   string
	CorrelationID string
}

// Outcomes are a connection's exchange outcomes in a window, from the real attempt and exchange tables.
type Outcomes struct {
	Success int // outbound SETTLED, inbound HANDED_OFF
	Failure int // outbound FAILED, inbound REJECTED
	Unknown int // outbound UNKNOWN: ambiguity counts against a pilot
}

// Facts reads the platform's side of readiness and parity.
type Facts interface {
	Outcomes(ctx context.Context, connectionID string, from, to time.Time) (Outcomes, error)
	// OpenHighVariances counts HIGH variances still open on the connection.
	OpenHighVariances(ctx context.Context, connectionID string) (int, error)
	// OverdueUnknown counts UNKNOWN attempts created before the cutoff and still unresolved.
	OverdueUnknown(ctx context.Context, connectionID string, cutoff time.Time) (int, error)
	// Counterparts returns the platform's state for each reference of a legacy connector of this direction:
	// OUTBOUND by domain_request_ref (attempt state), BATCH by external batch id (state "RECEIVED"..., presence only).
	Counterparts(ctx context.Context, direction, connectionID string, refs []string) (map[string]string, error)
	// InboundCount counts inbound exchanges received in the window (INBOUND parity compares counts).
	InboundCount(ctx context.Context, connectionID string, from, to time.Time) (int, error)
	// XICOnly counts platform records of the window that no registered reference accounts for.
	XICOnly(ctx context.Context, direction, connectionID string, from, to time.Time, refs []string) (int, error)
}

// RolloutTx is what MutateRollout hands to its function: the locked rollout and the effects to store with it.
type RolloutTx struct {
	R     *Rollout
	Event []Event
	Out   []OutEvent
	// Use, when set, is the request this change consumes: the store marks it USED in the same transaction, before
	// the rollout moves, because the database accepts a forward move only against a used, approved request.
	Use string
}

// LegacyTx is what MutateLegacy hands to its function.
type LegacyTx struct {
	L     *Legacy
	Event []Event
	Out   []OutEvent
	Use   string
}

// RequestTx is what MutateRequest hands to its function.
type RequestTx struct {
	Q     *Request
	Event []Event
	Out   []OutEvent
}

// RolloutFilter, LegacyFilter and RequestFilter narrow lists.
type RolloutFilter struct {
	Stage        string
	ConnectionID string
	Limit        int
}

type LegacyFilter struct {
	State string
	Limit int
}

type RequestFilter struct {
	SubjectKind string
	SubjectID   string
	Status      string
	Limit       int
}

// Repo is the persistence boundary.
type Repo interface {
	Facts

	InsertRollout(ctx context.Context, r *Rollout, ev Event, out []OutEvent) error
	GetRollout(ctx context.Context, id string) (*Rollout, error)
	// LatestRolloutFor returns the connection's newest rollout in any stage (a rolled-back one still blocks); ErrNotFound when it has none. The
	// worker form reads across tenants.
	LatestRolloutFor(ctx context.Context, connectionID string) (*Rollout, error)
	ListRollouts(ctx context.Context, f RolloutFilter) ([]Rollout, error)
	MutateRollout(ctx context.Context, id string, fn func(tx *RolloutTx) error) (*Rollout, error)
	// PilotRollouts lists pilots across tenants for the evaluator.
	PilotRollouts(ctx context.Context, limit int) ([]DueRef, error)
	// Admit takes one unit of the day's pilot budget atomically. It reports false, without taking anything, when the
	// day's budget is spent.
	Admit(ctx context.Context, r *Rollout, day time.Time) (bool, error)
	UsedToday(ctx context.Context, rolloutID string, day time.Time) (int, error)

	InsertRequest(ctx context.Context, q *Request, ev Event, out []OutEvent) error
	GetRequest(ctx context.Context, id string) (*Request, error)
	ListRequests(ctx context.Context, f RequestFilter) ([]Request, error)
	MutateRequest(ctx context.Context, id string, fn func(tx *RequestTx) error) (*Request, error)

	InsertLegacy(ctx context.Context, l *Legacy, ev Event, out []OutEvent) error
	GetLegacy(ctx context.Context, id string) (*Legacy, error)
	ListLegacy(ctx context.Context, f LegacyFilter) ([]Legacy, error)
	MutateLegacy(ctx context.Context, id string, fn func(tx *LegacyTx) error) (*Legacy, error)

	// RegisterExchanges stores new registrations append-only. A registration identical to a stored one is counted
	// as a duplicate and not stored; one that names a stored reference with a different payload hash is a conflict
	// and nothing of the call is stored.
	RegisterExchanges(ctx context.Context, legacyID string, in []LegacyExchange, by string, at time.Time) (stored, duplicates int, err error)
	// LatestExchanges returns the newest registration of every reference occurring in [from, to), up to limit.
	LatestExchanges(ctx context.Context, legacyID string, from, to time.Time, limit int) ([]LegacyExchange, error)
	InsertParity(ctx context.Context, p *ParityRun, ev Event, out []OutEvent) error
	ListParity(ctx context.Context, legacyID string, limit int) ([]ParityRun, error)
	Events(ctx context.Context, kind, id string) ([]Event, error)
	RecordSignal(ctx context.Context, tenant string, ev OutEvent) error
}

// DueRef names a rollout the evaluator should look at.
type DueRef struct{ TenantID, ID string }

// EventTypes lists every event this wave can emit; a test proves each is produced by a real flow.
var EventTypes = []string{
	"integration.rollout.planned", "integration.rollout.advance-requested", "integration.rollout.advanced",
	"integration.rollout.rolled-back", "integration.rollout.request-rejected",
	"integration.legacy.discovered", "integration.legacy.mapped", "integration.legacy.shadow-started",
	"integration.legacy.parity-run", "integration.legacy.cutover-requested", "integration.legacy.cutover",
	"integration.legacy.cutover-reverted", "integration.legacy.shadow-stopped", "integration.legacy.retired",
}

func withTenant(ctx context.Context, tenant string) context.Context {
	return middleware.WithTenant(ctx, tenant)
}
