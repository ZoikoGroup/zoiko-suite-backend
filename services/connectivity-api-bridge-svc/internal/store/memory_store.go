package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// MemoryStore is the in-process Store. It is not "just the test double": it
// enforces the same tenant scoping, optimistic locking, idempotency, no-rollback
// and separation-of-duties rules as PgStore, so a handler or service test that
// passes against it says something about production. Mutations are applied to a
// copy and committed only if the whole function succeeds, which is what a
// transaction does.
type MemoryStore struct {
	mu          sync.Mutex
	region      string
	bridges     map[string]*domain.ApiBridge
	logs        map[string][]*domain.IngestionLog
	changes     map[string][]domain.Change
	approvals   map[string][]domain.Approval
	health      map[string]*domain.Health
	transitions map[string][]domain.HealthTransition
	checkpoints map[string]*domain.Checkpoint // connector|stream
	resets      map[string]*domain.ResetRequest
	profiles    map[string]*domain.Profile // key@version
	idem        map[string]idemEntry       // tenant|key
	events      []Emitted
}

type idemEntry struct {
	bridgeID string
	hash     string
}

// Emitted is an event the store enqueued, recorded for tests.
type Emitted struct {
	TenantID string
	Event    domain.OutboxEvent
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		region:      "uk",
		bridges:     map[string]*domain.ApiBridge{},
		logs:        map[string][]*domain.IngestionLog{},
		changes:     map[string][]domain.Change{},
		approvals:   map[string][]domain.Approval{},
		health:      map[string]*domain.Health{},
		transitions: map[string][]domain.HealthTransition{},
		checkpoints: map[string]*domain.Checkpoint{},
		resets:      map[string]*domain.ResetRequest{},
		profiles:    map[string]*domain.Profile{},
		idem:        map[string]idemEntry{},
	}
}

// Events returns the events enqueued so far, oldest first.
func (m *MemoryStore) Events() []Emitted {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Emitted(nil), m.events...)
}

// EventTypes returns the types of the events enqueued so far.
func (m *MemoryStore) EventTypes() []string {
	var out []string
	for _, e := range m.Events() {
		out = append(out, e.Event.Type)
	}
	return out
}

func cloneBridge(b *domain.ApiBridge) *domain.ApiBridge {
	c := *b
	c.Scopes = append([]string{}, b.Scopes...)
	return &c
}

func (m *MemoryStore) prepareNew(ctx context.Context, b *domain.ApiBridge) {
	b.TenantID = middleware.GetTenantID(ctx)
	if b.BridgeID == "" {
		b.BridgeID = uuid.New().String()
	}
	now := time.Now().UTC()
	b.CreatedAt, b.UpdatedAt = now, now
	if b.Status == "" {
		b.Status = domain.StatusDraft
	}
	if b.Version < 1 {
		b.Version = 1
	}
	if b.SyncMode == "" {
		b.SyncMode = domain.SyncPush
	}
	if b.DestinationHost == "" {
		b.DestinationHost = domain.EndpointHost(b.EndpointURL)
	}
	if b.Scopes == nil {
		b.Scopes = []string{}
	}
}

func (m *MemoryStore) CreateBridge(ctx context.Context, b *domain.ApiBridge) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prepareNew(ctx, b)
	m.bridges[b.BridgeID] = cloneBridge(b)
	return nil
}

// GetBridgeByID is scoped to the caller's tenant. Returns ErrBridgeNotFound for
// another tenant's id, not a distinct forbidden error, so a probe cannot
// confirm the id exists.
func (m *MemoryStore) GetBridgeByID(ctx context.Context, id string) (*domain.ApiBridge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bridges[id]
	if !ok || b.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrBridgeNotFound
	}
	return cloneBridge(b), nil
}

