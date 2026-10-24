package store_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Wave 2 (AR / AP / bank) against real Postgres as the ordinary role: the standard rule packs posting end to end, control
// accounts and counterparties, and the reconciliation framework (runs, tolerances, cases, adjustments, the close-gate picture).

type w2 struct {
	*kenv
	book string
}

// w2Setup builds a chart with control accounts, an active book and the three standard packs, each rule tested, approved and
// activated by three different people.
func w2Setup(t *testing.T) *w2 {
	t.Helper()
	e := newKEnv(t)
	accounts := [][3]string{{"1000", "ASSET", "BANK"}, {"1050", "ASSET", "CLEARING"}, {"1100", "ASSET", "AR"}, {"1300", "ASSET", ""}, {"2000", "LIABILITY", "AP"}, {"2050", "LIABILITY", "CLEARING"},
		{"2150", "LIABILITY", "CLEARING"}, {"2200", "LIABILITY", ""}, {"4000", "REVENUE", ""}, {"4100", "REVENUE", ""}, {"6000", "EXPENSE", ""}, {"6900", "EXPENSE", ""}, {"6950", "EXPENSE", ""}, {"6960", "EXPENSE", ""},
		{"7100", "REVENUE", ""}, {"7200", "EXPENSE", ""}, {"7300", "REVENUE", ""}, {"7400", "EXPENSE", ""}}
	for _, a := range accounts {
		if err := e.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`, uuid.NewString(), e.tenant, a[0], a[0]+" account", a[1]); err != nil {
			t.Fatal(err)
		}
	}
	keys := map[string]string{"AR_CONTROL": "1100", "REVENUE": "4000", "OUTPUT_TAX": "2200", "BANK": "1000", "UNAPPLIED_CASH": "2150", "BAD_DEBT": "6900", "SALES_RETURNS": "4100", "PROCESSOR_CLEARING": "1050",
		"PROCESSOR_FEES": "6950", "AP_CONTROL": "2000", "EXPENSE": "6000", "INPUT_TAX": "1300", "PAYMENT_CLEARING": "2050", "BANK_FEES": "6960",
		"FX_REALIZED_GAIN": "7100", "FX_REALIZED_LOSS": "7200", "FX_UNREALIZED_GAIN": "7300", "FX_UNREALIZED_LOSS": "7400"}
	for k, c := range keys {
		if err := e.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,$3,$4,'2026-01-01','seed')`, uuid.NewString(), e.tenant, k, c); err != nil {
			t.Fatal(err)
		}
	}
	book := e.activeBook(t, "STAT")
	for _, a := range accounts {
		if a[2] == "" {
			continue
		}
		if st, b := e.call("PUT", "/v1/account-applicability/"+a[0], e.tenant, "chart-admin", map[string]any{"control_type": a[2]}); st != 200 {
			t.Fatalf("scope %s: %d %v", a[0], st, b)
		}
	}
	for _, pack := range []string{"ar-standard", "ap-standard", "bank-standard"} {
		st, b := e.call("POST", "/v1/posting-rule-packs/"+pack+"/instantiate", e.tenant, "builder", map[string]any{"version": "1.0.0", "effective_from": "2026-01-01", "book_type": "PRIMARY_STATUTORY", "accounting_framework_id": "IFRS"})
		if st != 201 {
			t.Fatalf("instantiate %s: %d %v", pack, st, b)
		}
		for _, rs := range b["rule_sets"].([]any) {
			if rs.(map[string]any)["status"] != "DRAFT" {
				t.Fatal("a pack creates drafts")
			}
			id := rs.(map[string]any)["posting_rule_set_id"].(string)
			for _, step := range [][2]string{{"test", "builder"}, {"approve", "approver"}, {"activate", "releaser"}} {
				if st, b := e.call("POST", "/v1/posting-rule-sets/"+id+"/"+step[0], e.tenant, step[1], map[string]any{"book_id": book}); st != 200 {
					t.Fatalf("%s %v: %d %v", step[0], rs.(map[string]any)["name"], st, b)
				}
			}
		}
	}
	return &w2{kenv: e, book: book}
}

var w2seq int

