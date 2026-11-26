package store

import (
	"context"

	"zoiko.io/hris-connector-svc/internal/domain"
	"zoiko.io/hris-connector-svc/internal/events"
)

// Store persists integrations and sync jobs. CreateIntegration and
// CreateSyncJob take the event that announces the change and write it to the
// transactional outbox in the same transaction as the row (internal/outbox):
// both commit or neither does. The event's tenant is always the verified
// tenant of ctx, never the caller's value.
type Store interface {
	CreateIntegration(ctx context.Context, integration *domain.HrisIntegration, event events.PublishParams) error
	GetIntegrationByID(ctx context.Context, id string) (*domain.HrisIntegration, error)
	ListIntegrations(ctx context.Context, legalEntityID string) ([]domain.HrisIntegration, error)
	CreateSyncJob(ctx context.Context, job *domain.SyncJob, event events.PublishParams) error
	GetSyncJobByID(ctx context.Context, id string) (*domain.SyncJob, error)
	ListSyncJobs(ctx context.Context, integrationID string) ([]domain.SyncJob, error)
}
