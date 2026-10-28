// Package recon is the reconciliation half of XIC-05 (ZS-SVC-T-001 §10.4).
//
// A reconciliation run compares what the PROVIDER declares (control counts, references, totals, callback receipts,
// batch manifests) with what the platform and the owning domain hold. The comparison is independent (I-25): a run that
// compares counts, references, amounts or time needs a declaration that does not come from the platform itself, and the
// person who supplies the domain's totals is not the person who supplied the provider's. A run that would compare one
// projection with itself is refused (NP-44).
//
// A difference opens a VARIANCE with its evidence and a case/exception path (I-26). Reconciliation never rewrites
// exchange history: it reads exchanges, attempts, receipts and batches and records its own findings, and nothing in
// this package has a way to edit any of them. An attempt that stays UNKNOWN past its limit is escalated, never forced
// to FAILED to clear a queue (NP-46).
package recon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// Run states (Figure 4).
const (
	RunOpen      = "OPEN"
	RunMatched   = "MATCHED"
	RunVariance  = "VARIANCE"
	RunEscalated = "ESCALATED"
	RunClosed    = "CLOSED"
)

// Run origins.
const (
	OriginRequested = "REQUESTED"
	OriginScheduled = "SCHEDULED"
)

// Basis dimensions (§10.4). COUNT, REFERENCE, AMOUNT and TIME compare against a provider declaration; STATE and
// EVIDENCE read what the provider already declared through callbacks and manifests.
const (
	BasisCount     = "COUNT"
	BasisReference = "REFERENCE"
	BasisAmount    = "AMOUNT"
	BasisState     = "STATE"
	BasisEvidence  = "EVIDENCE"
	BasisTime      = "TIME"
)

// Bases lists every dimension.
var Bases = []string{BasisCount, BasisReference, BasisAmount, BasisState, BasisEvidence, BasisTime}

// ExternalSources are where a provider declaration may come from. The platform itself is never one.
var ExternalSources = []string{"PROVIDER_STATEMENT", "PROVIDER_PORTAL_EXPORT", "PROVIDER_API", "PROVIDER_MANIFESTS"}

// Variance types.
const (
	VarCount           = "COUNT_MISMATCH"
	VarRefMissingInt   = "REFERENCE_MISSING_INTERNAL"
	VarRefMissingExt   = "REFERENCE_MISSING_EXTERNAL"
	VarAmount          = "AMOUNT_MISMATCH"
	VarState           = "STATE_MISMATCH"
	VarUnknown         = "UNRESOLVED_UNKNOWN"
	VarStalePending    = "STALE_PENDING"
	VarLateContradicts = "LATE_CALLBACK_CONTRADICTION"
	VarManifest        = "MANIFEST_VARIANCE"
	VarBatchIncomplete = "BATCH_INCOMPLETE"
	VarLateArrival     = "LATE_ARRIVAL"
)

// Variance severities and states.
const (
	SevLow    = "LOW"
	SevMedium = "MEDIUM"
	SevHigh   = "HIGH"

	VarOpen      = "OPEN"
	VarEscalated = "ESCALATED"
	VarResolved  = "RESOLVED"
	VarAccepted  = "ACCEPTED"
)

// Resolutions: how a variance was settled. ACCEPTED is separate, a decision to live with the difference.
var Resolutions = []string{"CORRECTED_AT_SOURCE", "DOMAIN_CORRECTED", "PROVIDER_CONFIRMED", "FALSE_POSITIVE", "SUPERSEDED"}

// Errors. They are stable so the HTTP layer maps them without parsing text.
var (
	ErrNotFound        = errors.New("not found")
	ErrBadRequest      = errors.New("bad request")
	ErrNotIndependent  = errors.New("the comparison is not independent: a count, reference, amount or time comparison needs a declaration from the provider")
	ErrSelfAction      = errors.New("that step must be taken by someone other than the person who supplied the other side of the comparison")
	ErrVersionConflict = errors.New("version conflict: the object changed since it was read")
	ErrState           = errors.New("the object is not in a state that allows this")
	ErrDependency      = errors.New("a dependency needed for this is unavailable")
)

