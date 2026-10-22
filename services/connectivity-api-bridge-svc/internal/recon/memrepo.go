package recon

import (
	"context"
	"sort"
	"sync"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// MemRepo is the in-memory Repo used by tests. The platform's side of a comparison is whatever the test sets in the
// Fact fields, so a test can state exactly what the platform holds.
type MemRepo struct {
	mu        sync.Mutex
	runs      map[string]*Run
	variances map[string]*Variance
	events    map[string][]Event
	Out       []OutEvent
	Signals   []OutEvent

	Inbound   map[string]int
	Outbound  map[string]int
	Refs      []string
	AttemptsF []AttemptFact
	BatchesF  []BatchFact
	Late      int
	FactsErr  error
}

// NewMemRepo returns an empty repository.
func NewMemRepo() *MemRepo {
	return &MemRepo{runs: map[string]*Run{}, variances: map[string]*Variance{}, events: map[string][]Event{}, Inbound: map[string]int{}, Outbound: map[string]int{}}
}

func (m *MemRepo) tenant(ctx context.Context) string { return middleware.GetTenantID(ctx) }

func (m *MemRepo) InboundCounts(context.Context, string, time.Time, time.Time) (map[string]int, error) {
	return m.Inbound, m.FactsErr
}
func (m *MemRepo) OutboundCounts(context.Context, string, time.Time, time.Time) (map[string]int, error) {
	return m.Outbound, m.FactsErr
}
func (m *MemRepo) References(_ context.Context, _ string, _, _ time.Time, limit int) ([]string, error) {
	if limit > 0 && len(m.Refs) > limit {
		return m.Refs[:limit], m.FactsErr
	}
	return m.Refs, m.FactsErr
}
func (m *MemRepo) Attempts(context.Context, string, time.Time, time.Time, int) ([]AttemptFact, error) {
	return m.AttemptsF, m.FactsErr
}
func (m *MemRepo) Batches(context.Context, string, time.Time, time.Time, int) ([]BatchFact, error) {
	return m.BatchesF, m.FactsErr
}
func (m *MemRepo) LateInbound(context.Context, string, time.Time, time.Time, time.Time) (int, error) {
	return m.Late, m.FactsErr
}

func (m *MemRepo) InsertRun(ctx context.Context, r *Run, ev Event, out []OutEvent) (*Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.runs {
		if x.TenantID == r.TenantID && x.DedupeKey == r.DedupeKey {
			c := *x
			return &c, true, nil
		}
	}
	c := *r
	m.runs[r.RunID] = &c
	m.events[r.RunID] = append(m.events[r.RunID], ev)
	m.Out = append(m.Out, out...)
	return r, false, nil
}

func (m *MemRepo) visible(ctx context.Context, r *Run) bool {
	t := m.tenant(ctx)
	return t != "" && r.TenantID == t
}

func (m *MemRepo) GetRun(ctx context.Context, id string) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || !m.visible(ctx, r) {
		return nil, ErrNotFound
	}
	c := *r
	return &c, nil
}

func (m *MemRepo) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Run
	for _, r := range m.runs {
		if m.visible(ctx, r) && (f.State == "" || r.State == f.State) && (f.ConnectionID == "" || r.ConnectionID == f.ConnectionID) {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *MemRepo) RunEvents(ctx context.Context, id string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || !m.visible(ctx, r) {
		return nil, ErrNotFound
	}
	return append([]Event(nil), m.events[id]...), nil
}

func (m *MemRepo) MutateRun(ctx context.Context, id string, fn func(tx *RunTx) error) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || !m.visible(ctx, r) {
		return nil, ErrNotFound
	}
	work := *r
	tx := &RunTx{Run: &work}
	if err := fn(tx); err != nil {
		return nil, err
	}
	changed := len(tx.Event) > 0 || len(tx.Out) > 0 || len(tx.Variances) > 0 || work.State != r.State || work.LeaseUntil != r.LeaseUntil || work.AwaitingDomain != r.AwaitingDomain
	if !changed {
		c := *r
		return &c, nil
	}
	for i := range tx.Variances {
		v := tx.Variances[i]
		var held *Variance
		for _, x := range m.variances {
			if x.TenantID == v.TenantID && x.ConnectionID == v.ConnectionID && x.Fingerprint == v.Fingerprint && !x.Final() {
				held = x
			}
		}
		if held != nil {
			held.RepeatCount++
			held.LastRunID = id
			held.Version++
			held.UpdatedAt = time.Now().UTC()
			if tx.VarianceEvent != nil {
				m.Out = append(m.Out, tx.VarianceEvent(*held, true))
			}
			continue
		}
		c := v
		m.variances[v.VarianceID] = &c
		if tx.VarianceEvent != nil {
			m.Out = append(m.Out, tx.VarianceEvent(c, false))
		}
	}
	work.Version++
	work.UpdatedAt = time.Now().UTC()
	*r = work
	m.events[id] = append(m.events[id], tx.Event...)
	m.Out = append(m.Out, tx.Out...)
	c := *r
	return &c, nil
}

