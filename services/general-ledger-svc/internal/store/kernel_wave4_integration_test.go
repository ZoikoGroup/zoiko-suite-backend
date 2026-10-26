package store_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// Wave 4 (payroll, fixed assets, inventory, tax) against real Postgres as the ordinary role: the subledger packs post approved
// results, and each domain reconciles to its GL control accounts — by group where the domain is grouped.

func w4Setup(t *testing.T) *w2 {
	t.Helper()
	w := w2Setup(t)
	extra := [][3]string{{"6100", "EXPENSE", ""}, {"6110", "EXPENSE", ""}, {"2310", "LIABILITY", "PAYROLL"}, {"2320", "LIABILITY", "PAYROLL"}, {"2330", "LIABILITY", "PAYROLL"}, {"2340", "LIABILITY", "PAYROLL"},
		{"1500", "ASSET", "ASSET"}, {"1590", "ASSET", "ASSET"}, {"6200", "EXPENSE", ""}, {"6210", "EXPENSE", ""}, {"3100", "EQUITY", ""}, {"2060", "LIABILITY", "CLEARING"}, {"7500", "REVENUE", ""}, {"7510", "EXPENSE", ""},
		{"1400", "ASSET", "INVENTORY"}, {"2070", "LIABILITY", "CLEARING"}, {"5000", "EXPENSE", ""}, {"5100", "EXPENSE", ""}, {"2210", "LIABILITY", "TAX"}, {"2220", "LIABILITY", "TAX"}, {"8000", "EXPENSE", ""}, {"8100", "EXPENSE", ""},
		{"1310", "ASSET", ""}, {"1200", "ASSET", ""}, {"2400", "LIABILITY", ""}, {"2500", "LIABILITY", ""}}
	for _, a := range extra {
		if err := w.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`, uuid.NewString(), w.tenant, a[0], a[0]+" account", a[1]); err != nil {
			t.Fatal(err)
		}
	}
	keys := map[string]string{"PAYROLL_EXPENSE": "6100", "EMPLOYER_CONTRIB_EXPENSE": "6110", "PAYROLL_TAX_LIABILITY": "2310", "EMPLOYEE_DEDUCTIONS_LIABILITY": "2320", "EMPLOYER_CONTRIB_LIABILITY": "2330", "NET_PAY_PAYABLE": "2340",
		"FIXED_ASSET_COST": "1500", "ACCUMULATED_DEPRECIATION": "1590", "DEPRECIATION_EXPENSE": "6200", "IMPAIRMENT_LOSS": "6210", "REVALUATION_SURPLUS": "3100", "ASSET_CLEARING": "2060", "DISPOSAL_GAIN": "7500", "DISPOSAL_LOSS": "7510",
		"INVENTORY": "1400", "GRNI": "2070", "COGS": "5000", "INVENTORY_VARIANCE": "5100", "TAX_PAYABLE": "2210", "INCOME_TAX_EXPENSE": "8000", "DEFERRED_TAX": "2220", "TAX_ADJUSTMENT": "8100", "WITHHOLDING_RECEIVABLE": "1310",
		"PREPAID_ASSET": "1200", "ACCRUED_LIABILITY": "2400", "DEFERRED_REVENUE": "2500"}
	for k, c := range keys {
		if err := w.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,$3,$4,'2026-01-01','seed')`, uuid.NewString(), w.tenant, k, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range extra {
		if a[2] == "" {
			continue
		}
		if st, b := w.call("PUT", "/v1/account-applicability/"+a[0], w.tenant, "chart-admin", map[string]any{"control_type": a[2]}); st != 200 {
			t.Fatalf("scope %s: %d %v", a[0], st, b)
		}
	}
	dims := map[string][]string{"cost_center": {"CC-100"}, "asset_class": {"VEHICLES", "EQUIPMENT"}, "location": {"WH-1", "WH-2"}, "item_class": {"FINISHED"}}
	for d, vals := range dims {
		if st, b := w.call("POST", "/v1/dimensions", w.tenant, "owner", map[string]any{"dimension_code": d, "name": d}); st != 201 {
			t.Fatalf("%d %v", st, b)
		}
		for _, v := range vals {
			if st, b := w.call("POST", "/v1/dimensions/"+d+"/values", w.tenant, "owner", map[string]any{"value_code": v, "name": v, "valid_from": "2026-01-01T00:00:00Z"}); st != 201 {
				t.Fatalf("%d %v", st, b)
			}
		}
	}
	for _, pack := range []string{"payroll-standard", "assets-standard", "inventory-standard", "tax-standard", "schedules-standard"} {
		st, b := w.call("POST", "/v1/posting-rule-packs/"+pack+"/instantiate", w.tenant, "builder", map[string]any{"version": "1.0.0", "effective_from": "2026-01-01", "book_type": "PRIMARY_STATUTORY", "accounting_framework_id": "IFRS"})
		if st != 201 {
			t.Fatalf("instantiate %s: %d %v", pack, st, b)
		}
		for _, rs := range b["rule_sets"].([]any) {
			id := rs.(map[string]any)["posting_rule_set_id"].(string)
			for _, step := range [][2]string{{"test", "builder"}, {"approve", "approver"}, {"activate", "releaser"}} {
				if st, b := w.call("POST", "/v1/posting-rule-sets/"+id+"/"+step[0], w.tenant, step[1], map[string]any{"book_id": w.book}); st != 200 {
					t.Fatalf("%s %v: %d %v", step[0], rs.(map[string]any)["name"], st, b)
				}
			}
		}
	}
	return w
}

// postD registers and posts an event carrying dimensions, on the 10th of March.
func (w *w2) postD(t *testing.T, etype, cp string, dims map[string]string, facts map[string]any) (int, map[string]any) {
	t.Helper()
	w2seq++
	src := fmt.Sprintf("w4-%d", w2seq)
	body := map[string]any{"event_type": etype, "event_version": 1, "legal_entity_id": w.entity, "source_system": "subledger", "source_object_type": "doc", "source_object_id": src,
		"source_event_id": src, "occurred_at": "2026-03-10T09:00:00Z", "document_date": "2026-03-10T00:00:00Z", "transaction_currency": "GBP", "facts": facts,
		"idempotency_key": "idem-" + src, "correlation_id": "corr-" + src, "causation_id": "cause-" + src}
	if cp != "" {
		body["counterparty_id"] = cp
	}
	if dims != nil {
		body["dimensions"] = dims
	}
	st, ev := w.call("POST", "/v1/accounting-events", w.tenant, "subledger", body)
	if st != 201 {
		t.Fatalf("register %s: %d %v", etype, st, ev)
	}
	return w.kenv.post(ev["accounting_event_id"].(string), w.book, nil)
}

func (w *w2) mustPostD(t *testing.T, etype, cp string, dims map[string]string, facts map[string]any) map[string]any {
	t.Helper()
	st, res := w.postD(t, etype, cp, dims, facts)
	if st != 201 {
		t.Fatalf("post %s: %d %v", etype, st, res)
	}
	return res
}

func (w *w2) reconX(typ, total, ref string, extra map[string]any) (int, map[string]any) {
	body := map[string]any{"reconciliation_type": typ, "book_id": w.book, "fiscal_period": "2026-03", "as_of": "2026-03-31", "population_a": map[string]any{"source": "subledger", "reference": ref, "total": total}}
	for k, v := range extra {
		body[k] = v
	}
	return w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", body)
}

func TestPayrollPostsTheApprovedResultAndReconcilesToItsLiabilities(t *testing.T) {
	w := w4Setup(t)
	cc := map[string]string{"cost_center": "CC-100"}
	run := func(ref string, gross, tax, other, employer, net string) map[string]any {
		return map[string]any{"gross_pay": gross, "tax_withheld": tax, "other_deductions": other, "employer_contributions": employer, "net_pay": net, "payroll_run_ref": ref, "approval_ref": "APPR-" + ref, "payroll_rule_version": "uk-2026.2"}
	}
	// the March run is approved, paid and remitted in full
	w.mustPostD(t, "PAYROLL.RUN_APPROVED", "", cc, run("PR-MAR", "10000.00", "1800.00", "700.00", "900.00", "7500.00"))
	w.mustPostD(t, "PAYROLL.PAYMENT_SETTLED", "", nil, map[string]any{"amount": "7500.00", "payment_ref": "PAY-1", "payroll_run_ref": "PR-MAR"})
	w.mustPostD(t, "PAYROLL.LIABILITY_REMITTED", "", nil, map[string]any{"tax_amount": "1800.00", "other_amount": "700.00", "employer_amount": "900.00", "remittance_ref": "REM-1"})
	if st, r := w.reconX("PAYROLL_GL", "0.00", "payroll-engine-mar", nil); st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("a fully settled run leaves nothing owed: %d %v", st, r)
	}
	// a second run is approved and not yet paid: the payroll engine says 5450.00 is owed, and the ledger agrees
	w.mustPostD(t, "PAYROLL.RUN_APPROVED", "", cc, run("PR-APR", "5000.00", "900.00", "350.00", "450.00", "3750.00"))
	st, r := w.reconX("PAYROLL_GL", "5450.00", "payroll-engine-apr", nil)
	if st != 201 || r["status"] != "MATCHED" || r["actual_total"] != "5450.000000" {
		t.Fatalf("%d %v", st, r)
	}
	// the engine and the ledger disagree by the net pay of one employee: a break, with the difference stated
	if st, r = w.reconX("PAYROLL_GL", "5200.00", "payroll-engine-apr-2", nil); st != 201 || r["status"] != "BREAK" || r["difference"] != "-250.000000" {
		t.Fatalf("%d %v", st, r)
	}
	// the kernel does not repair the payroll engine's arithmetic, and it asks for the evidence
	if st, b := w.postD(t, "PAYROLL.RUN_APPROVED", "", cc, run("PR-BAD", "1000.00", "200.00", "0.00", "0.00", "799.00")); st != 422 || b["family"] != "AK-BAL" {
		t.Fatalf("%d %v", st, b)
	}
	noApproval := run("PR-NA", "1000.00", "200.00", "0.00", "0.00", "800.00")
	delete(noApproval, "approval_ref")
	if st, b := w.postD(t, "PAYROLL.RUN_APPROVED", "", cc, noApproval); st != 422 || b["family"] != "AK-RULE" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.postD(t, "PAYROLL.RUN_APPROVED", "", nil, run("PR-ND", "1000.00", "200.00", "0.00", "0.00", "800.00")); st != 422 || b["family"] != "AK-DIM" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.postD(t, "PAYROLL.RUN_APPROVED", "", map[string]string{"cost_center": "CC-999"}, run("PR-NV", "1000.00", "200.00", "0.00", "0.00", "800.00")); st != 422 || b["code"] != "DIMENSION_VALUE_INVALID" {
		t.Fatalf("a cost centre that is not registered: %d %v", st, b)
	}
	// the dimension travels with the expense
	if w.scalar(t, `SELECT dimensions->>'cost_center' FROM ledger_entries WHERE account_code='6100' LIMIT 1`) != "CC-100" {
		t.Fatal("the cost centre is on the ledger entry")
	}
}

