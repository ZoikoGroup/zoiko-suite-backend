package store_test

import (
	"testing"
)

// Wave 3 exit criterion: a month-end scenario closes and reproduces. It runs the whole month — postings in two currencies, the
// revaluation, the reconciliations, the evidence, the seal, the signed snapshot — and then reopens, corrects, re-closes and checks
// that both snapshots can still be reproduced from the ledger.

func (w *w2) snapshot(purpose string) (int, map[string]any) {
	return w.call("POST", "/v1/balance-snapshots", w.tenant, "close-svc", map[string]any{"book_id": w.book, "fiscal_period": "2026-03", "purpose": purpose})
}

func (w *w2) evidence(require string) map[string]any {
	st, b := w.call("GET", "/v1/close-evidence?legal_entity_id="+w.entity+"&fiscal_period=2026-03&require="+require, w.tenant, "close-svc", nil)
	if st != 200 {
		panic(b)
	}
	return b
}

func TestMonthEndClosesAndReproduces(t *testing.T) {
	w := w2Setup(t)
	w.rate("USD", "GBP", "0.80", "SPOT", "2026-03-10T08:00:00Z", "ecb", "s1")
	w.rate("USD", "GBP", "0.88", "CLOSING", "2026-03-31T17:00:00Z", "ecb", "c1")

	// the month: a settled GBP invoice, an open GBP invoice, an open USD invoice, a supplier invoice and a bank fee
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-03-05", inv2("120.00", "100.00", "20.00"))
	w.postAt(t, "AR.CASH_RECEIVED", "CUST-1", "GBP", "2026-03-12", map[string]any{"amount": "120.00", "allocation_ref": "A-1"})
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-2", "GBP", "2026-03-15", inv2("60.00", "50.00", "10.00"))
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-3", "USD", "2026-03-10", inv2("100.00", "100.00", "0.00"))
	w.postAt(t, "AP.INVOICE_APPROVED", "SUP-1", "GBP", "2026-03-18", map[string]any{"gross": "60.00", "net": "50.00", "tax": "10.00", "tax_recoverable": "10.00", "duplicate_check": "PASSED", "match_status": "MATCHED"})
	w.postAt(t, "BANK.FEE", "", "GBP", "2026-03-20", inv2("12.00", "10.00", "2.00"))
	const all = "AR_GL,AP_GL,BANK_GL,EVENT_POSTING"

	// before the close: the foreign item has not been revalued and nothing has been reconciled
	ev := w.evidence(all)
	issues := ev["blocking_issues"].([]any)
	if ev["ready"] != false || len(issues) < 5 {
		t.Fatalf("%v", ev)
	}
	book := ev["books"].([]any)[0].(map[string]any)
	if book["revaluation"].(map[string]any)["required"] != true || book["revaluation"].(map[string]any)["open_foreign_items"].(float64) != 1 {
		t.Fatalf("%v", book["revaluation"])
	}

	// revalue: USD 100 carried at 80.00 is worth 88.00 at the closing rate
	st, run := w.reval("2026-03", "2026-03-31")
	if st != 201 || run["total_gain"] != "8.00" {
		t.Fatalf("%d %v", st, run)
	}

	// the subledgers' totals agree with the ledger as revalued: AR = 60 + 88, AP = 60, bank = 120 - 12
	for typ, total := range map[string]string{"AR_GL": "148.00", "AP_GL": "60.00", "BANK_GL": "108.00"} {
		if st, r := w.recon(typ, total, "month-end-"+typ); st != 201 || r["status"] != "MATCHED" {
			t.Fatalf("%s: %d %v", typ, st, r)
		}
	}
	if st, r := w.recon("EVENT_POSTING", "", ""); st != 201 || r["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, r)
	}
	if ev = w.evidence(all); ev["ready"] != true {
		t.Fatalf("everything is done, so the kernel says the period can close: %v", ev)
	}

	// a snapshot of a period that is not sealed is not a close snapshot
	if st, b := w.snapshot("CLOSE"); st != 409 || b["code"] != "PERIOD_NOT_SEALED" {
		t.Fatalf("%d %v", st, b)
	}
	st, seal := w.seal("SEAL", "", "")
	if st != 201 {
		t.Fatalf("%d %v", st, seal)
	}
	closeSnap := w.mustSnapshot(t, "CLOSE")
	if int64(closeSnap["ledger_watermark"].(float64)) != int64(seal["ledger_watermark"].(float64)) {
		t.Fatalf("a close snapshot is taken at the watermark of the seal: %v vs %v", closeSnap["ledger_watermark"], seal["ledger_watermark"])
	}
	if closeSnap["total_debit"] != closeSnap["total_credit"] || len(closeSnap["signature"].(string)) != 64 || closeSnap["key_id"] != "test-k1" || closeSnap["signed_by"] != "close-svc" {
		t.Fatalf("%v", closeSnap)
	}
	lines := map[string]map[string]any{}
	for _, l := range closeSnap["lines"].([]any) {
		m := l.(map[string]any)
		lines[m["account_code"].(string)] = m
	}
	if lines["1100"]["net_balance"] != "148.00" || lines["7300"]["credit_total"] != "8.00" || lines["1000"]["net_balance"] != "108.00" {
		t.Fatalf("%v", lines)
	}
	// the reversal of the revaluation is April's: it is not in the March snapshot
	if lines["7300"]["net_balance"] != "-8.00" {
		t.Fatalf("the reversal belongs to the next period: %v", lines["7300"])
	}
	verify := func(id string) map[string]any {
		st, b := w.call("POST", "/v1/balance-snapshots/"+id+"/verify", w.tenant, "auditor", map[string]any{})
		if st != 200 {
			t.Fatalf("%d %v", st, b)
		}
		return b
	}
	if v := verify(closeSnap["snapshot_id"].(string)); v["verified"] != true || v["reproducible_from_ledger"] != true || v["signature_valid"] != true {
		t.Fatalf("%v", v)
	}
	// the evidence now shows what the close rests on
	ev = w.evidence(all)
	b0 := ev["books"].([]any)[0].(map[string]any)
	if ev["seal"].(map[string]any)["status"] != "SEALED" || b0["latest_snapshot"].(map[string]any)["snapshot_id"] != closeSnap["snapshot_id"] || b0["revaluation"].(map[string]any)["done"] != true {
		t.Fatalf("%v", ev)
	}

	// a correction after the close: reopened under authority, posted, sealed again with a new snapshot
	if st, _ := w.seal("REOPEN", "AUTH-9", "supplier credit missed"); st != 200 {
		t.Fatal("reopen")
	}
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-2", "GBP", "2026-03-28", inv2("12.00", "10.00", "2.00"))
	st, re := w.seal("RESEAL", "", "")
	if st != 200 || re["entries_since_reopen"].(float64) < 3 {
		t.Fatalf("%d %v", st, re)
	}
	reSnap := w.mustSnapshot(t, "RECLOSE")
	if reSnap["ledger_watermark"].(float64) <= closeSnap["ledger_watermark"].(float64) {
		t.Fatal("the re-close snapshot reaches past the first")
	}
	rl := map[string]string{}
	for _, l := range reSnap["lines"].([]any) {
		m := l.(map[string]any)
		rl[m["account_code"].(string)] = m["net_balance"].(string)
	}
	if rl["1100"] != "160.00" {
		t.Fatalf("the correction is in the re-close snapshot: %v", rl)
	}
	// both reproduce from the ledger, whatever was posted after the first was taken
	if v := verify(closeSnap["snapshot_id"].(string)); v["verified"] != true {
		t.Fatalf("the original close snapshot still reproduces: %v", v)
	}
	if v := verify(reSnap["snapshot_id"].(string)); v["verified"] != true {
		t.Fatalf("%v", v)
	}
	if st, list := w.call("GET", "/v1/balance-snapshots?fiscal_period=2026-03&book_id="+w.book, w.tenant, "auditor", nil); st != 200 || list["total"].(float64) != 2 {
		t.Fatalf("%d %v", st, list)
	}

	// a snapshot is retained and cannot be altered; one that was altered anyway is caught
	wantRefused(t, "change a snapshot", w.asApp(w.tenant, `UPDATE balance_snapshots SET total_debit = 1`), "append-only")
	wantRefused(t, "delete a snapshot", w.asApp(w.tenant, `DELETE FROM balance_snapshots`), "append-only")
	if err := fixtureExec(w.ctx(), w.admin, `UPDATE balance_snapshots SET lines = replace(lines::text, '148.00', '149.00')::jsonb WHERE snapshot_id = $1::uuid`, closeSnap["snapshot_id"].(string)); err != nil {
		t.Fatal(err)
	}
	if v := verify(closeSnap["snapshot_id"].(string)); v["verified"] != false || v["content_matches_hash"] != false || v["reproducible_from_ledger"] != true {
		t.Fatalf("tampering is detected: %v", v)
	}
	if err := fixtureExec(w.ctx(), w.admin, `UPDATE balance_snapshots SET signature = repeat('0', 64) WHERE snapshot_id = $1::uuid`, reSnap["snapshot_id"].(string)); err != nil {
		t.Fatal(err)
	}
	if v := verify(reSnap["snapshot_id"].(string)); v["verified"] != false || v["signature_valid"] != false || v["reproducible_from_ledger"] != true {
		t.Fatalf("a forged signature is detected: %v", v)
	}
}

