package catalogue

import (
	"encoding/json"
	"fmt"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Wave7 returns the payroll-to-accounting and payroll-to-payment controls that became
// possible once the Workforce & Payroll Accounting Interface (ZS-SVC-L-001) exists in
// payroll-run-svc:
//
//	FIN-CTRL-005  Payroll run to GL        accepted payroll result gross  vs  its POSTED journal's gross expense
//	PAY-CTRL-004  Payroll net to payable   accepted payroll result net    vs  its POSTED journal's net-pay payable
//	FIN-CTRL-006  Payroll net-pay payment  accepted payroll result net    vs  its live net-pay payment instructions
//
// All three compare ONE record per accepted result on each side, paired by the result id:
// an accepted result with no posted journal (or no payment instruction) is a missing
// downstream record, and an amount that differs by even 0.0001 is a mismatch. Nothing is
// netted across results. Populations are served by payroll-run-svc as
// GET /v1/control-populations/{wfp-results|wfp-journals|wfp-net-instructions}.
//
// What they do not cover is stated in Status(): line-level and per-worker comparison,
// liabilities, and Banking settlement are reconciled inside payroll-run-svc (WFP
// reconciliation runs), not here.
func Wave7(effectiveFrom string) ([]domain.CreateControlDefinitionRequest, error) {
	if effectiveFrom == "" {
		return nil, fmt.Errorf("%w: catalogue needs effective_from", domain.ErrInvalidArgument)
	}
	evidence := j(map[string]any{"retention_class": "FINANCIAL_CONTROL", "retention_years": 7})
	pop := func(name, measure string) json.RawMessage {
		return j(map[string]any{"system": SysPayroll, "population": name, "params": map[string]string{"measure": measure}})
	}
	exact := j(map[string]any{"kind": "MATCH"})

	defs := []domain.CreateControlDefinitionRequest{
		{
			ControlCode: "FIN-CTRL-005", Name: "Payroll run to GL", Domain: "Payroll / Accounting",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "PERIOD_END", CloseGating: true,
			OwnerRole: "PAYROLL_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: pop("wfp-results", "gross"), TargetSpec: pop("wfp-journals", "gross"), InitialLogic: exact,
		},
		{
			ControlCode: "PAY-CTRL-004", Name: "Payroll result net pay to posted net-pay payable", Domain: "Payroll / Accounting",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "PERIOD_END", CloseGating: true,
			OwnerRole: "PAYROLL_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: pop("wfp-results", "net"), TargetSpec: pop("wfp-journals", "net"), InitialLogic: exact,
		},
		{
			ControlCode: "FIN-CTRL-006", Name: "Payroll net-pay to payment", Domain: "Payroll / Treasury",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "PERIOD_END", CloseGating: true,
			OwnerRole: "PAYROLL_OWNER", ReviewerRole: "TREASURY_ANALYST", CertifierRole: "TREASURER",
			SourceSpec: pop("wfp-results", "net"), TargetSpec: pop("wfp-net-instructions", "net"), InitialLogic: exact,
		},
	}
	for i := range defs {
		d := &defs[i]
		d.EvidencePolicy = evidence
		d.InitialEffectiveFrom = effectiveFrom
		d.InitialTestPack = "wave7/1"
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("catalogue %s: %w", d.ControlCode, err)
		}
	}
	return defs, nil
}
