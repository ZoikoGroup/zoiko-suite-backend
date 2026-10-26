package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// Wave 8 (granular payroll results): the payroll-results pack, as a payroll engine that accounts for every component kind separately
// uses it, against real Postgres as the ordinary role.

func (w *w2) payrollResultSetup(t *testing.T) {
	t.Helper()
	accounts := [][3]string{{"6150", "EXPENSE", ""}, {"6151", "EXPENSE", ""}, {"6152", "EXPENSE", ""}, {"6153", "EXPENSE", ""}, {"6154", "EXPENSE", ""}, {"6155", "EXPENSE", ""},
		{"2350", "LIABILITY", "PAYROLL"}, {"2351", "LIABILITY", "PAYROLL"}, {"2352", "LIABILITY", "PAYROLL"}, {"2353", "LIABILITY", "PAYROLL"}, {"2354", "LIABILITY", "PAYROLL"}, {"2355", "LIABILITY", "PAYROLL"},
		{"2361", "LIABILITY", "PAYROLL"}, {"2362", "LIABILITY", "PAYROLL"}, {"2363", "LIABILITY", "PAYROLL"}, {"2364", "LIABILITY", "PAYROLL"}, {"2365", "LIABILITY", "PAYROLL"}}
	for _, a := range accounts {
		if err := w.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`, uuid.NewString(), w.tenant, a[0], a[0]+" account", a[1]); err != nil {
			t.Fatal(err)
		}
		if a[2] != "" {
			if st, b := w.call("PUT", "/v1/account-applicability/"+a[0], w.tenant, "chart-admin", map[string]any{"control_type": a[2]}); st != 200 {
				t.Fatalf("scope %s: %d %v", a[0], st, b)
			}
		}
	}
	keys := map[string]string{"PAYROLL_GROSS_EXPENSE": "6150", "PAYROLL_EMPLOYER_TAX_EXPENSE": "6151", "PAYROLL_EMPLOYER_SOCIAL_EXPENSE": "6152", "PAYROLL_EMPLOYER_BENEFIT_EXPENSE": "6153",
		"PAYROLL_EMPLOYER_PENSION_EXPENSE": "6154", "PAYROLL_EMPLOYER_OTHER_EXPENSE": "6155", "PAYROLL_NET_PAY_PAYABLE": "2350", "PAYROLL_EMPLOYEE_DEDUCTION_PAYABLE": "2351", "PAYROLL_EMPLOYEE_TAX_PAYABLE": "2352",
		"PAYROLL_EMPLOYEE_SOCIAL_PAYABLE": "2353", "PAYROLL_EMPLOYEE_PENSION_PAYABLE": "2354", "PAYROLL_EMPLOYEE_BENEFIT_PAYABLE": "2355", "PAYROLL_EMPLOYER_TAX_PAYABLE": "2361", "PAYROLL_EMPLOYER_SOCIAL_PAYABLE": "2362",
		"PAYROLL_EMPLOYER_BENEFIT_PAYABLE": "2363", "PAYROLL_EMPLOYER_PENSION_PAYABLE": "2364", "PAYROLL_EMPLOYER_OTHER_PAYABLE": "2365"}
	for k, c := range keys {
		if err := w.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,$3,$4,'2026-01-01','seed')`, uuid.NewString(), w.tenant, k, c); err != nil {
			t.Fatal(err)
		}
	}
	st, b := w.call("POST", "/v1/posting-rule-packs/payroll-results/instantiate", w.tenant, "builder", map[string]any{"version": "1.0.0", "effective_from": "2026-01-01", "book_type": "PRIMARY_STATUTORY", "accounting_framework_id": "IFRS"})
	if st != 201 {
		t.Fatalf("instantiate: %d %v", st, b)
	}
	for _, rs := range b["rule_sets"].([]any) {
		id := rs.(map[string]any)["posting_rule_set_id"].(string)
		for _, step := range [][2]string{{"test", "builder"}, {"approve", "approver"}, {"activate", "releaser"}} {
			if st, b := w.call("POST", "/v1/posting-rule-sets/"+id+"/"+step[0], w.tenant, step[1], map[string]any{"book_id": w.book}); st != 200 {
				t.Fatalf("%s: %d %v", step[0], st, b)
			}
		}
	}
}

