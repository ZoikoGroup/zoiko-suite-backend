package events

import (
	"fmt"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// BuildLiabilityEvents renders the events of one WFP-06 change. kind is the store's
// LiabilityChangeKind value. Amounts travel only where the spec's consumers need
// them (a settlement's amount, an adjustment's delta); everything else is read
// through the API. Events are keyed by result so one result's liabilities stay in
// order.
func BuildLiabilityEvents(kind string, l *domain.PayrollLiability, exc *domain.LiabilityException, detail map[string]any, actor, correlationID, causationID string) ([]domain.OutboxEvent, error) {
	if kind == "EXCEPTION" {
		if exc == nil {
			return nil, fmt.Errorf("exception change without an exception")
		}
		payload := map[string]any{
			"exception_id": exc.ExceptionID, "result_id": exc.ResultID, "legal_entity_id": exc.LegalEntityID,
			"liability_id": exc.LiabilityID, "code": exc.Code, "subject_key": exc.SubjectKey, "amount": exc.Amount,
			"source": "WFP-06",
		}
		body, err := BuildWFPEnvelope(domain.EventPayrollReconExceptionCreated, correlationID, causationID, actor, exc.TenantID, exc.LegalEntityID, "", payload)
		if err != nil {
			return nil, err
		}
		return []domain.OutboxEvent{{TenantID: exc.TenantID, ObjectID: exc.ResultID, EventType: domain.EventPayrollReconExceptionCreated, Payload: body}}, nil
	}
	if l == nil {
		return nil, fmt.Errorf("liability change %q without a liability", kind)
	}
	var typ string
	switch kind {
	case "RECOGNIZED":
		typ = domain.EventPayrollLiabilityRecognized
	case "DUE":
		typ = domain.EventPayrollLiabilityDue
	case "SCHEDULED":
		typ = domain.EventPayrollLiabilityScheduled
	case "SETTLED":
		typ = domain.EventPayrollLiabilitySettled
	case "ADJUSTED", "SUPERSEDED":
		typ = domain.EventPayrollLiabilityAdjusted
	case "DISPUTED", "DISPUTE_CLEARED":
		typ = domain.EventPayrollLiabilityDisputed
	case "RECONCILIATION_FAILED":
		typ = domain.EventPayrollLiabilityReconciliationFailed
	default:
		return nil, fmt.Errorf("unknown liability change %q", kind)
	}
	payload := map[string]any{
		"liability_id": l.LiabilityID, "run_ref_id": l.RunRefID, "result_id": l.ResultID, "legal_entity_id": l.LegalEntityID,
		"payroll_period_id": l.PayrollPeriodID, "class_code": l.ClassCode, "liability_type": l.LiabilityType,
		"party_side": l.PartySide, "authority_counterparty_id": l.AuthorityCounterpartyID, "jurisdiction_id": l.JurisdictionID,
		"currency": l.Currency, "due_date": l.DueDate, "state": l.State, "exception_state": l.ExceptionState,
		"change": kind, "version": l.Version,
	}
	for k, v := range detail {
		payload[k] = v
	}
	body, err := BuildWFPEnvelope(typ, correlationID, causationID, actor, l.TenantID, l.LegalEntityID, "", payload)
	if err != nil {
		return nil, err
	}
	return []domain.OutboxEvent{{TenantID: l.TenantID, ObjectID: l.ResultID, EventType: typ, Payload: body}}, nil
}
