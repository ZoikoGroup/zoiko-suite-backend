package store_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

// Wave 6 (allocations, jurisdiction extensions, multi-book regression) against real Postgres as the ordinary role.

// cost books a cost into an expense account: a balanced journal against the bank account, written the way a feed would.
func (w *w2) cost(t *testing.T, account, amount, day string) {
	t.Helper()
	journal := uuid.NewString()
	for i, l := range [][3]string{{account, amount, "0"}, {"1000", "0", amount}} {
		if err := w.exec(`INSERT INTO ledger_entries (ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period, journal_id, journal_line_id, line_number, account_code, debit_amount, credit_amount,
				currency_code, transaction_date, posting_date, correlation_id, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'STAT', $4, $5::uuid, $6::uuid, $7, $8, $9::numeric, $10::numeric, 'GBP', $11::date, $11::date, 'seed', now())`,
			uuid.NewString(), w.tenant, w.entity, day[:7], journal, uuid.NewString(), i+1, l[0], l[1], l[2], day); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *w2) allocSetup(t *testing.T) {
	t.Helper()
	for _, a := range [][2]string{{"6050", "EXPENSE"}, {"6060", "EXPENSE"}, {"6070", "EXPENSE"}, {"6101", "EXPENSE"}, {"6102", "EXPENSE"}, {"6103", "EXPENSE"}, {"6990", "EXPENSE"}} {
		if err := w.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`,
			uuid.NewString(), w.tenant, a[0], a[0]+" account", a[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,'ALLOCATION_ROUNDING','6990','2026-01-01','seed')`,
		uuid.NewString(), w.tenant); err != nil {
		t.Fatal(err)
	}
	if st, b := w.call("POST", "/v1/dimensions", w.tenant, "owner", map[string]any{"dimension_code": "cost_center", "name": "cost_center"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	for _, v := range []string{"CC-A", "CC-B", "CC-C"} {
		if st, b := w.call("POST", "/v1/dimensions/cost_center/values", w.tenant, "owner", map[string]any{"value_code": v, "name": v, "valid_from": "2026-01-01T00:00:00Z"}); st != 201 {
			t.Fatalf("%d %v", st, b)
		}
	}
}

func allocBody(code, source, driver string) map[string]any {
	rec := func(k, acct string) map[string]any {
		return map[string]any{"key": k, "account_code": acct, "dimensions": map[string]any{"cost_center": "CC-" + k}}
	}
	return map[string]any{"rule_code": code, "effective_from": "2026-01-01", "content": map[string]any{"name": code, "source_accounts": []string{source}, "driver": driver,
		"recipients": []any{rec("A", "6101"), rec("B", "6102"), rec("C", "6103")}}}
}

func (w *w2) activeAllocRule(t *testing.T, code, source string) string {
	t.Helper()
	st, r := w.call("POST", "/v1/allocation-rules", w.tenant, "author", allocBody(code, source, "headcount"))
	if st != 201 || r["status"] != "DRAFT" {
		t.Fatalf("%d %v", st, r)
	}
	id := r["allocation_rule_id"].(string)
	if st, r := w.call("POST", "/v1/allocation-rules/"+id+"/approve", w.tenant, "approver", nil); st != 200 || r["status"] != "ACTIVE" {
		t.Fatalf("%d %v", st, r)
	}
	return id
}

func (w *w2) allocate(rule string, drivers map[string]string, ref string) (int, map[string]any) {
	body := map[string]any{"allocation_rule_id": rule, "book_id": w.book, "fiscal_period": "2026-03", "as_of": "2026-03-31"}
	if drivers != nil {
		body["drivers"] = drivers
	}
	if ref != "" {
		body["driver_ref"] = ref
	}
	return w.call("POST", "/v1/allocation-runs", w.tenant, "close-runner", body)
}

func (w *w2) net(t *testing.T, account string) string {
	t.Helper()
	return w.scalar(t, `SELECT COALESCE(sum(debit_amount - credit_amount), 0)::text FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = 'STAT' AND account_code = $3`, w.tenant, w.entity, account)
}

func TestAllocationSpreadsThePoolBalancedWithAnExplicitRoundingLine(t *testing.T) {
	w := w2Setup(t)
	w.allocSetup(t)
	w.cost(t, "6050", "100.00", "2026-03-15")
	rule := w.activeAllocRule(t, "IT-COST", "6050")
	// a drafted rule is not usable, and its author cannot activate it
	st, d := w.call("POST", "/v1/allocation-rules", w.tenant, "author", allocBody("DRAFTED", "6050", "headcount"))
	if st != 201 {
		t.Fatalf("%d %v", st, d)
	}
	if st, b := w.allocate(d["allocation_rule_id"].(string), map[string]string{"A": "1", "B": "1", "C": "1"}, "hr"); st != 422 || b["code"] != "ALLOCATION_RULE_NOT_ACTIVE" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.call("POST", "/v1/allocation-rules/"+d["allocation_rule_id"].(string)+"/approve", w.tenant, "author", nil); st != 403 || b["error"] != "separation_of_duties" {
		t.Fatalf("%d %v", st, b)
	}

	// the pool needs a driver snapshot with a reference
	if st, b := w.allocate(rule, map[string]string{"A": "1", "B": "1", "C": "1"}, ""); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.allocate(rule, map[string]string{"A": "1", "B": "1"}, "hr"); st != 422 {
		t.Fatalf("every recipient needs a driver value: %d %v", st, b)
	}
	if st, b := w.allocate(rule, map[string]string{"A": "0", "B": "0", "C": "0"}, "hr"); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.allocate(rule, map[string]string{"A": "1", "B": "1", "C": "1", "Z": "1"}, "hr"); st != 422 {
		t.Fatalf("%d %v", st, b)
	}

	st, run := w.allocate(rule, map[string]string{"A": "1", "B": "1", "C": "1"}, "hr-2026-03")
	if st != 201 || run["pool_amount"] != "100.00" || run["status"] != "POSTED" || run["journal_number"] == "" {
		t.Fatalf("%d %v", st, run)
	}
	pop := run["population"].(map[string]any)
	if pop["rounding_residual"] != "0.01" || pop["allocated_total"] != "100.00" || pop["driver_ref"] != "hr-2026-03" || len(pop["driver_snapshot"].(map[string]any)) != 3 {
		t.Fatalf("%v", pop)
	}
	for _, s := range pop["shares"].([]any) {
		if s.(map[string]any)["amount"] != "33.33" {
			t.Fatalf("%v", s)
		}
	}
	if w.net(t, "6050") != "0.00" || w.net(t, "6101") != "33.33" || w.net(t, "6102") != "33.33" || w.net(t, "6103") != "33.33" || w.net(t, "6990") != "0.01" {
		t.Fatalf("the pool is relieved, the recipients carry it, the residual is on its own line: %s %s %s %s %s", w.net(t, "6050"), w.net(t, "6101"), w.net(t, "6102"), w.net(t, "6103"), w.net(t, "6990"))
	}
	if got := w.scalar(t, `SELECT dimensions->>'cost_center' FROM ledger_entries WHERE tenant_id = $1::uuid AND account_code = '6102' AND book_id = 'STAT'`, w.tenant); got != "CC-B" {
		t.Fatalf("the allocated cost carries the recipient dimension: %q", got)
	}
	if n := w.count(t, `SELECT count(*) FROM (SELECT journal_id FROM journal_lines WHERE tenant_id = $1::uuid GROUP BY 1 HAVING sum(debit_amount) <> sum(credit_amount)) x`, w.tenant); n != 0 {
		t.Fatalf("%d unbalanced journals", n)
	}
	if got := w.scalar(t, `SELECT journal_type || '/' || journal_purpose FROM journal_headers WHERE journal_id = $1::uuid`, run["journal_id"].(string)); got != "ALLOCATION/ALLOCATION" {
		t.Fatal(got)
	}

	// idempotent: the same rule, book and period is the same run
	if st, again := w.allocate(rule, map[string]string{"A": "9", "B": "1", "C": "1"}, "other"); st != 200 || again["allocation_run_id"] != run["allocation_run_id"] {
		t.Fatalf("%d %v", st, again)
	}
	// nothing left to allocate
	if st, b := w.call("POST", "/v1/allocation-runs", w.tenant, "close-runner", map[string]any{"allocation_rule_id": rule, "book_id": w.book, "fiscal_period": "2026-04", "drivers": map[string]string{"A": "1", "B": "1", "C": "1"}, "driver_ref": "hr"}); st != 422 || b["code"] != "ALLOCATION_NO_POOL" {
		t.Fatalf("%d %v", st, b)
	}

	// withdrawing it reverses the journal and frees the period; a sealed period refuses
	id := run["allocation_run_id"].(string)
	if st, b := w.call("POST", "/v1/allocation-runs/"+id+"/reverse", w.tenant, "close-runner", map[string]any{}); st != 422 {
		t.Fatalf("a reason is required: %d %v", st, b)
	}
	st, rev := w.call("POST", "/v1/allocation-runs/"+id+"/reverse", w.tenant, "close-runner", map[string]any{"reason": "wrong headcount"})
	if st != 200 || rev["status"] != "REVERSED" || rev["reversal_journal_number"] == "" {
		t.Fatalf("%d %v", st, rev)
	}
	if w.net(t, "6050") != "100.00" || w.net(t, "6101") != "0.00" || w.net(t, "6990") != "0.00" {
		t.Fatalf("the pool is back: %s", w.net(t, "6050"))
	}
	st, run2 := w.allocate(rule, map[string]string{"A": "2", "B": "1", "C": "1"}, "hr-corrected")
	if st != 201 || run2["allocation_run_id"] == id || w.net(t, "6101") != "50.00" {
		t.Fatalf("%d %v", st, run2)
	}
	w.seal("SEAL", "", "")
	if st, b := w.call("POST", "/v1/allocation-runs/"+run2["allocation_run_id"].(string)+"/reverse", w.tenant, "close-runner", map[string]any{"reason": "late"}); st != 409 {
		t.Fatalf("a sealed period refuses the reversal: %d %v", st, b)
	}
}

func TestAllocationResidualCanRunTheOtherWayAndRulesAreVersioned(t *testing.T) {
	w := w2Setup(t)
	w.allocSetup(t)
	w.cost(t, "6060", "50.00", "2026-03-10")
	rule := w.activeAllocRule(t, "FAC", "6060")
	// 50.00 over three equal drivers is 16.67 x 3 = 50.01: the rounding line is a credit
	st, run := w.allocate(rule, map[string]string{"A": "1", "B": "1", "C": "1"}, "ops")
	if st != 201 || run["population"].(map[string]any)["rounding_residual"] != "-0.01" || w.net(t, "6990") != "-0.01" {
		t.Fatalf("%d %v", st, run)
	}
	// a new version replaces the active one; the old version is kept and retired, and cannot run
	st, v2 := w.call("POST", "/v1/allocation-rules", w.tenant, "author", allocBody("FAC", "6060", "headcount"))
	if st != 201 || v2["version"].(float64) != 2 {
		t.Fatalf("%d %v", st, v2)
	}
	if st, b := w.call("POST", "/v1/allocation-rules/"+v2["allocation_rule_id"].(string)+"/approve", w.tenant, "approver", nil); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.call("GET", "/v1/allocation-rules/"+rule, w.tenant, "reader", nil); st != 200 || b["status"] != "RETIRED" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.call("GET", "/v1/allocation-rules?rule_code=FAC", w.tenant, "reader", nil); st != 200 || b["total"].(float64) != 2 {
		t.Fatalf("%d %v", st, b)
	}
	// a rule with a fixed split needs no driver, and its percentages must add up
	bad := allocBody("FIX", "6070", "FIXED")
	if st, b := w.call("POST", "/v1/allocation-rules", w.tenant, "author", bad); st != 422 {
		t.Fatalf("percentages are required under FIXED: %d %v", st, b)
	}
	good := allocBody("FIX", "6070", "FIXED")
	for i, p := range []string{"50", "30", "20"} {
		good["content"].(map[string]any)["recipients"].([]any)[i].(map[string]any)["percent"] = p
	}
	st, fx := w.call("POST", "/v1/allocation-rules", w.tenant, "author", good)
	if st != 201 {
		t.Fatalf("%d %v", st, fx)
	}
	w.call("POST", "/v1/allocation-rules/"+fx["allocation_rule_id"].(string)+"/approve", w.tenant, "approver", nil)
	w.cost(t, "6070", "200.00", "2026-03-11")
	if st, r := w.allocate(fx["allocation_rule_id"].(string), nil, ""); st != 201 || w.net(t, "6101") != "116.67" && w.net(t, "6101") != "100.00" {
		t.Fatalf("%d %v %s", st, r, w.net(t, "6101"))
	}
	// an invalid dimension value is refused before anything posts
	baddim := allocBody("DIM", "6070", "headcount")
	baddim["content"].(map[string]any)["recipients"].([]any)[0].(map[string]any)["dimensions"] = map[string]any{"cost_center": "CC-NOPE"}
	st, dr := w.call("POST", "/v1/allocation-rules", w.tenant, "author", baddim)
	if st != 201 {
		t.Fatalf("%d %v", st, dr)
	}
	w.call("POST", "/v1/allocation-rules/"+dr["allocation_rule_id"].(string)+"/approve", w.tenant, "approver", nil)
	w.cost(t, "6070", "10.00", "2026-03-12")
	if st, b := w.allocate(dr["allocation_rule_id"].(string), map[string]string{"A": "1", "B": "1", "C": "1"}, "x"); st != 422 || b["code"] != "DIMENSION_INVALID" {
		t.Fatalf("%d %v", st, b)
	}
}

func TestAllocationRunsOncePerRuleBookAndPeriodUnderConcurrency(t *testing.T) {
	w := w2Setup(t)
	w.allocSetup(t)
	w.cost(t, "6050", "300.00", "2026-03-15")
	rule := w.activeAllocRule(t, "CONC", "6050")
	var wg sync.WaitGroup
	statuses := make([]int, 6)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = w.allocate(rule, map[string]string{"A": "1", "B": "1", "C": "1"}, "hr")
		}(i)
	}
	wg.Wait()
	created := 0
	for _, s := range statuses {
		switch s {
		case 201:
			created++
		case 200:
		default:
			t.Fatalf("%v", statuses)
		}
	}
	if created != 1 || w.net(t, "6050") != "0.00" || w.net(t, "6101") != "100.00" {
		t.Fatalf("one allocation, once: %v %s", statuses, w.net(t, "6101"))
	}
}

func TestAllocationRecordsAreImmutableAuthorizedAndTenantIsolated(t *testing.T) {
	w := w2Setup(t)
	w.allocSetup(t)
	w.cost(t, "6050", "90.00", "2026-03-15")
	rule := w.activeAllocRule(t, "IMM", "6050")
	st, run := w.allocate(rule, map[string]string{"A": "1", "B": "1", "C": "1"}, "hr")
	if st != 201 {
		t.Fatalf("%d %v", st, run)
	}
	id := run["allocation_run_id"].(string)
	for what, sql := range map[string]string{
		"rewrite the population":   `UPDATE allocation_runs SET population = '{}'::jsonb WHERE allocation_run_id = $1::uuid`,
		"change the pool":          `UPDATE allocation_runs SET pool_amount = 1 WHERE allocation_run_id = $1::uuid`,
		"delete the run":           `DELETE FROM allocation_runs WHERE allocation_run_id = $1::uuid`,
		"reverse without a reason": `UPDATE allocation_runs SET status = 'REVERSED', reversed_by = 'x' WHERE allocation_run_id = $1::uuid`,
		"rewrite the rule":         `UPDATE allocation_rules SET content = '{}'::jsonb WHERE allocation_rule_id = $1::uuid`,
		"delete the rule":          `DELETE FROM allocation_rules WHERE allocation_rule_id = $1::uuid`,
	} {
		arg := id
		if sql[7:23] == "allocation_rules" || sql[:6] == "DELETE" && sql[12:28] == "allocation_rules" {
			arg = rule
		}
		wantRefused(t, what, w.asApp(w.tenant, sql, arg), "")
	}
	other := uuid.NewString()
	if w.countAs(other, `SELECT count(*) FROM allocation_runs`) != 0 || w.countAs(other, `SELECT count(*) FROM allocation_rules`) != 0 {
		t.Fatal("another tenant sees allocations")
	}
	if st, _ := w.call("GET", "/v1/allocation-runs/"+id, other, "reader", nil); st != 404 {
		t.Fatalf("%d", st)
	}
	w.denied.Store("analyst|GL_ALLOCATION_RUN", true)
	w.denied.Store("analyst|GL_ALLOCATION_MANAGE", true)
	w.denied.Store("author|GL_ALLOCATION_APPROVE", true)
	if st, _ := w.call("POST", "/v1/allocation-runs", w.tenant, "analyst", map[string]any{}); st != 403 {
		t.Fatalf("%d", st)
	}
	if st, _ := w.call("POST", "/v1/allocation-rules", w.tenant, "analyst", map[string]any{}); st != 403 {
		t.Fatalf("%d", st)
	}
	if st, _ := w.call("POST", "/v1/allocation-rules/"+rule+"/retire", w.tenant, "author", nil); st != 403 {
		t.Fatalf("%d", st)
	}
}

// ── jurisdiction extensions and the multi-book regression ─────────────────────────────────────────────────────────────

func (w *w2) deBook(t *testing.T) string {
	t.Helper()
	b := bookBody(w.entity, "STAT-DE")
	b["jurisdiction"] = "DE"
	st, r := w.call("POST", "/v1/accounting-books", w.tenant, "owner", b)
	if st != 201 || r["jurisdiction"] != "DE" {
		t.Fatalf("%d %v", st, r)
	}
	id := r["book_id"].(string)
	if st, r = w.call("POST", "/v1/accounting-books/"+id+"/activate", w.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, r)
	}
	return id
}

