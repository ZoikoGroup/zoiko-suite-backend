package registry

import (
	"context"
	"encoding/json"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// MasterImportStore is the data-access contract for ORG-9 master imports (migration 000027). Asserted by the service like
// PartyStore. Every method runs in the caller's tenant under row-level security. It also owns the API-5 operation table.
type MasterImportStore interface {
	// CreateMasterImport records an upload: the import, its rows as received and its validation operation, atomically. An
	// idempotency key already used by this principal for this route returns the existing operation and Created false, without
	// recording anything; the same key with a different request is apicontract.ErrOperationConflict.
	CreateMasterImport(ctx context.Context, in NewMasterImport) (*MasterImportOutcome, error)
	// ApproveMasterImport approves a PREVIEW_READY import and records its import operation, atomically. A file with any invalid
	// row cannot be approved (ErrStateConflict), nor by its uploader (ErrSelfApproval). The same key returns the same operation.
	ApproveMasterImport(ctx context.Context, importID string, req domain.ApproveMasterImportRequest, actor string, op apicontract.Operation, allowUnversioned bool) (*MasterImportOutcome, error)

	// GetMasterImport returns the import; (nil, nil) when absent.
	GetMasterImport(ctx context.Context, importID string) (*domain.MasterImport, error)
	ListImportRows(ctx context.Context, importID string, q domain.ImportRowQuery) ([]domain.ImportRow, error)

	// BeginValidation moves an UPLOADED import to VALIDATING and returns its rows as received.
	BeginValidation(ctx context.Context, importID string) (*domain.MasterImport, []domain.StagedRow, error)
	// SaveValidation records every row's verdict and its errors, and moves the import to PREVIEW_READY, or BLOCKED when any row
	// is invalid.
	SaveValidation(ctx context.Context, importID string, verdicts []domain.RowValidation) (*domain.MasterImport, error)
	// StartImport is the point of no return: it moves the import to IMPORTING and its operation to running and not cancelable.
	// started is false, and the import CANCELLED, when cancellation was requested first.
	StartImport(ctx context.Context, importID, operationID string) (started bool, err error)
	// NextImportRows returns up to limit VALID rows that have no outcome yet, in row order.
	NextImportRows(ctx context.Context, importID string, limit int) ([]domain.StagedRow, error)
	// RecordRowOutcome records what importing a row did. A row has one outcome.
	RecordRowOutcome(ctx context.Context, importID string, rowNo int, o domain.RowOutcome) error
	// CompleteImport reconciles the import and ends it COMPLETED when every row is accounted for exactly once, FAILED when not.
	CompleteImport(ctx context.Context, importID string) (*domain.MasterImport, error)
	// CancelImport ends a not yet started import CANCELLED.
	CancelImport(ctx context.Context, importID string) error
	// FailImport ends an import FAILED when its work could not be carried out.
	FailImport(ctx context.Context, importID, reason string) error

	// Operations is the API-5 operation store, scoped to the caller's tenant.
	Operations() apicontract.OperationStore
}

// NewMasterImport is an upload to record.
type NewMasterImport struct {
	Request       domain.CreateMasterImportRequest
	Rows          []json.RawMessage
	Actor         string
	ScopeEntityID string
	// Operation is the validation operation, prefilled: id, tenant, principal, type, route, key, request hash.
	Operation apicontract.Operation
}

// MasterImportOutcome is an import and the operation that carries its current phase.
type MasterImportOutcome struct {
	Import    *domain.MasterImport  `json:"import"`
	Operation apicontract.Operation `json:"operation"`
	// Created is false when the request replayed an earlier one by its idempotency key.
	Created bool `json:"-"`
}
