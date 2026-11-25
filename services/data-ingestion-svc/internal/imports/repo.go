package imports

import (
	"context"
	"io"
	"time"

	"zoiko.io/data-ingestion-svc/internal/domain"
)

// PlatformScopeID is the tenant scope import profiles are published from, the same platform
// scope every service in the estate uses for platform configuration.
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// Event is one fact the batch emits through the transactional outbox, in the same
// transaction as the change it describes.
type Event struct {
	Type    string
	Payload map[string]any
}

// Event types.
const (
	EventValidated            = "import.validated"
	EventPreviewApproved      = "import.preview-approved"
	EventStarted              = "import.started"
	EventItemQuarantined      = "import.item-quarantined"
	EventItemReleaseRequested = "import.item-release-requested"
	EventItemReleased         = "import.item-released"
	EventItemReleaseRefused   = "import.item-release-refused"
	EventItemRevalidated      = "import.item-revalidated"
	EventCompleted            = "import.completed"
	EventReconciled           = "import.reconciled"
	EventReconciliationFail   = "import.reconciliation-failed"
	EventBatchQuarantined     = "import.quarantined"
	EventBatchFailed          = "import.failed"
)

// Patch names the batch columns a transition may set. A nil pointer leaves the column alone.
type Patch struct {
	ChunkCount        *int
	SizeBytes         *int64
	ContentSHA256     *string
	ChainHash         *string
	ValidateRequested *bool
	ScanEngine        *string
	ScanVerdict       *string
	ScanSignature     *string
	ScannedAt         *time.Time

	Rows            *int
	Valid           *int
	Invalid         *int
	ComputedControl *string
	MappingHash     *string

	Preview             *Preview
	PreviewFingerprint  *string
	PreviewedAt         *time.Time
	ApprovedFingerprint *string
	ApprovedBy          *string
	ApprovedAt          *time.Time
	ExecutedBy          *string
	ExecutedAt          *time.Time
	CompletedAt         *time.Time

	Imported       *int
	Quarantined    *int
	Failed         *int
	Skipped        *int
	Reconciliation *Reconciliation
	ReconciledAt   *time.Time
}

// Transition is one atomic batch change: the new status (empty keeps the current one), the
// statuses it may start from, the columns it sets, the log entry that explains it, and the
// events it emits, all committed together or not at all.
type Transition struct {
	TenantID        string
	BatchID         string
	From            []Status
	To              Status
	ExpectedVersion int // 0 means do not check
	Reason          string
	Patch           Patch
	Log             LogEntry
	Events          []Event
}

// ChunkInfo describes one stored chunk.
type ChunkInfo struct {
	Index  int
	Size   int
	SHA256 string
}

// ItemResult is how one item's delivery to the owning domain ended.
type ItemResult struct {
	Status           ItemStatus
	DomainStatus     *int
	ResultRef        string
	ErrorClass       string
	NextAttemptAt    time.Time
	QuarantineReason string
	QuarantineOwner  string
}

// Ref names a batch a worker should look at.
type Ref struct {
	TenantID string
	BatchID  string
}

