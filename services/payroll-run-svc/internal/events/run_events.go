package events

import (
	"encoding/json"
	"fmt"
	"time"

	"zoiko.io/payroll-run-svc/internal/domain"
)

// Event types of the legacy payroll run lifecycle.
const (
	RunInitiated  = domain.EventRunInitiated
	RunCalculated = domain.EventRunCalculated
	RunCompleted  = domain.EventRunCompleted
	RunBlocked    = domain.EventRunBlocked
)

// runPayload is the payload of a run lifecycle event. It is the single source of
// the field set, shared by the direct publisher and the outbox path.
func runPayload(eventType string, r domain.PayrollRun, reason string) map[string]any {
	switch eventType {
	case RunInitiated:
		return map[string]any{
			"run_id": r.RunID, "tenant_id": r.TenantID, "legal_entity_id": r.LegalEntityID, "run_number": r.RunNumber,
			"pay_period_start": r.PayPeriodStart, "pay_period_end": r.PayPeriodEnd, "pay_date": r.PayDate,
			"is_shadow_run": r.IsShadowRun, "status": r.Status, "initiated_at": r.CreatedAt,
		}
	case RunCalculated:
		return map[string]any{
			"run_id": r.RunID, "tenant_id": r.TenantID, "legal_entity_id": r.LegalEntityID, "run_number": r.RunNumber,
			"total_gross_pay": r.TotalGrossPay, "total_net_pay": r.TotalNetPay, "total_tax_deductions": r.TotalTaxDeductions,
			"employee_count": r.EmployeeCount, "is_shadow_run": r.IsShadowRun, "calculated_at": r.UpdatedAt,
		}
	case RunCompleted:
		return map[string]any{
			"run_id": r.RunID, "tenant_id": r.TenantID, "legal_entity_id": r.LegalEntityID, "run_number": r.RunNumber,
			"total_gross_pay": r.TotalGrossPay, "total_net_pay": r.TotalNetPay, "total_tax_deductions": r.TotalTaxDeductions,
			"employee_count": r.EmployeeCount, "is_shadow_run": r.IsShadowRun, "finalized_at": r.FinalizedAt,
		}
	case RunBlocked:
		return map[string]any{
			"run_id": r.RunID, "tenant_id": r.TenantID, "legal_entity_id": r.LegalEntityID, "run_number": r.RunNumber,
			"block_reason": reason, "blocked_at": time.Now().UTC(),
		}
	}
	return nil
}

// BuildRunEnvelope renders a run lifecycle event in the platform envelope. It is
// called at WRITE time, inside the transaction that changes the run, and the
// outbox stores the result verbatim.
func BuildRunEnvelope(eventType, correlationID, actorID string, r domain.PayrollRun, reason string) ([]byte, error) {
	payload := runPayload(eventType, r, reason)
	if payload == nil {
		return nil, fmt.Errorf("unknown run event type %q", eventType)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	return json.Marshal(envelope{
		EventType: eventType, EventVersion: "1.0", EmittedAt: time.Now().UTC(), SchemaVersion: "1.0",
		SourceService: "payroll-run-svc", CorrelationID: correlationID, TenantID: r.TenantID,
		LegalEntityID: r.LegalEntityID, ActorID: actorID, Payload: raw,
	})
}
