package catalogue

import (
	"encoding/json"
	"fmt"
	"strings"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Registered source system names added in Wave 3.
const (
	SysInventory = "inventory-management"
	SysPayroll   = "payroll-run"
)

// SysAssetManagement is asset-management-svc, the owner of the fixed-asset register and its depreciation runs.
const SysAssetManagement = "asset-management"

// Wave3Options carries the optional, entity-specific inputs of Wave 3. A control whose input is empty is not created.
type Wave3Options struct {
	// AccumulatedDepreciationAccounts are the ledger accounts (comma separated, credit-normal) the kernel posts
	// accumulated depreciation to; naming them creates FIN-CTRL-011. They are the tenant's depreciation mappings in
	// the ledger, which this service cannot discover.
	AccumulatedDepreciationAccounts string `json:"accumulated_depreciation_accounts"`
}

// Wave3 returns the controls of implementation wave 3 that can run on data the
// platform holds today: the physical-count control and the payroll integrity
// controls. The rest of wave 3 is BLOCKED by missing capabilities in other
// services — see Status() for exactly which.
//
// PAY-CTRL-* are extensions that implement §15's "Gross-to-net" and "Run-to-…"
// tests from payroll's own data. They do not replace FIN-CTRL-005/006, which
// need a payroll-to-GL and a payroll-to-payment path that do not exist yet.
func Wave3(effectiveFrom string, opts ...Wave3Options) ([]domain.CreateControlDefinitionRequest, error) {
	var o Wave3Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if effectiveFrom == "" {
		return nil, fmt.Errorf("%w: catalogue needs effective_from", domain.ErrInvalidArgument)
	}
	evidence := j(map[string]any{"retention_class": "FINANCIAL_CONTROL", "retention_years": 7})
	pop := func(sys, name string, params map[string]string) json.RawMessage {
		m := map[string]any{"system": sys, "population": name}
		if len(params) > 0 {
			m["params"] = params
		}
		return j(m)
	}
	// noPeriod: this source rejects period_id; the population is scoped by its own params.
	noPeriod := func(sys, name string, params map[string]string) json.RawMessage {
		return j(map[string]any{"system": sys, "population": name, "params": params, "no_period": true})
	}

	// Payroll runs that are not final, and shadow (parallel-run) payrolls, are not the
	// payable population. Excluding them is explicit, owned, and evidenced (§9).
	notFinal := func(side, attr string) map[string]any {
		return map[string]any{"side": side, "attr": attr, "values": []string{"INITIATED", "CALCULATED", "BLOCKED"},
			"reason":    "payroll run not finalised; integrity is asserted on completed runs",
			"authority": "PAYROLL_CONTROLLER"}
	}
	shadow := map[string]any{"side": "", "attr": "is_shadow_run", "values": []string{"true"},
		"reason": "shadow (parallel-run) payroll is not a payable population", "authority": "PAYROLL_CONTROLLER"}

	runVsSlips := func(measure string) json.RawMessage {
		return j(map[string]any{"kind": "MATCH", "allow_groups": true, "skip_content_duplicates": true,
			"exclusions": []map[string]any{notFinal("A", "status"), notFinal("B", "run_status"), shadow}})
	}

	defs := []domain.CreateControlDefinitionRequest{
		{
			ControlCode: "FIN-CTRL-013", Name: "Physical count to inventory", Domain: "Inventory",
			ControlType: "BALANCE", Assertions: []string{"EXISTENCE"}, RiskTier: "STANDARD",
			Frequency: "ON_DEMAND", OwnerRole: "INVENTORY_OWNER",
			// Book quantity as of the count cut-off is derived from committed movements, not from
			// the frozen system_quantity, so tampering with the frozen figure is detectable. A line
			// not yet counted is absent from side B and surfaces as a missing counted line.
			// The count is bound per run: create the run with scope {"count_id": "<uuid>"}.
			SourceSpec:   noPeriod(SysInventory, "stock-count-lines", map[string]string{"count_id": "${scope.count_id}", "side": "book"}),
			TargetSpec:   noPeriod(SysInventory, "stock-count-lines", map[string]string{"count_id": "${scope.count_id}", "side": "physical"}),
			InitialLogic: j(map[string]any{"kind": "MATCH"}),
		},
		{
			ControlCode: "PAY-CTRL-001", Name: "Payroll gross-to-net integrity (per pay slip)", Domain: "Payroll",
			ControlType: "TRANSACTION", Assertions: []string{"ACCURACY"}, RiskTier: "STANDARD",
			Frequency: "EVENT_DRIVEN", OwnerRole: "PAYROLL_OWNER",
			SourceSpec: pop(SysPayroll, "pay-slips", map[string]string{"measure": "net"}),
			InitialLogic: j(map[string]any{
				"kind": "ARITHMETIC",
				"equations": []map[string]any{{"name": "gross_to_net", "plus": []string{"attr:gross_pay"},
					"minus": []string{"attr:tax_withheld", "attr:benefits_deductions"}, "equals": "attr:net_pay"}},
				"exclusions": []map[string]any{notFinal("", "run_status"), shadow},
			}),
		},
		{
			ControlCode: "PAY-CTRL-002", Name: "Payroll run net total to pay slips", Domain: "Payroll",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "STANDARD",
			Frequency: "EVENT_DRIVEN", OwnerRole: "PAYROLL_OWNER",
			SourceSpec:   pop(SysPayroll, "payroll-runs", map[string]string{"measure": "net"}),
			TargetSpec:   pop(SysPayroll, "pay-slips", map[string]string{"measure": "net"}),
			InitialLogic: runVsSlips("net"),
		},
		{
			ControlCode: "PAY-CTRL-003", Name: "Payroll run gross total to pay slips", Domain: "Payroll",
			ControlType: "BALANCE", Assertions: []string{"COMPLETENESS", "ACCURACY"}, RiskTier: "STANDARD",
			Frequency: "EVENT_DRIVEN", OwnerRole: "PAYROLL_OWNER",
			SourceSpec:   pop(SysPayroll, "payroll-runs", map[string]string{"measure": "gross"}),
			TargetSpec:   pop(SysPayroll, "pay-slips", map[string]string{"measure": "gross"}),
			InitialLogic: runVsSlips("gross"),
		},
	}

	if strings.TrimSpace(o.AccumulatedDepreciationAccounts) != "" {
		// One record per (live run, asset category) on the register side, paired on the kernel's own reference
		// ('ast-dep:<run>:<category>', the source event id of the posting) with the credits on the accumulated
		// depreciation accounts. Both sides carry the period end as their date, so no date tolerance is needed.
		defs = append(defs, domain.CreateControlDefinitionRequest{
			ControlCode: "FIN-CTRL-011", Name: "Depreciation run to GL", Domain: "Assets",
			ControlType: "TRANSACTION", Assertions: []string{"ACCURACY", "VALUATION"}, RiskTier: "STANDARD",
			Frequency: "PERIOD_END", OwnerRole: "ASSET_ACCOUNTING_OWNER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
			SourceSpec: pop(SysAssetManagement, "depreciation-run-postings", nil),
			TargetSpec: j(map[string]any{"system": SysGL, "population": "account-postings",
				"params": map[string]string{"account_codes": strings.TrimSpace(o.AccumulatedDepreciationAccounts), "normal_balance": "CREDIT"}}),
			InitialLogic: j(map[string]any{"kind": "MATCH", "allow_groups": true, "skip_content_duplicates": true}),
		})
	}
	for i := range defs {
		d := &defs[i]
		d.EvidencePolicy = evidence
		d.InitialEffectiveFrom = effectiveFrom
		d.InitialTestPack = "wave3/1"
		if d.TargetSpec == nil {
			d.TargetSpec = json.RawMessage(`{}`)
		}
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("catalogue %s: %w", d.ControlCode, err)
		}
	}
	return defs, nil
}
