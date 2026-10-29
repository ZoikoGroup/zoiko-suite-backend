package store_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	glclose "zoiko.io/general-ledger-svc/internal/close"
	"zoiko.io/general-ledger-svc/internal/domain"
	"zoiko.io/general-ledger-svc/internal/handler"
	"zoiko.io/general-ledger-svc/internal/kernel"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// Wave 1 (posting core) against real Postgres as the ordinary role: atomic posting, replay safety, the period gate,
// numbering, immutability, reversal lineage, the manual-journal controls and the failure-injection cases of standard §40.

type fakePeriods struct {
	mu     sync.Mutex
	states map[string]string
	down   bool
}

func (f *fakePeriods) set(period, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == "" {
		delete(f.states, period)
		return
	}
	f.states[period] = state
}

func (f *fakePeriods) setDown(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = v
}

func (f *fakePeriods) check(period string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return domain.ErrCloseServiceUnavailable
	}
	switch f.states[period] {
	case "CLOSED":
		return domain.ErrPeriodHardClosed
	case "SOFT":
		return domain.ErrSoftCloseOverrideRequired
	}
	return nil
}

func (f *fakePeriods) CheckPeriodOpen(_ context.Context, _, _, period string) error {
	return f.check(period)
}

func (f *fakePeriods) CheckPeriodOpenAt(_ context.Context, _ string, ref glclose.PeriodRef) error {
	return f.check(ref.PeriodName)
}

type nopPublisher struct{}

func (nopPublisher) PublishJournalCreated(context.Context, domain.JournalHeader)   {}
func (nopPublisher) PublishJournalValidated(context.Context, domain.JournalHeader) {}
func (nopPublisher) PublishJournalPosted(context.Context, domain.JournalHeader)    {}
func (nopPublisher) PublishJournalReversed(context.Context, domain.JournalHeader, string) {
}

// scalar runs a query as the administrator and returns the first column as text.
func (e *kenv) scalar(t *testing.T, q string, args ...any) string {
	t.Helper()
	var s *string
	if err := e.admin.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (e *kenv) postSetup(t *testing.T) (book, rule string) {
	t.Helper()
	e.seedChart(t)
	book = e.activeBook(t, "STAT")
	rule = e.runRuleToActive(t, "1.0.0", book)
	return book, rule
}

func (e *kenv) registerEvent(t *testing.T, src, gross, net, tax string) string {
	t.Helper()
	st, ev := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", eventBody(e.entity, src, gross, net, tax))
	if st != 201 {
		t.Fatalf("register %s: %d %v", src, st, ev)
	}
	return ev["accounting_event_id"].(string)
}

func (e *kenv) post(eventID, book string, extra map[string]any) (int, map[string]any) {
	body := map[string]any{"accounting_event_id": eventID, "book_id": book}
	for k, v := range extra {
		body[k] = v
	}
	return e.call("POST", "/v1/accounting-postings", e.tenant, "poster", body)
}

func (e *kenv) mustPost(t *testing.T, eventID, book string) map[string]any {
	t.Helper()
	st, res := e.post(eventID, book, nil)
	if st != 201 {
		t.Fatalf("post: %d %v", st, res)
	}
	return res
}

func (e *kenv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestKernelPostingGolden(t *testing.T) {
	e := newKEnv(t)
	book, ruleID := e.postSetup(t)
	ev := e.registerEvent(t, "se-1", "120.00", "100.00", "20.00")

	res := e.mustPost(t, ev, book)
	jid := res["journal_id"].(string)
	if res["journal_number"] != "JE-2026-000001" || res["rule_version"] != "1.0.0" || res["posting_rule_set_id"] != ruleID || res["existing"] != false {
		t.Fatalf("%v", res)
	}

	// the journal: posted, numbered, sealed, linked to its event, book and execution
	h := func(col string) string {
		return e.scalar(t, `SELECT `+col+`::text FROM journal_headers WHERE journal_id=$1::uuid`, jid)
	}
	if h("status") != "FINALIZED" || h("approval_status") != "POSTED" || h("journal_number") != "JE-2026-000001" || len(h("journal_hash")) != 64 {
		t.Fatalf("header: %s %s %s %s", h("status"), h("approval_status"), h("journal_number"), h("journal_hash"))
	}
	if h("source_accounting_event_id") != ev || h("accounting_book_id") != book || h("posting_execution_id") != res["posting_execution_id"] || h("posted_by_principal_id") != "poster" || h("book_id") != "STAT" {
		t.Fatal("the journal is not linked to its event, book and execution")
	}
	if h("currency_code") != "GBP" || h("posting_date") != "2026-03-10" || h("fiscal_period") != "2026-03" {
		t.Fatalf("dates and currency: %s %s %s", h("currency_code"), h("posting_date"), h("fiscal_period"))
	}

	// the lines: exact, with the rule template that generated each one
	type line struct{ acct, debit, credit, tx, ccy, ref string }
	want := []line{{"1100", "120.00", "0.00", "120.00000000", "GBP", "L1"}, {"4000", "0.00", "100.00", "100.00000000", "GBP", "L2"}, {"2200", "0.00", "20.00", "20.00000000", "GBP", "L3"}}
	rows, err := e.admin.Query(context.Background(), `SELECT account_code, debit_amount::text, credit_amount::text, transaction_amount::text, transaction_currency, posting_rule_line_ref
		FROM journal_lines WHERE journal_id=$1::uuid ORDER BY line_number`, jid)
	if err != nil {
		t.Fatal(err)
	}
	var got []line
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.acct, &l.debit, &l.credit, &l.tx, &l.ccy, &l.ref); err != nil {
			t.Fatal(err)
		}
		got = append(got, l)
	}
	rows.Close()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("lines\n got %v\nwant %v", got, want)
	}
	if e.count(t, `SELECT count(*) FROM ledger_entries WHERE journal_id=$1::uuid AND book_id='STAT'`, jid) != 3 {
		t.Fatal("the ledger entries")
	}

	// the execution and the uniqueness record
	exec := res["posting_execution_id"].(string)
	if e.scalar(t, `SELECT status FROM posting_executions WHERE execution_id=$1::uuid`, exec) != "COMMITTED" ||
		e.scalar(t, `SELECT proposal_hash FROM posting_executions WHERE execution_id=$1::uuid`, exec) != res["proposal_hash"] {
		t.Fatal("the execution")
	}
	if e.scalar(t, `SELECT ledger_watermark::text FROM posting_executions WHERE execution_id=$1::uuid`, exec) != e.scalar(t, `SELECT max(entry_seq)::text FROM ledger_entries WHERE journal_id=$1::uuid`, jid) {
		t.Fatal("the ledger watermark is the last entry the posting wrote")
	}
	if e.count(t, `SELECT count(*) FROM event_posting_links WHERE accounting_event_id=$1::uuid AND accounting_book_id=$2::uuid`, ev, book) != 1 {
		t.Fatal("the event-to-posting record")
	}
	if e.count(t, `SELECT count(*) FROM eventing_outbox WHERE aggregate_id=$1 AND event_type LIKE '%journal.posted' AND position($2 in convert_from(payload,'UTF8'))>0`, jid, "JE-2026-000001") != 1 {
		t.Fatal("the journal.posted event is on the outbox with the journal number")
	}

	// a repeat returns the same consequence and writes nothing
	st, again := e.post(ev, book, nil)
	if st != 200 || again["journal_id"] != jid || again["existing"] != true {
		t.Fatalf("%d %v", st, again)
	}

	// the trace resolves the same posting from the journal, the event and the execution
	for _, q := range []string{"journal_id=" + jid, "accounting_event_id=" + ev, "posting_execution_id=" + exec} {
		st, tr := e.call("GET", "/v1/posting-traces?"+q, e.tenant, "auditor", nil)
		if st != 200 {
			t.Fatalf("%s: %d %v", q, st, tr)
		}
		event := tr["accounting_event"].(map[string]any)
		posts := tr["postings"].([]any)
		if event["payload_hash"] == "" || event["source_event_id"] != "se-1" || len(posts) != 1 {
			t.Fatalf("%s: %v", q, tr)
		}
		p := posts[0].(map[string]any)
		rule := p["posting_rule"].(map[string]any)
		ex := p["posting_execution"].(map[string]any)
		js := p["journals"].([]any)
		if rule["version"] != "1.0.0" || rule["content_hash"] == "" || ex["status"] != "COMMITTED" || ex["journal_event_id"] == "" || len(js) != 1 {
			t.Fatalf("%s: %v", q, p)
		}
		j := js[0].(map[string]any)
		ls := j["lines"].([]any)
		l0 := ls[0].(map[string]any)
		if j["role"] != "POSTING" || j["journal_number"] != "JE-2026-000001" || len(ls) != 3 || l0["posting_rule_line_ref"] != "L1" || l0["ledger_entry_seq"] == nil {
			t.Fatalf("%s: %v", q, j)
		}
	}
	if st, _ := e.call("GET", "/v1/posting-traces", e.tenant, "auditor", nil); st != 400 {
		t.Fatalf("a trace needs exactly one identifier: %d", st)
	}
	if st, _ := e.call("GET", "/v1/posting-traces?journal_id="+jid, uuid.NewString(), "auditor", nil); st != 404 {
		t.Fatalf("another tenant cannot trace this journal: %d", st)
	}
}

