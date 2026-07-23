package store

import (
	"context"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
)

type Store interface {
	CreateBridge(ctx context.Context, b *domain.ApiBridge) error
	GetBridgeByID(ctx context.Context, id string) (*domain.ApiBridge, error)
	ListBridges(ctx context.Context, legalEntityID string) ([]domain.ApiBridge, error)
	RecordIngestion(ctx context.Context, log *domain.IngestionLog) error
	ListIngestionLogs(ctx context.Context, bridgeID string) ([]domain.IngestionLog, error)
}