// deInvoiceRule is the standard invoice rule as a German extension that books revenue to 4100.
func deInvoiceRule(t *testing.T, name string) map[string]any {
	t.Helper()
	pack, _ := kernel.PackByName("ar-standard")
	for _, d := range pack.Drafts("1.0.0", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "PRIMARY_STATUTORY", "IFRS") {
		if d.EventType != "AR.INVOICE_ISSUED" {
			continue
		}
		d.Name, d.Jurisdiction = name, "DE"
		for i, lt := range d.Content.LineTemplates {
			if lt.Account.MappingKey == "REVENUE" {
				d.Content.LineTemplates[i].Account.MappingKey = "REVENUE_DE"
			}
		}
		for i := range d.TestPack {
			acc := map[string]string{}
			for k, v := range d.TestPack[i].Accounts {
				acc[k] = v
			}
			acc["REVENUE_DE"] = "4100"
			d.TestPack[i].Accounts = acc
			lines := append([]kernel.ExpectedLine(nil), d.TestPack[i].Expect.Lines...)
			for j := range lines {
				if lines[j].AccountCode == "4000" {
					lines[j].AccountCode = "4100"
				}
			}
			d.TestPack[i].Expect.Lines = lines
		}
		raw, _ := json.Marshal(d)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		return body
	}
	t.Fatal("the pack has no invoice rule")
	return nil
}