func TestKernelPostingIsIdempotentUnderConcurrency(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	ev := e.registerEvent(t, "se-dup", "120.00", "100.00", "20.00")

	var wg sync.WaitGroup
	var mu sync.Mutex
	created, replayed, other := 0, 0, 0
	ids := map[string]bool{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, res := e.post(ev, book, nil)
			mu.Lock()
			defer mu.Unlock()
			switch st {
			case 201:
				created++
			case 200:
				replayed++
			default:
				other++
				t.Errorf("%d %v", st, res)
			}
			if id, ok := res["journal_id"].(string); ok {
				ids[id] = true
			}
		}()
	}
	wg.Wait()
	if created != 1 || replayed != 9 || other != 0 || len(ids) != 1 {
		t.Fatalf("created %d, replayed %d, other %d, distinct journals %d", created, replayed, other, len(ids))
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM journal_headers`: 1, `SELECT count(*) FROM journal_lines`: 3, `SELECT count(*) FROM ledger_entries`: 3,
		`SELECT count(*) FROM event_posting_links`: 1, `SELECT count(*) FROM posting_executions WHERE kind='KERNEL_EVENT'`: 1,
		`SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%journal.posted'`: 1, `SELECT last_number::int FROM journal_number_counters`: 1,
	} {
		if got := e.count(t, q); got != want {
			t.Fatalf("%s: %d, want %d", q, got, want)
		}
	}
}

func TestKernelPostingPeriodGate(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	ev := e.registerEvent(t, "se-p", "120.00", "100.00", "20.00")

	// a closed period: refused, nothing posted, and the refusal is recorded and raised
	e.periods.set("2026-03", "CLOSED")
	st, b := e.post(ev, book, nil)
	if st != 409 || b["family"] != "AK-PERIOD" || b["code"] != "PERIOD_CLOSED" {
		t.Fatalf("%d %v", st, b)
	}
	if e.count(t, `SELECT count(*) FROM journal_headers`) != 0 || e.count(t, `SELECT count(*) FROM ledger_entries`) != 0 || e.count(t, `SELECT count(*) FROM journal_number_counters`) != 0 {
		t.Fatal("a closed period must leave no financial consequence")
	}
	if e.count(t, `SELECT count(*) FROM posting_executions WHERE status='FAILED' AND accounting_event_id=$1::uuid AND failure_reason LIKE 'AK-PERIOD PERIOD_CLOSED%'`, ev) != 1 ||
		e.count(t, `SELECT count(*) FROM eventing_outbox WHERE event_type='com.zoikosuite.accounting.kernel.posting.exception-raised'`) != 1 {
		t.Fatal("the failed posting is recorded and raised")
	}

	// soft close: an explicit, reasoned, separately authorized override
	e.periods.set("2026-03", "SOFT")
	if st, b = e.post(ev, book, nil); st != 409 || b["code"] != "SOFT_CLOSE_OVERRIDE_REQUIRED" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b = e.post(ev, book, map[string]any{"override_soft_close": true}); st != 409 || b["code"] != "OVERRIDE_REASON_REQUIRED" {
		t.Fatalf("%d %v", st, b)
	}
	e.denied.Store("poster|GL_SOFT_CLOSE_POSTING_OVERRIDE", true)
	if st, b = e.post(ev, book, map[string]any{"override_soft_close": true, "soft_close_override_reason": "late invoice"}); st != 403 || b["code"] != "OVERRIDE_NOT_AUTHORIZED" {
		t.Fatalf("%d %v", st, b)
	}
	e.denied.Delete("poster|GL_SOFT_CLOSE_POSTING_OVERRIDE")

	// the period service is down: nothing posts, and that is not recorded as an accounting failure
	failedBefore := e.count(t, `SELECT count(*) FROM posting_executions WHERE accounting_event_id=$1::uuid`, ev)
	e.periods.setDown(true)
	if st, b = e.post(ev, book, nil); st != 503 || b["family"] != "AK-EXT" {
		t.Fatalf("%d %v", st, b)
	}
	if e.count(t, `SELECT count(*) FROM posting_executions WHERE accounting_event_id=$1::uuid`, ev) != failedBefore+1 && e.count(t, `SELECT count(*) FROM journal_headers`) != 0 {
		t.Fatal("a dependency outage must not post")
	}
	e.periods.setDown(false)

	st, res := e.post(ev, book, map[string]any{"override_soft_close": true, "soft_close_override_reason": "late invoice"})
	if st != 201 {
		t.Fatalf("%d %v", st, res)
	}
	if e.scalar(t, `SELECT soft_close_override_reason FROM journal_headers WHERE journal_id=$1::uuid`, res["journal_id"].(string)) != "late invoice" {
		t.Fatal("the override reason is evidence on the journal")
	}
	// the earlier failed attempts are no longer outstanding once the event has posted
	if e.count(t, `SELECT count(*) FROM posting_executions WHERE accounting_event_id=$1::uuid AND status IN ('FAILED','QUARANTINED')`, ev) != 0 ||
		e.count(t, `SELECT count(*) FROM posting_executions WHERE accounting_event_id=$1::uuid AND status='RESOLVED'`, ev) < 1 {
		t.Fatal("failed attempts must be resolved by the successful posting")
	}
}

