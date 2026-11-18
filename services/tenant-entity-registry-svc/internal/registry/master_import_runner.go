package registry

// The work behind a master import, run as the API-5 operation that carries it. Validation checks every row; importing creates
// the parties of the valid rows through the ordinary party creation path, as the principal who uploaded the file, and
// reconciles. Both phases are safe to run again: validation only moves an UPLOADED import, and a row's party is created under a
// request id derived from the import and the row, so a repeated row never makes a second party.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// PartyCreator creates a party from a request: the ordinary path, with its authorization and its matching.
type PartyCreator interface {
	CreateParty(ctx context.Context, req domain.CreatePartyRequest) (*domain.CreatePartyResult, error)
}

// ImportBatchSize is how many rows are taken at a time while importing.
const ImportBatchSize = 100

// ImportRunner carries out the phases of a master import.
type ImportRunner struct {
	store   MasterImportStore
	parties PartyCreator
	log     *zap.Logger
}

// NewImportRunner returns a runner over a store and the party creation path.
func NewImportRunner(store MasterImportStore, parties PartyCreator, log *zap.Logger) *ImportRunner {
	if log == nil {
		log = zap.NewNop()
	}
	return &ImportRunner{store: store, parties: parties, log: log}
}

func importResource(importID string) string { return "/v1/master-imports/" + importID }

func importDefaults(imp *domain.MasterImport) domain.ImportDefaults {
	return domain.ImportDefaults{ImportID: imp.ImportID, SourceName: imp.SourceName, SourceClass: domain.SourceClass(imp.SourceClass),
		EvidenceRef: imp.EvidenceRef, ValidFrom: imp.ValidFrom}
}

func (r *ImportRunner) cancelled(ctx context.Context, tenantID, opID string) bool {
	op, err := r.store.Operations().Get(ctx, tenantID, opID)
	return err == nil && op.State == apicontract.OperationCancelRequested
}

// fail ends the operation failed with a problem, and the import with it.
func (r *ImportRunner) fail(ctx context.Context, tenantID, importID, opID, code string, cause error) {
	r.log.Error("master import failed", zap.String("import_id", importID), zap.String("code", code), zap.Error(cause))
	if err := r.store.FailImport(ctx, importID, cause.Error()); err != nil {
		r.log.Error("master import: could not mark failed", zap.String("import_id", importID), zap.Error(err))
	}
	p := apicontract.Problem{Type: apicontract.ProblemBase + code, Title: "The master import failed", Status: http.StatusInternalServerError, Code: code, Detail: cause.Error()}
	if _, err := r.store.Operations().Advance(ctx, tenantID, opID, apicontract.OperationFailed, nil, &p, importResource(importID)); err != nil {
		r.log.Error("master import: could not fail the operation", zap.String("operation_id", opID), zap.Error(err))
	}
}

// RunValidation validates every row of an uploaded file. The operation ends succeeded when the verdicts are recorded, whatever
// they are: an import with invalid rows is a valid answer, BLOCKED, not a failure of the operation.
func (r *ImportRunner) RunValidation(ctx context.Context, importID, opID string) {
	tenantID := domain.TenantFromContext(ctx)
	ops := r.store.Operations()
	if r.cancelled(ctx, tenantID, opID) {
		_ = r.store.CancelImport(ctx, importID)
		_, _ = ops.Advance(ctx, tenantID, opID, apicontract.OperationCancelled, nil, nil, "")
		return
	}
	if _, err := ops.Advance(ctx, tenantID, opID, apicontract.OperationRunning, nil, nil, ""); err != nil {
		r.log.Warn("master import: validation not started", zap.String("operation_id", opID), zap.Error(err))
		return
	}
	imp, rows, err := r.store.BeginValidation(ctx, importID)
	if err != nil {
		r.fail(ctx, tenantID, importID, opID, "import_validation_failed", err)
		return
	}
	raws := make([][]byte, len(rows))
	for i, row := range rows {
		raws[i] = row.Raw
	}
	verdicts := domain.ValidateImportRows(raws, importDefaults(imp))
	if r.cancelled(ctx, tenantID, opID) {
		_ = r.store.CancelImport(ctx, importID)
		_, _ = ops.Advance(ctx, tenantID, opID, apicontract.OperationCancelled, nil, nil, "")
		return
	}
	done, err := r.store.SaveValidation(ctx, importID, verdicts)
	if err != nil {
		r.fail(ctx, tenantID, importID, opID, "import_validation_failed", err)
		return
	}
	_, _ = ops.Advance(ctx, tenantID, opID, apicontract.OperationSucceeded,
		&apicontract.OperationProgress{CompletedUnits: int64(done.RowCount), TotalUnits: int64(done.RowCount)}, nil, importResource(importID))
}

