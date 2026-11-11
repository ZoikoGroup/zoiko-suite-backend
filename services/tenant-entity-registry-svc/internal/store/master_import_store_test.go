package store_test

// ORG-9 master imports against a real Postgres, end to end through the runner and the real party creation path: the async
// operations (202 states, polling, idempotency, cancel before the point of no return), a bad row blocking approval, the uploader
// not approving their own file, every row accounted for at reconciliation, existing matches left as conflicts, a run that can be
// repeated, and what the database holds whatever the service does.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	importUploader = "p-import-uploader"
	importApprover = "p-import-approver"
)

// storeCreator is the ordinary party creation path, as the uploader.
type storeCreator struct {
	f       *orgFixture
	failRef string
	after   func()
	calls   int
}

func (c *storeCreator) CreateParty(ctx context.Context, req domain.CreatePartyRequest) (*domain.CreatePartyResult, error) {
	c.calls++
	if c.after != nil {
		c.after()
	}
	if req.SourceRef == c.failRef {
		return nil, fmt.Errorf("%w: the party was refused", registry.ErrInvalidInput)
	}
	return c.f.s.CreateParty(ctx, registry.PartyCreateCommand{Request: req, Actor: importUploader, Fingerprint: req.Fingerprint()})
}

func (f *orgFixture) runner(c *storeCreator) *registry.ImportRunner {
	return registry.NewImportRunner(f.s, c, nil)
}

func partyRow(ref, name, lei string) string {
	return fmt.Sprintf(`{"row_ref":%q,"party_kind":"LEGAL_ENTITY","names":[{"type":"LEGAL_NAME","value":%q}],"country":"GB","identifiers":[{"scheme":"LEI","value":%q}]}`, ref, name, lei)
}

func uploadReq(rows ...string) domain.CreateMasterImportRequest {
	return domain.CreateMasterImportRequest{Kind: domain.ImportKindParty, SourceName: "crm", SourceVersion: "2026-10", EvidenceRef: "export-77", ValidFrom: "2025-01-01",
		Rows: json.RawMessage("[" + strings.Join(rows, ",") + "]")}
}

func (f *orgFixture) upload(t *testing.T, req domain.CreateMasterImportRequest, key string) *registry.MasterImportOutcome {
	t.Helper()
	out, err := f.tryUpload(req, key)
	require.NoError(t, err)
	return out
}

func (f *orgFixture) tryUpload(req domain.CreateMasterImportRequest, key string) (*registry.MasterImportOutcome, error) {
	rows, err := req.SplitRows()
	if err != nil {
		return nil, err
	}
	return f.s.CreateMasterImport(f.ctx, registry.NewMasterImport{Request: req, Rows: rows, Actor: importUploader, Operation: apicontract.Operation{
		ID: apicontract.NewOperationID(), TenantID: f.tenantID, PrincipalID: importUploader, Type: registry.OpMasterImportValidate, Route: registry.RouteMasterImportUpload,
		State: apicontract.OperationQueued, Cancelable: true, IdempotencyKey: key, RequestHash: req.RequestHash()}})
}

func (f *orgFixture) op(t *testing.T, id string) apicontract.Operation {
	t.Helper()
	op, err := f.s.Operations().Get(f.ctx, f.tenantID, id)
	require.NoError(t, err)
	return op
}

func (f *orgFixture) imp(t *testing.T, id string) *domain.MasterImport {
	t.Helper()
	m, err := f.s.GetMasterImport(f.ctx, id)
	require.NoError(t, err)
	require.NotNil(t, m)
	return m
}

// validated uploads a file and runs its validation.
func (f *orgFixture) validated(t *testing.T, req domain.CreateMasterImportRequest, key string) (*domain.MasterImport, apicontract.Operation) {
	t.Helper()
	out := f.upload(t, req, key)
	f.runner(&storeCreator{f: f}).RunValidation(f.ctx, out.Import.ImportID, out.Operation.ID)
	return f.imp(t, out.Import.ImportID), f.op(t, out.Operation.ID)
}

