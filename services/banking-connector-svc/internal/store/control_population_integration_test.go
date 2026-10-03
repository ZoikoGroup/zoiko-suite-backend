//go:build integration

package store_test

import (
	"context"
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

const (
	cpT1 = "tenant-cp-1"
	cpT2 = "tenant-cp-2"
)

func mustExec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed (%s): %v", sql, err)
	}
}

// seedTenant lays out, for one tenant (ids are prefixed to stay unique):
//
//	conn A  le-1  acct-A USD   stmt A1 2026-08-31 close 1000 (ACCEPTED)  lines +500 "R1", -200 "R2"
//	                           stmt A2 2026-09-15 close 1100 (RECEIVED)  line  +100 "R3"
//	conn B  le-1  acct-B EUR   stmt B1 2026-09-10 close  300 (ACCEPTED)  line   -50 ""   (no bank reference)
//	conn C  le-2  acct-C USD   stmt C1 2026-09-01                       line   +7  (other entity, must never appear for le-1)
//
// Canonical transactions: one per line, plus a SUPERSEDED predecessor of R1.
func seedTenant(t *testing.T, admin *pgxpool.Pool, tn, p string) {
	t.Helper()
	conn := func(id, le, acct, cur string) {
		mustExec(t, admin, `INSERT INTO bank_connections (connection_id, tenant_id, legal_entity_id, bank_name, account_number, currency, bank_account_id)
			VALUES ($1,$2,$3,'Bank','ACCT',$4,$5)`, p+id, tn, le, cur, acct)
	}
	stmt := func(id, connID, date, close, status string) {
		mustExec(t, admin, `INSERT INTO bank_statements (statement_id, connection_id, tenant_id, statement_format, statement_date, closing_balance, status)
			VALUES ($1,$2,$3,'BAI2',$4::timestamptz,$5::numeric,$6)`, p+id, p+connID, tn, date, close, status)
	}
	line := func(id, stmtID string, seq int, date, amt, cur, ref string) {
		mustExec(t, admin, `INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, raw_reference)
			VALUES ($1,$2,$3,$4,$5::timestamptz,$6::numeric,$7,$8)`, p+id, p+stmtID, tn, seq, date, amt, cur, ref)
	}
	txn := func(id, lineID, date, amt, cur, status string) {
		mustExec(t, admin, `INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, transaction_date, amount, currency, status)
			VALUES ($1,$2,$3,$4::timestamptz,$5::numeric,$6,$7)`, p+id, tn, p+lineID, date, amt, cur, status)
	}
	conn("cA", "le-1", "acct-A", "USD")
	conn("cB", "le-1", "acct-B", "EUR")
	conn("cC", "le-2", "acct-C", "USD")
	stmt("sA1", "cA", "2026-08-31T00:00:00Z", "1000", "ACCEPTED")
	stmt("sA2", "cA", "2026-09-15T00:00:00Z", "1100", "RECEIVED")
	stmt("sB1", "cB", "2026-09-10T00:00:00Z", "300", "ACCEPTED")
	stmt("sC1", "cC", "2026-09-01T00:00:00Z", "10", "ACCEPTED")
	line("l1", "sA1", 1, "2026-08-30T10:00:00Z", "500", "USD", "R1")
	line("l2", "sA1", 2, "2026-08-31T10:00:00Z", "-200", "USD", "R2")
	line("l3", "sA2", 1, "2026-09-15T10:00:00Z", "100", "USD", "R3")
	line("l4", "sB1", 1, "2026-09-10T10:00:00Z", "-50", "EUR", "")
	line("l5", "sC1", 1, "2026-09-01T10:00:00Z", "7", "USD", "RC")
	// The superseded predecessor is inserted first: at most one live row per line.
	txn("t1old", "l1", "2026-08-30T10:00:00Z", "499", "USD", "SUPERSEDED")
	txn("t1", "l1", "2026-08-30T10:00:00Z", "500", "USD", "NORMALIZED")
	txn("t2", "l2", "2026-08-31T10:00:00Z", "-200", "USD", "NORMALIZED")
	txn("t3", "l3", "2026-09-15T10:00:00Z", "100", "USD", "NORMALIZED")
	txn("t4", "l4", "2026-09-10T10:00:00Z", "-50", "EUR", "NORMALIZED")
	txn("t5", "l5", "2026-09-01T10:00:00Z", "7", "USD", "NORMALIZED")
}