func resultFacts(extra map[string]string) map[string]any {
	f := map[string]any{"gross": "0.00", "net": "0.00", "result_ref": "RES-9", "result_version": "1"}
	for _, k := range []string{"ee_deduction", "ee_tax", "ee_social", "ee_pension", "ee_benefit", "er_tax", "er_social", "er_pension", "er_benefit", "er_other"} {
		f[k] = "0.00"
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func TestGranularPayrollResultPostsEachComponentOnItsOwnAccountAndReverses(t *testing.T) {
	w := w2Setup(t)
	w.payrollResultSetup(t)
	ev := w.eventAt(t, "PAYROLL.RESULT_ACCEPTED", "", "GBP", "2026-03-25", resultFacts(map[string]string{"gross": "10000.00", "net": "7500.00", "ee_tax": "1800.00", "ee_pension": "700.00", "er_tax": "600.00", "er_pension": "300.00"}))
	st, res := w.kenv.post(ev, w.book, nil)
	if st != 201 || res["journal_number"] == "" {
		t.Fatalf("%d %v", st, res)
	}
	for acct, want := range map[string]string{"6150": "10000.00", "6151": "600.00", "6154": "300.00", "2350": "-7500.00", "2352": "-1800.00", "2354": "-700.00", "2361": "-600.00", "2364": "-300.00", "6152": "0", "2351": "0"} {
		if got := w.net(t, acct); got != want {
			t.Fatalf("account %s: %s, want %s", acct, got, want)
		}
	}
	// the payable accounts are control accounts: the legacy posting command cannot write them, only an event can
	// a repeat of the same event posts nothing more
	if st, again := w.kenv.post(ev, w.book, nil); st != 200 || again["journal_id"] != res["journal_id"] {
		t.Fatalf("%d %v", st, again)
	}
	// payroll reverses through the ledger reversal command: the journal is reversed and the accounts are back to nothing
	if st, b := w.call("POST", "/v1/postings/reversals", w.tenant, "payroll-svc", map[string]any{"original_journal_id": res["journal_id"], "reason": "result superseded"}); st != 201 && st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if w.net(t, "6150") != "0.00" || w.net(t, "2352") != "0.00" {
		t.Fatalf("reversed: %s %s", w.net(t, "6150"), w.net(t, "2352"))
	}
	if got := w.scalar(t, `SELECT status FROM journal_headers WHERE journal_id = $1::uuid`, res["journal_id"].(string)); got != "REVERSED" {
		t.Fatal(got)
	}
}

func TestGranularPayrollResultRefusesWhatDoesNotAddUpAndRoundsFourDecimals(t *testing.T) {
	w := w2Setup(t)
	w.payrollResultSetup(t)
	unbalanced := w.eventAt(t, "PAYROLL.RESULT_ACCEPTED", "", "GBP", "2026-03-25", resultFacts(map[string]string{"gross": "1000.00", "net": "900.00", "ee_tax": "50.00"}))
	if st, b := w.kenv.post(unbalanced, w.book, nil); st != 422 || b["family"] != "AK-BAL" {
		t.Fatalf("gross that is not net plus deductions is refused, not plugged: %d %v", st, b)
	}
	// four-decimal amounts from the payroll engine are rounded half-even to the currency at the rule rounding stage, and still balance
	fine := w.eventAt(t, "PAYROLL.RESULT_ACCEPTED", "", "GBP", "2026-03-25", resultFacts(map[string]string{"gross": "1000.0060", "net": "1000.0060"}))
	if st, b := w.kenv.post(fine, w.book, nil); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if w.net(t, "6150") != "1000.01" || w.net(t, "2350") != "-1000.01" {
		t.Fatalf("%s %s", w.net(t, "6150"), w.net(t, "2350"))
	}
	if n := w.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid`, w.tenant); n != 1 {
		t.Fatalf("only the balanced result was posted: %d", n)
	}
}
