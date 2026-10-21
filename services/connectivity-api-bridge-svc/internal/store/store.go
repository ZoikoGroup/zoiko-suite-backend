package store

import (
	"context"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
)

// Store is the connector persistence boundary. Every method is scoped to the
// tenant in the request context; there is no cross-tenant read or write except
// ListProfiles/GetProfile, which are platform reference data.
type Store interface {
	// ── Pre-existing surface ──
	CreateBridge(ctx context.Context, b *domain.ApiBridge) error
	GetBridgeByID(ctx context.Context, id string) (*domain.ApiBridge, error)
	ListBridges(ctx context.Context, legalEntityID string) ([]domain.ApiBridge, error)
	RecordIngestion(ctx context.Context, log *domain.IngestionLog) error
	ListIngestionLogs(ctx context.Context, bridgeID string) ([]domain.IngestionLog, error)

	// ── Connector governance ──

	// CreateConnector inserts b and its effects atomically. When idemKey is
	// non-empty and was used before by this tenant for the SAME request, it
	// returns created=false with the original connector in b; for a DIFFERENT
	// request it returns domain.ErrIdempotencyConflict.
	CreateConnector(ctx context.Context, b *domain.ApiBridge, idemKey, idemHash string, fx *domain.Effects) (created bool, err error)

	// MutateConnector locks the connector, applies fn, and writes the connector
	// and every effect (evidence rows and outbox events) in one transaction. If
	// fn returns an error nothing is written.
	MutateConnector(ctx context.Context, id string, fn domain.MutateFunc) (*domain.ApiBridge, error)

	ListChanges(ctx context.Context, connectorID string) ([]domain.Change, error)
	ListApprovals(ctx context.Context, connectorID string) ([]domain.Approval, error)
	GetHealth(ctx context.Context, connectorID string) (*domain.Health, error)
	ListHealthTransitions(ctx context.Context, connectorID string, limit int) ([]domain.HealthTransition, error)

	// ── Sync checkpoints ──
	ListCheckpoints(ctx context.Context, connectorID string) ([]domain.Checkpoint, error)
	// AdvanceCheckpoint moves a stream forward. expectedVersion 0 creates the
	// stream; otherwise it must equal the stored checkpoint_version. A watermark
	// before the stored one is refused (ErrCheckpointRegress).
	AdvanceCheckpoint(ctx context.Context, connectorID, stream, cursor string, watermark *time.Time, expectedVersion int64, actor string, ev domain.OutboxEvent) (*domain.Checkpoint, error)
	CreateResetRequest(ctx context.Context, r *domain.ResetRequest, ev domain.OutboxEvent) error
	// DecideResetRequest applies or rejects a reset. The decider must differ
	// from the requester. Approving moves the checkpoint back and bumps its
	// version, in the same transaction.
	DecideResetRequest(ctx context.Context, requestID, actor string, approve bool, ev domain.OutboxEvent) (*domain.ResetRequest, error)

	// ── Provider profiles (platform reference data) ──
	PublishProfile(ctx context.Context, p *domain.Profile, ev domain.OutboxEvent) error
	GetProfile(ctx context.Context, providerKey string, version int) (*domain.Profile, error)
	ListProfiles(ctx context.Context, providerKey string) ([]domain.Profile, error)
	// SetProfileStatus moves a profile forward (CERTIFIED, DEPRECATED,
	// UNSUPPORTED). It never moves one back. A DEPRECATED profile records sunset (when given) as the end of its
	// deprecation window.
	SetProfileStatus(ctx context.Context, providerKey string, version int, status string, at time.Time, sunset *time.Time, ev domain.OutboxEvent) (*domain.Profile, error)
}

// PlatformScopeID is the platform-scope tenant id the rest of the estate uses
// for platform-wide reference data. Provider profiles may only be written from
// it (enforced by row-level security as well as by the handler).
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// Both implementations satisfy Store; the compiler checks it.
var (
	_ Store = (*PgStore)(nil)
	_ Store = (*MemoryStore)(nil)
)