type cpFixture struct {
	admin *pgxpool.Pool
	s     *store.PgStore
}

func newCPFixture(t *testing.T) cpFixture {
	t.Helper()
	admin := openAdminPool(t)
	// Queries run as a NOBYPASSRLS role so RLS is live underneath the explicit predicates.
	appPool := appRolePool(t, admin)
	seedTenant(t, admin, cpT1, "a-")
	seedTenant(t, admin, cpT2, "b-")
	return cpFixture{admin: admin, s: store.NewPgStore(appPool)}
}

func (f cpFixture) get(t *testing.T, tenant string, q domain.ControlPopulationQuery) *domain.ControlPopulationPage {
	t.Helper()
	q.TenantID = tenant
	if q.LegalEntityID == "" {
		q.LegalEntityID = "le-1"
	}
	if q.Limit == 0 {
		q.Limit = 1000
	}
	page, err := f.s.ControlPopulation(middleware.WithTenant(context.Background(), tenant), q)
	if err != nil {
		t.Fatalf("ControlPopulation: %v", err)
	}
	return page
}

// walk pages through a whole extraction with the given limit.
func (f cpFixture) walk(t *testing.T, tenant string, q domain.ControlPopulationQuery, limit int) (recs []domain.ControlRecord, wms []string, last *domain.ControlPopulationPage) {
	t.Helper()
	q.Limit = limit
	for i := 0; i < 50; i++ {
		page := f.get(t, tenant, q)
		recs = append(recs, page.Records...)
		wms = append(wms, page.Watermark)
		last = page
		if page.NextCursor == "" {
			return
		}
		q.AfterRecordID = page.NextCursor
	}
	t.Fatal("paging did not terminate")
	return
}

func ids(recs []domain.ControlRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.RecordID
	}
	return out
}

func TestControlPopulation_PagingVisitsEveryRowOnceWithStableWatermark(t *testing.T) {
	f := newCPFixture(t)
	for _, pop := range []string{domain.PopulationBankTransactions, domain.PopulationBankStatements} {
		full := f.get(t, cpT1, domain.ControlPopulationQuery{Population: pop})
		recs, wms, _ := f.walk(t, cpT1, domain.ControlPopulationQuery{Population: pop}, 2)
		if len(recs) != full.DeclaredTotals.RowCount || len(recs) != len(full.Records) {
			t.Fatalf("%s: paged %d, single page %d, declared %d", pop, len(recs), len(full.Records), full.DeclaredTotals.RowCount)
		}
		seen := map[string]bool{}
		for i, r := range recs {
			if seen[r.RecordID] {
				t.Fatalf("%s: %s visited twice", pop, r.RecordID)
			}
			seen[r.RecordID] = true
			if r.RecordID != full.Records[i].RecordID {
				t.Fatalf("%s: order differs at %d: %s vs %s", pop, i, r.RecordID, full.Records[i].RecordID)
			}
			if i > 0 && recs[i-1].RecordID >= r.RecordID {
				t.Fatalf("%s: not ascending on record_id: %v", pop, ids(recs))
			}
		}
		for _, wm := range wms {
			if wm != full.Watermark || wm == "" {
				t.Fatalf("%s: watermark differs across pages: %v vs %s", pop, wms, full.Watermark)
			}
		}
	}
	// le-1 transactions: t1old, t1, t2, t3, t4 (SUPERSEDED row included, not dropped); le-2's t5 excluded.
	tx := f.get(t, cpT1, domain.ControlPopulationQuery{Population: domain.PopulationBankTransactions})
	if tx.DeclaredTotals.RowCount != 5 {
		t.Fatalf("expected 5 transactions, got %d: %v", tx.DeclaredTotals.RowCount, ids(tx.Records))
	}
	if tx.NextCursor != "" {
		t.Fatal("last page must have an empty next_cursor")
	}
}

