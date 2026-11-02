// Package testsupport provides in-memory fakes shared by the service and
// handler unit tests. It is never imported by production code; the real
// store is exercised by internal/store's database tests.
package testsupport

import (
	"context"
	"sort"
	"sync"

	"zoiko.io/decision-support-svc/internal/authz"
	"zoiko.io/decision-support-svc/internal/clients"
	"zoiko.io/decision-support-svc/internal/domain"
)

// MemStore is an in-memory service.Store with the same tenant, version and
// idempotency semantics as the Postgres store.
type MemStore struct {
	mu              sync.Mutex
	recommendations map[string]domain.Recommendation // key: tenant|id
	dispositions    map[string][]domain.Disposition  // key: tenant|id
	idempotency     map[string]domain.IdempotencyRecord
	Outbox          []domain.OutboxMessage

	// FailWith, when set, is returned by every method (simulates an outage).
	FailWith error
}

// NewMemStore builds an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		recommendations: map[string]domain.Recommendation{},
		dispositions:    map[string][]domain.Disposition{},
		idempotency:     map[string]domain.IdempotencyRecord{},
	}
}

func key(tenantID, id string) string { return tenantID + "|" + id }

// FindIdempotencyRecord implements service.Store.
func (m *MemStore) FindIdempotencyRecord(_ context.Context, tenantID, idemKey string) (*domain.IdempotencyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWith != nil {
		return nil, m.FailWith
	}
	record, found := m.idempotency[key(tenantID, idemKey)]
	if !found {
		return nil, nil
	}
	return &record, nil
}

// InsertRecommendation implements service.Store.
func (m *MemStore) InsertRecommendation(_ context.Context, write domain.NewRecommendationWrite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWith != nil {
		return m.FailWith
	}
	rec := write.Recommendation
	if _, taken := m.idempotency[key(rec.TenantID, write.Idempotency.Key)]; taken {
		return domain.ErrIdempotencyRace
	}
	m.idempotency[key(rec.TenantID, write.Idempotency.Key)] = write.Idempotency
	m.recommendations[key(rec.TenantID, rec.RecommendationID)] = rec
	m.dispositions[key(rec.TenantID, rec.RecommendationID)] = []domain.Disposition{write.Disposition}
	m.Outbox = append(m.Outbox, write.Outbox...)
	return nil
}

// GetRecommendation implements service.Store.
func (m *MemStore) GetRecommendation(_ context.Context, tenantID, id string) (*domain.Recommendation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWith != nil {
		return nil, m.FailWith
	}
	rec, found := m.recommendations[key(tenantID, id)]
	if !found {
		return nil, domain.ErrRecommendationNotFound
	}
	return &rec, nil
}

// ListRecommendations implements service.Store.
func (m *MemStore) ListRecommendations(_ context.Context, tenantID string, filter domain.ListFilter) ([]domain.Recommendation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWith != nil {
		return nil, m.FailWith
	}
	var out []domain.Recommendation
	for _, rec := range m.recommendations {
		if rec.TenantID != tenantID {
			continue
		}
		if filter.LegalEntityID != "" && rec.LegalEntityID != filter.LegalEntityID {
			continue
		}
		if filter.Status != "" && rec.Status != filter.Status {
			continue
		}
		if filter.SubjectReference != "" && rec.SubjectReference != filter.SubjectReference {
			continue
		}
		if filter.After != nil && !isBefore(rec, *filter.After) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].RecommendationID > out[j].RecommendationID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func isBefore(rec domain.Recommendation, cursor domain.PageCursor) bool {
	if rec.CreatedAt.Equal(cursor.CreatedAt) {
		return rec.RecommendationID < cursor.RecommendationID
	}
	return rec.CreatedAt.Before(cursor.CreatedAt)
}

// ApplyTransition implements service.Store.
func (m *MemStore) ApplyTransition(_ context.Context, write domain.TransitionWrite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWith != nil {
		return m.FailWith
	}
	rec := write.Updated
	if _, taken := m.idempotency[key(rec.TenantID, write.Idempotency.Key)]; taken {
		return domain.ErrIdempotencyRace
	}
	stored, found := m.recommendations[key(rec.TenantID, rec.RecommendationID)]
	if !found || stored.Version != write.ExpectedVersion || stored.Status != write.FromStatus {
		return domain.ErrVersionMismatch
	}
	if write.ReplacementID != nil {
		replacement, ok := m.recommendations[key(rec.TenantID, *write.ReplacementID)]
		if !ok || replacement.Status != domain.StatusReviewRequired {
			return domain.ErrReplacementNotPending
		}
		replacement.SupersedesRecommendationID = &rec.RecommendationID
		replacement.Version++
		m.recommendations[key(rec.TenantID, *write.ReplacementID)] = replacement
	}
	m.idempotency[key(rec.TenantID, write.Idempotency.Key)] = write.Idempotency
	m.recommendations[key(rec.TenantID, rec.RecommendationID)] = rec
	m.dispositions[key(rec.TenantID, rec.RecommendationID)] = append(m.dispositions[key(rec.TenantID, rec.RecommendationID)], write.Disposition)
	m.Outbox = append(m.Outbox, write.Outbox...)
	return nil
}

// ListDispositions implements service.Store.
func (m *MemStore) ListDispositions(_ context.Context, tenantID, id string) ([]domain.Disposition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWith != nil {
		return nil, m.FailWith
	}
	return append([]domain.Disposition(nil), m.dispositions[key(tenantID, id)]...), nil
}

// Put stores rec directly (test setup).
func (m *MemStore) Put(rec domain.Recommendation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recommendations[key(rec.TenantID, rec.RecommendationID)] = rec
}

// OutboxOfType returns the outbox messages with the given type.
func (m *MemStore) OutboxOfType(messageType string) []domain.OutboxMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.OutboxMessage
	for _, msg := range m.Outbox {
		if msg.MessageType == messageType {
			out = append(out, msg)
		}
	}
	return out
}

// Authorizer is a fake authz.Client: it returns Err for every check (nil =
// GRANT) and records the requests it saw.
type Authorizer struct {
	mu       sync.Mutex
	Err      error
	Requests []authz.Request
}

// CheckAllowed implements service.Authorizer.
func (a *Authorizer) CheckAllowed(_ context.Context, req authz.Request) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Requests = append(a.Requests, req)
	return a.Err
}

// History is a fake governance decision log reader.
type History struct {
	Decisions []clients.PriorDecision
	Err       error
	Queries   []clients.HistoryQuery
}

// ListPriorDecisions implements service.HistorySource.
func (h *History) ListPriorDecisions(_ context.Context, q clients.HistoryQuery) ([]clients.PriorDecision, error) {
	h.Queries = append(h.Queries, q)
	if h.Err != nil {
		return nil, h.Err
	}
	return h.Decisions, nil
}

// Outcomes builds prior decisions from outcome names.
func Outcomes(names ...string) []clients.PriorDecision {
	out := make([]clients.PriorDecision, 0, len(names))
	for _, n := range names {
		out = append(out, clients.PriorDecision{Outcome: n})
	}
	return out
}
