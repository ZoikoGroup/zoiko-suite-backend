package catalogue

import (
	"encoding/json"
	"fmt"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Wave5 returns the accounting-kernel controls that can run on data the ledger
// holds today: journal balance, direct posting to restricted control accounts,
// unposted accounting events and the manual-journal review.
//
// All four test the general ledger itself, so the source is general-ledger-svc and
// there is no second population. Runs bind their scope:
//
//	020/021/023  scope {"fiscal_period": "<ledger fiscal period text>"}
//	022          scope {"created_before": "<RFC3339 or YYYY-MM-DD cut-off>"}
//
// 019 (event to journal), 024 (closed-period block) and 031 (suspense aging) stay
// BLOCKED — see Status(). 041/042 are computed by the control service itself.
func Wave5(effectiveFrom string) ([]domain.CreateControlDefinitionRequest, error) {
	if effectiveFrom == "" {
		return nil, fmt.Errorf("%w: catalogue needs effective_from", domain.ErrInvalidArgument)
	}
	evidence := j(map[string]any{"retention_class": "FINANCIAL_CONTROL", "retention_years": 7})
	glPop := func(name string, params map[string]string) json.RawMessage {
		return j(map[string]any{"system": SysGL, "population": name, "params": params, "no_period": true})
	}
	fp := map[string]string{"fiscal_period": "${scope.fiscal_period}"}

	defs := []domain.CreateControlDefinitionRequest{
		{
			ControlCode: "FIN-CTRL-019", Name: "Accounting event to journal", Domain: "Accounting Kernel",
			ControlType: "INTERFACE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "STANDARD",
			Frequency: "DAILY", OwnerRole: "GL_OWNER",
			SourceSpec: glPop("event-journal-breaks", map[string]string{"created_before": "${scope.created_before}"}),
			InitialLogic: j(map[string]any{"kind": "EXCEPTION_SCAN", "finding_category": "MISSING",
				"finding_reason": "EVENT_JOURNAL_BREAK", "finding_assertion": "COMPLETENESS"}),
		},
		{
			ControlCode: "FIN-CTRL-020", Name: "Journal debit-credit balance", Domain: "Accounting Kernel",
			ControlType: "TRANSACTION", Assertions: []string{"ACCURACY"}, RiskTier: "KEY",
			Frequency: "CONTINUOUS", CloseGating: true, OwnerRole: "GL_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: glPop("journal-balances", fp),
			InitialLogic: j(map[string]any{
				"kind": "ARITHMETIC",
				"equations": []map[string]any{{"name": "debits_equal_credits",
					"plus": []string{"attr:debit_total"}, "equals": "attr:credit_total"}},
			}),
		},
		{
			ControlCode: "FIN-CTRL-021", Name: "Control-account direct posting", Domain: "Accounting Kernel",
			ControlType: "PREVENTIVE", Assertions: []string{"CLASSIFICATION", "AUTHORIZATION"}, RiskTier: "KEY",
			Frequency: "CONTINUOUS", CloseGating: true, OwnerRole: "GL_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: glPop("control-account-postings", fp),
			InitialLogic: j(map[string]any{"kind": "EXCEPTION_SCAN", "finding_category": "CLASSIFICATION",
				"finding_reason": "DIRECT_CONTROL_ACCOUNT_POSTING", "finding_assertion": "CLASSIFICATION"}),
		},
		{
			ControlCode: "FIN-CTRL-022", Name: "Unposted accounting events", Domain: "Accounting Kernel",
			ControlType: "MONITORING", Assertions: []string{"COMPLETENESS"}, RiskTier: "STANDARD",
			Frequency: "DAILY", OwnerRole: "GL_OWNER",
			SourceSpec: glPop("unposted-events", map[string]string{"created_before": "${scope.created_before}"}),
			InitialLogic: j(map[string]any{"kind": "EXCEPTION_SCAN", "finding_category": "LATE_DATA",
				"finding_reason": "UNPOSTED_ACCOUNTING_EVENT", "finding_assertion": "COMPLETENESS"}),
		},
		{
			ControlCode: "FIN-CTRL-023", Name: "Manual journal independent review", Domain: "Accounting / Close",
			ControlType: "REVIEW", Assertions: []string{"AUTHORIZATION", "OCCURRENCE"}, RiskTier: "KEY",
			Frequency: "PERIOD_END", CloseGating: true, OwnerRole: "GL_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: glPop("manual-journals", fp),
			InitialLogic: j(map[string]any{"kind": "EXCEPTION_SCAN", "finding_category": "AUTHORIZATION",
				"finding_reason": "MANUAL_JOURNAL_REVIEW_GAP", "finding_assertion": "AUTHORIZATION",
				"conditions": []map[string]any{{"attr": "review_gap", "op": "nonempty",
					"detail": "manual journal has no independent approver (missing or self-approved)"}}}),
		},
	}
	for i := range defs {
		d := &defs[i]
		d.EvidencePolicy = evidence
		d.InitialEffectiveFrom = effectiveFrom
		d.InitialTestPack = "wave5/1"
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("catalogue %s: %w", d.ControlCode, err)
		}
	}
	return defs, nil
}