func rat(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("not a decimal string: %q", s)
	}
	return r
}

func TestControlPopulation_DeclaredTotalsEqualReturnedSumsPerCurrency(t *testing.T) {
	f := newCPFixture(t)
	for _, pop := range []string{domain.PopulationBankTransactions, domain.PopulationBankStatements} {
		recs, _, last := f.walk(t, cpT1, domain.ControlPopulationQuery{Population: pop}, 2)
		sums := map[string]*big.Rat{}
		for _, r := range recs {
			if sums[r.Currency] == nil {
				sums[r.Currency] = new(big.Rat)
			}
			sums[r.Currency].Add(sums[r.Currency], rat(t, r.Amount))
		}
		if len(sums) != len(last.DeclaredTotals.Totals) {
			t.Fatalf("%s: currencies %v vs declared %v", pop, sums, last.DeclaredTotals.Totals)
		}
		for cur, want := range sums {
			if got := rat(t, last.DeclaredTotals.Totals[cur]); got.Cmp(want) != 0 {
				t.Fatalf("%s %s: declared %s != returned sum %s", pop, cur, got, want)
			}
		}
	}
	// Concrete: USD = 499 + 500 - 200 + 100; EUR = -50.
	tx := f.get(t, cpT1, domain.ControlPopulationQuery{Population: domain.PopulationBankTransactions, Limit: 1})
	if rat(t, tx.DeclaredTotals.Totals["USD"]).Cmp(big.NewRat(899, 1)) != 0 || rat(t, tx.DeclaredTotals.Totals["EUR"]).Cmp(big.NewRat(-50, 1)) != 0 {
		t.Fatalf("totals: %v", tx.DeclaredTotals.Totals)
	}
	// declared_totals cover the whole set even when the page is tiny.
	if tx.DeclaredTotals.RowCount != 5 || len(tx.Records) != 1 {
		t.Fatalf("row_count=%d records=%d", tx.DeclaredTotals.RowCount, len(tx.Records))
	}
}

func TestControlPopulation_SignConventionAndFieldChoices(t *testing.T) {
	f := newCPFixture(t)
	tx := f.get(t, cpT1, domain.ControlPopulationQuery{Population: domain.PopulationBankTransactions})
	by := map[string]domain.ControlRecord{}
	for _, r := range tx.Records {
		by[r.RecordID] = r
	}
	if r := by["a-t1"]; r.Amount != "500.0000" || r.Reference != "R1" || r.Currency != "USD" || r.Date != "2026-08-30" ||
		r.Attributes["bank_account_id"] != "acct-A" || r.Attributes["statement_id"] != "a-sA1" || r.Attributes["status"] != "NORMALIZED" {
		t.Fatalf("credit record: %+v", r)
	}
	if r := by["a-t2"]; r.Amount != "-200.0000" || r.Reference != "R2" {
		t.Fatalf("debit must be negative: %+v", r)
	}
	if r := by["a-t1old"]; r.Attributes["status"] != "SUPERSEDED" {
		t.Fatalf("superseded row must be present and marked: %+v", r)
	}
	if r := by["a-t3"]; r.Attributes["statement_status"] != "RECEIVED" {
		t.Fatalf("parent statement status must be exposed: %+v", r)
	}
	// A line with no bank reference must not get an empty (matchable) reference.
	if r := by["a-t4"]; r.Reference != "txn:a-t4" || r.Currency != "EUR" || r.Amount != "-50.0000" {
		t.Fatalf("no-reference fallback: %+v", r)
	}
	if _, leaked := by["a-t5"]; leaked {
		t.Fatal("another legal entity's transaction leaked")
	}

	st := f.get(t, cpT1, domain.ControlPopulationQuery{Population: domain.PopulationBankStatements})
	if st.DeclaredTotals.RowCount != 3 {
		t.Fatalf("expected 3 statements, got %v", ids(st.Records))
	}
	sb := map[string]domain.ControlRecord{}
	for _, r := range st.Records {
		sb[r.RecordID] = r
	}
	if r := sb["a-sA1"]; r.Reference != "acct-A|2026-08-31" || r.Amount != "1000.0000" || r.Currency != "USD" || r.Date != "2026-08-31" ||
		r.Attributes["bank_account_id"] != "acct-A" || r.Attributes["status"] != "ACCEPTED" {
		t.Fatalf("statement record: %+v", r)
	}
	if r := sb["a-sB1"]; r.Currency != "EUR" || r.Reference != "acct-B|2026-09-10" {
		t.Fatalf("statement currency comes from the account: %+v", r)
	}
}

