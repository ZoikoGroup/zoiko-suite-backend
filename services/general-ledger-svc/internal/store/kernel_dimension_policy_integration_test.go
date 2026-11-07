package store_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// §12, §13: an account says which dimensions its lines must, may or must not carry, rules can forbid or require combinations of
// dimensions, and a retired account maps forward to its replacement without rewriting history.
func TestAccountDimensionPolicyCombinationsAndReplacement(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	book := e.activeBook(t, "STAT")

	for _, d := range []string{"cost_center", "fund", "project"} {
		if st, b := e.call("POST", "/v1/dimensions", e.tenant, "owner", map[string]any{"dimension_code": d, "name": d}); st != 201 {
			t.Fatalf("%d %v", st, b)
		}
	}
	for d, vals := range map[string][]string{"cost_center": {"CC1", "CC2"}, "fund": {"F1"}, "project": {"P1"}} {
		for _, v := range vals {
			if st, b := e.call("POST", "/v1/dimensions/"+d+"/values", e.tenant, "owner", map[string]any{"value_code": v, "name": v, "valid_from": "2026-01-01T00:00:00Z"}); st != 201 {
				t.Fatalf("%d %v", st, b)
			}
		}
	}

	// the revenue line carries the event's dimensions; where the cost centre is CC1 a project is needed too
	r := invoiceRule("1.0.0")
	content := r["content"].(map[string]any)
	content["line_templates"].([]any)[1].(map[string]any)["dimensions"] = map[string]any{
		"cost_center": "$dimensions.cost_center", "fund": "$dimensions.fund", "project": "$dimensions.project"}
	content["dimension_combinations"] = []any{map[string]any{"when": map[string]any{"dimension": "cost_center", "value": "CC1"}, "require": []any{"project"}}}
	e.activate(t, r, book)

	// the account's own policy, set after the rule is active: the cost centre is required and the fund is not allowed
	scope := func(body map[string]any) (int, map[string]any) {
		return e.call("PUT", "/v1/account-applicability/4000", e.tenant, "chart-admin", body)
	}
	if st, b := scope(map[string]any{"dimension_policy": map[string]any{"cost_center": "MAYBE"}}); st != 422 || b["code"] != "SCOPE_INVALID" {
		t.Fatalf("an unknown mode: %d %v", st, b)
	}
	if st, b := scope(map[string]any{"replacement_account": "4000"}); st != 422 {
		t.Fatalf("an account cannot replace itself: %d %v", st, b)
	}
	if st, b := scope(map[string]any{"replacement_account": "9999"}); st != 422 || b["code"] != "REPLACEMENT_INVALID" {
		t.Fatalf("an unknown replacement: %d %v", st, b)
	}
	policy := map[string]any{"cost_center": "REQUIRED", "fund": "PROHIBITED"}
	if st, b := scope(map[string]any{"dimension_policy": policy}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	st, got := e.call("GET", "/v1/account-applicability/4000", e.tenant, "auditor", nil)
	if st != 200 || got["dimension_policy"].(map[string]any)["cost_center"] != "REQUIRED" || got["dimension_policy"].(map[string]any)["fund"] != "PROHIBITED" {
		t.Fatalf("%d %v", st, got)
	}

	dimEvent := func(src string, dims map[string]any) string {
		t.Helper()
		body := eventBody(e.entity, src, "120.00", "100.00", "20.00")
		if dims != nil {
			body["dimensions"] = dims
		}
		st, ev := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", body)
		if st != 201 {
			t.Fatalf("register %s: %d %v", src, st, ev)
		}
		return ev["accounting_event_id"].(string)
	}
	refused := func(src string, dims map[string]any, code string) {
		t.Helper()
		ev := dimEvent(src, dims)
		if st, b := e.post(ev, book, nil); st != 422 || b["code"] != code || b["family"] != "AK-DIM" {
			t.Fatalf("%s: want %s, got %d %v", src, code, st, b)
		}
		if n := e.count(t, `SELECT count(*) FROM journal_headers WHERE source_accounting_event_id=$1::uuid`, ev); n != 0 {
			t.Fatalf("%s: a refused posting leaves no journal, found %d", src, n)
		}
	}
	refused("d-none", nil, "DIMENSION_MISSING")                                                   // the account requires the cost centre
	refused("d-fund", map[string]any{"cost_center": "CC2", "fund": "F1"}, "DIMENSION_PROHIBITED") // the account prohibits the fund
	refused("d-combo", map[string]any{"cost_center": "CC1"}, "DIMENSION_COMBINATION_REQUIRED")    // the rule: CC1 needs a project
	ok := e.mustPost(t, dimEvent("d-ok", map[string]any{"cost_center": "CC1", "project": "P1"}), book)
	if got := e.scalar(t, `SELECT dimensions->>'project' FROM journal_lines WHERE journal_id=$1::uuid AND account_code='4000'`, ok["journal_id"].(string)); got != "P1" {
		t.Fatalf("the line carries its dimensions: %q", got)
	}
	e.mustPost(t, dimEvent("d-cc2", map[string]any{"cost_center": "CC2"}), book) // the combination is about CC1 only

	// manual journals answer to the account's policy too
	manual := func(corr string, dims map[string]any) (int, map[string]any) {
		line := map[string]any{"account_code": "4000", "credit_amount": 50.00}
		if dims != nil {
			line["dimensions"] = dims
		}
		return e.call("POST", "/v1/journals", e.tenant, "preparer", map[string]any{
			"tenant_id": e.tenant, "legal_entity_id": e.entity, "fiscal_period": "2026-03", "description": "manual", "correlation_id": corr, "journal_type": "STANDARD",
			"transaction_date": "2026-03-10", "posting_date": "2026-03-10", "currency_code": "GBP",
			"lines": []any{map[string]any{"account_code": "1100", "debit_amount": 50.00}, line}})
	}
	if st, b := manual("m-none", nil); st != 422 || b["error"] != "dimension_required" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := manual("m-fund", map[string]any{"cost_center": "CC2", "fund": "F1"}); st != 422 || b["error"] != "dimension_prohibited" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := manual("m-ok", map[string]any{"cost_center": "CC2"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}

	// the revenue account is retired. Until it names a replacement the posting is refused, as before.
	if err := e.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,'4100','4100','REVENUE','seed')`,
		uuid.NewString(), e.tenant); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(`UPDATE chart_of_accounts SET status='INACTIVE' WHERE tenant_id=$1::uuid AND account_code='4000'`, e.tenant); err != nil {
		t.Fatal(err)
	}
	later := dimEvent("d-later", map[string]any{"cost_center": "CC2"})
	if st, b := e.post(later, book, nil); st != 422 || b["family"] != "AK-COA" {
		t.Fatalf("a retired account without a replacement: %d %v", st, b)
	}
	// a replacement has to be open, and an account that is itself retired will not do
	if st, b := scope(map[string]any{"dimension_policy": policy, "replacement_account": "4000"}); st != 422 {
		t.Fatalf("itself: %d %v", st, b)
	}
	if err := e.exec(`UPDATE chart_of_accounts SET status='INACTIVE' WHERE tenant_id=$1::uuid AND account_code='4100'`, e.tenant); err != nil {
		t.Fatal(err)
	}
	if st, b := scope(map[string]any{"dimension_policy": policy, "replacement_account": "4100"}); st != 422 || b["code"] != "REPLACEMENT_INVALID" {
		t.Fatalf("a retired replacement: %d %v", st, b)
	}
	if err := e.exec(`UPDATE chart_of_accounts SET status='ACTIVE' WHERE tenant_id=$1::uuid AND account_code='4100'`, e.tenant); err != nil {
		t.Fatal(err)
	}
	if st, b := scope(map[string]any{"dimension_policy": policy, "replacement_account": "4100"}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if st, g := e.call("GET", "/v1/account-applicability/4000", e.tenant, "auditor", nil); st != 200 || g["replacement_account"] != "4100" {
		t.Fatalf("%d %v", st, g)
	}

	// now the posting lands on the replacement, which has its own (no) policy, and the evidence says it was mapped forward
	moved := e.mustPost(t, later, book)
	mj := moved["journal_id"].(string)
	if e.scalar(t, `SELECT count(*)::text FROM journal_lines WHERE journal_id=$1::uuid AND account_code='4100' AND credit_amount=100`, mj) != "1" ||
		e.scalar(t, `SELECT count(*)::text FROM journal_lines WHERE journal_id=$1::uuid AND account_code='4000'`, mj) != "0" {
		t.Fatal("revenue lands on the replacement account")
	}
	if tr := e.scalar(t, `SELECT calculation_trace::text FROM posting_executions WHERE journal_id=$1::uuid`, mj); !strings.Contains(tr, "retired and maps forward to 4100") {
		t.Fatalf("the execution's evidence says so: %s", tr)
	}
	// history is not rewritten: the journal posted to the account before it was retired still says 4000
	if e.scalar(t, `SELECT count(*)::text FROM journal_lines WHERE journal_id=$1::uuid AND account_code='4000'`, ok["journal_id"].(string)) != "1" {
		t.Fatal("an earlier posting keeps the account it was made to")
	}
	// the scope change is evented
	if e.outboxCount(t, "com.zoikosuite.accounting.kernel.account.scope-changed") != 2 {
		t.Fatal("account scope changes are raised on the outbox")
	}
}
