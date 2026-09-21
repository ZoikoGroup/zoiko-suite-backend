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

// BNK02Store is kept separate from Store — same "self-contained composed
// interface" pattern used for every other capability added to an
// existing service in this build. Only PgStore implements it: MemoryStore
// is unused in production (main.go always wires PgStore) and has no real
// transactional CAS/RLS story an in-memory equivalent could honestly
// provide, so it's left implementing only the original Store.
type BNK02Store interface {
	InitiateConnection(ctx context.Context, params domain.InitiateConnectionParams) (*domain.BankConnection, bool, error)
	CompleteConnectionAuthorization(ctx context.Context, params domain.CompleteConnectionAuthorizationParams) (*domain.BankConnection, error)
	ActivateConnection(ctx context.Context, params domain.ActivateConnectionParams) (*domain.BankConnection, error)
	RefreshConnection(ctx context.Context, params domain.RefreshConnectionParams) (*domain.BankConnection, error)
	TriggerReconsent(ctx context.Context, params domain.TriggerReconsentParams) (*domain.BankConnection, error)
	SuspendConnection(ctx context.Context, params domain.SuspendConnectionParams) (*domain.BankConnection, error)
	RevokeConnection(ctx context.Context, params domain.RevokeConnectionParams) (*domain.BankConnection, error)
	ReconnectProvider(ctx context.Context, params domain.ReconnectProviderParams) (*domain.BankConnection, error)
	RotateConnectionCredential(ctx context.Context, params domain.RotateConnectionCredentialParams) (*domain.BankConnection, error)
	ListConnectionEvents(ctx context.Context, tenantID, connectionID string) ([]domain.ConnectionEvent, error)
	CreateRegionPolicy(ctx context.Context, params domain.CreateRegionPolicyParams) (*domain.BankRegionPolicy, error)
	IsRegionAllowed(ctx context.Context, tenantID, legalEntityID, region string) (bool, error)
	HasRegionPolicy(ctx context.Context, tenantID, legalEntityID string) (bool, error)
}

// BNK0304Store is BNK-03 Statement Ingestion + BNK-04 Transaction
// Normalization's persistence surface — same composed-interface pattern
// as BNK02Store, colocated because normalization reads BNK-03's evidence
// synchronously.
type BNK0304Store interface {
	IngestStatement(ctx context.Context, tenantID string, req domain.IngestStatementLinesRequest, actorPrincipalID string) (*domain.IngestStatementResult, error)
	ValidateStatement(ctx context.Context, tenantID, statementID string) error
	AcceptStatement(ctx context.Context, tenantID, statementID string) error
	QuarantineStatement(ctx context.Context, tenantID, statementID, reason string) error
	ReprocessQuarantinedStatement(ctx context.Context, tenantID, statementID string) error
	NormalizeTransaction(ctx context.Context, params domain.NormalizeTransactionParams) (*domain.NormalizeTransactionResult, error)
	ReNormalizeTransaction(ctx context.Context, params domain.ReNormalizeTransactionParams) (*domain.CanonicalTransaction, error)
	QuarantineTransaction(ctx context.Context, params domain.QuarantineTransactionParams) (*domain.MappingException, error)
	ApproveMappingException(ctx context.Context, params domain.ApproveMappingExceptionParams) (*domain.CanonicalTransaction, error)
	GetCanonicalTransaction(ctx context.Context, tenantID, transactionID string) (*domain.CanonicalTransaction, error)
	CreateTransactionMapping(ctx context.Context, params domain.CreateTransactionMappingParams) (*domain.TransactionMapping, error)
}