func TestControlPopulation_BankAccountFilter(t *testing.T) {
	f := newCPFixture(t)
	for _, pop := range []string{domain.PopulationBankTransactions, domain.PopulationBankStatements} {
		recs, _, last := f.walk(t, cpT1, domain.ControlPopulationQuery{Population: pop, BankAccountID: "acct-B"}, 1)
		if len(recs) != 1 || recs[0].Attributes["bank_account_id"] != "acct-B" || last.DeclaredTotals.RowCount != 1 {
			t.Fatalf("%s acct-B: %+v", pop, recs)
		}
		if len(last.DeclaredTotals.Totals) != 1 || last.DeclaredTotals.Totals["EUR"] == "" {
			t.Fatalf("%s: filtered totals must cover only the filtered set: %v", pop, last.DeclaredTotals.Totals)
		}
		none := f.get(t, cpT1, domain.ControlPopulationQuery{Population: pop, BankAccountID: "acct-nope"})
		if none.DeclaredTotals.RowCount != 0 || len(none.Records) != 0 {
			t.Fatalf("%s unknown account should be empty: %+v", pop, none)
		}
	}
	// acct-C belongs to le-2: filtering for it under le-1 must find nothing.
	if p := f.get(t, cpT1, domain.ControlPopulationQuery{Population: domain.PopulationBankTransactions, BankAccountID: "acct-C"}); p.DeclaredTotals.RowCount != 0 {
		t.Fatal("cross-entity bank_account_id leaked")
	}
}

func TestControlPopulation_PeriodCutOff(t *testing.T) {
	f := newCPFixture(t)
	aug := f.get(t, cpT1, domain.ControlPopulationQuery{Population: domain.PopulationBankTransactions, PeriodEnd: "2026-08-31"})
	if got := ids(aug.Records); len(got) != 3 || got[0] != "a-t1" && got[0] != "a-t1old" {
		t.Fatalf("as of 2026-08-31 expected t1old,t1,t2: %v", got)
	}
	for _, r := range aug.Records {
		if r.Date > "2026-08-31" {
			t.Fatalf("record after cut-off: %+v", r)
		}
	}
	sep := f.get(t, cpT1, domain.ControlPopulationQuery{Population: domain.PopulationBankTransactions, PeriodEnd: "2026-09-30"})
	if sep.DeclaredTotals.RowCount != 5 {
		t.Fatalf("cumulative as of Sep: %v", ids(sep.Records))
	}
	// Inclusive of the last day itself, exclusive of the next.
	edge := f.get(t, cpT1, domain.ControlPopulationQuery{Population: domain.PopulationBankStatements, PeriodEnd: "2026-08-31"})
	if len(edge.Records) != 1 || edge.Records[0].RecordID != "a-sA1" {
		t.Fatalf("statement dated on cut-off day: %v", ids(edge.Records))
	}
	if aug.Watermark == sep.Watermark {
		t.Fatal("different in-scope sets must have different watermarks")
	}
}

