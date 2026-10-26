package store_test

import (
	"fmt"
	"sync"
	"testing"
)

// Foreign currency in the ledger (standard §23, §24) against real Postgres as the ordinary role: the original basis kept,
// realized exchange difference on settlement, and period-end revaluation that never uses a stale or alternate rate.

func (w *w2) eventAt(t *testing.T, etype, cp, ccy, day string, facts map[string]any) string {
	t.Helper()
	w2seq++
	src := fmt.Sprintf("fx-%d", w2seq)
	body := map[string]any{"event_type": etype, "event_version": 1, "legal_entity_id": w.entity, "source_system": "subledger", "source_object_type": "doc", "source_object_id": src,
		"source_event_id": src, "occurred_at": day + "T09:00:00Z", "document_date": day + "T00:00:00Z", "transaction_currency": ccy, "facts": facts,
		"idempotency_key": "idem-" + src, "correlation_id": "corr-" + src, "causation_id": "cause-" + src}
	if cp != "" {
		body["counterparty_id"] = cp
	}
	st, ev := w.call("POST", "/v1/accounting-events", w.tenant, "subledger", body)
	if st != 201 {
		t.Fatalf("register %s: %d %v", etype, st, ev)
	}
	return ev["accounting_event_id"].(string)
}

func (w *w2) postAt(t *testing.T, etype, cp, ccy, day string, facts map[string]any) map[string]any {
	t.Helper()
	st, res := w.kenv.post(w.eventAt(t, etype, cp, ccy, day, facts), w.book, nil)
	if st != 201 {
		t.Fatalf("post %s %s: %d %v", etype, day, st, res)
	}
	return res
}

func (w *w2) reval(period, date string) (int, map[string]any) {
	return w.call("POST", "/v1/revaluation-runs", w.tenant, "close-runner", map[string]any{"book_id": w.book, "fiscal_period": period, "valuation_date": date})
}

func (w *w2) jl(t *testing.T, journalID, code, col string) string {
	return w.scalar(t, `SELECT `+col+`::text FROM journal_lines WHERE journal_id=$1::uuid AND account_code=$2`, journalID, code)
}

func TestSettlementAtAnotherRatePostsARealizedDifference(t *testing.T) {
	w := w2Setup(t)
	w.rate("USD", "GBP", "0.80", "SPOT", "2026-03-10T08:00:00Z", "ecb", "s1")
	w.rate("USD", "GBP", "0.85", "SPOT", "2026-03-20T08:00:00Z", "ecb", "s2")

	// a USD 100 invoice at 0.80: the original basis is GBP 80.00 and stays so
	inv := w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "USD", "2026-03-10", inv2("100.00", "100.00", "0.00"))
	ji := inv["journal_id"].(string)
	if w.jl(t, ji, "1100", "debit_amount") != "80.00" || w.jl(t, ji, "1100", "transaction_amount") != "100.00000000" || w.jl(t, ji, "1100", "transaction_currency") != "USD" {
		t.Fatal("the receivable keeps its USD amount and its GBP basis")
	}

	// the customer pays USD 100 when the rate is 0.85: the bank receives GBP 85, the receivable is retired at its 80, the 5 is realized
	pay := w.postAt(t, "AR.CASH_RECEIVED_FX", "CUST-1", "USD", "2026-03-20", map[string]any{"amount": "100.00", "allocation_ref": "ALLOC-1", "carrying_functional": "80.00"})
	jp := pay["journal_id"].(string)
	if w.jl(t, jp, "1000", "debit_amount") != "85.00" || w.jl(t, jp, "1100", "credit_amount") != "80.00" || w.jl(t, jp, "7100", "credit_amount") != "5.00" {
		t.Fatalf("bank %s, receivable %s, realized gain %s", w.jl(t, jp, "1000", "debit_amount"), w.jl(t, jp, "1100", "credit_amount"), w.jl(t, jp, "7100", "credit_amount"))
	}
	if w.scalar(t, `SELECT posting_rule_line_ref FROM journal_lines WHERE journal_id=$1::uuid AND account_code='7100'`, jp) != "REALIZED_FX" || w.jl(t, jp, "7100", "transaction_amount") != "0.00000000" {
		t.Fatal("the realized difference is an explicit, traceable line with no transaction amount")
	}
	if w.scalar(t, `SELECT exchange_rate::text FROM journal_lines WHERE journal_id=$1::uuid AND account_code='1000'`, jp) != "0.850000000000" {
		t.Fatal("the settlement rate is recorded")
	}
	// nothing is left owed by the customer, in either currency
	st, pos := w.call("GET", "/v1/foreign-positions?book_id="+w.book+"&as_of=2026-03-31", w.tenant, "recon-runner", nil)
	if st != 200 {
		t.Fatalf("%d %v", st, pos)
	}
	for _, p := range pos["positions"].([]any) {
		pp := p.(map[string]any)
		if pp["account_code"] == "1100" {
			t.Fatalf("a settled receivable has no open position: %v", pp)
		}
	}
	// without a settlement-day rate nothing posts: the spot rate of the 10th is too old for the 25th, and it is not silently reused
	if st, b := w.kenv.post(w.eventAt(t, "AR.INVOICE_ISSUED", "CUST-2", "USD", "2026-03-25", inv2("10.00", "10.00", "0.00")), w.book, nil); st != 422 || b["code"] != "RATE_STALE" {
		t.Fatalf("%d %v", st, b)
	}
}