func TestKernelPostingRefusesInactiveAccount(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	ev := e.registerEvent(t, "se-coa", "120.00", "100.00", "20.00")
	if err := e.exec(`UPDATE chart_of_accounts SET status='INACTIVE' WHERE tenant_id=$1::uuid AND account_code='1100'`, e.tenant); err != nil {
		t.Fatal(err)
	}
	st, b := e.post(ev, book, nil)
	if st != 422 || b["family"] != "AK-COA" {
		t.Fatalf("%d %v", st, b)
	}
	if e.count(t, `SELECT count(*) FROM journal_headers`) != 0 || e.count(t, `SELECT count(*) FROM posting_executions WHERE status='FAILED'`) != 1 {
		t.Fatal("an inactive account posts nothing and leaves a failed execution")
	}
	if err := e.exec(`UPDATE chart_of_accounts SET status='ACTIVE' WHERE tenant_id=$1::uuid AND account_code='1100'`, e.tenant); err != nil {
		t.Fatal(err)
	}
	e.mustPost(t, ev, book)
}

func TestKernelPostingKeepsHistoryWhenTheRuleChanges(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	ev1 := e.registerEvent(t, "se-old", "120.00", "100.00", "20.00")
	first := e.mustPost(t, ev1, book)

	// version 2 books revenue to another account from 1 April
	if err := e.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,'4100','4100','REVENUE','seed')`, uuid.NewString(), e.tenant); err != nil {
		t.Fatal(err)
	}
	if err := e.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,'REV2','4100','2026-01-01','seed')`, uuid.NewString(), e.tenant); err != nil {
		t.Fatal(err)
	}
	v2 := invoiceRule("2.0.0")
	v2["effective_from"] = "2026-04-01T00:00:00Z"
	v2["content"].(map[string]any)["line_templates"].([]any)[1].(map[string]any)["account"] = map[string]any{"mapping_key": "REV2"}
	tp := v2["test_pack"].([]any)[0].(map[string]any)
	tp["accounts"].(map[string]any)["REV2"] = "4100"
	tp["expect"].(map[string]any)["lines"].([]any)[1].(map[string]any)["account_code"] = "4100"
	st, rs := e.call("POST", "/v1/posting-rule-sets", e.tenant, "builder", v2)
	if st != 201 {
		t.Fatalf("%d %v", st, rs)
	}
	id := rs["posting_rule_set_id"].(string)
	for _, step := range [][2]string{{"test", "builder"}, {"approve", "approver"}, {"activate", "releaser"}} {
		if st, b := e.call("POST", "/v1/posting-rule-sets/"+id+"/"+step[0], e.tenant, step[1], map[string]any{"book_id": book}); st != 200 {
			t.Fatalf("%s: %d %v", step[0], st, b)
		}
	}

	ev2 := eventBody(e.entity, "se-new", "120.00", "100.00", "20.00")
	ev2["occurred_at"], ev2["document_date"] = "2026-04-10T09:00:00Z", "2026-04-10T00:00:00Z"
	st, evr := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", ev2)
	if st != 201 {
		t.Fatalf("%d %v", st, evr)
	}
	second := e.mustPost(t, evr["accounting_event_id"].(string), book)
	if second["rule_version"] != "2.0.0" || e.scalar(t, `SELECT account_code FROM journal_lines WHERE journal_id=$1::uuid AND posting_rule_line_ref='L2'`, second["journal_id"].(string)) != "4100" {
		t.Fatalf("the new event posts under the new version: %v", second)
	}
	// the old event is still answered by the version it posted under, and its journal is untouched
	st, replay := e.post(ev1, book, nil)
	if st != 200 || replay["rule_version"] != "1.0.0" || replay["journal_id"] != first["journal_id"] {
		t.Fatalf("%d %v", st, replay)
	}
	if e.scalar(t, `SELECT account_code FROM journal_lines WHERE journal_id=$1::uuid AND posting_rule_line_ref='L2'`, first["journal_id"].(string)) != "4000" {
		t.Fatal("history is not rewritten")
	}
}

