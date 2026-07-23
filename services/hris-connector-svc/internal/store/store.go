package store

import (
	"context"

	"zoiko.io/hris-connector-svc/internal/domain"
)

type Store interface {
	CreateIntegration(ctx context.Context, integration *domain.HrisIntegration) error
	GetIntegrationByID(ctx context.Context, id string) (*domain.HrisIntegration, error)
	ListIntegrations(ctx context.Context, legalEntityID string) ([]domain.HrisIntegration, error)
	CreateSyncJob(ctx context.Context, job *domain.SyncJob) error
	GetSyncJobByID(ctx context.Context, id string) (*domain.SyncJob, error)
	ListSyncJobs(ctx context.Context, integrationID string) ([]domain.SyncJob, error)
}