func (f *orgFixture) approve(importID string, version int64, actor, key string) (*registry.MasterImportOutcome, error) {
	return f.s.ApproveMasterImport(f.ctx, importID, domain.ApproveMasterImportRequest{ExpectedVersion: version, EvidenceRef: "approval-1"}, actor, apicontract.Operation{
		ID: apicontract.NewOperationID(), TenantID: f.tenantID, PrincipalID: actor, Type: registry.OpMasterImportImport, Route: "POST /v1/master-imports/" + importID + ":approve",
		State: apicontract.OperationQueued, Cancelable: true, IdempotencyKey: key, RequestHash: importID + "|approval-1"}, false)
}

func (f *orgFixture) rowsOf(t *testing.T, importID, status string) []domain.ImportRow {
	t.Helper()
	rows, err := f.s.ListImportRows(f.ctx, importID, domain.ImportRowQuery{Status: status, Limit: 200})
	require.NoError(t, err)
	return rows
}

var threeRows = []string{
	partyRow("C-1", "Alpha Holdings Limited", "LEI-IMP-1"),
	partyRow("C-2", "Zenith Foods Limited", "LEI-IMP-2"),
	partyRow("C-3", "Quartz Mining Limited", "LEI-IMP-3"),
}

func TestMasterImport_UploadValidatePreviewApproveImportReconcile(t *testing.T) {
	f := newORGFixture(t)
	out := f.upload(t, uploadReq(threeRows...), "upload-1")
	assert.True(t, out.Created)
	assert.Equal(t, domain.ImportUploaded, out.Import.Status)
	assert.Equal(t, 3, out.Import.RowCount)
	assert.Equal(t, apicontract.OperationQueued, out.Operation.State, "202: accepted, not yet running")
	assert.Equal(t, out.Operation.ID, out.Import.ValidateOpID)
	assert.Len(t, out.Import.ContentSHA256, 64)
	assert.Equal(t, 3, len(f.rowsOf(t, out.Import.ImportID, domain.RowPending)), "every row is kept as received, pending its verdict")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'master_import.uploaded' AND tenant_id = $1`, f.tenantID))

	imp, op := f.validated(t, uploadReq(threeRows...), "upload-2")
	assert.Equal(t, domain.ImportPreviewReady, imp.Status)
	assert.Equal(t, 3, imp.ValidRows)
	assert.Zero(t, imp.InvalidRows)
	assert.Equal(t, apicontract.OperationSucceeded, op.State, "polling reaches a terminal state")
	assert.Equal(t, "/v1/master-imports/"+imp.ImportID, op.ResourceRef)
	assert.EqualValues(t, 3, op.Progress.CompletedUnits)
	assert.False(t, op.Cancelable)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'master_import.validated' AND tenant_id = $1`, f.tenantID))
	valid := f.rowsOf(t, imp.ImportID, domain.RowValid)
	require.Len(t, valid, 3)
	assert.Equal(t, "C-1", valid[0].RowRef)
	assert.Equal(t, 0, partyCount(t, f), "nothing is created by validation or preview")

	_, err := f.approve(imp.ImportID, imp.RecordVersion, importUploader, "approve-1")
	require.ErrorIs(t, err, registry.ErrSelfApproval, "nobody approves their own upload")
	_, err = f.approve(imp.ImportID, 0, importApprover, "approve-1")
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	_, err = f.approve(imp.ImportID, imp.RecordVersion+7, importApprover, "approve-1")
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.approve(uuid.NewString(), 1, importApprover, "approve-1")
	require.ErrorIs(t, err, registry.ErrNotFound)

	appr, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "approve-1")
	require.NoError(t, err)
	assert.True(t, appr.Created)
	assert.Equal(t, domain.ImportApproved, appr.Import.Status)
	assert.Equal(t, importApprover, appr.Import.ApprovedBy)
	assert.Equal(t, appr.Operation.ID, appr.Import.ImportOpID)
	assert.Equal(t, apicontract.OperationQueued, appr.Operation.State)
	assert.Equal(t, 0, partyCount(t, f), "approval starts the work; it has not run")

	// Approving again with the same key is the same operation, and records nothing.
	again, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "approve-1")
	require.NoError(t, err)
	assert.False(t, again.Created)
	assert.Equal(t, appr.Operation.ID, again.Operation.ID)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'master_import.approved' AND tenant_id = $1`, f.tenantID))

	creator := &storeCreator{f: f}
	f.runner(creator).RunImport(f.ctx, imp.ImportID, appr.Operation.ID)
	done := f.imp(t, imp.ImportID)
	assert.Equal(t, domain.ImportCompleted, done.Status)
	assert.Equal(t, 3, done.ImportedRows)
	require.NotNil(t, done.Reconciliation)
	assert.True(t, done.Reconciliation.Matched, "%+v", done.Reconciliation)
	assert.Equal(t, 3, done.Reconciliation.SourceRows)
	assert.Equal(t, 3, done.Reconciliation.StagedRows)
	assert.Equal(t, 3, done.Reconciliation.Imported)
	assert.Equal(t, 3, partyCount(t, f))
	op = f.op(t, appr.Operation.ID)
	assert.Equal(t, apicontract.OperationSucceeded, op.State)
	assert.EqualValues(t, 3, op.Progress.CompletedUnits)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'master_import.completed' AND tenant_id = $1`, f.tenantID))

	imported := f.rowsOf(t, imp.ImportID, domain.RowImported)
	require.Len(t, imported, 3)
	for _, r := range imported {
		assert.Equal(t, "CREATED", r.Outcome)
		assert.NotEmpty(t, r.PartyID)
		assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM parties WHERE party_id = $1`, r.PartyID))
	}
	// The import carries its provenance: each party was created from a row of this import, as a system import, never as the registry.
	assert.Equal(t, 3, f.count(t, `SELECT count(*) FROM party_create_requests WHERE tenant_id = $1 AND create_request_id LIKE $2`, f.tenantID, "imp:"+imp.ImportID+":%"))
}

func TestMasterImport_ABadRowBlocksApprovalAndEveryErrorIsKept(t *testing.T) {
	f := newORGFixture(t)
	imp, op := f.validated(t, uploadReq(
		partyRow("B-1", "Good Co Limited", "LEI-B-1"),
		`{"row_ref":"B-2","party_kind":"NOT_A_KIND","names":[]}`,
		`"garbage"`,
		partyRow("B-1", "Repeated Ref Limited", "LEI-B-4"),
		partyRow("B-5", "Same Lei Limited", "LEI-B-1"),
	), "bad-file")
	assert.Equal(t, domain.ImportBlocked, imp.Status)
	assert.Equal(t, 1, imp.ValidRows)
	assert.Equal(t, 4, imp.InvalidRows)
	assert.Equal(t, apicontract.OperationSucceeded, op.State, "validation itself succeeded; the answer is that the file is blocked")

	invalid := f.rowsOf(t, imp.ImportID, domain.RowInvalid)
	require.Len(t, invalid, 4)
	for _, r := range invalid {
		assert.NotEmpty(t, r.Errors, "row %d keeps why it is invalid", r.RowNo)
		assert.NotEmpty(t, r.RowRef)
	}
	byNo := map[int]domain.ImportRow{}
	for _, r := range invalid {
		byNo[r.RowNo] = r
	}
	assert.Equal(t, domain.IssueInvalid, byNo[2].Errors[0].Code)
	assert.Equal(t, domain.IssueMalformed, byNo[3].Errors[0].Code)
	assert.Equal(t, domain.IssueDuplicateRef, byNo[4].Errors[0].Code)
	assert.Equal(t, domain.IssueDuplicateIdentifier, byNo[5].Errors[len(byNo[5].Errors)-1].Code)

	_, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "approve-bad")
	require.ErrorIs(t, err, registry.ErrStateConflict, "a bad row blocks approval")
	assert.Contains(t, err.Error(), "4 invalid")
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM async_operations WHERE route LIKE '%:approve'`), "a refused approval leaves no operation behind")
	assert.Equal(t, domain.ImportBlocked, f.imp(t, imp.ImportID).Status)
	assert.Equal(t, 0, partyCount(t, f))

	// The table refuses it as well, whatever the service does.
	require.Error(t, f.execTenant(t, `UPDATE master_imports SET status = 'APPROVED', approved_by = 'someone', approved_at = now() WHERE import_id = $1`, imp.ImportID))
	require.Error(t, f.execTenant(t, `UPDATE master_imports SET status = 'PREVIEW_READY' WHERE import_id = $1`, imp.ImportID), "a blocked import does not become previewable")
}

