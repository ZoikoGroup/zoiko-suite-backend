package kernel

// The payroll-results pack: the granular form of an accepted payroll result, for payroll engines that account for each component kind
// on its own account (an employee tax payable, an employer pension expense and its payable, and so on). The mapping keys are the ones
// workforce payroll already uses, so a tenant maps them once. The engine computes; this rule posts its accepted result and refuses one
// that does not balance: gross plus the employer cost must equal net pay plus every liability.

// payrollKinds are the component kinds of the accepted result: the fact that carries the amount, and the mapping keys of the accounts.
var payrollEmployeeKinds = [][2]string{{"ee_deduction", "DEDUCTION"}, {"ee_tax", "TAX"}, {"ee_social", "SOCIAL"}, {"ee_pension", "PENSION"}, {"ee_benefit", "BENEFIT"}}
var payrollEmployerKinds = [][2]string{{"er_tax", "TAX"}, {"er_social", "SOCIAL"}, {"er_pension", "PENSION"}, {"er_benefit", "BENEFIT"}, {"er_other", "OTHER"}}

func payrollResultFacts(extra map[string]string) map[string]string {
	f := map[string]string{"gross": "0.00", "net": "0.00", "result_ref": "RES-1", "result_version": "1"}
	for _, k := range payrollEmployeeKinds {
		f[k[0]] = "0.00"
	}
	for _, k := range payrollEmployerKinds {
		f[k[0]] = "0.00"
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func payrollResultPacks() []RulePack {
	lines := []LineTemplate{line("GROSS", Debit, "PAYROLL_GROSS_EXPENSE", "gross")}
	for _, k := range payrollEmployerKinds {
		lines = append(lines, line("ERX_"+k[1], Debit, "PAYROLL_EMPLOYER_"+k[1]+"_EXPENSE", k[0], gt0(k[0])))
	}
	lines = append(lines, line("NET", Credit, "PAYROLL_NET_PAY_PAYABLE", "net"))
	for _, k := range payrollEmployeeKinds {
		lines = append(lines, line("EEP_"+k[1], Credit, "PAYROLL_EMPLOYEE_"+k[1]+"_PAYABLE", k[0], gt0(k[0])))
	}
	for _, k := range payrollEmployerKinds {
		lines = append(lines, line("ERP_"+k[1], Credit, "PAYROLL_EMPLOYER_"+k[1]+"_PAYABLE", k[0], gt0(k[0])))
	}
	return []RulePack{{
		Name:        "payroll-results",
		Description: "Payroll results by component kind: an accepted result version posted with each employee deduction and employer cost on its own expense and liability account",
		Rules: []PackRule{{
			Name: "payroll-result-accepted", Domain: "PAYROLL", EventType: "PAYROLL.RESULT_ACCEPTED",
			Content: content(nil, exists("result_ref", "result_version"), lines...),
			TestPack: []TestCase{
				golden("a result with deductions and employer costs", "PAYROLL.RESULT_ACCEPTED", "",
					payrollResultFacts(map[string]string{"gross": "10000.00", "net": "7500.00", "ee_tax": "1800.00", "ee_pension": "700.00", "er_tax": "600.00", "er_pension": "300.00"}),
					dr("6150", "10000.00"), dr("6151", "600.00"), dr("6154", "300.00"),
					cr("2350", "7500.00"), cr("2352", "1800.00"), cr("2354", "700.00"), cr("2361", "600.00"), cr("2364", "300.00")),
				golden("a result with nothing withheld", "PAYROLL.RESULT_ACCEPTED", "",
					payrollResultFacts(map[string]string{"gross": "1000.00", "net": "1000.00"}), dr("6150", "1000.00"), cr("2350", "1000.00")),
				refused("a result that is not an accepted version is not posted", "PAYROLL.RESULT_ACCEPTED", "",
					map[string]string{"gross": "1000.00", "net": "1000.00"}, FamRule),
				refused("gross that is not net plus the deductions is the payroll engine's error", "PAYROLL.RESULT_ACCEPTED", "",
					payrollResultFacts(map[string]string{"gross": "1000.00", "net": "900.00", "ee_tax": "50.00"}), FamBal),
			},
		}},
	}}
}
