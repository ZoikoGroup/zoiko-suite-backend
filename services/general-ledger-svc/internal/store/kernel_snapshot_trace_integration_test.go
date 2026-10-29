package store_test

import (
	"strconv"
	"testing"
)

// §31, §32, A12: a figure of a retained snapshot is explained by the ledger entries behind it, each led back to its journal, accounting
// event, rule version and source — and the entries add up to the figure.
func TestSnapshotFigureTracesBackToItsSources(t *testing.T) {
	w := w2Setup(t)
	a := w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-03-05", inv2("120.00", "100.00", "20.00"))
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-2", "GBP", "2026-03-15", inv2("60.00", "50.00", "10.00"))
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-3", "GBP", "2026-03-18", inv2("36.00", "30.00", "6.00"))
	if st, b := w.call("POST", "/v1/journals/"+a["journal_id"].(string)+"/reverse", w.tenant, "reviewer",
		map[string]any{"reason": "wrong customer", "correlation_id": "rev-trace", "correction_kind": "ERROR_CORRECTION", "posting_date": "2026-03-20"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	st, snap := w.snapshot("AD_HOC")
	if st != 201 {
		t.Fatalf("%d %v", st, snap)
	}
	id := snap["snapshot_id"].(string)

	// a posting that arrives after the snapshot is not part of what the snapshot says
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-4", "GBP", "2026-03-25", inv2("24.00", "20.00", "4.00"))

	trace := func(q string) (int, map[string]any) {
		return w.call("GET", "/v1/balance-snapshots/"+id+"/trace?"+q, w.tenant, "auditor", nil)
	}
	st, tr := trace("account_code=1100")
	if st != 200 {
		t.Fatalf("%d %v", st, tr)
	}
	fig, tot := tr["snapshot_figure"].(map[string]any), tr["traced_total"].(map[string]any)
	if fig["debit_total"] != "216.00" || fig["credit_total"] != "120.00" || tot["debit_total"] != "216.00" || tot["credit_total"] != "120.00" || tot["entry_count"].(float64) != 4 || tr["reconciles"] != true {
		t.Fatalf("the entries must add up to the figure: %v", tr)
	}
	entries := tr["entries"].([]any)
	if len(entries) != 4 || tr["next_after"] != nil {
		t.Fatalf("%d entries, after %v", len(entries), tr["next_after"])
	}
	var last float64
	for i, x := range entries {
		e := x.(map[string]any)
		if e["ledger_entry_seq"].(float64) <= last {
			t.Fatalf("entries are in ledger order: %v", e)
		}
		last = e["ledger_entry_seq"].(float64)
		src, _ := e["source"].(map[string]any)
		rule, _ := e["posting_rule"].(map[string]any)
		if src == nil || src["event_type"] != "AR.INVOICE_ISSUED" || src["source_system"] == "" || src["source_object_id"] == "" || rule == nil || rule["version"] == "" || rule["posting_rule_set_id"] == "" ||
			e["journal_number"] == "" || e["posting_rule_line_ref"] == nil {
			t.Fatalf("entry %d does not lead back to its source and rule: %v", i, e)
		}
	}
	if entries[3].(map[string]any)["journal_type"] != "REVERSAL" || entries[3].(map[string]any)["credit_amount"] != "120.00" {
		t.Fatalf("the reversal is part of the lineage: %v", entries[3])
	}

	// paged
	st, p1 := trace("account_code=1100&limit=3")
	if st != 200 || len(p1["entries"].([]any)) != 3 || p1["next_after"] == nil || p1["reconciles"] != true {
		t.Fatalf("%d %v", st, p1)
	}
	st, p2 := trace("account_code=1100&limit=3&after=" + strconv.FormatInt(int64(p1["next_after"].(float64)), 10))
	if st != 200 || len(p2["entries"].([]any)) != 1 || p2["next_after"] != nil {
		t.Fatalf("%d %v", st, p2)
	}

	// refusals
	if st, _ := trace(""); st != 400 {
		t.Fatalf("no account: %d", st)
	}
	if st, _ := trace("account_code=9999"); st != 404 {
		t.Fatalf("an account the snapshot has no figure for: %d", st)
	}
	if st, _ := trace("account_code=1100&limit=0"); st != 400 {
		t.Fatalf("limit 0: %d", st)
	}
	if st, _ := trace("account_code=1100&after=-1"); st != 400 {
		t.Fatalf("after -1: %d", st)
	}
	if st, _ := w.call("GET", "/v1/balance-snapshots/00000000-0000-0000-0000-000000000000/trace?account_code=1100", w.tenant, "auditor", nil); st != 404 {
		t.Fatalf("unknown snapshot: %d", st)
	}
	if st, _ := w.call("GET", "/v1/balance-snapshots/"+id+"/trace?account_code=1100", "00000000-0000-0000-0000-00000000dead", "auditor", nil); st != 404 {
		t.Fatalf("another tenant must not see it: %d", st)
	}
	// reading a snapshot is not enough to see the postings behind it, and seeing postings is not enough to read the snapshot
	w.denied.Store("snap-only|GL_POSTING_VIEW", true)
	if st, _ := w.call("GET", "/v1/balance-snapshots/"+id+"/trace?account_code=1100", w.tenant, "snap-only", nil); st != 403 {
		t.Fatalf("without GL_POSTING_VIEW: %d", st)
	}
	w.denied.Store("post-only|GL_SNAPSHOT_READ", true)
	if st, _ := w.call("GET", "/v1/balance-snapshots/"+id+"/trace?account_code=1100", w.tenant, "post-only", nil); st != 403 {
		t.Fatalf("without GL_SNAPSHOT_READ: %d", st)
	}
}
