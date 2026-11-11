package events

// ORG-9 master import events. They carry identifiers, states, counts, the content hash and the principals, and never a row:
// a row can carry personal data and identifiers, and a consumer reads outcomes from the registry.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventMasterImportUploaded  = "master_import.uploaded"
	EventMasterImportValidated = "master_import.validated"
	EventMasterImportApproved  = "master_import.approved"
	EventMasterImportCompleted = "master_import.completed"
	EventMasterImportCancelled = "master_import.cancelled"
)

// MasterImportEventNames is every master import wire name the code can emit.
var MasterImportEventNames = []string{EventMasterImportUploaded, EventMasterImportValidated, EventMasterImportApproved,
	EventMasterImportCompleted, EventMasterImportCancelled}

// MasterImportRecord is any of the master import events, for the import as it stands after the change.
func MasterImportRecord(eventType string, imp domain.MasterImport, actor, corr string) (*outbox.Record, error) {
	p := map[string]any{
		"tenant_id": imp.TenantID, "import_id": imp.ImportID, "kind": imp.Kind, "status": imp.Status,
		"source_name": imp.SourceName, "source_version": imp.SourceVersion, "content_sha256": imp.ContentSHA256,
		"row_count": imp.RowCount, "valid_rows": imp.ValidRows, "invalid_rows": imp.InvalidRows,
		"imported_rows": imp.ImportedRows, "conflict_rows": imp.ConflictRows, "failed_rows": imp.FailedRows,
		"uploaded_by": imp.UploadedBy, "approved_by": imp.ApprovedBy,
	}
	if imp.Reconciliation != nil {
		p["reconciliation"] = imp.Reconciliation
	}
	evidence := imp.EvidenceRef
	if imp.ApprovalEvidence != "" && eventType != EventMasterImportUploaded {
		evidence = imp.ApprovalEvidence
	}
	return BuildRecord(RecordSpec{
		EventType: eventType, TenantID: imp.TenantID, ActorID: actor, CorrelationID: corr,
		ObjectID: imp.ImportID, ObjectVersion: imp.RecordVersion, PartitionKey: imp.ImportID, EvidenceRef: evidence, Payload: p,
	})
}
