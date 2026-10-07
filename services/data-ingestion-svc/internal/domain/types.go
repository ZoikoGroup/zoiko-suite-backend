package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	PrefixIngestionRun     = "dir_"
	PrefixSourceCheckpoint = "dsc_"
	PrefixLandingObject    = "dlo_"
	PrefixQuarantineItem   = "dqi_"
	PrefixIdempotencyKey   = "dik_"
)

// errorString is a plain sentinel error, comparable with errors.Is across
// package boundaries without pulling in a dependency.
type errorString string

func (e errorString) Error() string { return string(e) }

// IdempotentReplayError is returned when a claim key was already used for
// an identical request — the caller gets back the resource that request
// already produced, not a duplicate.
type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

// IngestionRunStatus is the lifecycle state of an ingestion run.
type IngestionRunStatus string

const (
	IngestionRunDraft                IngestionRunStatus = "Draft"
	IngestionRunRunning              IngestionRunStatus = "Running"
	IngestionRunPartiallyQuarantined IngestionRunStatus = "PartiallyQuarantined"
	IngestionRunCompleted            IngestionRunStatus = "Completed"
	IngestionRunFailed               IngestionRunStatus = "Failed"
	IngestionRunSuperseded           IngestionRunStatus = "Superseded"
)

// IngestionRun is one ingestion attempt.
type IngestionRun struct {
	RunID           string             `json:"run_id"`
	TenantID        string             `json:"tenant_id"`
	SourceID        string             `json:"source_id"`
	Status          IngestionRunStatus `json:"status"`
	StartedAt       time.Time          `json:"started_at"`
	ClosedAt        *time.Time         `json:"closed_at,omitempty"`
	CheckpointRef   *string            `json:"checkpoint_ref,omitempty"`
	CreatedBy       string             `json:"created_by"`
	ResidencyRegion string             `json:"residency_region"`
	Classification  string             `json:"classification"`
	Purpose         string             `json:"purpose"`
}

