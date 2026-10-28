package kernel

// The subledger packs (standard §18–§22). The authoritative engines — payroll, the asset register, the inventory quantity ledger,
// the tax determination service, the schedule owners — compute the numbers. These rules only turn an approved result into
// balanced, evidenced journal lines: none of them recomputes payroll tax, depreciation, a cost layer or a tax rate. Every rule
// refuses an event that lacks the evidence its domain requires, and one whose totals do not add up is not posted.

func goldenD(name, event, cp string, dims map[string]string, facts map[string]string, want ...ExpectedLine) TestCase {
	tc := golden(name, event, cp, facts, want...)
	tc.Event.Dimensions = dims
	return tc
}

func refusedD(name, event, cp string, dims map[string]string, facts map[string]string, family string) TestCase {
	tc := refused(name, event, cp, facts, family)
	tc.Event.Dimensions = dims
	return tc
}

func required(dims ...string) []DimensionRule {
	out := make([]DimensionRule, 0, len(dims))
	for _, d := range dims {
		out = append(out, DimensionRule{Dimension: d, Mode: DimRequired})
	}
	return out
}

func withDims(c RuleContent, dims ...string) RuleContent {
	c.DimensionRules = required(dims...)
	return c
}

func exists(facts ...string) []Condition {
	out := make([]Condition, 0, len(facts))
	for _, f := range facts {
		out = append(out, Condition{Fact: f, Op: "exists"})
	}
	return out
}

func lineDims(ref, side, key, amount string, dims map[string]string, when ...Condition) LineTemplate {
	l := line(ref, side, key, amount, when...)
	l.Dimensions = dims
	return l
}

