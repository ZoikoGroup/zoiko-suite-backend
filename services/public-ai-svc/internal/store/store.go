// Package store persists sessions, turns, signed releases, the revocation
// ledger, flags, usage counters and the transactional outbox. Two
// implementations satisfy Store: Postgres (production) and Memory (unit
// tests and local runs without a database). Both pass the same conformance
// suite in storetest.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrTurnConflict        = errors.New("a turn is already in progress for this session")
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
)

// Session is an anonymous public chat session. The grant itself is never
// stored, only its SHA-256 hash.
type Session struct {
	SessionID   string
	GrantHash   []byte
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Locale      string
	MarketCode  string
	PageContext string
	DeletedAt   *time.Time
	// PendingContext is the JSON carry-over after a clarifying question:
	// intent and slots only, never visitor text.
	PendingContext []byte
}

// Turn is one request/response exchange. Response holds the validated public
// envelope only; the raw visitor message is never persisted.
type Turn struct {
	TurnID       string
	SessionID    string
	ClientTurnID string
	RequestHash  []byte
	Sequence     int
	Status       string
	IntentID     string
	Response     []byte
	Completed    bool
	CreatedAt    time.Time
	// FeedbackRating is "useful", "not_useful" or "" (Doc 01 FR-09).
	FeedbackRating string
	FeedbackReason string
}

// BeginResult tells the caller whether to run a new turn or replay one.
type BeginResult struct {
	Turn   Turn
	Replay bool // true: Turn is a completed earlier turn with the same key and payload
}

// Release is a signed, immutable artifact (kind "kb" or "claims").
type Release struct {
	ReleaseID string
	Kind      string
	Manifest  []byte
	Signature []byte
	CreatedAt time.Time
}

// Revocation is an append-only ledger entry. Watermark is assigned by the store.
type Revocation struct {
	Watermark   int64
	TargetKind  string // "source" or "claim"
	TargetID    string
	ReasonCode  string
	EffectiveAt time.Time
	CreatedAt   time.Time
}

// Conflict records contradicting authoritative claims (Doc 03 §16).
type Conflict struct {
	ConflictID      string
	SubjectKey      string
	ClaimIDs        []string
	DetectionType   string
	Severity        string
	ResolutionState string
	DetectedAt      time.Time
}

// Flag is one feature flag / kill-switch value.
type Flag struct {
	Name        string
	Value       []byte // JSON
	ActorRef    string
	ApprovalRef string
	UpdatedAt   time.Time
}

// OutboxEvent is a serialized event awaiting publication.
type OutboxEvent struct {
	OutboxID  int64
	EventID   string
	Topic     string
	EventType string
	Payload   []byte
	CreatedAt time.Time
	Attempts  int
}

// Store is the persistence contract.
type Store interface {
	Ping(ctx context.Context) error

	CreateSession(ctx context.Context, s Session, idempotencyKey string, requestHash []byte) error
	// SessionByIdempotencyKey returns the session created under key, with the
	// stored request hash, or ErrNotFound.
	SessionByIdempotencyKey(ctx context.Context, key string) (Session, []byte, error)
	// SessionByGrant also returns deleted sessions (DeletedAt set); callers
	// must refuse them for everything except a repeat DELETE.
	SessionByGrant(ctx context.Context, grantHash []byte) (Session, error)
	RotateGrant(ctx context.Context, sessionID string, newGrantHash []byte) error
	DeleteSession(ctx context.Context, sessionID string, at time.Time) error
	// SetPendingContext replaces (nil clears) the session's carry-over.
	SetPendingContext(ctx context.Context, sessionID string, data []byte) error

	// BeginTurn atomically either replays a completed turn with the same
	// (session, client_turn_id) and request hash, rejects a mismatched
	// payload (ErrIdempotencyMismatch), rejects a concurrent turn
	// (ErrTurnConflict), or claims the session's single active-turn slot until
	// lockUntil and allocates the next turn_sequence.
	BeginTurn(ctx context.Context, t Turn, lockUntil time.Time) (BeginResult, error)
	// CompleteTurn stores the response, releases the slot, adds usage and
	// writes outbox events in one transaction.
	CompleteTurn(ctx context.Context, t Turn, usageBuckets []string, tokens int64, events []OutboxEvent) error
	// AbortTurn releases the slot and forgets an unfinished turn so the
	// client can retry with the same key.
	AbortTurn(ctx context.Context, sessionID, turnID string) error
	// SetFeedback records a rating on a completed turn of sessionID
	// (ErrNotFound otherwise). Repeating it replaces the previous rating.
	SetFeedback(ctx context.Context, sessionID, turnID, rating, reason string, at time.Time) error
	// PurgeSessions deletes sessions (and their turns) that expired or were
	// deleted before cutoff, returning how many were removed (retention).
	PurgeSessions(ctx context.Context, cutoff time.Time) (int, error)
	// AppendEvents writes events to the outbox on their own.
	AppendEvents(ctx context.Context, events []OutboxEvent) error

	PutRelease(ctx context.Context, r Release) error
	GetRelease(ctx context.Context, releaseID string) (Release, error)
	SetPointer(ctx context.Context, kind, releaseID string, events []OutboxEvent) error
	GetPointer(ctx context.Context, kind string) (string, error)

	AddRevocation(ctx context.Context, r Revocation, events []OutboxEvent) (int64, error)
	Revocations(ctx context.Context, afterWatermark int64) ([]Revocation, error)

	RecordConflict(ctx context.Context, c Conflict) error

	SetFlag(ctx context.Context, f Flag, events []OutboxEvent) error
	Flags(ctx context.Context) ([]Flag, error)

	Usage(ctx context.Context, bucket string) (int64, error)

	UnpublishedEvents(ctx context.Context, limit int) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, outboxIDs []int64, at time.Time) error
	MarkAttempt(ctx context.Context, outboxIDs []int64) error
}
