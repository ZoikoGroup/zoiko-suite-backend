package federation

import (
	"context"
	"errors"
	"time"
)

// ErrTokenReplayed means an ID token that already produced a session was presented again.
var ErrTokenReplayed = errors.New("the ID token has already been exchanged")

// Repo is the persistence the federation service and the login path need. The Postgres implementation
// (store.PgStore) writes every state change, its evidence row and its outbox event in one transaction.
type Repo interface {
	KeyRepo
	Health

	CreateConnection(ctx context.Context, c Connection, v Version, actor, correlationID string) error
	// AddVersion appends the next version to a connection. v.Version is assigned by the store.
	AddVersion(ctx context.Context, tenantID, connectionID string, v Version, actor, correlationID string) (*Version, error)
	GetConnection(ctx context.Context, tenantID, connectionID string) (*ConnectionDetail, error)
	ListConnections(ctx context.Context, tenantID string) ([]Connection, error)
	// DecideVersion approves or rejects a pending version. Approval requires the fingerprint the approver
	// saw and a decider other than the author, and supersedes the version it replaces.
	DecideVersion(ctx context.Context, tenantID, connectionID string, version int, approve bool,
		actor, fingerprint, reason, correlationID string) (*ConnectionDetail, error)
	// SetStatus performs SUSPENDED / ACTIVE (resume) / REVOKED transitions.
	SetStatus(ctx context.Context, tenantID, connectionID, to, actor, reason, correlationID string) (*Connection, error)

	// FindLoginConnection finds the live connection of a tenant for a claimed issuer, with its approved
	// version. Nil, nil when the tenant has none.
	FindLoginConnection(ctx context.Context, tenantID, issuer string) (*Connection, *Version, error)

	FindLink(ctx context.Context, tenantID, issuer, subject string) (*Link, error)
	GetLink(ctx context.Context, tenantID, linkID string) (*Link, error)
	ListLinks(ctx context.Context, tenantID, principalID, issuer string, limit int) ([]Link, error)
	CreateLink(ctx context.Context, l Link, actor, correlationID string) (*Link, error)

	// CompleteLogin records a successful federated login: it consumes the token (ErrTokenReplayed if it
	// was already used), writes the evidence row, advances the link and enqueues the event, atomically.
	CompleteLogin(ctx context.Context, rec LoginRecord, linkID, tokenHash string, tokenExpiry time.Time) error
	// RecordDenied writes the evidence row for a login whose signature verified but which was refused.
	RecordDenied(ctx context.Context, rec LoginRecord) error
	ListLogins(ctx context.Context, tenantID, connectionID string, limit int) ([]LoginRecord, error)
}
