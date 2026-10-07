//go:build integration

package store_test

import (
	"context"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

type w4Fixture struct {
	*cpFixture
	pool *pgxpool.Pool
}

func newW4Fixture(t *testing.T) *w4Fixture {
	t.Helper()
	pool := openTestPool(t)
	tenant := uuid.New().String()
	return &w4Fixture{
		cpFixture: &cpFixture{
			s:      store.New(pool, zap.NewNop()),
			tenant: tenant,
			entity: uuid.New().String(),
			ctx:    svcmiddleware.WithTenant(context.Background(), tenant),
		},
		pool: pool,
	}
}

func dr(acct string, amt float64) domain.JournalLine {
	return domain.JournalLine{AccountCode: acct, DebitAmount: amt}
}
func cr(acct string, amt float64) domain.JournalLine {
	return domain.JournalLine{AccountCode: acct, CreditAmount: amt}
}

// journal creates and FINALIZES a journal (lines must balance) in the fixture's
// tenant/entity with an explicit fiscal period and currency.
func (f *w4Fixture) journal(t *testing.T, period, currency string, txn domain.Date, lines ...domain.JournalLine) string {
	t.Helper()
	return f.journalAs(t, f.tenant, f.entity, period, currency, txn, lines...)
}

func (f *w4Fixture) journalAs(t *testing.T, tenant, entity, period, currency string, txn domain.Date, lines ...domain.JournalLine) string {
	t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenant, LegalEntityID: entity,
		FiscalPeriod: period, Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: txn,
		PostingDate: txn, CurrencyCode: currency,
		CreatedByPrincipalID: "preparer-1", CorrelationID: uuid.New().String(),
		ApprovalStatus: domain.ApprovalStatusPosted,
	}
	if _, _, err := f.s.CreateJournal(ctx, h, lines); err != nil {
		t.Fatalf("CreateJournal: %v", err)
	}
	for _, step := range [][2]domain.JournalStatus{
		{domain.JournalStatusPending, domain.JournalStatusValidated},
		{domain.JournalStatusValidated, domain.JournalStatusFinalized},
	} {
		if err := f.s.TransitionJournal(ctx, tenant, h.JournalID, step[0], step[1], "preparer-1"); err != nil {
			t.Fatalf("transition to %s: %v", step[1], err)
		}
	}
	return h.JournalID
}

// reverseViaStore posts a reversal exactly as the ReverseJournal handler does:
// store.ReverseJournal with a header born FINALIZED. It returns the reversing
// journal id.
func (f *w4Fixture) reverseViaStore(t *testing.T, original, period, currency string, txn domain.Date, lines ...domain.JournalLine) string {
	t.Helper()
	principal := "reverser-1"
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: f.tenant, LegalEntityID: f.entity,
		FiscalPeriod: period, Status: domain.JournalStatusFinalized,
		ReversalOfJournalID: &original, Description: "Reversal of " + original,
		CreatedByPrincipalID: principal, PostedByPrincipalID: &principal,
		CorrelationID: uuid.New().String(),
		JournalType:   domain.JournalTypeReversal, TransactionDate: txn, PostingDate: txn, CurrencyCode: currency,
	}
	if _, _, err := f.s.ReverseJournal(f.ctx, f.tenant, original, h, invert(lines), principal); err != nil {
		t.Fatalf("ReverseJournal: %v", err)
	}
	return h.JournalID
}

