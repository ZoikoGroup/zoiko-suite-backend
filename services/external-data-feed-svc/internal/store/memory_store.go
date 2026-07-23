package store

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"zoiko.io/external-data-feed-svc/internal/domain"
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

func (m *MemoryStore) CreateSubscription(ctx context.Context, sub *domain.DataFeedSubscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sub.FeedID == "" {
		sub.FeedID = uuid.New().String()
	}
	now := time.Now().UTC()
	sub.CreatedAt = now
	sub.UpdatedAt = now
	m.subscriptions[sub.FeedID] = sub
	return nil
}

func (m *MemoryStore) GetSubscriptionByID(ctx context.Context, id string) (*domain.DataFeedSubscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sub, ok := m.subscriptions[id]
	if !ok {
		return nil, domain.ErrFeedNotFound
	}
	return sub, nil
}

func (m *MemoryStore) ListSubscriptions(ctx context.Context, legalEntityID string) ([]domain.DataFeedSubscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]domain.DataFeedSubscription, 0)
	for _, sub := range m.subscriptions {
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
	m.events[event.EventID] = event
	return nil
}

func (m *MemoryStore) ListEvents(ctx context.Context, feedID string) ([]domain.DataFeedEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]domain.DataFeedEvent, 0)
	for _, ev := range m.events {
		if feedID == "" || ev.FeedID == feedID {
			res = append(res, *ev)
		}
	}
	return res, nil
}
