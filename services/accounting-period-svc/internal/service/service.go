// Package service holds REF-05's business rules. It talks to storage only
// through the Tx interface, so the same rules run against Postgres in
// production (internal/store) and against an in-memory transaction in tests
// (internal/memstore). Every state change, its history row, its idempotency
// record and its outbox event are written through ONE Tx, which the store
// commits atomically.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/events"
)

// OutboxEntry is one event to be written to the outbox in the caller's Tx.
type OutboxEntry struct {
	TenantID  string
	ObjectID  string
	EventType string
	Payload   []byte
}

// IdempotencyRecord remembers the material result of a command.
type IdempotencyRecord struct {
	Key         string
	Operation   string
	RequestHash string
	Response    json.RawMessage
}

// Tx is one storage transaction, scoped to the tenant the Store was opened
// for. "Absent" is reported as (nil, nil).
type Tx interface {
	// LockKey serialises concurrent work on a logical key for the Tx's duration.
	LockKey(ctx context.Context, key string) error

	GetIdempotency(ctx context.Context, key string) (*IdempotencyRecord, error)
	PutIdempotency(ctx context.Context, rec IdempotencyRecord) error

	GetPeriod(ctx context.Context, id string, forUpdate bool) (*domain.Period, error)
	FindPeriodByKey(ctx context.Context, legalEntityID, calendarVersionID, bookScope, moduleScope, periodKey string) (*domain.Period, error)
	// ListPeriodsByKey returns every period of the entity with this period_key (any calendar version / scope).
	ListPeriodsByKey(ctx context.Context, legalEntityID, periodKey string) ([]domain.Period, error)
	// FindPeriodsCovering returns every period of the entity whose [start,end] contains date (any scope/state).
	FindPeriodsCovering(ctx context.Context, legalEntityID, date string) ([]domain.Period, error)
	ListPeriods(ctx context.Context, legalEntityID, state string, limit, offset int) ([]domain.Period, error)
	InsertPeriod(ctx context.Context, p *domain.Period) error
	// UpdatePeriodState writes state, version and the reopen window where
	// version = expectedVersion, returning domain.ErrVersionConflictStore when no row matched.
	UpdatePeriodState(ctx context.Context, p *domain.Period, expectedVersion int64) error

	InsertHistory(ctx context.Context, e *domain.HistoryEntry) error
	// ListHistory returns the period's history, oldest first.
	ListHistory(ctx context.Context, periodID string) ([]domain.HistoryEntry, error)

	// CalendarUsage reports the latest end_date of periods of the calendar
	// (optionally one version) and whether any of them is not OPEN.
	CalendarUsage(ctx context.Context, calendarID, calendarVersionID string) (latestEnd *string, anyNotOpen bool, err error)

	Enqueue(ctx context.Context, e OutboxEntry) error
}

// Store opens transactions. tenantID is installed as app.tenant_id for
// row-level security.
type Store interface {
	InTx(ctx context.Context, tenantID string, fn func(Tx) error) error
}

// Service implements REF-05.
type Service struct {
	store       Store
	calendar    CalendarClient
	provenance  ProvenanceVerifier
	now         func() time.Time
	newID       func() string
	maxReopen   time.Duration
	auditFailed func(error)
}

// DefaultReopenMaxWindow is the default bound on a reopen's expires_at.
const DefaultReopenMaxWindow = 72 * time.Hour

// MaxPeriodsPerMaterialize caps one materialisation, so one PeriodOpened event
// per period is always possible within a single transaction.
const MaxPeriodsPerMaterialize = 60

// New builds the service with the real clock and UUIDv7 identifiers. calendar
// and provenance must be supplied (use the HTTP clients in internal/clients).
func New(store Store, calendar CalendarClient, provenance ProvenanceVerifier) *Service {
	return &Service{
		store: store, calendar: calendar, provenance: provenance,
		now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
		newID: func() string {
			id, err := uuid.NewV7()
			if err != nil {
				panic(fmt.Sprintf("uuidv7: %v", err)) // entropy source failure; cannot continue safely
			}
			return id.String()
		},
		maxReopen:   DefaultReopenMaxWindow,
		auditFailed: func(error) {},
	}
}

// WithClock overrides the clock (tests; the posting gate and reopen expiry use it).
func (s *Service) WithClock(now func() time.Time) *Service { s.now = now; return s }

// WithIDGenerator overrides id generation (tests).
func (s *Service) WithIDGenerator(f func() string) *Service { s.newID = f; return s }

// WithReopenMaxWindow overrides the maximum reopen window.
func (s *Service) WithReopenMaxWindow(d time.Duration) *Service { s.maxReopen = d; return s }

// WithAuditFailureHook is called when a PeriodCommandRejected event could not be recorded.
func (s *Service) WithAuditFailureHook(f func(error)) *Service { s.auditFailed = f; return s }

// Now exposes the service clock (handlers use it for response timestamps).
func (s *Service) Now() time.Time { return s.now() }

// Meta is the trusted request context shared by every command.
type Meta struct {
	Actor          string
	TenantID       string
	CorrelationID  string
	CausationID    string
	IdempotencyKey string
	RequestHash    string
	Reason         string
}

