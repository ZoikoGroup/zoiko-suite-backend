package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Wave 5 (intercompany pairing, translation, elimination and the group book) against real Postgres as the ordinary role.

type cenv struct {
	*kenv
	group, uk, us         string
	gBook, ukBook, usBook string
	groupID               string
}

func (c *cenv) seed(t *testing.T, entity, bookCode, ccy, day string, lines ...[]string) {
	t.Helper()
	journal := uuid.NewString()
	for i, l := range lines {
		// account, debit, credit, counterparty, transaction currency, transaction amount
		cp, txc, txa := "", "", ""
		if len(l) > 3 {
			cp = l[3]
		}
		if len(l) > 5 {
			txc, txa = l[4], l[5]
		}
		if err := c.exec(`INSERT INTO ledger_entries (ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period, journal_id, journal_line_id, line_number, account_code, debit_amount, credit_amount,
				currency_code, transaction_date, posting_date, correlation_id, created_at, counterparty_id, transaction_amount, transaction_currency)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6::uuid, $7::uuid, $8, $9, $10::numeric, $11::numeric, $12, $13::date, $13::date, 'seed', now(), NULLIF($14, ''), NULLIF($15, '')::numeric, NULLIF($16, ''))`,
			uuid.NewString(), c.tenant, entity, bookCode, day[:7], journal, uuid.NewString(), i+1, l[0], l[1], l[2], ccy, day, cp, txa, txc); err != nil {
			t.Fatal(err)
		}
	}
}

func (c *cenv) mkBook(t *testing.T, entity, code, typ, ccy string) string {
	t.Helper()
	b := bookBody(entity, code)
	b["book_type"], b["functional_currency"] = typ, ccy
	st, r := c.call("POST", "/v1/accounting-books", c.tenant, "owner", b)
	if st != 201 {
		t.Fatalf("book %s: %d %v", code, st, r)
	}
	id := r["book_id"].(string)
	if st, r = c.call("POST", "/v1/accounting-books/"+id+"/activate", c.tenant, "owner", map[string]any{}); st != 200 {
		t.Fatalf("%d %v", st, r)
	}
	return id
}

var icRoles = map[string]any{"1500": "DUE_FROM", "2500": "DUE_TO", "4500": "IC_REVENUE", "5000": "IC_EXPENSE"}

func identity(codes ...string) map[string]any {
	m := map[string]any{}
	for _, c := range codes {
		m[c] = c
	}
	return m
}