func subledgerPacks() []RulePack {
	cc := map[string]string{"cost_center": "CC-100"}
	ac := map[string]string{"asset_class": "VEHICLES"}
	inv := map[string]string{"location": "WH-1", "item_class": "FINISHED"}
	return []RulePack{
		{
			Name:        "payroll-standard",
			Description: "Payroll: an approved payroll run, the net-pay payment and the remittance of the statutory and benefit liabilities. The payroll engine computes; the kernel posts its approved result",
			Rules: []PackRule{
				{Name: "payroll-run-approved", Domain: "PAYROLL", EventType: "PAYROLL.RUN_APPROVED",
					Content: withDims(content(nil, exists("payroll_run_ref", "approval_ref", "payroll_rule_version"),
						line("L1", Debit, "PAYROLL_EXPENSE", "gross_pay"),
						line("L2", Debit, "EMPLOYER_CONTRIB_EXPENSE", "employer_contributions", gt0("employer_contributions")),
						line("L3", Credit, "PAYROLL_TAX_LIABILITY", "tax_withheld", gt0("tax_withheld")),
						line("L4", Credit, "EMPLOYEE_DEDUCTIONS_LIABILITY", "other_deductions", gt0("other_deductions")),
						line("L5", Credit, "EMPLOYER_CONTRIB_LIABILITY", "employer_contributions", gt0("employer_contributions")),
						line("L6", Credit, "NET_PAY_PAYABLE", "net_pay")), "cost_center"),
					TestPack: []TestCase{
						goldenD("a run with deductions and employer contributions", "PAYROLL.RUN_APPROVED", "", cc,
							f{"gross_pay": "10000.00", "tax_withheld": "1800.00", "other_deductions": "700.00", "employer_contributions": "900.00", "net_pay": "7500.00", "payroll_run_ref": "PR-2026-03", "approval_ref": "APPR-1", "payroll_rule_version": "uk-2026.2"},
							dr("6100", "10000.00"), dr("6110", "900.00"), cr("2310", "1800.00"), cr("2320", "700.00"), cr("2330", "900.00"), cr("2340", "7500.00")),
						goldenD("a run with no employer contributions", "PAYROLL.RUN_APPROVED", "", cc,
							f{"gross_pay": "1000.00", "tax_withheld": "200.00", "other_deductions": "0.00", "employer_contributions": "0.00", "net_pay": "800.00", "payroll_run_ref": "PR-2", "approval_ref": "APPR-2", "payroll_rule_version": "v1"},
							dr("6100", "1000.00"), cr("2310", "200.00"), cr("2340", "800.00")),
						refusedD("a run that is not approved is not posted", "PAYROLL.RUN_APPROVED", "", cc,
							f{"gross_pay": "1000.00", "tax_withheld": "200.00", "other_deductions": "0.00", "employer_contributions": "0.00", "net_pay": "800.00", "payroll_run_ref": "PR-3", "payroll_rule_version": "v1"}, FamRule),
						refusedD("gross that does not equal deductions plus net pay is the payroll engine's error, not the ledger's to fix", "PAYROLL.RUN_APPROVED", "", cc,
							f{"gross_pay": "1000.00", "tax_withheld": "200.00", "other_deductions": "0.00", "employer_contributions": "0.00", "net_pay": "799.00", "payroll_run_ref": "PR-4", "approval_ref": "A", "payroll_rule_version": "v1"}, FamBal),
						refusedD("the cost centre is required", "PAYROLL.RUN_APPROVED", "", nil,
							f{"gross_pay": "1000.00", "tax_withheld": "200.00", "other_deductions": "0.00", "employer_contributions": "0.00", "net_pay": "800.00", "payroll_run_ref": "PR-5", "approval_ref": "A", "payroll_rule_version": "v1"}, FamDim),
					}},
				{Name: "payroll-payment-settled", Domain: "PAYROLL", EventType: "PAYROLL.PAYMENT_SETTLED",
					Content: content(nil, exists("payment_ref", "payroll_run_ref"), line("L1", Debit, "NET_PAY_PAYABLE", "amount"), line("L2", Credit, "BANK", "amount")),
					TestPack: []TestCase{
						golden("net pay settled against the payable", "PAYROLL.PAYMENT_SETTLED", "", f{"amount": "7500.00", "payment_ref": "PAY-1", "payroll_run_ref": "PR-2026-03"}, dr("2340", "7500.00"), cr("1000", "7500.00")),
						refused("a payment names its payroll run", "PAYROLL.PAYMENT_SETTLED", "", f{"amount": "7500.00", "payment_ref": "PAY-1"}, FamRule),
					}},
				{Name: "payroll-liability-remitted", Domain: "PAYROLL", EventType: "PAYROLL.LIABILITY_REMITTED",
					Content: content(nil, exists("remittance_ref"),
						line("L1", Debit, "PAYROLL_TAX_LIABILITY", "tax_amount", gt0("tax_amount")),
						line("L2", Debit, "EMPLOYEE_DEDUCTIONS_LIABILITY", "other_amount", gt0("other_amount")),
						line("L3", Debit, "EMPLOYER_CONTRIB_LIABILITY", "employer_amount", gt0("employer_amount")),
						line("L4", Credit, "BANK", "tax_amount + other_amount + employer_amount")),
					TestPack: []TestCase{
						golden("a remittance settles the registered liabilities", "PAYROLL.LIABILITY_REMITTED", "", f{"tax_amount": "1800.00", "other_amount": "700.00", "employer_amount": "900.00", "remittance_ref": "REM-1"},
							dr("2310", "1800.00"), dr("2320", "700.00"), dr("2330", "900.00"), cr("1000", "3400.00")),
						golden("one liability only", "PAYROLL.LIABILITY_REMITTED", "", f{"tax_amount": "1800.00", "other_amount": "0.00", "employer_amount": "0.00", "remittance_ref": "REM-2"}, dr("2310", "1800.00"), cr("1000", "1800.00")),
					}},
			},
		},
		{
			Name:        "assets-standard",
			Description: "Fixed assets: capitalization separate from payment, depreciation from the register's schedule, impairment, revaluation, transfer between classes and disposal. The asset register computes; the kernel posts",
			Rules: []PackRule{
				{Name: "asset-capitalized", Domain: "ASSET", EventType: "ASSET.CAPITALIZED",
					Content: withDims(content(nil, exists("asset_ref", "policy_version"), line("L1", Debit, "FIXED_ASSET_COST", "amount"), line("L2", Credit, "ASSET_CLEARING", "amount")), "asset_class"),
					TestPack: []TestCase{
						goldenD("recognition of an asset, not yet paid for", "ASSET.CAPITALIZED", "", ac, f{"amount": "24000.00", "asset_ref": "FA-1", "policy_version": "p1"}, dr("1500", "24000.00"), cr("2060", "24000.00")),
						refusedD("the asset class is required", "ASSET.CAPITALIZED", "", nil, f{"amount": "24000.00", "asset_ref": "FA-1", "policy_version": "p1"}, FamDim),
					}},
				{Name: "asset-acquisition-paid", Domain: "ASSET", EventType: "ASSET.ACQUISITION_PAID",
					Content:  content(nil, exists("asset_ref"), line("L1", Debit, "ASSET_CLEARING", "amount"), line("L2", Credit, "BANK", "amount")),
					TestPack: []TestCase{golden("payment clears the acquisition clearing account", "ASSET.ACQUISITION_PAID", "", f{"amount": "24000.00", "asset_ref": "FA-1"}, dr("2060", "24000.00"), cr("1000", "24000.00"))}},
				{Name: "asset-depreciation-posted", Domain: "ASSET", EventType: "ASSET.DEPRECIATION_POSTED",
					Content: withDims(content(nil, exists("schedule_ref", "policy_version", "period_ref"), line("L1", Debit, "DEPRECIATION_EXPENSE", "amount"), line("L2", Credit, "ACCUMULATED_DEPRECIATION", "amount")), "asset_class"),
					TestPack: []TestCase{
						goldenD("a month's depreciation from the schedule", "ASSET.DEPRECIATION_POSTED", "", ac, f{"amount": "400.00", "schedule_ref": "SCH-1", "policy_version": "p1", "period_ref": "2026-03"}, dr("6200", "400.00"), cr("1590", "400.00")),
						refusedD("depreciation names its schedule and policy version", "ASSET.DEPRECIATION_POSTED", "", ac, f{"amount": "400.00", "period_ref": "2026-03"}, FamRule),
					}},
				{Name: "asset-impaired", Domain: "ASSET", EventType: "ASSET.IMPAIRED",
					Content:  withDims(content(nil, exists("impairment_ref", "approval_ref"), line("L1", Debit, "IMPAIRMENT_LOSS", "amount"), line("L2", Credit, "ACCUMULATED_DEPRECIATION", "amount")), "asset_class"),
					TestPack: []TestCase{goldenD("an approved impairment", "ASSET.IMPAIRED", "", ac, f{"amount": "1500.00", "impairment_ref": "IMP-1", "approval_ref": "A-1"}, dr("6210", "1500.00"), cr("1590", "1500.00"))}},
				{Name: "asset-revalued-up", Domain: "ASSET", EventType: "ASSET.REVALUED_UP",
					Content:  withDims(content(nil, exists("valuation_ref", "approval_ref"), line("L1", Debit, "FIXED_ASSET_COST", "amount"), line("L2", Credit, "REVALUATION_SURPLUS", "amount")), "asset_class"),
					TestPack: []TestCase{goldenD("an upward revaluation goes to the surplus", "ASSET.REVALUED_UP", "", ac, f{"amount": "2000.00", "valuation_ref": "V-1", "approval_ref": "A-1"}, dr("1500", "2000.00"), cr("3100", "2000.00"))}},
				{Name: "asset-revalued-down", Domain: "ASSET", EventType: "ASSET.REVALUED_DOWN",
					Content:  withDims(content(nil, exists("valuation_ref", "approval_ref"), line("L1", Debit, "IMPAIRMENT_LOSS", "amount"), line("L2", Credit, "FIXED_ASSET_COST", "amount")), "asset_class"),
					TestPack: []TestCase{goldenD("a downward revaluation", "ASSET.REVALUED_DOWN", "", ac, f{"amount": "500.00", "valuation_ref": "V-2", "approval_ref": "A-2"}, dr("6210", "500.00"), cr("1500", "500.00"))}},
				{Name: "asset-transferred", Domain: "ASSET", EventType: "ASSET.TRANSFERRED",
					Content: content(nil, exists("transfer_ref", "from_class", "to_class"),
						lineDims("L1", Debit, "FIXED_ASSET_COST", "cost", map[string]string{"asset_class": "$facts.to_class"}),
						lineDims("L2", Credit, "FIXED_ASSET_COST", "cost", map[string]string{"asset_class": "$facts.from_class"}),
						lineDims("L3", Debit, "ACCUMULATED_DEPRECIATION", "accumulated", map[string]string{"asset_class": "$facts.from_class"}, gt0("accumulated")),
						lineDims("L4", Credit, "ACCUMULATED_DEPRECIATION", "accumulated", map[string]string{"asset_class": "$facts.to_class"}, gt0("accumulated"))),
					TestPack: []TestCase{golden("a transfer between classes moves cost and accumulated depreciation, and changes no total", "ASSET.TRANSFERRED", "",
						f{"cost": "10000.00", "accumulated": "4000.00", "transfer_ref": "T-1", "from_class": "VEHICLES", "to_class": "EQUIPMENT"}, dr("1500", "10000.00"), cr("1500", "10000.00"), dr("1590", "4000.00"), cr("1590", "4000.00"))}},
				{Name: "asset-disposed", Domain: "ASSET", EventType: "ASSET.DISPOSED",
					Content: withDims(content(nil, exists("disposal_ref", "approval_ref"),
						line("L1", Debit, "BANK", "proceeds", gt0("proceeds")), line("L2", Debit, "ACCUMULATED_DEPRECIATION", "accumulated", gt0("accumulated")),
						line("L3", Debit, "DISPOSAL_LOSS", "loss", gt0("loss")), line("L4", Credit, "FIXED_ASSET_COST", "cost"), line("L5", Credit, "DISPOSAL_GAIN", "gain", gt0("gain"))), "asset_class"),
					TestPack: []TestCase{
						goldenD("a disposal at a gain", "ASSET.DISPOSED", "", ac, f{"cost": "10000.00", "accumulated": "6000.00", "proceeds": "5000.00", "gain": "1000.00", "loss": "0.00", "disposal_ref": "D-1", "approval_ref": "A-1"},
							dr("1000", "5000.00"), dr("1590", "6000.00"), cr("1500", "10000.00"), cr("7500", "1000.00")),
						goldenD("a disposal at a loss", "ASSET.DISPOSED", "", ac, f{"cost": "10000.00", "accumulated": "6000.00", "proceeds": "3000.00", "gain": "0.00", "loss": "1000.00", "disposal_ref": "D-2", "approval_ref": "A-2"},
							dr("1000", "3000.00"), dr("1590", "6000.00"), dr("7510", "1000.00"), cr("1500", "10000.00")),
						refusedD("a gain or loss that does not close the disposal is the register's error", "ASSET.DISPOSED", "", ac, f{"cost": "10000.00", "accumulated": "6000.00", "proceeds": "5000.00", "gain": "900.00", "loss": "0.00", "disposal_ref": "D-3", "approval_ref": "A-3"}, FamBal),
					}},
			},
		},
		{
			Name:        "inventory-standard",
			Description: "Inventory: receipt against GRNI, GRNI cleared by the supplier invoice, issue to cost of sales and counted adjustments. The inventory ledger holds quantities and cost layers; the kernel posts its valuation",
			Rules: []PackRule{
				{Name: "inventory-received", Domain: "INVENTORY", EventType: "INVENTORY.RECEIVED",
					Content: withDims(content(nil, exists("receipt_ref", "valuation_policy"), line("L1", Debit, "INVENTORY", "amount"), line("L2", Credit, "GRNI", "amount")), "location", "item_class"),
					TestPack: []TestCase{
						goldenD("a goods receipt before the invoice", "INVENTORY.RECEIVED", "", inv, f{"amount": "5000.00", "receipt_ref": "GR-1", "valuation_policy": "fifo-v2"}, dr("1400", "5000.00"), cr("2070", "5000.00")),
						refusedD("the location and item class are required", "INVENTORY.RECEIVED", "", map[string]string{"location": "WH-1"}, f{"amount": "5000.00", "receipt_ref": "GR-1", "valuation_policy": "fifo-v2"}, FamDim),
					}},
				{Name: "inventory-grni-cleared", Domain: "INVENTORY", EventType: "INVENTORY.GRNI_CLEARED",
					Content:  content(nil, exists("receipt_ref", "invoice_ref"), line("L1", Debit, "GRNI", "amount"), line("L2", Credit, "AP_CONTROL", "amount")),
					TestPack: []TestCase{golden("the supplier invoice clears the accrual", "INVENTORY.GRNI_CLEARED", "SUP-1", f{"amount": "5000.00", "receipt_ref": "GR-1", "invoice_ref": "SI-1"}, dr("2070", "5000.00"), cr("2000", "5000.00"))}},
				{Name: "inventory-issued", Domain: "INVENTORY", EventType: "INVENTORY.ISSUED",
					Content: withDims(RuleContent{Conditions: append(exists("shipment_ref", "costing_layer_ref"), Condition{Fact: "on_hand_after", Op: "ge", Value: "0"}), RoundingPolicyID: "HALF_EVEN",
						LineTemplates: []LineTemplate{line("L1", Debit, "COGS", "amount"), line("L2", Credit, "INVENTORY", "amount")}}, "location", "item_class"),
					TestPack: []TestCase{
						goldenD("an issue at the cost layer's value", "INVENTORY.ISSUED", "", inv, f{"amount": "1200.00", "shipment_ref": "SH-1", "costing_layer_ref": "L-9", "on_hand_after": "40"}, dr("5000", "1200.00"), cr("1400", "1200.00")),
						refusedD("an issue that would take stock negative is refused unless a policy allows it", "INVENTORY.ISSUED", "", inv, f{"amount": "1200.00", "shipment_ref": "SH-2", "costing_layer_ref": "L-9", "on_hand_after": "-3"}, FamRule),
					}},
				{Name: "inventory-adjusted-down", Domain: "INVENTORY", EventType: "INVENTORY.ADJUSTED_DOWN",
					Content:  withDims(content(nil, exists("count_ref", "approval_ref"), line("L1", Debit, "INVENTORY_VARIANCE", "amount"), line("L2", Credit, "INVENTORY", "amount")), "location", "item_class"),
					TestPack: []TestCase{goldenD("an approved count shortage", "INVENTORY.ADJUSTED_DOWN", "", inv, f{"amount": "300.00", "count_ref": "C-1", "approval_ref": "A-1"}, dr("5100", "300.00"), cr("1400", "300.00"))}},
				{Name: "inventory-adjusted-up", Domain: "INVENTORY", EventType: "INVENTORY.ADJUSTED_UP",
					Content:  withDims(content(nil, exists("count_ref", "approval_ref"), line("L1", Debit, "INVENTORY", "amount"), line("L2", Credit, "INVENTORY_VARIANCE", "amount")), "location", "item_class"),
					TestPack: []TestCase{goldenD("an approved count surplus", "INVENTORY.ADJUSTED_UP", "", inv, f{"amount": "100.00", "count_ref": "C-2", "approval_ref": "A-2"}, dr("1400", "100.00"), cr("5100", "100.00"))}},
			},
		},
		{
			Name:        "tax-standard",
			Description: "Tax accounting: payment of a registered obligation, customer withholding, approved return adjustments and the income tax provision. The tax services determine; the kernel posts their authoritative results",
			Rules: []PackRule{
				{Name: "tax-payment-made", Domain: "TAX", EventType: "TAX.PAYMENT_MADE",
					Content: content(nil, exists("obligation_ref", "tax_type", "jurisdiction"), line("L1", Debit, "TAX_PAYABLE", "amount"), line("L2", Credit, "BANK", "amount")),
					TestPack: []TestCase{
						golden("a payment settles a registered obligation", "TAX.PAYMENT_MADE", "", f{"amount": "2200.00", "obligation_ref": "OB-1", "tax_type": "VAT", "jurisdiction": "GB"}, dr("2210", "2200.00"), cr("1000", "2200.00")),
						refused("a payment must name the obligation it settles", "TAX.PAYMENT_MADE", "", f{"amount": "2200.00", "tax_type": "VAT", "jurisdiction": "GB"}, FamRule),
					}},
				{Name: "tax-withholding-receivable", Domain: "TAX", EventType: "TAX.WITHHOLDING_RECEIVABLE",
					Content:  content(nil, exists("authority", "tax_type", "certificate_ref"), line("L1", Debit, "WITHHOLDING_RECEIVABLE", "amount"), line("L2", Credit, "AR_CONTROL", "amount")),
					TestPack: []TestCase{golden("tax withheld by a customer is a receivable from the authority", "TAX.WITHHOLDING_RECEIVABLE", "CUST-1", f{"amount": "150.00", "authority": "HMRC", "tax_type": "WHT", "certificate_ref": "C-1"}, dr("1310", "150.00"), cr("1100", "150.00"))}},
				{Name: "tax-return-adjustment-up", Domain: "TAX", EventType: "TAX.RETURN_ADJUSTMENT_UP",
					Content:  content(nil, exists("return_version", "approval_ref", "period_ref"), line("L1", Debit, "TAX_ADJUSTMENT", "amount"), line("L2", Credit, "TAX_PAYABLE", "amount")),
					TestPack: []TestCase{golden("an approved adjustment that increases the liability", "TAX.RETURN_ADJUSTMENT_UP", "", f{"amount": "80.00", "return_version": "r2", "approval_ref": "A-1", "period_ref": "2026-03"}, dr("8100", "80.00"), cr("2210", "80.00"))}},
				{Name: "tax-return-adjustment-down", Domain: "TAX", EventType: "TAX.RETURN_ADJUSTMENT_DOWN",
					Content: content(nil, exists("return_version", "approval_ref", "period_ref"), line("L1", Debit, "TAX_PAYABLE", "amount"), line("L2", Credit, "TAX_ADJUSTMENT", "amount")),
					TestPack: []TestCase{
						golden("an approved adjustment that reduces the liability", "TAX.RETURN_ADJUSTMENT_DOWN", "", f{"amount": "80.00", "return_version": "r2", "approval_ref": "A-1", "period_ref": "2026-03"}, dr("2210", "80.00"), cr("8100", "80.00")),
						refused("an adjustment without an approved return version is not posted", "TAX.RETURN_ADJUSTMENT_DOWN", "", f{"amount": "80.00", "period_ref": "2026-03"}, FamRule),
					}},
				{Name: "tax-provision-current", Domain: "TAX", EventType: "TAX.PROVISION_CURRENT",
					Content:  content(nil, exists("provision_ref", "provision_authority"), line("L1", Debit, "INCOME_TAX_EXPENSE", "amount"), line("L2", Credit, "TAX_PAYABLE", "amount")),
					TestPack: []TestCase{golden("the current income tax provision", "TAX.PROVISION_CURRENT", "", f{"amount": "9000.00", "provision_ref": "P-1", "provision_authority": "TAXDIR"}, dr("8000", "9000.00"), cr("2210", "9000.00"))}},
				{Name: "tax-provision-deferred", Domain: "TAX", EventType: "TAX.PROVISION_DEFERRED",
					Content:  content(nil, exists("provision_ref", "provision_authority"), line("L1", Debit, "INCOME_TAX_EXPENSE", "amount"), line("L2", Credit, "DEFERRED_TAX", "amount")),
					TestPack: []TestCase{golden("the deferred income tax provision", "TAX.PROVISION_DEFERRED", "", f{"amount": "1200.00", "provision_ref": "P-1", "provision_authority": "TAXDIR"}, dr("8000", "1200.00"), cr("2220", "1200.00"))}},
			},
		},
		{
			Name:        "schedules-standard",
			Description: "Schedules: accrual recognition and reversal, prepayment capitalization and recognition, deferred revenue recognition. Every posting names its schedule line and the balance that remains",
			Rules: []PackRule{
				{Name: "schedule-accrual-recognized", Domain: "SCHEDULE", EventType: "SCHEDULE.ACCRUAL_RECOGNIZED",
					Content: content(nil, exists("schedule_ref", "schedule_line", "remaining_balance"), line("L1", Debit, "EXPENSE", "amount"), line("L2", Credit, "ACCRUED_LIABILITY", "amount")),
					TestPack: []TestCase{
						golden("an accrual", "SCHEDULE.ACCRUAL_RECOGNIZED", "", f{"amount": "500.00", "schedule_ref": "S-1", "schedule_line": "1", "remaining_balance": "0.00"}, dr("6000", "500.00"), cr("2400", "500.00")),
						refused("a recognition names its schedule line and the remaining balance", "SCHEDULE.ACCRUAL_RECOGNIZED", "", f{"amount": "500.00", "schedule_ref": "S-1"}, FamRule),
					}},
				{Name: "schedule-accrual-reversed", Domain: "SCHEDULE", EventType: "SCHEDULE.ACCRUAL_REVERSED",
					Content:  content(nil, exists("schedule_ref", "schedule_line", "reverses_line"), line("L1", Debit, "ACCRUED_LIABILITY", "amount"), line("L2", Credit, "EXPENSE", "amount")),
					TestPack: []TestCase{golden("the scheduled reversal of an accrual", "SCHEDULE.ACCRUAL_REVERSED", "", f{"amount": "500.00", "schedule_ref": "S-1", "schedule_line": "2", "reverses_line": "1"}, dr("2400", "500.00"), cr("6000", "500.00"))}},
				{Name: "schedule-prepayment-paid", Domain: "SCHEDULE", EventType: "SCHEDULE.PREPAYMENT_PAID",
					Content:  content(nil, exists("schedule_ref"), line("L1", Debit, "PREPAID_ASSET", "amount"), line("L2", Credit, "BANK", "amount")),
					TestPack: []TestCase{golden("a prepayment paid from the bank", "SCHEDULE.PREPAYMENT_PAID", "", f{"amount": "1200.00", "schedule_ref": "S-2"}, dr("1200", "1200.00"), cr("1000", "1200.00"))}},
				{Name: "schedule-prepayment-invoiced", Domain: "SCHEDULE", EventType: "SCHEDULE.PREPAYMENT_INVOICED",
					Content:  content(nil, exists("schedule_ref"), line("L1", Debit, "PREPAID_ASSET", "amount"), line("L2", Credit, "AP_CONTROL", "amount")),
					TestPack: []TestCase{golden("a prepayment raised through payables", "SCHEDULE.PREPAYMENT_INVOICED", "SUP-1", f{"amount": "1200.00", "schedule_ref": "S-2"}, dr("1200", "1200.00"), cr("2000", "1200.00"))}},
				{Name: "schedule-prepayment-recognized", Domain: "SCHEDULE", EventType: "SCHEDULE.PREPAYMENT_RECOGNIZED",
					Content:  content(nil, exists("schedule_ref", "schedule_line", "remaining_balance"), line("L1", Debit, "EXPENSE", "amount"), line("L2", Credit, "PREPAID_ASSET", "amount")),
					TestPack: []TestCase{golden("a month of a prepayment", "SCHEDULE.PREPAYMENT_RECOGNIZED", "", f{"amount": "100.00", "schedule_ref": "S-2", "schedule_line": "3", "remaining_balance": "900.00"}, dr("6000", "100.00"), cr("1200", "100.00"))}},
				{Name: "schedule-deferral-recognized", Domain: "SCHEDULE", EventType: "SCHEDULE.DEFERRAL_RECOGNIZED",
					Content:  content(nil, exists("schedule_ref", "schedule_line", "remaining_balance"), line("L1", Debit, "DEFERRED_REVENUE", "amount"), line("L2", Credit, "REVENUE", "amount")),
					TestPack: []TestCase{golden("revenue recognised from a deferral", "SCHEDULE.DEFERRAL_RECOGNIZED", "", f{"amount": "250.00", "schedule_ref": "S-3", "schedule_line": "1", "remaining_balance": "750.00"}, dr("2500", "250.00"), cr("4000", "250.00"))}},
			},
		},
	}
}