func (w *w2) event(t *testing.T, etype, cp string, facts map[string]any) string {
	t.Helper()
	w2seq++
	src := fmt.Sprintf("w2-%d", w2seq)
	body := map[string]any{"event_type": etype, "event_version": 1, "legal_entity_id": w.entity, "source_system": "subledger", "source_object_type": "doc", "source_object_id": src,
		"source_event_id": src, "occurred_at": "2026-03-10T09:00:00Z", "document_date": "2026-03-10T00:00:00Z", "transaction_currency": "GBP", "facts": facts,
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

func (w *w2) post(t *testing.T, etype, cp string, facts map[string]any) map[string]any {
	t.Helper()
	st, res := w.kenv.post(w.event(t, etype, cp, facts), w.book, nil)
	if st != 201 {
		t.Fatalf("post %s: %d %v", etype, st, res)
	}
	return res
}

func (w *w2) recon(typ, total, ref string) (int, map[string]any) {
	body := map[string]any{"reconciliation_type": typ, "book_id": w.book, "fiscal_period": "2026-03", "as_of": "2026-03-31"}
	if total != "" {
		body["population_a"] = map[string]any{"source": "subledger", "reference": ref, "total": total, "count": 3}
	}
	return w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", body)
}

func (w *w2) balance(t *testing.T, code string) string {
	return w.scalar(t, `SELECT (COALESCE(sum(debit_amount),0) - COALESCE(sum(credit_amount),0))::text FROM ledger_entries WHERE account_code=$1`, code)
}

func inv(gross, net, tax string) map[string]any {
	return map[string]any{"gross": gross, "net": net, "tax": tax}
}

func TestRulePacksPostArApAndBankEndToEnd(t *testing.T) {
	w := w2Setup(t)

	// AR: two invoices, a receipt applied to the first, a credit note for the second, unapplied cash for a third customer
	w.post(t, "AR.INVOICE_ISSUED", "CUST-1", inv("120.00", "100.00", "20.00"))
	w.post(t, "AR.INVOICE_ISSUED", "CUST-2", inv("60.00", "50.00", "10.00"))
	w.post(t, "AR.CASH_RECEIVED", "CUST-1", map[string]any{"amount": "120.00", "allocation_ref": "ALLOC-1"})
	w.post(t, "AR.CREDIT_NOTE_ISSUED", "CUST-2", map[string]any{"gross": "60.00", "net": "50.00", "tax": "10.00", "original_invoice_ref": "INV-2"})
	w.post(t, "AR.CASH_RECEIVED_UNAPPLIED", "CUST-3", map[string]any{"amount": "75.50"})
	w.post(t, "AR.INVOICE_ISSUED", "CUST-4", inv("200.00", "200.00", "0.00"))
	// the card route: processor clearing first, then settlement net of the fee
	w.post(t, "AR.RECEIPT_VIA_PROCESSOR", "CUST-4", map[string]any{"amount": "50.00"})
	w.post(t, "BANK.PROCESSOR_SETTLED", "", map[string]any{"gross": "50.00", "fee": "1.45", "net_settlement": "48.55"})
	// AP: an invoice, its payment accepted and settled, and a second payment that the bank rejects
	w.post(t, "AP.INVOICE_APPROVED", "SUP-1", map[string]any{"gross": "120.00", "net": "100.00", "tax": "20.00", "tax_recoverable": "20.00", "duplicate_check": "PASSED", "match_status": "MATCHED"})
	w.post(t, "AP.INVOICE_APPROVED", "SUP-2", map[string]any{"gross": "90.00", "net": "90.00", "tax": "0.00", "tax_recoverable": "0.00", "duplicate_check": "PASSED", "match_status": "NOT_REQUIRED"})
	w.post(t, "AP.PAYMENT_ACCEPTED", "SUP-1", map[string]any{"amount": "120.00"})
	w.post(t, "AP.PAYMENT_SETTLED", "SUP-1", map[string]any{"amount": "120.00"})
	w.post(t, "AP.PAYMENT_ACCEPTED", "SUP-2", map[string]any{"amount": "90.00"})
	w.post(t, "AP.PAYMENT_REJECTED", "SUP-2", map[string]any{"amount": "90.00"})
	w.post(t, "BANK.FEE", "", inv("12.00", "10.00", "2.00"))

	// by hand: AR = 120+60-120-60+200-50 = 150; AP owed = 120+90-120+0 = 90 (SUP-2's payment was rejected and restored);
	// bank = 120 receipt + 75.50 unapplied + 48.55 settlement - 120 payment - 12 fee = 112.05
	if got := w.balance(t, "1100"); got != "150.00" {
		t.Fatalf("AR control %s", got)
	}
	if got := w.balance(t, "2000"); got != "-90.00" {
		t.Fatalf("AP control %s", got)
	}
	if got := w.balance(t, "1000"); got != "112.05" {
		t.Fatalf("bank %s", got)
	}
	if got := w.balance(t, "2050"); got != "0.00" {
		t.Fatalf("payment clearing must be empty once settled and rejected: %s", got)
	}
	if got := w.balance(t, "1050"); got != "0.00" {
		t.Fatalf("processor clearing must be empty once settled: %s", got)
	}
	if got := w.balance(t, "2150"); got != "-75.50" {
		t.Fatalf("the unapplied cash waits in clearing: %s", got)
	}

	// the lines of control accounts carry their counterparty and, in the ledger, the control type the account had
	if w.count(t, `SELECT count(*) FROM ledger_entries WHERE control_type IN ('AR','AP') AND counterparty_id IS NULL`) != 0 {
		t.Fatal("every AR and AP ledger entry names its counterparty")
	}
	if w.scalar(t, `SELECT string_agg(DISTINCT counterparty_id, ',' ORDER BY counterparty_id) FROM ledger_entries WHERE control_type='AR'`) != "CUST-1,CUST-2,CUST-4" {
		t.Fatal("the receivable lines name their customers")
	}

	// the subledgers say the same, so every reconciliation matches
	for typ, total := range map[string]string{"AR_GL": "150.00", "AP_GL": "90.00", "BANK_GL": "112.05"} {
		st, run := w.recon(typ, total, "stmt-"+typ)
		if st != 201 || run["status"] != "MATCHED" || run["difference"] != "0.000000" {
			t.Fatalf("%s: %d %v", typ, st, run)
		}
	}
	// per customer, the ledger position is what the receivables subledger should hold
	st, bal := w.call("GET", "/v1/counterparty-balances?control_type=AR&book_id="+w.book+"&as_of=2026-03-31", w.tenant, "recon-runner", nil)
	if st != 200 || bal["total_balance"] != "150.00" {
		t.Fatalf("%d %v", st, bal)
	}
	want := map[string]string{"CUST-1": "0.00", "CUST-2": "0.00", "CUST-4": "150.00"}
	for _, c := range bal["counterparties"].([]any) {
		cp := c.(map[string]any)
		if want[cp["counterparty_id"].(string)] != cp["balance"] {
			t.Fatalf("%v", cp)
		}
	}
}

func TestControlAccountsAndCounterpartiesAreEnforced(t *testing.T) {
	w := w2Setup(t)
	facts := map[string]any{"gross": "120.00", "net": "100.00", "tax": "20.00", "tax_recoverable": "20.00", "duplicate_check": "PASSED", "match_status": "MATCHED"}

	// a payable needs its supplier: refused, and the failed execution is evidence
	missing := w.event(t, "AP.INVOICE_APPROVED", "", facts)
	st, b := w.kenv.post(missing, w.book, nil)
	if st != 422 || b["family"] != "AK-COA" || b["code"] != "COUNTERPARTY_REQUIRED" {
		t.Fatalf("%d %v", st, b)
	}
	// a rule that waits for a duplicate check does not post before it
	pending := map[string]any{"gross": "120.00", "net": "100.00", "tax": "20.00", "tax_recoverable": "20.00", "duplicate_check": "PENDING", "match_status": "MATCHED"}
	if st, b = w.kenv.post(w.event(t, "AP.INVOICE_APPROVED", "SUP-1", pending), w.book, nil); st != 422 || b["family"] != "AK-RULE" {
		t.Fatalf("%d %v", st, b)
	}

	// the correction is a new event naming the one it corrects; posting it clears the failed attempt
	fix := map[string]any{"event_type": "AP.INVOICE_APPROVED", "event_version": 1, "legal_entity_id": w.entity, "source_system": "subledger", "source_object_type": "doc", "source_object_id": "fix",
		"source_event_id": "fix-1", "occurred_at": "2026-03-10T09:00:00Z", "document_date": "2026-03-10T00:00:00Z", "transaction_currency": "GBP", "facts": facts, "counterparty_id": "SUP-1",
		"idempotency_key": "idem-fix-1", "correlation_id": "corr-fix", "causation_id": "cause-fix", "supersedes_event_id": missing}
	st, fe := w.call("POST", "/v1/accounting-events", w.tenant, "subledger", fix)
	if st != 201 {
		t.Fatalf("%d %v", st, fe)
	}
	if w.count(t, `SELECT count(*) FROM posting_executions WHERE status IN ('FAILED','QUARANTINED') AND accounting_event_id=$1::uuid`, missing) != 1 {
		t.Fatal("the refusal is on record")
	}
	if st, b = w.kenv.post(fe["accounting_event_id"].(string), w.book, nil); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	if w.count(t, `SELECT count(*) FROM posting_executions WHERE status='RESOLVED' AND accounting_event_id=$1::uuid`, missing) != 1 {
		t.Fatal("posting the correction resolves the failed attempt at the original")
	}

	// manual journals: a control account is not an open door, and a line on one names its counterparty
	manual := func(corr string, cp any, override bool) map[string]any {
		line := map[string]any{"account_code": "1100", "debit_amount": 40}
		if cp != nil {
			line["counterparty_id"] = cp
		}
		return map[string]any{"tenant_id": w.tenant, "legal_entity_id": w.entity, "fiscal_period": "2026-03", "description": "manual", "correlation_id": corr, "journal_type": "STANDARD",
			"transaction_date": "2026-03-10", "posting_date": "2026-03-10", "currency_code": "GBP", "override_control_account_restriction": override,
			"lines": []any{line, map[string]any{"account_code": "4000", "credit_amount": 40}}}
	}
	if st, b = w.call("POST", "/v1/journals", w.tenant, "preparer", manual("m-direct", "CUST-1", false)); st == 201 {
		t.Fatalf("direct posting to the AR control account must be refused: %v", b)
	}
	if st, b = w.call("POST", "/v1/journals", w.tenant, "preparer", manual("m-nocp", nil, true)); st != 422 || b["error"] != "counterparty_required" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b = w.call("POST", "/v1/journals", w.tenant, "preparer", manual("m-ok", "CUST-1", true)); st != 201 {
		t.Fatalf("with the exceptional authority and the counterparty it is allowed: %d %v", st, b)
	}
	// the scope endpoint is controlled
	if st, _ := w.call("PUT", "/v1/account-applicability/1100", w.tenant, "chart-admin", map[string]any{"control_type": "NOT_A_TYPE"}); st != 400 {
		t.Fatalf("%d", st)
	}
	if st, got := w.call("GET", "/v1/account-applicability/1100", w.tenant, "chart-admin", nil); st != 200 || got["control_type"] != "AR" {
		t.Fatalf("%d %v", st, got)
	}
}

func TestReconciliationCaseLifecycle(t *testing.T) {
	w := w2Setup(t)
	w.post(t, "AR.INVOICE_ISSUED", "CUST-1", inv("120.00", "100.00", "20.00"))

	// the subledger says 130.00 and the ledger says 120.00: a break of 10.00, with an owner and an SLA
	st, run := w.recon("AR_GL", "130.00", "aging-1")
	if st != 201 || run["status"] != "BREAK" || run["difference"] != "10.000000" || run["owner"] != "recon-runner" || run["sla_due_at"] == nil {
		t.Fatalf("%d %v", st, run)
	}
	id := run["reconciliation_run_id"].(string)
	popB := run["population_b"].(map[string]any)
	if popB["source"] != "LEDGER" || popB["watermark"] == "" || popB["count"].(float64) != 1 {
		t.Fatalf("%v", popB)
	}
	if st, a := w.call("POST", "/v1/reconciliation-runs/"+id+"/assign", w.tenant, "lead", map[string]any{"owner": "ar-team", "sla_hours": 24}); st != 200 || a["owner"] != "ar-team" {
		t.Fatalf("%d %v", st, a)
	}

	// accepting an exception: evidence, a reason, and someone other than the person who ran it
	if st, _ := w.call("POST", "/v1/reconciliation-runs/"+id+"/accept", w.tenant, "recon-approver", map[string]any{"note": "timing"}); st != 422 {
		t.Fatalf("evidence is required: %d", st)
	}
	if st, b := w.call("POST", "/v1/reconciliation-runs/"+id+"/accept", w.tenant, "recon-runner", map[string]any{"note": "timing", "evidence": []string{"ticket-1"}}); st != 403 || b["error"] != "separation_of_duties" {
		t.Fatalf("the runner cannot accept: %d %v", st, b)
	}
	wantRefused(t, "SQL self-approval of an exception", w.asApp(w.tenant,
		`UPDATE reconciliation_runs SET status='ACCEPTED_EXCEPTION', approved_by=created_by, resolution_note='x', evidence='["e"]'::jsonb WHERE reconciliation_run_id=$1::uuid`, id), "approver other than")

	// a manual correction needs the posted journal that made it, and a reconciliation adjustment needs an open case
	adj := func(corr, purpose, caseID string) map[string]any {
		b := map[string]any{"tenant_id": w.tenant, "legal_entity_id": w.entity, "fiscal_period": "2026-03", "description": "reconciliation adjustment", "correlation_id": corr, "journal_type": "ADJUSTMENT",
			"journal_purpose": purpose, "transaction_date": "2026-03-10", "posting_date": "2026-03-10", "currency_code": "GBP", "override_control_account_restriction": true,
			"lines": []any{map[string]any{"account_code": "1100", "debit_amount": 10, "counterparty_id": "CUST-1"}, map[string]any{"account_code": "4000", "credit_amount": 10}}}
		if caseID != "" {
			b["reconciliation_case_id"] = caseID
		}
		return b
	}
	if st, b := w.call("POST", "/v1/journals", w.tenant, "preparer", adj("adj-1", "RECONCILIATION_ADJUSTMENT", "")); st != 422 || b["error"] != "reconciliation_case_required" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.call("POST", "/v1/journals", w.tenant, "preparer", adj("adj-2", "RECONCILIATION_ADJUSTMENT", uuid.NewString())); st != 422 || b["error"] != "reconciliation_case_unknown" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.call("POST", "/v1/journals", w.tenant, "preparer", adj("adj-3", "CORRECTION", id)); st != 400 || b["error"] != "invalid_reconciliation_case" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := w.call("POST", "/v1/reconciliation-runs/"+id+"/resolve", w.tenant, "recon-resolver", map[string]any{"resolution_type": "MANUAL_CORRECTION", "evidence": []string{"j"}, "journal_id": uuid.NewString()}); st != 422 || b["code"] != "RESOLUTION_JOURNAL_INVALID" {
		t.Fatalf("a correction names the journal that made it: %d %v", st, b)
	}
	st, j := w.call("POST", "/v1/journals", w.tenant, "preparer", adj("adj-4", "RECONCILIATION_ADJUSTMENT", id))
	if st != 201 || j["reconciliation_case_id"] != id {
		t.Fatalf("%d %v", st, j)
	}
	jid := j["journal_id"].(string)
	// the adjustment is a journal like any other: it is not yet posted, so it cannot close the case
	if st, b := w.call("POST", "/v1/reconciliation-runs/"+id+"/resolve", w.tenant, "recon-resolver", map[string]any{"resolution_type": "MANUAL_CORRECTION", "evidence": []string{"j"}, "journal_id": jid}); st != 422 {
		t.Fatalf("an unposted adjustment cannot resolve the case: %d %v", st, b)
	}
	// the subledger was right: post +10 on the receivable, as a controlled adjustment, then close the case
	for _, step := range []string{"validate", "post"} {
		if st, b := w.call("POST", "/v1/journals/"+jid+"/"+step, w.tenant, "preparer", map[string]any{}); st != 200 {
			t.Fatalf("%s: %d %v", step, st, b)
		}
	}
	// the ledger moved (150 -> ... 130), so the same reconciliation now matches
	if st, again := w.recon("AR_GL", "130.00", "aging-1"); st != 201 || again["status"] != "MATCHED" {
		t.Fatalf("%d %v", st, again)
	}
	st, done := w.call("POST", "/v1/reconciliation-runs/"+id+"/resolve", w.tenant, "recon-resolver", map[string]any{"resolution_type": "MANUAL_CORRECTION", "note": "missing invoice booked", "evidence": []string{"journal " + jid}, "journal_id": jid})
	if st != 200 || done["status"] != "RESOLVED" || done["resolution_type"] != "MANUAL_CORRECTION" || done["resolved_by"] != "recon-resolver" {
		t.Fatalf("%d %v", st, done)
	}
	if st, _ := w.call("POST", "/v1/reconciliation-runs/"+id+"/resolve", w.tenant, "recon-resolver", map[string]any{"resolution_type": "TIMING", "evidence": []string{"x"}}); st != 409 {
		t.Fatalf("a resolved case is closed: %d", st)
	}
	// a case that is closed cannot take another adjustment
	if st, b := w.call("POST", "/v1/journals", w.tenant, "preparer", adj("adj-5", "RECONCILIATION_ADJUSTMENT", id)); st != 422 || b["error"] != "reconciliation_case_not_open" {
		t.Fatalf("%d %v", st, b)
	}

	// the history keeps every step
	st, got := w.call("GET", "/v1/reconciliation-runs/"+id, w.tenant, "recon-runner", nil)
	hist := got["history"].([]any)
	if st != 200 || len(hist) != 3 || hist[0].(map[string]any)["kind"] != "RUN" || hist[1].(map[string]any)["kind"] != "ASSIGNED" || hist[2].(map[string]any)["kind"] != "RESOLVED" {
		t.Fatalf("%d %v", st, got)
	}
	if w.count(t, `SELECT count(*) FROM eventing_outbox WHERE event_type IN ('com.zoikosuite.accounting.kernel.reconciliation.break-raised','com.zoikosuite.accounting.kernel.reconciliation.resolved')`) != 2 {
		t.Fatal("the break and its resolution are raised")
	}

	// an accepted exception is a different ending: a second break, accepted by another person with evidence
	_, second := w.recon("AR_GL", "999.00", "aging-2")
	sid := second["reconciliation_run_id"].(string)
	st, acc := w.call("POST", "/v1/reconciliation-runs/"+sid+"/accept", w.tenant, "recon-approver", map[string]any{"note": "subledger cut-off", "evidence": []string{"ticket-9"}})
	if st != 200 || acc["status"] != "ACCEPTED_EXCEPTION" || acc["approved_by"] != "recon-approver" {
		t.Fatalf("%d %v", st, acc)
	}

	// the run itself never changes
	for what, q := range map[string]string{
		"change the totals":          `UPDATE reconciliation_runs SET actual_total = 0, expected_total = 0, difference = 0 WHERE reconciliation_run_id=$1::uuid`,
		"change the populations":     `UPDATE reconciliation_runs SET population_a = '{}'::jsonb WHERE reconciliation_run_id=$1::uuid`,
		"delete the run":             `DELETE FROM reconciliation_runs WHERE reconciliation_run_id=$1::uuid`,
		"reopen a resolved case":     `UPDATE reconciliation_runs SET status='BREAK' WHERE reconciliation_run_id=$1::uuid`,
		"turn a break into a match":  `UPDATE reconciliation_runs SET status='MATCHED' WHERE reconciliation_run_id=$1::uuid`,
		"rewrite the history":        `UPDATE reconciliation_case_events SET actor='x' WHERE reconciliation_run_id=$1::uuid`,
		"delete the history":         `DELETE FROM reconciliation_case_events WHERE reconciliation_run_id=$1::uuid`,
	} {
		wantRefused(t, what, w.asApp(w.tenant, q, id), "")
	}
}

func TestReconciliationTolerancesAreVersioned(t *testing.T) {
	w := w2Setup(t)
	w.post(t, "AR.INVOICE_ISSUED", "CUST-1", inv("100.00", "100.00", "0.00"))
	tol := func(match, warn, entity string) (int, map[string]any) {
		b := map[string]any{"reconciliation_type": "AR_GL", "match_amount": match, "warning_amount": warn, "effective_from": "2026-01-01"}
		if entity != "" {
			b["legal_entity_id"] = entity
		}
		return w.call("POST", "/v1/reconciliation-tolerances", w.tenant, "recon-admin", b)
	}
	if st, _ := tol("5.00", "1.00", ""); st != 422 {
		t.Fatalf("the warning threshold cannot be below the match threshold: %d", st)
	}
	st, v1 := tol("0.50", "5.00", "")
	if st != 201 || v1["version"].(float64) != 1 {
		t.Fatalf("%d %v", st, v1)
	}
	for diff, want := range map[string]string{"99.70": "MATCHED", "97.00": "WARNING", "90.00": "BREAK"} {
		st, run := w.recon("AR_GL", diff, "r-"+diff)
		if st != 201 || run["status"] != want || run["tolerance_rule_id"] != v1["tolerance_rule_id"] || run["tolerance_version"].(float64) != 1 {
			t.Fatalf("%s: %d %v", diff, st, run)
		}
	}
	// version 2 replaces it from its date; the old runs keep the version they were judged by
	st, v2 := tol("2.00", "10.00", "")
	if st != 201 || v2["version"].(float64) != 2 {
		t.Fatalf("%d %v", st, v2)
	}
	if _, run := w.recon("AR_GL", "98.50", "r-v2"); run["status"] != "MATCHED" || run["tolerance_version"].(float64) != 2 {
		t.Fatalf("%v", run)
	}
	// an entity's own rule outranks the tenant-wide one
	if st, ent := tol("0.00", "0.00", w.entity); st != 201 || ent["version"].(float64) != 1 {
		t.Fatalf("%d %v", st, ent)
	}
	if _, run := w.recon("AR_GL", "98.50", "r-ent"); run["status"] != "BREAK" {
		t.Fatalf("the entity rule is stricter: %v", run)
	}
	st, list := w.call("GET", "/v1/reconciliation-tolerances?reconciliation_type=AR_GL", w.tenant, "recon-admin", nil)
	if st != 200 || list["total"].(float64) != 3 {
		t.Fatalf("%d %v", st, list)
	}
	wantRefused(t, "change a tolerance", w.asApp(w.tenant, `UPDATE reconciliation_tolerances SET warning_amount = 1000`), "append-only")
	wantRefused(t, "delete a tolerance", w.asApp(w.tenant, `DELETE FROM reconciliation_tolerances`), "append-only")
}

func TestReconciliationRefusesWhatItCannotCompare(t *testing.T) {
	w := w2Setup(t)
	if st, b := w.recon("AR_GL", "", ""); st != 422 || b["code"] != "RECON_INVALID" {
		t.Fatalf("a subledger population is required: %d %v", st, b)
	}
	if st, b := w.recon("AR_GL", "12.3456789", "x"); st != 422 {
		t.Fatalf("more than six decimals: %d %v", st, b)
	}
	if st, b := w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", map[string]any{"reconciliation_type": "AR_GL", "book_id": w.book, "as_of": "2026-03-31",
		"population_a": map[string]any{"source": "s", "reference": "r", "total": "1.00", "currency": "USD"}}); st != 422 || b["family"] != "AK-FX" {
		t.Fatalf("a population in another currency: %d %v", st, b)
	}
	if st, _ := w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", map[string]any{"reconciliation_type": "AR_GL", "book_id": w.book, "as_of": "2099-01-01",
		"population_a": map[string]any{"source": "s", "reference": "r", "total": "1.00"}}); st != 422 {
		t.Fatalf("not in the future: %d", st)
	}
	if st, _ := w.call("POST", "/v1/reconciliation-runs", w.tenant, "recon-runner", map[string]any{"reconciliation_type": "PAYROLL_GL", "book_id": w.book, "as_of": "2026-03-31"}); st != 422 {
		t.Fatalf("a type that is not yet supported: %d", st)
	}
	if st, _ := w.call("POST", "/v1/reconciliation-runs", uuid.NewString(), "recon-runner", map[string]any{"reconciliation_type": "AR_GL", "book_id": w.book, "as_of": "2026-03-31",
		"population_a": map[string]any{"source": "s", "reference": "r", "total": "0"}}); st != 404 {
		t.Fatalf("another tenant cannot reconcile this book: %d", st)
	}
	if st, _ := w.call("GET", "/v1/reconciliation-runs/"+uuid.NewString(), w.tenant, "recon-runner", nil); st != 404 {
		t.Fatalf("%d", st)
	}
}