// reverse posts a reversing journal (REVERSAL type, linked to the original)
// through the PENDING -> VALIDATED -> FINALIZED path, the only path that
// appends ledger_entries, so the ledger really holds the reversal entries.
func (f *w4Fixture) reverse(t *testing.T, original, period, currency string, txn domain.Date, lines ...domain.JournalLine) string {
	t.Helper()
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: f.tenant, LegalEntityID: f.entity,
		FiscalPeriod: period, Status: domain.JournalStatusPending,
		ReversalOfJournalID: &original, Description: "Reversal of " + original,
		CreatedByPrincipalID: "reverser-1", CorrelationID: uuid.New().String(),
		JournalType: domain.JournalTypeReversal, TransactionDate: txn, PostingDate: txn, CurrencyCode: currency,
		ApprovalStatus: domain.ApprovalStatusPosted,
	}
	if _, _, err := f.s.CreateJournal(f.ctx, h, invert(lines)); err != nil {
		t.Fatalf("CreateJournal (reversal): %v", err)
	}
	for _, step := range [][2]domain.JournalStatus{
		{domain.JournalStatusPending, domain.JournalStatusValidated},
		{domain.JournalStatusValidated, domain.JournalStatusFinalized},
	} {
		if err := f.s.TransitionJournal(f.ctx, f.tenant, h.JournalID, step[0], step[1], "reverser-1"); err != nil {
			t.Fatalf("reversal transition to %s: %v", step[1], err)
		}
	}
	return h.JournalID
}

func invert(lines []domain.JournalLine) []domain.JournalLine {
	inv := make([]domain.JournalLine, len(lines))
	for i, l := range lines {
		inv[i] = domain.JournalLine{AccountCode: l.AccountCode, DebitAmount: l.CreditAmount, CreditAmount: l.DebitAmount}
	}
	return inv
}