func TestFixedAssetsReconcileByClassAndATotalDoesNotHideAWrongGroup(t *testing.T) {
	w := w4Setup(t)
	veh, eq := map[string]string{"asset_class": "VEHICLES"}, map[string]string{"asset_class": "EQUIPMENT"}
	cap := func(d map[string]string, amount, ref string) {
		w.mustPostD(t, "ASSET.CAPITALIZED", "", d, map[string]any{"amount": amount, "asset_ref": ref, "policy_version": "p1"})
	}
	dep := func(d map[string]string, amount, ref string) {
		w.mustPostD(t, "ASSET.DEPRECIATION_POSTED", "", d, map[string]any{"amount": amount, "schedule_ref": ref, "policy_version": "p1", "period_ref": "2026-03"})
	}
	cap(veh, "24000.00", "FA-1")
	cap(eq, "6000.00", "FA-2")
	w.mustPostD(t, "ASSET.ACQUISITION_PAID", "", nil, map[string]any{"amount": "30000.00", "asset_ref": "FA-1/2"})
	dep(veh, "400.00", "SCH-1")
	dep(eq, "100.00", "SCH-2")

	// the register: cost less accumulated depreciation, by class
	groups := map[string]any{"VEHICLES": "23600.00", "EQUIPMENT": "5900.00"}
	// a grouped run without the register's figure for each group is not a population
	if st, r := w.reconX("ASSET_GL", "29500.00", "register-mar", map[string]any{"group_by": "asset_class"}); st != 422 || r["code"] != "RECON_INVALID" {
		t.Fatalf("%d %v", st, r)
	}
	body := func(total string, g map[string]any) (int, map[string]any) {
		return w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", map[string]any{"reconciliation_type": "ASSET_GL", "book_id": w.book, "fiscal_period": "2026-03", "as_of": "2026-03-31",
			"group_by": "asset_class", "population_a": map[string]any{"source": "asset-register", "reference": "register-mar", "total": total, "groups": g}})
	}
	st, r := body("29500.00", groups)
	if st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, r)
	}
	gs := r["population_b"].(map[string]any)["detail"].(map[string]any)["groups"].([]any)
	if len(gs) != 2 || gs[0].(map[string]any)["key"] != "EQUIPMENT" || gs[0].(map[string]any)["actual"] != "5900.000000" {
		t.Fatalf("%v", gs)
	}
	if st, v := w.call("POST", "/v1/reconciliation-runs/"+r["reconciliation_run_id"].(string)+"/verify", w.tenant, "auditor", map[string]any{}); st != 200 || v["reproducible"] != true {
		t.Fatalf("%d %v", st, v)
	}
	// groups that do not add up to the total are not a population
	if st, r = body("29500.00", map[string]any{"VEHICLES": "23600.00", "EQUIPMENT": "5000.00"}); st != 422 {
		t.Fatalf("%d %v", st, r)
	}

	// a transfer of 5000 of cost (and 200 of accumulated depreciation) from vehicles to equipment changes no total
	w.mustPostD(t, "ASSET.TRANSFERRED", "", nil, map[string]any{"cost": "5000.00", "accumulated": "200.00", "transfer_ref": "T-1", "from_class": "VEHICLES", "to_class": "EQUIPMENT"})
	if w.scalar(t, `SELECT string_agg(dimensions->>'asset_class' || ':' || debit_amount::text || '/' || credit_amount::text, ',' ORDER BY line_number) FROM ledger_entries WHERE journal_id = (SELECT journal_id FROM journal_headers ORDER BY created_at DESC LIMIT 1)`) !=
		"EQUIPMENT:5000.00/0.00,VEHICLES:0.00/5000.00,VEHICLES:200.00/0.00,EQUIPMENT:0.00/200.00" {
		t.Fatal("the transfer moves cost and accumulated depreciation between the classes")
	}
	// the old figures still add up to the same total, but the groups no longer agree with the ledger: the case is a break
	st, r = body("29500.00", groups)
	if st != 201 || r["status"] != "BREAK" || r["difference"] != "0.000000" {
		t.Fatalf("a matching total does not excuse a group that is out: %d %v", st, r)
	}
	// the register that took the transfer: VEHICLES 19000 - 200 = 18800, EQUIPMENT 11000 - 300 = 10700
	if st, r = body("29500.00", map[string]any{"VEHICLES": "18800.00", "EQUIPMENT": "10700.00"}); st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, r)
	}

	// a disposal: cost 3000 with 100 accumulated, sold for 3500 at a gain of 600
	w.mustPostD(t, "ASSET.DISPOSED", "", eq, map[string]any{"cost": "3000.00", "accumulated": "100.00", "proceeds": "3500.00", "gain": "600.00", "loss": "0.00", "disposal_ref": "D-1", "approval_ref": "A-1"})
	if st, b := w.postD(t, "ASSET.DISPOSED", "", eq, map[string]any{"cost": "3000.00", "accumulated": "100.00", "proceeds": "3500.00", "gain": "500.00", "loss": "0.00", "disposal_ref": "D-2", "approval_ref": "A-2"}); st != 422 || b["family"] != "AK-BAL" {
		t.Fatalf("a disposal whose gain does not close it: %d %v", st, b)
	}
	if st, r = body("26600.00", map[string]any{"VEHICLES": "18800.00", "EQUIPMENT": "7800.00"}); st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, r)
	}
}

