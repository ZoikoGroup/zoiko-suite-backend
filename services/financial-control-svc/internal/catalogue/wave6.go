package catalogue

import (
	"encoding/json"
	"fmt"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Registered source system names added in Wave 6.
const (
	SysFinancialClose = "financial-close"
	SysTaxAuthority   = "tax-authority-interface"
	SysPayeeBanking   = "payee-banking-identity"
)

// Wave6 returns the migration, filing-acknowledgement and bank-detail controls that
// can run on data the platform holds today. Five controls of the wave (033-036, 044)
// and 039 remain BLOCKED — see Status() for the missing capability of each.
//
// Run scopes:
//
//	037  {"submitted_before": "<RFC3339 or YYYY-MM-DD>"}
//	038  {"batch_id": "<migration batch uuid>"}
//	045  {"changed_from": "YYYY-MM-DD", "changed_to": "YYYY-MM-DD"}
func Wave6(effectiveFrom string) ([]domain.CreateControlDefinitionRequest, error) {
	if effectiveFrom == "" {
		return nil, fmt.Errorf("%w: catalogue needs effective_from", domain.ErrInvalidArgument)
	}
	evidence := j(map[string]any{"retention_class": "FINANCIAL_CONTROL", "retention_years": 7})
	pop := func(sys, name string, params map[string]string) json.RawMessage {
		return j(map[string]any{"system": sys, "population": name, "params": params, "no_period": true})
	}

	defs := []domain.CreateControlDefinitionRequest{
		{
			ControlCode: "FIN-CTRL-037", Name: "Tax filing acknowledgement", Domain: "Tax",
			ControlType: "EXTERNAL", Assertions: []string{"COMPLETENESS", "OCCURRENCE"}, RiskTier: "KEY",
			Frequency: "DAILY", OwnerRole: "TAX_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: pop(SysTaxAuthority, "unacknowledged-filings", map[string]string{"submitted_before": "${scope.submitted_before}"}),
			InitialLogic: j(map[string]any{"kind": "EXCEPTION_SCAN", "finding_category": "EXTERNAL_STATUS",
				"finding_reason": "FILING_NOT_ACKNOWLEDGED", "finding_assertion": "COMPLETENESS"}),
		},
		{
			ControlCode: "FIN-CTRL-038", Name: "Opening balance migration tie-out", Domain: "Migration",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "ON_DEMAND", OwnerRole: "MIGRATION_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: pop(SysFinancialClose, "migration-batch-tieout", map[string]string{"batch_id": "${scope.batch_id}"}),
			InitialLogic: j(map[string]any{
				"kind": "ARITHMETIC",
				"equations": []map[string]any{
					{"name": "debits_tie_to_declared", "plus": []string{"attr:crosswalk_debits"}, "equals": "attr:expected_total_debits"},
					{"name": "credits_tie_to_declared", "plus": []string{"attr:crosswalk_credits"}, "equals": "attr:expected_total_credits"},
					{"name": "rows_tie_to_declared", "plus": []string{"attr:crosswalk_row_count"}, "equals": "attr:expected_row_count"},
					{"name": "opening_balance_balanced", "plus": []string{"attr:crosswalk_debits"}, "equals": "attr:crosswalk_credits"},
				},
			}),
		},
		{
			ControlCode: "FIN-CTRL-045", Name: "Supplier bank detail change segregation of duties", Domain: "Payments",
			ControlType: "REVIEW", Assertions: []string{"AUTHORIZATION"}, RiskTier: "KEY",
			Frequency: "DAILY", OwnerRole: "TREASURY_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: pop(SysPayeeBanking, "destination-changes",
				map[string]string{"changed_from": "${scope.changed_from}", "changed_to": "${scope.changed_to}"}),
			InitialLogic: j(map[string]any{"kind": "EXCEPTION_SCAN", "finding_category": "AUTHORIZATION",
				"finding_reason": "BANK_DETAIL_CHANGE_SOD_GAP", "finding_assertion": "AUTHORIZATION",
				"conditions": []map[string]any{{"attr": "sod_gap", "op": "nonempty",
					"detail": "bank detail change lacks independent proposer/verifier/approver"}}}),
		},
	}
	for i := range defs {
		d := &defs[i]
		d.EvidencePolicy = evidence
		d.InitialEffectiveFrom = effectiveFrom
		d.InitialTestPack = "wave6/1"
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("catalogue %s: %w", d.ControlCode, err)
		}
	}
	return defs, nil
}