func (f *w4Fixture) ledgerRows(t *testing.T, journalID string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ledger_entries WHERE tenant_id=$1 AND journal_id=$2`, f.tenant, journalID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *w4Fixture) jat(t *testing.T, codes []string, nb string, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	p, err := f.s.QueryJournalAccountTotals(f.ctx, f.tenant, domain.JournalAccountTotalsQuery{
		LegalEntityID: f.entity, AccountCodes: codes, NormalBalance: nb, Limit: limit, AfterRecordID: after,
	})
	if err != nil {
		t.Fatalf("QueryJournalAccountTotals: %v", err)
	}
	return p
}

func (f *w4Fixture) tb(t *testing.T, period string, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	p, err := f.s.QueryTrialBalancePopulation(f.ctx, f.tenant, domain.TrialBalancePopulationQuery{
		LegalEntityID: f.entity, FiscalPeriod: period, Limit: limit, AfterRecordID: after,
	})
	if err != nil {
		t.Fatalf("QueryTrialBalancePopulation: %v", err)
	}
	return p
}

func byID(p *domain.ControlPopulationPage) map[string]domain.ControlPopulationRecord {
	m := map[string]domain.ControlPopulationRecord{}
	for _, r := range p.Records {
		m[r.RecordID] = r
	}
	return m
}

// ---- journal-account-totals ----

func TestJournalAccountTotals_OnlyRequestedAccounts_NetPerJournal_SignByNormalBalance(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	// Journal 1: two lines on 1200 (debit 100.10, credit 30.05) + revenue 4000.
	j1 := f.journal(t, "2026-09", "USD", d, dr("1200", 100.10), cr("1200", 30.05), cr("4000", 70.05))
	// Journal 2: touches 1200 and 1210 (both requested).
	j2 := f.journal(t, "2026-09", "USD", domain.NewDate(2026, 9, 5), dr("1200", 5), dr("1210", 2), cr("4000", 7))
	// Journal 3: unrelated accounts only.
	f.journal(t, "2026-09", "USD", d, dr("5000", 9), cr("6000", 9))

	p := f.jat(t, []string{"1200", "1210"}, "DEBIT", 100, "")
	if p.DeclaredTotals.RowCount != 2 || len(p.Records) != 2 {
		t.Fatalf("want exactly the 2 journals touching 1200/1210, got %+v", p.Records)
	}
	m := byID(p)
	r1, ok := m[j1+":USD"]
	if !ok {
		t.Fatalf("missing record for journal 1: %v", m)
	}
	if r1.Reference != j1 || r1.Amount != "70.05" || r1.Currency != "USD" || r1.Date != "2026-09-01" ||
		r1.Attributes["journal_id"] != j1 || r1.Attributes["fiscal_period"] != "2026-09" ||
		r1.Attributes["account_codes"] != "1200" || r1.Attributes["entry_count"] != "2" {
		t.Fatalf("journal 1 record wrong (net over multiple lines on the account): %+v", r1)
	}
	r2 := m[j2+":USD"]
	if r2.Amount != "7.00" || r2.Date != "2026-09-05" || r2.Attributes["account_codes"] != "1200,1210" || r2.Attributes["entry_count"] != "2" {
		t.Fatalf("journal 2 record wrong: %+v", r2)
	}
	if p.DeclaredTotals.Totals["USD"] != "77.05" {
		t.Fatalf("declared total = %q, want 77.05", p.DeclaredTotals.Totals["USD"])
	}

	// CREDIT normal balance flips the sign.
	c := f.jat(t, []string{"1200", "1210"}, "CREDIT", 100, "")
	cm := byID(c)
	if cm[j1+":USD"].Amount != "-70.05" || cm[j2+":USD"].Amount != "-7.00" || c.DeclaredTotals.Totals["USD"] != "-77.05" {
		t.Fatalf("CREDIT sign wrong: %+v totals=%v", c.Records, c.DeclaredTotals.Totals)
	}
	// A different account set: only that journal, and the DEBIT/CREDIT signs of 4000.
	rev := f.jat(t, []string{"4000"}, "CREDIT", 100, "")
	if rev.DeclaredTotals.RowCount != 2 || rev.DeclaredTotals.Totals["USD"] != "77.05" {
		t.Fatalf("4000 CREDIT: %+v", rev.DeclaredTotals)
	}
}

func TestJournalAccountTotals_TwoCurrencyJournal_YieldsTwoRecordsSameReference(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	// Ledger currency comes from the journal header, so a two-currency journal
	// is built the only way the schema can hold one: the header currency is
	// USD and one entry's stored currency is EUR is impossible through the
	// service. Post two journals and re-tag nothing; instead insert the second
	// currency's entry for the SAME journal directly (ledger_entries is
	// append-only but INSERT is permitted), as a legacy/foreign feed would.
	j := f.journal(t, "2026-09", "USD", d, dr("1200", 10), cr("4000", 10))
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO ledger_entries (ledger_entry_id, tenant_id, legal_entity_id, book_id, fiscal_period,
			journal_id, journal_line_id, line_number, account_code, debit_amount, credit_amount, currency_code,
			transaction_date, posting_date, correlation_id, created_at)
		VALUES ($1,$2,$3,'','2026-09',$4,$5,99,'1200',4.00,0,'EUR',$6,$6,'corr-x', now())`,
		uuid.New().String(), f.tenant, f.entity, j, uuid.New().String(), "2026-09-02")
	if err != nil {
		t.Fatalf("seeding the second-currency entry: %v", err)
	}

	p := f.jat(t, []string{"1200"}, "DEBIT", 100, "")
	if len(p.Records) != 2 {
		t.Fatalf("a two-currency journal must yield two records, got %+v", p.Records)
	}
	m := byID(p)
	usd, eur := m[j+":USD"], m[j+":EUR"]
	if usd.Reference != j || eur.Reference != j || usd.Amount != "10.00" || eur.Amount != "4.00" ||
		usd.Currency != "USD" || eur.Currency != "EUR" || eur.Date != "2026-09-02" {
		t.Fatalf("unexpected two-currency records: %+v / %+v", usd, eur)
	}
	if p.DeclaredTotals.Totals["USD"] != "10.00" || p.DeclaredTotals.Totals["EUR"] != "4.00" {
		t.Fatalf("declared totals must be per currency: %v", p.DeclaredTotals.Totals)
	}
}