func (m *MemoryStore) ListBridges(ctx context.Context, legalEntityID string) ([]domain.ApiBridge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.ApiBridge, 0)
	for _, b := range m.bridges {
		if b.TenantID == tenantID && (legalEntityID == "" || b.LegalEntityID == legalEntityID) {
			res = append(res, *cloneBridge(b))
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].CreatedAt.After(res[j].CreatedAt) })
	return res, nil
}

func (m *MemoryStore) RecordIngestion(ctx context.Context, log *domain.IngestionLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if log.LogID == "" {
		log.LogID = uuid.New().String()
	}
	log.IngestedAt = time.Now().UTC()
	log.TenantID = middleware.GetTenantID(ctx)
	c := *log
	m.logs[log.BridgeID] = append(m.logs[log.BridgeID], &c)
	return nil
}

func (m *MemoryStore) ListIngestionLogs(ctx context.Context, bridgeID string) ([]domain.IngestionLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.IngestionLog, 0)
	for _, l := range m.logs[bridgeID] {
		if l.TenantID == tenantID {
			res = append(res, *l)
		}
	}
	return res, nil
}

// ── Governance ────────────────────────────────────────────────────────────────

func (m *MemoryStore) CreateConnector(ctx context.Context, b *domain.ApiBridge, idemKey, idemHash string, fx *domain.Effects) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prepareNew(ctx, b)
	if idemKey != "" {
		if e, ok := m.idem[b.TenantID+"|"+idemKey]; ok {
			if e.hash != idemHash {
				return false, domain.ErrIdempotencyConflict
			}
			*b = *cloneBridge(m.bridges[e.bridgeID])
			return false, nil
		}
	}
	if err := m.checkEffects(b, fx); err != nil {
		return false, err
	}
	m.bridges[b.BridgeID] = cloneBridge(b)
	if idemKey != "" {
		m.idem[b.TenantID+"|"+idemKey] = idemEntry{bridgeID: b.BridgeID, hash: idemHash}
	}
	m.applyEffects(b, nil, fx)
	return true, nil
}

func (m *MemoryStore) MutateConnector(ctx context.Context, id string, fn domain.MutateFunc) (*domain.ApiBridge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.bridges[id]
	if !ok || cur.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrBridgeNotFound
	}
	work := cloneBridge(cur)
	h := m.healthOf(cur)
	prior := *h
	fx, err := fn(work, h)
	if err != nil {
		return nil, err // the copy is discarded: nothing was written
	}
	if err := m.checkEffects(work, fx); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(cur, work) {
		m.bridges[id] = cloneBridge(work)
	}
	m.applyEffects(work, &prior, fx)
	return cloneBridge(work), nil
}

func (m *MemoryStore) healthOf(b *domain.ApiBridge) *domain.Health {
	if h, ok := m.health[b.BridgeID]; ok {
		c := *h
		return &c
	}
	return &domain.Health{ConnectorID: b.BridgeID, TenantID: b.TenantID, State: domain.HealthHealthy}
}

