package batch

import (
	"context"
	"sort"
	"sync"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// MemRepo is an in-memory Repo for unit and handler tests. It enforces tenant isolation, the unique batch identity per
// connection and operation, the version bump, the state graph, the maker-checker rule on replay approval and the
// idempotent row add, and it records the outbox events it is given. The Postgres tests prove the database holds the
// rules itself.
type MemRepo struct {
	mu      sync.Mutex
	b       map[string]*Batch
	files   map[string][]File // by batch id, in order
	rows    map[string][]Row  // by batch id
	events  map[string][]Event
	replays map[string]*ReplayJob
	items   map[string][]ReplayItem
	Out     []OutEvent
	Signals []OutEvent
	// FailNext makes the next Insert fail, to test storage outages.
	FailNext bool
}

// NewMemRepo returns an empty MemRepo.
func NewMemRepo() *MemRepo {
	return &MemRepo{b: map[string]*Batch{}, files: map[string][]File{}, rows: map[string][]Row{}, events: map[string][]Event{},
		replays: map[string]*ReplayJob{}, items: map[string][]ReplayItem{}}
}

func cloneBatch(b *Batch) *Batch {
	c := *b
	if b.Manifest != nil {
		c.Manifest = append([]byte(nil), b.Manifest...)
	}
	return &c
}

func (m *MemRepo) owned(ctx context.Context, id string) (*Batch, bool) {
	b, ok := m.b[id]
	if !ok || b.TenantID != middleware.GetTenantID(ctx) {
		return nil, false
	}
	return b, true
}

// Insert implements Repo.
func (m *MemRepo) Insert(ctx context.Context, b *Batch, files []File, ev Event, out []OutEvent) (*Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext {
		m.FailNext = false
		return nil, context.DeadlineExceeded
	}
	if b.TenantID != middleware.GetTenantID(ctx) {
		return nil, ErrNotFound
	}
	if b.DedupeKey != "" {
		for _, x := range m.b {
			if x.TenantID == b.TenantID && x.DedupeKey == b.DedupeKey {
				return cloneBatch(x), ErrDuplicate
			}
		}
	}
	m.b[b.BatchID] = cloneBatch(b)
	m.files[b.BatchID] = cloneFiles(files)
	ev.At = b.ReceivedAt
	m.events[b.BatchID] = append(m.events[b.BatchID], ev)
	m.Out = append(m.Out, out...)
	return nil, nil
}

func cloneFiles(files []File) []File {
	out := make([]File, len(files))
	for i, f := range files {
		out[i] = f
		out[i].Content = append([]byte(nil), f.Content...)
		if f.Content == nil {
			out[i].Content = nil
		}
		out[i].ContentRetained = len(out[i].Content) > 0
	}
	return out
}

// InsertRejected implements Repo.
func (m *MemRepo) InsertRejected(ctx context.Context, b *Batch, files []File, ev Event, out []OutEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailNext {
		m.FailNext = false
		return context.DeadlineExceeded
	}
	if b.TenantID != middleware.GetTenantID(ctx) {
		return ErrNotFound
	}
	b.DedupeKey = ""
	m.b[b.BatchID] = cloneBatch(b)
	for i := range files {
		if files[i].FileID == "" {
			files[i].FileID = b.BatchID + "-f" + string(rune('a'+i))
		}
		files[i].BatchID, files[i].TenantID = b.BatchID, b.TenantID
	}
	m.files[b.BatchID] = cloneFiles(files)
	ev.At = b.ReceivedAt
	m.events[b.BatchID] = append(m.events[b.BatchID], ev)
	m.Out = append(m.Out, out...)
	return nil
}

// Get implements Repo.
func (m *MemRepo) Get(ctx context.Context, id string) (*Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.owned(ctx, id)
	if !ok {
		return nil, ErrNotFound
	}
	return cloneBatch(b), nil
}

// GetByDedupe implements Repo.
func (m *MemRepo) GetByDedupe(ctx context.Context, tenant, key string) (*Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.b {
		if b.TenantID == tenant && b.DedupeKey == key && key != "" && tenant == middleware.GetTenantID(ctx) {
			return cloneBatch(b), nil
		}
	}
	return nil, ErrNotFound
}

// List implements Repo.
func (m *MemRepo) List(ctx context.Context, f Filter) ([]Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Batch{}
	for _, b := range m.b {
		if b.TenantID != middleware.GetTenantID(ctx) || (f.State != "" && b.State != f.State) || (f.ConnectionID != "" && b.ConnectionID != f.ConnectionID) {
			continue
		}
		out = append(out, *cloneBatch(b))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].ReceivedAt.After(out[j].ReceivedAt)
		}
		return out[i].BatchID < out[j].BatchID
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

// AddEvent implements Repo.
func (m *MemRepo) AddEvent(ctx context.Context, id string, ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, id); !ok {
		return ErrNotFound
	}
	m.events[id] = append(m.events[id], ev)
	return nil
}