// External is the provider's side of a comparison: what the provider says happened in the period.
type External struct {
	Source      string         `json:"source"`
	Counts      map[string]int `json:"counts,omitempty"`
	References  []string       `json:"references,omitempty"`
	TotalAmount string         `json:"total_amount,omitempty"`
	Currency    string         `json:"currency,omitempty"`
	Cutoff      *time.Time     `json:"cutoff,omitempty"`
}

// DomainTotals is the owning domain's side of an amount comparison, supplied by the domain's service principal.
type DomainTotals struct {
	TotalAmount string `json:"total_amount"`
	Currency    string `json:"currency"`
	Reference   string `json:"reference,omitempty"`
}

// Run is one reconciliation run.
type Run struct {
	RunID              string          `json:"run_id"`
	TenantID           string          `json:"tenant_id"`
	ConnectionID       string          `json:"connection_id"`
	ConnectorID        string          `json:"connector_id"`
	Family             string          `json:"family"`
	Origin             string          `json:"origin"`
	PeriodFrom         time.Time       `json:"period_from"`
	PeriodTo           time.Time       `json:"period_to"`
	Basis              []string        `json:"basis"`
	State              string          `json:"state"`
	ExternalSource     string          `json:"external_source,omitempty"`
	External           json.RawMessage `json:"external,omitempty"`
	ExternalSuppliedBy string          `json:"external_supplied_by,omitempty"`
	AwaitingDomain     bool            `json:"awaiting_domain_totals"`
	DomainTotals       json.RawMessage `json:"domain_totals,omitempty"`
	DomainTotalsBy     string          `json:"domain_totals_by,omitempty"`
	DedupeKey          string          `json:"-"`
	Result             json.RawMessage `json:"result,omitempty"`
	VarianceCount      int             `json:"variance_count"`
	OpenVariances      int             `json:"open_variances"`
	HighVariances      int             `json:"high_variances"`
	EvaluatedAt        *time.Time      `json:"evaluated_at,omitempty"`
	NextEvaluateAt     *time.Time      `json:"next_evaluate_at,omitempty"`
	LeaseUntil         *time.Time      `json:"-"`
	ClosedBy           string          `json:"closed_by,omitempty"`
	CloseNote          string          `json:"close_note,omitempty"`
	RequestedBy        string          `json:"requested_by"`
	CorrelationID      string          `json:"correlation_id,omitempty"`
	Version            int64           `json:"version"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// Variance is a difference a run found. Refs holds the evidence: counts, amounts and object references, never content.
type Variance struct {
	VarianceID     string          `json:"variance_id"`
	TenantID       string          `json:"tenant_id"`
	RunID          string          `json:"run_id"`
	LastRunID      string          `json:"last_run_id"`
	ConnectionID   string          `json:"connection_id"`
	Type           string          `json:"variance_type"`
	Severity       string          `json:"severity"`
	State          string          `json:"state"`
	Fingerprint    string          `json:"-"`
	Refs           json.RawMessage `json:"refs"`
	RepeatCount    int             `json:"repeat_count"`
	CaseRef        string          `json:"case_ref,omitempty"`
	Resolution     string          `json:"resolution,omitempty"`
	ResolutionNote string          `json:"resolution_note,omitempty"`
	ResolvedBy     string          `json:"resolved_by,omitempty"`
	ResolvedAt     *time.Time      `json:"resolved_at,omitempty"`
	EscalatedAt    *time.Time      `json:"escalated_at,omitempty"`
	Version        int64           `json:"version"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Final reports whether the variance has been settled.
func (v *Variance) Final() bool { return v.State == VarResolved || v.State == VarAccepted }

// Event is append-only evidence of a change. It never carries exchange content.
type Event struct {
	Kind       string          `json:"kind"`
	VarianceID string          `json:"variance_id,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	Actor      string          `json:"actor"`
	At         time.Time       `json:"at"`
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

// RunFilter and VarianceFilter narrow lists.
type RunFilter struct {
	State        string
	ConnectionID string
	Limit        int
}

type VarianceFilter struct {
	State        string
	RunID        string
	ConnectionID string
	Severity     string
	Limit        int
}

// RunTx is what MutateRun hands to its function: the locked run and the effects to record in the same transaction.
type RunTx struct {
	Run   *Run
	Event []Event
	Out   []OutEvent
	// Variances are findings to store. A finding whose fingerprint matches a variance still open on the connection is
	// not stored again: that variance's repeat count grows and it is linked to this run.
	Variances []Variance
	// VarianceEvent builds the outbox event of each stored variance (repeat is true when an open one was counted again).
	VarianceEvent func(v Variance, repeat bool) OutEvent
}

// VarTx is what MutateVariance hands to its function.
type VarTx struct {
	V     *Variance
	Run   *Run // the run the variance belongs to; its counters follow the variance
	Event []Event
	Out   []OutEvent
	// RunClosed builds the outbox event of a run that closed because this was its last open variance.
	RunClosed func(r *Run) OutEvent
}

// DueRef names a run (or connection) a worker should drive.
type DueRef struct{ TenantID, ID string }

// ── the read model ─────────────────────────────────────────────────────────

// AttemptFact is what reconciliation reads of an outbound attempt, never its payload.
type AttemptFact struct {
	AttemptID         string
	State             string
	ProviderReference string
	ResolutionMethod  string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	EscalatedAt       *time.Time
	// LatestReceiptState is the target state the provider last reported for the attempt through an applied or
	// rejected callback ("" when it reported nothing).
	LatestReceiptState string
	NeedsReconcile     bool
}

// BatchFact is what reconciliation reads of a batch.
type BatchFact struct {
	BatchID         string
	ExternalBatchID string
	State           string
	DeclaredRows    *int
	AdmittedRows    int
	AcceptedRows    int
	RejectedRows    int
	QuarantinedRows int
	DomainRejected  int
	PendingRows     int
	VarianceRows    int
	ProcessingDone  bool
	ReceivedAt      time.Time
}

// Facts reads the platform's side of a comparison. It is a separate interface so the comparison can be shown to read
// the real tables and never a projection built from the declaration it is compared with.
type Facts interface {
	// InboundCounts counts a connection's inbound exchanges received in [from, to) under the names the provider uses.
	InboundCounts(ctx context.Context, connectionID string, from, to time.Time) (map[string]int, error)
	// OutboundCounts counts a connection's outbound attempts created in [from, to).
	OutboundCounts(ctx context.Context, connectionID string, from, to time.Time) (map[string]int, error)
	// References lists the references the platform holds for the period: batch ids and provider references.
	References(ctx context.Context, connectionID string, from, to time.Time, limit int) ([]string, error)
	Attempts(ctx context.Context, connectionID string, from, to time.Time, limit int) ([]AttemptFact, error)
	Batches(ctx context.Context, connectionID string, from, to time.Time, limit int) ([]BatchFact, error)
	// LateInbound counts inbound exchanges received after the cutoff although they belong to the period's cut.
	LateInbound(ctx context.Context, connectionID string, from, to, cutoff time.Time) (int, error)
}

// Repo is the persistence boundary of reconciliation.
type Repo interface {
	Facts
	// InsertRun stores a new run and its event atomically. A run whose dedupe key is already held is not stored: the
	// holder is returned with existing set.
	InsertRun(ctx context.Context, r *Run, ev Event, out []OutEvent) (held *Run, existing bool, err error)
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)
	RunEvents(ctx context.Context, id string) ([]Event, error)
	MutateRun(ctx context.Context, id string, fn func(tx *RunTx) error) (*Run, error)

	GetVariance(ctx context.Context, id string) (*Variance, error)
	ListVariances(ctx context.Context, f VarianceFilter) ([]Variance, error)
	MutateVariance(ctx context.Context, id string, fn func(tx *VarTx) error) (*Variance, error)

	// DueRuns returns OPEN runs that are ready to evaluate (not waiting for domain totals), across tenants.
	DueRuns(ctx context.Context, now time.Time, limit int) ([]DueRef, error)
	// LastScheduledEnd is the end of the newest scheduled run of a connection (zero when it has none).
	LastScheduledEnd(ctx context.Context, tenant, connectionID string) (time.Time, error)
	RecordSignal(ctx context.Context, tenant string, ev OutEvent) error
}

// EventTypes lists every event reconciliation can emit; a test proves each is produced by a real flow.
var EventTypes = []string{
	"integration.reconciliation.requested", "integration.reconciliation.completed", "integration.reconciliation.variance",
	"integration.reconciliation.escalated", "integration.reconciliation.variance-resolved", "integration.reconciliation.closed",
}

func withTenant(ctx context.Context, tenant string) context.Context {
	return middleware.WithTenant(ctx, tenant)
}