func (m *MemoryStore) applyEffects(b *domain.ApiBridge, prior *domain.Health, fx *domain.Effects) {
	if fx == nil {
		return
	}
	now := time.Now().UTC()
	if c := fx.Change; c != nil {
		cc := *c
		cc.ChangeID, cc.ConnectorID, cc.TenantID, cc.ChangedAt = uuid.New().String(), b.BridgeID, b.TenantID, now
		m.changes[b.BridgeID] = append(m.changes[b.BridgeID], cc)
	}
	if a := fx.Approval; a != nil {
		ac := *a
		ac.ApprovalID, ac.ConnectorID, ac.TenantID, ac.DecidedAt = uuid.New().String(), b.BridgeID, b.TenantID, now
		m.approvals[b.BridgeID] = append(m.approvals[b.BridgeID], ac)
	}
	if h := fx.Health; h != nil {
		from := domain.HealthHealthy
		if prior != nil {
			from = prior.State
		}
		cur := m.healthOf(b)
		cur.State, cur.ReasonCode, cur.Detail, cur.ConsecutiveFailures = h.State, h.ReasonCode, h.Detail, h.ConsecutiveFailures
		cur.LastCheckedAt, cur.UpdatedAt = now, now
		if h.Success {
			t := now
			cur.LastSuccessAt = &t
		}
		m.health[b.BridgeID] = cur
		if from != h.State {
			m.transitions[b.BridgeID] = append(m.transitions[b.BridgeID], domain.HealthTransition{
				TransitionID: uuid.New().String(), ConnectorID: b.BridgeID, TenantID: b.TenantID,
				FromState: from, ToState: h.State, ReasonCode: h.ReasonCode, Detail: h.Detail, At: now,
			})
		}
	}
	if l := fx.Ingestion; l != nil {
		lc := *l
		if lc.LogID == "" {
			lc.LogID = uuid.New().String()
		}
		lc.TenantID, lc.BridgeID, lc.IngestedAt = b.TenantID, b.BridgeID, now
		m.logs[b.BridgeID] = append(m.logs[b.BridgeID], &lc)
		*l = lc
	}
	for _, ev := range fx.Events {
		m.record(b.TenantID, b, ev)
	}
}

// normalize fills an event's defaults from the connector it describes, exactly
// as PgStore does when it writes the outbox row.
func normalize(b *domain.ApiBridge, ev domain.OutboxEvent) domain.OutboxEvent {
	if b != nil {
		if ev.AggregateID == "" {
			ev.AggregateID = b.BridgeID
		}
		if ev.LegalEntityID == "" {
			ev.LegalEntityID = b.LegalEntityID
		}
		if ev.AggregateVersion == nil {
			v := b.Version
			ev.AggregateVersion = &v
		}
	}
	return ev
}

// check refuses an event the canonical envelope would refuse. PgStore builds the
// same envelope, so a test against this store cannot pass on an event Postgres
// would reject.
func (m *MemoryStore) check(tenant string, b *domain.ApiBridge, ev domain.OutboxEvent) error {
	if _, err := BuildEnvelope(m.region, tenant, normalize(b, ev)); err != nil {
		return fmt.Errorf("build %s event: %w", ev.Type, err)
	}
	return nil
}

func (m *MemoryStore) checkEffects(b *domain.ApiBridge, fx *domain.Effects) error {
	if fx == nil {
		return nil
	}
	for _, ev := range fx.Events {
		if err := m.check(b.TenantID, b, ev); err != nil {
			return err
		}
	}
	return nil
}

func (m *MemoryStore) record(tenant string, b *domain.ApiBridge, ev domain.OutboxEvent) {
	m.events = append(m.events, Emitted{TenantID: tenant, Event: normalize(b, ev)})
}

func (m *MemoryStore) owned(ctx context.Context, id string) (*domain.ApiBridge, error) {
	b, ok := m.bridges[id]
	if !ok || b.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrBridgeNotFound
	}
	return b, nil
}

func (m *MemoryStore) ListChanges(ctx context.Context, id string) ([]domain.Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.owned(ctx, id); err != nil {
		return nil, err
	}
	return append([]domain.Change{}, m.changes[id]...), nil
}

func (m *MemoryStore) ListApprovals(ctx context.Context, id string) ([]domain.Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.owned(ctx, id); err != nil {
		return nil, err
	}
	return append([]domain.Approval{}, m.approvals[id]...), nil
}

