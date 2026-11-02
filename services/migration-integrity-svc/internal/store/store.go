package store

import (
	"context"
	"errors"
	"time"

	"zoiko.io/migration-integrity-svc/internal/domain"
)

// ─── Interface ────────────────────────────────────────────────────────────────

// Store persists migration jobs, their integrity checks and quarantine items.
//
// Every write method runs in one transaction that (a) installs the tenant for
// row-level security, (b) claims the request's Idempotency-Key, (c) re-checks
// the state machine under a row lock, (d) writes the change, (e) enqueues the
// evidence event into the transactional outbox and (f) records the response
// for replay. An event therefore exists if and only if the change it reports
// committed, and a replayed request can never apply twice.
type Store interface {
	// CreateJob stores a decided validation run with its checks and entries.
	CreateJob(ctx context.Context, tenantID string, job *domain.MigrationJob, checks []domain.IntegrityCheck, entries []domain.AuditEntry, actorID, correlationID string, claim IdempotencyClaim) error

	// GetJobByID returns the job with its checks and entries, or ErrJobNotFound
	// (also for another tenant's job: RLS hides it).
	GetJobByID(ctx context.Context, tenantID, id string) (*domain.MigrationJob, error)

	// ListJobs returns one page of job summaries (no checks, no entries) and
	// whether another page follows.
	ListJobs(ctx context.Context, tenantID string, filter ListFilter) ([]domain.MigrationJob, bool, error)

	// ArchiveJob moves a job to ARCHIVED (soft delete; nothing is removed).
	ArchiveJob(ctx context.Context, tenantID string, cmd ArchiveCommand, claim IdempotencyClaim) (*domain.MigrationJob, error)

	// DispositionEntry moves a quarantine item out of QUARANTINED and returns
	// it together with the job's new version.
	DispositionEntry(ctx context.Context, tenantID string, cmd DispositionCommand, claim IdempotencyClaim) (*domain.AuditEntry, int, error)

	// FindIdempotentResponse returns the stored response for a key, or nil
	// when the key has not completed a request.
	FindIdempotentResponse(ctx context.Context, tenantID, principalID, key string) (*StoredResponse, error)
}

// ─── Errors ───────────────────────────────────────────────────────────────────
//
// The state-machine errors are the domain sentinels (domain.ErrJobArchived,
// domain.ErrJobAlreadyArchived, domain.ErrEntryAlreadyDecided); the store
// adds only what is about rows and concurrency.

var (
	// ErrJobNotFound: no such job in this tenant. 404.
	ErrJobNotFound = errors.New("migration job not found")
	// ErrEntryNotFound: no such audit entry on this job. 404.
	ErrEntryNotFound = errors.New("audit entry not found on this migration job")
	// ErrVersionMismatch: If-Match named a version the job no longer has. 412.
	ErrVersionMismatch = errors.New("the migration job has changed since the version named in If-Match")
	// ErrIdempotencyKeyInUse: another request with the same Idempotency-Key
	// committed first; the caller should replay its stored response.
	ErrIdempotencyKeyInUse = errors.New("idempotency key already used by a committed request")
)

// ─── Commands and filters ─────────────────────────────────────────────────────

// ListFilter selects one page of jobs. LegalEntityID is required by the
// handler; Status is optional; Limit and Offset are already bounded.
type ListFilter struct {
	LegalEntityID string
	Status        string
	Limit         int
	Offset        int
}

// ArchiveCommand archives one job. ExpectedVersion 0 means "no If-Match".
type ArchiveCommand struct {
	JobID           string
	ExpectedVersion int
	ActorID         string
	CorrelationID   string
}

// DispositionCommand records one quarantine decision.
type DispositionCommand struct {
	JobID           string
	EntryID         string
	TargetStatus    domain.EntryStatus
	Reason          string
	EvidenceRef     string
	WaiverExpiresAt *time.Time
	ExpectedVersion int
	ActorID         string
	CorrelationID   string
}

// ─── Idempotency ──────────────────────────────────────────────────────────────

// IdempotencyClaim identifies one write request for replay protection. A
// zero claim (empty Key) records nothing; tests and internal callers use it.
type IdempotencyClaim struct {
	Key            string
	PrincipalID    string
	Operation      string
	RequestHash    string // hex SHA-256 of method, path and body
	ResponseStatus int    // HTTP status recorded with the response
}

// IsZero reports whether the claim carries no key.
func (c IdempotencyClaim) IsZero() bool { return c.Key == "" }

// StoredResponse is a completed request's response, replayed verbatim.
type StoredResponse struct {
	Operation   string
	RequestHash string
	Status      int
	Body        []byte
}
