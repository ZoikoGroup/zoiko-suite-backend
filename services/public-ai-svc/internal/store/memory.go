package store

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"
)

// Memory is an in-process Store for unit tests and local runs. It honours the
// same atomicity rules as Postgres by holding one mutex per call.
type Memory struct {
	mu          sync.Mutex
	sessions    map[string]*memSession
	byGrant     map[string]string
	sessionIdem map[string]idemEntry
	turns       map[string]*Turn // key: sessionID + "\x00" + clientTurnID
	releases    map[string]Release
	pointers    map[string]string
	revocations []Revocation
	conflicts   map[string]Conflict
	flags       map[string]Flag
	usage       map[string]int64
	outbox      []OutboxEvent
	published   map[int64]bool
	nextOutbox  int64
}

type memSession struct {
	Session
	seq          int
	activeTurnID string
	activeUntil  time.Time
}

type idemEntry struct {
	sessionID string
	hash      []byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		sessions:    map[string]*memSession{},
		byGrant:     map[string]string{},
		sessionIdem: map[string]idemEntry{},
		turns:       map[string]*Turn{},
		releases:    map[string]Release{},
		pointers:    map[string]string{},
		conflicts:   map[string]Conflict{},
		flags:       map[string]Flag{},
		usage:       map[string]int64{},
		published:   map[int64]bool{},
	}
}

func (m *Memory) Ping(context.Context) error { return nil }

func (m *Memory) CreateSession(_ context.Context, s Session, key string, hash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.SessionID] = &memSession{Session: s}
	m.byGrant[string(s.GrantHash)] = s.SessionID
	if key != "" {
		m.sessionIdem[key] = idemEntry{sessionID: s.SessionID, hash: append([]byte(nil), hash...)}
	}
	return nil
}

func (m *Memory) SessionByIdempotencyKey(_ context.Context, key string) (Session, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.sessionIdem[key]
	if !ok {
		return Session{}, nil, ErrNotFound
	}
	s, ok := m.sessions[e.sessionID]
	if !ok {
		return Session{}, nil, ErrNotFound
	}
	return s.Session, e.hash, nil
}

func (m *Memory) SessionByGrant(_ context.Context, grantHash []byte) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byGrant[string(grantHash)]
	if !ok {
		return Session{}, ErrNotFound
	}
	return m.sessions[id].Session, nil
}

func (m *Memory) RotateGrant(_ context.Context, sessionID string, newHash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return ErrNotFound
	}
	delete(m.byGrant, string(s.GrantHash))
	s.GrantHash = append([]byte(nil), newHash...)
	m.byGrant[string(newHash)] = sessionID
	return nil
}

func (m *Memory) DeleteSession(_ context.Context, sessionID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return ErrNotFound
	}
	if s.DeletedAt == nil {
		s.DeletedAt = &at
	}
	delete(m.byGrant, string(s.GrantHash))
	for k, t := range m.turns {
		if t.SessionID == sessionID {
			delete(m.turns, k)
		}
	}
	return nil
}

func turnKey(sessionID, clientTurnID string) string { return sessionID + "\x00" + clientTurnID }

func (m *Memory) BeginTurn(_ context.Context, t Turn, lockUntil time.Time) (BeginResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[t.SessionID]
	if !ok || s.DeletedAt != nil {
		return BeginResult{}, ErrNotFound
	}
	if prev, ok := m.turns[turnKey(t.SessionID, t.ClientTurnID)]; ok {
		if !bytes.Equal(prev.RequestHash, t.RequestHash) {
			return BeginResult{}, ErrIdempotencyMismatch
		}
		if prev.Completed {
			return BeginResult{Turn: *prev, Replay: true}, nil
		}
		return BeginResult{}, ErrTurnConflict
	}
	if s.activeTurnID != "" && time.Now().Before(s.activeUntil) {
		return BeginResult{}, ErrTurnConflict
	}
	s.seq++
	s.activeTurnID, s.activeUntil = t.TurnID, lockUntil
	t.Sequence = s.seq
	stored := t
	m.turns[turnKey(t.SessionID, t.ClientTurnID)] = &stored
	return BeginResult{Turn: t}, nil
}

