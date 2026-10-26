package inbound

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// MemRepo is an in-memory Repo for unit and handler tests. It enforces tenant isolation, the unique dedupe key per
// tenant, the platform-only write of targets, the version bump and the one-live-target-per-contract rule, and it
// records the outbox events it is given. The Postgres tests prove the database holds the rules itself.
type MemRepo struct {
	mu       sync.Mutex
	ex       map[string]*Exchange
	events   map[string][]Event
	reqs     map[string][]QRequest
	targets  map[string]*Target
	Out      []OutEvent
	Signals  []OutEvent
	FailNext bool // makes the next Insert fail, to test storage outages
}

// NewMemRepo returns an empty MemRepo.
func NewMemRepo() *MemRepo {
	return &MemRepo{ex: map[string]*Exchange{}, events: map[string][]Event{}, reqs: map[string][]QRequest{}, targets: map[string]*Target{}}
}

func cloneEx(e *Exchange) *Exchange {
	c := *e
	c.Raw = append([]byte(nil), e.Raw...)
	c.Canonical = append([]byte(nil), e.Canonical...)
	if e.Raw == nil {
		c.Raw = nil
	}
	if e.Canonical == nil {
		c.Canonical = nil
	}
	c.RawRetained = len(c.Raw) > 0
	return &c
}

func (m *MemRepo) owned(ctx context.Context, id string) (*Exchange, bool) {
	e, ok := m.ex[id]
	if !ok || e.TenantID != middleware.GetTenantID(ctx) {
		return nil, false
	}
	return e, true
}

func (m *MemRepo) Insert(ctx context.Context, e *Exchange, ev Event, out []OutEvent) (*Exchange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext {
		m.FailNext = false
		return nil, context.DeadlineExceeded
	}
	if e.TenantID != middleware.GetTenantID(ctx) {
		return nil, ErrNotFound
	}
	if e.DedupeKey != "" {
		for _, x := range m.ex {
			if x.TenantID == e.TenantID && x.DedupeKey == e.DedupeKey {
				return cloneEx(x), ErrDedupe
			}
		}
	}
	m.ex[e.ExchangeID] = cloneEx(e)
	ev.At = e.ReceivedAt
	m.events[e.ExchangeID] = append(m.events[e.ExchangeID], ev)
	m.Out = append(m.Out, out...)
	return nil, nil
}

func (m *MemRepo) InsertDuplicate(ctx context.Context, e *Exchange, ev Event, out []OutEvent) error {
	_, err := m.Insert(ctx, e, ev, out)
	return err
}

func (m *MemRepo) Get(ctx context.Context, id string) (*Exchange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.owned(ctx, id)
	if !ok {
		return nil, ErrNotFound
	}
	return cloneEx(e), nil
}