func inv2(gross, net, tax string) map[string]any {
	return map[string]any{"gross": gross, "net": net, "tax": tax}
}

func TestRevaluationValuesOpenForeignItemsAtTheClosingRate(t *testing.T) {
	w := w2Setup(t)
	w.rate("USD", "GBP", "0.80", "SPOT", "2026-03-10T08:00:00Z", "ecb", "s1")
	w.rate("USD", "GBP", "0.85", "SPOT", "2026-03-20T08:00:00Z", "ecb", "s2")
	w.rate("USD", "GBP", "0.86", "SPOT", "2026-03-25T08:00:00Z", "ecb", "s3")
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "USD", "2026-03-10", inv2("100.00", "100.00", "0.00"))
	w.postAt(t, "AR.CASH_RECEIVED_FX", "CUST-1", "USD", "2026-03-20", map[string]any{"amount": "100.00", "allocation_ref": "A-1", "carrying_functional": "80.00"})
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-2", "USD", "2026-03-25", inv2("50.00", "50.00", "0.00"))
	// open at month end: the bank holds USD 100 (carried at GBP 85.00) and CUST-2 owes USD 50 (carried at GBP 43.00)

	// the closing rate is missing: only a spot rate exists, and a revaluation never stands one in for the other
	st, b := w.reval("2026-03", "2026-03-31")
	if st != 422 || b["code"] != "REVALUATION_RATE_UNAVAILABLE" || b["family"] != "AK-FX" {
		t.Fatalf("%d %v", st, b)
	}
	if w.count(t, `SELECT count(*) FROM revaluation_runs`) != 0 || w.count(t, `SELECT count(*) FROM journal_headers WHERE journal_type='REVALUATION'`) != 0 {
		t.Fatal("a refused revaluation leaves nothing behind")
	}
	if st, _ := w.rate("USD", "GBP", "0.88", "CLOSING", "2026-03-31T17:00:00Z", "ecb", "c1"); st != 201 {
		t.Fatal("closing rate")
	}

	// the accounts a gain and a loss are posted to are the tenant's own mappings
	if err := w.exec(`DELETE FROM account_mappings WHERE tenant_id=$1::uuid AND mapping_key='FX_UNREALIZED_GAIN'`, w.tenant); err != nil {
		t.Fatal(err)
	}
	if st, b := w.reval("2026-03", "2026-03-31"); st != 422 || b["code"] != "REVALUATION_ACCOUNT_UNAVAILABLE" {
		t.Fatalf("%d %v", st, b)
	}
	if err := w.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES (gen_random_uuid(), $1::uuid, 'FX_UNREALIZED_GAIN', '7300', '2026-01-01', 'seed')`, w.tenant); err != nil {
		t.Fatal(err)
	}

	st, run := w.reval("2026-03", "2026-03-31")
	if st != 201 || run["item_count"].(float64) != 2 || run["total_gain"] != "4.00" || run["total_loss"] != "0.00" || run["policy_id"] != "CLOSING_ON_DATE" {
		t.Fatalf("%d %v", st, run)
	}
	pop := run["population"].(map[string]any)
	items := pop["items"].([]any)
	byAcct := map[string]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		byAcct[m["account_code"].(string)] = m
	}
	// bank: USD 100 x 0.88 = 88.00 against 85.00 carried; receivable: USD 50 x 0.88 = 44.00 against 43.00
	if byAcct["1000"]["revalued_functional"] != "88.00" || byAcct["1000"]["difference"] != "3.00" || byAcct["1000"]["outcome"] != "GAIN" ||
		byAcct["1100"]["revalued_functional"] != "44.00" || byAcct["1100"]["difference"] != "1.00" || byAcct["1100"]["counterparty_id"] != "CUST-2" {
		t.Fatalf("%v", items)
	}
	rates := pop["rates"].([]any)
	if len(rates) != 1 || rates[0].(map[string]any)["rate_type"] != "CLOSING" || rates[0].(map[string]any)["rate"] != "0.88" {
		t.Fatalf("%v", rates)
	}
	jid, rid := run["journal_id"].(string), run["reversal_journal_id"].(string)
	if w.jl(t, jid, "1000", "debit_amount") != "3.00" || w.jl(t, jid, "1100", "debit_amount") != "1.00" || w.jl(t, jid, "7300", "credit_amount") != "4.00" {
		t.Fatal("the revaluation journal")
	}
	if w.scalar(t, `SELECT journal_type||'|'||journal_purpose||'|'||posting_date::text||'|'||fiscal_period FROM journal_headers WHERE journal_id=$1::uuid`, jid) != "REVALUATION|REVALUATION|2026-03-31|2026-03" {
		t.Fatal("it is separately identifiable")
	}
	// the reversal lands on the first day of the next period and neutralizes it
	if w.scalar(t, `SELECT journal_type||'|'||reversal_of_journal_id::text||'|'||posting_date::text||'|'||fiscal_period FROM journal_headers WHERE journal_id=$1::uuid`, rid) != "REVERSAL|"+jid+"|2026-04-01|2026-04" ||
		w.scalar(t, `SELECT status FROM journal_headers WHERE journal_id=$1::uuid`, jid) != "REVERSED" || w.jl(t, rid, "7300", "debit_amount") != "4.00" {
		t.Fatal("the unrealized difference reverses in the next period")
	}
	// the carrying amounts after both journals are the original ones again
	if w.scalar(t, `SELECT sum(debit_amount - credit_amount)::text FROM ledger_entries WHERE account_code='1000'`) != "85.00" {
		t.Fatal("bank back at its recognised basis after the reversal")
	}
	// idempotent by entity, book, period and run type
	if st, again := w.reval("2026-03", "2026-03-31"); st != 200 || again["revaluation_run_id"] != run["revaluation_run_id"] || again["existing"] != true {
		t.Fatalf("%d %v", st, again)
	}
	if w.count(t, `SELECT count(*) FROM journal_headers WHERE journal_type IN ('REVALUATION') OR (journal_type='REVERSAL' AND journal_purpose='REVALUATION')`) != 2 {
		t.Fatal("one revaluation and one reversal, however often it is asked for")
	}
	wantRefused(t, "change a run", w.asApp(w.tenant, `UPDATE revaluation_runs SET total_gain = 0`), "append-only")
	wantRefused(t, "delete a run", w.asApp(w.tenant, `DELETE FROM revaluation_runs`), "append-only")
	if st, got := w.call("GET", "/v1/revaluation-runs", w.tenant, "auditor", nil); st != 200 || got["total"].(float64) != 1 {
		t.Fatalf("%d %v", st, got)
	}
}

func TestRevaluationOfALossAndOfLiabilities(t *testing.T) {
	w := w2Setup(t)
	w.rate("USD", "GBP", "0.80", "SPOT", "2026-03-10T08:00:00Z", "ecb", "s1")
	// a USD 100 payable at 0.80 = GBP 80.00; the dollar strengthens to 0.88: the liability is worth more, which is a loss
	w.postAt(t, "AP.INVOICE_APPROVED", "SUP-1", "USD", "2026-03-10", map[string]any{"gross": "100.00", "net": "100.00", "tax": "0.00", "tax_recoverable": "0.00", "duplicate_check": "PASSED", "match_status": "NOT_REQUIRED"})
	w.rate("USD", "GBP", "0.88", "CLOSING", "2026-03-31T17:00:00Z", "ecb", "c1")
	st, run := w.reval("2026-03", "2026-03-31")
	if st != 201 || run["total_loss"] != "8.00" || run["total_gain"] != "0.00" {
		t.Fatalf("%d %v", st, run)
	}
	jid := run["journal_id"].(string)
	if w.jl(t, jid, "2000", "credit_amount") != "8.00" || w.jl(t, jid, "7400", "debit_amount") != "8.00" {
		t.Fatal("a liability that grows is credited, against the unrealized loss")
	}
	if w.scalar(t, `SELECT counterparty_id FROM journal_lines WHERE journal_id=$1::uuid AND account_code='2000'`, jid) != "SUP-1" {
		t.Fatal("the revaluation line names the supplier")
	}
}

func TestRevaluationRespectsTheSealAndTheGate(t *testing.T) {
	w := w2Setup(t)
	w.rate("USD", "GBP", "0.80", "SPOT", "2026-03-10T08:00:00Z", "ecb", "s1")
	w.rate("USD", "GBP", "0.88", "CLOSING", "2026-03-31T17:00:00Z", "ecb", "c1")
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "USD", "2026-03-10", inv2("100.00", "100.00", "0.00"))

	// the reversal would land in a period that is closed: the revaluation is refused as a whole
	w.periods.set("2026-04", "CLOSED")
	st, b := w.reval("2026-03", "2026-03-31")
	if st != 409 || b["family"] != "AK-PERIOD" {
		t.Fatalf("%d %v", st, b)
	}
	w.periods.set("2026-04", "")
	// a sealed period refuses it inside the posting transaction, and the run is not recorded
	if st, _ := w.seal("SEAL", "", ""); st != 201 {
		t.Fatal("seal")
	}
	if st, b := w.reval("2026-03", "2026-03-31"); st != 409 || b["code"] != "PERIOD_SEALED" {
		t.Fatalf("%d %v", st, b)
	}
	if w.count(t, `SELECT count(*) FROM revaluation_runs`) != 0 || w.count(t, `SELECT count(*) FROM journal_headers WHERE journal_type='REVALUATION'`) != 0 {
		t.Fatal("nothing was left behind by the refused runs")
	}
	if st, _ := w.call("POST", "/v1/revaluation-runs", w.tenant, "close-runner", map[string]any{"book_id": w.book, "valuation_date": "2099-03-31"}); st != 422 {
		t.Fatalf("not in the future: %d", st)
	}
}

func TestRevaluationRunsOncePerPeriodUnderConcurrency(t *testing.T) {
	w := w2Setup(t)
	w.rate("USD", "GBP", "0.80", "SPOT", "2026-03-10T08:00:00Z", "ecb", "s1")
	w.rate("USD", "GBP", "0.88", "CLOSING", "2026-03-31T17:00:00Z", "ecb", "c1")
	w.postAt(t, "AR.INVOICE_ISSUED", "CUST-1", "USD", "2026-03-10", inv2("100.00", "100.00", "0.00"))
	var wg sync.WaitGroup
	var mu sync.Mutex
	created, existing := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, b := w.reval("2026-03", "2026-03-31")
			mu.Lock()
			defer mu.Unlock()
			switch st {
			case 201:
				created++
			case 200:
				existing++
			default:
				t.Errorf("%d %v", st, b)
			}
		}()
	}
	wg.Wait()
	if created != 1 || existing != 5 {
		t.Fatalf("created %d existing %d", created, existing)
	}
	if w.count(t, `SELECT count(*) FROM revaluation_runs`) != 1 || w.count(t, `SELECT count(*) FROM journal_headers WHERE journal_type='REVALUATION'`) != 1 {
		t.Fatal("one run, one journal")
	}
}
