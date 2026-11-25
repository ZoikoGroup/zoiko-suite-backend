package domain

import "time"

// OutboxEvent is one domain event written to outbox_events in the SAME
// transaction as the state change it describes. internal/events.Relay later
// publishes it to Kafka, using OutboxEventID as the event id so consumers can
// deduplicate redeliveries.
type OutboxEvent struct {
	OutboxEventID string
	AggregateType string
	AggregateID   string
	EventType     string
	TenantID      string
	LegalEntityID string
	ActorID       string
	CorrelationID string
	Payload       interface{}
	CreatedAt     time.Time
}

// Aggregate types used in outbox_events.aggregate_type.
const (
	AggregateReportDefinition = "report_definition"
	AggregateReportRun        = "report_run"
	AggregateAuditExport      = "audit_export"
)

// Event types. Names follow the "<domain>.<object>.<verb>.v1" catalogue style
// (Audit & Assurance spec §6 uses audit.export.delivered.v1).
const (
	EventDefinitionCreated       = "report.definition.created.v1"
	EventDefinitionStatusChanged = "report.definition.status_changed.v1"
	EventRunRecorded             = "report.run.not_implemented.v1"

	EventExportRequested        = "audit.export.requested.v1"
	EventExportApproved         = "audit.export.approved.v1"
	EventExportBuilt            = "audit.export.built.v1"
	EventExportBuildFailed      = "audit.export.build_failed.v1"
	EventExportRedactionApplied = "audit.export.redaction_applied.v1"
	EventExportSealed           = "audit.export.sealed.v1"
	EventExportDelivered        = "audit.export.delivered.v1"
	EventExportAccessRevoked    = "audit.export.access_revoked.v1"
)