// consolSetup builds the group the acceptance scenarios use: a GBP group book on a reporting entity, a GBP parent company (UK) and a
// USD subsidiary (US), a chart, the mapping, and March's rates and ledgers.
func consolSetup(t *testing.T) *cenv {
	t.Helper()
	e := newKEnv(t)
	c := &cenv{kenv: e, group: uuid.NewString(), uk: uuid.NewString(), us: uuid.NewString()}
	for _, a := range [][2]string{{"1000", "ASSET"}, {"1500", "ASSET"}, {"2500", "LIABILITY"}, {"3000", "EQUITY"}, {"4000", "REVENUE"}, {"4500", "REVENUE"}, {"5000", "EXPENSE"},
		{"6000", "EXPENSE"}, {"7300", "REVENUE"}, {"3900", "EQUITY"}, {"6990", "EXPENSE"}} {
		if err := e.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,$3,$4,$5,'seed')`,
			uuid.NewString(), e.tenant, a[0], a[0]+" account", a[1]); err != nil {
			t.Fatal(err)
		}
	}
	for k, code := range map[string]string{"CTA": "3900", "IC_ELIM_DIFFERENCE": "6990"} {
		if err := e.exec(`INSERT INTO account_mappings (account_mapping_id, tenant_id, mapping_key, account_code, effective_from, created_by_principal_id) VALUES ($1,$2,$3,$4,'2026-01-01','seed')`,
			uuid.NewString(), e.tenant, k, code); err != nil {
			t.Fatal(err)
		}
	}
	c.gBook = c.mkBook(t, c.group, "GROUP", "GROUP_CONSOLIDATION", "GBP")
	c.ukBook = c.mkBook(t, c.uk, "STAT-UK", "PRIMARY_STATUTORY", "GBP")
	c.usBook = c.mkBook(t, c.us, "STAT-US", "PRIMARY_STATUTORY", "USD")
	st, g := c.call("POST", "/v1/consolidation-groups", c.tenant, "group-finance", map[string]any{"group_code": "GRP", "group_name": "Group", "parent_entity_id": c.group, "group_book_id": c.gBook,
		"presentation_currency": "GBP", "ic_tolerance": "2.00"})
	if st != 201 {
		t.Fatalf("group: %d %v", st, g)
	}
	c.groupID = g["group_id"].(string)
	for _, m := range [][3]string{{c.uk, c.ukBook, "100"}, {c.us, c.usBook, "100"}} {
		if st, r := c.call("POST", "/v1/consolidation-groups/"+c.groupID+"/members", c.tenant, "group-finance", map[string]any{"legal_entity_id": m[0], "source_book_id": m[1], "ownership_pct": m[2], "effective_from": "2025-01-01"}); st != 201 {
			t.Fatalf("member: %d %v", st, r)
		}
	}
	if st, r := c.call("PUT", "/v1/consolidation-groups/"+c.groupID+"/mappings", c.tenant, "group-finance", map[string]any{"account_map": identity("1000", "1500", "2500", "3000", "4000", "4500", "5000"), "ic_roles": icRoles}); st != 201 || r["version"].(float64) != 1 {
		t.Fatalf("mapping: %d %v", st, r)
	}
	e.rate("USD", "GBP", "0.80", "CLOSING", "2026-03-31T17:00:00Z", "ecb", "c-mar")
	e.rate("USD", "GBP", "0.78", "AVERAGE", "2026-03-31T17:00:00Z", "ecb", "a-mar")

	// the UK: capital, a loan to the US (USD 100 booked at 0.80) and goods sold to it (USD 50)
	c.seed(t, c.uk, "STAT-UK", "GBP", "2026-01-05", []string{"1000", "10000.00", "0"}, []string{"3000", "0", "10000.00"})
	c.seed(t, c.uk, "STAT-UK", "GBP", "2026-03-10", []string{"1500", "80.00", "0", c.us, "USD", "100.00"}, []string{"1000", "0", "80.00"})
	c.seed(t, c.uk, "STAT-UK", "GBP", "2026-03-12", []string{"1500", "40.00", "0", c.us, "USD", "50.00"}, []string{"4500", "0", "40.00", c.us, "USD", "50.00"})
	// the US: capital, the loan, the purchase from the UK and an outside sale
	c.seed(t, c.us, "STAT-US", "USD", "2026-01-05", []string{"1000", "5000.00", "0"}, []string{"3000", "0", "5000.00"})
	c.seed(t, c.us, "STAT-US", "USD", "2026-03-10", []string{"1000", "100.00", "0"}, []string{"2500", "0", "100.00", c.uk, "USD", "100.00"})
	c.seed(t, c.us, "STAT-US", "USD", "2026-03-12", []string{"5000", "50.00", "0", c.uk, "USD", "50.00"}, []string{"2500", "0", "50.00", c.uk, "USD", "50.00"})
	c.seed(t, c.us, "STAT-US", "USD", "2026-03-20", []string{"1000", "300.00", "0"}, []string{"4000", "0", "300.00"})
	return c
}

func (c *cenv) seal(t *testing.T, entity, period string) {
	t.Helper()
	if st, b := c.call("POST", "/v1/period-seals", c.tenant, "close-svc", map[string]any{"legal_entity_id": entity, "fiscal_period": period, "action": "SEAL"}); st != 201 && st != 200 {
		t.Fatalf("seal: %d %v", st, b)
	}
}

func (c *cenv) run(period string) (int, map[string]any) {
	return c.call("POST", "/v1/consolidation-runs", c.tenant, "preparer", map[string]any{"group_id": c.groupID, "fiscal_period": period})
}

func (c *cenv) mustRun(t *testing.T, period string) map[string]any {
	t.Helper()
	st, r := c.run(period)
	if st != 201 {
		t.Fatalf("run %s: %d %v", period, st, r)
	}
	return r
}

// groupBook is the net balance of every account in the group book.
func (c *cenv) groupBook(t *testing.T) map[string]string {
	t.Helper()
	rows, err := c.admin.Query(context.Background(), `SELECT account_code, (sum(debit_amount) - sum(credit_amount))::text FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND book_id = 'GROUP' GROUP BY 1 HAVING sum(debit_amount) <> sum(credit_amount)`, c.tenant, c.group)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, n string
		if err := rows.Scan(&a, &n); err != nil {
			t.Fatal(err)
		}
		out[a] = n
	}
	return out
}

func wantBook(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("group book %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("group book %v, want %v", got, want)
		}
	}
}

func (c *cenv) statutoryEntries(t *testing.T) int {
	return c.count(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1::uuid AND legal_entity_id IN ($2::uuid, $3::uuid)`, c.tenant, c.uk, c.us)
}

