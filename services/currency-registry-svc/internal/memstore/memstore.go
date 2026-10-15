// Package memstore is an in-memory service.Store used by tests. It is NOT a
// production store: it exists so the real business rules in internal/service
// (and the HTTP handlers over them) can be exercised without Postgres. A
// transaction works on a private copy and is published only if the callback
// returns nil, so a failure part-way (for example a failed outbox write) rolls
// the whole operation back exactly as the Postgres store does.
//
// What it does NOT model: row-level security, database constraints, triggers.
// Those are covered only by the (skipped-without-DB) Postgres suite.
package memstore

import (
	"context"
	"errors"
	"sort"
	"sync"

	"zoiko.io/currency-registry-svc/internal/domain"
	"zoiko.io/currency-registry-svc/internal/service"
)

type idemKey struct{ tenant, key string }
type overlayKey struct{ tenant, currencyID string }

type data struct {
	currencies []domain.Currency
	mus        []domain.MinorUnitVersion
	history    []domain.StatusHistoryEntry
	imports    []domain.Import
	overlay    map[overlayKey]domain.TenantSupport
	idem       map[idemKey]service.IdempotencyRecord
	outbox     []service.OutboxEntry
}

func (d *data) clone() *data {
	n := &data{
		currencies: append([]domain.Currency(nil), d.currencies...),
		mus:        append([]domain.MinorUnitVersion(nil), d.mus...),
		history:    append([]domain.StatusHistoryEntry(nil), d.history...),
		imports:    append([]domain.Import(nil), d.imports...),
		overlay:    map[overlayKey]domain.TenantSupport{},
		idem:       map[idemKey]service.IdempotencyRecord{},
		outbox:     append([]service.OutboxEntry(nil), d.outbox...),
	}
	for k, v := range d.overlay {
		n.overlay[k] = v
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
func New() *Store {
	return &Store{d: (&data{}).clone()}
}

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

// ── inspection helpers for tests ─────────────────────────────────────────────

// Outbox returns the committed outbox entries.
func (s *Store) Outbox() []service.OutboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]service.OutboxEntry(nil), s.d.outbox...)
}

// Currencies returns all committed currency rows.
func (s *Store) Currencies() []domain.Currency {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Currency(nil), s.d.currencies...)
}

// MinorUnitRows returns all committed minor-unit version rows.
func (s *Store) MinorUnitRows() []domain.MinorUnitVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.MinorUnitVersion(nil), s.d.mus...)
}

// Imports returns all committed import rows.
func (s *Store) Imports() []domain.Import {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Import(nil), s.d.imports...)
}

// History returns all committed status-history rows.
func (s *Store) History() []domain.StatusHistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.StatusHistoryEntry(nil), s.d.history...)
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

func (t *tx) GetCurrencyByID(_ context.Context, id string, _ bool) (*domain.Currency, error) {
	for _, c := range t.d.currencies {
		if c.CurrencyID == id {
			cp := c
			return &cp, nil
		}
	}
	return nil, nil
}

func (t *tx) find(match func(domain.Currency) bool) []domain.Currency {
	var out []domain.Currency
	for _, c := range t.d.currencies {
		if match(c) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ValidFrom.Before(out[j].ValidFrom) })
	return out
}

func (t *tx) FindCurrenciesByAlpha(_ context.Context, alpha string) ([]domain.Currency, error) {
	return t.find(func(c domain.Currency) bool { return c.AlphaCode == alpha }), nil
}

func (t *tx) FindCurrenciesByNumeric(_ context.Context, numeric string) ([]domain.Currency, error) {
	return t.find(func(c domain.Currency) bool { return c.NumericCode == numeric }), nil
}

