// Package catalogue holds the ZS-CONTROL-001 §29 enterprise control catalogue as
// executable definitions.
//
// Owner, reviewer and certifier roles, tolerances, materiality and the
// key-control classification are CONTROLLED DECISIONS (§34) that belong to the
// entity's Finance leadership. The values here are defaults so the controls can
// be created and exercised; every definition is created with an UNAPPROVED rule
// version and needs independent approval before any run can pin it.
package catalogue

import (
	"encoding/json"
	"fmt"
	"strings"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Registered source system names (see SOURCE_ENDPOINTS).
const (
	SysAR      = "accounts-receivable"
	SysAP      = "accounts-payable"
	SysGL      = "general-ledger"
	SysBanking = "banking-connector"
)

// Wave2Options carries the entity-specific inputs a definition cannot guess.
type Wave2Options struct {
	// GL account codes (comma separated) of the control accounts to reconcile against.
	ARControlAccounts string `json:"ar_control_accounts"`
	APControlAccounts string `json:"ap_control_accounts"`
	CashAccounts      string `json:"cash_accounts"`
	// BankAccountsExpected are accounts that MUST deliver a statement every day (FIN-CTRL-032).
	BankAccountsExpected []string `json:"bank_accounts_expected"`
	// EffectiveFrom is the date the first rule version takes effect (YYYY-MM-DD).
	EffectiveFrom string `json:"effective_from"`
}

// Wave2 returns the AR, AP and bank/cash controls of implementation wave 2.
// Every returned request has passed domain validation.
func Wave2(o Wave2Options) ([]domain.CreateControlDefinitionRequest, error) {
	var missing []string
	if strings.TrimSpace(o.ARControlAccounts) == "" {
		missing = append(missing, "ar_control_accounts")
	}
	if strings.TrimSpace(o.APControlAccounts) == "" {
		missing = append(missing, "ap_control_accounts")
	}
	if strings.TrimSpace(o.CashAccounts) == "" {
		missing = append(missing, "cash_accounts")
	}
	if o.EffectiveFrom == "" {
		missing = append(missing, "effective_from")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: catalogue needs %s", domain.ErrInvalidArgument, strings.Join(missing, ", "))
	}

	evidence := j(map[string]any{"retention_class": "FINANCIAL_CONTROL", "retention_years": 7})
	gl := func(codes, normal string) json.RawMessage {
		return j(map[string]any{"system": SysGL, "population": "account-postings",
			"params": map[string]string{"account_codes": codes, "normal_balance": normal}})
	}
	pop := func(sys, name string) json.RawMessage { return j(map[string]string{"system": sys, "population": name}) }

	// group controls: several ledger lines legitimately share one document reference.
	groupLogic := j(map[string]any{"kind": "MATCH", "allow_groups": true, "skip_content_duplicates": true})

	defs := []domain.CreateControlDefinitionRequest{
		{
			ControlCode: "FIN-CTRL-001", Name: "AR subledger to GL", Domain: "Accounting / AR",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "PERIOD_END", CloseGating: true,
			OwnerRole: "AR_ACCOUNTING_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: pop(SysAR, "open-invoices"), TargetSpec: gl(o.ARControlAccounts, "DEBIT"),
			InitialLogic: groupLogic,
		},
		{
			ControlCode: "FIN-CTRL-002", Name: "AP subledger to GL", Domain: "Accounting / AP",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "PERIOD_END", CloseGating: true,
			OwnerRole: "AP_ACCOUNTING_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: pop(SysAP, "open-invoices"), TargetSpec: gl(o.APControlAccounts, "CREDIT"),
			InitialLogic: groupLogic,
		},
		{
			ControlCode: "FIN-CTRL-003", Name: "Bank statement to cash GL", Domain: "Treasury",
			ControlType: "BALANCE", Assertions: []string{"EXISTENCE", "COMPLETENESS", "ACCURACY"}, RiskTier: "KEY",
			Frequency: "DAILY", CloseGating: true,
			OwnerRole: "TREASURY_OWNER", ReviewerRole: "TREASURY_ANALYST", CertifierRole: "TREASURER",
			SourceSpec: pop(SysBanking, "bank-transactions"), TargetSpec: gl(o.CashAccounts, "DEBIT"),
			InitialLogic: j(map[string]any{
				"kind": "MATCH", "allow_groups": true, "skip_content_duplicates": true,
				// Deposits/payments in transit: unmatched items in the last 5 days are reconciling
				// items with an expected clearing date; older ones are real omissions.
				"outstanding_days": 5,
				// Re-normalised statement lines are superseded, not additional cash. The exclusion is
				// explicit, owned, and its count/value impact is recorded in the evidence (§9).
				"exclusions": []map[string]any{{
					"side": "A", "attr": "status", "values": []string{"SUPERSEDED"},
					"reason":    "canonical bank transaction superseded by a re-normalisation; its replacement is in the population",
					"authority": "TREASURER",
				}},
			}),
		},
		{
			ControlCode: "FIN-CTRL-027", Name: "Customer invoice sequence / issue completeness", Domain: "AR",
			ControlType: "TRANSACTION", Assertions: []string{"COMPLETENESS"}, RiskTier: "STANDARD",
			Frequency: "DAILY", OwnerRole: "AR_ACCOUNTING_OWNER",
			SourceSpec:   pop(SysAR, "open-invoices"),
			InitialLogic: j(map[string]any{"kind": "SEQUENCE_GAP"}),
		},
		{
			ControlCode: "FIN-CTRL-028", Name: "Supplier invoice duplicate control", Domain: "AP",
			ControlType: "MONITORING", Assertions: []string{"OCCURRENCE", "ACCURACY"}, RiskTier: "STANDARD",
			Frequency: "CONTINUOUS", OwnerRole: "AP_ACCOUNTING_OWNER",
			SourceSpec: pop(SysAP, "open-invoices"),
			InitialLogic: j(map[string]any{"kind": "DUPLICATE_SCAN",
				"key_fields": []string{"attr:vendor_id", "amount", "currency", "date"}}),
		},
		{
			ControlCode: "FIN-CTRL-032", Name: "Bank feed completeness", Domain: "Treasury / Integration",
			ControlType: "INTERFACE", Assertions: []string{"COMPLETENESS"}, RiskTier: "STANDARD",
			Frequency: "DAILY", OwnerRole: "TREASURY_OWNER",
			SourceSpec: pop(SysBanking, "bank-statements"),
			InitialLogic: j(map[string]any{"kind": "DATE_COVERAGE", "group_attr": "bank_account_id",
				"skip_weekends": true, "expected_groups": nonNil(o.BankAccountsExpected)}),
		},
	}

	for i := range defs {
		d := &defs[i]
		d.EvidencePolicy = evidence
		d.InitialEffectiveFrom = o.EffectiveFrom
		d.InitialTestPack = "wave2/1"
		if d.TargetSpec == nil {
			d.TargetSpec = json.RawMessage(`{}`)
		}
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("catalogue %s: %w", d.ControlCode, err)
		}
	}
	return defs, nil
}

func j(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // static literals only
	}
	return b
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
