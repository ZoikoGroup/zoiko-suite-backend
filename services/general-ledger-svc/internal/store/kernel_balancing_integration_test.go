package store_test

import (
	"testing"

	"github.com/google/uuid"
)

// §13: where a rule designates a balancing dimension, the posted journal balances within each value of it, through explicit due-to and
// due-from lines, and stays that way when it is reversed.
func TestJournalBalancesWithinTheBalancingDimension(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	book := e.activeBook(t, "STAT")
	for _, a := range [][2]string{{"2900", "LIABILITY"}, {"1900", "ASSET"}} {
		if err := e.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`,
			uuid.NewString(), e.tenant, a[0], a[0], a[1]); err != nil {
			t.Fatal(err)
		}
	}
	for k, c := range map[string]string{"DUE_TO": "2900", "DUE_FROM": "1900"} {
		if err := e.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,$3,$4,'2026-01-01','seed')`,
			uuid.NewString(), e.tenant, k, c); err != nil {
			t.Fatal(err)
		}
	}
	if st, b := e.call("POST", "/v1/dimensions", e.tenant, "owner", map[string]any{"dimension_code": "unit", "name": "Business unit"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	for _, v := range []string{"A", "B"} {
		if st, b := e.call("POST", "/v1/dimensions/unit/values", e.tenant, "owner", map[string]any{"value_code": v, "name": v, "valid_from": "2026-01-01T00:00:00Z"}); st != 201 {
			t.Fatalf("%d %v", st, b)
		}
	}

	// the receivable is booked in unit A and the revenue and tax in unit B, so neither unit balances by itself
	r := invoiceRule("1.0.0")
	content := r["content"].(map[string]any)
	lt := content["line_templates"].([]any)
	lt[0].(map[string]any)["dimensions"] = map[string]any{"unit": "A"}
	lt[1].(map[string]any)["dimensions"] = map[string]any{"unit": "B"}
	lt[2].(map[string]any)["dimensions"] = map[string]any{"unit": "B"}
	content["balancing_dimension"] = map[string]any{"dimension": "unit", "due_to_account": map[string]any{"mapping_key": "DUE_TO"}, "due_from_account": map[string]any{"mapping_key": "DUE_FROM"}}
	pack := r["test_pack"].([]any)[0].(map[string]any)
	pack["accounts"].(map[string]any)["DUE_TO"], pack["accounts"].(map[string]any)["DUE_FROM"] = "2900", "1900"
	pack["expect"] = map[string]any{"lines": []any{
		map[string]any{"side": "DEBIT", "account_code": "1100", "amount": "120.00"}, map[string]any{"side": "CREDIT", "account_code": "4000", "amount": "100.00"},
		map[string]any{"side": "CREDIT", "account_code": "2200", "amount": "20.00"},
		map[string]any{"side": "CREDIT", "account_code": "2900", "amount": "120.00"}, map[string]any{"side": "DEBIT", "account_code": "1900", "amount": "120.00"}}}
	e.activate(t, r, book)

	res := e.mustPost(t, e.registerEvent(t, "se-bal", "120.00", "100.00", "20.00"), book)
	jid := res["journal_id"].(string)
	if n := e.count(t, `SELECT count(*) FROM journal_lines WHERE journal_id=$1::uuid`, jid); n != 5 {
		t.Fatalf("the three lines of the rule and the two that balance the units: %d", n)
	}
	line := func(ref, col string) string {
		return e.scalar(t, `SELECT `+col+` FROM journal_lines WHERE journal_id=$1::uuid AND posting_rule_line_ref=$2`, jid, ref)
	}
	if line("DUE_TO", "account_code") != "2900" || line("DUE_TO", "credit_amount::text") != "120.00" || line("DUE_TO", "dimensions->>'unit'") != "A" ||
		line("DUE_FROM", "account_code") != "1900" || line("DUE_FROM", "debit_amount::text") != "120.00" || line("DUE_FROM", "dimensions->>'unit'") != "B" {
		t.Fatal("unit A owes the other unit and unit B is owed by it, each line in its own unit")
	}
	// every unit balances by itself, in the journal and in the ledger
	imbalanced := func(table string) int {
		return e.count(t, `SELECT count(*) FROM (SELECT dimensions->>'unit' u FROM `+table+` WHERE journal_id=$1::uuid GROUP BY 1 HAVING sum(debit_amount) <> sum(credit_amount)) x`, jid)
	}
	if imbalanced("journal_lines") != 0 || imbalanced("ledger_entries") != 0 {
		t.Fatal("each unit balances by itself")
	}
	if e.scalar(t, `SELECT (calculation_trace::text LIKE '%balanced within each value of unit%')::text FROM posting_executions WHERE journal_id=$1::uuid`, jid) != "true" {
		t.Fatal("the execution's evidence says what was added")
	}

	// a reversal is balanced by unit as well
	st, rev := e.call("POST", "/v1/journals/"+jid+"/reverse", e.tenant, "reviewer", map[string]any{"reason": "wrong customer", "correlation_id": "rev-bal", "correction_kind": "ERROR_CORRECTION"})
	if st != 201 {
		t.Fatalf("%d %v", st, rev)
	}
	rid := rev["journal_id"].(string)
	if n := e.count(t, `SELECT count(*) FROM (SELECT dimensions->>'unit' u FROM journal_lines WHERE journal_id=$1::uuid GROUP BY 1 HAVING sum(debit_amount) <> sum(credit_amount)) x`, rid); n != 0 {
		t.Fatalf("the reversal keeps every unit balanced: %d", n)
	}
	// and the net effect of the journal and its reversal is nothing, unit by unit
	if n := e.count(t, `SELECT count(*) FROM (SELECT dimensions->>'unit' u, account_code FROM ledger_entries GROUP BY 1, 2 HAVING sum(debit_amount) <> sum(credit_amount)) x`); n != 0 {
		t.Fatalf("%d account and unit positions are not back to nothing", n)
	}
}
