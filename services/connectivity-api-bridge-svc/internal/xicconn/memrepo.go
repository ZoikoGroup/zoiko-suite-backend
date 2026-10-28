package xicconn

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// MemRepo is an in-memory Repo for unit and handler tests. It enforces tenant isolation, the live-connection
// exclusivity of a provider account, the platform-wide exclusivity of a credential reference, single-use
// authorization state, and the version bump, so tests built on it exercise the same rules as Postgres. The
// Postgres tests prove the database holds them itself.
type MemRepo struct {
	mu       sync.Mutex
	conns    map[string]*Connection
	checks   map[string][]Check
	certs    map[string][]Certification
	sessions map[string][]Session
	requests map[string][]Request
	events   map[string][]Event
	idem     map[string]string
	Out      []OutEvent
	Signals  []OutEvent
}

// NewMemRepo returns an empty MemRepo.
func NewMemRepo() *MemRepo {
	return &MemRepo{conns: map[string]*Connection{}, checks: map[string][]Check{}, certs: map[string][]Certification{},
		sessions: map[string][]Session{}, requests: map[string][]Request{}, events: map[string][]Event{}, idem: map[string]string{}}
}

func clone(c *Connection) *Connection {
	raw, _ := json.Marshal(c)
	var out Connection
	_ = json.Unmarshal(raw, &out)
	if out.Bindings == nil {
		out.Bindings = []Binding{}
	}
	if out.Endpoints == nil {
		out.Endpoints = map[string]string{}
	}
	return &out
}

func (m *MemRepo) owned(ctx context.Context, id string) (*Connection, bool) {
	c, ok := m.conns[id]
	if !ok || c.TenantID != middleware.GetTenantID(ctx) {
		return nil, false
	}
	return c, true
}

// refTaken reports whether another live binding anywhere holds ref.
func (m *MemRepo) refTaken(ref, exceptConn, exceptProfile string) bool {
	for _, c := range m.conns {
		for _, b := range c.Bindings {
			if b.Status == BindingRevoked || (c.ConnectionID == exceptConn && b.AuthProfile == exceptProfile) {
				continue
			}
			if strings.EqualFold(b.CurrentRef, ref) {
				return true
			}
		}
	}
	return false
}

func (m *MemRepo) Create(ctx context.Context, c *Connection, idemKey, idemHash string, ev OutEvent) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenant := middleware.GetTenantID(ctx)
	if c.TenantID != tenant {
		return false, ErrNotFound
	}
	if idemKey != "" {
		k := tenant + "|" + idemKey
		if h, ok := m.idem[k]; ok {
			if !strings.HasPrefix(h, idemHash+"|") {
				return false, ErrIdempotency
			}
			*c = *clone(m.conns[strings.TrimPrefix(h, idemHash+"|")])
			return false, nil
		}
	}
	for _, x := range m.conns {
		if x.TenantID == c.TenantID && x.Environment == c.Environment && x.ConnectorID == c.ConnectorID &&
			x.ProviderAccountHash == c.ProviderAccountHash && x.Status != StatusRevoked {
			return false, ErrExists
		}
	}
	m.conns[c.ConnectionID] = clone(c)
	if idemKey != "" {
		m.idem[tenant+"|"+idemKey] = idemHash + "|" + c.ConnectionID
	}
	m.Out = append(m.Out, ev)
	return true, nil
}

func (m *MemRepo) Get(ctx context.Context, id string) (*Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.owned(ctx, id)
	if !ok {
		return nil, ErrNotFound
	}
	return clone(c), nil
}

func (m *MemRepo) GetAcrossTenants(ctx context.Context, id string) (*Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(c), nil
}

