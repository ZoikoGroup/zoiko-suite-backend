package store

// Canonical DRC events (ZS-SVC-S-001 §9.3) for the retention, legal-hold and
// preservation modules. Each is written to the transactional outbox INSIDE the
// transaction that makes the change it announces, so a rolled-back change
// leaves no event and a committed one leaves exactly one.
//
// The event NAMES are the specification's (§9.3), e.g. legal_hold_activated.
// The older document and record events keep their existing dotted names
// (document.uploaded, record.declared, ...): whether those should be renamed
// is a compatibility question for their consumers and is deliberately not
// decided here. Payloads carry the §9.3 "minimum payload anchors" that this
// service's data model can supply; the ones it cannot (a hold scope_version,
// an incident_ref, an export recipient_scope) are not invented.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/document-vault-svc/internal/outbox"
)

const (
	evtRetentionScheduleResolved = "retention_schedule_resolved"
	evtRecordDispositionDue      = "record_disposition_due"
	evtLegalHoldActivated        = "legal_hold_activated"
	evtLegalHoldTargetAdded      = "legal_hold_target_added"
	evtLegalHoldReleased         = "legal_hold_released"
	evtRenditionCreated          = "rendition_created"
	evtIntegrityCheckFailed      = "integrity_check_failed"
	evtExportPackageCreated      = "export_package_created"
)

// enqueueDRCEvent writes one event in the caller's transaction.
// legalEntityID may be "" for a tenant-level aggregate (hold, export package).
func enqueueDRCEvent(ctx context.Context, tx pgx.Tx, aggregateType, aggregateID, eventType, tenantID, legalEntityID, actorID, correlationID string, payload map[string]any) error {
	if correlationID == "" {
		correlationID = aggregateID
	}
	payload["tenant_id"] = tenantID
	env, err := outbox.NewVariantAEnvelope(eventType, correlationID, tenantID, legalEntityID, actorID, payload)
	if err != nil {
		return fmt.Errorf("build outbox envelope: %w", err)
	}
	var actor *string
	if actorID != "" {
		actor = &actorID
	}
	if err := outbox.Insert(ctx, tx, outbox.Event{
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		EventType:     eventType,
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		ActorID:       actor,
		CorrelationID: correlationID,
		Payload:       env,
	}); err != nil {
		return fmt.Errorf("outbox insert: %w", err)
	}
	return nil
}

// recordLegalEntity returns the legal entity that owns a record ("" if none).
func recordLegalEntity(ctx context.Context, tx pgx.Tx, tenantID, recordID string) (string, error) {
	var le string
	if err := tx.QueryRow(ctx, `SELECT legal_entity_id::text FROM records WHERE record_id = $1 AND tenant_id::text = $2`,
		recordID, tenantID).Scan(&le); err != nil {
		return "", fmt.Errorf("document store unavailable: %w", err)
	}
	return le, nil
}

// activeHoldState reports whether a record is the target of an active hold.
func activeHoldState(ctx context.Context, tx pgx.Tx, tenantID, recordID string) (string, error) {
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM legal_hold_targets
		WHERE record_id = $1 AND tenant_id::text = $2 AND applied_at IS NOT NULL AND released_at IS NULL)`,
		recordID, tenantID).Scan(&held); err != nil {
		return "", fmt.Errorf("document store unavailable: %w", err)
	}
	if held {
		return "ACTIVE_HOLD", nil
	}
	return "NO_HOLD", nil
}