func TestKernelPostingForeignCurrencyKeepsTheOriginalBasis(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	body := eventBody(e.entity, "se-usd", "120.00", "100.00", "20.00")
	body["transaction_currency"] = "USD"
	st, evr := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", body)
	if st != 201 {
		t.Fatalf("%d %v", st, evr)
	}
	ev := evr["accounting_event_id"].(string)
	if st, b := e.post(ev, book, nil); st != 422 || b["family"] != "AK-FX" {
		t.Fatalf("no rate, no posting: %d %v", st, b)
	}
	// a caller does not bring its own rate where the kernel has a rate store
	if st, b := e.post(ev, book, map[string]any{"fx_basis": map[string]any{"rate": "0.8", "rate_type": "SPOT", "source": "treasury", "as_of": "2026-03-10"}}); st != 422 || b["code"] != "RATE_OVERRIDE_NOT_ALLOWED" {
		t.Fatalf("%d %v", st, b)
	}
	st, rate := e.call("POST", "/v1/fx-rates", e.tenant, "treasury-feed", map[string]any{"base_currency": "USD", "quote_currency": "GBP", "rate": "0.8", "rate_type": "SPOT",
		"effective_at": "2026-03-10T08:00:00Z", "source": "treasury", "source_rate_id": "usdgbp-0310"})
	if st != 201 {
		t.Fatalf("%d %v", st, rate)
	}
	st, res := e.post(ev, book, nil)
	if st != 201 {
		t.Fatalf("%d %v", st, res)
	}
	jid := res["journal_id"].(string)
	l := func(col string) string {
		return e.scalar(t, `SELECT `+col+`::text FROM journal_lines WHERE journal_id=$1::uuid AND posting_rule_line_ref='L1'`, jid)
	}
	if l("debit_amount") != "96.00" || l("transaction_amount") != "120.00000000" || l("transaction_currency") != "USD" || l("exchange_rate") != "0.800000000000" || l("rate_ref") != rate["fx_rate_id"] {
		t.Fatalf("%s %s %s %s %s", l("debit_amount"), l("transaction_amount"), l("transaction_currency"), l("exchange_rate"), l("rate_ref"))
	}
	if e.scalar(t, `SELECT currency_code FROM journal_headers WHERE journal_id=$1::uuid`, jid) != "GBP" {
		t.Fatal("the journal is in the book's functional currency")
	}
}

func TestPostedJournalsAreImmutable(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	jid := e.mustPost(t, e.registerEvent(t, "se-imm", "120.00", "100.00", "20.00"), book)["journal_id"].(string)

	for what, q := range map[string]string{
		"edit the description":       `UPDATE journal_headers SET description='changed' WHERE journal_id=$1::uuid`,
		"edit the posting date":      `UPDATE journal_headers SET posting_date='2026-01-01' WHERE journal_id=$1::uuid`,
		"edit the number":            `UPDATE journal_headers SET journal_number='JE-2026-999999' WHERE journal_id=$1::uuid`,
		"edit the hash":              `UPDATE journal_headers SET journal_hash='x' WHERE journal_id=$1::uuid`,
		"take it back to pending":    `UPDATE journal_headers SET status='PENDING' WHERE journal_id=$1::uuid`,
		"delete it":                  `DELETE FROM journal_headers WHERE journal_id=$1::uuid`,
		"edit a line":                `UPDATE journal_lines SET debit_amount=999 WHERE journal_id=$1::uuid AND line_number=1`,
		"delete a line":              `DELETE FROM journal_lines WHERE journal_id=$1::uuid AND line_number=1`,
		"add a line to a sealed one": `INSERT INTO journal_lines (journal_line_id, journal_id, tenant_id, line_number, account_code, debit_amount, credit_amount) SELECT gen_random_uuid(), journal_id, tenant_id, 9, '1100', 1, 0 FROM journal_headers WHERE journal_id=$1::uuid`,
	} {
		wantRefused(t, what, e.asApp(e.tenant, q, jid), "")
	}
	// the only legal change to a posted journal is the reversal flag
	if err := e.asApp(e.tenant, `UPDATE journal_headers SET status='REVERSED', reversed_by_principal_id='x', reversed_at=now() WHERE journal_id=$1::uuid`, jid); err != nil {
		t.Fatalf("FINALIZED to REVERSED is the one allowed move: %v", err)
	}
	wantRefused(t, "reverse it back", e.asApp(e.tenant, `UPDATE journal_headers SET status='FINALIZED' WHERE journal_id=$1::uuid`, jid), "")
}

func TestJournalSealRefusesAnUnbalancedJournal(t *testing.T) {
	e := newKEnv(t)
	ctx := svcmiddleware.WithTenant(context.Background(), e.tenant)
	h := &domain.JournalHeader{
		JournalID: uuid.NewString(), TenantID: e.tenant, LegalEntityID: e.entity, FiscalPeriod: "2026-03", Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: domain.NewDate(2026, 3, 10), PostingDate: domain.NewDate(2026, 3, 10), CurrencyCode: "GBP",
		CreatedByPrincipalID: "u", CorrelationID: "unbalanced-1", ApprovalStatus: domain.ApprovalStatusDraft,
	}
	// one cent out of balance (standard §40.2 A2)
	lines := []domain.JournalLine{{AccountCode: "1000", DebitAmount: 100.01}, {AccountCode: "4000", CreditAmount: 100}}
	if _, _, err := e.pg.CreateJournal(ctx, h, lines); err != nil {
		t.Fatal(err)
	}
	if err := e.pg.TransitionJournal(ctx, e.tenant, h.JournalID, domain.JournalStatusPending, domain.JournalStatusValidated, "u"); err != nil {
		t.Fatal(err)
	}
	err := e.pg.TransitionJournal(ctx, e.tenant, h.JournalID, domain.JournalStatusValidated, domain.JournalStatusFinalized, "u")
	if !errors.Is(err, domain.ErrUnbalancedJournal) {
		t.Fatalf("the database must refuse to post it: %v", err)
	}
	if e.count(t, `SELECT count(*) FROM ledger_entries`) != 0 || e.scalar(t, `SELECT status FROM journal_headers WHERE journal_id=$1::uuid`, h.JournalID) != "VALIDATED" {
		t.Fatal("no ledger line may be committed, and the journal stays unposted")
	}
}

func TestKernelPostingIsAtomicWhenTheLastWriteFails(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	ev := e.registerEvent(t, "se-atomic", "120.00", "100.00", "20.00")

	// inject a failure into the last write of the posting transaction
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION test_boom() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected failure at the last write'; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER test_boom BEFORE INSERT ON event_posting_links FOR EACH ROW EXECUTE FUNCTION test_boom()`,
	} {
		if err := e.exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if st, b := e.post(ev, book, nil); st != 503 {
		t.Fatalf("%d %v", st, b)
	}
	for _, q := range []string{
		`SELECT count(*) FROM journal_headers`, `SELECT count(*) FROM journal_lines`, `SELECT count(*) FROM ledger_entries`, `SELECT count(*) FROM posting_executions`,
		`SELECT count(*) FROM journal_number_counters`, `SELECT count(*) FROM event_posting_links`, `SELECT count(*) FROM eventing_outbox WHERE event_type LIKE '%journal.posted'`,
	} {
		if n := e.count(t, q); n != 0 {
			t.Fatalf("%s: %d — a failed posting left a partial consequence", q, n)
		}
	}
	if err := e.exec(`DROP TRIGGER test_boom ON event_posting_links`); err != nil {
		t.Fatal(err)
	}
	// the number the failed attempt would have used is still free
	if res := e.mustPost(t, ev, book); res["journal_number"] != "JE-2026-000001" {
		t.Fatalf("%v", res)
	}
}

