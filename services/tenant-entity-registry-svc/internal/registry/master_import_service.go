package registry

// ORG-9 master imports: upload, approve, read, and the API-5 operations that carry the work (ZS-SVC-U-001 s19, ZS-API-001 s18).
//
// Uploading and approving are always asynchronous: they answer at once with an operation, and the work runs in the background as
// the principal who uploaded the file. The same Idempotency-Key from the same principal on the same route is the same
// operation, never a second import. A file with any invalid row cannot be approved, and nobody approves their own upload.

import (
	"context"
	"errors"
	"fmt"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// Operation types and routes.
const (
	OpMasterImportValidate  = "master_import.validate"
	OpMasterImportImport    = "master_import.import"
	RouteMasterImportUpload = "POST /v1/master-imports"
)

func routeMasterImportApprove(importID string) string {
	return "POST /v1/master-imports/" + importID + ":approve"
}

// SetImportExecutor sets how background work is run. The default is a goroutine. A test runs it inline.
func (s *Service) SetImportExecutor(run func(func())) { s.importAsync = run }

func (s *Service) masterImportStore() (MasterImportStore, error) {
	ms, ok := s.store.(MasterImportStore)
	if !ok {
		return nil, ErrServiceUnavailable
	}
	return ms, nil
}

func (s *Service) runImportAsync(f func()) {
	if s.importAsync != nil {
		s.importAsync(f)
		return
	}
	go f()
}

// detached keeps what the request carried (tenant, principal, scope) but not its cancellation: the work outlives the request.
func detached(ctx context.Context, tenantID, principal, scopeEntity string) context.Context {
	c := context.WithoutCancel(ctx)
	c = domain.WithTenant(c, tenantID)
	c = domain.WithPrincipal(c, principal)
	if scopeEntity != "" {
		c = domain.WithLegalEntity(c, scopeEntity)
	}
	return c
}

// CreateMasterImport records an upload and starts its validation. idempotencyKey may be empty, in which case every call is a
// new import.
func (s *Service) CreateMasterImport(ctx context.Context, req domain.CreateMasterImportRequest, idempotencyKey string) (*MasterImportOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "master-import", "upload"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	ms, err := s.masterImportStore()
	if err != nil {
		return nil, err
	}
	rows, err := req.SplitRows()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	tenantID, principal, scope := domain.TenantFromContext(ctx), domain.PrincipalFromContext(ctx), domain.LegalEntityFromContext(ctx)
	out, err := ms.CreateMasterImport(ctx, NewMasterImport{
		Request: req, Rows: rows, Actor: principal, ScopeEntityID: scope,
		Operation: apicontract.Operation{ID: apicontract.NewOperationID(), TenantID: tenantID, PrincipalID: principal, Type: OpMasterImportValidate,
			Route: RouteMasterImportUpload, State: apicontract.OperationQueued, Cancelable: true, IdempotencyKey: idempotencyKey,
			RequestHash: req.RequestHash(), Progress: &apicontract.OperationProgress{TotalUnits: int64(len(rows))}},
	})
	if err != nil {
		if errors.Is(err, apicontract.ErrOperationConflict) {
			return nil, fmt.Errorf("%w", ErrIdempotencyKeyMismatch)
		}
		return nil, fmt.Errorf("store.CreateMasterImport: %w", err)
	}
	if out.Created {
		dctx := detached(ctx, tenantID, principal, scope)
		runner := NewImportRunner(ms, s, s.log)
		s.runImportAsync(func() { runner.RunValidation(dctx, out.Import.ImportID, out.Operation.ID) })
	}
	return out, nil
}

// ApproveMasterImport approves a previewed import, which starts importing it.
func (s *Service) ApproveMasterImport(ctx context.Context, importID string, req domain.ApproveMasterImportRequest, idempotencyKey string) (*MasterImportOutcome, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.authorize(ctx, "master-import", "approve"); err != nil {
		return nil, err
	}
	if err := s.assertTenantMayTransact(ctx, ""); err != nil {
		return nil, err
	}
	ms, err := s.masterImportStore()
	if err != nil {
		return nil, err
	}
	principal := domain.PrincipalFromContext(ctx)
	route := routeMasterImportApprove(importID)
	if idempotencyKey == "" {
		// Approving twice is the same approval: one import has one import operation.
		idempotencyKey = "approve:" + importID
	}
	out, err := ms.ApproveMasterImport(ctx, importID, req, principal, apicontract.Operation{
		ID: apicontract.NewOperationID(), TenantID: domain.TenantFromContext(ctx), PrincipalID: principal, Type: OpMasterImportImport, Route: route,
		State: apicontract.OperationQueued, Cancelable: true, IdempotencyKey: idempotencyKey, RequestHash: importID + "|" + req.EvidenceRef,
	}, s.expectedVersionOptional)
	if err != nil {
		if errors.Is(err, apicontract.ErrOperationConflict) {
			return nil, fmt.Errorf("%w", ErrIdempotencyKeyMismatch)
		}
		return nil, fmt.Errorf("store.ApproveMasterImport: %w", err)
	}
	if out.Created {
		// The work runs as the principal who uploaded the file, in the scope it was uploaded under.
		imp := out.Import
		dctx := detached(ctx, imp.TenantID, imp.UploadedBy, imp.ScopeEntityID)
		runner := NewImportRunner(ms, s, s.log)
		s.runImportAsync(func() { runner.RunImport(dctx, imp.ImportID, out.Operation.ID) })
	}
	return out, nil
}

// GetMasterImport returns an import with its counts and, once finished, its reconciliation.
func (s *Service) GetMasterImport(ctx context.Context, importID string) (*domain.MasterImport, error) {
	ms, err := s.masterImportStore()
	if err != nil {
		return nil, err
	}
	out, err := ms.GetMasterImport(ctx, importID)
	if err != nil {
		return nil, fmt.Errorf("store.GetMasterImport: %w", err)
	}
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// ListImportRows returns the verdicts and outcomes of an import's rows.
func (s *Service) ListImportRows(ctx context.Context, importID string, q domain.ImportRowQuery) ([]domain.ImportRow, error) {
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	ms, err := s.masterImportStore()
	if err != nil {
		return nil, err
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	out, err := ms.ListImportRows(ctx, importID, q)
	if err != nil {
		return nil, fmt.Errorf("store.ListImportRows: %w", err)
	}
	return out, nil
}

// GetOperation returns an API-5 operation of the tenant.
func (s *Service) GetOperation(ctx context.Context, operationID string) (apicontract.Operation, error) {
	ms, err := s.masterImportStore()
	if err != nil {
		return apicontract.Operation{}, err
	}
	op, err := ms.Operations().Get(ctx, domain.TenantFromContext(ctx), operationID)
	if errors.Is(err, apicontract.ErrOperationNotFound) {
		return apicontract.Operation{}, ErrNotFound
	}
	if err != nil {
		return apicontract.Operation{}, fmt.Errorf("store.GetOperation: %w", err)
	}
	return op, nil
}

// CancelOperation asks for an operation to stop. Only before its point of no return: once an import has started creating
// parties it cannot be cancelled (ErrStateConflict).
func (s *Service) CancelOperation(ctx context.Context, operationID string) (apicontract.Operation, error) {
	if err := s.authorize(ctx, "master-import", "cancel"); err != nil {
		return apicontract.Operation{}, err
	}
	ms, err := s.masterImportStore()
	if err != nil {
		return apicontract.Operation{}, err
	}
	op, err := ms.Operations().RequestCancel(ctx, domain.TenantFromContext(ctx), operationID)
	switch {
	case errors.Is(err, apicontract.ErrOperationNotFound):
		return apicontract.Operation{}, ErrNotFound
	case errors.Is(err, apicontract.ErrOperationNotCancelable):
		return apicontract.Operation{}, fmt.Errorf("%w: the operation is finished or past its point of no return", ErrStateConflict)
	case err != nil:
		return apicontract.Operation{}, fmt.Errorf("store.CancelOperation: %w", err)
	}
	return op, nil
}
