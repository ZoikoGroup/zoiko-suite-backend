package store

import (
	"context"

	"zoiko.io/esignature-integration-svc/internal/domain"
)

type Store interface {
	CreateEnvelope(ctx context.Context, env *domain.SignatureEnvelope) error
	GetEnvelopeByID(ctx context.Context, id string) (*domain.SignatureEnvelope, error)
	ListEnvelopes(ctx context.Context, legalEntityID string) ([]domain.SignatureEnvelope, error)
	UpdateEnvelopeStatus(ctx context.Context, id string, req *domain.UpdateStatusRequest) (*domain.SignatureEnvelope, error)
}