func (w *w2) mustSnapshot(t *testing.T, purpose string) map[string]any {
	t.Helper()
	st, b := w.snapshot(purpose)
	if st != 201 {
		t.Fatalf("snapshot %s: %d %v", purpose, st, b)
	}
	return b
}

func TestSnapshotMovementIsThePeriodsAndCumulativeIsEverything(t *testing.T) {
	w := w2Setup(t)
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-02-15", inv2("10.00", "10.00", "0.00"))
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-03-15", inv2("120.00", "100.00", "20.00"))
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "GBP", "2026-04-15", inv2("5.00", "5.00", "0.00"))
	snap := w.mustSnapshot(t, "AD_HOC")
	for _, l := range snap["lines"].([]any) {
		m := l.(map[string]any)
		if m["account_code"] == "1100" && (m["debit_total"] != "130.00" || m["period_debit"] != "120.00" || m["net_balance"] != "130.00") {
			t.Fatalf("cumulative to the end of March, with March's own movement: %v", m)
		}
	}
	if snap["account_count"].(float64) != 3 {
		t.Fatalf("%v", snap)
	}
	if st, b := w.call("POST", "/v1/balance-snapshots", w.tenant, "close-svc", map[string]any{"book_id": w.book, "fiscal_period": "2026-03", "purpose": "BOGUS"}); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := w.call("POST", "/v1/balance-snapshots", w.tenant, "close-svc", map[string]any{"book_id": w.book, "fiscal_period": "March"}); st != 422 {
		t.Fatalf("a period is YYYY-MM: %d", st)
	}
}
