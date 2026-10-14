package events

import (
	"fmt"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// BuildPaymentEvents renders the event of one WFP-07 change. kind is the store's PaymentChange
// kind. No amount, payee reference or destination travels: consumers read the instruction through
// the API. Events are keyed by result so one result's payments stay in order.
func BuildPaymentEvents(kind string, i *domain.PaymentInstruction, reason, actor, correlationID, causationID string) ([]domain.OutboxEvent, error) {
	if i == nil {
		return nil, fmt.Errorf("payment change %q without an instruction", kind)
	}
	var typ string
	switch kind {
	case "CREATED":
		typ = domain.EventPayrollPaymentInstrCreated
	case "APPROVED":
		typ = domain.EventPayrollPaymentInstrApproved
	case "INVALIDATED":
		typ = domain.EventPayrollPaymentInstrInvalidated
	case "SUBMITTED":
		typ = domain.EventPayrollPaymentSubmitted
	case "CANCELLED":
		typ = domain.EventPayrollPaymentInstrCancelled
	case "SUPERSEDED":
		typ = domain.EventPayrollPaymentInstrSuperseded
	default:
		return nil, fmt.Errorf("unknown payment change %q", kind)
	}
	payload := map[string]any{
		"instruction_id": i.InstructionID, "instruction_type": i.InstructionType, "run_ref_id": i.RunRefID, "result_id": i.ResultID,
		"worker_id": i.WorkerID, "liability_id": i.LiabilityID, "legal_entity_id": i.LegalEntityID, "currency": i.Currency,
		"value_date": i.ValueDate, "state": i.State, "fingerprint": i.Fingerprint, "required_signatures": i.RequiredSignatures,
		"attempt_id": i.AttemptID, "banking_status": i.BankingStatus, "reason": reason, "version": i.Version,
	}
	body, err := BuildWFPEnvelope(typ, correlationID, causationID, actor, i.TenantID, i.LegalEntityID, "", payload)
	if err != nil {
		return nil, err
	}
	return []domain.OutboxEvent{{TenantID: i.TenantID, ObjectID: i.ResultID, EventType: typ, Payload: body}}, nil
}
