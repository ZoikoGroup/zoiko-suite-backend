package events

import (
	"fmt"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// BuildReconEvents renders the events of one Wave 8 change. Events are keyed by the
// payroll result so one result's reconciliation stays in order with its accounting,
// liabilities and payments. No worker, bank or payee detail travels; an exception carries
// its difference amount because consumers (close, finance control) need to size it.
func BuildReconEvents(kind string, run *domain.ReconRun, exc *domain.ReconException, ins *domain.PaymentInstruction,
	obs *domain.BankingObservation, actor, correlationID, causationID string) ([]domain.OutboxEvent, error) {
	switch kind {
	case "RUN_COMPLETED", "RUN_CERTIFIED":
		if run == nil {
			return nil, fmt.Errorf("reconciliation change %q without a run", kind)
		}
		typ := domain.EventPayrollReconRunCompleted
		if kind == "RUN_CERTIFIED" {
			typ = domain.EventPayrollReconRunCertified
		}
		payload := map[string]any{
			"recon_run_id": run.ReconRunID, "run_ref_id": run.RunRefID, "result_id": run.ResultID, "legal_entity_id": run.LegalEntityID,
			"outcome": run.Outcome, "population_hash": run.PopulationHash, "exception_count": run.ExceptionCount,
			"reperformed_from": run.ReperformedFrom, "certified_by": run.CertifiedBy,
		}
		body, err := BuildWFPEnvelope(typ, correlationID, causationID, actor, run.TenantID, run.LegalEntityID, "", payload)
		if err != nil {
			return nil, err
		}
		return []domain.OutboxEvent{{TenantID: run.TenantID, ObjectID: run.ResultID, EventType: typ, Payload: body}}, nil

	case "EXCEPTION_CREATED", "EXCEPTION_RESOLVED", "EXCEPTION_WAIVED":
		if exc == nil {
			return nil, fmt.Errorf("reconciliation change %q without an exception", kind)
		}
		typ := domain.EventPayrollReconExceptionCreated
		switch kind {
		case "EXCEPTION_RESOLVED":
			typ = domain.EventPayrollReconExceptionResolved
		case "EXCEPTION_WAIVED":
			typ = domain.EventPayrollReconExceptionWaived
		}
		payload := map[string]any{
			"exception_id": exc.ExceptionID, "result_id": exc.ResultID, "run_ref_id": exc.RunRefID, "legal_entity_id": exc.LegalEntityID,
			"recon_run_id": exc.ReconRunID, "code": exc.Code, "subject_key": exc.SubjectKey, "severity": exc.Severity,
			"amount": exc.Amount, "state": exc.State, "resolution": exc.Resolution, "source": "WFP-RECON",
		}
		body, err := BuildWFPEnvelope(typ, correlationID, causationID, actor, exc.TenantID, exc.LegalEntityID, "", payload)
		if err != nil {
			return nil, err
		}
		return []domain.OutboxEvent{{TenantID: exc.TenantID, ObjectID: exc.ResultID, EventType: typ, Payload: body}}, nil

	case "BANKING_OBSERVED":
		if obs == nil || ins == nil {
			return nil, fmt.Errorf("banking observation event needs the observation and its instruction")
		}
		payload := map[string]any{
			"instruction_id": ins.InstructionID, "result_id": ins.ResultID, "run_ref_id": ins.RunRefID, "legal_entity_id": ins.LegalEntityID,
			"instruction_type": ins.InstructionType, "source": obs.Source, "raw_status": obs.RawStatus, "outcome": obs.Outcome,
		}
		body, err := BuildWFPEnvelope(domain.EventPayrollBankingOutcomeObserved, correlationID, causationID, actor, ins.TenantID, ins.LegalEntityID, "", payload)
		if err != nil {
			return nil, err
		}
		return []domain.OutboxEvent{{TenantID: ins.TenantID, ObjectID: ins.ResultID, EventType: domain.EventPayrollBankingOutcomeObserved, Payload: body}}, nil

	case "INSTRUCTION_RETURNED":
		if ins == nil {
			return nil, fmt.Errorf("returned event needs the instruction")
		}
		payload := map[string]any{
			"instruction_id": ins.InstructionID, "result_id": ins.ResultID, "run_ref_id": ins.RunRefID, "legal_entity_id": ins.LegalEntityID,
			"instruction_type": ins.InstructionType, "liability_id": ins.LiabilityID, "worker_id": ins.WorkerID, "state": ins.State,
		}
		body, err := BuildWFPEnvelope(domain.EventPayrollPaymentInstrReturned, correlationID, causationID, actor, ins.TenantID, ins.LegalEntityID, "", payload)
		if err != nil {
			return nil, err
		}
		return []domain.OutboxEvent{{TenantID: ins.TenantID, ObjectID: ins.ResultID, EventType: domain.EventPayrollPaymentInstrReturned, Payload: body}}, nil
	}
	return nil, fmt.Errorf("unknown reconciliation change %q", kind)
}
