package registry_test

// ORG-9 service: validation before authorization before the store, the right permission per command, the idempotency key mapped
// to one operation, background work started exactly when something was created, and a store without imports answering
// "unavailable". The phases themselves are exercised against a real database in the store tests.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type importOps struct {
	registry.MasterImportStore
	cancelErr error
	getErr    error
	canceled  string
}

func (o *importOps) Create(context.Context, apicontract.Operation) (apicontract.Operation, bool, error) {
	return apicontract.Operation{}, false, nil
}
func (o *importOps) Get(_ context.Context, _ string, id string) (apicontract.Operation, error) {
	return apicontract.Operation{ID: id}, o.getErr
}
func (o *importOps) FindByKey(context.Context, string, string, string, string) (apicontract.Operation, bool, error) {
	return apicontract.Operation{}, false, nil
}
func (o *importOps) Advance(context.Context, string, string, apicontract.OperationState, *apicontract.OperationProgress, *apicontract.Problem, string) (apicontract.Operation, error) {
	return apicontract.Operation{}, nil
}
func (o *importOps) RequestCancel(_ context.Context, _ string, id string) (apicontract.Operation, error) {
	o.canceled = id
	return apicontract.Operation{ID: id, State: apicontract.OperationCancelRequested}, o.cancelErr
}

type importFake struct {
	*memStore
	registry.MasterImportStore
	ops     *importOps
	calls   int
	created bool
	err     error
	actor   string
	lastOp  apicontract.Operation
	lastKey string
	scope   string
}

func (f *importFake) Operations() apicontract.OperationStore { return f.ops }
func (f *importFake) CreateMasterImport(_ context.Context, in registry.NewMasterImport) (*registry.MasterImportOutcome, error) {
	f.calls++
	f.actor, f.lastOp, f.lastKey, f.scope = in.Actor, in.Operation, in.Operation.IdempotencyKey, in.ScopeEntityID
	if f.err != nil {
		return nil, f.err
	}
	return &registry.MasterImportOutcome{Import: &domain.MasterImport{ImportID: "imp1", TenantID: "tenant-001", UploadedBy: in.Actor}, Operation: in.Operation, Created: f.created}, nil
}
func (f *importFake) ApproveMasterImport(_ context.Context, id string, _ domain.ApproveMasterImportRequest, actor string, op apicontract.Operation, _ bool) (*registry.MasterImportOutcome, error) {
	f.calls++
	f.actor, f.lastOp, f.lastKey = actor, op, op.IdempotencyKey
	if f.err != nil {
		return nil, f.err
	}
	return &registry.MasterImportOutcome{Import: &domain.MasterImport{ImportID: id, TenantID: "tenant-001", UploadedBy: "uploader"}, Operation: op, Created: f.created}, nil
}
func (f *importFake) GetMasterImport(_ context.Context, id string) (*domain.MasterImport, error) {
	f.calls++
	if id == "missing" {
		return nil, nil
	}
	return &domain.MasterImport{ImportID: id}, nil
}

func importSvc(t *testing.T, az registry.AuthorizationClient) (*registry.Service, *importFake, *int) {
	t.Helper()
	f := &importFake{memStore: newMemStore(), ops: &importOps{}, created: true}
	svc := newSvc(t, f, az, acceptAllJurisd{})
	ran := 0
	svc.SetImportExecutor(func(fn func()) { ran++ }) // work is started, not run: the phases are tested against a database
	return svc, f, &ran
}

func upload() domain.CreateMasterImportRequest {
	return domain.CreateMasterImportRequest{Kind: domain.ImportKindParty, SourceName: "crm", SourceVersion: "1", EvidenceRef: "e", ValidFrom: "2026-01-01",
		Rows: json.RawMessage(`[{"row_ref":"a"}]`)}
}

func TestMasterImport_UploadValidatesAuthorizesThenRecordsAndStartsWork(t *testing.T) {
	svc, f, ran := importSvc(t, permitAllAuthZ{})
	bad := upload()
	bad.Kind = "NOPE"
	_, err := svc.CreateMasterImport(tenantCtx("tenant-001"), bad, "k")
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, f.calls)

	az := &recordingAuthZ{}
	asvc, af, aran := importSvc(t, az)
	out, err := asvc.CreateMasterImport(tenantCtx("tenant-001"), upload(), "key-1")
	require.NoError(t, err)
	assert.Equal(t, "master-import", az.resource)
	assert.Equal(t, "upload", az.action)
	assert.Equal(t, testPrincipal, af.actor)
	assert.Equal(t, "key-1", af.lastKey)
	assert.Equal(t, registry.OpMasterImportValidate, af.lastOp.Type)
	assert.Equal(t, registry.RouteMasterImportUpload, af.lastOp.Route)
	assert.Equal(t, testPrincipal, af.lastOp.PrincipalID)
	assert.Equal(t, upload().RequestHash(), af.lastOp.RequestHash, "the hash is what makes the same key with another file a mismatch")
	assert.True(t, af.lastOp.Cancelable)
	assert.NotEmpty(t, out.Operation.ID)
	assert.Equal(t, 1, *aran, "validation is started in the background")

	// A replay by key records nothing and starts nothing.
	af.created = false
	_, err = asvc.CreateMasterImport(tenantCtx("tenant-001"), upload(), "key-1")
	require.NoError(t, err)
	assert.Equal(t, 1, *aran)

	af.err = apicontract.ErrOperationConflict
	_, err = asvc.CreateMasterImport(tenantCtx("tenant-001"), upload(), "key-1")
	require.ErrorIs(t, err, registry.ErrIdempotencyKeyMismatch)
	require.ErrorIs(t, err, registry.ErrConflict)

	deny, df, dran := importSvc(t, denyAllAuthZ{})
	_, err = deny.CreateMasterImport(tenantCtx("tenant-001"), upload(), "k")
	require.ErrorIs(t, err, registry.ErrUnauthorized)
	assert.Zero(t, df.calls)
	assert.Zero(t, *dran)

	f.tenants["tenant-001"].LifecycleState = domain.TenantLifecycleSuspended
	_, err = svc.CreateMasterImport(tenantCtx("tenant-001"), upload(), "k")
	require.Error(t, err)
	assert.Zero(t, f.calls)
	assert.Zero(t, *ran)
}

