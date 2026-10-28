package outbound

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// MemRepo is an in-memory Repo for unit and handler tests. It enforces tenant isolation, the one-attempt-per-logical-
// operation rule, the version bump, the state graph, the resubmission rule and the submit counter, and it records the
// outbox events it is given. The Postgres tests prove the database holds the rules itself.
type MemRepo struct {
	mu       sync.Mutex
	at       map[string]*Attempt
	events   map[string][]Event
	receipts map[string]*Receipt
	Out      []OutEvent
	Signals  []OutEvent
	// FailNext makes the next Reserve fail, to test storage outages.
	FailNext bool
}

// NewMemRepo returns an empty MemRepo.
func NewMemRepo() *MemRepo {
	return &MemRepo{at: map[string]*Attempt{}, events: map[string][]Event{}, receipts: map[string]*Receipt{}}
}

func cloneAttempt(a *Attempt) *Attempt {
	c := *a
	if a.Body != nil {
		c.Body = append([]byte(nil), a.Body...)
	}
	c.BodyRetained = len(c.Body) > 0
	return &c
}

func (m *MemRepo) owned(ctx context.Context, id string) (*Attempt, bool) {
	a, ok := m.at[id]
	if !ok || a.TenantID != middleware.GetTenantID(ctx) {
		return nil, false
	}
	return a, true
}

// Reserve implements Repo.
func (m *MemRepo) Reserve(ctx context.Context, a *Attempt, ev Event, out []OutEvent) (*Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext {
		m.FailNext = false
		return nil, context.DeadlineExceeded
	}
	if a.TenantID != middleware.GetTenantID(ctx) {
		return nil, ErrNotFound
	}
	for _, x := range m.at {
		if x.TenantID == a.TenantID && x.ConnectionID == a.ConnectionID && x.OperationID == a.OperationID && x.DomainRequestRef == a.DomainRequestRef {
			return cloneAttempt(x), ErrDuplicate
		}
	}
	m.at[a.AttemptID] = cloneAttempt(a)
	ev.At = a.CreatedAt
	m.events[a.AttemptID] = append(m.events[a.AttemptID], ev)
	m.Out = append(m.Out, out...)
	return nil, nil
}

// Get implements Repo.
func (m *MemRepo) Get(ctx context.Context, id string) (*Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.owned(ctx, id)
	if !ok {
		return nil, ErrNotFound
	}
	return cloneAttempt(a), nil
}

// List implements Repo.
func (m *MemRepo) List(ctx context.Context, f Filter) ([]Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Attempt{}
	for _, a := range m.at {
		if a.TenantID != middleware.GetTenantID(ctx) {
			continue
		}
		if (f.State != "" && a.State != f.State) || (f.ConnectionID != "" && a.ConnectionID != f.ConnectionID) ||
			(f.DomainRequestRef != "" && a.DomainRequestRef != f.DomainRequestRef) {
			continue
		}
		out = append(out, *cloneAttempt(a))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].AttemptID < out[j].AttemptID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Events implements Repo.
func (m *MemRepo) Events(ctx context.Context, id string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, id); !ok {
		return nil, ErrNotFound
	}
	return append([]Event{}, m.events[id]...), nil
}

var legal = map[string][]string{
	StateReserved:   {StateSubmitting, StateCancelled},
	StateSubmitting: {StateReserved, StatePending, StateUnknown, StateSettled, StateFailed},
	StatePending:    {StateSubmitting, StateUnknown, StateSettled, StateFailed},
	StateUnknown:    {StateSubmitting, StatePending, StateSettled, StateFailed},
}

// Mutate implements Repo.
func (m *MemRepo) Mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.owned(ctx, id)
	if !ok {
		return nil, ErrNotFound
	}
	w := cloneAttempt(cur)
	tx := &Tx{A: w}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if tx.Receipt != nil {
		if holder := m.heldKey(tx.Receipt); holder != nil {
			return nil, ErrDuplicate
		}
	}
	if tx.NoUpdate {
		if tx.Event != nil {
			m.events[id] = append(m.events[id], *tx.Event)
		}
		if tx.Receipt != nil {
			r := *tx.Receipt
			m.receipts[r.ReceiptID] = &r
		}
		m.Out = append(m.Out, tx.Out...)
		return cloneAttempt(cur), nil
	}
	if w.State != cur.State {
		okMove := false
		for _, to := range legal[cur.State] {
			if to == w.State {
				okMove = true
			}
		}
		if !okMove {
			return nil, ErrTransition
		}
		if w.State == StateSubmitting {
			if (cur.State == StatePending || cur.State == StateUnknown) && !cur.ResubmitAllowed {
				return nil, ErrTransition
			}
			if w.SubmitCount != cur.SubmitCount+1 {
				return nil, ErrTransition
			}
		}
	}
	if w.State == cur.State && w.SubmitCount != cur.SubmitCount {
		return nil, ErrTransition
	}
	if w.State == StateSubmitting && cur.State != StateSubmitting && w.LastSubmitAt == nil {
		return nil, ErrTransition
	}
	if w.State == StateUnknown && w.AmbiguityReason == "" {
		return nil, ErrTransition
	}
	if (w.State == StateSettled || w.State == StateFailed) && (len(w.FinalOutcomeRef) == 0 || w.ResolutionMethod == "") {
		return nil, ErrTransition
	}
	if w.ResolutionMethod == MethodManual && (w.ResolvedBy == "" || w.ResolvedBy == w.RequestedBy || w.EvidenceRef == "") {
		return nil, ErrTransition
	}
	if len(w.Body) > 0 && !w.Open() {
		return nil, ErrTransition
	}
	w.Version = cur.Version + 1
	w.UpdatedAt = time.Now().UTC()
	w.BodyRetained = len(w.Body) > 0
	m.at[id] = w
	if tx.Event != nil {
		m.events[id] = append(m.events[id], *tx.Event)
	}
	if tx.Receipt != nil {
		r := *tx.Receipt
		m.receipts[r.ReceiptID] = &r
	}
	m.Out = append(m.Out, tx.Out...)
	return cloneAttempt(w), nil
}