func (m *MemoryStore) GetHealth(ctx context.Context, id string) (*domain.Health, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := m.owned(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.healthOf(b), nil
}

func (m *MemoryStore) ListHealthTransitions(ctx context.Context, id string, limit int) ([]domain.HealthTransition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.owned(ctx, id); err != nil {
		return nil, err
	}
	all := m.transitions[id]
	out := make([]domain.HealthTransition, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- { // newest first, like the database
		out = append(out, all[i])
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ── Checkpoints ───────────────────────────────────────────────────────────────

func (m *MemoryStore) ListCheckpoints(ctx context.Context, id string) ([]domain.Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.owned(ctx, id); err != nil {
		return nil, err
	}
	out := make([]domain.Checkpoint, 0)
	for k, c := range m.checkpoints {
		if c.ConnectorID == id && k != "" {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stream < out[j].Stream })
	return out, nil
}

func (m *MemoryStore) AdvanceCheckpoint(ctx context.Context, id, stream, cursor string, watermark *time.Time,
	expectedVersion int64, actor string, ev domain.OutboxEvent) (*domain.Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := m.owned(ctx, id)
	if err != nil {
		return nil, err
	}
	if !b.Live() {
		return nil, domain.ErrInvalidTransition
	}
	key := id + "|" + stream
	cur, exists := m.checkpoints[key]
	now := time.Now().UTC()
	var out domain.Checkpoint
	switch {
	case !exists:
		if expectedVersion != 0 {
			return nil, domain.ErrCheckpointConflict
		}
		out = domain.Checkpoint{ConnectorID: id, TenantID: b.TenantID, Stream: stream, Cursor: cursor, Watermark: watermark,
			Version: 1, UpdatedBy: actor, UpdatedAt: now}
	default:
		if expectedVersion != cur.Version {
			return nil, domain.ErrCheckpointConflict
		}
		if cur.Watermark != nil && (watermark == nil || watermark.Before(*cur.Watermark)) {
			return nil, domain.ErrCheckpointRegress
		}
		out = *cur
		out.Cursor, out.Watermark, out.Version, out.UpdatedBy, out.UpdatedAt = cursor, watermark, cur.Version+1, actor, now
	}
	ev.AggregateType, ev.AggregateID, ev.LegalEntityID = "sync-checkpoint", id+"/"+stream, b.LegalEntityID
	v := out.Version
	ev.AggregateVersion = &v
	if err := m.check(b.TenantID, nil, ev); err != nil {
		return nil, err
	}
	c := out
	m.checkpoints[key] = &c
	m.record(b.TenantID, nil, ev)
	return &out, nil
}

func (m *MemoryStore) CreateResetRequest(ctx context.Context, r *domain.ResetRequest, ev domain.OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := m.owned(ctx, r.ConnectorID)
	if err != nil {
		return err
	}
	if _, ok := m.checkpoints[r.ConnectorID+"|"+r.Stream]; !ok {
		return domain.ErrCheckpointNotFound
	}
	r.RequestID, r.TenantID, r.Status, r.CreatedAt = uuid.New().String(), b.TenantID, domain.ResetPending, time.Now().UTC()
	ev.AggregateType, ev.AggregateID, ev.LegalEntityID = "sync-checkpoint", r.ConnectorID+"/"+r.Stream, b.LegalEntityID
	if err := m.check(b.TenantID, nil, ev); err != nil {
		return err
	}
	c := *r
	m.resets[r.RequestID] = &c
	m.record(b.TenantID, nil, ev)
	return nil
}

func (m *MemoryStore) DecideResetRequest(ctx context.Context, requestID, actor string, approve bool, ev domain.OutboxEvent) (*domain.ResetRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.resets[requestID]
	if !ok || r.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrResetNotFound
	}
	if r.Status != domain.ResetPending {
		return nil, domain.ErrResetNotPending
	}
	if actor == "" || actor == r.RequestedBy {
		return nil, domain.ErrSelfApproval
	}
	b := m.bridges[r.ConnectorID]
	now := time.Now().UTC()
	status := domain.ResetRejected
	if approve {
		cp, ok := m.checkpoints[r.ConnectorID+"|"+r.Stream]
		if !ok {
			return nil, domain.ErrCheckpointNotFound
		}
		status = domain.ResetApplied
		cp.Cursor, cp.Watermark, cp.Version, cp.UpdatedBy, cp.UpdatedAt = r.ToCursor, r.ToWatermark, cp.Version+1, actor, now
		v := cp.Version
		ev.AggregateVersion = &v
	}
	r.Status, r.DecidedBy, r.DecidedAt = status, actor, &now
	ev.AggregateType, ev.AggregateID, ev.LegalEntityID = "sync-checkpoint", r.ConnectorID+"/"+r.Stream, b.LegalEntityID
	m.record(r.TenantID, nil, ev)
	out := *r
	return &out, nil
}

// ── Profiles ──────────────────────────────────────────────────────────────────

func profileKey(key string, version int) string { return key + "@" + itoa(version) }

func (m *MemoryStore) PublishProfile(ctx context.Context, p *domain.Profile, ev domain.OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if middleware.GetTenantID(ctx) != PlatformScopeID {
		return domain.ErrPlatformScopeRequired
	}
	k := profileKey(p.ProviderKey, p.Version)
	if _, exists := m.profiles[k]; exists {
		return domain.ErrProfileExists
	}
	p.ProfileID, p.CreatedAt = uuid.New().String(), time.Now().UTC()
	if p.Status == "" {
		p.Status = domain.ProfileDraft
	}
	ev.AggregateType, ev.AggregateID = "external-profile", k
	v := int64(p.Version)
	ev.AggregateVersion = &v
	if err := m.check(middleware.GetTenantID(ctx), nil, ev); err != nil {
		return err
	}
	c := *p
	m.profiles[k] = &c
	m.record(middleware.GetTenantID(ctx), nil, ev)
	return nil
}

func (m *MemoryStore) GetProfile(ctx context.Context, key string, version int) (*domain.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.profiles[profileKey(key, version)]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	c := *p
	return &c, nil
}

func (m *MemoryStore) ListProfiles(ctx context.Context, key string) ([]domain.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Profile, 0)
	for _, p := range m.profiles {
		if key == "" || p.ProviderKey == key {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProviderKey != out[j].ProviderKey {
			return out[i].ProviderKey < out[j].ProviderKey
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

var profileRank = map[string]int{domain.ProfileDraft: 0, domain.ProfileCertified: 1, domain.ProfileDeprecated: 2, domain.ProfileUnsupported: 3}

func (m *MemoryStore) SetProfileStatus(ctx context.Context, key string, version int, status string, at time.Time, sunset *time.Time, ev domain.OutboxEvent) (*domain.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if middleware.GetTenantID(ctx) != PlatformScopeID {
		return nil, domain.ErrProfileNotFound // RLS would match no row
	}
	k := profileKey(key, version)
	p, ok := m.profiles[k]
	if !ok {
		return nil, domain.ErrProfileNotFound
	}
	if profileRank[status] < profileRank[p.Status] {
		return nil, domain.ErrInvalidTransition
	}
	ev.AggregateType, ev.AggregateID = "external-profile", k
	v := int64(version)
	ev.AggregateVersion = &v
	if err := m.check(middleware.GetTenantID(ctx), nil, ev); err != nil {
		return nil, err
	}
	p.Status = status
	if (status == domain.ProfileDeprecated || status == domain.ProfileUnsupported) && p.DeprecatedAt == nil {
		t := at
		p.DeprecatedAt = &t
	}
	if status == domain.ProfileDeprecated && sunset != nil && p.SunsetAt == nil {
		t := *sunset
		p.SunsetAt = &t
	}
	if status == domain.ProfileUnsupported && p.SunsetAt == nil {
		t := at
		p.SunsetAt = &t
	}
	m.record(middleware.GetTenantID(ctx), nil, ev)
	c := *p
	return &c, nil
}

// ResetEvents forgets the events recorded so far, so a test can seed reference
// data and then assert only on what the behaviour under test emits.
func (m *MemoryStore) ResetEvents() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = nil
}