type ackLostRepo struct{ kernel.Repo }

func (r ackLostRepo) PostUnit(ctx context.Context, u *kernel.PostingUnit) (*kernel.PostResult, error) {
	if _, err := r.Repo.PostUnit(ctx, u); err != nil {
		return nil, err
	}
	return nil, errors.New("connection reset after commit")
}

func TestKernelPostingSurvivesALostAcknowledgement(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	ev := e.registerEvent(t, "se-ack", "120.00", "100.00", "20.00")

	svc := &kernel.Service{Repo: ackLostRepo{e.repo}}
	dir := kernel.TenantDirectory{Tenant: e.tenant, Ref: e.repo, Units: fakeUnits{}}
	gate := handler.ClosePeriodGate{Close: e.periods, Authz: fakeAuthz{e}}
	if _, err := svc.PostEvent(e.ctx(), e.tenant, kernel.Actor{PrincipalID: "worker"}, kernel.PostRequest{AccountingEventID: ev, BookID: book}, dir, gate); err == nil {
		t.Fatal("the caller saw a failure")
	}
	// the caller retries: it gets the consequence that committed, not a second one
	st, res := e.post(ev, book, nil)
	if st != 200 || res["existing"] != true {
		t.Fatalf("%d %v", st, res)
	}
	if e.count(t, `SELECT count(*) FROM journal_headers`) != 1 || e.count(t, `SELECT count(*) FROM ledger_entries`) != 3 {
		t.Fatal("exactly one consequence")
	}
}

func TestKernelPostedJournalsBalanceForAnyFacts(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	rng := rand.New(rand.NewSource(42))
	money := func(c int) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }
	posted, refused := 0, 0
	for i := 0; i < 60; i++ {
		net, tax := 1+rng.Intn(500000), rng.Intn(100000)
		gross := net + tax
		if i%4 == 3 {
			gross++ // one cent out of balance
		}
		ev := e.registerEvent(t, fmt.Sprintf("se-prop-%d", i), money(gross), money(net), money(tax))
		st, b := e.post(ev, book, nil)
		switch {
		case st == 201 && i%4 != 3:
			posted++
		case st == 422 && i%4 == 3 && strings.HasPrefix(fmt.Sprint(b["family"]), "AK-"):
			refused++
		default:
			t.Fatalf("event %d: %d %v", i, st, b)
		}
	}
	if posted != 45 || refused != 15 {
		t.Fatalf("posted %d refused %d", posted, refused)
	}
	if e.count(t, `SELECT count(*) FROM journal_headers`) != posted {
		t.Fatal("only balanced events posted")
	}
	if e.count(t, `SELECT count(*) FROM (SELECT journal_id FROM journal_lines GROUP BY journal_id HAVING sum(debit_amount) <> sum(credit_amount)) x`) != 0 {
		t.Fatal("every posted journal balances")
	}
	if e.scalar(t, `SELECT ((SELECT sum(debit_amount) FROM ledger_entries) = (SELECT sum(credit_amount) FROM ledger_entries))::text`) != "true" {
		t.Fatal("the ledger as a whole balances")
	}
	// numbers are gap-free and unique
	if e.scalar(t, `SELECT (max(last_number) = $1::bigint)::text FROM journal_number_counters`, int64(posted)) != "true" ||
		e.count(t, `SELECT count(DISTINCT journal_number) FROM journal_headers`) != posted {
		t.Fatal("journal numbers are gap-free and unique")
	}
}

func TestReversalKeepsLineageAndForeignBasis(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	orig := e.mustPost(t, e.registerEvent(t, "se-rev", "120.00", "100.00", "20.00"), book)
	jid := orig["journal_id"].(string)

	if st, b := e.call("POST", "/v1/journals/"+jid+"/reverse", e.tenant, "reviewer", map[string]any{"reason": "wrong customer", "correlation_id": "rev-1", "correction_kind": "NOT_A_KIND"}); st != 400 {
		t.Fatalf("%d %v", st, b)
	}
	st, rev := e.call("POST", "/v1/journals/"+jid+"/reverse", e.tenant, "reviewer", map[string]any{
		"reason": "wrong customer", "correlation_id": "rev-1", "correction_kind": "ERROR_CORRECTION", "posting_date": "2026-04-01"})
	if st != 201 {
		t.Fatalf("%d %v", st, rev)
	}
	rid := rev["journal_id"].(string)
	r := func(col string) string {
		return e.scalar(t, `SELECT `+col+`::text FROM journal_headers WHERE journal_id=$1::uuid`, rid)
	}
	if r("reversal_of_journal_id") != jid || r("reversal_reason") != "wrong customer" || r("correction_kind") != "ERROR_CORRECTION" || r("posting_date") != "2026-04-01" ||
		r("journal_type") != "REVERSAL" || r("status") != "FINALIZED" || r("journal_number") != "JE-2026-000002" || len(r("journal_hash")) != 64 {
		t.Fatal("the reversal carries its lineage, number and seal")
	}
	if r("source_accounting_event_id") != e.scalar(t, `SELECT source_accounting_event_id::text FROM journal_headers WHERE journal_id=$1::uuid`, jid) || r("accounting_book_id") != book {
		t.Fatal("the reversal stays in the original's event and book")
	}
	if e.scalar(t, `SELECT status FROM journal_headers WHERE journal_id=$1::uuid`, jid) != "REVERSED" {
		t.Fatal("the original is marked reversed")
	}
	// the reversal is its own execution, under the rule version the original posted with
	exec := r("posting_execution_id")
	if exec == "" || e.scalar(t, `SELECT kind FROM posting_executions WHERE execution_id=$1::uuid`, exec) != "KERNEL_REVERSAL" ||
		e.scalar(t, `SELECT rule_version FROM posting_executions WHERE execution_id=$1::uuid`, exec) != "1.0.0" {
		t.Fatal("the reversal's execution")
	}
	// exact inverse, the same transaction basis, the same rule provenance
	if e.scalar(t, `SELECT ((SELECT sum(debit_amount) FROM journal_lines WHERE journal_id=$1::uuid) = (SELECT sum(credit_amount) FROM journal_lines WHERE journal_id=$2::uuid))::text`, jid, rid) != "true" ||
		e.scalar(t, `SELECT debit_amount::text FROM journal_lines WHERE journal_id=$1::uuid AND account_code='4000'`, rid) != "100.00" ||
		e.scalar(t, `SELECT posting_rule_line_ref FROM journal_lines WHERE journal_id=$1::uuid AND account_code='4000'`, rid) != "L2" ||
		e.scalar(t, `SELECT transaction_amount::text FROM journal_lines WHERE journal_id=$1::uuid AND account_code='4000'`, rid) != "100.00000000" {
		t.Fatal("the reversal is the exact inverse and keeps the original basis")
	}
	// the net effect on the ledger is nil
	if e.scalar(t, `SELECT (sum(debit_amount) - sum(credit_amount))::text FROM ledger_entries WHERE account_code='1100'`) != "0.00" {
		t.Fatal("the original and its reversal net to zero")
	}
	// the trace shows both journals and how they relate
	st, tr := e.call("GET", "/v1/posting-traces?journal_id="+rid, e.tenant, "auditor", nil)
	if st != 200 {
		t.Fatalf("%d %v", st, tr)
	}
	p := tr["postings"].([]any)[0].(map[string]any)
	js := p["journals"].([]any)
	rels := p["relations"].([]any)
	if len(js) != 2 || js[0].(map[string]any)["role"] != "POSTING" || js[1].(map[string]any)["role"] != "REVERSAL" || len(rels) != 1 ||
		rels[0].(map[string]any)["kind"] != "REVERSES" || rels[0].(map[string]any)["from_journal_id"] != rid {
		t.Fatalf("%v", p)
	}
	// reversing the same journal again is a retry, and a different one is refused
	if st, _ := e.call("POST", "/v1/journals/"+jid+"/reverse", e.tenant, "reviewer", map[string]any{"reason": "wrong customer", "correlation_id": "rev-1"}); st != 200 {
		t.Fatalf("a retried reversal: %d", st)
	}
	if st, _ := e.call("POST", "/v1/journals/"+jid+"/reverse", e.tenant, "reviewer", map[string]any{"reason": "again", "correlation_id": "rev-2"}); st != 422 {
		t.Fatalf("a second reversal of the same journal: %d", st)
	}
	// the reposting is a new event, never the old identity forced through again
	if st, res := e.post(orig["accounting_event_id"].(string), book, map[string]any{"posting_basis": "PRIMARY"}); st != 200 || res["journal_id"] != jid {
		t.Fatalf("the original event still resolves to its original posting: %d %v", st, res)
	}
}

