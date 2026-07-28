package store

import (
	"context"

	"zoiko.io/banking-connector-svc/internal/domain"
)

type Store interface {
	CreateConnection(ctx context.Context, c *domain.BankConnection) error
	GetConnectionByID(ctx context.Context, id string) (*domain.BankConnection, error)
	ListConnections(ctx context.Context, legalEntityID string) ([]domain.BankConnection, error)
	RecordStatement(ctx context.Context, stmt *domain.BankStatement) error
	ListStatements(ctx context.Context, connectionID string) ([]domain.BankStatement, error)
}