func TestMasterImport_RetryWithTheSameKeyGivesTheSameOperation(t *testing.T) {
	f := newORGFixture(t)
	first := f.upload(t, uploadReq(threeRows...), "same-key")
	retry := f.upload(t, uploadReq(threeRows...), "same-key")
	assert.False(t, retry.Created)
	assert.Equal(t, first.Operation.ID, retry.Operation.ID, "the same key is the same operation")
	assert.Equal(t, first.Import.ImportID, retry.Import.ImportID)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM master_imports`), "no second import")
	assert.Equal(t, 3, f.count(t, `SELECT count(*) FROM master_import_rows`), "and no second copy of the rows")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'master_import.uploaded' AND tenant_id = $1`, f.tenantID))

	_, err := f.tryUpload(uploadReq(threeRows[:2]...), "same-key")
	require.ErrorIs(t, err, apicontract.ErrOperationConflict, "the same key with another file is refused")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM master_imports`))

	// No key means no deduplication, and a different principal's key is theirs alone.
	f.upload(t, uploadReq(threeRows...), "")
	f.upload(t, uploadReq(threeRows...), "")
	assert.Equal(t, 3, f.count(t, `SELECT count(*) FROM master_imports`))
	other, err := f.s.CreateMasterImport(f.ctx, registry.NewMasterImport{Request: uploadReq(threeRows...), Rows: mustRows(t, uploadReq(threeRows...)), Actor: "p-other", Operation: apicontract.Operation{
		ID: apicontract.NewOperationID(), TenantID: f.tenantID, PrincipalID: "p-other", Type: registry.OpMasterImportValidate, Route: registry.RouteMasterImportUpload,
		State: apicontract.OperationQueued, Cancelable: true, IdempotencyKey: "same-key", RequestHash: uploadReq(threeRows...).RequestHash()}})
	require.NoError(t, err)
	assert.True(t, other.Created)
	assert.NotEqual(t, first.Operation.ID, other.Operation.ID)
}

func mustRows(t *testing.T, req domain.CreateMasterImportRequest) []json.RawMessage {
	t.Helper()
	rows, err := req.SplitRows()
	require.NoError(t, err)
	return rows
}

func TestMasterImport_AnExistingMatchIsAConflictNeverAnOverwrite(t *testing.T) {
	f := newORGFixture(t)
	existing := f.party(t, "pre-1", "Alpha Holdings Limited", "LEI-PRE-1")
	before := f.fresh(t, existing.PartyID)

	imp, _ := f.validated(t, uploadReq(
		partyRow("D-1", "Alpha Holdings Limited", "LEI-D-1"), // the same name and country as an existing party
		partyRow("D-2", "Zenith Foods Limited", "LEI-D-2"),
	), "dup-file")
	require.Equal(t, domain.ImportPreviewReady, imp.Status)
	appr, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "")
	require.NoError(t, err)
	f.runner(&storeCreator{f: f}).RunImport(f.ctx, imp.ImportID, appr.Operation.ID)

	done := f.imp(t, imp.ImportID)
	assert.Equal(t, domain.ImportCompleted, done.Status)
	assert.True(t, done.Reconciliation.Matched)
	assert.Equal(t, 1, done.ImportedRows)
	assert.Equal(t, 1, done.ConflictRows, "the row that matches an existing party is left for stewardship")
	conflicts := f.rowsOf(t, imp.ImportID, domain.RowConflict)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "D-1", conflicts[0].RowRef)
	assert.Contains(t, []string{"MATCHED_EXISTING", "REVIEW_REQUIRED"}, conflicts[0].Outcome)
	after := f.fresh(t, existing.PartyID)
	assert.Equal(t, before.MasterVersion, after.MasterVersion, "the existing party is untouched: no blind overwrite")
}

func TestMasterImport_ACancelBeforeThePointOfNoReturnStopsItAndNothingIsCreated(t *testing.T) {
	f := newORGFixture(t)

	// Cancelled while validation is still queued.
	up := f.upload(t, uploadReq(threeRows...), "cancel-validate")
	cancelled, err := f.s.Operations().RequestCancel(f.ctx, f.tenantID, up.Operation.ID)
	require.NoError(t, err)
	assert.Equal(t, apicontract.OperationCancelRequested, cancelled.State)
	f.runner(&storeCreator{f: f}).RunValidation(f.ctx, up.Import.ImportID, up.Operation.ID)
	assert.Equal(t, apicontract.OperationCancelled, f.op(t, up.Operation.ID).State)
	assert.Equal(t, domain.ImportCancelled, f.imp(t, up.Import.ImportID).Status)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'master_import.cancelled' AND tenant_id = $1`, f.tenantID))
	_, err = f.s.Operations().RequestCancel(f.ctx, f.tenantID, up.Operation.ID)
	require.ErrorIs(t, err, apicontract.ErrOperationNotCancelable, "a finished operation cannot be cancelled")

	// Cancelled after approval but before the import started.
	imp, _ := f.validated(t, uploadReq(threeRows...), "cancel-import")
	appr, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "")
	require.NoError(t, err)
	_, err = f.s.Operations().RequestCancel(f.ctx, f.tenantID, appr.Operation.ID)
	require.NoError(t, err)
	creator := &storeCreator{f: f}
	f.runner(creator).RunImport(f.ctx, imp.ImportID, appr.Operation.ID)
	assert.Equal(t, apicontract.OperationCancelled, f.op(t, appr.Operation.ID).State)
	assert.Equal(t, domain.ImportCancelled, f.imp(t, imp.ImportID).Status)
	assert.Zero(t, creator.calls, "no party was created")
	assert.Equal(t, 0, partyCount(t, f))
}