// SourceCheckpoint is the durable watermark per source.
type SourceCheckpoint struct {
	CheckpointID    string    `json:"checkpoint_id"`
	SourceID        string    `json:"source_id"`
	TenantID        string    `json:"tenant_id"`
	LastPosition    string    `json:"last_position"`
	LastCommittedAt time.Time `json:"last_committed_at"`
	SchemaVersion   string    `json:"schema_version"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// LandingObject is one committed batch's landed records.
type LandingObject struct {
	LandingID       string    `json:"landing_id"`
	RunID           string    `json:"run_id"`
	SourceID        string    `json:"source_id"`
	TenantID        string    `json:"tenant_id"`
	RecordCount     int       `json:"record_count"`
	ContentHash     string    `json:"content_hash"`
	SchemaVersion   string    `json:"schema_version"`
	Classification  string    `json:"classification"`
	ResidencyRegion string    `json:"residency_region"`
	LandedAt        time.Time `json:"landed_at"`
	LandedBy        string    `json:"landed_by"`
}

// QuarantineItem is a batch or record that failed validation.
type QuarantineItem struct {
	QuarantineID    string    `json:"quarantine_id"`
	RunID           string    `json:"run_id"`
	SourceID        string    `json:"source_id"`
	TenantID        string    `json:"tenant_id"`
	Reason          string    `json:"reason"`
	PayloadRef      string    `json:"payload_ref"`
	SchemaVersion   string    `json:"schema_version"`
	Classification  string    `json:"classification"`
	ResidencyRegion string    `json:"residency_region"`
	QuarantinedAt   time.Time `json:"quarantined_at"`
	QuarantinedBy   string    `json:"quarantined_by"`
}

// BatchRecord represents a single record in a batch for commit/quarantine.
type BatchRecord struct {
	SourceEventID string                 `json:"source_event_id"`
	DedupKey      string                 `json:"dedup_key"`
	Payload       map[string]interface{} `json:"payload"`
}

// CommitBatchRequest is the request to commit a batch. NextPosition is the
// caller's own watermark for what comes after this batch (only the source
// poller knows its own position semantics — an offset, a cursor, a
// timestamp) — the checkpoint advances to it once the batch lands.
type CommitBatchRequest struct {
	RunID           string        `json:"run_id"`
	Records         []BatchRecord `json:"records"`
	SchemaVersion   string        `json:"schema_version"`
	Classification  string        `json:"classification"`
	ResidencyRegion string        `json:"residency_region"`
	NextPosition    string        `json:"next_position"`
}

// QuarantineBatchRequest is the request to quarantine a batch.
type QuarantineBatchRequest struct {
	RunID           string        `json:"run_id"`
	Records         []BatchRecord `json:"records"`
	SchemaVersion   string        `json:"schema_version"`
	Classification  string        `json:"classification"`
	ResidencyRegion string        `json:"residency_region"`
	Reason          string        `json:"reason"`
}

// ReplayFromCheckpointRequest rewinds/advances the checkpoint owned by
// RunID's source — checkpoints are keyed one-per-(tenant,source), so the
// run's source_id is enough to identify which one.
type ReplayFromCheckpointRequest struct {
	RunID    string `json:"run_id"`
	Position string `json:"position"`
}

// CloseRunRequest is the request to close a run.
type CloseRunRequest struct {
	RunID string `json:"run_id"`
}

// IdempotencyClaim is recorded in the same transaction as the change it guards.
type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

// SellerScope is the idempotency owner scope for ingestion commands.
const SellerScope = "seller"

// ComputeContentHash computes a SHA256 hash over every record's identity
// and payload — evidence that the exact committed content can be
// reproduced/verified later, not just that some records existed. Records
// are hashed in dedup-key order so the same batch always hashes the same
// way regardless of submission order; encoding/json.Marshal already sorts
// map keys, so payload encoding is deterministic without extra work.
func ComputeContentHash(records []BatchRecord) string {
	sorted := make([]BatchRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DedupKey < sorted[j].DedupKey })

	var b strings.Builder
	for _, r := range sorted {
		b.WriteString(r.SourceEventID)
		b.WriteByte('\x00')
		b.WriteString(r.DedupKey)
		b.WriteByte('\x00')
		payloadJSON, err := json.Marshal(r.Payload)
		if err != nil {
			payloadJSON = []byte("null")
		}
		b.Write(payloadJSON)
		b.WriteByte('\x1e')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ValidateIngestionRun validates an IngestionRun.
func ValidateIngestionRun(r *IngestionRun) error {
	if r.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if r.TenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	if r.SourceID == "" {
		return fmt.Errorf("source_id is required")
	}
	if r.Status == "" {
		return fmt.Errorf("status is required")
	}
	if r.CreatedBy == "" {
		return fmt.Errorf("created_by is required")
	}
	if r.ResidencyRegion == "" {
		return fmt.Errorf("residency_region is required")
	}
	if r.Classification == "" {
		return fmt.Errorf("classification is required")
	}
	if r.Purpose == "" {
		return fmt.Errorf("purpose is required")
	}
	return nil
}

// ValidateCommitBatchRequest validates a CommitBatchRequest.
func ValidateCommitBatchRequest(req *CommitBatchRequest) error {
	if req.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if len(req.Records) == 0 {
		return fmt.Errorf("at least one record is required")
	}
	if req.SchemaVersion == "" {
		return fmt.Errorf("schema_version is required")
	}
	if req.Classification == "" {
		return fmt.Errorf("classification is required")
	}
	if req.ResidencyRegion == "" {
		return fmt.Errorf("residency_region is required")
	}
	if req.NextPosition == "" {
		return fmt.Errorf("next_position is required")
	}
	for i, rec := range req.Records {
		if rec.SourceEventID == "" {
			return fmt.Errorf("record %d: source_event_id is required", i)
		}
		if rec.DedupKey == "" {
			return fmt.Errorf("record %d: dedup_key is required", i)
		}
	}
	return nil
}

// ValidateQuarantineBatchRequest validates a QuarantineBatchRequest.
func ValidateQuarantineBatchRequest(req *QuarantineBatchRequest) error {
	if req.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if len(req.Records) == 0 {
		return fmt.Errorf("at least one record is required")
	}
	if req.SchemaVersion == "" {
		return fmt.Errorf("schema_version is required")
	}
	if req.Classification == "" {
		return fmt.Errorf("classification is required")
	}
	if req.ResidencyRegion == "" {
		return fmt.Errorf("residency_region is required")
	}
	if strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	for i, rec := range req.Records {
		if rec.SourceEventID == "" {
			return fmt.Errorf("record %d: source_event_id is required", i)
		}
		if rec.DedupKey == "" {
			return fmt.Errorf("record %d: dedup_key is required", i)
		}
	}
	return nil
}

var (
	ErrIngestionRunNotFound     = errorString("ingestion run not found")
	ErrIngestionRunInvalidState = errorString("ingestion run is not in a state that allows this action")
	ErrSourceCheckpointNotFound = errorString("source checkpoint not found")
	ErrLandingObjectNotFound    = errorString("landing object not found")
	ErrQuarantineItemNotFound   = errorString("quarantine item not found")
	ErrDuplicateSourceEvent     = errorString("duplicate source event: dedup key already committed")
	ErrWrongResidencyRegion     = errorString("batch residency region does not match tenant's resolved residency policy")
	ErrSchemaIncompatible       = errorString("batch schema incompatible with declared schema version")
	ErrIdempotencyKeyReused     = errorString("idempotency key was already used for a different request")
	ErrRunAlreadyClosed         = errorString("ingestion run is already closed")
	ErrRunNotRunning            = errorString("ingestion run must be in Running or PartiallyQuarantined state")
)
