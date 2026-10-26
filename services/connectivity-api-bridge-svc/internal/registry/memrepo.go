package registry

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// MemRepo is an in-memory Repo for unit and handler tests. It enforces the same visibility, platform-scope,
// idempotency, uniqueness and immutability rules as the Postgres repo, and records the events it is given so
// tests can assert on them. The Postgres tests are the proof that the database holds those rules itself.
type MemRepo struct {
	mu          sync.Mutex
	connectors  map[string]*Connector
	versions    map[string]*ContractVersion
	gates       map[string][]Gate
	transitions map[string][]Transition
	runs        map[string][]ValidationRun
	idem        map[string]idemRec
	Events      []Event
}

type idemRec struct{ hash, id string }

// NewMemRepo returns an empty MemRepo.
func NewMemRepo() *MemRepo {
	return &MemRepo{connectors: map[string]*Connector{}, versions: map[string]*ContractVersion{}, gates: map[string][]Gate{},
		transitions: map[string][]Transition{}, runs: map[string][]ValidationRun{}, idem: map[string]idemRec{}}
}

func isPlatform(ctx context.Context) bool { return middleware.GetTenantID(ctx) == PlatformScopeID }

func connectorVisible(ctx context.Context, c *Connector) bool {
	if isPlatform(ctx) {
		return true
	}
	return c.Lifecycle == ConnectorPublished || c.Lifecycle == ConnectorDeprecated || c.Lifecycle == ConnectorRetired
}

func versionVisible(ctx context.Context, v *ContractVersion) bool {
	if isPlatform(ctx) {
		return true
	}
	return v.Status == VersionPublished || v.Status == VersionSuperseded || v.Status == VersionRetired
}

func cloneV(v *ContractVersion) *ContractVersion {
	raw, _ := json.Marshal(v)
	var out ContractVersion
	_ = json.Unmarshal(raw, &out)
	return &out
}

func (m *MemRepo) idemKey(ctx context.Context, kind, key string) string {
	return middleware.GetTenantID(ctx) + "|" + kind + "|" + key
}

func (m *MemRepo) CreateConnector(ctx context.Context, c *Connector, idemKey, idemHash string, e Event) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !isPlatform(ctx) {
		return false, ErrNotPlatformScope
	}
	if idemKey != "" {
		if rec, ok := m.idem[m.idemKey(ctx, "connector", idemKey)]; ok {
			if rec.hash != idemHash {
				return false, ErrIdempotency
			}
			*c = *m.connectors[rec.id]
			return false, nil
		}
	}
	if _, ok := m.connectors[c.ConnectorID]; ok {
		return false, ErrConnectorExists
	}
	cp := *c
	m.connectors[c.ConnectorID] = &cp
	if idemKey != "" {
		m.idem[m.idemKey(ctx, "connector", idemKey)] = idemRec{idemHash, c.ConnectorID}
	}
	m.Events = append(m.Events, e)
	return true, nil
}

func (m *MemRepo) GetConnector(ctx context.Context, id string) (*Connector, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.connectors[id]
	if !ok || !connectorVisible(ctx, c) {
		return nil, ErrConnectorNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *MemRepo) ListConnectors(ctx context.Context, family, lifecycle string) ([]Connector, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Connector{}
	for _, c := range m.connectors {
		if connectorVisible(ctx, c) && (family == "" || c.Family == family) && (lifecycle == "" || c.Lifecycle == lifecycle) {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectorID < out[j].ConnectorID })
	return out, nil
}

func (m *MemRepo) MutateConnector(ctx context.Context, id string, fn func(c *Connector) (*Event, error)) (*Connector, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !isPlatform(ctx) {
		return nil, ErrNotPlatformScope
	}
	c, ok := m.connectors[id]
	if !ok {
		return nil, ErrConnectorNotFound
	}
	cp := *c
	e, err := fn(&cp)
	if err != nil {
		return nil, err
	}
	cp.Version++
	*c = cp
	if e != nil {
		m.Events = append(m.Events, *e)
	}
	out := *c
	return &out, nil
}

func (m *MemRepo) CreateVersion(ctx context.Context, v *ContractVersion, idemKey, idemHash string, e Event) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !isPlatform(ctx) {
		return false, ErrNotPlatformScope
	}
	if idemKey != "" {
		if rec, ok := m.idem[m.idemKey(ctx, "version", idemKey)]; ok {
			if rec.hash != idemHash {
				return false, ErrIdempotency
			}
			v.ContractVersionID = rec.id
			return false, nil
		}
	}
	for _, x := range m.versions {
		if x.ConnectorID == v.ConnectorID && x.Semver == v.Semver {
			return false, ErrVersionExists
		}
	}
	m.versions[v.ContractVersionID] = cloneV(v)
	if idemKey != "" {
		m.idem[m.idemKey(ctx, "version", idemKey)] = idemRec{idemHash, v.ContractVersionID}
	}
	m.Events = append(m.Events, e)
	return true, nil
}