func TestMasterImport_CancellingAfterThePointOfNoReturnIsRefused(t *testing.T) {
	f := newORGFixture(t)
	imp, _ := f.validated(t, uploadReq(threeRows...), "late-cancel")
	appr, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "")
	require.NoError(t, err)

	var cancelErr error
	var cancelableDuring bool
	creator := &storeCreator{f: f}
	creator.after = func() {
		if creator.calls == 1 {
			cancelableDuring = f.op(t, appr.Operation.ID).Cancelable
			_, cancelErr = f.s.Operations().RequestCancel(f.ctx, f.tenantID, appr.Operation.ID)
		}
	}
	f.runner(creator).RunImport(f.ctx, imp.ImportID, appr.Operation.ID)
	assert.False(t, cancelableDuring, "once parties are being created the operation is no longer cancelable")
	require.ErrorIs(t, cancelErr, apicontract.ErrOperationNotCancelable)
	assert.Equal(t, domain.ImportCompleted, f.imp(t, imp.ImportID).Status, "and it runs to the end")
	assert.Equal(t, 3, partyCount(t, f))
}

func TestMasterImport_AFailedRowIsAccountedForAndTheRestImport(t *testing.T) {
	f := newORGFixture(t)
	imp, _ := f.validated(t, uploadReq(threeRows...), "partial")
	appr, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "")
	require.NoError(t, err)
	f.runner(&storeCreator{f: f, failRef: "C-2"}).RunImport(f.ctx, imp.ImportID, appr.Operation.ID)

	done := f.imp(t, imp.ImportID)
	assert.Equal(t, domain.ImportCompleted, done.Status)
	assert.Equal(t, 2, done.ImportedRows)
	assert.Equal(t, 1, done.FailedRows)
	assert.True(t, done.Reconciliation.Matched, "a failed row is accounted for: %+v", done.Reconciliation)
	failed := f.rowsOf(t, imp.ImportID, domain.RowFailed)
	require.Len(t, failed, 1)
	assert.Equal(t, "C-2", failed[0].RowRef)
	require.NotEmpty(t, failed[0].Errors)
	assert.Equal(t, domain.IssueInvalid, failed[0].Errors[0].Code)
	assert.Equal(t, 2, partyCount(t, f), "no partial hidden import: what did not import says so")
}