func (t *tx) ListCurrencies(_ context.Context, status string, limit, offset int) ([]domain.Currency, error) {
	out := t.find(func(c domain.Currency) bool { return status == "" || string(c.Status) == status })
	sort.SliceStable(out, func(i, j int) bool { return out[i].AlphaCode < out[j].AlphaCode })
	if offset > len(out) {
		offset = len(out)
	}
	out = out[offset:]
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

func (t *tx) InsertCurrency(_ context.Context, c *domain.Currency) error {
	for _, e := range t.d.currencies {
		if e.Status != domain.StatusRetired && c.Status != domain.StatusRetired &&
			(e.AlphaCode == c.AlphaCode || e.NumericCode == c.NumericCode) {
			return errors.New("memstore: unique violation on active alpha/numeric code")
		}
	}
	t.d.currencies = append(t.d.currencies, *c)
	return nil
}

func (t *tx) UpdateCurrency(_ context.Context, c *domain.Currency, expectedVersion int64) error {
	for i, e := range t.d.currencies {
		if e.CurrencyID != c.CurrencyID {
			continue
		}
		if e.Version != expectedVersion {
			return domain.ErrVersionConflictStore
		}
		cp := *c
		cp.MinorUnit = nil
		t.d.currencies[i] = cp
		return nil
	}
	return domain.ErrVersionConflictStore
}

func (t *tx) InsertMinorUnitVersion(_ context.Context, mv *domain.MinorUnitVersion) error {
	for _, e := range t.d.mus {
		if e.CurrencyID == mv.CurrencyID && e.ValidFrom.Equal(mv.ValidFrom) {
			return errors.New("memstore: duplicate minor-unit version valid_from")
		}
	}
	cp := *mv
	cp.ValidTo = nil
	t.d.mus = append(t.d.mus, cp)
	return nil
}

func (t *tx) ListMinorUnitVersions(_ context.Context, currencyID string) ([]domain.MinorUnitVersion, error) {
	var out []domain.MinorUnitVersion
	for _, v := range t.d.mus {
		if v.CurrencyID == currencyID {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ValidFrom.Before(out[j].ValidFrom) })
	return out, nil
}

func (t *tx) InsertStatusHistory(_ context.Context, e *domain.StatusHistoryEntry) error {
	t.d.history = append(t.d.history, *e)
	return nil
}

func (t *tx) GetImportBySourceVersion(_ context.Context, source, version string) (*domain.Import, error) {
	for _, i := range t.d.imports {
		if i.SourceName == source && i.SourceVersion == version {
			cp := i
			return &cp, nil
		}
	}
	return nil, nil
}

func (t *tx) InsertImport(_ context.Context, imp *domain.Import) error {
	for _, e := range t.d.imports {
		if e.SourceName == imp.SourceName && e.SourceVersion == imp.SourceVersion && e.ManifestHash == imp.ManifestHash {
			return errors.New("memstore: duplicate import (source, version, hash)")
		}
	}
	t.d.imports = append(t.d.imports, *imp)
	return nil
}

func (t *tx) GetTenantSupport(_ context.Context, tenantID, currencyID string) (*domain.TenantSupport, error) {
	r, ok := t.d.overlay[overlayKey{tenantID, currencyID}]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (t *tx) UpsertTenantSupport(_ context.Context, ts *domain.TenantSupport, expectedVersion int64) error {
	k := overlayKey{ts.TenantID, ts.CurrencyID}
	cur, ok := t.d.overlay[k]
	switch {
	case !ok && expectedVersion != 0, ok && cur.Version != expectedVersion:
		return domain.ErrVersionConflictStore
	}
	t.d.overlay[k] = *ts
	return nil
}

func (t *tx) ListTenantSupport(_ context.Context, tenantID string) ([]domain.TenantSupport, error) {
	var out []domain.TenantSupport
	for k, v := range t.d.overlay {
		if k.tenant == tenantID {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AlphaCode < out[j].AlphaCode })
	return out, nil
}

func (t *tx) Enqueue(_ context.Context, e service.OutboxEntry) error {
	if t.s.FailEnqueue {
		return errors.New("memstore: injected outbox failure")
	}
	t.d.outbox = append(t.d.outbox, e)
	return nil
}