func TestJurisdictionExtensionOverridesTheGlobalRuleOnlyInBooksOfThatJurisdiction(t *testing.T) {
	w := w2Setup(t)
	if err := w.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,'REVENUE_DE','4100','2026-01-01','seed')`, uuid.NewString(), w.tenant); err != nil {
		t.Fatal(err)
	}
	// a jurisdiction is a controlled code, on a book and on a rule set, and it never changes
	bad := bookBody(w.entity, "STAT-BAD")
	bad["jurisdiction"] = "germany"
	if st, b := w.call("POST", "/v1/accounting-books", w.tenant, "owner", bad); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	deBook := w.deBook(t)
	wantRefused(t, "change the jurisdiction of a book", w.asApp(w.tenant, `UPDATE accounting_books SET jurisdiction = 'FR', version = version + 1 WHERE book_id = $1::uuid`, deBook), "")

	// before any extension both books follow the global rule
	ev1 := w.eventAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-03-05", inv2("120.00", "100.00", "20.00"))
	for _, b := range []string{w.book, deBook} {
		if st, r := w.kenv.post(ev1, b, nil); st != 201 {
			t.Fatalf("%d %v", st, r)
		}
	}
	st, cmp := w.call("GET", "/v1/book-comparison?accounting_event_id="+ev1, w.tenant, "auditor", nil)
	if st != 200 || cmp["single_source_record"] != true || len(cmp["books"].([]any)) != 2 || len(cmp["differences"].([]any)) != 0 {
		t.Fatalf("%d %v", st, cmp)
	}

	// the extension: authored, tested against a book of its own jurisdiction only, approved and activated by others
	body := deInvoiceRule(t, "ar-invoice-issued-de")
	bad2 := deInvoiceRule(t, "ar-invoice-issued-xx")
	bad2["jurisdiction"] = "de"
	if st, b := w.call("POST", "/v1/posting-rule-sets", w.tenant, "builder", bad2); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	st, rs := w.call("POST", "/v1/posting-rule-sets", w.tenant, "builder", body)
	if st != 201 || rs["jurisdiction"] != "DE" {
		t.Fatalf("%d %v", st, rs)
	}
	id := rs["posting_rule_set_id"].(string)
	if st, b := w.call("POST", "/v1/posting-rule-sets/"+id+"/test", w.tenant, "builder", map[string]any{"book_id": w.book}); st != 422 {
		t.Fatalf("a German extension is not tested against a global book: %d %v", st, b)
	}
	for _, step := range [][2]string{{"test", "builder"}, {"approve", "approver"}, {"activate", "releaser"}} {
		if st, b := w.call("POST", "/v1/posting-rule-sets/"+id+"/"+step[0], w.tenant, step[1], map[string]any{"book_id": deBook}); st != 200 {
			t.Fatalf("%s: %d %v", step[0], st, b)
		}
	}
	wantRefused(t, "change the jurisdiction of a rule set", w.exec(`UPDATE posting_rule_sets SET jurisdiction = 'FR', row_version = row_version + 1 WHERE posting_rule_set_id = $1::uuid`, id), "")
	// a second active German extension for the same event is an overlap; the global rule and the extension coexist
	rs2 := deInvoiceRule(t, "ar-invoice-issued-de-b")
	st, second := w.call("POST", "/v1/posting-rule-sets", w.tenant, "builder", rs2)
	if st != 201 {
		t.Fatalf("%d %v", st, second)
	}
	id2 := second["posting_rule_set_id"].(string)
	for _, step := range [][2]string{{"test", "builder"}, {"approve", "approver"}} {
		if st, b := w.call("POST", "/v1/posting-rule-sets/"+id2+"/"+step[0], w.tenant, step[1], map[string]any{"book_id": deBook}); st != 200 {
			t.Fatalf("%s: %d %v", step[0], st, b)
		}
	}
	if st, b := w.call("POST", "/v1/posting-rule-sets/"+id2+"/activate", w.tenant, "releaser", map[string]any{"book_id": deBook}); st != 409 {
		t.Fatalf("two active extensions of one jurisdiction overlap: %d %v", st, b)
	}

	// a new event: the same source record, a different journal set in each book
	ev2 := w.eventAt(t, "AR.INVOICE_ISSUED", "CUST-2", "GBP", "2026-03-20", inv2("240.00", "200.00", "40.00"))
	for _, b := range []string{w.book, deBook} {
		if st, r := w.kenv.post(ev2, b, nil); st != 201 {
			t.Fatalf("%d %v", st, r)
		}
	}
	st, cmp = w.call("GET", "/v1/book-comparison?accounting_event_id="+ev2, w.tenant, "auditor", nil)
	if st != 200 || cmp["single_source_record"] != true {
		t.Fatalf("%d %v", st, cmp)
	}
	diffs := cmp["differences"].([]any)
	if len(diffs) != 2 {
		t.Fatalf("revenue is in 4000 in the global book and 4100 in the German book: %v", diffs)
	}
	for _, d := range diffs {
		m := d.(map[string]any)
		by := m["net_by_book"].(map[string]any)
		switch m["account_code"] {
		case "4000":
			if by["STAT"] != "-200.00" || by["STAT-DE"] != "0.00" {
				t.Fatalf("%v", m)
			}
		case "4100":
			if by["STAT-DE"] != "-200.00" || by["STAT"] != "0.00" {
				t.Fatalf("%v", m)
			}
		default:
			t.Fatalf("%v", m)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM accounting_events WHERE tenant_id = $1::uuid`, w.tenant); n != 2 {
		t.Fatalf("the source record exists once per event, whatever the number of books: %d", n)
	}

	// the multi-book regression: every book balances on its own, reconciles on its own and closes on its own
	for _, b := range []string{"STAT", "STAT-DE"} {
		if got := w.scalar(t, `SELECT (sum(debit_amount) = sum(credit_amount))::text FROM ledger_entries WHERE tenant_id = $1::uuid AND book_id = $2`, w.tenant, b); got != "true" {
			t.Fatalf("book %s does not balance", b)
		}
	}
	for _, book := range []string{w.book, deBook} {
		st, r := w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", map[string]any{"reconciliation_type": "AR_GL", "book_id": book, "fiscal_period": "2026-03", "as_of": "2026-03-31",
			"population_a": map[string]any{"source": "subledger", "reference": "ar-" + book, "total": "360.00"}})
		if st != 201 || r["status"] != "MATCHED" {
			t.Fatalf("each book reconciles to the same receivable: %d %v", st, r)
		}
		if st, s := w.call("POST", "/v1/balance-snapshots", w.tenant, "close-svc", map[string]any{"book_id": book, "fiscal_period": "2026-03", "purpose": "AD_HOC"}); st != 201 {
			t.Fatalf("%d %v", st, s)
		}
	}
}
