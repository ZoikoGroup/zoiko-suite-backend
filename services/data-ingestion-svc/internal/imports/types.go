// Package imports is INT-06, controlled bulk import (ZS-SVC-M-001 §4.6, §5): a file is
// uploaded in chunks, hash-checked, malware-scanned, parsed, validated against a
// certified profile, previewed, approved, executed as owning-domain commands, and
// reconciled.
//
// It owns ImportBatch, the source manifest and content hash, the mapping version, the
// validation and preview results, item statuses, quarantine and the reconciliation
// result. It does not own the imported business facts once the owning domain has accepted
// them, and it never writes another domain's data: every item is an ordinary command to the
// owning service, carrying a deterministic idempotency key.
package imports

import (
	"errors"
	"time"
)

// Status is a batch's lifecycle state.
type Status string

const (
	StatusCreated                 Status = "CREATED"
	StatusUploading               Status = "UPLOADING"
	StatusUploaded                Status = "UPLOADED"
	StatusScanned                 Status = "SCANNED"
	StatusParsed                  Status = "PARSED"
	StatusValidated               Status = "VALIDATED"
	StatusPreviewed               Status = "PREVIEWED"
	StatusApproved                Status = "APPROVED"
	StatusImporting               Status = "IMPORTING"
	StatusCompleted               Status = "COMPLETED"
	StatusCompletedWithExceptions Status = "COMPLETED_WITH_EXCEPTIONS" // explicit: some items are not imported
	StatusReconciled              Status = "RECONCILED"
	StatusQuarantined             Status = "QUARANTINED"
	StatusFailed                  Status = "FAILED"
	StatusCancelled               Status = "CANCELLED"
)

