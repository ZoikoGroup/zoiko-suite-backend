package store_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// §27.1: intercompany reciprocal balances are reconciled between the two entities' own ledgers; a one-sided item is a break.
func TestIntercompanyReciprocalBalancesReconcile(t *testing.T) {
	c := crossSetup(t)
	c.mapIC(t)
	c.cost(t, "6050", "100.00", "2026-03-15")
	rule := c.crossRule(t, "X-REC", map[string]any{"legal_entity_id": c.entity2, "book_id": c.book2})
	if st, run := c.runRule(rule); st != 201 {
		t.Fatalf("%d %v", st, run)
	}
	ic := func(extra map[string]any) (int, map[string]any) {
		body := map[string]any{"reconciliation_type": "INTERCOMPANY_GL", "book_id": c.book, "counterparty_book_id": c.book2, "fiscal_period": "2026-03", "as_of": "2026-03-31"}
		for k, v := range extra {
			body[k] = v
		}
		return c.call("POST", "/v1/reconciliation-runs", c.tenant, "recon-runner", body)
	}

	// the allocation left a receivable of 50.00 in one entity and a payable of 50.00 in the other: they cancel
	st, run := ic(nil)
	if st != 201 || run["status"] != "MATCHED" || run["expected_total"] != "50.000000" || run["actual_total"] != "50.000000" || run["difference"] != "0.000000" {
		t.Fatalf("%d %v", st, run)
	}
	pa, pb := run["population_a"].(map[string]any), run["population_b"].(map[string]any)
	if pa["source"] != "LEDGER_INTERCOMPANY" || pa["detail"].(map[string]any)["counterparty_entity_id"] != c.entity2 || pb["detail"].(map[string]any)["counterparty_entity_id"] != c.entity ||
		pa["watermark"] == "" || pb["watermark"] == "" {
		t.Fatalf("each side names its entity, its counterparty and the watermark it read to: %v", run)
	}
	if st, v := c.call("POST", "/v1/reconciliation-runs/"+run["reconciliation_run_id"].(string)+"/verify", c.tenant, "auditor", map[string]any{}); st != 200 || v["reproducible"] != true {
		t.Fatalf("%d %v", st, v)
	}

	// a receivable booked in one entity only: the other entity has nothing to match it
	jid := c.mustCreate(t, map[string]any{
		"tenant_id": c.tenant, "legal_entity_id": c.entity, "fiscal_period": "2026-03", "description": "recharge not yet booked by the other entity", "correlation_id": "ic-one-sided",
		"journal_type": "STANDARD", "transaction_date": "2026-03-20", "posting_date": "2026-03-20", "currency_code": "GBP", "override_control_account_restriction": true, "book_id": "STAT",
		"lines": []any{map[string]any{"account_code": "1800", "debit_amount": 10.00, "counterparty_id": c.entity2}, map[string]any{"account_code": "4000", "credit_amount": 10.00}}})
	if st, b := c.call("POST", "/v1/journals/"+jid+"/validate", c.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := c.call("POST", "/v1/journals/"+jid+"/post", c.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	st, brk := ic(nil)
	if st != 201 || brk["status"] != "BREAK" || brk["expected_total"] != "60.000000" || brk["actual_total"] != "50.000000" || brk["difference"] != "10.000000" || brk["owner"] == "" || brk["sla_due_at"] == nil {
		t.Fatalf("a one-sided item is a break with an owner and an SLA: %d %v", st, brk)
	}
	if c.outboxCount(t, "com.zoikosuite.accounting.kernel.reconciliation.break-raised") != 1 {
		t.Fatal("the break is raised on the outbox")
	}
	// before the item was booked the pair still matched: the as-of date is honoured, and the break is still reproducible
	if st, early := ic(map[string]any{"as_of": "2026-03-19"}); st != 201 || early["status"] != "MATCHED" {
		t.Fatalf("as of the day before: %d %v", st, early)
	}
	if st, v := c.call("POST", "/v1/reconciliation-runs/"+brk["reconciliation_run_id"].(string)+"/verify", c.tenant, "auditor", map[string]any{}); st != 200 || v["reproducible"] != true {
		t.Fatalf("%d %v", st, v)
	}

	// a tolerance makes the same difference a warning, like any other type
	if st, b := c.call("POST", "/v1/reconciliation-tolerances", c.tenant, "tolerance-admin", map[string]any{"reconciliation_type": "INTERCOMPANY_GL", "match_amount": "0", "warning_amount": "15", "effective_from": "2026-01-01"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if st, w := ic(nil); st != 201 || w["status"] != "WARNING" {
		t.Fatalf("within the warning tolerance: %d %v", st, w)
	}

	// refusals: the counterparty book is required, belongs to another entity and is in the same currency
	if st, b := ic(map[string]any{"counterparty_book_id": ""}); st != 422 {
		t.Fatalf("no counterparty book: %d %v", st, b)
	}
	if st, b := ic(map[string]any{"counterparty_book_id": c.book}); st != 422 {
		t.Fatalf("the same entity: %d %v", st, b)
	}
	usd := bookBody(uuid.NewString(), "STAT")
	usd["functional_currency"] = "USD"
	st, ub := c.call("POST", "/v1/accounting-books", c.tenant, "owner", usd)
	if st != 201 {
		t.Fatalf("%d %v", st, ub)
	}
	if st, b := ic(map[string]any{"counterparty_book_id": ub["book_id"]}); st != 422 || !strings.Contains(b["error"].(string), "recon_currency") {
		t.Fatalf("books in different currencies are not compared: %d %v", st, b)
	}
}

// §27.1: the ledger is reconciled against the signed snapshot a report is made from.
func TestLedgerReconcilesAgainstItsSignedSnapshot(t *testing.T) {
	w := w2Setup(t)
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-03-05", inv2("120.00", "100.00", "20.00"))
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-2", "GBP", "2026-03-15", inv2("60.00", "50.00", "10.00"))
	st, snap := w.snapshot("AD_HOC")
	if st != 201 {
		t.Fatalf("%d %v", st, snap)
	}
	id := snap["snapshot_id"].(string)
	run := func(snapshot string) (int, map[string]any) {
		return w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", map[string]any{"reconciliation_type": "LEDGER_SNAPSHOT", "snapshot_id": snapshot})
	}

	st, r := run(id)
	if st != 201 || r["status"] != "MATCHED" || r["difference"] != "0.000000" || r["expected_total"] != r["actual_total"] || r["accounting_book_id"] != w.book || r["as_of"] != snap["as_of"] {
		t.Fatalf("a fresh signed snapshot reconciles to the ledger, in the scope the snapshot fixes: %d %v", st, r)
	}
	pa := r["population_a"].(map[string]any)
	if pa["source"] != "SIGNED_SNAPSHOT" || pa["reference"] != id || pa["detail"].(map[string]any)["signature_valid"] != true {
		t.Fatalf("%v", pa)
	}
	// a posting after the snapshot does not disturb it: the ledger is read at the snapshot's watermark
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-3", "GBP", "2026-03-25", inv2("24.00", "20.00", "4.00"))
	if st, again := run(id); st != 201 || again["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, again)
	}
	if st, v := w.call("POST", "/v1/reconciliation-runs/"+r["reconciliation_run_id"].(string)+"/verify", w.tenant, "auditor", map[string]any{}); st != 200 || v["reproducible"] != true {
		t.Fatalf("%d %v", st, v)
	}

	// refusals
	if st, _ := run(""); st != 422 {
		t.Fatalf("no snapshot: %d", st)
	}
	if st, _ := run(uuid.NewString()); st != 422 {
		t.Fatalf("an unknown snapshot: %d", st)
	}
	if st, _ := w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", map[string]any{"reconciliation_type": "LEDGER_SNAPSHOT", "snapshot_id": id, "book_id": uuid.NewString()}); st != 422 {
		t.Fatalf("a book the snapshot is not of: %d", st)
	}

	// a snapshot whose content was altered after it was signed is a break, found per account
	if err := w.exec(`ALTER TABLE balance_snapshots DISABLE TRIGGER balance_snapshots_no_change`); err != nil {
		t.Fatal(err)
	}
	if err := w.exec(`UPDATE balance_snapshots SET lines = replace(lines::text, '"debit_total": "180.00"', '"debit_total": "181.00"')::jsonb WHERE snapshot_id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	if err := w.exec(`ALTER TABLE balance_snapshots ENABLE TRIGGER balance_snapshots_no_change`); err != nil {
		t.Fatal(err)
	}
	st, bad := run(id)
	if st != 201 || bad["status"] != "BREAK" {
		t.Fatalf("an altered snapshot is a break: %d %v", st, bad)
	}
	if diff, _ := bad["population_b"].(map[string]any)["detail"].(map[string]any)["accounts_differing"].([]any); len(diff) != 1 || diff[0].(map[string]any)["account_code"] != "1100" {
		t.Fatalf("the break names the account that differs: %v", bad["population_b"])
	}
	if bad["population_a"].(map[string]any)["detail"].(map[string]any)["content_matches_hash"] != false {
		t.Fatalf("and says the content no longer matches its hash: %v", bad["population_a"])
	}
}