// ── manual-journal controls (standard §9) ───────────────────────────────────────

func (e *kenv) manualBody(corr, jtype, purpose string, amount float64, evidence ...string) map[string]any {
	b := map[string]any{
		"tenant_id": e.tenant, "legal_entity_id": e.entity, "fiscal_period": "2026-03", "description": "manual", "correlation_id": corr, "journal_type": jtype,
		"transaction_date": "2026-03-10", "posting_date": "2026-03-10", "currency_code": "GBP",
		"lines": []any{map[string]any{"account_code": "6000", "debit_amount": amount}, map[string]any{"account_code": "2000", "credit_amount": amount}},
	}
	if purpose != "" {
		b["journal_purpose"] = purpose
	}
	if len(evidence) > 0 {
		b["evidence_refs"] = evidence
	}
	return b
}

func (e *kenv) mustCreate(t *testing.T, body map[string]any) string {
	t.Helper()
	st, b := e.call("POST", "/v1/journals", e.tenant, "preparer", body)
	if st != 201 {
		t.Fatalf("create: %d %v", st, b)
	}
	return b["journal_id"].(string)
}

func TestManualJournalPurposeIsControlled(t *testing.T) {
	e := newKEnv(t)
	if st, b := e.call("POST", "/v1/journals", e.tenant, "preparer", e.manualBody("m-1", "ADJUSTMENT", "", 50)); st != 400 || b["error"] != "journal_purpose_required" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/journals", e.tenant, "preparer", e.manualBody("m-2", "ADJUSTMENT", "BECAUSE", 50)); st != 400 || b["error"] != "invalid_journal_purpose" {
		t.Fatalf("%d %v", st, b)
	}
	id := e.mustCreate(t, e.manualBody("m-3", "ADJUSTMENT", "ACCRUAL", 50))
	if e.scalar(t, `SELECT journal_purpose FROM journal_headers WHERE journal_id=$1::uuid`, id) != "ACCRUAL" {
		t.Fatal("the purpose is stored")
	}
	// a plain standard journal needs no purpose
	e.mustCreate(t, e.manualBody("m-4", "STANDARD", "", 50))
}