var batchGraph = map[string][]string{
	StateReceived:   {StateValidating, StateRejected, StateQuarantined},
	StateValidating: {StateAccepted, StateRejected, StateQuarantined},
	StateAccepted:   {StateProcessing, StateRejected, StateQuarantined},
	StateProcessing: {StatePartial, StateReconciled, StateQuarantined},
	StatePartial:    {StateProcessing, StateReconciled},
	StateReconciled: {StateProcessing, StatePartial},
}

// Mutate implements Repo.
func (m *MemRepo) Mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.owned(ctx, id)
	if !ok {
		return nil, ErrNotFound
	}
	w := cloneBatch(cur)
	tx := &Tx{B: w}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if w.State != cur.State {
		okMove := false
		for _, to := range batchGraph[cur.State] {
			if to == w.State {
				okMove = true
			}
		}
		if !okMove {
			return nil, ErrTransition
		}
	}
	w.Version = cur.Version + 1
	w.UpdatedAt = time.Now().UTC()
	m.b[id] = w
	if tx.Event != nil {
		m.events[id] = append(m.events[id], *tx.Event)
	}
	m.Out = append(m.Out, tx.Out...)
	return cloneBatch(w), nil
}

// Files implements Repo.
func (m *MemRepo) Files(ctx context.Context, batchID string, withContent bool) ([]File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, batchID); !ok {
		return nil, ErrNotFound
	}
	out := cloneFiles(m.files[batchID])
	if !withContent {
		for i := range out {
			out[i].Content = nil
		}
	}
	return out, nil
}

// UpdateFile implements Repo.
func (m *MemRepo) UpdateFile(ctx context.Context, f File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, f.BatchID); !ok {
		return ErrNotFound
	}
	for i := range m.files[f.BatchID] {
		if m.files[f.BatchID][i].FileID == f.FileID {
			cur := &m.files[f.BatchID][i]
			cur.State, cur.ReasonCode, cur.ParsedRows = f.State, f.ReasonCode, f.ParsedRows
			if !f.ContentRetained {
				cur.Content, cur.ContentRetained = nil, false
			}
			return nil
		}
	}
	return ErrNotFound
}

// AddRows implements Repo.
func (m *MemRepo) AddRows(ctx context.Context, rows []Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rows {
		if _, ok := m.owned(ctx, r.BatchID); !ok {
			return ErrNotFound
		}
		dup := false
		for _, x := range m.rows[r.BatchID] {
			if x.FileID == r.FileID && x.RowNo == r.RowNo {
				dup = true
			}
		}
		if !dup {
			m.rows[r.BatchID] = append(m.rows[r.BatchID], r)
		}
	}
	return nil
}

// Rows implements Repo.
func (m *MemRepo) Rows(ctx context.Context, batchID string, f RowFilter) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, batchID); !ok {
		return nil, ErrNotFound
	}
	want := map[string]bool{}
	for _, s := range f.States {
		want[s] = true
	}
	if f.State != "" {
		want[f.State] = true
	}
	nos := map[int]bool{}
	for _, n := range f.RowNos {
		nos[n] = true
	}
	var out []Row
	for _, r := range m.rows[batchID] {
		if (len(want) > 0 && !want[r.State]) || (f.FileID != "" && r.FileID != f.FileID) || (len(nos) > 0 && !nos[r.RowNo]) || r.RowNo <= f.After {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RowNo != out[j].RowNo {
			return out[i].RowNo < out[j].RowNo
		}
		return out[i].FileID < out[j].FileID
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// RowCount implements Repo.
func (m *MemRepo) RowCount(ctx context.Context, batchID, fileID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, batchID); !ok {
		return 0, ErrNotFound
	}
	n := 0
	for _, r := range m.rows[batchID] {
		if r.FileID == fileID {
			n++
		}
	}
	return n, nil
}

// RowStateCounts implements Repo.
func (m *MemRepo) RowStateCounts(ctx context.Context, batchID string) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owned(ctx, batchID); !ok {
		return nil, ErrNotFound
	}
	out := map[string]int{}
	for _, r := range m.rows[batchID] {
		out[r.State]++
	}
	return out, nil
}

// UpdateRows implements Repo.
func (m *MemRepo) UpdateRows(ctx context.Context, rows []Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rows {
		if _, ok := m.owned(ctx, r.BatchID); !ok {
			return ErrNotFound
		}
		for i := range m.rows[r.BatchID] {
			x := &m.rows[r.BatchID][i]
			if x.RowID == r.RowID {
				x.State, x.ReasonCode, x.UpdatedAt = r.State, r.ReasonCode, r.UpdatedAt
			}
		}
	}
	return nil
}

