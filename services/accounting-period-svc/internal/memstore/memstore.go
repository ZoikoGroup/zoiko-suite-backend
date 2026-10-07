// Package memstore is an in-memory service.Store used by tests. It is NOT a
// production store: it exists so the real business rules in internal/service
// (and the HTTP handlers over them) can be exercised without Postgres. A
// transaction works on a private copy and is published only if the callback
// returns nil, so a failure part-way (for example a failed outbox write) rolls
// the whole operation back exactly as the Postgres store does.
//
// What it does NOT model: row-level security, database constraints, triggers.
// Tenant scoping is modelled by filtering every read on the Tx's tenant.
// Those database guarantees are covered only by the (skipped-without-DB)
// Postgres suite.
package memstore

import (
	"context"
	"errors"
	"sort"
	"sync"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/service"
)

type idemKey struct{ tenant, key string }

type data struct {
	periods []domain.Period
	history []domain.HistoryEntry
	idem    map[idemKey]service.IdempotencyRecord
	outbox  []service.OutboxEntry
}

func (d *data) clone() *data {
	n := &data{
		periods: append([]domain.Period(nil), d.periods...),
		history: append([]domain.HistoryEntry(nil), d.history...),
		idem:    map[idemKey]service.IdempotencyRecord{},
		outbox:  append([]service.OutboxEntry(nil), d.outbox...),
	}
	for k, v := range d.idem {
		n.idem[k] = v
	}
	return n
}

// Store is the in-memory store.
type Store struct {
	mu sync.Mutex
	d  *data

	// FailEnqueue makes the next outbox write fail (to prove atomicity).
	FailEnqueue bool
}

// New returns an empty store.
func New() *Store { return &Store{d: (&data{}).clone()} }

// InTx implements service.Store.
func (s *Store) InTx(_ context.Context, tenantID string, fn func(service.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work := s.d.clone()
	if err := fn(&tx{s: s, d: work, tenant: tenantID}); err != nil {
		return err
	}
	s.d = work
	return nil
}

// Outbox returns the committed outbox entries.
func (s *Store) Outbox() []service.OutboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]service.OutboxEntry(nil), s.d.outbox...)
}

// Periods returns all committed period rows (every tenant).
func (s *Store) Periods() []domain.Period {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Period(nil), s.d.periods...)
}

// History returns all committed history rows (every tenant).
func (s *Store) History() []domain.HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.HistoryEntry(nil), s.d.history...)
}

type tx struct {
	s      *Store
	d      *data
	tenant string
}

func (t *tx) LockKey(context.Context, string) error { return nil } // the Store mutex serialises transactions

