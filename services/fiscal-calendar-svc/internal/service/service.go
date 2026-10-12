// Package service holds REF-04's business rules. It talks to storage only
// through the Tx interface, so the same rules run against Postgres in
// production (internal/store) and against an in-memory transaction in tests
// (internal/memstore). Every state change, its idempotency record and its
// outbox event are written through ONE Tx, which the store commits atomically.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
	"zoiko.io/fiscal-calendar-svc/internal/events"
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

// Tx is one storage transaction, scoped to one tenant. "Absent" is reported as
// (nil, nil). Every method sees only the transaction's own tenant.
type Tx interface {
	// LockKey serialises concurrent work on a logical key for the Tx's duration.
	LockKey(ctx context.Context, key string) error

	GetIdempotency(ctx context.Context, key string) (*IdempotencyRecord, error)
	PutIdempotency(ctx context.Context, rec IdempotencyRecord) error

	GetCalendarByID(ctx context.Context, id string, forUpdate bool) (*domain.FiscalCalendar, error)
	FindCalendarByCode(ctx context.Context, legalEntityID, code string) (*domain.FiscalCalendar, error)
	InsertCalendar(ctx context.Context, c *domain.FiscalCalendar) error
	// UpdateCalendar writes status/version/recorded_at where version = expectedVersion,
	// returning domain.ErrVersionConflictStore when no row matched.
	UpdateCalendar(ctx context.Context, c *domain.FiscalCalendar, expectedVersion int64) error

	GetVersionByID(ctx context.Context, id string, forUpdate bool) (*domain.FiscalCalendarVersion, error)
	// ListVersions returns every version of a calendar, version_no ascending.
	ListVersions(ctx context.Context, calendarID string) ([]domain.FiscalCalendarVersion, error)
	// ListInForceVersions returns every ACTIVE or SUPERSEDED version of a
	// (legal entity, scope), across calendars, effective_from ascending.
	ListInForceVersions(ctx context.Context, legalEntityID, scope string) ([]domain.FiscalCalendarVersion, error)
	InsertVersion(ctx context.Context, v *domain.FiscalCalendarVersion) error
	// UpdateVersion writes the lifecycle columns (status, approval/activation
	// evidence, effective_to when closed by supersession, version) where
	// version = expectedVersion. Pattern and effective dates are never written.
	UpdateVersion(ctx context.Context, v *domain.FiscalCalendarVersion, expectedVersion int64) error
	InsertStatusHistory(ctx context.Context, e *domain.StatusHistoryEntry) error

	GetPlanByID(ctx context.Context, id string, forUpdate bool) (*domain.CalendarTransitionPlan, error)
	// ListPlansForVersion returns every plan whose to_version is the version, oldest first.
	ListPlansForVersion(ctx context.Context, toVersionID string) ([]domain.CalendarTransitionPlan, error)
	InsertPlan(ctx context.Context, p *domain.CalendarTransitionPlan) error
	UpdatePlan(ctx context.Context, p *domain.CalendarTransitionPlan, expectedVersion int64) error

	Enqueue(ctx context.Context, e OutboxEntry) error
}

// Store opens transactions. tenantID is the actor's tenant context (installed
// as app.tenant_id for row-level security on every table).
type Store interface {
	InTx(ctx context.Context, tenantID string, fn func(Tx) error) error
}

// PeriodHistory is what REF-05 reports about a calendar's materialised use.
type PeriodHistory struct {
	// LatestPeriodEnd is the end date of the latest posted or hard-closed
	// period of the calendar; nil when there is none.
	LatestPeriodEnd          *domain.Date
	HasPostedOrClosedPeriods bool
}

// PeriodHistoryClient asks accounting-period-svc (REF-05) whether a calendar
// has posted or closed periods. Any error is treated as "unknown" and the
// calling command FAILS CLOSED with DEPENDENCY_UNAVAILABLE.
type PeriodHistoryClient interface {
	CalendarUsage(ctx context.Context, tenantID, legalEntityID, calendarID string) (*PeriodHistory, error)
}

// Service implements REF-04.
type Service struct {
	store   Store
	history PeriodHistoryClient
	now     func() time.Time
	newID   func() string
}

// New builds the service with the real clock and UUIDv7 identifiers.
func New(store Store) *Service {
	return &Service{
		store: store,
		now:   func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
		newID: func() string {
			id, err := uuid.NewV7()
			if err != nil {
				panic(fmt.Sprintf("uuidv7: %v", err)) // entropy source failure; cannot continue safely
			}
			return id.String()
		},
	}
}

// WithClock overrides the clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service { s.now = now; return s }

// WithIDGenerator overrides id generation (tests).
func (s *Service) WithIDGenerator(f func() string) *Service { s.newID = f; return s }

// WithPeriodHistory installs the REF-05 client. Without one, activating any
// version that is not the first of its calendar fails closed.
func (s *Service) WithPeriodHistory(c PeriodHistoryClient) *Service { s.history = c; return s }

// Meta is the trusted request context shared by every command.
type Meta struct {
	Actor          string
	TenantID       string
	LegalEntityID  string
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
	if m.LegalEntityID == "" {
		return domain.Errf(domain.CodeContextInvalid, "legal entity context is required")
	}
	if m.IdempotencyKey == "" {
		return domain.Errf(domain.CodeContextInvalid, "Idempotency-Key is required")
	}
	if m.Reason == "" {
		return domain.Errf(domain.CodeContextInvalid, "reason is required")
	}
	return nil
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
	if rec != nil {
		if rec.RequestHash != m.RequestHash || rec.Operation != op {
			return nil, false, domain.Errf(domain.CodeContextInvalid, "Idempotency-Key was already used for a different request")
		}
		var out T
		if err := json.Unmarshal(rec.Response, &out); err != nil {
			return nil, false, fmt.Errorf("decode stored idempotent response: %w", err)
		}
		return &out, true, nil
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

// run executes fn inside one idempotent transaction.
func run[T any](ctx context.Context, s *Service, m Meta, op string, fn func(tx Tx) (*T, error)) (*T, bool, error) {
	var res *T
	var replayed bool
	err := s.store.InTx(ctx, m.TenantID, func(tx Tx) error {
		out, rep, err := idempotent(ctx, tx, m, op, func() (*T, error) { return fn(tx) })
		if err != nil {
			return err
		}
		res, replayed = out, rep
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return res, replayed, nil
}

func (s *Service) enqueue(ctx context.Context, tx Tx, ev events.Event) error {
	key, body, err := events.Build(ev)
	if err != nil {
		return err
	}
	return tx.Enqueue(ctx, OutboxEntry{TenantID: ev.TenantID, ObjectID: key, EventType: ev.Type, Payload: body})
}

// midnightUTC is a date as an instant, for event effective_at.
func midnightUTC(d domain.Date) time.Time { return d.Time.UTC() }

// requireEntity refuses a command whose trusted legal-entity context does not
// match the object it targets.
func requireEntity(m Meta, objectEntity string) error {
	if m.LegalEntityID != objectEntity {
		return domain.Errf(domain.CodeContextInvalid, "legal entity context does not match the target object's legal entity")
	}
	return nil
}