func TestMasterImport_ApproveIsItsOwnPermissionAndAKeyedOperation(t *testing.T) {
	az := &recordingAuthZ{}
	svc, f, ran := importSvc(t, az)
	_, err := svc.ApproveMasterImport(tenantCtx("tenant-001"), "imp1", domain.ApproveMasterImportRequest{}, "")
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	assert.Zero(t, f.calls)

	_, err = svc.ApproveMasterImport(tenantCtx("tenant-001"), "imp1", domain.ApproveMasterImportRequest{ExpectedVersion: 2, EvidenceRef: "e"}, "")
	require.NoError(t, err)
	assert.Equal(t, "approve", az.action)
	assert.Equal(t, "master-import", az.resource)
	assert.Equal(t, registry.OpMasterImportImport, f.lastOp.Type)
	assert.Equal(t, "approve:imp1", f.lastKey, "without a key, approving twice is still one operation")
	assert.Equal(t, "POST /v1/master-imports/imp1:approve", f.lastOp.Route)
	assert.Equal(t, 1, *ran)

	_, err = svc.ApproveMasterImport(tenantCtx("tenant-001"), "imp1", domain.ApproveMasterImportRequest{ExpectedVersion: 2, EvidenceRef: "e"}, "caller-key")
	require.NoError(t, err)
	assert.Equal(t, "caller-key", f.lastKey)

	f.created = false
	_, err = svc.ApproveMasterImport(tenantCtx("tenant-001"), "imp1", domain.ApproveMasterImportRequest{ExpectedVersion: 2, EvidenceRef: "e"}, "caller-key")
	require.NoError(t, err)
	assert.Equal(t, 2, *ran, "a replay starts nothing")

	f.err = registry.ErrSelfApproval
	_, err = svc.ApproveMasterImport(tenantCtx("tenant-001"), "imp1", domain.ApproveMasterImportRequest{ExpectedVersion: 2, EvidenceRef: "e"}, "")
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	assert.Equal(t, 2, *ran)

	deny, _, _ := importSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized})
	_, err = deny.ApproveMasterImport(tenantCtx("tenant-001"), "imp1", domain.ApproveMasterImportRequest{ExpectedVersion: 2, EvidenceRef: "e"}, "")
	require.ErrorIs(t, err, registry.ErrUnauthorized)
}

func TestMasterImport_OperationsAndReads(t *testing.T) {
	az := &recordingAuthZ{}
	svc, f, _ := importSvc(t, az)
	ctx := tenantCtx("tenant-001")

	op, err := svc.CancelOperation(ctx, "op_1")
	require.NoError(t, err)
	assert.Equal(t, apicontract.OperationCancelRequested, op.State)
	assert.Equal(t, "cancel", az.action)
	assert.Equal(t, "op_1", f.ops.canceled)

	f.ops.cancelErr = apicontract.ErrOperationNotCancelable
	_, err = svc.CancelOperation(ctx, "op_1")
	require.ErrorIs(t, err, registry.ErrStateConflict, "past the point of no return is a conflict")
	f.ops.cancelErr = apicontract.ErrOperationNotFound
	_, err = svc.CancelOperation(ctx, "op_9")
	require.ErrorIs(t, err, registry.ErrNotFound)

	f.ops.getErr = apicontract.ErrOperationNotFound
	_, err = svc.GetOperation(ctx, "op_9")
	require.ErrorIs(t, err, registry.ErrNotFound)
	f.ops.getErr = nil
	got, err := svc.GetOperation(ctx, "op_1")
	require.NoError(t, err)
	assert.Equal(t, "op_1", got.ID)

	_, err = svc.GetMasterImport(ctx, "missing")
	assert.ErrorIs(t, err, registry.ErrNotFound)
	_, err = svc.ListImportRows(ctx, "imp1", domain.ImportRowQuery{Status: "NOPE"})
	assert.ErrorIs(t, err, registry.ErrInvalidInput)

	deny, _, _ := importSvc(t, &recordingAuthZ{err: registry.ErrUnauthorized})
	_, err = deny.CancelOperation(ctx, "op_1")
	assert.ErrorIs(t, err, registry.ErrUnauthorized)
}

func TestMasterImport_AStoreWithoutImportsIsUnavailable(t *testing.T) {
	svc := newSvc(t, newMemStore(), permitAllAuthZ{}, acceptAllJurisd{})
	ctx := tenantCtx("tenant-001")
	_, err := svc.CreateMasterImport(ctx, upload(), "k")
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.GetMasterImport(ctx, "imp1")
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.GetOperation(ctx, "op_1")
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
	_, err = svc.CancelOperation(ctx, "op_1")
	assert.ErrorIs(t, err, registry.ErrServiceUnavailable)
}
