// Package service holds REF-02's business rules. It talks to storage only
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

	"zoiko.io/currency-registry-svc/internal/domain"
	"zoiko.io/currency-registry-svc/internal/events"
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

// Tx is one storage transaction. "Absent" is reported as (nil, nil).
type Tx interface {
	// LockKey serialises concurrent work on a logical key for the Tx's duration.
	LockKey(ctx context.Context, key string) error

	GetIdempotency(ctx context.Context, key string) (*IdempotencyRecord, error)
	PutIdempotency(ctx context.Context, rec IdempotencyRecord) error

	GetCurrencyByID(ctx context.Context, id string, forUpdate bool) (*domain.Currency, error)
	// FindCurrenciesByAlpha/Numeric return every currency (retired included) with the code, valid_from ascending.
	FindCurrenciesByAlpha(ctx context.Context, alpha string) ([]domain.Currency, error)
	FindCurrenciesByNumeric(ctx context.Context, numeric string) ([]domain.Currency, error)
	ListCurrencies(ctx context.Context, status string, limit, offset int) ([]domain.Currency, error)
	InsertCurrency(ctx context.Context, c *domain.Currency) error
	// UpdateCurrency writes status/name/flag/valid_to/version/last_import_* where version = expectedVersion,
	// returning domain.ErrVersionConflictStore when no row matched.
	UpdateCurrency(ctx context.Context, c *domain.Currency, expectedVersion int64) error

	InsertMinorUnitVersion(ctx context.Context, mv *domain.MinorUnitVersion) error
	// ListMinorUnitVersions returns raw versions, valid_from ascending (ValidTo is NOT populated).
	ListMinorUnitVersions(ctx context.Context, currencyID string) ([]domain.MinorUnitVersion, error)

	InsertStatusHistory(ctx context.Context, e *domain.StatusHistoryEntry) error

	GetImportBySourceVersion(ctx context.Context, source, version string) (*domain.Import, error)
	InsertImport(ctx context.Context, imp *domain.Import) error

	GetTenantSupport(ctx context.Context, tenantID, currencyID string) (*domain.TenantSupport, error)
	UpsertTenantSupport(ctx context.Context, ts *domain.TenantSupport, expectedVersion int64) error
	ListTenantSupport(ctx context.Context, tenantID string) ([]domain.TenantSupport, error)

	Enqueue(ctx context.Context, e OutboxEntry) error
}

// Store opens transactions. tenantID is the actor's tenant context (installed
// as app.tenant_id for row-level security on tenant-scoped tables); it may be
// empty for reads of global data.
type Store interface {
	InTx(ctx context.Context, tenantID string, fn func(Tx) error) error
}

// Service implements REF-02.
type Service struct {
	store Store
	now   func() time.Time
	newID func() string
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

func (s *Service) enqueue(ctx context.Context, tx Tx, ev events.Event) error {
	key, body, err := events.Build(ev)
	if err != nil {
		return err
	}
	return tx.Enqueue(ctx, OutboxEntry{TenantID: ev.TenantID, ObjectID: key, EventType: ev.Type, Payload: body})
}

// pickCurrent chooses the registry row that represents "the currency with this
// code right now": the non-retired one if any, else the latest retired one.
func pickCurrent(cs []domain.Currency) *domain.Currency {
	var best *domain.Currency
	for i := range cs {
		c := &cs[i]
		if c.Status != domain.StatusRetired {
			return c
		}
		if best == nil || c.ValidFrom.After(best.ValidFrom) {
			best = c
		}
	}
	return best
}

// pickAsOf chooses the row whose valid_from is the latest not after t.
func pickAsOf(cs []domain.Currency, t time.Time) *domain.Currency {
	var best *domain.Currency
	for i := range cs {
		c := &cs[i]
		if c.ValidFrom.After(t) {
			continue
		}
		if best == nil || c.ValidFrom.After(best.ValidFrom) {
			best = c
		}
	}
	return best
}

func nonRetired(cs []domain.Currency) *domain.Currency {
	for i := range cs {
		if cs[i].Status != domain.StatusRetired {
			return &cs[i]
		}
	}
	return nil
}

// withDerivedValidTo fills ValidTo: the next version's valid_from, or the
// currency's own valid_to for the last version.
func withDerivedValidTo(vs []domain.MinorUnitVersion, currencyValidTo *time.Time) []domain.MinorUnitVersion {
	out := make([]domain.MinorUnitVersion, len(vs))
	copy(out, vs)
	for i := range out {
		if i+1 < len(out) {
			t := out[i+1].ValidFrom
			out[i].ValidTo = &t
		} else if currencyValidTo != nil {
			t := *currencyValidTo
			out[i].ValidTo = &t
		} else {
			out[i].ValidTo = nil
		}
	}
	return out
}

// minorUnitAt returns the version in force at t: the one with the latest
// valid_from not after t, or nil. After a retirement the last version is still returned (history stays readable).
func minorUnitAt(vs []domain.MinorUnitVersion, t time.Time) *domain.MinorUnitVersion {
	var best *domain.MinorUnitVersion
	for i := range vs {
		v := &vs[i]
		if v.ValidFrom.After(t) {
			continue
		}
		if best == nil || v.ValidFrom.After(best.ValidFrom) {
			best = v
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

func (s *Service) attachMinorUnit(ctx context.Context, tx Tx, c *domain.Currency, t time.Time) error {
	vs, err := tx.ListMinorUnitVersions(ctx, c.CurrencyID)
	if err != nil {
		return err
	}
	vs = withDerivedValidTo(vs, c.ValidTo)
	c.MinorUnit = minorUnitAt(vs, t)
	return nil
}
