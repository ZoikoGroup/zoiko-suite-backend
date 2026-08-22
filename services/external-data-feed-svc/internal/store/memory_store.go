package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/external-data-feed-svc/internal/domain"
	"zoiko.io/external-data-feed-svc/internal/middleware"
)

type MemoryStore struct {
	mu            sync.RWMutex
	subscriptions map[string]*domain.DataFeedSubscription
	events        map[string]*domain.DataFeedEvent
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		subscriptions: make(map[string]*domain.DataFeedSubscription),
		events:        make(map[string]*domain.DataFeedEvent),
	}
}

// Tenant scoping in this store mirrors PgStore exactly, on purpose. It is
// not "just the test double": a store that isolates tenants in Postgres
// but not in memory means every handler test asserting isolation passes
// against a fake that cannot fail, which is how the unscoped reads here
// survived. Both implementations answer the same question the same way, so
// a handler test proves something about production.
func (m *MemoryStore) CreateSubscription(ctx context.Context, sub *domain.DataFeedSubscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sub.FeedID == "" {
		sub.FeedID = uuid.New().String()
	}
	now := time.Now().UTC()
	sub.CreatedAt = now
	sub.UpdatedAt = now
	// Attribution comes from the verified context, never the caller's payload.
	sub.TenantID = middleware.GetTenantID(ctx)
	m.subscriptions[sub.FeedID] = sub
	return nil
}

// GetSubscriptionByID is scoped to the caller's tenant. Returns
// ErrFeedNotFound for another tenant's id — not a distinct forbidden
// error, so a probe cannot confirm the id exists.
func (m *MemoryStore) GetSubscriptionByID(ctx context.Context, id string) (*domain.DataFeedSubscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sub, ok := m.subscriptions[id]
	if !ok || sub.TenantID != middleware.GetTenantID(ctx) {
		return nil, domain.ErrFeedNotFound
	}
	return sub, nil
}

// ListSubscriptions is scoped to the caller's tenant, with legal entity as
// an optional narrowing WITHIN that tenant.
func (m *MemoryStore) ListSubscriptions(ctx context.Context, legalEntityID string) ([]domain.DataFeedSubscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.DataFeedSubscription, 0)
	for _, sub := range m.subscriptions {
		if sub.TenantID != tenantID {
			continue
		}
		if legalEntityID == "" || sub.LegalEntityID == legalEntityID {
			res = append(res, *sub)
		}
	}
	return res, nil
}

func (m *MemoryStore) IngestEvent(ctx context.Context, event *domain.DataFeedEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.EventID == "" {
		event.EventID = uuid.New().String()
	}
	event.ReceivedAt = time.Now().UTC()
	event.TenantID = middleware.GetTenantID(ctx)
	m.events[event.EventID] = event
	return nil
}

// ListEvents is scoped to the caller's tenant, with feed as an optional
// narrowing WITHIN that tenant.
//
// Unscoped, the feed_id filter was the ONLY predicate, so calling this
// with no feed_id returned every tenant's feed events — including the
// payload, i.e. the actual market/credit/company data flowing through
// their subscriptions.
func (m *MemoryStore) ListEvents(ctx context.Context, feedID string) ([]domain.DataFeedEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenantID := middleware.GetTenantID(ctx)
	res := make([]domain.DataFeedEvent, 0)
	for _, ev := range m.events {
		if ev.TenantID != tenantID {
			continue
		}
		if feedID == "" || ev.FeedID == feedID {
			res = append(res, *ev)
		}
	}
	return res, nil
}