func (m *MemRepo) List(ctx context.Context, f Filter) ([]Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Connection{}
	for _, c := range m.conns {
		if c.TenantID != middleware.GetTenantID(ctx) {
			continue
		}
		if (f.Status != "" && c.Status != f.Status) || (f.ConnectorID != "" && c.ConnectorID != f.ConnectorID) || (f.Environment != "" && c.Environment != f.Environment) {
			continue
		}
		out = append(out, *clone(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemRepo) Mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.owned(ctx, id)
	if !ok {
		return nil, ErrNotFound
	}
	work := clone(cur)
	tx := &Tx{C: work, Checks: append([]Check{}, m.checks[id]...), Certs: append([]Certification{}, m.certs[id]...),
		Requests: append([]Request{}, m.requests[id]...)}
	if ss := m.sessions[id]; len(ss) > 0 {
		last := ss[len(ss)-1]
		tx.Session = &last
	}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if work.Status == StatusRevoked && cur.Status == StatusRevoked {
		return nil, ErrTransition
	}
	// a credential reference is bound at most once across the platform
	for _, b := range work.Bindings {
		if b.Status != BindingRevoked && m.refTaken(b.CurrentRef, id, b.AuthProfile) {
			return nil, ErrCrossTenantCredential
		}
	}
	work.Version = cur.Version + 1
	work.UpdatedAt = time.Now().UTC()
	m.conns[id] = work
	if tx.NewCheck != nil {
		m.checks[id] = append(m.checks[id], *tx.NewCheck)
	}
	if tx.NewCert != nil {
		m.certs[id] = append(m.certs[id], *tx.NewCert)
	}
	if tx.NewSession != nil {
		m.sessions[id] = append(m.sessions[id], *tx.NewSession)
	}
	if tx.ConsumeSession && len(m.sessions[id]) > 0 {
		now := time.Now().UTC()
		m.sessions[id][len(m.sessions[id])-1].ConsumedAt = &now
	}
	if tx.NewRequest != nil {
		m.requests[id] = append(m.requests[id], *tx.NewRequest)
	}
	if tx.UpdateRequest != nil {
		for i := range m.requests[id] {
			if m.requests[id][i].RequestID == tx.UpdateRequest.RequestID {
				m.requests[id][i] = *tx.UpdateRequest
			}
		}
	}
	if tx.NewEvent != nil {
		m.events[id] = append(m.events[id], *tx.NewEvent)
	}
	m.Out = append(m.Out, tx.Out...)
	return clone(work), nil
}

func (m *MemRepo) ListChecks(ctx context.Context, id string) ([]Check, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, id); !ok {
		return nil, ErrNotFound
	}
	return append([]Check{}, m.checks[id]...), nil
}

func (m *MemRepo) ListCertifications(ctx context.Context, id string) ([]Certification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, id); !ok {
		return nil, ErrNotFound
	}
	return append([]Certification{}, m.certs[id]...), nil
}

func (m *MemRepo) ListEvents(ctx context.Context, id string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, id); !ok {
		return nil, ErrNotFound
	}
	return append([]Event{}, m.events[id]...), nil
}

func (m *MemRepo) ListRequests(ctx context.Context, id string) ([]Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, id); !ok {
		return nil, ErrNotFound
	}
	return append([]Request{}, m.requests[id]...), nil
}

func (m *MemRepo) Due(ctx context.Context, now time.Time, window time.Duration, limit int) ([]DueRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DueRef
	for _, c := range m.conns {
		if c.Status == StatusRevoked {
			continue
		}
		due := false
		for _, b := range c.Bindings {
			if b.ExpiresAt != nil && b.ExpiresAt.Sub(now) <= window {
				due = true
			}
			if b.PreviousGraceUntil != nil && !b.PreviousGraceUntil.After(now) {
				due = true
			}
		}
		for _, r := range m.requests[c.ConnectionID] {
			if r.Kind == RequestMigration && r.Status == ReqApproved && r.EffectiveAt != nil && !r.EffectiveAt.After(now) {
				due = true
			}
		}
		if due {
			out = append(out, DueRef{TenantID: c.TenantID, ConnectionID: c.ConnectionID})
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemRepo) RecordSignal(ctx context.Context, tenant string, ev OutEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Signals = append(m.Signals, ev)
	return nil
}

// Types returns the outbox event types recorded so far, for assertions.
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

// LiveRefs returns the live connections across tenants.
func (m *MemRepo) LiveRefs(_ context.Context, limit int) ([]DueRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DueRef
	for _, c := range m.conns {
		if c.Live() && (limit < 1 || len(out) < limit) {
			out = append(out, DueRef{TenantID: c.TenantID, ConnectionID: c.ConnectionID})
		}
	}
	return out, nil
}
