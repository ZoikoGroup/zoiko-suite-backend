package events

import (
	"fmt"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// BuildAccountingEvents renders the events of one WFP-05 change. kind is the
// store's AccountingChangeKind value (CREATED, SUBMITTED, POSTED, REJECTED,
// CORRECTED). No amounts and no line detail travel; consumers read the subject
// through the API. Events are keyed by result, so everything that happens to one
// result's accounting stays in order.
func BuildAccountingEvents(kind string, b, original *domain.AccountingBridge, reason, actor, correlationID, causationID string) ([]domain.OutboxEvent, error) {
	var typ string
	switch kind {
	case "CREATED":
		typ = domain.EventPayrollAccountingEventCreated
	case "SUBMITTED":
		typ = domain.EventPayrollAccountingSubmitted
	case "POSTED":
		typ = domain.EventPayrollAccountingPosted
	case "REJECTED":
		typ = domain.EventPayrollAccountingRejected
	case "CORRECTED":
		typ = domain.EventPayrollAccountingCorrected
	default:
		return nil, fmt.Errorf("unknown accounting change %q", kind)
	}
	payload := map[string]any{
		"bridge_id": b.BridgeID, "run_ref_id": b.RunRefID, "result_id": b.ResultID, "legal_entity_id": b.LegalEntityID,
		"accounting_book_id": b.AccountingBookID, "posting_purpose": b.PostingPurpose, "fiscal_period": b.FiscalPeriod,
		"state": b.State, "line_count": b.LineCount, "evidence_flags": b.EvidenceFlags, "source_event_id": b.SourceEventID,
		"journal_id": b.JournalID, "execution_id": b.ExecutionID, "original_bridge_id": b.OriginalBridgeID,
		"correction_kind": b.CorrectionKind, "reason": reason,
	}
	if original != nil && b.OriginalBridgeID == nil {
		payload["related_bridge_id"] = original.BridgeID
	}
	body, err := BuildWFPEnvelope(typ, correlationID, causationID, actor, b.TenantID, b.LegalEntityID, "", payload)
	if err != nil {
		return nil, err
	}
	return []domain.OutboxEvent{{TenantID: b.TenantID, ObjectID: b.ResultID, EventType: typ, Payload: body}}, nil
}
