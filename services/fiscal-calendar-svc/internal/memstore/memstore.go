// Package memstore is an in-memory service.Store used by tests. It is NOT a
// production store: it exists so the real business rules in internal/service
// (and the HTTP handlers over them) can be exercised without Postgres. A
// transaction works on a private copy and is published only if the callback
// returns nil, so a failure part-way (for example a failed outbox write) rolls
// the whole operation back exactly as the Postgres store does. Every read and
// write is filtered by the transaction's tenant, mirroring row-level security.
//
// What it does NOT model: row-level security itself, database constraints
// (including the effective-interval exclusion constraint) and triggers. Those
// are covered only by the (skipped-without-DB) Postgres suite.
package memstore

import (
	"context"
	"errors"
	"sort"
	"sync"

	"zoiko.io/fiscal-calendar-svc/internal/domain"
	"zoiko.io/fiscal-calendar-svc/internal/service"
)

type idemKey struct{ tenant, key string }

type data struct {
	calendars []domain.FiscalCalendar
	versions  []domain.FiscalCalendarVersion
	history   []domain.StatusHistoryEntry
	historyT  []string // tenant of each history row
	plans     []domain.CalendarTransitionPlan
	idem      map[idemKey]service.IdempotencyRecord
	outbox    []service.OutboxEntry
}

func (d *data) clone() *data {
	n := &data{
		calendars: append([]domain.FiscalCalendar(nil), d.calendars...),
		versions:  append([]domain.FiscalCalendarVersion(nil), d.versions...),
		history:   append([]domain.StatusHistoryEntry(nil), d.history...),
		historyT:  append([]string(nil), d.historyT...),
		plans:     append([]domain.CalendarTransitionPlan(nil), d.plans...),
		idem:      map[idemKey]service.IdempotencyRecord{},
		outbox:    append([]service.OutboxEntry(nil), d.outbox...),
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

// ── inspection helpers for tests (unfiltered by tenant) ──────────────────────

// Outbox returns the committed outbox entries.
func (s *Store) Outbox() []service.OutboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]service.OutboxEntry(nil), s.d.outbox...)
}

// Calendars returns all committed calendar rows.
func (s *Store) Calendars() []domain.FiscalCalendar {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.FiscalCalendar(nil), s.d.calendars...)
}

// Versions returns all committed version rows.
func (s *Store) Versions() []domain.FiscalCalendarVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.FiscalCalendarVersion(nil), s.d.versions...)
}

// History returns all committed status-history rows.
func (s *Store) History() []domain.StatusHistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.StatusHistoryEntry(nil), s.d.history...)
}

// Plans returns all committed plan rows.
func (s *Store) Plans() []domain.CalendarTransitionPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.CalendarTransitionPlan(nil), s.d.plans...)
}

type tx struct {
	s      *Store
	d      *data
	tenant string
}

func (t *tx) LockKey(context.Context, string) error { return nil } // the store mutex serialises

func (t *tx) GetIdempotency(_ context.Context, key string) (*service.IdempotencyRecord, error) {
	r, ok := t.d.idem[idemKey{t.tenant, key}]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (t *tx) PutIdempotency(_ context.Context, rec service.IdempotencyRecord) error {
	t.d.idem[idemKey{t.tenant, rec.Key}] = rec
	return nil
}

// ── calendars ────────────────────────────────────────────────────────────────

func (t *tx) GetCalendarByID(_ context.Context, id string, _ bool) (*domain.FiscalCalendar, error) {
	for _, c := range t.d.calendars {
		if c.TenantID == t.tenant && c.CalendarID == id {
			cp := c
			return &cp, nil
		}
	}
	return nil, nil
}

func (t *tx) FindCalendarByCode(_ context.Context, legalEntityID, code string) (*domain.FiscalCalendar, error) {
	for _, c := range t.d.calendars {
		if c.TenantID == t.tenant && c.LegalEntityID == legalEntityID && c.Code == code {
			cp := c
			return &cp, nil
		}
	}
	return nil, nil
}

func (t *tx) InsertCalendar(_ context.Context, c *domain.FiscalCalendar) error {
	if c.TenantID != t.tenant {
		return errors.New("memstore: row tenant differs from transaction tenant (RLS WITH CHECK)")
	}
	for _, e := range t.d.calendars {
		if e.TenantID == c.TenantID && e.LegalEntityID == c.LegalEntityID && e.Code == c.Code {
			return domain.Errf(domain.CodeDuplicateCandidate, "calendar code already exists for this legal entity")
		}
	}
	t.d.calendars = append(t.d.calendars, *c)
	return nil
}

func (t *tx) UpdateCalendar(_ context.Context, c *domain.FiscalCalendar, expectedVersion int64) error {
	for i, e := range t.d.calendars {
		if e.TenantID != t.tenant || e.CalendarID != c.CalendarID {
			continue
		}
		if e.Version != expectedVersion {
			return domain.ErrVersionConflictStore
		}
		t.d.calendars[i] = *c
		return nil
	}
	return domain.ErrVersionConflictStore
}

// ── versions ─────────────────────────────────────────────────────────────────

func (t *tx) GetVersionByID(_ context.Context, id string, _ bool) (*domain.FiscalCalendarVersion, error) {
	for _, v := range t.d.versions {
		if v.TenantID == t.tenant && v.VersionID == id {
			cp := v
			return &cp, nil
		}
	}
	return nil, nil
}

func (t *tx) filterVersions(match func(domain.FiscalCalendarVersion) bool) []domain.FiscalCalendarVersion {
	var out []domain.FiscalCalendarVersion
	for _, v := range t.d.versions {
		if v.TenantID == t.tenant && match(v) {
			out = append(out, v)
		}
	}
	return out
}

func (t *tx) ListVersions(_ context.Context, calendarID string) ([]domain.FiscalCalendarVersion, error) {
	out := t.filterVersions(func(v domain.FiscalCalendarVersion) bool { return v.CalendarID == calendarID })
	sort.SliceStable(out, func(i, j int) bool { return out[i].VersionNo < out[j].VersionNo })
	return out, nil
}

func (t *tx) ListInForceVersions(_ context.Context, legalEntityID, scope string) ([]domain.FiscalCalendarVersion, error) {
	out := t.filterVersions(func(v domain.FiscalCalendarVersion) bool {
		return v.LegalEntityID == legalEntityID && v.Scope == scope && v.Status.InForce()
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].EffectiveFrom.Before(out[j].EffectiveFrom) })
	return out, nil
}