func TestControlPopulation_TenantIsolation(t *testing.T) {
	f := newCPFixture(t)
	for _, pop := range []string{domain.PopulationBankTransactions, domain.PopulationBankStatements} {
		one := f.get(t, cpT1, domain.ControlPopulationQuery{Population: pop})
		two := f.get(t, cpT2, domain.ControlPopulationQuery{Population: pop})
		for _, r := range one.Records {
			if r.RecordID[:2] != "a-" {
				t.Fatalf("%s: tenant 1 saw %s", pop, r.RecordID)
			}
		}
		for _, r := range two.Records {
			if r.RecordID[:2] != "b-" {
				t.Fatalf("%s: tenant 2 saw %s", pop, r.RecordID)
			}
		}
		if len(one.Records) == 0 || len(one.Records) != len(two.Records) {
			t.Fatalf("%s: each tenant should see its own %d rows", pop, len(one.Records))
		}
		// Unknown tenant sees nothing.
		if none := f.get(t, "tenant-none", domain.ControlPopulationQuery{Population: pop}); none.DeclaredTotals.RowCount != 0 {
			t.Fatalf("%s: unknown tenant saw rows", pop)
		}
	}
	// A query whose tenant disagrees with the verified context is refused.
	_, err := f.s.ControlPopulation(middleware.WithTenant(context.Background(), cpT2), domain.ControlPopulationQuery{
		Population: domain.PopulationBankTransactions, TenantID: cpT1, LegalEntityID: "le-1", Limit: 10})
	if err == nil {
		t.Fatal("tenant/context mismatch must be refused")
	}
}

func TestControlPopulation_WatermarkStableThenChangesWithStatus(t *testing.T) {
	f := newCPFixture(t)
	q := domain.ControlPopulationQuery{Population: domain.PopulationBankTransactions}
	sq := domain.ControlPopulationQuery{Population: domain.PopulationBankStatements}
	before := f.get(t, cpT1, q)
	sBefore := f.get(t, cpT1, sq)
	if again := f.get(t, cpT1, q); again.Watermark != before.Watermark {
		t.Fatal("watermark must be stable while nothing changes")
	}

	// Quarantine the RECEIVED statement a-sA2 through the service's own command.
	ctx := middleware.WithTenant(context.Background(), cpT1)
	if err := f.s.QuarantineStatement(ctx, cpT1, "a-sA2", "bad balance"); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	after := f.get(t, cpT1, q)
	sAfter := f.get(t, cpT1, sq)
	if after.Watermark == before.Watermark {
		t.Fatal("transactions watermark must change when a parent statement status changes")
	}
	if sAfter.Watermark == sBefore.Watermark {
		t.Fatal("statements watermark must change when a statement status changes")
	}
	// Still present, now marked - never silently excluded.
	if after.DeclaredTotals.RowCount != before.DeclaredTotals.RowCount {
		t.Fatal("quarantined data must stay in the population")
	}
	for _, r := range sAfter.Records {
		if r.RecordID == "a-sA2" && r.Attributes["status"] != "QUARANTINED" {
			t.Fatalf("quarantine not marked: %+v", r)
		}
	}

	// A new row changes the watermark too.
	mustExec(t, f.admin, `INSERT INTO bank_statement_lines (line_id, statement_id, tenant_id, line_seq, posted_date, amount, currency, raw_reference)
		VALUES ('a-l9','a-sB1',$1,2,'2026-09-11T00:00:00Z',5,'EUR','R9')`, cpT1)
	mustExec(t, f.admin, `INSERT INTO bank_transactions_canonical (transaction_id, tenant_id, statement_line_id, transaction_date, amount, currency)
		VALUES ('a-t9',$1,'a-l9','2026-09-11T00:00:00Z',5,'EUR')`, cpT1)
	if grown := f.get(t, cpT1, q); grown.Watermark == after.Watermark || grown.DeclaredTotals.RowCount != 6 {
		t.Fatalf("new row must change the watermark: %s vs %s", grown.Watermark, after.Watermark)
	}
}