func (m *MemRepo) GetVersion(ctx context.Context, id string) (*ContractVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.versions[id]
	if !ok || !versionVisible(ctx, v) {
		return nil, ErrVersionNotFound
	}
	return cloneV(v), nil
}

func (m *MemRepo) ListVersions(ctx context.Context, connectorID string) ([]ContractVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ContractVersion{}
	for _, v := range m.versions {
		if v.ConnectorID == connectorID && versionVisible(ctx, v) {
			out = append(out, *cloneV(v))
		}
	}
	sort.Slice(out, func(i, j int) bool { return semverCompare(out[i].Semver, out[j].Semver) < 0 })
	return out, nil
}

func (m *MemRepo) baseline(connectorID string) *ContractVersion {
	for _, v := range m.versions {
		if v.ConnectorID == connectorID && v.Status == VersionPublished {
			return cloneV(v)
		}
	}
	return nil
}

func (m *MemRepo) Baseline(ctx context.Context, connectorID string) (*ContractVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baseline(connectorID), nil
}

func (m *MemRepo) ReplaceDraft(ctx context.Context, v *ContractVersion, expectedVersion int64, e Event) (*ContractVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !isPlatform(ctx) {
		return nil, ErrNotPlatformScope
	}
	cur, ok := m.versions[v.ContractVersionID]
	if !ok {
		return nil, ErrVersionNotFound
	}
	if cur.Status != VersionDraft {
		return nil, ErrNotDraft
	}
	if cur.RowVersion != expectedVersion {
		return nil, ErrVersionConflict
	}
	for _, x := range m.versions {
		if x.ConnectorID == v.ConnectorID && x.Semver == v.Semver && x.ContractVersionID != v.ContractVersionID {
			return nil, ErrVersionExists
		}
	}
	n := cloneV(v)
	n.Status, n.RowVersion = VersionDraft, cur.RowVersion+1
	m.versions[v.ContractVersionID] = n
	m.Events = append(m.Events, e)
	return cloneV(n), nil
}

func (m *MemRepo) MutateVersion(ctx context.Context, id string, fn func(tx *VersionTx) error) (*ContractVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !isPlatform(ctx) {
		return nil, ErrNotPlatformScope
	}
	cur, ok := m.versions[id]
	if !ok {
		return nil, ErrVersionNotFound
	}
	conn := *m.connectors[cur.ConnectorID]
	tx := &VersionTx{V: cloneV(cur), Connector: &conn, Gates: append([]Gate{}, m.gates[id]...), Baseline: m.baseline(cur.ConnectorID)}
	if err := fn(tx); err != nil {
		return nil, err
	}
	changed := tx.V.Status != cur.Status || tx.V.ContentHash != cur.ContentHash
	if changed {
		tx.V.RowVersion = cur.RowVersion + 1
	}
	if tx.Supersede != "" {
		if old, ok := m.versions[tx.Supersede]; ok {
			old.Status = VersionSuperseded
			old.RowVersion++
		}
	}
	m.versions[id] = cloneV(tx.V)
	if tx.NewGate != nil {
		m.gates[id] = append(m.gates[id], *tx.NewGate)
	}
	if tx.NewRun != nil {
		m.runs[id] = append(m.runs[id], *tx.NewRun)
	}
	if tx.Transition != nil {
		m.transitions[id] = append(m.transitions[id], *tx.Transition)
	}
	if tx.ConnectorLifecycle != "" {
		m.connectors[cur.ConnectorID].Lifecycle = tx.ConnectorLifecycle
	}
	m.Events = append(m.Events, tx.Events...)
	return cloneV(m.versions[id]), nil
}

func (m *MemRepo) ListGates(ctx context.Context, id string) ([]Gate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.versions[id]; !ok || !versionVisible(ctx, v) {
		return nil, ErrVersionNotFound
	}
	return append([]Gate{}, m.gates[id]...), nil
}

func (m *MemRepo) ListTransitions(ctx context.Context, id string) ([]Transition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.versions[id]; !ok || !versionVisible(ctx, v) {
		return nil, ErrVersionNotFound
	}
	return append([]Transition{}, m.transitions[id]...), nil
}

func (m *MemRepo) ListValidationRuns(ctx context.Context, id string) ([]ValidationRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.versions[id]; !ok || !versionVisible(ctx, v) {
		return nil, ErrVersionNotFound
	}
	return append([]ValidationRun{}, m.runs[id]...), nil
}

func (m *MemRepo) GetMapping(ctx context.Context, id string) (*MappingVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.versions {
		if !versionVisible(ctx, v) {
			continue
		}
		for _, mv := range v.MappingVersions {
			if mv.MappingVersionID == id {
				cp := mv
				switch v.Status {
				case VersionPublished:
					cp.Status = MappingPublished
				case VersionSuperseded:
					cp.Status = MappingSuperseded
				case VersionRetired:
					cp.Status = MappingRetired
				default:
					cp.Status = MappingDraft
				}
				return &cp, nil
			}
		}
	}
	return nil, ErrMappingNotFound
}

var _ Repo = (*MemRepo)(nil)