// Repo is the persistence the import engine needs. The PostgreSQL implementation lives in
// internal/store; every method runs in a tenant-scoped transaction except the two discovery
// methods, which name themselves as the import worker.
type Repo interface {
	PutProfile(ctx context.Context, p *Profile) (*Profile, error)
	GetProfile(ctx context.Context, key string, version int) (*Profile, error) // version 0 means the latest certified
	CertifyProfile(ctx context.Context, key string, version int, by string, at time.Time) (*Profile, error)
	SetProfileStatus(ctx context.Context, key string, version int, to ProfileStatus) (*Profile, error)
	ListProfiles(ctx context.Context, key string) ([]Profile, error)

	CreateBatch(ctx context.Context, b *Batch, claim domain.IdempotencyClaim) (*Batch, error)
	GetBatch(ctx context.Context, tenantID, batchID string) (*Batch, error)
	ListBatches(ctx context.Context, tenantID string, status Status, limit int) ([]Batch, error)
	Transition(ctx context.Context, t Transition) (*Batch, error)
	ListLog(ctx context.Context, tenantID, batchID string, limit int) ([]LogEntry, error)

	PutChunk(ctx context.Context, tenantID, batchID string, index int, content []byte, sha256 string) (stored bool, err error)
	Chunks(ctx context.Context, tenantID, batchID string) ([]ChunkInfo, error)
	OpenContent(ctx context.Context, tenantID, batchID string) (io.ReadCloser, error)

	ResetItems(ctx context.Context, tenantID, batchID string) error
	InsertItems(ctx context.Context, tenantID, batchID string, items []Item) error
	GetItem(ctx context.Context, tenantID, batchID string, rowNo int) (*Item, error)
	ListItems(ctx context.Context, tenantID, batchID string, status ItemStatus, afterRow, limit int) ([]Item, error)
	ItemCounts(ctx context.Context, tenantID, batchID string) (map[ItemStatus]int, error)
	ErrorCounts(ctx context.Context, tenantID, batchID string) (map[string]int, error)
	SumColumn(ctx context.Context, tenantID, batchID, column string, status ItemStatus) (string, error)

	// ActivateItems moves a validated batch's items into execution: VALID becomes PENDING,
	// and INVALID becomes SKIPPED (only reachable under a skip-invalid policy).
	ActivateItems(ctx context.Context, tenantID, batchID string) (pending, skipped int, err error)
	ClaimItems(ctx context.Context, tenantID, batchID string, now time.Time, limit int, lease time.Duration) ([]Item, error)
	FinishItem(ctx context.Context, tenantID, batchID string, rowNo int, res ItemResult, events []Event) error
	// QuarantineItem holds a pending item back and records the episode in the quarantine ledger.
	QuarantineItem(ctx context.Context, tenantID, batchID string, rowNo int, reason, owner, by, correlationID string, events []Event) error
	// RetryItem puts a FAILED item back in the queue. A QUARANTINED item is refused with
	// ErrReleaseRequired: it leaves quarantine only through the governed release below.
	RetryItem(ctx context.Context, tenantID, batchID string, rowNo int, now time.Time, maxAttempts int) error

	// RequestRelease asks for a quarantined item to be released, naming the correction made in the
	// owning domain. The item stays quarantined until someone else decides.
	RequestRelease(ctx context.Context, tenantID, batchID string, rowNo int, by, reason, correctionRef, correlationID string, events []Event) error
	// DecideRelease answers the item's open release request. release=true re-queues the item so the
	// owning domain re-validates it; false leaves it quarantined. The decider must be independent of
	// the requester and of whoever quarantined the item.
	// answersRequest, when non-zero, is the ledger id of the release request the decision was made on: if the
	// item's open request is any other, the decision is refused with ErrStaleRelease (optimistic concurrency).
	DecideRelease(ctx context.Context, tenantID, batchID string, rowNo int, by, reason, correlationID string, release bool, answersRequest int64, now time.Time, maxRetries int, events []Event) error
	// QuarantineLedger is the append-only history of an item (rowNo > 0) or of the whole batch (rowNo 0).
	QuarantineLedger(ctx context.Context, tenantID, batchID string, rowNo int) ([]LedgerEntry, error)
	// QuarantineCounts are the numbers the quarantine state is derived from.
	QuarantineCounts(ctx context.Context, tenantID, batchID string) (quarantined, total, openRequests int, err error)
	Pending(ctx context.Context, tenantID, batchID string) (int, error)
	CountMissingRefs(ctx context.Context, tenantID, batchID string) (int, error)

	DueValidations(ctx context.Context, limit int) ([]Ref, error)
	DueExecutions(ctx context.Context, now time.Time, limit int) ([]Ref, error)
}
