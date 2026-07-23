package store

import (
	"context"

	"zoiko.io/external-data-feed-svc/internal/domain"
)

type Store interface {
	CreateSubscription(ctx context.Context, sub *domain.DataFeedSubscription) error
	GetSubscriptionByID(ctx context.Context, id string) (*domain.DataFeedSubscription, error)
	ListSubscriptions(ctx context.Context, legalEntityID string) ([]domain.DataFeedSubscription, error)
	IngestEvent(ctx context.Context, event *domain.DataFeedEvent) error
	ListEvents(ctx context.Context, feedID string) ([]domain.DataFeedEvent, error)
}