func TestMasterImport_ReconciliationFailsWhenARowWasNotAccountedFor(t *testing.T) {
	f := newORGFixture(t)
	imp, _ := f.validated(t, uploadReq(threeRows...), "unreconciled")
	appr, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "")
	require.NoError(t, err)
	started, err := f.s.StartImport(f.ctx, imp.ImportID, appr.Operation.ID)
	require.NoError(t, err)
	require.True(t, started)
	// One row is imported and the other two are never touched.
	rows, err := f.s.NextImportRows(f.ctx, imp.ImportID, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, f.s.RecordRowOutcome(f.ctx, imp.ImportID, rows[0].RowNo, domain.RowOutcome{Status: domain.RowImported, Outcome: "CREATED", PartyID: uuid.NewString()}))
	require.Error(t, f.s.RecordRowOutcome(f.ctx, imp.ImportID, rows[0].RowNo, domain.RowOutcome{Status: domain.RowConflict, Outcome: "X"}), "a row has one outcome")

	done, err := f.s.CompleteImport(f.ctx, imp.ImportID)
	require.NoError(t, err)
	assert.Equal(t, domain.ImportFailed, done.Status, "an import that does not account for every row does not complete")
	assert.False(t, done.Reconciliation.Matched)
	assert.Equal(t, 2, done.Reconciliation.Unresolved)
	assert.Equal(t, 1, done.ImportedRows)
}