// DueForProcessing implements Repo.
func (m *MemRepo) DueForProcessing(_ context.Context, now time.Time, limit int) ([]DueRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DueRef
	for _, b := range m.b {
		due := false
		switch b.State {
		case StateReceived:
			due = b.NextProcessAt != nil && !b.NextProcessAt.After(now)
		case StateValidating, StateAccepted:
			due = b.LeaseUntil == nil || !b.LeaseUntil.After(now)
		case StateProcessing, StatePartial:
			due = (b.NextProcessAt != nil && !b.NextProcessAt.After(now)) || (!b.ProcessingDone && (b.LeaseUntil == nil || !b.LeaseUntil.After(now)))
		}
		if due {
			out = append(out, DueRef{TenantID: b.TenantID, ID: b.BatchID})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PurgeContent implements Repo.
func (m *MemRepo) PurgeContent(_ context.Context, now time.Time, _ int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id := range m.files {
		for i := range m.files[id] {
			f := &m.files[id][i]
			if len(f.Content) > 0 && f.ContentExpiresAt != nil && !f.ContentExpiresAt.After(now) {
				f.Content, f.ContentRetained = nil, false
				n++
			}
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

// ── replay jobs ──────────────────────────────────────────────────────────────

// InsertReplay implements Repo.
func (m *MemRepo) InsertReplay(ctx context.Context, j *ReplayJob, items []ReplayItem, ev OutEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.TenantID != middleware.GetTenantID(ctx) {
		return ErrReplayNotFound
	}
	c := *j
	m.replays[j.ReplayID] = &c
	m.items[j.ReplayID] = append([]ReplayItem(nil), items...)
	m.Out = append(m.Out, ev)
	return nil
}

func (m *MemRepo) ownedReplay(ctx context.Context, id string) (*ReplayJob, bool) {
	j, ok := m.replays[id]
	if !ok || j.TenantID != middleware.GetTenantID(ctx) {
		return nil, false
	}
	return j, true
}

// GetReplay implements Repo.
func (m *MemRepo) GetReplay(ctx context.Context, id string) (*ReplayJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.ownedReplay(ctx, id)
	if !ok {
		return nil, ErrReplayNotFound
	}
	c := *j
	return &c, nil
}

// ListReplays implements Repo.
func (m *MemRepo) ListReplays(ctx context.Context, f ReplayFilter) ([]ReplayJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ReplayJob{}
	for _, j := range m.replays {
		if j.TenantID != middleware.GetTenantID(ctx) || (f.State != "" && j.State != f.State) || (f.BatchID != "" && j.BatchID != f.BatchID) {
			continue
		}
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.After(out[k].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ReplayItems implements Repo.
func (m *MemRepo) ReplayItems(ctx context.Context, id string, limit int) ([]ReplayItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.ownedReplay(ctx, id); !ok {
		return nil, ErrReplayNotFound
	}
	out := append([]ReplayItem(nil), m.items[id]...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var replayGraph = map[string][]string{
	ReplayPendingApproval: {ReplayApproved, ReplayRejected, ReplayCancelled},
	ReplayApproved:        {ReplayRunning, ReplayCancelled},
	ReplayRunning:         {ReplayCompleted, ReplayFailed},
}

// MutateReplay implements Repo.
func (m *MemRepo) MutateReplay(ctx context.Context, id string, fn func(tx *ReplayTx) error) (*ReplayJob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.ownedReplay(ctx, id)
	if !ok {
		return nil, ErrReplayNotFound
	}
	w := *cur
	tx := &ReplayTx{J: &w}
	if err := fn(tx); err != nil {
		return nil, err
	}
	if w.State != cur.State {
		okMove := false
		for _, to := range replayGraph[cur.State] {
			if to == w.State {
				okMove = true
			}
		}
		if !okMove {
			return nil, ErrTransition
		}
		if cur.State == ReplayPendingApproval && (w.State == ReplayApproved || w.State == ReplayRejected) && (w.DecidedBy == "" || w.DecidedBy == w.RequestedBy) {
			return nil, ErrSelfAction
		}
	}
	w.Version = cur.Version + 1
	w.UpdatedAt = time.Now().UTC()
	m.replays[id] = &w
	m.Out = append(m.Out, tx.Out...)
	c := w
	return &c, nil
}

// DueReplays implements Repo.
func (m *MemRepo) DueReplays(_ context.Context, limit int) ([]DueRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	var out []DueRef
	for _, j := range m.replays {
		if j.State == ReplayApproved || (j.State == ReplayRunning && (j.LeaseUntil == nil || !j.LeaseUntil.After(now))) {
			out = append(out, DueRef{TenantID: j.TenantID, ID: j.ReplayID})
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// UpdateReplayItems implements Repo.
func (m *MemRepo) UpdateReplayItems(ctx context.Context, items []ReplayItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range items {
		list := m.items[it.ReplayID]
		for i := range list {
			if list[i].ExchangeID == it.ExchangeID {
				list[i] = it
			}
		}
	}
	return nil
}

var _ Repo = (*MemRepo)(nil)