func (m *MemRepo) GetVariance(ctx context.Context, id string) (*Variance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.variances[id]
	if !ok || v.TenantID != m.tenant(ctx) {
		return nil, ErrNotFound
	}
	c := *v
	return &c, nil
}

func (m *MemRepo) ListVariances(ctx context.Context, f VarianceFilter) ([]Variance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Variance
	for _, v := range m.variances {
		if v.TenantID == m.tenant(ctx) && (f.State == "" || v.State == f.State) && (f.RunID == "" || v.RunID == f.RunID || v.LastRunID == f.RunID) &&
			(f.ConnectionID == "" || v.ConnectionID == f.ConnectionID) && (f.Severity == "" || v.Severity == f.Severity) {
			out = append(out, *v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *MemRepo) MutateVariance(ctx context.Context, id string, fn func(tx *VarTx) error) (*Variance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.variances[id]
	if !ok || v.TenantID != m.tenant(ctx) {
		return nil, ErrNotFound
	}
	work := *v
	run := m.runs[v.RunID]
	rc := *run
	tx := &VarTx{V: &work, Run: &rc}
	if err := fn(tx); err != nil {
		return nil, err
	}
	work.Version++
	work.UpdatedAt = time.Now().UTC()
	*v = work
	m.events[v.RunID] = append(m.events[v.RunID], tx.Event...)
	m.Out = append(m.Out, tx.Out...)
	if work.Final() {
		for _, rid := range []string{v.RunID, v.LastRunID} {
			r := m.runs[rid]
			open := 0
			for _, x := range m.variances {
				if (x.RunID == rid || x.LastRunID == rid) && !x.Final() {
					open++
				}
			}
			r.OpenVariances = open
			if open == 0 && (r.State == RunVariance || r.State == RunEscalated) {
				r.State, r.ClosedBy, r.CloseNote = RunClosed, work.ResolvedBy, "all variances settled"
				r.Version++
				if tx.RunClosed != nil {
					m.Out = append(m.Out, tx.RunClosed(r))
				}
			}
		}
	}
	c := *v
	return &c, nil
}

func (m *MemRepo) DueRuns(_ context.Context, now time.Time, limit int) ([]DueRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DueRef
	for _, r := range m.runs {
		if r.State == RunOpen && !r.AwaitingDomain && (r.NextEvaluateAt == nil || !r.NextEvaluateAt.After(now)) && (r.LeaseUntil == nil || !r.LeaseUntil.After(now)) {
			out = append(out, DueRef{TenantID: r.TenantID, ID: r.RunID})
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemRepo) LastScheduledEnd(_ context.Context, tenant, conn string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var last time.Time
	for _, r := range m.runs {
		if r.TenantID == tenant && r.ConnectionID == conn && r.Origin == OriginScheduled && r.PeriodTo.After(last) {
			last = r.PeriodTo
		}
	}
	return last, nil
}

func (m *MemRepo) RecordSignal(_ context.Context, _ string, ev OutEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Signals = append(m.Signals, ev)
	return nil
}