func TestJournalAccountTotals_ReversalAppearsUnderItsOwnJournalID(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	orig := f.journal(t, "2026-09", "USD", d, dr("1200", 50), cr("4000", 50))
	rev := f.reverse(t, orig, "2026-09", "USD", d, dr("1200", 50), cr("4000", 50))
	t.Logf("ledger_entries rows: original=%d reversal=%d", f.ledgerRows(t, orig), f.ledgerRows(t, rev))
	if f.ledgerRows(t, rev) == 0 {
		t.Fatal("the reversing journal must have ledger entries")
	}

	p := f.jat(t, []string{"1200"}, "DEBIT", 100, "")
	m := byID(p)
	if len(m) != 2 || m[orig+":USD"].Amount != "50.00" || m[rev+":USD"].Amount != "-50.00" ||
		m[rev+":USD"].Reference != rev || m[orig+":USD"].Reference != orig {
		t.Fatalf("original and reversal must be separate records under their own ids: %+v", p.Records)
	}
	if p.DeclaredTotals.Totals["USD"] != "0.00" {
		t.Fatalf("total should net to zero, got %v", p.DeclaredTotals.Totals)
	}
}

func TestJournalAccountTotals_Paging_EveryRowOnce_StableWatermark_TotalsTie(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	for i := 0; i < 5; i++ {
		f.journal(t, "2026-09", "USD", d, dr("1200", float64(10+i)+0.1), cr("4000", float64(10+i)+0.1))
	}
	seen := map[string]bool{}
	sum := new(big.Rat)
	var watermark, prev string
	after, pages := "", 0
	for {
		p := f.jat(t, []string{"1200"}, "DEBIT", 2, after)
		pages++
		if watermark == "" {
			watermark = p.Watermark
		} else if p.Watermark != watermark {
			t.Fatalf("watermark changed between pages: %q vs %q", watermark, p.Watermark)
		}
		if p.DeclaredTotals.RowCount != 5 {
			t.Fatalf("row_count = %d, want 5 on every page", p.DeclaredTotals.RowCount)
		}
		for _, r := range p.Records {
			if seen[r.RecordID] {
				t.Fatalf("record %s twice", r.RecordID)
			}
			if prev != "" && r.RecordID <= prev {
				t.Fatalf("not ascending: %s after %s", r.RecordID, prev)
			}
			prev = r.RecordID
			seen[r.RecordID] = true
			v, ok := new(big.Rat).SetString(r.Amount)
			if !ok {
				t.Fatalf("amount %q not decimal", r.Amount)
			}
			sum.Add(sum, v)
		}
		if p.NextRecordID == "" {
			break
		}
		after = p.NextRecordID
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("visited %d rows over %d pages, want 5/3", len(seen), pages)
	}
	declared, _ := new(big.Rat).SetString(f.jat(t, []string{"1200"}, "DEBIT", 2, "").DeclaredTotals.Totals["USD"])
	if declared.Cmp(sum) != 0 || sum.FloatString(2) != "60.50" {
		t.Fatalf("declared %s vs sum %s (want 60.50)", declared.FloatString(2), sum.FloatString(2))
	}
}

func TestJournalAccountTotals_TenantEntityIsolation_AndWatermark(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	mine := f.journal(t, "2026-09", "USD", d, dr("1200", 5), cr("4000", 5))
	otherTenant, otherEntity := uuid.New().String(), uuid.New().String()
	f.journalAs(t, otherTenant, otherEntity, "2026-09", "USD", d, dr("1200", 99), cr("4000", 99))
	f.journalAs(t, otherTenant, f.entity, "2026-09", "USD", d, dr("1200", 77), cr("4000", 77))
	f.journalAs(t, f.tenant, otherEntity, "2026-09", "USD", d, dr("1200", 55), cr("4000", 55))

	before := f.jat(t, []string{"1200"}, "DEBIT", 100, "")
	if len(before.Records) != 1 || before.Records[0].Reference != mine {
		t.Fatalf("expected only this tenant+entity's journal, got %+v", before.Records)
	}
	if again := f.jat(t, []string{"1200"}, "DEBIT", 100, ""); again.Watermark != before.Watermark {
		t.Fatal("watermark must be stable when nothing changed")
	}
	f.journal(t, "2026-09", "USD", d, dr("5000", 3), cr("6000", 3))
	if unrelated := f.jat(t, []string{"1200"}, "DEBIT", 100, ""); unrelated.Watermark != before.Watermark {
		t.Fatal("a posting outside the population must not change the watermark")
	}
	f.journal(t, "2026-09", "USD", d, dr("1200", 1), cr("4000", 1))
	after := f.jat(t, []string{"1200"}, "DEBIT", 100, "")
	if after.Watermark == before.Watermark || after.DeclaredTotals.RowCount != 2 {
		t.Fatalf("posting another journal must change the watermark: %q vs %q", before.Watermark, after.Watermark)
	}

	foreign, err := f.s.QueryJournalAccountTotals(svcmiddleware.WithTenant(context.Background(), uuid.New().String()),
		uuid.New().String(), domain.JournalAccountTotalsQuery{
			LegalEntityID: f.entity, AccountCodes: []string{"1200"}, NormalBalance: "DEBIT", Limit: 10,
		})
	if err != nil {
		t.Fatal(err)
	}
	if foreign.DeclaredTotals.RowCount != 0 || len(foreign.Records) != 0 {
		t.Fatalf("a foreign tenant saw rows: %+v", foreign)
	}
}