func (t *tx) GetIdempotency(_ context.Context, key string) (*service.IdempotencyRecord, error) {
	r, ok := t.d.idem[idemKey{t.tenant, key}]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (t *tx) PutIdempotency(_ context.Context, rec service.IdempotencyRecord) error {
	k := idemKey{t.tenant, rec.Key}
	if _, dup := t.d.idem[k]; dup {
		return errors.New("memstore: duplicate idempotency key")
	}
	t.d.idem[k] = rec
	return nil
}

func (t *tx) index(id string) int {
	for i := range t.d.periods {
		if t.d.periods[i].TenantID == t.tenant && t.d.periods[i].PeriodID == id {
			return i
		}
	}
	return -1
}

func clonePeriod(p domain.Period) *domain.Period {
	if p.Reopen != nil {
		w := *p.Reopen
		p.Reopen = &w
	}
	return &p
}

func (t *tx) GetPeriod(_ context.Context, id string, _ bool) (*domain.Period, error) {
	if i := t.index(id); i >= 0 {
		return clonePeriod(t.d.periods[i]), nil
	}
	return nil, nil
}

func (t *tx) FindPeriodByKey(_ context.Context, entity, versionID, book, module, key string) (*domain.Period, error) {
	for _, p := range t.d.periods {
		if p.TenantID == t.tenant && p.LegalEntityID == entity && p.CalendarVersionID == versionID &&
			p.BookScope == book && p.ModuleScope == module && p.PeriodKey == key {
			return clonePeriod(p), nil
		}
	}
	return nil, nil
}

func (t *tx) filter(pred func(domain.Period) bool) []domain.Period {
	var out []domain.Period
	for _, p := range t.d.periods {
		if p.TenantID == t.tenant && pred(p) {
			out = append(out, *clonePeriod(p))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartDate != out[j].StartDate {
			return out[i].StartDate < out[j].StartDate
		}
		return out[i].PeriodID < out[j].PeriodID
	})
	return out
}

func (t *tx) ListPeriodsByKey(_ context.Context, entity, key string) ([]domain.Period, error) {
	return t.filter(func(p domain.Period) bool { return p.LegalEntityID == entity && p.PeriodKey == key }), nil
}

func (t *tx) FindPeriodsCovering(_ context.Context, entity, date string) ([]domain.Period, error) {
	return t.filter(func(p domain.Period) bool {
		return p.LegalEntityID == entity && p.StartDate <= date && date <= p.EndDate
	}), nil
}

func (t *tx) ListPeriods(_ context.Context, entity, state string, limit, offset int) ([]domain.Period, error) {
	all := t.filter(func(p domain.Period) bool {
		return p.LegalEntityID == entity && (state == "" || string(p.State) == state)
	})
	if offset > len(all) {
		offset = len(all)
	}
	all = all[offset:]
	if limit > 0 && limit < len(all) {
		all = all[:limit]
	}
	return all, nil
}

func (t *tx) InsertPeriod(_ context.Context, p *domain.Period) error {
	for _, e := range t.d.periods {
		if e.TenantID == p.TenantID && e.LegalEntityID == p.LegalEntityID && e.CalendarVersionID == p.CalendarVersionID &&
			e.BookScope == p.BookScope && e.ModuleScope == p.ModuleScope && e.PeriodKey == p.PeriodKey {
			return errors.New("memstore: duplicate period")
		}
	}
	cp := *clonePeriod(*p)
	cp.TenantID = t.tenant
	t.d.periods = append(t.d.periods, cp)
	return nil
}

func (t *tx) UpdatePeriodState(_ context.Context, p *domain.Period, expectedVersion int64) error {
	i := t.index(p.PeriodID)
	if i < 0 || t.d.periods[i].Version != expectedVersion {
		return domain.ErrVersionConflictStore
	}
	cur := &t.d.periods[i]
	cur.State, cur.Version, cur.UpdatedAt = p.State, p.Version, p.UpdatedAt
	cur.Reopen = nil
	if p.Reopen != nil {
		w := *p.Reopen
		cur.Reopen = &w
	}
	return nil
}

func (t *tx) InsertHistory(_ context.Context, e *domain.HistoryEntry) error {
	cp := *e
	cp.TenantID = t.tenant
	t.d.history = append(t.d.history, cp)
	return nil
}

func (t *tx) ListHistory(_ context.Context, periodID string) ([]domain.HistoryEntry, error) {
	var out []domain.HistoryEntry
	for _, h := range t.d.history {
		if h.TenantID == t.tenant && h.PeriodID == periodID {
			out = append(out, h)
		}
	}
	return out, nil
}

func (t *tx) CalendarUsage(_ context.Context, calendarID, versionID string) (*string, bool, error) {
	var latest *string
	anyNotOpen := false
	for _, p := range t.d.periods {
		if p.TenantID != t.tenant || p.CalendarID != calendarID || (versionID != "" && p.CalendarVersionID != versionID) {
			continue
		}
		if latest == nil || p.EndDate > *latest {
			e := p.EndDate
			latest = &e
		}
		if p.State != domain.StateOpen {
			anyNotOpen = true
		}
	}
	return latest, anyNotOpen, nil
}

func (t *tx) Enqueue(_ context.Context, e service.OutboxEntry) error {
	if t.s.FailEnqueue {
		return errors.New("memstore: outbox write failed")
	}
	t.d.outbox = append(t.d.outbox, e)
	return nil
}
