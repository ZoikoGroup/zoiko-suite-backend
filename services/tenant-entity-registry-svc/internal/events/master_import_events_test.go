package events_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

func TestMasterImportEvents_CarryCountsAndStatesNeverRows(t *testing.T) {
	imp := domain.MasterImport{ImportID: "0190a000-0000-7000-8000-0000000a0001", TenantID: "0190a000-0000-7000-8000-0000000000aa", Kind: domain.ImportKindParty,
		Status: domain.ImportCompleted, SourceName: "crm", SourceVersion: "2026-10", ContentSHA256: "abc", RowCount: 10, ValidRows: 8, InvalidRows: 0,
		ImportedRows: 6, ConflictRows: 1, FailedRows: 1, UploadedBy: "up", ApprovedBy: "ap", EvidenceRef: "upload-ev", ApprovalEvidence: "approve-ev", RecordVersion: 4,
		Reconciliation: &domain.Reconciliation{SourceRows: 10, StagedRows: 10, Imported: 6, Conflicts: 1, Failed: 1, Invalid: 2, Matched: true}}
	for _, eventType := range events.MasterImportEventNames {
		rec, err := events.MasterImportRecord(eventType, imp, "ap", "c")
		require.NoError(t, err)
		assert.Equal(t, eventType, rec.EventType)
		p := string(rec.Payload)
		assert.Contains(t, p, `"import_id":"`+imp.ImportID+`"`)
		assert.Contains(t, p, `"row_count":10`)
		assert.Contains(t, p, `"imported_rows":6`)
		assert.Contains(t, p, `"content_sha256":"abc"`)
		assert.Contains(t, p, `"uploaded_by":"up"`)
		assert.Contains(t, p, `"matched":true`, "the reconciliation travels with the event")
		assert.NotContains(t, p, "Alpha", "a row is never carried")
	}
	up, err := events.MasterImportRecord(events.EventMasterImportUploaded, imp, "up", "c")
	require.NoError(t, err)
	assert.Contains(t, string(up.Payload), "upload-ev", "an upload is evidenced by its own reference")
	assert.NotContains(t, string(up.Payload), "approve-ev")
	done, err := events.MasterImportRecord(events.EventMasterImportCompleted, imp, "ap", "c")
	require.NoError(t, err)
	assert.Contains(t, string(done.Payload), "approve-ev")
	assert.Len(t, events.MasterImportEventNames, 5)
}
