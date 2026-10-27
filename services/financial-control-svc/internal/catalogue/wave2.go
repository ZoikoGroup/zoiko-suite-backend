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
	// SysPaymentRun is payment-run-svc (AP-11), the owner of supplier payment instructions (FIN-CTRL-030).
	SysPaymentRun = "payment-run"
	// SysTreasury is treasury-svc: the bank account master (FIN-CTRL-044).
	SysTreasury = "treasury"
	// SysPaymentInitiation is payment-initiation-adapter-svc: the payments instructed to the provider (FIN-CTRL-004).
	SysPaymentInitiation = "payment-initiation-adapter"
)

// Wave2Options carries the entity-specific inputs a definition cannot guess.
type Wave2Options struct {
	// GL account codes (comma separated) of the control accounts to reconcile against.
	ARControlAccounts string `json:"ar_control_accounts"`
	APControlAccounts string `json:"ap_control_accounts"`
	CashAccounts      string `json:"cash_accounts"`
	// BankAccountsExpected are accounts that MUST deliver a statement every day (FIN-CTRL-032).
	BankAccountsExpected []string `json:"bank_accounts_expected"`
	// FXPairsExpected are currency pairs ("EUR/USD") that MUST have a rate on every business day (FIN-CTRL-026).
	// Left empty, only pairs that delivered at least one rate in the period are tested, so a dead feed would pass.
	FXPairsExpected []string `json:"fx_pairs_expected"`
	// FXRateType is the ledger rate type tested for daily completeness (FIN-CTRL-026); empty means SPOT. Which type
	// must exist every business day is the entity's decision (a CLOSING rate, for instance, is only needed at period end).
	FXRateType string `json:"fx_rate_type"`
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
	fxRateType := strings.ToUpper(strings.TrimSpace(o.FXRateType))
	if fxRateType == "" {
		fxRateType = "SPOT"
	}
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
			// Payments instructed to the provider (side A, signed negative: money OUT) against the bank's debits
			// (side B). The two are paired on the payment_reference sent to the provider, which the bank/provider is
			// expected to echo on the statement line; where it is not echoed, a payment surfaces as unmatched, never
			// as a pass. Receipts and attempts that never reached the provider are excluded by explicit, owned rules
			// whose count and value are recorded in the evidence. Unmatched items dated within 5 days of the period
			// end are reconciling items (in transit), not permanent mismatches.
			ControlCode: "FIN-CTRL-004", Name: "Payment provider settlement", Domain: "Treasury",
			ControlType: "TRANSACTION", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "STANDARD",
			Frequency: "DAILY",
			OwnerRole: "TREASURY_OWNER", ReviewerRole: "TREASURY_ANALYST", CertifierRole: "TREASURER",
			SourceSpec: pop(SysPaymentInitiation, "payment-attempts"), TargetSpec: pop(SysBanking, "bank-transactions"),
			InitialLogic: j(map[string]any{
				"kind": "MATCH", "skip_content_duplicates": true, "outstanding_days": 5,
				"exclusions": []map[string]any{
					{"side": "A", "attr": "status", "values": []string{"PREPARED", "REJECTED_BEFORE_SUBMISSION", "CANCELLED"},
						"reason":    "payment attempt that never reached the provider moved no money",
						"authority": "TREASURER"},
					{"side": "B", "attr": "direction", "values": []string{"CREDIT", "ZERO"},
						"reason":    "bank credits and zero lines are receipts, not payments",
						"authority": "TREASURER"},
					{"side": "B", "attr": "status", "values": []string{"SUPERSEDED"},
						"reason":    "canonical bank transaction superseded by a re-normalisation; its replacement is in the population",
						"authority": "TREASURER"},
				},
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
			// The aging limit is a controlled decision (§34), so the cut-off is not baked in: the run scope carries
			// {"unapplied_before": "YYYY-MM-DD"} = run date minus the approved limit.
			ControlCode: "FIN-CTRL-029", Name: "Unapplied cash aging", Domain: "AR / Treasury",
			ControlType: "MONITORING", Assertions: []string{"CLASSIFICATION", "ACCURACY"}, RiskTier: "STANDARD",
			Frequency: "DAILY", OwnerRole: "AR_ACCOUNTING_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT",
			SourceSpec: j(map[string]any{"system": SysAR, "population": "aged-unapplied-receipts", "no_period": true,
				"params": map[string]string{"unapplied_before": "${scope.unapplied_before}"}}),
			InitialLogic: j(map[string]any{"kind": "EXCEPTION_SCAN", "finding_category": "CLASSIFICATION",
				"finding_reason": "UNAPPLIED_CASH_AGED", "finding_assertion": "CLASSIFICATION"}),
		},
		{
			// Reads the LEDGER's own sourced FX rates (the ones revaluation and translation use), not treasury's analytics
			// rates. Run period_id (YYYY-MM) selects the month; each expected pair needs a rate of the chosen type on every
			// business day up to today. The rate's source is carried on each record, so a gap can be attributed to a feed.
			ControlCode: "FIN-CTRL-026", Name: "FX rate source completeness", Domain: "Reference / Accounting",
			ControlType: "INTERFACE", Assertions: []string{"ACCURACY", "VALUATION"}, RiskTier: "STANDARD",
			Frequency: "DAILY", OwnerRole: "GL_OWNER", ReviewerRole: "TREASURY_ANALYST",
			SourceSpec: j(map[string]any{"system": SysGL, "population": "fx-rate-observations",
				"params": map[string]string{"rate_type": fxRateType}}),
			InitialLogic: j(map[string]any{"kind": "DATE_COVERAGE", "group_attr": "pair",
				"skip_weekends": true, "expected_groups": nonNil(o.FXPairsExpected)}),
		},
		{
			// Same per-run cut-off as FIN-CTRL-029: {"unapplied_before": "YYYY-MM-DD"}.
			ControlCode: "FIN-CTRL-030", Name: "Supplier unapplied payment aging", Domain: "AP / Treasury",
			ControlType: "MONITORING", Assertions: []string{"CLASSIFICATION", "ACCURACY"}, RiskTier: "STANDARD",
			Frequency: "DAILY", OwnerRole: "AP_ACCOUNTING_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT",
			SourceSpec: j(map[string]any{"system": SysPaymentRun, "population": "unapplied-supplier-payments", "no_period": true,
				"params": map[string]string{"unapplied_before": "${scope.unapplied_before}"}}),
			InitialLogic: j(map[string]any{"kind": "EXCEPTION_SCAN", "finding_category": "CLASSIFICATION",
				"finding_reason": "SUPPLIER_PAYMENT_UNAPPLIED", "finding_assertion": "CLASSIFICATION"}),
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