func TestInventoryReconcilesByLocationAndRefusesNegativeStock(t *testing.T) {
	w := w4Setup(t)
	d := func(loc string) map[string]string { return map[string]string{"location": loc, "item_class": "FINISHED"} }
	w.mustPostD(t, "INVENTORY.RECEIVED", "", d("WH-1"), map[string]any{"amount": "5000.00", "receipt_ref": "GR-1", "valuation_policy": "fifo-v2"})
	w.mustPostD(t, "INVENTORY.RECEIVED", "", d("WH-2"), map[string]any{"amount": "3000.00", "receipt_ref": "GR-2", "valuation_policy": "fifo-v2"})
	w.mustPostD(t, "INVENTORY.ISSUED", "", d("WH-1"), map[string]any{"amount": "1200.00", "shipment_ref": "SH-1", "costing_layer_ref": "L-1", "on_hand_after": "40"})
	w.mustPostD(t, "INVENTORY.ADJUSTED_DOWN", "", d("WH-2"), map[string]any{"amount": "300.00", "count_ref": "C-1", "approval_ref": "A-1"})
	w.mustPostD(t, "INVENTORY.GRNI_CLEARED", "SUP-1", nil, map[string]any{"amount": "5000.00", "receipt_ref": "GR-1", "invoice_ref": "SI-1"})

	// negative stock needs an explicit policy: without one the issue is refused and nothing posts
	before := w.count(t, `SELECT count(*) FROM journal_headers`)
	if st, b := w.postD(t, "INVENTORY.ISSUED", "", d("WH-1"), map[string]any{"amount": "9999.00", "shipment_ref": "SH-9", "costing_layer_ref": "L-1", "on_hand_after": "-5"}); st != 422 || b["family"] != "AK-RULE" {
		t.Fatalf("%d %v", st, b)
	}
	if w.count(t, `SELECT count(*) FROM journal_headers`) != before {
		t.Fatal("a refused issue posts nothing")
	}
	body := func(total string, g map[string]any) (int, map[string]any) {
		return w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", map[string]any{"reconciliation_type": "INVENTORY_GL", "book_id": w.book, "fiscal_period": "2026-03", "as_of": "2026-03-31",
			"group_by": "location", "population_a": map[string]any{"source": "inventory-ledger", "reference": "valuation-mar", "total": total, "groups": g}})
	}
	// valuation by location: WH-1 5000 - 1200 = 3800; WH-2 3000 - 300 = 2700
	if st, r := body("6500.00", map[string]any{"WH-1": "3800.00", "WH-2": "2700.00"}); st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, r)
	}
	// the inventory ledger valued WH-2 at 3000 and WH-1 at 3500: same total, wrong locations
	if st, r := body("6500.00", map[string]any{"WH-1": "3500.00", "WH-2": "3000.00"}); st != 201 || r["status"] != "BREAK" {
		t.Fatalf("%d %v", st, r)
	}
	// goods received not invoiced: 8000 received, 5000 cleared by the supplier invoice
	if st, r := w.reconX("AP_GL", "5000.00", "ap-aging", nil); st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("the cleared GRNI is a payable of 5000 to SUP-1: %d %v", st, r)
	}
	if w.balance(t, "2070") != "-3000.00" {
		t.Fatalf("GRNI still holds the goods not yet invoiced: %s", w.balance(t, "2070"))
	}
}