func TestMasterImport_AnInterruptedImportResumesWithoutDuplicates(t *testing.T) {
	f := newORGFixture(t)
	imp, _ := f.validated(t, uploadReq(threeRows...), "resume")
	appr, err := f.approve(imp.ImportID, imp.RecordVersion, importApprover, "")
	require.NoError(t, err)

	// The first run gets one row done, then stops (the process went away): the party is made and its outcome recorded.
	started, err := f.s.StartImport(f.ctx, imp.ImportID, appr.Operation.ID)
	require.NoError(t, err)
	require.True(t, started)
	rows, err := f.s.NextImportRows(f.ctx, imp.ImportID, 1)
	require.NoError(t, err)
	parsed, err := domain.ParseImportRow(rows[0].Raw)
	require.NoError(t, err)
	d := domain.ImportDefaults{ImportID: imp.ImportID, SourceName: "crm", SourceClass: domain.SourceSystemImport, EvidenceRef: "export-77", ValidFrom: "2025-01-01"}
	res, err := (&storeCreator{f: f}).CreateParty(f.ctx, *parsed.PartyRequest(d, rows[0].RowNo))
	require.NoError(t, err)
	require.Equal(t, 1, partyCount(t, f))
	// ... and it dies between making the party and recording the outcome of the second row.
	rows2, _ := f.s.NextImportRows(f.ctx, imp.ImportID, 2)
	parsed2, _ := domain.ParseImportRow(rows2[1].Raw)
	_, err = (&storeCreator{f: f}).CreateParty(f.ctx, *parsed2.PartyRequest(d, rows2[1].RowNo))
	require.NoError(t, err)
	require.NoError(t, f.s.RecordRowOutcome(f.ctx, imp.ImportID, rows[0].RowNo, domain.OutcomeOf(res)))
	require.Equal(t, 2, partyCount(t, f))

	// Running it again finishes the work: the half-done row finds its party by its request id instead of making another.
	f.runner(&storeCreator{f: f}).RunImport(f.ctx, imp.ImportID, appr.Operation.ID)
	done := f.imp(t, imp.ImportID)
	assert.Equal(t, domain.ImportCompleted, done.Status)
	assert.True(t, done.Reconciliation.Matched)
	assert.Equal(t, 3, partyCount(t, f), "three rows, three parties: a repeated row never makes a second one")
	assert.Equal(t, apicontract.OperationSucceeded, f.op(t, appr.Operation.ID).State)
	again, err := f.s.StartImport(f.ctx, imp.ImportID, appr.Operation.ID)
	require.Error(t, err, "a finished operation is not started again")
	assert.False(t, again)
}