func (m *MemRepo) GetByDedupe(ctx context.Context, tenant, key string) (*Exchange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.ex {
		if x.TenantID == tenant && x.DedupeKey == key && tenant == middleware.GetTenantID(ctx) {
			return cloneEx(x), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemRepo) List(ctx context.Context, f Filter) ([]Exchange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Exchange{}
	for _, e := range m.ex {
		if e.TenantID != middleware.GetTenantID(ctx) {
			continue
		}
		if (f.State != "" && e.State != f.State) || (f.ConnectionID != "" && e.ConnectionID != f.ConnectionID) || (f.Class != "" && e.QuarantineClass != f.Class) {
			continue
		}
		out = append(out, *cloneEx(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.Before(out[j].ReceivedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}

func (m *MemRepo) Events(ctx context.Context, id string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, id); !ok {
		return nil, ErrNotFound
	}
	return append([]Event{}, m.events[id]...), nil
}

func (m *MemRepo) Requests(ctx context.Context, id string) ([]QRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, id); !ok {
		return nil, ErrNotFound
	}
	return append([]QRequest{}, m.reqs[id]...), nil
}

func (m *MemRepo) Mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Exchange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.owned(ctx, id)
	if !ok {
		return nil, ErrNotFound
	}
	work := cloneEx(cur)
	tx := &Tx{E: work, Requests: append([]QRequest{}, m.reqs[id]...)}
	if err := fn(tx); err != nil {
		return nil, err
	}
	work.Version = cur.Version + 1
	work.UpdatedAt = time.Now().UTC()
	m.ex[id] = work
	if tx.NewEvent != nil {
		m.events[id] = append(m.events[id], *tx.NewEvent)
	}
	if tx.NewRequest != nil {
		m.reqs[id] = append(m.reqs[id], *tx.NewRequest)
	}
	if tx.UpdateRequest != nil {
		for i := range m.reqs[id] {
			if m.reqs[id][i].RequestID == tx.UpdateRequest.RequestID {
				m.reqs[id][i] = *tx.UpdateRequest
			}
		}
	}
	m.Out = append(m.Out, tx.Out...)
	return cloneEx(work), nil
}

func (m *MemRepo) Due(ctx context.Context, now time.Time, limit int) ([]DueRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DueRef
	for _, e := range m.ex {
		if e.State == StateValidated && e.NextAttemptAt != nil && !e.NextAttemptAt.After(now) {
			out = append(out, DueRef{TenantID: e.TenantID, ExchangeID: e.ExchangeID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExchangeID < out[j].ExchangeID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemRepo) PurgeExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.ex {
		if e.RawExpiresAt != nil && !e.RawExpiresAt.After(now) && (e.Raw != nil || e.Canonical != nil) {
			e.Raw, e.Canonical = nil, nil
			n++
			if limit > 0 && n >= limit {
				break
			}
		}
	}
	return n, nil
}

func (m *MemRepo) CreateTarget(ctx context.Context, t *Target, ev OutEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if middleware.GetTenantID(ctx) != platformScope {
		return ErrNotPlatform
	}
	for _, x := range m.targets {
		if x.CanonicalContract == t.CanonicalContract && x.Status != TargetRetired {
			return ErrTargetExists
		}
	}
	c := *t
	m.targets[t.TargetID] = &c
	m.Out = append(m.Out, ev)
	return nil
}

func (m *MemRepo) GetTarget(ctx context.Context, id string) (*Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[id]
	if !ok {
		return nil, ErrTargetNotFound
	}
	c := *t
	return &c, nil
}

func (m *MemRepo) ActiveTarget(ctx context.Context, canonicalContract string) (*Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.targets {
		if t.CanonicalContract == canonicalContract && t.Status == TargetActive {
			c := *t
			return &c, nil
		}
	}
	return nil, nil
}

func (m *MemRepo) ListTargets(ctx context.Context) ([]Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Target{}
	for _, t := range m.targets {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CanonicalContract < out[j].CanonicalContract })
	return out, nil
}

func (m *MemRepo) MutateTarget(ctx context.Context, id string, fn func(t *Target) (*OutEvent, error)) (*Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if middleware.GetTenantID(ctx) != platformScope {
		return nil, ErrNotPlatform
	}
	cur, ok := m.targets[id]
	if !ok {
		return nil, ErrTargetNotFound
	}
	work := *cur
	ev, err := fn(&work)
	if err != nil {
		return nil, err
	}
	work.Version = cur.Version + 1
	m.targets[id] = &work
	if ev != nil {
		m.Out = append(m.Out, *ev)
	}
	c := work
	return &c, nil
}

func (m *MemRepo) RecordSignal(ctx context.Context, tenant string, ev OutEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Signals = append(m.Signals, ev)
	return nil
}

// Types returns the outbox event types recorded so far.
func (m *MemRepo) Types() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.Out))
	for _, e := range m.Out {
		out = append(out, e.Type)
	}
	return out
}

var _ Repo = (*MemRepo)(nil)
var _ = json.Marshal

// Summaries implements Repo.
func (m *MemRepo) Summaries(ctx context.Context, ids []string) (map[string]ExchangeSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]ExchangeSummary{}
	for _, id := range ids {
		if e, ok := m.owned(ctx, id); ok {
			out[id] = ExchangeSummary{State: e.State, DomainOutcome: e.DomainOutcome, QuarantineClass: e.QuarantineClass, ReasonCode: e.ReasonCode, CanonicalHash: e.CanonicalHash}
		}
	}
	return out, nil
}

// Held implements Repo.
func (m *MemRepo) Held(ctx context.Context, connectionID, operationID string, from, to time.Time, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var hit []*Exchange
	for _, e := range m.ex {
		if e.TenantID != middleware.GetTenantID(ctx) || e.ConnectionID != connectionID || e.OperationID != operationID || e.State != StateQuarantined ||
			e.ReceivedAt.Before(from) || e.ReceivedAt.After(to) {
			continue
		}
		hit = append(hit, e)
	}
	sort.Slice(hit, func(i, j int) bool {
		if !hit[i].ReceivedAt.Equal(hit[j].ReceivedAt) {
			return hit[i].ReceivedAt.Before(hit[j].ReceivedAt)
		}
		return hit[i].ExchangeID < hit[j].ExchangeID
	})
	var out []string
	for _, e := range hit {
		out = append(out, e.ExchangeID)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