func (m Meta) check() error {
	if m.Actor == "" || m.TenantID == "" {
		return domain.Errf(domain.CodeContextInvalid, "actor and tenant context are required")
	}
	if m.IdempotencyKey == "" {
		return domain.Errf(domain.CodeContextInvalid, "Idempotency-Key is required")
	}
	if m.Reason == "" {
		return domain.Errf(domain.CodeContextInvalid, "reason is required")
	}
	return nil
}

// replayOrNil returns the stored result of (tenant, Idempotency-Key) when one
// exists for the SAME request, nil when none exists, and CONTEXT_INVALID when
// the key was used for a different request.
func replayOrNil[T any](rec *IdempotencyRecord, m Meta, op string) (*T, error) {
	if rec == nil {
		return nil, nil
	}
	if rec.RequestHash != m.RequestHash || rec.Operation != op {
		return nil, domain.Errf(domain.CodeContextInvalid, "Idempotency-Key was already used for a different request")
	}
	var out T
	if err := json.Unmarshal(rec.Response, &out); err != nil {
		return nil, fmt.Errorf("decode stored idempotent response: %w", err)
	}
	return &out, nil
}

// idempotent runs fn at most once per (tenant, Idempotency-Key). A replay with
// the same request returns the ORIGINAL stored result; the same key with a
// different request is refused.
func idempotent[T any](ctx context.Context, tx Tx, m Meta, op string, fn func() (*T, error)) (*T, bool, error) {
	if err := tx.LockKey(ctx, "idem:"+m.TenantID+":"+m.IdempotencyKey); err != nil {
		return nil, false, err
	}
	rec, err := tx.GetIdempotency(ctx, m.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if prev, err := replayOrNil[T](rec, m, op); err != nil || prev != nil {
		return prev, prev != nil, err
	}
	res, err := fn()
	if err != nil {
		return nil, false, err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil, false, fmt.Errorf("encode idempotent response: %w", err)
	}
	if err := tx.PutIdempotency(ctx, IdempotencyRecord{Key: m.IdempotencyKey, Operation: op, RequestHash: m.RequestHash, Response: b}); err != nil {
		return nil, false, err
	}
	return res, false, nil
}

// peekReplay is the read-only first phase of every command: it finds a stored
// result for the key without doing any external call.
func peekReplay[T any](ctx context.Context, s Store, m Meta, op string) (*T, error) {
	var out *T
	err := s.InTx(ctx, m.TenantID, func(tx Tx) error {
		rec, err := tx.GetIdempotency(ctx, m.IdempotencyKey)
		if err != nil {
			return err
		}
		out, err = replayOrNil[T](rec, m, op)
		return err
	})
	return out, err
}

func (s *Service) enqueue(ctx context.Context, tx Tx, ev events.Event) error {
	key, body, err := events.Build(ev)
	if err != nil {
		return err
	}
	return tx.Enqueue(ctx, OutboxEntry{TenantID: ev.TenantID, ObjectID: key, EventType: ev.Type, Payload: body})
}

// ── upstream contracts ───────────────────────────────────────────────────────

// CalendarPeriod is one period of a fiscal-calendar version (REF-04 preview).
type CalendarPeriod struct {
	PeriodKey string `json:"period_key"`
	PeriodNo  int    `json:"period_no"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Kind      string `json:"kind"`
}

// CalendarPreview is REF-04's periods-preview response.
type CalendarPreview struct {
	CalendarID    string           `json:"calendar_id"`
	VersionID     string           `json:"version_id"`
	VersionNo     int              `json:"version_no"`
	LegalEntityID string           `json:"legal_entity_id"`
	FiscalYear    int              `json:"fiscal_year"`
	Periods       []CalendarPeriod `json:"periods"`
}

// CalendarRef is REF-04's :resolve response.
type CalendarRef struct {
	CalendarID string `json:"calendar_id"`
	VersionID  string `json:"version_id"`
}

// CalendarClient reads REF-04 (fiscal-calendar-svc). Implementations return
// typed *domain.Error: DEPENDENCY_UNAVAILABLE when unreachable, NOT_FOUND when
// REF-04 does not know the object.
type CalendarClient interface {
	// Resolve is GET /v1/fiscal-calendars:resolve?legal_entity_id=&scope=&date=
	Resolve(ctx context.Context, tenantID, legalEntityID, scope, date string) (*CalendarRef, error)
	// PeriodsPreview is GET /v1/fiscal-calendar-versions/{vid}/periods-preview?fiscal_year=YYYY
	PeriodsPreview(ctx context.Context, tenantID, versionID string, fiscalYear int) (*CalendarPreview, error)
}

// ProvenanceRequest is what the verifier checks against ACC-14.
type ProvenanceRequest struct {
	TenantID           string
	LegalEntityID      string
	PeriodKey          string
	Command            domain.Command
	Acc14WorkflowRef   string
	ControlSnapshotRef string
}

// ProvenanceVerifier verifies, with ACC-14, that the workflow ref names an
// APPROVED workflow for exactly this period and command and carries exactly
// this control snapshot. Implementations return SOURCE_UNVERIFIED when the
// evidence is absent or does not match, and DEPENDENCY_UNAVAILABLE when
// ACC-14 cannot be reached; the command is rejected in both cases (fail closed).
type ProvenanceVerifier interface {
	Verify(ctx context.Context, req ProvenanceRequest) error
}
