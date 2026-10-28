// Package batch is XIC-05, Batch/File Exchange, Replay & Reconciliation Service of ZS-SVC-T-001 (§10).
//
// A file delivery is a BATCH: a manifest (what the sender says it sent) and one or more files. The package keeps the
// two kinds of truth strictly apart (§10.2): "file accepted" means the transport and the file envelope were valid and
// says nothing about any row; every row is admitted through the same pipeline as a real-time delivery and keeps its
// own outcome. Integrity is proven before a single row is read (I-21): the manifest and its checksums are verified,
// sizes and decompression are bounded, and content that is not plain text is never parsed. A batch that arrives twice
// is the same batch (the original reference is returned, nothing is reprocessed); the same batch identity with
// different content is a conflict to investigate. Replay and backfill are scoped, pinned to the versions the data
// arrived under, justified, approved where the risk is high, dry-runnable, and idempotent against effects already
// applied (I-23, I-24).
package batch

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
)

// Batch states (§4).
const (
	StateReceived    = "RECEIVED"
	StateValidating  = "VALIDATING"
	StateAccepted    = "ACCEPTED"
	StatePartial     = "PARTIAL"
	StateRejected    = "REJECTED"
	StateProcessing  = "PROCESSING"
	StateReconciled  = "RECONCILED"
	StateQuarantined = "QUARANTINED"
)

// File states.
const (
	FileReceived = "RECEIVED"
	FileVerified = "VERIFIED"
	FileRejected = "REJECTED"
)

// Row outcomes. A row's outcome is its own: it is never inferred from the file's.
const (
	RowPending        = "PENDING" // admitted; the domain has not answered
	RowAccepted       = "ACCEPTED"
	RowDomainRejected = "DOMAIN_REJECTED"
	RowQuarantined    = "QUARANTINED"
	RowRejected       = "REJECTED"
	RowDuplicate      = "DUPLICATE"
)

// Channels a file can arrive through.
const (
	ChannelHTTPS = "HTTPS"
	ChannelSFTP  = "SFTP"
)

// Reasons recorded when a batch is stopped or flagged. Stable machine values that never carry a payload value.
const (
	ReasonIntegrity        = "BATCH_INTEGRITY_FAILED"
	ReasonManifestInvalid  = "MANIFEST_INVALID"
	ReasonSchemaVersion    = "SCHEMA_VERSION_UNSUPPORTED"
	ReasonFileMissing      = "FILE_MISSING"
	ReasonFileUnexpected   = "FILE_NOT_IN_MANIFEST"
	ReasonFileName         = "FILE_NAME_NOT_ALLOWED"
	ReasonFileTooLarge     = "FILE_TOO_LARGE"
	ReasonTooManyFiles     = "TOO_MANY_FILES"
	ReasonTooManyRows      = "TOO_MANY_ROWS"
	ReasonCompression      = "COMPRESSION_NOT_ALLOWED"
	ReasonDecompression    = "DECOMPRESSION_LIMIT"
	ReasonContentUnsafe    = "CONTENT_NOT_PLAIN_TEXT"
	ReasonEncoding         = "ENCODING_INVALID"
	ReasonHeaderMismatch   = "HEADER_MISMATCH"
	ReasonRecordCount      = "RECORD_COUNT_REQUIRED"
	ReasonConflict         = "BATCH_ID_CONFLICT"
	ReasonScanUnavailable  = "SCAN_UNAVAILABLE"
	ReasonScanNotClean     = "SCAN_NOT_CLEAN"
	ReasonConnectionState  = "CONNECTION_NOT_ACTIVE"
	ReasonRowStructure     = "ROW_STRUCTURE"
	ReasonRowNotJSON       = "ROW_NOT_JSON_OBJECT"
	ReasonManifestVariance = "MANIFEST_VARIANCE"
)

// Errors.
var (
	ErrNotFound        = errors.New("batch not found")
	ErrDuplicate       = errors.New("this batch identity is already held")
	ErrVersionConflict = errors.New("version conflict: the object changed since it was read")
	ErrTransition      = errors.New("transition is not allowed from the current state")
	ErrBadRequest      = errors.New("bad request")
	ErrSelfAction      = errors.New("that step must be taken by someone other than the person who requested it")
	ErrDependency      = errors.New("a dependency needed for this is unavailable")
	ErrReplayNotFound  = errors.New("replay job not found")
	ErrReplayScope     = errors.New("a replay names its scope explicitly: batch rows, exchange ids, or a bounded window of one connection")
	ErrReplayState     = errors.New("the replay job is not in a state that allows this")
	ErrApprovalNeeded  = errors.New("this replay exceeds the risk threshold and needs approval")
)