func TestMasterImport_RowsPageAndNeverShowTheRowAsReceived(t *testing.T) {
	f := newORGFixture(t)
	imp, _ := f.validated(t, uploadReq(threeRows...), "paging")
	page, err := f.s.ListImportRows(f.ctx, imp.ImportID, domain.ImportRowQuery{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, 1, page[0].RowNo)
	next, err := f.s.ListImportRows(f.ctx, imp.ImportID, domain.ImportRowQuery{AfterRow: page[1].RowNo, Limit: 2})
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.Equal(t, 3, next[0].RowNo)
	encoded, _ := json.Marshal(page)
	assert.NotContains(t, string(encoded), "LEI-IMP", "the row as received can carry identifiers and is not returned")
	assert.NotContains(t, string(encoded), "Alpha Holdings")
	_, err = f.s.ListImportRows(f.ctx, uuid.NewString(), domain.ImportRowQuery{Limit: 5})
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.s.ListImportRows(f.ctx, "not-a-uuid", domain.ImportRowQuery{Limit: 5})
	require.ErrorIs(t, err, registry.ErrNotFound)
	got, err := f.s.GetMasterImport(f.ctx, "not-a-uuid")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMasterImport_TheDatabaseHoldsWhatTheServicePromises(t *testing.T) {
	f := newORGFixture(t)
	imp, _ := f.validated(t, uploadReq(threeRows...), "guards")
	id := imp.ImportID
	require.Error(t, f.execTenant(t, `UPDATE master_import_rows SET raw = '{"row_ref":"x"}'::jsonb WHERE import_id = $1 AND row_no = 1`, id), "a row as received never changes")
	require.Error(t, f.execTenant(t, `UPDATE master_import_rows SET row_ref = 'other' WHERE import_id = $1 AND row_no = 1`, id), "nor its reference once known")
	require.Error(t, f.execTenant(t, `DELETE FROM master_import_rows WHERE import_id = $1`, id))
	require.Error(t, f.execTenant(t, `DELETE FROM master_imports WHERE import_id = $1`, id))
	require.Error(t, f.execTenant(t, `UPDATE master_import_rows SET status = 'PENDING' WHERE import_id = $1 AND row_no = 1`, id), "a row never moves backward")
	require.Error(t, f.execTenant(t, `UPDATE master_import_rows SET status = 'IMPORTED' WHERE import_id = $1 AND row_no = 1`, id), "an imported row names its party")
	require.Error(t, f.execTenant(t, `UPDATE master_imports SET row_count = 99 WHERE import_id = $1`, id), "an import as uploaded never changes")
	require.Error(t, f.execTenant(t, `UPDATE master_imports SET content_sha256 = repeat('0', 64) WHERE import_id = $1`, id))
	require.Error(t, f.execTenant(t, `UPDATE master_imports SET status = 'IMPORTING' WHERE import_id = $1`, id), "an import cannot skip approval")
	require.Error(t, f.execTenant(t, `UPDATE master_imports SET status = 'APPROVED', approved_by = uploaded_by, approved_at = now() WHERE import_id = $1`, id), "nobody approves their own upload")
	require.Error(t, f.execTenant(t, `UPDATE master_imports SET status = 'APPROVED' WHERE import_id = $1`, id), "an approval names its approver")
	require.Error(t, f.execTenant(t, `INSERT INTO master_import_rows (import_id, tenant_id, row_no, raw, status, errors) VALUES ($1, $2, 99, '{}'::jsonb, 'INVALID', '[]'::jsonb)`, id, f.tenantID),
		"an invalid row says why")
}

func TestMasterImport_TenantsNeverSeeEachOther(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	imp, op := a.validated(t, uploadReq(threeRows...), "isolation")

	got, err := b.s.GetMasterImport(b.ctx, imp.ImportID)
	require.NoError(t, err)
	assert.Nil(t, got)
	_, err = b.s.ListImportRows(b.ctx, imp.ImportID, domain.ImportRowQuery{Limit: 5})
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.Operations().Get(b.ctx, b.tenantID, op.ID)
	require.ErrorIs(t, err, apicontract.ErrOperationNotFound)
	_, err = b.s.Operations().RequestCancel(b.ctx, b.tenantID, op.ID)
	require.ErrorIs(t, err, apicontract.ErrOperationNotFound)
	_, err = b.approve(imp.ImportID, imp.RecordVersion, importApprover, "")
	require.ErrorIs(t, err, registry.ErrNotFound)
	// The same key in another tenant is another operation.
	other := b.upload(t, uploadReq(threeRows...), "isolation")
	assert.True(t, other.Created)
	assert.NotEqual(t, op.ID, other.Operation.ID)
}

// Migration 000027 reverses cleanly while no import exists and applies again; with one it refuses.
func TestMasterImport_MigrationDownAndUp(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	exists := func(q string) bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, q).Scan(&n))
		return n > 0
	}
	const table = `SELECT count(*) FROM pg_tables WHERE tablename = 'master_import_rows'`

	f.upload(t, uploadReq(threeRows...), "mig")
	_, err := f.pool.Exec(ctx, read("000027_master_imports.down.sql"))
	require.Error(t, err, "down is refused while imports exist")
	assert.Contains(t, err.Error(), "000027 down refused")

	f2 := newORGFixture(t)
	_, err = f2.pool.Exec(ctx, read("000027_master_imports.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists(table))
	_, err = f2.pool.Exec(ctx, read("000027_master_imports.up.sql"))
	require.NoError(t, err, "up applies again after down")
	assert.True(t, exists(table))
}