// DueForResolution implements Repo.
func (m *MemRepo) DueForResolution(_ context.Context, now, escalateBefore time.Time, limit int) ([]DueRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DueRef
	for _, a := range m.at {
		due := false
		switch a.State {
		case StateSubmitting, StatePending, StateUnknown:
			due = a.NextResolutionAt != nil && !a.NextResolutionAt.After(now)
			if !due && a.EscalatedAt == nil && a.State != StateSubmitting && a.LastSubmitAt != nil && !a.LastSubmitAt.After(escalateBefore) {
				due = true
			}
		}
		if due {
			out = append(out, DueRef{TenantID: a.TenantID, AttemptID: a.AttemptID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptID < out[j].AttemptID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PurgeBodies implements Repo.
func (m *MemRepo) PurgeBodies(_ context.Context, now time.Time, _ int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, a := range m.at {
		if len(a.Body) > 0 && (!a.Open() || (a.BodyExpiresAt != nil && !a.BodyExpiresAt.After(now))) {
			a.Body, a.BodyRetained = nil, false
			n++
		}
	}
	return n, nil
}

// RecordSignal implements Repo.
func (m *MemRepo) RecordSignal(_ context.Context, _ string, ev OutEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Signals = append(m.Signals, ev)
	return nil
}

// Types returns the types of every outbox event recorded so far, in order.
func (m *MemRepo) Types() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t []string
	for _, e := range m.Out {
		t = append(t, e.Type)
	}
	for _, e := range m.Signals {
		t = append(t, e.Type)
	}
	return t
}

var _ Repo = (*MemRepo)(nil)
var _ = errors.New

// heldKey returns the receipt that already holds r's dedupe key in the same tenant, if any.
func (m *MemRepo) heldKey(r *Receipt) *Receipt {
	if r.DedupeKey == "" {
		return nil
	}
	for _, x := range m.receipts {
		if x.TenantID == r.TenantID && x.DedupeKey == r.DedupeKey {
			return x
		}
	}
	return nil
}

// InsertReceipt implements Repo.
func (m *MemRepo) InsertReceipt(ctx context.Context, r *Receipt, out []OutEvent) (*Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext {
		m.FailNext = false
		return nil, context.DeadlineExceeded
	}
	if r.TenantID != middleware.GetTenantID(ctx) {
		return nil, ErrReceiptNotFound
	}
	if h := m.heldKey(r); h != nil {
		c := *h
		return &c, ErrDuplicate
	}
	c := *r
	m.receipts[r.ReceiptID] = &c
	m.Out = append(m.Out, out...)
	return nil, nil
}

// GetReceipt implements Repo.
func (m *MemRepo) GetReceipt(ctx context.Context, id string) (*Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.receipts[id]
	if !ok || r.TenantID != middleware.GetTenantID(ctx) {
		return nil, ErrReceiptNotFound
	}
	c := *r
	return &c, nil
}

// GetReceiptByDedupe implements Repo.
func (m *MemRepo) GetReceiptByDedupe(ctx context.Context, tenant, key string) (*Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.receipts {
		if r.TenantID == tenant && r.DedupeKey == key && key != "" {
			c := *r
			return &c, nil
		}
	}
	return nil, ErrReceiptNotFound
}

// ListReceipts implements Repo.
func (m *MemRepo) ListReceipts(ctx context.Context, f ReceiptFilter) ([]Receipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Receipt{}
	for _, r := range m.receipts {
		if r.TenantID != middleware.GetTenantID(ctx) {
			continue
		}
		if (f.State != "" && r.State != f.State) || (f.ConnectionID != "" && r.ConnectionID != f.ConnectionID) ||
			(f.AttemptID != "" && r.AttemptID != f.AttemptID) || (f.NeedsReconciliation && !r.NeedsReconciliation) {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ReceivedAt.After(out[j].ReceivedAt)
		}
		return out[i].ReceiptID < out[j].ReceiptID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// FindAttempt implements Repo.
func (m *MemRepo) FindAttempt(ctx context.Context, connectionID, operationID, reference, clientRef string) (*Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var byRef, byClient *Attempt
	for _, a := range m.at {
		if a.TenantID != middleware.GetTenantID(ctx) || a.ConnectionID != connectionID || a.OperationID != operationID {
			continue
		}
		if reference != "" && a.ProviderReference == reference {
			byRef = a
		}
		if clientRef != "" && a.DomainRequestRef == clientRef {
			byClient = a
		}
	}
	switch {
	case byRef != nil:
		return cloneAttempt(byRef), nil
	case byClient != nil:
		return cloneAttempt(byClient), nil
	}
	return nil, ErrNotFound
}