func TestEventPostingReconciliationFindsMissingConsequences(t *testing.T) {
	w := w2Setup(t)
	w.post(t, "AR.INVOICE_ISSUED", "CUST-1", inv("120.00", "100.00", "20.00"))
	// an event nothing posts: no rule covers it
	orphan := w.event(t, "AR.SOMETHING_NEW", "CUST-1", map[string]any{"amount": "5.00"})
	st, run := w.recon("EVENT_POSTING", "", "")
	if st != 201 || run["status"] != "BREAK" || run["expected_total"] != "2.000000" || run["actual_total"] != "1.000000" {
		t.Fatalf("%d %v", st, run)
	}
	missing := run["population_b"].(map[string]any)["detail"].(map[string]any)["missing_event_ids"].([]any)
	if len(missing) != 1 || missing[0] != orphan {
		t.Fatalf("%v", missing)
	}
	id := run["reconciliation_run_id"].(string)
	// reproducible later, even after the missing event gets its posting
	if st, v := w.call("POST", "/v1/reconciliation-runs/"+id+"/verify", w.tenant, "auditor", map[string]any{}); st != 200 || v["reproducible"] != true {
		t.Fatalf("%d %v", st, v)
	}
	// a ledger reconciliation is reproducible from its watermark after more entries post
	w.post(t, "AR.INVOICE_ISSUED", "CUST-2", inv("10.00", "10.00", "0.00"))
	_, ar := w.recon("AR_GL", "120.00", "aging-3")
	w.post(t, "AR.INVOICE_ISSUED", "CUST-2", inv("10.00", "10.00", "0.00"))
	if st, v := w.call("POST", "/v1/reconciliation-runs/"+ar["reconciliation_run_id"].(string)+"/verify", w.tenant, "auditor", map[string]any{}); st != 200 || v["reproducible"] != true {
		t.Fatalf("the position at the watermark is the same whatever posted after: %d %v", st, v)
	}
}