func TestTaxResultsPostAgainstRegisteredObligationsAndReconcile(t *testing.T) {
	w := w4Setup(t)
	w.mustPostD(t, "TAX.PROVISION_CURRENT", "", nil, map[string]any{"amount": "9000.00", "provision_ref": "P-1", "provision_authority": "TAXDIR"})
	w.mustPostD(t, "TAX.PROVISION_DEFERRED", "", nil, map[string]any{"amount": "1200.00", "provision_ref": "P-1", "provision_authority": "TAXDIR"})
	w.mustPostD(t, "TAX.PAYMENT_MADE", "", nil, map[string]any{"amount": "2200.00", "obligation_ref": "OB-1", "tax_type": "VAT", "jurisdiction": "GB"})
	w.mustPostD(t, "TAX.RETURN_ADJUSTMENT_UP", "", nil, map[string]any{"amount": "80.00", "return_version": "r2", "approval_ref": "A-1", "period_ref": "2026-03"})
	if st, b := w.postD(t, "TAX.PAYMENT_MADE", "", nil, map[string]any{"amount": "1.00", "tax_type": "VAT", "jurisdiction": "GB"}); st != 422 || b["family"] != "AK-RULE" {
		t.Fatalf("a payment that names no obligation: %d %v", st, b)
	}
	// payable 9000 - 2200 + 80 = 6880, deferred 1200: the tax control accounts hold 8080
	if st, r := w.reconX("TAX_GL", "8080.00", "tax-ledger-mar", nil); st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, r)
	}
	if st, r := w.reconX("TAX_GL", "8000.00", "tax-ledger-bad", nil); st != 201 || r["status"] != "BREAK" || r["difference"] != "-80.000000" {
		t.Fatalf("%d %v", st, r)
	}
}