func (t *tx) InsertVersion(_ context.Context, v *domain.FiscalCalendarVersion) error {
	if v.TenantID != t.tenant {
		return errors.New("memstore: row tenant differs from transaction tenant (RLS WITH CHECK)")
	}
	for _, e := range t.d.versions {
		if e.TenantID == v.TenantID && e.CalendarID == v.CalendarID && e.VersionNo == v.VersionNo {
			return errors.New("memstore: duplicate (calendar_id, version_no)")
		}
	}
	t.d.versions = append(t.d.versions, *v)
	return nil
}

// UpdateVersion writes ONLY the lifecycle columns, as the Postgres store does:
// pattern, start anchor and effective_from are never taken from the argument.
func (t *tx) UpdateVersion(_ context.Context, v *domain.FiscalCalendarVersion, expectedVersion int64) error {
	for i, e := range t.d.versions {
		if e.TenantID != t.tenant || e.VersionID != v.VersionID {
			continue
		}
		if e.Version != expectedVersion {
			return domain.ErrVersionConflictStore
		}
		e.Status = v.Status
		e.EffectiveTo = v.EffectiveTo
		e.ApprovedBy, e.ApprovalReason, e.ApprovedAt = v.ApprovedBy, v.ApprovalReason, v.ApprovedAt
		e.ActivatedBy, e.ActivatedAt = v.ActivatedBy, v.ActivatedAt
		e.SupersededByVersion = v.SupersededByVersion
		e.Version, e.RecordedAt = v.Version, v.RecordedAt
		t.d.versions[i] = e
		return nil
	}
	return domain.ErrVersionConflictStore
}

func (t *tx) InsertStatusHistory(_ context.Context, e *domain.StatusHistoryEntry) error {
	t.d.history = append(t.d.history, *e)
	t.d.historyT = append(t.d.historyT, t.tenant)
	return nil
}

// ── plans ────────────────────────────────────────────────────────────────────

func (t *tx) GetPlanByID(_ context.Context, id string, _ bool) (*domain.CalendarTransitionPlan, error) {
	for _, p := range t.d.plans {
		if p.TenantID == t.tenant && p.PlanID == id {
			cp := p
			return &cp, nil
		}
	}
	return nil, nil
}

func (t *tx) ListPlansForVersion(_ context.Context, toVersionID string) ([]domain.CalendarTransitionPlan, error) {
	var out []domain.CalendarTransitionPlan
	for _, p := range t.d.plans {
		if p.TenantID == t.tenant && p.ToVersionID == toVersionID {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (t *tx) InsertPlan(_ context.Context, p *domain.CalendarTransitionPlan) error {
	if p.TenantID != t.tenant {
		return errors.New("memstore: row tenant differs from transaction tenant (RLS WITH CHECK)")
	}
	t.d.plans = append(t.d.plans, *p)
	return nil
}

func (t *tx) UpdatePlan(_ context.Context, p *domain.CalendarTransitionPlan, expectedVersion int64) error {
	for i, e := range t.d.plans {
		if e.TenantID != t.tenant || e.PlanID != p.PlanID {
			continue
		}
		if e.Version != expectedVersion {
			return domain.ErrVersionConflictStore
		}
		e.Status, e.DecidedBy, e.DecisionReason, e.DecidedAt, e.Version = p.Status, p.DecidedBy, p.DecisionReason, p.DecidedAt, p.Version
		t.d.plans[i] = e
		return nil
	}
	return domain.ErrVersionConflictStore
}

func (t *tx) Enqueue(_ context.Context, e service.OutboxEntry) error {
	if t.s.FailEnqueue {
		return errors.New("memstore: injected outbox failure")
	}
	t.d.outbox = append(t.d.outbox, e)
	return nil
}