func TestMultiEntityConsolidationPairsTranslatesEliminatesAndCertifies(t *testing.T) {
	c := consolSetup(t)
	before := c.statutoryEntries(t)

	// the members have not closed March: a consolidation consumes closed books
	if st, b := c.run("2026-03"); st != 409 || b["code"] != "INPUT_NOT_SEALED" {
		t.Fatalf("%d %v", st, b)
	}
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")

	// the pairing report before the run: both pairs match in their transaction currency
	st, pr := c.call("GET", "/v1/intercompany-pairings?group_id="+c.groupID+"&fiscal_period=2026-03", c.tenant, "reader", nil)
	if st != 200 || pr["summary"].(map[string]any)["MATCHED"].(float64) != 2 {
		t.Fatalf("%d %v", st, pr)
	}

	run := c.mustRun(t, "2026-03")
	if run["status"] != "PROCESSED" || run["blocking_count"].(float64) != 0 || run["mapping_version"].(float64) != 1 || run["translation_journal_number"] == "" || run["elimination_journal_number"] == "" {
		t.Fatalf("%v", run)
	}
	pop := run["population"].(map[string]any)
	if len(pop["members"].([]any)) != 2 || len(pop["rates"].([]any)) != 2 {
		t.Fatalf("both members frozen as signed snapshots, one USD rate per class: %v", pop)
	}
	for _, m := range pop["members"].([]any) {
		in := m.(map[string]any)
		if in["snapshot_id"] == "" || in["content_hash"] == "" || in["sealed"] != true {
			t.Fatalf("%v", in)
		}
	}
	// 9920 + 4320 cash; equity 14000; external revenue 234; translation reserve 5; the difference of the intercompany expense 1
	wantBook(t, c.groupBook(t), map[string]string{"1000": "14240.00", "3000": "-14000.00", "4000": "-234.00", "3900": "-5.00", "6990": "-1.00"})
	cons := pop["consolidated"].(map[string]any)
	for _, gone := range []string{"1500", "2500", "4500", "5000"} {
		if _, ok := cons[gone]; ok {
			t.Fatalf("the intercompany account %s is eliminated: %v", gone, cons)
		}
	}
	for _, p := range pop["intercompany_pairs"].([]any) {
		if p.(map[string]any)["eliminated"] != true {
			t.Fatalf("%v", p)
		}
	}
	// the group results are in the group book and the members' statutory ledgers were not touched
	if after := c.statutoryEntries(t); after != before {
		t.Fatalf("the member ledgers changed: %d -> %d entries", before, after)
	}
	if n := c.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND journal_type IN ('CONSOLIDATION', 'ELIMINATION') AND journal_purpose IN ('CONSOLIDATION', 'ELIMINATION') AND status = 'FINALIZED'`, c.tenant, c.group); n != 2 {
		t.Fatalf("a translation and an elimination journal: %d", n)
	}

	// the same group and period again is the same run
	if st, again := c.run("2026-03"); st != 200 || again["consolidation_run_id"] != run["consolidation_run_id"] {
		t.Fatalf("%d %v", st, again)
	}

	id := run["consolidation_run_id"].(string)
	if st, b := c.call("POST", "/v1/consolidation-runs/"+id+"/certify", c.tenant, "preparer", nil); st != 403 || b["error"] != "separation_of_duties" {
		t.Fatalf("the preparer cannot certify their own run: %d %v", st, b)
	}
	st, cert := c.call("POST", "/v1/consolidation-runs/"+id+"/certify", c.tenant, "group-controller", nil)
	if st != 200 || cert["status"] != "CERTIFIED" || cert["certified_by"] != "group-controller" || cert["group_snapshot_id"] == "" {
		t.Fatalf("%d %v", st, cert)
	}
	snapID := cert["group_snapshot_id"].(string)
	if st, v := c.call("POST", "/v1/balance-snapshots/"+snapID+"/verify", c.tenant, "auditor", nil); st != 200 || v["verified"] != true {
		t.Fatalf("the signed group snapshot reproduces: %d %v", st, v)
	}
	if got := c.scalar(t, `SELECT purpose FROM balance_snapshots WHERE snapshot_id = $1::uuid`, snapID); got != "CONSOLIDATION_CLOSE" {
		t.Fatal(got)
	}
	if st, b := c.call("POST", "/v1/consolidation-runs/"+id+"/certify", c.tenant, "other-controller", nil); st != 409 {
		t.Fatalf("a certified run is not certified twice: %d %v", st, b)
	}
}

func TestUnmatchedIntercompanyBlocksCertificationAndSupersedeReopensThePeriod(t *testing.T) {
	c := consolSetup(t)
	// the UK booked a further USD 25 receivable the US has not recorded
	c.seed(t, c.uk, "STAT-UK", "GBP", "2026-03-25", []string{"1500", "20.00", "0", c.us, "USD", "25.00"}, []string{"1000", "0", "20.00"})
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")

	st, pr := c.call("GET", "/v1/intercompany-pairings?group_id="+c.groupID+"&fiscal_period=2026-03", c.tenant, "reader", nil)
	sum := pr["summary"].(map[string]any)
	if st != 200 || sum["AMOUNT_DIFFERENCE"].(float64) != 1 || sum["MATCHED"].(float64) != 1 {
		t.Fatalf("%d %v", st, pr)
	}

	run := c.mustRun(t, "2026-03")
	id := run["consolidation_run_id"].(string)
	if run["blocking_count"].(float64) != 1 {
		t.Fatalf("%v", run["population"].(map[string]any)["exceptions"])
	}
	exc := run["population"].(map[string]any)["exceptions"].([]any)[0].(map[string]any)
	if exc["code"] != "IC_PAIR_AMOUNT_DIFFERENCE" || exc["blocking"] != true {
		t.Fatalf("%v", exc)
	}
	// the matched pair was eliminated, the pair with a difference stays in the group figures until someone resolves it
	book := c.groupBook(t)
	if book["1500"] != "140.00" || book["2500"] != "-120.00" {
		t.Fatalf("the unmatched balance pair stays; only the matched profit-and-loss pair came out: %v", book)
	}
	if st, b := c.call("POST", "/v1/consolidation-runs/"+id+"/certify", c.tenant, "group-controller", nil); st != 422 || b["code"] != "CONSOLIDATION_BLOCKED" {
		t.Fatalf("%d %v", st, b)
	}

	// supersede withdraws the run: its journals are reversed in the period and the period can be consolidated again
	if st, b := c.call("POST", "/v1/consolidation-runs/"+id+"/supersede", c.tenant, "group-controller", map[string]any{}); st != 422 {
		t.Fatalf("a reason is required: %d %v", st, b)
	}
	st, sup := c.call("POST", "/v1/consolidation-runs/"+id+"/supersede", c.tenant, "group-controller", map[string]any{"reason": "US to record the missing receivable"})
	if st != 200 || sup["status"] != "SUPERSEDED" || sup["superseded_by"] != "group-controller" {
		t.Fatalf("%d %v", st, sup)
	}
	wantBook(t, c.groupBook(t), map[string]string{})
	if n := c.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND status = 'REVERSED'`, c.tenant, c.group); n != 2 {
		t.Fatalf("both journals of the run are reversed: %d", n)
	}
	if st, b := c.call("POST", "/v1/consolidation-runs/"+id+"/supersede", c.tenant, "group-controller", map[string]any{"reason": "again"}); st != 409 {
		t.Fatalf("%d %v", st, b)
	}
	// the US records its side; the period consolidates cleanly and certifies
	c.seed(t, c.us, "STAT-US", "USD", "2026-03-25", []string{"1000", "25.00", "0"}, []string{"2500", "0", "25.00", c.uk, "USD", "25.00"})
	run2 := c.mustRun(t, "2026-03")
	if run2["consolidation_run_id"] == id || run2["blocking_count"].(float64) != 0 {
		t.Fatalf("a new, clean run: %v", run2["population"].(map[string]any)["exceptions"])
	}
	if st, b := c.call("POST", "/v1/consolidation-runs/"+run2["consolidation_run_id"].(string)+"/certify", c.tenant, "group-controller", nil); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
}

