// Package store persists sessions, turns, signed releases, the revocation
// ledger, flags, usage counters and the transactional outbox. Two
// implementations satisfy Store: Postgres (production) and Memory (unit
// tests and local runs without a database). Both pass the same conformance
// suite in storetest.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrAuditIncomplete     = errors.New("a control action needs an actor and an approval reference")
	ErrTurnConflict        = errors.New("a turn is already in progress for this session")
	ErrIdempotencyMismatch = errors.New("idempotency key reused with a different request")
	// ErrSessionGone: the session expired or was deleted while its turn ran;
	// the turn is not completed (SEC-T057).
	ErrSessionGone = errors.New("session expired or deleted")
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
	// LastSeenAt is the last completed turn (CreatedAt until one completes):
	// idle expiry runs from it (Doc 06 §17). TokensUsed is the session's
	// running total for the per-session cap (Doc 06 §13). Origin is the
	// site origin that created the session; its grant works only there
	// (Doc 05 §08). Counts and an origin only, never content.
	LastSeenAt time.Time
	TokensUsed int64
	Origin     string
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
	// Doc 03 §16 minimum record: whether the claim is blocked until the case
	// is resolved, and the accountable owner (migration 005).
	BlockedClaim  bool
	AssignedOwner string
}

// Flag is one feature flag / kill-switch value.
type Flag struct {
	Name        string
	Value       []byte // JSON
	ActorRef    string
	ApprovalRef string
	UpdatedAt   time.Time
	// Environment is recorded with the change in the control audit.
	Environment string
}

// AuditEntry is one operator control action (Doc 06 §21, Doc 09 §05/§22):
// flag and kill-switch changes, promotion and rollback, revocation and
// dead-letter replay, with actor, approval, reason and before/after hashes.
// The ledger is append-only (migration 004 rejects UPDATE and DELETE).
type AuditEntry struct {
	AuditID     int64
	Action      string
	Target      string
	Actor       string
	Approval    string
	Reason      string
	BeforeHash  string
	AfterHash   string
	Environment string
	At          time.Time
	// RequestID and ActorRole complete the Doc 09 §21 audit envelope.
	RequestID string
	ActorRole string
	// PrevHash and RowHash chain the rows (AuditRowHash); set by the store.
	PrevHash, RowHash string
}

// Hash is the before/after fingerprint recorded in the control audit: the
// value itself is never stored there.
func Hash(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
	// SetSessionMarket keeps the market the visitor named for the rest of
	// the session (Doc 02 §10). It is never inferred from the IP address.
	SetSessionMarket(ctx context.Context, sessionID, market string) error

	// BeginTurn atomically either replays a completed turn with the same
	// (session, client_turn_id) and request hash, rejects a mismatched
	// payload (ErrIdempotencyMismatch), rejects a concurrent turn
	// (ErrTurnConflict), or claims the session's single active-turn slot until
	// lockUntil and allocates the next turn_sequence.
	BeginTurn(ctx context.Context, t Turn, lockUntil time.Time) (BeginResult, error)
	// CompleteTurn stores the response, releases the slot, adds usage and
	// writes outbox events in one transaction. It refuses (ErrSessionGone)
	// when the session has expired or was deleted (SEC-T057).
	CompleteTurn(ctx context.Context, t Turn, usageBuckets []string, tokens int64, events []OutboxEvent) error
	// AbortTurn releases the slot and forgets an unfinished turn so the
	// client can retry with the same key.
	AbortTurn(ctx context.Context, sessionID, turnID string) error
	// SetFeedback records a rating on a completed turn of sessionID
	// (ErrNotFound otherwise). Repeating it replaces the previous rating.
	SetFeedback(ctx context.Context, sessionID, turnID, rating, reason string, at time.Time) error
	// PurgeSessions deletes sessions (and their turns) that expired or were
	// deleted before cutoff, returning how many were removed (retention).
	// It also drops outbox rows delivered (or dead-lettered) before cutoff
	// and usage counters older than the previous month (Doc 06 §17).
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
	// Conflicts lists recorded conflict cases, oldest first.
	Conflicts(ctx context.Context) ([]Conflict, error)
	// AppendAudit records a control action; it fails if actor or approval
	// is missing. ListAudit returns the newest entries first.
	AppendAudit(ctx context.Context, e AuditEntry) error
	ListAudit(ctx context.Context, limit int) ([]AuditEntry, error)
	Flags(ctx context.Context) ([]Flag, error)

	Usage(ctx context.Context, bucket string) (int64, error)

	UnpublishedEvents(ctx context.Context, limit int) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, outboxIDs []int64, at time.Time) error
	MarkAttempt(ctx context.Context, outboxIDs []int64) error
}

// UsageKeepFrom returns the oldest day and month usage buckets kept at now:
// the current and the previous calendar month (Doc 06 §17). Bucket names
// are "tokens:day:YYYY-MM-DD" and "tokens:month:YYYY-MM" (limits.UsageBuckets);
// they sort lexically by date.
func UsageKeepFrom(now time.Time) (day, month string) {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	return "tokens:day:" + first.Format("2006-01-02"), "tokens:month:" + first.Format("2006-01")
}

// RevocationEpochField is the kb.source.revoked.v1 data field the store
// fills with the ledger watermark it assigns, in the same transaction
// (Doc 05 §22).
const RevocationEpochField = "revocation_epoch"

// stampRevocationEpoch sets data.revocation_epoch to wm on every event that
// carries the field; other events are returned unchanged.
func stampRevocationEpoch(events []OutboxEvent, wm int64) []OutboxEvent {
	out := make([]OutboxEvent, len(events))
	for i, e := range events {
		out[i] = e
		var env map[string]json.RawMessage
		if json.Unmarshal(e.Payload, &env) != nil {
			continue
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(env["data"], &data) != nil {
			continue
		}
		if _, ok := data[RevocationEpochField]; !ok {
			continue
		}
		data[RevocationEpochField], _ = json.Marshal(wm)
		env["data"], _ = json.Marshal(data)
		if b, err := json.Marshal(env); err == nil {
			out[i].Payload = b
		}
	}
	return out
}