// Batch is the durable record of one file delivery. File bodies are internal: the API never returns them.
type Batch struct {
	BatchID           string          `json:"batch_id"`
	TenantID          string          `json:"tenant_id"`
	ConnectionID      string          `json:"connection_id"`
	ConnectorID       string          `json:"connector_id"`
	OperationID       string          `json:"operation_id"`
	ContractVersionID string          `json:"contract_version_id"`
	MappingVersionID  string          `json:"mapping_version_id,omitempty"`
	ExternalBatchID   string          `json:"external_batch_id"`
	SourceSystem      string          `json:"source_system"`
	SourceSchema      string          `json:"source_schema"`
	Channel           string          `json:"channel"`
	State             string          `json:"state"`
	ReasonCode        string          `json:"reason_code,omitempty"`
	XICCode           string          `json:"xic_code,omitempty"`
	ManifestHash      string          `json:"manifest_hash"`
	ContentHash       string          `json:"content_hash"`
	DedupeKey         string          `json:"-"`
	OriginalBatchID   string          `json:"original_batch_id,omitempty"`
	Manifest          json.RawMessage `json:"manifest,omitempty"`
	FileCount         int             `json:"file_count"`
	DeclaredRows      *int            `json:"declared_rows,omitempty"`
	ParsedRows        int             `json:"parsed_rows"`
	AdmittedRows      int             `json:"admitted_rows"`
	AcceptedRows      int             `json:"accepted_rows"`
	PendingRows       int             `json:"pending_rows"`
	RejectedRows      int             `json:"rejected_rows"`
	DuplicateRows     int             `json:"duplicate_rows"`
	QuarantinedRows   int             `json:"quarantined_rows"`
	DomainRejected    int             `json:"domain_rejected_rows"`
	VarianceRows      int             `json:"variance_rows"`
	ProcessingDone    bool            `json:"processing_done"`
	AuthMechanism     string          `json:"auth_mechanism,omitempty"`
	SourceNetHash     string          `json:"source_net_hash,omitempty"`
	CorrelationID     string          `json:"correlation_id,omitempty"`
	LeaseUntil        *time.Time      `json:"-"`
	NextProcessAt     *time.Time      `json:"next_process_at,omitempty"`
	ProcessAttempts   int             `json:"process_attempts"`
	Version           int64           `json:"version"`
	ReceivedAt        time.Time       `json:"received_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// File is one file of a batch. Its content is kept only for a bounded time, so a batch can be processed and replayed.
type File struct {
	FileID           string     `json:"file_id"`
	BatchID          string     `json:"batch_id"`
	TenantID         string     `json:"tenant_id"`
	Name             string     `json:"name"`
	DeclaredSHA256   string     `json:"declared_sha256"`
	DeclaredSize     int64      `json:"declared_size"`
	Compression      string     `json:"compression,omitempty"`
	ActualSHA256     string     `json:"actual_sha256,omitempty"`
	ActualSize       int64      `json:"actual_size"`
	DeclaredRecords  *int       `json:"declared_records,omitempty"`
	ParsedRows       int        `json:"parsed_rows"`
	State            string     `json:"state"`
	ReasonCode       string     `json:"reason_code,omitempty"`
	Content          []byte     `json:"-"`
	ContentRetained  bool       `json:"content_retained"`
	ContentExpiresAt *time.Time `json:"content_expires_at,omitempty"`
	Ordinal          int        `json:"ordinal"`
}

// Row is one row of a file with its own outcome. It holds a hash of the row and the exchange it became, never the row.
type Row struct {
	RowID         string    `json:"row_id"`
	BatchID       string    `json:"batch_id"`
	FileID        string    `json:"file_id"`
	TenantID      string    `json:"tenant_id"`
	RowNo         int       `json:"row_no"`
	SourceRowHash string    `json:"source_row_hash"`
	ExchangeID    string    `json:"exchange_id,omitempty"`
	State         string    `json:"state"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Event is the append-only evidence of a batch change. It never carries row or file content.
type Event struct {
	Kind   string          `json:"kind"`
	From   string          `json:"from,omitempty"`
	To     string          `json:"to,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
	Actor  string          `json:"actor"`
	At     time.Time       `json:"at"`
}

// OutEvent is a domain event for the transactional outbox.
type OutEvent struct {
	Type          string
	AggregateType string
	AggregateID   string
	ActorID       string
	CorrelationID string
	Data          map[string]any
}

// Actor is who is acting.
type Actor struct {
	PrincipalID   string
	CorrelationID string
}

// Filter narrows a list of batches.
type Filter struct {
	State        string
	ConnectionID string
	Limit        int
}

// RowFilter narrows a list of rows.
type RowFilter struct {
	States []string // any of these outcomes
	RowNos []int    // any of these row numbers
	State  string
	FileID string
	After  int
	Limit  int
}

// Tx is what Mutate hands to its function.
type Tx struct {
	B     *Batch
	Event *Event
	Out   []OutEvent
}

// Repo is the persistence boundary of XIC-05 batches.
type Repo interface {
	// Insert stores a new batch with its files, event and outbox events, atomically. When the batch claims a dedupe
	// key already held it returns ErrDuplicate with the holder in existing, and writes nothing.
	Insert(ctx context.Context, b *Batch, files []File, ev Event, out []OutEvent) (existing *Batch, err error)
	// InsertRejected stores a batch that was refused (no dedupe key, no content) as evidence.
	InsertRejected(ctx context.Context, b *Batch, files []File, ev Event, out []OutEvent) error
	Get(ctx context.Context, id string) (*Batch, error)
	GetByDedupe(ctx context.Context, tenant, key string) (*Batch, error)
	List(ctx context.Context, f Filter) ([]Batch, error)
	Events(ctx context.Context, id string) ([]Event, error)
	AddEvent(ctx context.Context, id string, ev Event) error
	Mutate(ctx context.Context, id string, fn func(tx *Tx) error) (*Batch, error)

	Files(ctx context.Context, batchID string, withContent bool) ([]File, error)
	UpdateFile(ctx context.Context, f File) error
	// AddRows stores rows not yet stored: a row already present (same file and row number) is left as it is, so a
	// processing run that was interrupted resumes where it stopped.
	AddRows(ctx context.Context, rows []Row) error
	Rows(ctx context.Context, batchID string, f RowFilter) ([]Row, error)
	RowCount(ctx context.Context, batchID, fileID string) (int, error)
	// RowStateCounts counts a batch's rows by outcome.
	RowStateCounts(ctx context.Context, batchID string) (map[string]int, error)
	UpdateRows(ctx context.Context, rows []Row) error

	// DueForProcessing returns batches to process (RECEIVED, or VALIDATING/PROCESSING with an expired lease) and
	// batches to refresh, across tenants (read only).
	DueForProcessing(ctx context.Context, now time.Time, limit int) ([]DueRef, error)
	// PurgeContent removes file bodies whose retention has passed.
	PurgeContent(ctx context.Context, now time.Time, limit int) (int, error)
	RecordSignal(ctx context.Context, tenant string, ev OutEvent) error

	// Replay jobs.
	InsertReplay(ctx context.Context, j *ReplayJob, items []ReplayItem, ev OutEvent) error
	GetReplay(ctx context.Context, id string) (*ReplayJob, error)
	ListReplays(ctx context.Context, f ReplayFilter) ([]ReplayJob, error)
	ReplayItems(ctx context.Context, id string, limit int) ([]ReplayItem, error)
	MutateReplay(ctx context.Context, id string, fn func(tx *ReplayTx) error) (*ReplayJob, error)
	DueReplays(ctx context.Context, limit int) ([]DueRef, error)
	UpdateReplayItems(ctx context.Context, items []ReplayItem) error
}

// DueRef names a batch (or replay job) a worker should drive.
type DueRef struct{ TenantID, ID string }

func withTenant(ctx context.Context, tenant string) context.Context {
	return middleware.WithTenant(ctx, tenant)
}

// EventTypes lists every event XIC-05 can emit (ZS-SVC-T-001 section 14.1 integration.batch.* and
// integration.replay.* plus the receipt, conflict and security events around them). A test proves each is produced
// by a real flow.
var EventTypes = []string{
	"integration.batch.received", "integration.batch.accepted", "integration.batch.rejected", "integration.batch.duplicate", "integration.batch.conflict",
	"integration.batch.partial", "integration.batch.reconciled",
	"integration.replay.requested", "integration.replay.approved", "integration.replay.rejected", "integration.replay.started",
	"integration.replay.completed", "integration.replay.cancelled",
	"integration.security.batch-decompression-limit", "integration.security.batch-content-unsafe",
}

func middlewareTenant(ctx context.Context) string { return middleware.GetTenantID(ctx) }