func TestReconciliationStatusForTheCloseGate(t *testing.T) {
	w := w2Setup(t)
	status := func() map[string]any {
		st, b := w.call("GET", "/v1/reconciliation-status?book_id="+w.book+"&fiscal_period=2026-03", w.tenant, "close-svc", nil)
		if st != 200 {
			t.Fatalf("%d %v", st, b)
		}
		return b
	}
	if s := status(); s["clear"] != true || len(s["required_types_not_run"].([]any)) != 4 {
		t.Fatalf("nothing is open, but nothing has been reconciled either: %v", s)
	}
	w.post(t, "AR.INVOICE_ISSUED", "CUST-1", inv("120.00", "100.00", "20.00"))
	_, matched := w.recon("AR_GL", "120.00", "a1")
	if s := status(); s["clear"] != true || s["latest_status_by_type"].(map[string]any)["AR_GL"] != "MATCHED" {
		t.Fatalf("%v", s)
	}
	_, brk := w.recon("AR_GL", "100.00", "a2")
	s := status()
	if s["clear"] != false || s["open_breaks"].(float64) != 1 || len(s["open_case_ids"].([]any)) != 1 || s["open_case_ids"].([]any)[0] != brk["reconciliation_run_id"] {
		t.Fatalf("an open break blocks the close: %v", s)
	}
	_ = matched
	if st, _ := w.call("POST", "/v1/reconciliation-runs/"+brk["reconciliation_run_id"].(string)+"/resolve", w.tenant, "recon-resolver", map[string]any{"resolution_type": "TIMING", "evidence": []string{"cut-off"}}); st != 200 {
		t.Fatal("resolve")
	}
	if s := status(); s["clear"] != true {
		t.Fatalf("%v", s)
	}
	// a failed posting also blocks the close until it is settled
	w.kenv.post(w.event(t, "AR.INVOICE_ISSUED", "", inv("1.00", "1.00", "0.00")), w.book, nil)
	if s := status(); s["clear"] != false || s["failed_posting_executions"].(float64) != 1 {
		t.Fatalf("%v", s)
	}
	if st, _ := w.call("GET", "/v1/reconciliation-status?book_id="+w.book, w.tenant, "close-svc", nil); st != 400 {
		t.Fatalf("a period is required: %d", st)
	}
}