func (m *Memory) CompleteTurn(_ context.Context, t Turn, buckets []string, tokens int64, events []OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.turns[turnKey(t.SessionID, t.ClientTurnID)]
	if !ok || cur.TurnID != t.TurnID {
		return ErrNotFound
	}
	cur.Status, cur.IntentID, cur.Response, cur.Completed = t.Status, t.IntentID, t.Response, true
	if s, ok := m.sessions[t.SessionID]; ok && s.activeTurnID == t.TurnID {
		s.activeTurnID = ""
	}
	for _, b := range buckets {
		m.usage[b] += tokens
	}
	m.appendOutbox(events)
	return nil
}

func (m *Memory) AbortTurn(_ context.Context, sessionID, turnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, t := range m.turns {
		if t.SessionID == sessionID && t.TurnID == turnID && !t.Completed {
			delete(m.turns, k)
		}
	}
	if s, ok := m.sessions[sessionID]; ok && s.activeTurnID == turnID {
		s.activeTurnID = ""
	}
	return nil
}

func (m *Memory) PutRelease(_ context.Context, r Release) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.releases[r.ReleaseID]; exists {
		return nil // immutable: first write wins
	}
	m.releases[r.ReleaseID] = r
	return nil
}

func (m *Memory) GetRelease(_ context.Context, id string) (Release, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.releases[id]
	if !ok {
		return Release{}, ErrNotFound
	}
	return r, nil
}

func (m *Memory) SetPointer(_ context.Context, kind, id string, events []OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.releases[id]
	if !ok || r.Kind != kind {
		return ErrNotFound
	}
	m.pointers[kind] = id
	m.appendOutbox(events)
	return nil
}

func (m *Memory) GetPointer(_ context.Context, kind string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.pointers[kind]
	if !ok {
		return "", ErrNotFound
	}
	return id, nil
}

func (m *Memory) AddRevocation(_ context.Context, r Revocation, events []OutboxEvent) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.Watermark = int64(len(m.revocations) + 1)
	m.revocations = append(m.revocations, r)
	m.appendOutbox(events)
	return r.Watermark, nil
}

func (m *Memory) Revocations(_ context.Context, after int64) ([]Revocation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Revocation
	for _, r := range m.revocations {
		if r.Watermark > after {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *Memory) RecordConflict(_ context.Context, c Conflict) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.conflicts[c.ConflictID]; !ok {
		m.conflicts[c.ConflictID] = c
	}
	return nil
}

func (m *Memory) SetFlag(_ context.Context, f Flag, events []OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flags[f.Name] = f
	m.appendOutbox(events)
	return nil
}

func (m *Memory) Flags(context.Context) ([]Flag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Flag, 0, len(m.flags))
	for _, f := range m.flags {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) Usage(_ context.Context, bucket string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage[bucket], nil
}

func (m *Memory) appendOutbox(events []OutboxEvent) {
	for _, e := range events {
		dup := false
		for _, x := range m.outbox {
			if x.EventID == e.EventID {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		m.nextOutbox++
		e.OutboxID = m.nextOutbox
		m.outbox = append(m.outbox, e)
	}
}

func (m *Memory) UnpublishedEvents(_ context.Context, limit int) ([]OutboxEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []OutboxEvent
	for _, e := range m.outbox {
		if !m.published[e.OutboxID] {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) MarkPublished(_ context.Context, ids []int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.published[id] = true
	}
	return nil
}

func (m *Memory) MarkAttempt(_ context.Context, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		for _, id := range ids {
			if m.outbox[i].OutboxID == id {
				m.outbox[i].Attempts++
			}
		}
	}
	return nil
}