func TestSchedulePostingsNameTheirLineAndTheRemainingBalance(t *testing.T) {
	w := w4Setup(t)
	w.mustPostD(t, "SCHEDULE.PREPAYMENT_INVOICED", "SUP-1", nil, map[string]any{"amount": "1200.00", "schedule_ref": "S-2"})
	w.mustPostD(t, "SCHEDULE.PREPAYMENT_RECOGNIZED", "", nil, map[string]any{"amount": "100.00", "schedule_ref": "S-2", "schedule_line": "1", "remaining_balance": "1100.00"})
	w.mustPostD(t, "SCHEDULE.ACCRUAL_RECOGNIZED", "", nil, map[string]any{"amount": "500.00", "schedule_ref": "S-1", "schedule_line": "1", "remaining_balance": "0.00"})
	w.mustPostD(t, "SCHEDULE.ACCRUAL_REVERSED", "", nil, map[string]any{"amount": "500.00", "schedule_ref": "S-1", "schedule_line": "2", "reverses_line": "1"})
	if st, b := w.postD(t, "SCHEDULE.PREPAYMENT_RECOGNIZED", "", nil, map[string]any{"amount": "100.00", "schedule_ref": "S-2"}); st != 422 || b["family"] != "AK-RULE" {
		t.Fatalf("a recognition without its schedule line and remaining balance: %d %v", st, b)
	}
	if w.balance(t, "1200") != "1100.00" || w.balance(t, "2400") != "0.00" {
		t.Fatalf("prepaid %s accrued %s", w.balance(t, "1200"), w.balance(t, "2400"))
	}
}