// Terminal reports whether nothing further can happen to a batch in the status.
func (s Status) Terminal() bool {
	switch s {
	case StatusReconciled, StatusQuarantined, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// PreExecution reports whether the batch can still be cancelled.
func (s Status) PreExecution() bool {
	switch s {
	case StatusCreated, StatusUploading, StatusUploaded, StatusScanned, StatusParsed, StatusValidated, StatusPreviewed, StatusApproved:
		return true
	}
	return false
}

// Reasons recorded when a batch is stopped.
const (
	ReasonMalware             = "MALWARE"
	ReasonScanFailed          = "SCAN_NOT_CLEAN"
	ReasonHashMismatch        = "HASH_MISMATCH"
	ReasonSchemaMismatch      = "SCHEMA_MISMATCH"
	ReasonValidationErrors    = "VALIDATION_ERRORS"
	ReasonControlMissing      = "CONTROL_TOTALS_MISSING"
	ReasonControlMismatch     = "CONTROL_TOTAL_MISMATCH"
	ReasonRowCountMismatch    = "ROW_COUNT_MISMATCH"
	ReasonTooLarge            = "TOO_LARGE"
	ReasonTooManyRows         = "TOO_MANY_ROWS"
	ReasonPreviewDiffers      = "PREVIEW_DIFFERS_FROM_EXECUTION"
	ReasonProfileUnavailable  = "PROFILE_UNAVAILABLE"
	ReasonCancelled           = "CANCELLED"
	ReasonUnsupportedEncoding = "UNSUPPORTED_ENCODING"
)

// Batch is one import.
type Batch struct {
	BatchID             string
	TenantID            string
	LegalEntityID       string
	ProfileKey          string
	ProfileVersion      int
	Material            bool
	SourceSystem        string
	JurisdictionContext string
	FileName            string
	DeclaredContentType string
	DeclaredSHA256      string
	DeclaredRows        *int
	DeclaredControl     string
	Status              Status
	StatusReason        string
	Version             int

	ChunkCount    int
	SizeBytes     int64
	ContentSHA256 string
	ChainHash     string

	ValidateRequestedAt *time.Time
	ScanEngine          string
	ScanVerdict         string
	ScanSignature       string
	ScannedAt           *time.Time

	RowCount        int
	ValidRows       int
	InvalidRows     int
	ComputedControl string
	MappingHash     string

	Preview             *Preview
	PreviewFingerprint  string
	PreviewedAt         *time.Time
	ApprovedFingerprint string
	ApprovedBy          string
	ApprovedAt          *time.Time
	ExecutedBy          string
	ExecutedAt          *time.Time
	CompletedAt         *time.Time

	ImportedItems    int
	QuarantinedItems int
	FailedItems      int
	SkippedItems     int
	Reconciliation   *Reconciliation
	ReconciledAt     *time.Time

	CreatedBy     string
	CorrelationID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ItemStatus is an item's state.
type ItemStatus string

const (
	ItemValid       ItemStatus = "VALID"
	ItemInvalid     ItemStatus = "INVALID"
	ItemSkipped     ItemStatus = "SKIPPED"
	ItemPending     ItemStatus = "PENDING"
	ItemImported    ItemStatus = "IMPORTED"
	ItemQuarantined ItemStatus = "QUARANTINED"
	ItemFailed      ItemStatus = "FAILED"
)

// Item is one row of the file.
type Item struct {
	BatchID          string
	RowNo            int
	TenantID         string
	RowHash          string
	Values           map[string]any
	Status           ItemStatus
	ErrorCodes       []RowError
	IdempotencyKey   string
	Attempts         int
	NextAttemptAt    time.Time
	LockedUntil      *time.Time
	DomainStatus     *int
	ResultRef        string
	LastErrorClass   string
	QuarantineReason string
	QuarantineOwner  string
	QuarantinedBy    string
	UpdatedAt        time.Time
}

// RowError is one reason a row is invalid. It names the column and a stable code, and
// never the value: the value may be personal or financial data.
type RowError struct {
	Column string `json:"column,omitempty"`
	Code   string `json:"code"`
}

// Row error codes.
const (
	CodeRequired       = "REQUIRED"
	CodeType           = "TYPE"
	CodeMaxLen         = "MAX_LEN"
	CodePattern        = "PATTERN"
	CodeEnum           = "ENUM"
	CodeDuplicateKey   = "DUPLICATE_KEY_IN_FILE"
	CodeCrossEntity    = "CROSS_ENTITY"
	CodeUnknownField   = "UNKNOWN_FIELD"
	CodeFieldCount     = "FIELD_COUNT"
	CodeMalformedLine  = "MALFORMED_LINE"
	CodeNegativeAmount = "NEGATIVE_NOT_ALLOWED"
)

// Preview is what the approver sees: the totals and a sample, never the whole file.
type Preview struct {
	RowCount        int            `json:"row_count"`
	ValidRows       int            `json:"valid_rows"`
	InvalidRows     int            `json:"invalid_rows"`
	ControlColumn   string         `json:"control_column,omitempty"`
	ControlTotal    string         `json:"control_total,omitempty"`
	DeclaredRows    *int           `json:"declared_rows,omitempty"`
	DeclaredControl string         `json:"declared_control_total,omitempty"`
	ErrorCounts     map[string]int `json:"error_counts"`
	SampleValid     []SampleRow    `json:"sample_valid"`
	SampleInvalid   []SampleRow    `json:"sample_invalid"`
	ProfileKey      string         `json:"profile_key"`
	ProfileVersion  int            `json:"profile_version"`
	MappingHash     string         `json:"mapping_hash"`
	ContentSHA256   string         `json:"content_sha256"`
	PartialPolicy   string         `json:"partial_policy"`
	Material        bool           `json:"material"`
	WillBeSkipped   int            `json:"will_be_skipped"`
	WillBeSent      int            `json:"will_be_sent"`
}

// SampleRow is a row shown in the preview.
type SampleRow struct {
	RowNo  int            `json:"row_no"`
	Values map[string]any `json:"values,omitempty"`
	Errors []RowError     `json:"errors,omitempty"`
}

// Reconciliation is the outcome of comparing what was sent with what the owning domain
// accepted.
type Reconciliation struct {
	Matched          bool     `json:"matched"`
	TotalItems       int      `json:"total_items"`
	Imported         int      `json:"imported"`
	Quarantined      int      `json:"quarantined"`
	Failed           int      `json:"failed"`
	Skipped          int      `json:"skipped"`
	ExpectedControl  string   `json:"expected_control_total,omitempty"`
	ImportedControl  string   `json:"imported_control_total,omitempty"`
	MissingResultRef int      `json:"items_without_result_ref"`
	Problems         []string `json:"problems"`
}

// LogEntry is one fact about a batch, append-only.
type LogEntry struct {
	LogID         int64
	TenantID      string
	BatchID       string
	Kind          string
	Actor         string
	FromStatus    Status
	ToStatus      Status
	Reason        string
	Detail        map[string]any
	CorrelationID string
	CreatedAt     time.Time
}

// Actor identifies who is acting.
type Actor struct {
	TenantID      string
	PrincipalID   string
	CorrelationID string
}

// Errors. Messages name the rule and never echo caller input.
var (
	ErrNotFound            = errors.New("import batch not found")
	ErrProfileNotFound     = errors.New("import profile not found")
	ErrItemNotFound        = errors.New("import item not found")
	ErrVersionConflict     = errors.New("import batch was changed by someone else; re-read it and retry")
	ErrInvalidState        = errors.New("that action is not allowed in the batch's current state")
	ErrValidation          = errors.New("invalid import request")
	ErrDuplicateBatch      = errors.New("this file has already been submitted for this profile and entity")
	ErrSelfApproval        = errors.New("a material import cannot be approved by the person who created it")
	ErrPreviewMismatch     = errors.New("the approval does not match the previewed content")
	ErrHashMismatch        = errors.New("the uploaded content does not match its declared hash")
	ErrChunkConflict       = errors.New("a different chunk was already stored at that index")
	ErrTooLarge            = errors.New("the import exceeds the allowed size")
	ErrNoScanner           = errors.New("no malware scanner is configured; imports are refused until one is")
	ErrScannerUnavailable  = errors.New("the malware scanner is unavailable; the content is unscanned")
	ErrNotPlatformScope    = errors.New("import profiles are published from the platform scope only")
	ErrProfileNotCertified = errors.New("the import profile is not certified")
	ErrProfileSelfCertify  = errors.New("a profile cannot be certified by the person who authored it")
	ErrTargetNotConfigured = errors.New("the profile names a target this deployment does not have")
	ErrRetryNotAllowed     = errors.New("that item cannot be retried")
	ErrIncompleteUpload    = errors.New("the upload is incomplete")
)
