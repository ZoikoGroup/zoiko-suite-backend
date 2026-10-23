package rollout

import (
	"context"
	"sort"
	"sync"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// MemRepo is the in-memory Repo used by tests. The platform's side of readiness and parity is whatever the test
// sets in the fact fields, so a test can state exactly what the platform holds. Fail, when set, is asked before every
// repository call with the call's name and may return an error to inject a fault.
type MemRepo struct {
	mu       sync.Mutex
	rollouts map[string]*Rollout
	requests map[string]*Request
	legacy   map[string]*Legacy
	regs     map[string][]LegacyExchange // legacy id -> registrations in order
	parity   map[string][]ParityRun
	usage    map[string]int // rollout id + day -> admitted
	events   map[string][]Event
	Out      []OutEvent
	Signals  []OutEvent

	OutcomesF   Outcomes
	HighF       int
	UnknownF    int
	Counterpart map[string]string
	InboundN    int
	XICOnlyN    int
	FactsErr    error
	Fail        func(op string) error
}

// NewMemRepo returns an empty repository.
func NewMemRepo() *MemRepo {
	return &MemRepo{rollouts: map[string]*Rollout{}, requests: map[string]*Request{}, legacy: map[string]*Legacy{}, regs: map[string][]LegacyExchange{},
		parity: map[string][]ParityRun{}, usage: map[string]int{}, events: map[string][]Event{}, Counterpart: map[string]string{}}
}

var _ Repo = (*MemRepo)(nil)

func (m *MemRepo) tenant(ctx context.Context) string { return middleware.GetTenantID(ctx) }

func (m *MemRepo) fault(op string) error {
	if m.Fail != nil {
		return m.Fail(op)
	}
	return nil
}

func (m *MemRepo) Outcomes(context.Context, string, time.Time, time.Time) (Outcomes, error) {
	return m.OutcomesF, m.FactsErr
}
func (m *MemRepo) OpenHighVariances(context.Context, string) (int, error) { return m.HighF, m.FactsErr }
func (m *MemRepo) OverdueUnknown(context.Context, string, time.Time) (int, error) {
	return m.UnknownF, m.FactsErr
}
func (m *MemRepo) Counterparts(_ context.Context, _, _ string, refs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range refs {
		if st, ok := m.Counterpart[r]; ok {
			out[r] = st
		}
	}
	return out, m.FactsErr
}
func (m *MemRepo) InboundCount(context.Context, string, time.Time, time.Time) (int, error) {
	return m.InboundN, m.FactsErr
}
func (m *MemRepo) XICOnly(context.Context, string, string, time.Time, time.Time, []string) (int, error) {
	return m.XICOnlyN, m.FactsErr
}

func (m *MemRepo) InsertRollout(ctx context.Context, r *Rollout, ev Event, out []OutEvent) error {
	if err := m.fault("InsertRollout"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.TenantID != m.tenant(ctx) {
		return ErrNotFound
	}
	for _, x := range m.rollouts {
		if x.ConnectionID == r.ConnectionID && x.Stage != StageRolledBack {
			return ErrExists
		}
	}
	c := *r
	m.rollouts[r.RolloutID] = &c
	m.events[r.RolloutID] = append(m.events[r.RolloutID], ev)
	m.Out = append(m.Out, out...)
	return nil
}

func (m *MemRepo) GetRollout(ctx context.Context, id string) (*Rollout, error) {
	if err := m.fault("GetRollout"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rollouts[id]
	if !ok || r.TenantID != m.tenant(ctx) {
		return nil, ErrNotFound
	}
	c := *r
	return &c, nil
}

func (m *MemRepo) LatestRolloutFor(ctx context.Context, connectionID string) (*Rollout, error) {
	if err := m.fault("LatestRolloutFor"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Rollout
	for _, r := range m.rollouts {
		if r.ConnectionID == connectionID && (best == nil || r.CreatedAt.After(best.CreatedAt)) {
			best = r
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	c := *best
	return &c, nil
}

func (m *MemRepo) ListRollouts(ctx context.Context, f RolloutFilter) ([]Rollout, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Rollout
	for _, r := range m.rollouts {
		if r.TenantID != m.tenant(ctx) || (f.Stage != "" && r.Stage != f.Stage) || (f.ConnectionID != "" && r.ConnectionID != f.ConnectionID) {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *MemRepo) MutateRollout(ctx context.Context, id string, fn func(tx *RolloutTx) error) (*Rollout, error) {
	if err := m.fault("MutateRollout"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rollouts[id]
	if !ok || r.TenantID != m.tenant(ctx) {
		return nil, ErrNotFound
	}
	work := *r
	tx := &RolloutTx{R: &work}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if tx.Use != "" {
		q, ok := m.requests[tx.Use]
		if !ok || q.Status != ReqApproved || q.DecidedBy == "" || q.DecidedBy == q.RequestedBy || q.SubjectID != id || q.FromState != r.Stage || q.ToState != work.Stage {
			return nil, ErrState
		}
		q.Status = ReqUsed
	} else if r.Stage != work.Stage && work.Stage != StageRolledBack {
		return nil, ErrState // the way forward exists only through a request
	}
	work.Version, work.UpdatedAt = r.Version+1, time.Now().UTC()
	*r = work
	m.events[id] = append(m.events[id], tx.Event...)
	m.Out = append(m.Out, tx.Out...)
	c := *r
	return &c, nil
}

func (m *MemRepo) PilotRollouts(_ context.Context, limit int) ([]DueRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DueRef
	for _, r := range m.rollouts {
		if r.Stage == StagePilot {
			out = append(out, DueRef{TenantID: r.TenantID, ID: r.RolloutID})
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemRepo) Admit(_ context.Context, r *Rollout, day time.Time) (bool, error) {
	if err := m.fault("Admit"); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := r.RolloutID + day.UTC().Format("2006-01-02")
	if m.usage[k] >= r.PilotDailyCap {
		return false, nil
	}
	m.usage[k]++
	return true, nil
}

func (m *MemRepo) UsedToday(_ context.Context, rolloutID string, day time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage[rolloutID+day.UTC().Format("2006-01-02")], nil
}

func (m *MemRepo) InsertRequest(ctx context.Context, q *Request, ev Event, out []OutEvent) error {
	if err := m.fault("InsertRequest"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.requests {
		if x.SubjectKind == q.SubjectKind && x.SubjectID == q.SubjectID && x.ToState == q.ToState && x.Status == ReqPending {
			return ErrState
		}
	}
	c := *q
	m.requests[q.RequestID] = &c
	m.events[q.SubjectID] = append(m.events[q.SubjectID], ev)
	m.Out = append(m.Out, out...)
	return nil
}

func (m *MemRepo) GetRequest(ctx context.Context, id string) (*Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.requests[id]
	if !ok || q.TenantID != m.tenant(ctx) {
		return nil, ErrNotFound
	}
	c := *q
	return &c, nil
}

func (m *MemRepo) ListRequests(ctx context.Context, f RequestFilter) ([]Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Request
	for _, q := range m.requests {
		if q.TenantID != m.tenant(ctx) || (f.SubjectKind != "" && q.SubjectKind != f.SubjectKind) || (f.SubjectID != "" && q.SubjectID != f.SubjectID) ||
			(f.Status != "" && q.Status != f.Status) {
			continue
		}
		out = append(out, *q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *MemRepo) MutateRequest(ctx context.Context, id string, fn func(tx *RequestTx) error) (*Request, error) {
	if err := m.fault("MutateRequest"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.requests[id]
	if !ok || q.TenantID != m.tenant(ctx) {
		return nil, ErrNotFound
	}
	work := *q
	tx := &RequestTx{Q: &work}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if work.Status != ReqPending && work.Status != ReqExpired && (work.DecidedBy == "" || work.DecidedBy == work.RequestedBy) {
		return nil, ErrSelfAction
	}
	work.Version++
	*q = work
	m.events[q.SubjectID] = append(m.events[q.SubjectID], tx.Event...)
	m.Out = append(m.Out, tx.Out...)
	c := *q
	return &c, nil
}

func (m *MemRepo) InsertLegacy(ctx context.Context, l *Legacy, ev Event, out []OutEvent) error {
	if err := m.fault("InsertLegacy"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.legacy {
		if x.TenantID == l.TenantID && x.Name == l.Name {
			return ErrExists
		}
	}
	c := *l
	m.legacy[l.LegacyID] = &c
	m.events[l.LegacyID] = append(m.events[l.LegacyID], ev)
	m.Out = append(m.Out, out...)
	return nil
}

func (m *MemRepo) GetLegacy(ctx context.Context, id string) (*Legacy, error) {
	if err := m.fault("GetLegacy"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.legacy[id]
	if !ok || l.TenantID != m.tenant(ctx) {
		return nil, ErrNotFound
	}
	c := *l
	return &c, nil
}

func (m *MemRepo) ListLegacy(ctx context.Context, f LegacyFilter) ([]Legacy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Legacy
	for _, l := range m.legacy {
		if l.TenantID != m.tenant(ctx) || (f.State != "" && l.State != f.State) {
			continue
		}
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *MemRepo) MutateLegacy(ctx context.Context, id string, fn func(tx *LegacyTx) error) (*Legacy, error) {
	if err := m.fault("MutateLegacy"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.legacy[id]
	if !ok || l.TenantID != m.tenant(ctx) {
		return nil, ErrNotFound
	}
	work := *l
	tx := &LegacyTx{L: &work}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if work.State == LegacyCutover || work.State == LegacyRetired {
		if work.State != l.State {
			q, ok := m.requests[tx.Use]
			if tx.Use == "" || !ok || q.Status != ReqApproved || q.DecidedBy == "" || q.DecidedBy == q.RequestedBy || q.SubjectID != id || q.FromState != l.State || q.ToState != work.State {
				return nil, ErrState
			}
			q.Status = ReqUsed
		}
	}
	work.Version, work.UpdatedAt = l.Version+1, time.Now().UTC()
	*l = work
	m.events[id] = append(m.events[id], tx.Event...)
	m.Out = append(m.Out, tx.Out...)
	c := *l
	return &c, nil
}

func (m *MemRepo) RegisterExchanges(ctx context.Context, legacyID string, in []LegacyExchange, by string, at time.Time) (int, int, error) {
	if err := m.fault("RegisterExchanges"); err != nil {
		return 0, 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.legacy[legacyID]
	if !ok || l.TenantID != m.tenant(ctx) {
		return 0, 0, ErrNotFound
	}
	// the whole call or nothing: check every entry before storing any
	have := m.regs[legacyID]
	hashOf := map[string]string{}
	seen := map[string]bool{}
	for _, e := range have {
		hashOf[e.ExternalRef] = e.PayloadHash
		seen[e.ExternalRef+"|"+e.Outcome] = true
	}
	var add []LegacyExchange
	dup := 0
	for _, e := range in {
		if h, ok := hashOf[e.ExternalRef]; ok && h != e.PayloadHash {
			return 0, 0, ErrConflict
		}
		hashOf[e.ExternalRef] = e.PayloadHash
		k := e.ExternalRef + "|" + e.Outcome
		if seen[k] {
			dup++
			continue
		}
		seen[k] = true
		e.TenantID, e.LegacyID, e.RegisteredBy, e.RegisteredAt = l.TenantID, legacyID, by, at
		add = append(add, e)
	}
	m.regs[legacyID] = append(m.regs[legacyID], add...)
	return len(add), dup, nil
}

func (m *MemRepo) LatestExchanges(ctx context.Context, legacyID string, from, to time.Time, limit int) ([]LegacyExchange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	latest := map[string]LegacyExchange{}
	var order []string
	for _, e := range m.regs[legacyID] {
		if e.OccurredAt.Before(from) || !e.OccurredAt.Before(to) {
			continue
		}
		if _, ok := latest[e.ExternalRef]; !ok {
			order = append(order, e.ExternalRef)
		}
		latest[e.ExternalRef] = e
	}
	var out []LegacyExchange
	for _, r := range order {
		out = append(out, latest[r])
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemRepo) InsertParity(ctx context.Context, p *ParityRun, ev Event, out []OutEvent) error {
	if err := m.fault("InsertParity"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parity[p.LegacyID] = append([]ParityRun{*p}, m.parity[p.LegacyID]...)
	m.events[p.LegacyID] = append(m.events[p.LegacyID], ev)
	m.Out = append(m.Out, out...)
	return nil
}

func (m *MemRepo) ListParity(_ context.Context, legacyID string, limit int) ([]ParityRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]ParityRun{}, m.parity[legacyID]...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemRepo) Events(_ context.Context, _ string, id string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event{}, m.events[id]...), nil
}

func (m *MemRepo) RecordSignal(_ context.Context, _ string, ev OutEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Signals = append(m.Signals, ev)
	return nil
}

// OutTypes returns the event types emitted so far.
func (m *MemRepo) OutTypes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t []string
	for _, o := range m.Out {
		t = append(t, o.Type)
	}
	return t
}