func TestManualJournalEvidenceAboveTheThreshold(t *testing.T) {
	e := newKEnv(t) // the test policy: evidence above 10000.00
	big := e.mustCreate(t, e.manualBody("ev-1", "STANDARD", "", 20000))
	if st, b := e.call("POST", "/v1/journals/"+big+"/submit", e.tenant, "preparer", map[string]any{}); st != 422 || b["error"] != "evidence_required" {
		t.Fatalf("%d %v", st, b)
	}
	withDoc := e.mustCreate(t, e.manualBody("ev-2", "STANDARD", "", 20000, "doc-77"))
	if st, b := e.call("POST", "/v1/journals/"+withDoc+"/submit", e.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	small := e.mustCreate(t, e.manualBody("ev-3", "STANDARD", "", 500))
	if st, b := e.call("POST", "/v1/journals/"+small+"/submit", e.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
}

func TestManualJournalWithdrawAndCopy(t *testing.T) {
	e := newKEnv(t)
	d := e.mustCreate(t, e.manualBody("w-1", "STANDARD", "", 70))
	if st, _ := e.call("POST", "/v1/journals/"+d+"/withdraw", e.tenant, "preparer", map[string]any{}); st != 400 {
		t.Fatalf("a reason is required: %d", st)
	}
	st, b := e.call("POST", "/v1/journals/"+d+"/withdraw", e.tenant, "preparer", map[string]any{"reason": "entered twice"})
	if st != 200 || b["approval_status"] != "CANCELLED" || b["withdrawn_reason"] != "entered twice" || b["withdrawn_by_principal_id"] != "preparer" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/journals/"+d+"/withdraw", e.tenant, "preparer", map[string]any{"reason": "again"}); st != 422 || b["error"] != "not_withdrawable" {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := e.call("POST", "/v1/journals/"+d+"/submit", e.tenant, "preparer", map[string]any{}); st != 422 {
		t.Fatalf("a withdrawn journal cannot be submitted: %d", st)
	}
	if st, _ := e.call("POST", "/v1/journals/"+uuid.NewString()+"/withdraw", e.tenant, "preparer", map[string]any{"reason": "x"}); st != 404 {
		t.Fatalf("%d", st)
	}
	submitted := e.mustCreate(t, e.manualBody("w-2", "STANDARD", "", 70))
	e.call("POST", "/v1/journals/"+submitted+"/submit", e.tenant, "preparer", map[string]any{})
	if st, b := e.call("POST", "/v1/journals/"+submitted+"/withdraw", e.tenant, "preparer", map[string]any{"reason": "late"}); st != 422 {
		t.Fatalf("a submitted journal is rejected, not withdrawn: %d %v", st, b)
	}

	// a copy is a new draft with a new identity and provenance, and none of the original's posting identifiers
	book, _ := e.postSetup(t)
	posted := e.mustPost(t, e.registerEvent(t, "se-copy", "120.00", "100.00", "20.00"), book)["journal_id"].(string)
	if st, _ := e.call("POST", "/v1/journals/"+posted+"/copy", e.tenant, "preparer", map[string]any{}); st != 400 {
		t.Fatalf("a copy needs its own correlation_id: %d", st)
	}
	st, cp := e.call("POST", "/v1/journals/"+posted+"/copy", e.tenant, "preparer", map[string]any{"correlation_id": "copy-1"})
	if st != 201 || cp["journal_id"] == posted || cp["copy_of_journal_id"] != posted || cp["status"] != "PENDING" || cp["approval_status"] != "DRAFT" {
		t.Fatalf("%d %v", st, cp)
	}
	for _, k := range []string{"journal_number", "journal_hash", "posting_execution_id", "source_accounting_event_id", "accounting_book_id", "posted_at"} {
		if _, has := cp[k]; has {
			t.Fatalf("a copy must not carry %s", k)
		}
	}
	if st, again := e.call("POST", "/v1/journals/"+posted+"/copy", e.tenant, "preparer", map[string]any{"correlation_id": "copy-1"}); st != 200 || again["journal_id"] != cp["journal_id"] {
		t.Fatalf("a retried copy: %d %v", st, again)
	}
}

func TestManualJournalPostsNumberedAndFutureDatedWaits(t *testing.T) {
	e := newKEnv(t)
	id := e.mustCreate(t, e.manualBody("n-1", "STANDARD", "", 90))
	for _, step := range []string{"validate", "post"} {
		if st, b := e.call("POST", "/v1/journals/"+id+"/"+step, e.tenant, "preparer", map[string]any{}); st != 200 {
			t.Fatalf("%s: %d %v", step, st, b)
		}
	}
	if e.scalar(t, `SELECT journal_number FROM journal_headers WHERE journal_id=$1::uuid`, id) != "JE-2026-000001" || len(e.scalar(t, `SELECT journal_hash FROM journal_headers WHERE journal_id=$1::uuid`, id)) != 64 {
		t.Fatal("every posting path numbers and seals the journal")
	}

	future := e.manualBody("n-2", "STANDARD", "", 90)
	future["transaction_date"], future["posting_date"] = "2099-01-01", "2099-01-01"
	fid := e.mustCreate(t, future)
	if st, b := e.call("POST", "/v1/journals/"+fid+"/validate", e.tenant, "preparer", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := e.call("POST", "/v1/journals/"+fid+"/post", e.tenant, "preparer", map[string]any{}); st != 409 || b["error"] != "posting_date_in_future" {
		t.Fatalf("a future-dated journal waits for its date: %d %v", st, b)
	}
	if e.scalar(t, `SELECT status FROM journal_headers WHERE journal_id=$1::uuid`, fid) != "VALIDATED" {
		t.Fatal("and stays unposted")
	}
}

func TestRecurringJournalTemplates(t *testing.T) {
	e := newKEnv(t)
	e.seedChart(t)
	body := func(name string, acct string) map[string]any {
		return map[string]any{"legal_entity_id": e.entity, "name": name, "journal_type": "ACCRUAL", "journal_purpose": "ACCRUAL", "currency_code": "GBP", "description": "rent accrual",
			"lines": []any{map[string]any{"account_code": acct, "debit_amount": 100}, map[string]any{"account_code": "2000", "credit_amount": 100}}, "frequency": "MONTHLY", "first_run_date": "2026-01-31"}
	}
	st, tpl := e.call("POST", "/v1/recurring-journal-templates", e.tenant, "owner", body("rent", "6000"))
	if st != 201 || tpl["status"] != "ACTIVE" || tpl["next_run_date"] != "2026-01-31" {
		t.Fatalf("%d %v", st, tpl)
	}
	id := tpl["template_id"].(string)
	if st, _ := e.call("POST", "/v1/recurring-journal-templates", e.tenant, "owner", body("rent", "6000")); st != 409 {
		t.Fatalf("one name per entity: %d", st)
	}
	// the template is not a way round the control-account rule
	if err := e.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id, is_control_account, direct_posting_restricted) VALUES ($1,$2,'1300','AR control','ASSET','seed',true,true)`, uuid.NewString(), e.tenant); err != nil {
		t.Fatal(err)
	}
	if st, b := e.call("POST", "/v1/recurring-journal-templates", e.tenant, "owner", body("ar-adjust", "1300")); st == 201 {
		t.Fatalf("a restricted control account must be refused in a template: %v", b)
	}

	gen := func(day string) (int, map[string]any) {
		return e.call("POST", "/v1/recurring-journal-templates/"+id+"/generate", e.tenant, "scheduler", map[string]any{"run_date": day})
	}
	st, j1 := gen("2026-01-31")
	if st != 201 || j1["recurring_template_id"] != id || j1["journal_purpose"] != "ACCRUAL" || j1["status"] != "PENDING" || j1["approval_status"] != "DRAFT" || j1["posting_date"] != "2026-01-31" {
		t.Fatalf("%d %v", st, j1)
	}
	if st, again := gen("2026-01-31"); st != 200 || again["journal_id"] != j1["journal_id"] {
		t.Fatalf("generating the same run twice: %d %v", st, again)
	}
	if st, _ := gen("2026-03-31"); st != 409 {
		t.Fatalf("a run that would skip one is refused: %d", st)
	}
	// anchored on the 31st: February is the 28th, then March is the 31st again
	_, got := e.call("GET", "/v1/recurring-journal-templates/"+id, e.tenant, "owner", nil)
	if got["next_run_date"] != "2026-02-28" {
		t.Fatalf("%v", got)
	}
	if st, _ := gen("2026-02-28"); st != 201 {
		t.Fatal("february")
	}
	if _, got = e.call("GET", "/v1/recurring-journal-templates/"+id, e.tenant, "owner", nil); got["next_run_date"] != "2026-03-31" {
		t.Fatalf("%v", got)
	}
	// the template itself is never posted
	if e.count(t, `SELECT count(*) FROM journal_headers WHERE status IN ('FINALIZED','REVERSED')`) != 0 {
		t.Fatal("generation creates drafts only")
	}
	if st, _ := e.call("POST", "/v1/recurring-journal-templates/"+id+"/pause", e.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatal("pause")
	}
	if st, _ := gen("2026-03-31"); st != 409 {
		t.Fatalf("a paused template generates nothing: %d", st)
	}
	if st, _ := e.call("POST", "/v1/recurring-journal-templates/"+id+"/resume", e.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatal("resume")
	}
	if st, _ := gen("2026-03-31"); st != 201 {
		t.Fatal("march")
	}
}

func TestAccountApplicabilityRestrictsPosting(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	other := e.activeBook(t, "OTHER")
	ev := e.registerEvent(t, "se-app", "120.00", "100.00", "20.00")
	put := func(code string, body map[string]any) (int, map[string]any) {
		return e.call("PUT", "/v1/account-applicability/"+code, e.tenant, "chart-admin", body)
	}

	// revenue is valid in another book only: the posting in this book is refused and leaves nothing
	if st, b := put("4000", map[string]any{"book_ids": []string{other}}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if st, got := e.call("GET", "/v1/account-applicability/4000", e.tenant, "chart-admin", nil); st != 200 || len(got["book_ids"].([]any)) != 1 {
		t.Fatalf("%d %v", st, got)
	}
	st, b := e.post(ev, book, nil)
	if st != 422 || b["family"] != "AK-COA" || b["code"] != "ACCOUNT_NOT_IN_BOOK" {
		t.Fatalf("%d %v", st, b)
	}
	if e.count(t, `SELECT count(*) FROM journal_headers`) != 0 {
		t.Fatal("nothing posted")
	}
	if st, b = put("4000", map[string]any{"book_ids": []string{other, book}}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}

	// a currency restriction
	if st, b = put("4000", map[string]any{"book_ids": []string{other, book}, "currency_restriction": "USD"}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b = e.post(ev, book, nil); st != 422 || b["code"] != "ACCOUNT_CURRENCY_RESTRICTED" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b = put("4000", map[string]any{}); st != 200 {
		t.Fatalf("lifting the restriction: %d %v", st, b)
	}
	e.mustPost(t, ev, book)

	// the commands themselves are controlled
	if st, b = put("4000", map[string]any{"book_ids": []string{uuid.NewString()}}); st != 422 || b["code"] != "BOOK_UNKNOWN" {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ = put("4000", map[string]any{"book_ids": []string{"not-a-uuid"}}); st != 400 {
		t.Fatalf("%d", st)
	}
	if st, _ = put("9999", map[string]any{}); st != 404 {
		t.Fatalf("%d", st)
	}
	e.denied.Store("clerk|COA_ACCOUNT_AMEND", true)
	if st, _ = e.call("PUT", "/v1/account-applicability/4000", e.tenant, "clerk", map[string]any{}); st != 403 {
		t.Fatalf("%d", st)
	}
}

func TestSupersedingEventKeepsTheOriginal(t *testing.T) {
	e := newKEnv(t)
	book, _ := e.postSetup(t)
	orig := e.registerEvent(t, "se-orig", "120.00", "100.00", "20.00")
	e.mustPost(t, orig, book)

	fix := eventBody(e.entity, "se-fix", "130.00", "110.00", "20.00")
	fix["supersedes_event_id"] = orig
	st, b := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", fix)
	if st != 201 || b["supersedes_event_id"] != orig {
		t.Fatalf("%d %v", st, b)
	}
	fixID := b["accounting_event_id"].(string)
	if st, again := e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", fix); st != 200 || again["accounting_event_id"] != fixID {
		t.Fatalf("a replayed correction: %d %v", st, again)
	}
	unknown := eventBody(e.entity, "se-fix2", "1.00", "1.00", "0.00")
	unknown["supersedes_event_id"] = uuid.NewString()
	if st, b = e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", unknown); st != 422 || b["code"] != "SUPERSEDED_EVENT_UNKNOWN" {
		t.Fatalf("%d %v", st, b)
	}
	foreign := eventBody(uuid.NewString(), "se-fix3", "1.00", "1.00", "0.00")
	foreign["supersedes_event_id"] = orig
	if st, b = e.call("POST", "/v1/accounting-events", e.tenant, "ar-svc", foreign); st != 403 || b["code"] != "ENTITY_MISMATCH" {
		t.Fatalf("%d %v", st, b)
	}
	// the correction posts as its own consequence; the original posting is untouched
	e.mustPost(t, fixID, book)
	if e.count(t, `SELECT count(*) FROM event_posting_links`) != 2 {
		t.Fatal("two events, two consequences")
	}
	st, tr := e.call("GET", "/v1/posting-traces?accounting_event_id="+orig, e.tenant, "auditor", nil)
	sup, _ := tr["superseded_by_event_ids"].([]any)
	if st != 200 || len(sup) != 1 || sup[0] != fixID {
		t.Fatalf("%d %v", st, tr)
	}
}

func TestThePreparerCannotApproveTheirOwnJournal(t *testing.T) {
	e := newKEnv(t)
	call := func(path, who string) (int, map[string]any) {
		return e.call("POST", path, e.tenant, who, map[string]any{})
	}
	st, b := e.call("POST", "/v1/journals", e.tenant, "alice", e.manualBody("sod-1", "STANDARD", "", 40))
	if st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	id := b["journal_id"].(string)
	if st, b = call("/v1/journals/"+id+"/submit", "bob"); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	// alice prepared it; bob only submitted it. Neither may approve.
	for _, who := range []string{"alice", "bob"} {
		if st, b = call("/v1/journals/"+id+"/approve", who); st != 403 || b["error"] != "self_approval_not_permitted" {
			t.Fatalf("%s: %d %v", who, st, b)
		}
	}
	if st, b = call("/v1/journals/"+id+"/approve", "carol"); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
}