// Subledger services still post through the legacy posting command, with the lines they computed and no book. The control accounts
// they post to stay open to that command until the operator moves them to events, and a reconciliation can count those entries.
func TestLegacySubledgerPostingsAreCountedWhenAskedFor(t *testing.T) {
	w := w4Setup(t)
	veh := map[string]string{"asset_class": "VEHICLES"}
	w.mustPostD(t, "ASSET.CAPITALIZED", "", veh, map[string]any{"amount": "24000.00", "asset_ref": "FA-1", "policy_version": "p1"})

	legacy := func(src string) map[string]any {
		return map[string]any{"legal_entity_id": w.entity, "fiscal_period": "2026-03", "description": "asset acquisition from the register", "source_event_id": src, "correlation_id": "c-" + src,
			"transaction_currency": "GBP", "document_date": "2026-03-12", "posting_date": "2026-03-12",
			"lines": []any{map[string]any{"account_code": "1500", "debit_amount": 500}, map[string]any{"account_code": "2060", "credit_amount": 500}}}
	}
	// the control accounts are restricted by default, so the legacy command is refused until the operator opens them
	if st, b := w.call("POST", "/v1/postings/events", w.tenant, "asset-svc", legacy("leg-1")); st < 400 {
		t.Fatalf("a control account is closed to the legacy command by default: %d %v", st, b)
	}
	for _, code := range []string{"1500", "2060"} {
		ct := "ASSET"
		if code == "2060" {
			ct = "CLEARING"
		}
		if st, b := w.call("PUT", "/v1/account-applicability/"+code, w.tenant, "chart-admin", map[string]any{"control_type": ct, "direct_posting_restricted": false}); st != 200 {
			t.Fatalf("%d %v", st, b)
		}
	}
	if st, b := w.call("POST", "/v1/postings/events", w.tenant, "asset-svc", legacy("leg-2")); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	// the book-scoped position does not see an entry that names no book; asking for unbooked entries brings it in
	if st, r := w.reconX("ASSET_GL", "24000.00", "register", nil); st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, r)
	}
	if st, r := w.reconX("ASSET_GL", "24000.00", "register", map[string]any{"include_unbooked": true}); st != 201 || r["status"] != "BREAK" || r["difference"] != "-500.000000" {
		t.Fatalf("%d %v", st, r)
	}
	st, r := w.reconX("ASSET_GL", "24500.00", "register", map[string]any{"include_unbooked": true})
	if st != 201 || r["status"] != "MATCHED" || r["population_b"].(map[string]any)["detail"].(map[string]any)["include_unbooked"] != true {
		t.Fatalf("%d %v", st, r)
	}
	if st, v := w.call("POST", "/v1/reconciliation-runs/"+r["reconciliation_run_id"].(string)+"/verify", w.tenant, "auditor", map[string]any{}); st != 200 || v["reproducible"] != true {
		t.Fatalf("a run that counted unbooked entries is reproduced with them: %d %v", st, v)
	}
}

func TestEverySubledgerPackIsListedAndGoverned(t *testing.T) {
	e := newKEnv(t)
	st, b := e.call("GET", "/v1/posting-rule-packs", e.tenant, "builder", nil)
	if st != 200 || len(b["packs"].([]any)) != 10 {
		t.Fatalf("%d %v", st, b)
	}
	seen := map[string]bool{}
	for _, p := range b["packs"].([]any) {
		seen[p.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"ar-standard", "ap-standard", "bank-standard", "payroll-standard", "assets-standard", "inventory-standard", "tax-standard", "schedules-standard", "payroll-results", "inventory-valuation"} {
		if !seen[n] {
			t.Fatalf("missing pack %s", n)
		}
	}
}