func TestOnlyOneApproverCanAcceptACase(t *testing.T) {
	w := w2Setup(t)
	w.post(t, "AR.INVOICE_ISSUED", "CUST-1", inv("120.00", "100.00", "20.00"))
	_, run := w.recon("AR_GL", "100.00", "race")
	id := run["reconciliation_run_id"].(string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, lost := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, _ := w.call("POST", "/v1/reconciliation-runs/"+id+"/accept", w.tenant, fmt.Sprintf("approver-%d", i), map[string]any{"note": "cut-off", "evidence": []string{"t"}})
			mu.Lock()
			defer mu.Unlock()
			switch st {
			case 200:
				won++
			case 409:
				lost++
			default:
				t.Errorf("unexpected %d", st)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 || lost != 5 {
		t.Fatalf("won %d lost %d", won, lost)
	}
	if w.count(t, `SELECT count(*) FROM reconciliation_case_events WHERE reconciliation_run_id=$1::uuid AND kind='ACCEPTED'`, id) != 1 {
		t.Fatal("one acceptance on record")
	}
}

func TestRulePackInstantiationIsGovernedAndRepeatable(t *testing.T) {
	e := newKEnv(t)
	if st, b := e.call("GET", "/v1/posting-rule-packs", e.tenant, "builder", nil); st != 200 || len(b["packs"].([]any)) != 3 {
		t.Fatalf("%d %v", st, b)
	}
	body := map[string]any{"version": "1.0.0", "effective_from": "2026-01-01", "book_type": "PRIMARY_STATUTORY", "accounting_framework_id": "IFRS"}
	st, b := e.call("POST", "/v1/posting-rule-packs/bank-standard/instantiate", e.tenant, "builder", body)
	if st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	id := b["rule_sets"].([]any)[0].(map[string]any)["posting_rule_set_id"].(string)
	if st, _ := e.call("POST", "/v1/posting-rule-packs/bank-standard/instantiate", e.tenant, "builder", body); st != 409 {
		t.Fatalf("the same version twice: %d", st)
	}
	if st, _ := e.call("POST", "/v1/posting-rule-packs/nope/instantiate", e.tenant, "builder", body); st != 404 {
		t.Fatalf("%d", st)
	}
	// instantiating does not activate: the author still cannot approve their own drafts
	book := e.activeBook(t, "STAT")
	if st, _ := e.call("POST", "/v1/posting-rule-sets/"+id+"/test", e.tenant, "builder", map[string]any{"book_id": book}); st != 200 {
		t.Fatal("test")
	}
	if st, _ := e.call("POST", "/v1/posting-rule-sets/"+id+"/approve", e.tenant, "builder", map[string]any{}); st != 403 {
		t.Fatalf("the author cannot approve a pack rule: %d", st)
	}
}