func TestSecondPeriodPostsOnlyTheChangeAndOnlyTheLatestRunCanBeWithdrawn(t *testing.T) {
	c := consolSetup(t)
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")
	mar := c.mustRun(t, "2026-03")
	marBook := c.groupBook(t)

	// April: external activity, the UK revalues its USD receivable to the new closing rate, and the mapping gains accounts (version 2)
	c.rate("USD", "GBP", "0.82", "CLOSING", "2026-04-30T17:00:00Z", "ecb", "c-apr")
	c.rate("USD", "GBP", "0.81", "AVERAGE", "2026-04-30T17:00:00Z", "ecb", "a-apr")
	c.seed(t, c.uk, "STAT-UK", "GBP", "2026-04-10", []string{"1000", "500.00", "0"}, []string{"4000", "0", "500.00"})
	c.seed(t, c.uk, "STAT-UK", "GBP", "2026-04-30", []string{"1500", "3.00", "0", c.us, "USD", "0"}, []string{"7300", "0", "3.00"})
	c.seed(t, c.us, "STAT-US", "USD", "2026-04-15", []string{"6000", "40.00", "0"}, []string{"1000", "0", "40.00"})
	if st, r := c.call("PUT", "/v1/consolidation-groups/"+c.groupID+"/mappings", c.tenant, "group-finance", map[string]any{"account_map": identity("1000", "1500", "2500", "3000", "4000", "4500", "5000", "6000", "7300"), "ic_roles": icRoles}); st != 201 || r["version"].(float64) != 2 {
		t.Fatalf("%d %v", st, r)
	}
	if st, r := c.call("GET", "/v1/consolidation-groups/"+c.groupID+"/mappings", c.tenant, "reader", nil); st != 200 || len(r["mappings"].([]any)) != 2 || r["mappings"].([]any)[0].(map[string]any)["status"] != "ACTIVE" || r["mappings"].([]any)[1].(map[string]any)["status"] != "SUPERSEDED" {
		t.Fatalf("%d %v", st, r)
	}
	// April's population is not sealed yet; it can still be consolidated, but the run cannot be certified
	st, open := c.call("POST", "/v1/consolidation-runs", c.tenant, "preparer", map[string]any{"group_id": c.groupID, "fiscal_period": "2026-04"})
	if st != 409 || open["code"] != "INPUT_NOT_SEALED" {
		t.Fatalf("%d %v", st, open)
	}
	c.seal(t, c.uk, "2026-04")
	c.seal(t, c.us, "2026-04")
	apr := c.mustRun(t, "2026-04")
	if apr["mapping_version"].(float64) != 2 || apr["prior_run_id"] != mar["consolidation_run_id"] || apr["blocking_count"].(float64) != 0 {
		t.Fatalf("%v", apr)
	}
	// the group book carries April's cumulative figures: March's plus only the change
	// 10420 + 5360x0.82 = 14815.20; equity -14100; revenue -500 - 243; costs; reserve; difference
	st, cert := c.call("POST", "/v1/consolidation-runs/"+apr["consolidation_run_id"].(string)+"/certify", c.tenant, "group-controller", nil)
	if st != 200 || cert["status"] != "CERTIFIED" {
		t.Fatalf("the group book reconciles to the run: %d %v", st, cert)
	}
	// March is covered by April now: it can neither be withdrawn nor re-run out of order
	if st, b := c.call("POST", "/v1/consolidation-runs/"+mar["consolidation_run_id"].(string)+"/supersede", c.tenant, "group-controller", map[string]any{"reason": "x"}); st != 422 || b["code"] != "SUPERSEDE_NOT_LATEST" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := c.call("POST", "/v1/consolidation-runs/"+mar["consolidation_run_id"].(string)+"/certify", c.tenant, "group-controller", nil); st != 422 || b["code"] != "CONSOLIDATION_NOT_LATEST" {
		t.Fatalf("%d %v", st, b)
	}
	// withdrawing April puts the group book back to March
	if st, b := c.call("POST", "/v1/consolidation-runs/"+apr["consolidation_run_id"].(string)+"/supersede", c.tenant, "group-controller", map[string]any{"reason": "re-run after an adjustment"}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	wantBook(t, c.groupBook(t), marBook)
	if st, r := c.call("GET", "/v1/consolidation-runs?group_id="+c.groupID, c.tenant, "reader", nil); st != 200 || r["total"].(float64) != 2 {
		t.Fatalf("%d %v", st, r)
	}
}

func TestGroupSetupRefusesWhatWouldPutGroupResultsInAMemberLedger(t *testing.T) {
	c := consolSetup(t)
	call := func(method, path string, body any) (int, map[string]any) {
		return c.call(method, path, c.tenant, "group-finance", body)
	}
	if st, b := call("POST", "/v1/consolidation-groups", map[string]any{"group_code": "BAD1", "group_name": "x", "parent_entity_id": c.uk, "group_book_id": c.ukBook, "presentation_currency": "GBP"}); st != 422 || b["code"] != "GROUP_BOOK_INVALID" {
		t.Fatalf("a statutory book is not a group book: %d %v", st, b)
	}
	if st, b := call("POST", "/v1/consolidation-groups", map[string]any{"group_code": "BAD2", "group_name": "x", "parent_entity_id": c.uk, "group_book_id": c.gBook, "presentation_currency": "GBP"}); st != 422 || b["code"] != "GROUP_BOOK_INVALID" {
		t.Fatalf("the book belongs to another entity: %d %v", st, b)
	}
	if st, b := call("POST", "/v1/consolidation-groups", map[string]any{"group_code": "BAD3", "group_name": "x", "parent_entity_id": c.group, "group_book_id": c.gBook, "presentation_currency": "USD"}); st != 422 || b["code"] != "GROUP_BOOK_INVALID" {
		t.Fatalf("the book is not in the presentation currency: %d %v", st, b)
	}
	if st, b := call("POST", "/v1/consolidation-groups", map[string]any{"group_code": "GRP", "group_name": "dup", "parent_entity_id": c.group, "group_book_id": c.gBook, "presentation_currency": "GBP"}); st != 409 {
		t.Fatalf("%d %v", st, b)
	}
	path := "/v1/consolidation-groups/" + c.groupID + "/members"
	if st, b := call("POST", path, map[string]any{"legal_entity_id": c.group, "source_book_id": c.gBook, "ownership_pct": "100", "effective_from": "2026-01-01"}); st != 422 {
		t.Fatalf("the reporting entity cannot be a member: %d %v", st, b)
	}
	if st, b := call("POST", path, map[string]any{"legal_entity_id": c.us, "source_book_id": c.ukBook, "ownership_pct": "100", "effective_from": "2026-01-01"}); st != 422 || b["code"] != "MEMBER_INVALID" {
		t.Fatalf("another entity's book: %d %v", st, b)
	}
	if st, b := call("POST", path, map[string]any{"legal_entity_id": c.us, "source_book_id": c.usBook, "ownership_pct": "100", "effective_from": "2026-01-01"}); st != 409 {
		t.Fatalf("overlapping membership of the same entity: %d %v", st, b)
	}
	for _, pct := range []string{"0", "101", "x"} {
		if st, b := call("POST", path, map[string]any{"legal_entity_id": c.us, "source_book_id": c.usBook, "ownership_pct": pct, "effective_from": "2030-01-01"}); st != 422 {
			t.Fatalf("%s: %d %v", pct, st, b)
		}
	}
	if st, b := call("PUT", "/v1/consolidation-groups/"+c.groupID+"/mappings", map[string]any{"account_map": map[string]any{"1000": "9999"}}); st != 422 || b["code"] != "GROUP_ACCOUNT_UNAVAILABLE" {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := call("PUT", "/v1/consolidation-groups/"+c.groupID+"/mappings", map[string]any{"account_map": identity("1000"), "ic_roles": map[string]any{"1500": "DUE_FROM"}}); st != 422 {
		t.Fatalf("a role needs a mapped account: %d %v", st, b)
	}
	if st, b := call("PUT", "/v1/consolidation-groups/"+c.groupID+"/mappings", map[string]any{"account_map": identity("1000"), "ic_roles": map[string]any{"1000": "NONSENSE"}}); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
}

func TestAMembershipThatChangesInsideThePeriodBlocksTheCertification(t *testing.T) {
	c := consolSetup(t)
	// the US was acquired in the middle of March: 100% from the 15th
	if err := c.exec(`DELETE FROM consolidation_members WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid`, c.tenant, c.us); err == nil {
		t.Fatal("the guard refuses to delete a membership")
	}
	if err := fixtureExec(context.Background(), c.admin, `DELETE FROM consolidation_members WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid`, c.tenant, c.us); err != nil {
		t.Fatal(err)
	}
	if st, b := c.call("POST", "/v1/consolidation-groups/"+c.groupID+"/members", c.tenant, "group-finance", map[string]any{"legal_entity_id": c.us, "source_book_id": c.usBook, "ownership_pct": "80", "effective_from": "2026-03-15"}); st != 201 {
		t.Fatalf("%d %v", st, b)
	}
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")
	run := c.mustRun(t, "2026-03")
	codes := map[string]bool{}
	for _, e := range run["population"].(map[string]any)["exceptions"].([]any) {
		codes[e.(map[string]any)["code"].(string)] = true
	}
	if !codes["MEMBERSHIP_CHANGED_IN_PERIOD"] || run["blocking_count"].(float64) == 0 {
		t.Fatalf("%v", run["population"].(map[string]any)["exceptions"])
	}
	nci := run["population"].(map[string]any)["non_controlling_interest"].([]any)
	if len(nci) != 1 || nci[0].(map[string]any)["nci_pct"] != "20.0000" {
		t.Fatalf("the minority's share is calculated and recorded: %v", nci)
	}
	if st, b := c.call("POST", "/v1/consolidation-runs/"+run["consolidation_run_id"].(string)+"/certify", c.tenant, "group-controller", nil); st != 422 || b["code"] != "CONSOLIDATION_BLOCKED" {
		t.Fatalf("%d %v", st, b)
	}
	// a membership is ended, never edited
	members := c.scalar(t, `SELECT member_id::text FROM consolidation_members WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid`, c.tenant, c.us)
	if err := c.asApp(c.tenant, `UPDATE consolidation_members SET ownership_pct = 10 WHERE member_id = $1::uuid`, members); err == nil {
		t.Fatal("ownership is history, not an edit")
	}
	if st, b := c.call("POST", "/v1/consolidation-groups/"+c.groupID+"/members/"+members+"/end", c.tenant, "group-finance", map[string]any{"effective_to": "2026-06-30"}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := c.call("POST", "/v1/consolidation-groups/"+c.groupID+"/members/"+members+"/end", c.tenant, "group-finance", map[string]any{"effective_to": "2026-07-31"}); st != 409 {
		t.Fatal("a membership ends once")
	}
}

func TestConsolidationRefusesAMissingMappingAndAMissingRate(t *testing.T) {
	c := consolSetup(t)
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")
	// a balance in an account the mapping does not know: the run stops, it does not drop the balance
	if err := c.exec(`INSERT INTO chart_of_accounts (account_id, tenant_id, account_code, account_name, account_type, created_by_principal_id) VALUES ($1,$2,'8888','unmapped','ASSET','seed')`, uuid.NewString(), c.tenant); err != nil {
		t.Fatal(err)
	}
	c.seed(t, c.us, "STAT-US", "USD", "2026-03-22", []string{"8888", "9.00", "0"}, []string{"1000", "0", "9.00"})
	// the entity's seal watermark is from before: the period is still sealed, and the population includes the later entry
	st, b := c.run("2026-03")
	if st != 422 || b["code"] != "CONSOLIDATION_MAPPING_MISSING" {
		t.Fatalf("%d %v", st, b)
	}
	if n := c.count(t, `SELECT count(*) FROM consolidation_runs WHERE tenant_id = $1::uuid`, c.tenant); n != 0 {
		t.Fatalf("no run was recorded: %d", n)
	}
	if got := c.groupBook(t); len(got) != 0 {
		t.Fatalf("nothing was posted: %v", got)
	}
	// an average rate that is not in the store
	c2 := consolSetup(t)
	c2.seal(t, c2.uk, "2026-04")
	c2.seal(t, c2.us, "2026-04")
	if st, b := c2.run("2026-04"); st != 422 || b["code"] != "CONSOLIDATION_RATE_UNAVAILABLE" {
		t.Fatalf("%d %v", st, b)
	}
	// no signing key, no run: the inputs are frozen as signed snapshots
	if st, b := c2.call("POST", "/v1/consolidation-runs", c2.tenant, "preparer", map[string]any{"group_id": "not-a-uuid", "fiscal_period": "2026-03"}); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
	if st, b := c2.call("POST", "/v1/consolidation-runs", c2.tenant, "preparer", map[string]any{"group_id": c2.groupID, "fiscal_period": "March"}); st != 422 {
		t.Fatalf("%d %v", st, b)
	}
}

func TestConsolidationRunsOncePerGroupAndPeriodUnderConcurrency(t *testing.T) {
	c := consolSetup(t)
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")
	var wg sync.WaitGroup
	statuses := make([]int, 6)
	ids := make([]string, 6)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, r := c.run("2026-03")
			statuses[i] = st
			if r != nil {
				ids[i], _ = r["consolidation_run_id"].(string)
			}
		}(i)
	}
	wg.Wait()
	created := 0
	for i, st := range statuses {
		switch st {
		case 201:
			created++
		case 200, 409:
			// 200: the run another request wrote; 409: the population moved under a competing run (AK-CONC)
		default:
			t.Fatalf("request %d: %d", i, st)
		}
	}
	if created != 1 {
		t.Fatalf("exactly one run is created: %v", statuses)
	}
	if n := c.count(t, `SELECT count(*) FROM consolidation_runs WHERE tenant_id = $1::uuid AND status <> 'SUPERSEDED'`, c.tenant); n != 1 {
		t.Fatalf("one live run: %d", n)
	}
	if n := c.count(t, `SELECT count(*) FROM journal_headers WHERE tenant_id = $1::uuid AND legal_entity_id = $2::uuid AND journal_type IN ('CONSOLIDATION', 'ELIMINATION')`, c.tenant, c.group); n != 2 {
		t.Fatalf("the group book was posted once: %d journals", n)
	}
	// the group book still balances in total
	total := 0.0
	for _, v := range c.groupBook(t) {
		var f float64
		fmt.Sscanf(v, "%f", &f)
		total += f
	}
	if total > 0.005 || total < -0.005 {
		t.Fatalf("the group book balances: %v", total)
	}
}

func TestConsolidationRecordsAreImmutableAndTenantIsolated(t *testing.T) {
	c := consolSetup(t)
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")
	run := c.mustRun(t, "2026-03")
	id := run["consolidation_run_id"].(string)
	for what, sql := range map[string]string{
		"rewrite the population":     `UPDATE consolidation_runs SET population = '{}'::jsonb WHERE run_id = $1::uuid`,
		"change the hash":            `UPDATE consolidation_runs SET run_hash = repeat('a', 64) WHERE run_id = $1::uuid`,
		"delete the run":             `DELETE FROM consolidation_runs WHERE run_id = $1::uuid`,
		"certify without a snapshot": `UPDATE consolidation_runs SET status = 'CERTIFIED', certified_by = 'x' WHERE run_id = $1::uuid`,
		"certify as the preparer":    `UPDATE consolidation_runs SET status = 'CERTIFIED', certified_by = created_by, group_snapshot_id = (SELECT snapshot_id FROM balance_snapshots LIMIT 1) WHERE run_id = $1::uuid`,
	} {
		wantRefused(t, what, c.asApp(c.tenant, sql, id), "")
	}
	wantRefused(t, "edit a mapping version", c.asApp(c.tenant, `UPDATE consolidation_mappings SET account_map = '{}'::jsonb WHERE group_id = $1::uuid`, c.groupID), "")
	wantRefused(t, "delete a mapping version", c.asApp(c.tenant, `DELETE FROM consolidation_mappings WHERE group_id = $1::uuid`, c.groupID), "")
	other := uuid.NewString()
	if n := c.countAs(other, `SELECT count(*) FROM consolidation_runs`); n != 0 {
		t.Fatalf("another tenant sees %d runs", n)
	}
	if n := c.countAs(other, `SELECT count(*) FROM consolidation_groups`); n != 0 {
		t.Fatalf("another tenant sees %d groups", n)
	}
	if st, _ := c.call("GET", "/v1/consolidation-runs/"+id, other, "reader", nil); st != 404 {
		t.Fatalf("%d", st)
	}
	if st, _ := c.call("GET", "/v1/consolidation-groups/"+c.groupID, other, "reader", nil); st != 404 {
		t.Fatalf("%d", st)
	}
}

func TestConsolidationActionsAreSeparatelyAuthorized(t *testing.T) {
	c := consolSetup(t)
	c.seal(t, c.uk, "2026-03")
	c.seal(t, c.us, "2026-03")
	c.denied.Store("analyst|GL_CONSOLIDATION_RUN", true)
	if st, _ := c.call("POST", "/v1/consolidation-runs", c.tenant, "analyst", map[string]any{"group_id": c.groupID, "fiscal_period": "2026-03"}); st != 403 {
		t.Fatalf("%d", st)
	}
	run := c.mustRun(t, "2026-03")
	c.denied.Store("preparer|GL_CONSOLIDATION_CERTIFY", true)
	if st, _ := c.call("POST", "/v1/consolidation-runs/"+run["consolidation_run_id"].(string)+"/certify", c.tenant, "preparer", nil); st != 403 {
		t.Fatalf("%d", st)
	}
	c.denied.Store("analyst|GL_CONSOLIDATION_MANAGE", true)
	if st, _ := c.call("POST", "/v1/consolidation-groups", c.tenant, "analyst", map[string]any{}); st != 403 {
		t.Fatalf("%d", st)
	}
}

func TestAnUnsealedInputIsAllowedOnlyAsABlockingException(t *testing.T) {
	c := consolSetup(t)
	st, run := c.call("POST", "/v1/consolidation-runs", c.tenant, "preparer", map[string]any{"group_id": c.groupID, "fiscal_period": "2026-03", "allow_unsealed": true})
	if st != 201 || run["blocking_count"].(float64) < 2 {
		t.Fatalf("%d %v", st, run)
	}
	if st, b := c.call("POST", "/v1/consolidation-runs/"+run["consolidation_run_id"].(string)+"/certify", c.tenant, "group-controller", nil); st != 422 || b["code"] != "CONSOLIDATION_BLOCKED" {
		t.Fatalf("%d %v", st, b)
	}
}