// ---- trial-balance ----

func TestTrialBalancePopulation_ExactPeriod_SignedNets_ReversalsCounted_MaxSeq(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	orig := f.journal(t, "2026-09", "USD", d, dr("1200", 0.10), cr("4000", 0.10))
	f.journal(t, "2026-09", "USD", d, dr("1200", 0.20), cr("4000", 0.20)) // 0.1+0.2 must be exactly 0.30
	f.journal(t, "2026-09", "USD", d, dr("1000", 1234567890123.45), cr("4000", 1234567890123.45))
	// Other periods must be excluded, including a near-miss text.
	f.journal(t, "2026-10", "USD", d, dr("1200", 500), cr("4000", 500))
	f.journal(t, "2026-9", "USD", d, dr("1200", 600), cr("4000", 600))
	f.journal(t, "2026-09 ", "USD", d, dr("1200", 700), cr("4000", 700))

	p := f.tb(t, "2026-09", 100, "")
	m := byID(p)
	a, ok := m["1200:USD"]
	if !ok || a.Reference != "1200" || a.Amount != "0.30" || a.Currency != "USD" || a.Date != "2026-09-01" ||
		a.Attributes["account_code"] != "1200" || a.Attributes["fiscal_period"] != "2026-09" || a.Attributes["entry_count"] != "2" {
		t.Fatalf("1200 record wrong (exact sum, exact period only): %+v", a)
	}
	if m["4000:USD"].Amount != "-1234567890123.75" || m["1000:USD"].Amount != "1234567890123.45" {
		t.Fatalf("signed nets wrong: %+v", p.Records)
	}
	if p.DeclaredTotals.RowCount != 3 || p.DeclaredTotals.Totals["USD"] != "0.00" {
		t.Fatalf("a balanced ledger's trial balance must total zero: %+v", p.DeclaredTotals)
	}

	var maxSeq int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT MAX(entry_seq) FROM ledger_entries WHERE tenant_id=$1 AND legal_entity_id=$2 AND fiscal_period='2026-09'`,
		f.tenant, f.entity).Scan(&maxSeq); err != nil {
		t.Fatal(err)
	}
	wantWM := "gl2:n=3;entry_seq=" + itoa64(maxSeq) + ";md5="
	if len(p.Watermark) <= len(wantWM) || p.Watermark[:len(wantWM)] != wantWM {
		t.Fatalf("watermark %q should start with %q", p.Watermark, wantWM)
	}
	if got := m["4000:USD"].Attributes["max_entry_seq"]; got == "" {
		t.Fatal("max_entry_seq attribute missing")
	}
	var acct4000Max int64
	_ = f.pool.QueryRow(context.Background(),
		`SELECT MAX(entry_seq) FROM ledger_entries WHERE tenant_id=$1 AND fiscal_period='2026-09' AND account_code='4000'`, f.tenant).Scan(&acct4000Max)
	if m["4000:USD"].Attributes["max_entry_seq"] != itoa64(acct4000Max) {
		t.Fatalf("max_entry_seq = %q, want %d", m["4000:USD"].Attributes["max_entry_seq"], acct4000Max)
	}

	// Reversal: original and its reversal are both in the ledger and both counted.
	rev := f.reverse(t, orig, "2026-09", "USD", d, dr("1200", 0.10), cr("4000", 0.10))
	if f.ledgerRows(t, rev) == 0 {
		t.Fatal("the reversing journal must have ledger entries")
	}
	after := f.tb(t, "2026-09", 100, "")
	if got := byID(after)["1200:USD"]; got.Amount != "0.20" || got.Attributes["entry_count"] != "3" {
		t.Fatalf("original + reversal must both count (0.10 + 0.20 - 0.10): %+v", got)
	}
}

func TestTrialBalancePopulation_OriginalAndReversalNetToZero(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	orig := f.journal(t, "2026-09", "USD", d, dr("1200", 50), cr("4000", 50))
	rev := f.reverse(t, orig, "2026-09", "USD", d, dr("1200", 50), cr("4000", 50))
	if f.ledgerRows(t, rev) == 0 {
		t.Fatal("the reversing journal must have ledger entries")
	}
	p := f.tb(t, "2026-09", 100, "")
	m := byID(p)
	if m["1200:USD"].Amount != "0.00" || m["1200:USD"].Attributes["entry_count"] != "2" ||
		m["4000:USD"].Amount != "0.00" || m["4000:USD"].Attributes["entry_count"] != "2" {
		t.Fatalf("reversed original + reversal must both be counted and net to zero: %+v", p.Records)
	}
}

func TestTrialBalancePopulation_MultiCurrencyAccountsSplit(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	f.journal(t, "2026-09", "USD", d, dr("1200", 10), cr("4000", 10))
	f.journal(t, "2026-09", "EUR", d, dr("1200", 3), cr("4000", 3))
	p := f.tb(t, "2026-09", 100, "")
	m := byID(p)
	if len(m) != 4 || m["1200:USD"].Amount != "10.00" || m["1200:EUR"].Amount != "3.00" {
		t.Fatalf("account+currency grouping wrong: %+v", p.Records)
	}
	if p.DeclaredTotals.Totals["USD"] != "0.00" || p.DeclaredTotals.Totals["EUR"] != "0.00" {
		t.Fatalf("totals per currency: %v", p.DeclaredTotals.Totals)
	}
}

func TestTrialBalancePopulation_Paging_Isolation_Watermark_AndNoSnapshotInsert(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	for i, acct := range []string{"1000", "1100", "1200", "1300", "1400"} {
		f.journal(t, "2026-09", "USD", d, dr(acct, float64(i)+1.25), cr("4000", float64(i)+1.25))
	}
	// 6 accounts (5 debit + 4000).
	otherTenant, otherEntity := uuid.New().String(), uuid.New().String()
	f.journalAs(t, otherTenant, otherEntity, "2026-09", "USD", d, dr("1000", 99), cr("4000", 99))
	f.journalAs(t, otherTenant, f.entity, "2026-09", "USD", d, dr("1000", 98), cr("4000", 98))

	snapshots := func() int {
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM trial_balance_snapshots`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	snapsBefore := snapshots()

	seen := map[string]bool{}
	sum := new(big.Rat)
	var watermark, prev string
	after, pages := "", 0
	for {
		p := f.tb(t, "2026-09", 2, after)
		pages++
		if watermark == "" {
			watermark = p.Watermark
		} else if p.Watermark != watermark {
			t.Fatalf("watermark changed between pages")
		}
		if p.DeclaredTotals.RowCount != 6 {
			t.Fatalf("row_count = %d, want 6", p.DeclaredTotals.RowCount)
		}
		for _, r := range p.Records {
			if seen[r.RecordID] || (prev != "" && r.RecordID <= prev) {
				t.Fatalf("record %s repeated or out of order (prev %s)", r.RecordID, prev)
			}
			prev = r.RecordID
			seen[r.RecordID] = true
			v, _ := new(big.Rat).SetString(r.Amount)
			sum.Add(sum, v)
		}
		if p.NextRecordID == "" {
			break
		}
		after = p.NextRecordID
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 6 || pages != 3 {
		t.Fatalf("visited %d rows over %d pages, want 6/3", len(seen), pages)
	}
	declared, _ := new(big.Rat).SetString(f.tb(t, "2026-09", 2, "").DeclaredTotals.Totals["USD"])
	if declared.Cmp(sum) != 0 || sum.Sign() != 0 {
		t.Fatalf("declared %s vs sum %s (balanced ledger -> 0)", declared.FloatString(2), sum.FloatString(2))
	}

	if snapshots() != snapsBefore {
		t.Fatalf("the trial-balance population must be read-only: trial_balance_snapshots %d -> %d", snapsBefore, snapshots())
	}

	before := f.tb(t, "2026-09", 100, "")
	f.journal(t, "2026-10", "USD", d, dr("1000", 1), cr("4000", 1))
	if other := f.tb(t, "2026-09", 100, ""); other.Watermark != before.Watermark {
		t.Fatal("a posting in another period must not change this period's watermark")
	}
	f.journal(t, "2026-09", "USD", d, dr("1000", 1), cr("4000", 1))
	if changed := f.tb(t, "2026-09", 100, ""); changed.Watermark == before.Watermark {
		t.Fatal("a new posting in the period must change the watermark")
	}
	if snapshots() != snapsBefore {
		t.Fatal("reads must still not insert snapshots")
	}

	empty := f.tb(t, "no-such-period", 10, "")
	if empty.DeclaredTotals.RowCount != 0 || len(empty.Records) != 0 {
		t.Fatalf("unknown period should be empty: %+v", empty)
	}
}

func itoa64(n int64) string { return big.NewInt(n).String() }

// accountNet returns SUM(debit)-SUM(credit) per account over the given
// journals' ledger_entries.
func (f *w4Fixture) accountNet(t *testing.T, journalIDs ...string) map[string]float64 {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT account_code, SUM(debit_amount) - SUM(credit_amount)
		FROM ledger_entries
		WHERE tenant_id = $1 AND journal_id = ANY($2::uuid[])
		GROUP BY account_code`, f.tenant, journalIDs)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var code string
		var net float64
		if err := rows.Scan(&code, &net); err != nil {
			t.Fatal(err)
		}
		out[code] = net
	}
	return out
}

func (f *w4Fixture) outboxCount(t *testing.T, journalID, eventType string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE tenant_id=$1 AND aggregate_id=$2 AND event_type=$3`,
		f.tenant, journalID, eventType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AK-INV-003: a correction creates new accounting records. The reversing
// journal must carry its own ledger entries, and the pair must net to zero.
func TestReverseJournalViaStore_PostsNeutralizingLedgerEntries(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	orig := f.journal(t, "2026-09", "USD", d, dr("1200", 50), cr("4000", 50))
	rev := f.reverseViaStore(t, orig, "2026-09", "USD", d, dr("1200", 50), cr("4000", 50))

	if got := f.ledgerRows(t, orig); got != 2 {
		t.Fatalf("original ledger rows = %d, want 2", got)
	}
	if got := f.ledgerRows(t, rev); got != 2 {
		t.Fatalf("reversal ledger rows = %d, want 2", got)
	}
	// Debit/credit really swapped on the reversal.
	only := f.accountNet(t, rev)
	if only["1200"] != -50 || only["4000"] != 50 {
		t.Fatalf("reversal nets = %v, want 1200:-50 4000:+50", only)
	}
	// Pair nets to zero per account and in total.
	for code, net := range f.accountNet(t, orig, rev) {
		if net != 0 {
			t.Errorf("account %s nets %v across original+reversal, want 0", code, net)
		}
	}
	// Trial balance population: totals of debits equal credits over both.
	p := f.tb(t, "2026-09", 100, "")
	if p.DeclaredTotals.RowCount == 0 {
		t.Fatal("trial balance should see the posted entries")
	}
	if f.outboxCount(t, rev, "journal.posted") != 0 {
		t.Errorf("reversal emits journal.reversed only, not a separate journal.posted")
	}
	if f.outboxCount(t, orig, "journal.reversed") != 1 {
		t.Errorf("expected one journal.reversed event for the original")
	}
}

// An idempotent replay (same correlation_id) must not double-post.
func TestReverseJournalViaStore_ReplayDoesNotDoublePost(t *testing.T) {
	f := newW4Fixture(t)
	d := domain.NewDate(2026, 9, 1)
	orig := f.journal(t, "2026-09", "USD", d, dr("1200", 50), cr("4000", 50))
	principal := "reverser-1"
	corr := uuid.New().String()
	build := func() *domain.JournalHeader {
		return &domain.JournalHeader{
			JournalID: uuid.New().String(), TenantID: f.tenant, LegalEntityID: f.entity,
			FiscalPeriod: "2026-09", Status: domain.JournalStatusFinalized,
			ReversalOfJournalID: &orig, Description: "Reversal of " + orig,
			CreatedByPrincipalID: principal, PostedByPrincipalID: &principal,
			CorrelationID: corr, JournalType: domain.JournalTypeReversal,
			TransactionDate: d, PostingDate: d, CurrencyCode: "USD",
		}
	}
	lines := invert([]domain.JournalLine{dr("1200", 50), cr("4000", 50)})

	first := build()
	if _, created, err := f.s.ReverseJournal(f.ctx, f.tenant, orig, first, lines, principal); err != nil || !created {
		t.Fatalf("first reversal: created=%v err=%v", created, err)
	}
	second := build()
	if _, created, err := f.s.ReverseJournal(f.ctx, f.tenant, orig, second, lines, principal); err != nil || created {
		t.Fatalf("replay: created=%v err=%v, want created=false", created, err)
	}

	var total int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM ledger_entries WHERE tenant_id=$1`, f.tenant).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 4 {
		t.Fatalf("ledger rows after replay = %d, want 4 (2 original + 2 reversal)", total)
	}
	if f.outboxCount(t, orig, "journal.reversed") != 1 {
		t.Error("expected exactly one journal.reversed for the original")
	}
}

// A journal built exactly as PostAccountingEvent now builds it stores its
// currency and dates, and its ledger rows carry them.
func TestEventPostedHeader_LedgerRowsCarryCurrencyAndDates(t *testing.T) {
	f := newW4Fixture(t)
	src := "EVT-1"
	doc, post := domain.NewDate(2026, 9, 1), domain.NewDate(2026, 9, 3)
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: f.tenant, LegalEntityID: f.entity,
		FiscalPeriod: "2026-09", Status: domain.JournalStatusPending, Description: "event",
		CreatedByPrincipalID: "svc-1", CorrelationID: uuid.New().String(), SourceEventID: &src,
		JournalType: domain.JournalTypeStandard, TransactionDate: doc, PostingDate: post, CurrencyCode: "EUR",
		ApprovalStatus: domain.ApprovalStatusPostingRequested,
	}
	if _, _, err := f.s.CreateJournal(f.ctx, h, []domain.JournalLine{dr("1200", 5), cr("4000", 5)}); err != nil {
		t.Fatalf("CreateJournal with event-style header: %v", err)
	}
	for _, step := range [][2]domain.JournalStatus{
		{domain.JournalStatusPending, domain.JournalStatusValidated},
		{domain.JournalStatusValidated, domain.JournalStatusFinalized},
	} {
		if err := f.s.TransitionJournal(f.ctx, f.tenant, h.JournalID, step[0], step[1], "svc-1"); err != nil {
			t.Fatalf("transition to %s: %v", step[1], err)
		}
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM ledger_entries
		WHERE tenant_id=$1 AND journal_id=$2 AND currency_code='EUR'
		  AND transaction_date='2026-09-01' AND posting_date='2026-09-03' AND source_event_id=$3`,
		f.tenant, h.JournalID, src).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("ledger rows carrying currency/dates/source = %d, want 2", n)
	}
	p := f.tb(t, "2026-09", 10, "")
	for _, r := range p.Records {
		if r.Currency != "EUR" {
			t.Errorf("trial balance record currency = %q, want EUR", r.Currency)
		}
	}
}