// RunImport creates the parties of an approved import and reconciles it.
func (r *ImportRunner) RunImport(ctx context.Context, importID, opID string) {
	tenantID := domain.TenantFromContext(ctx)
	ops := r.store.Operations()
	started, err := r.store.StartImport(ctx, importID, opID)
	if err != nil {
		r.fail(ctx, tenantID, importID, opID, "import_failed", err)
		return
	}
	if !started {
		_, _ = ops.Advance(ctx, tenantID, opID, apicontract.OperationCancelled, nil, nil, "")
		return
	}
	imp, err := r.store.GetMasterImport(ctx, importID)
	if err != nil || imp == nil {
		r.fail(ctx, tenantID, importID, opID, "import_failed", fmt.Errorf("import not readable: %v", err))
		return
	}
	defaults := importDefaults(imp)
	total := int64(imp.ValidRows)
	var done int64
	for {
		rows, err := r.store.NextImportRows(ctx, importID, ImportBatchSize)
		if err != nil {
			r.fail(ctx, tenantID, importID, opID, "import_failed", err)
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if err := r.importRow(ctx, importID, defaults, row); err != nil {
				r.fail(ctx, tenantID, importID, opID, "import_failed", err)
				return
			}
			done++
		}
		_, _ = ops.Advance(ctx, tenantID, opID, apicontract.OperationRunning, &apicontract.OperationProgress{CompletedUnits: done, TotalUnits: total}, nil, "")
	}
	final, err := r.store.CompleteImport(ctx, importID)
	if err != nil {
		r.fail(ctx, tenantID, importID, opID, "import_failed", err)
		return
	}
	if final.Status != domain.ImportCompleted {
		r.fail(ctx, tenantID, importID, opID, "import_reconciliation_failed", fmt.Errorf("the rows imported do not reconcile with the rows received"))
		return
	}
	_, _ = ops.Advance(ctx, tenantID, opID, apicontract.OperationSucceeded, &apicontract.OperationProgress{CompletedUnits: done, TotalUnits: total}, nil, importResource(importID))
}

// importRow creates one row's party and records the outcome. A refusal of the row is its outcome, recorded and visible; only a
// failure to record makes the import itself fail.
func (r *ImportRunner) importRow(ctx context.Context, importID string, d domain.ImportDefaults, row domain.StagedRow) error {
	parsed, err := domain.ParseImportRow(row.Raw)
	if err != nil {
		return r.store.RecordRowOutcome(ctx, importID, row.RowNo, domain.RowOutcome{Status: domain.RowFailed,
			Issues: []domain.RowIssue{{Code: domain.IssueMalformed, Message: err.Error()}}})
	}
	res, err := r.parties.CreateParty(ctx, *parsed.PartyRequest(d, row.RowNo))
	if err != nil {
		code := domain.IssueCreateFailed
		if errors.Is(err, ErrInvalidInput) {
			code = domain.IssueInvalid
		}
		return r.store.RecordRowOutcome(ctx, importID, row.RowNo, domain.RowOutcome{Status: domain.RowFailed,
			Issues: []domain.RowIssue{{Code: code, Message: err.Error()}}})
	}
	return r.store.RecordRowOutcome(ctx, importID, row.RowNo, domain.OutcomeOf(res))
}
