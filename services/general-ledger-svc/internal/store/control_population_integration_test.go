//go:build integration

package store_test

import (
	"context"
	"math/big"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

type cpFixture struct {
	s      *store.PgStore
	tenant string
	entity string
	ctx    context.Context
}

func newCPFixture(t *testing.T) *cpFixture {
	t.Helper()
	pool := openTestPool(t)
	tenant := uuid.New().String()
	return &cpFixture{
		s:      store.New(pool, zap.NewNop()),
		tenant: tenant,
		entity: uuid.New().String(),
		ctx:    svcmiddleware.WithTenant(context.Background(), tenant),
	}
}

// post creates and FINALIZES a two-line journal: debit debitAcct / credit
// creditAcct for amount, in the fixture's tenant and entity.
func (f *cpFixture) post(t *testing.T, srcEvent *string, txn domain.Date, debitAcct, creditAcct string, amount float64) string {
	t.Helper()
	return f.postAs(t, f.tenant, f.entity, srcEvent, txn, debitAcct, creditAcct, amount)
}

func (f *cpFixture) postAs(t *testing.T, tenant, entity string, srcEvent *string, txn domain.Date, debitAcct, creditAcct string, amount float64) string {
	t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	h := &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenant, LegalEntityID: entity,
		FiscalPeriod: txn.String()[:7], Status: domain.JournalStatusPending,
		JournalType: domain.JournalTypeStandard, TransactionDate: txn,
		PostingDate: txn, CurrencyCode: "USD",
		CreatedByPrincipalID: "preparer-1", CorrelationID: uuid.New().String(),
		ApprovalStatus: domain.ApprovalStatusPosted, SourceEventID: srcEvent,
	}
	lines := []domain.JournalLine{
		{AccountCode: debitAcct, DebitAmount: amount},
		{AccountCode: creditAcct, CreditAmount: amount},
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

func (f *cpFixture) query(t *testing.T, codes []string, nb, periodEnd string, limit int, after string) *domain.AccountPostingsPage {
	t.Helper()
	p, err := f.s.QueryAccountPostings(f.ctx, f.tenant, domain.AccountPostingsQuery{
		LegalEntityID: f.entity, PeriodEnd: periodEnd, AccountCodes: codes,
		NormalBalance: nb, Limit: limit, AfterRecordID: after,
	})
	if err != nil {
		t.Fatalf("QueryAccountPostings: %v", err)
	}
	return p
}

func sp(s string) *string { return &s }

func TestControlPopulation_OnlyRequestedAccountsAppear_AndSignFollowsNormalBalance(t *testing.T) {
	f := newCPFixture(t)
	d := domain.NewDate(2026, 9, 1)
	// AR 1200 debited / revenue 4000 credited; then a receipt: cash 1000 debit / AR 1200 credit.
	f.post(t, sp("INV-1"), d, "1200", "4000", 100.25)
	f.post(t, sp("INV-1"), d, "1000", "1200", 40)
	f.post(t, sp("INV-2"), d, "5000", "6000", 7) // unrelated accounts

	debit := f.query(t, []string{"1200"}, "DEBIT", "", 100, "")
	if len(debit.Records) != 2 || debit.DeclaredTotals.RowCount != 2 {
		t.Fatalf("expected exactly the 2 postings on account 1200, got %+v", debit.Records)
	}
	got := map[string]string{}
	for _, r := range debit.Records {
		if r.Attributes["account_code"] != "1200" {
			t.Fatalf("entry on another account leaked: %+v", r)
		}
		if r.Reference != "INV-1" || r.Currency != "USD" || r.Date != "2026-09-01" {
			t.Fatalf("unexpected record: %+v", r)
		}
		got[r.Amount] = r.RecordID
	}
	if _, ok := got["100.25"]; !ok {
		t.Fatalf("DEBIT normal balance: invoice line should be +100.25, got %v", got)
	}
	if _, ok := got["-40.00"]; !ok {
		t.Fatalf("DEBIT normal balance: receipt line should be -40.00, got %v", got)
	}
	if debit.DeclaredTotals.Totals["USD"] != "60.25" {
		t.Fatalf("declared total = %q, want 60.25", debit.DeclaredTotals.Totals["USD"])
	}

	credit := f.query(t, []string{"1200"}, "CREDIT", "", 100, "")
	got = map[string]string{}
	for _, r := range credit.Records {
		got[r.Amount] = r.RecordID
	}
	if _, ok := got["-100.25"]; !ok {
		t.Fatalf("CREDIT normal balance: invoice line should be -100.25, got %v", got)
	}
	if _, ok := got["40.00"]; !ok {
		t.Fatalf("CREDIT normal balance: receipt line should be +40.00, got %v", got)
	}

	// Two accounts: the revenue-side entry now appears too.
	both := f.query(t, []string{"1200", "4000"}, "CREDIT", "", 100, "")
	if both.DeclaredTotals.RowCount != 3 {
		t.Fatalf("expected 3 entries on 1200+4000, got %d", both.DeclaredTotals.RowCount)
	}
}

func TestControlPopulation_EmptySourceEventID_BecomesJournalReference(t *testing.T) {
	f := newCPFixture(t)
	d := domain.NewDate(2026, 9, 1)
	nilID := f.post(t, nil, d, "1200", "4000", 10)
	blankID := f.post(t, sp("   "), d, "1200", "4000", 20)
	named := f.post(t, sp("INV-9"), d, "1200", "4000", 30)

	p := f.query(t, []string{"1200"}, "DEBIT", "", 100, "")
	refs := map[string]string{}
	for _, r := range p.Records {
		refs[r.Attributes["journal_id"]] = r.Reference
	}
	if refs[nilID] != "journal:"+nilID {
		t.Fatalf("NULL source_event_id -> %q, want journal:%s", refs[nilID], nilID)
	}
	if refs[blankID] != "journal:"+blankID {
		t.Fatalf("blank source_event_id -> %q, want journal:%s", refs[blankID], blankID)
	}
	if refs[named] != "INV-9" {
		t.Fatalf("named reference lost: %q", refs[named])
	}
}

func TestControlPopulation_Paging_VisitsEveryRowOnce_WithStableWatermark_AndTotalsTie(t *testing.T) {
	f := newCPFixture(t)
	d := domain.NewDate(2026, 9, 1)
	for i := 0; i < 5; i++ {
		f.post(t, sp("INV-"+string(rune('A'+i))), d, "1200", "4000", float64(10+i)+0.5)
	}

	seen := map[string]bool{}
	sum := new(big.Rat)
	var watermark, prev string
	after, pages := "", 0
	for {
		p := f.query(t, []string{"1200"}, "DEBIT", "", 2, after)
		pages++
		if watermark == "" {
			watermark = p.Watermark
		} else if p.Watermark != watermark {
			t.Fatalf("watermark changed between pages: %q vs %q", watermark, p.Watermark)
		}
		if p.DeclaredTotals.RowCount != 5 {
			t.Fatalf("declared row_count = %d, want 5 on every page", p.DeclaredTotals.RowCount)
		}
		for _, r := range p.Records {
			if seen[r.RecordID] {
				t.Fatalf("record %s returned twice", r.RecordID)
			}
			if prev != "" && r.RecordID <= prev {
				t.Fatalf("records not ascending by record_id: %s after %s", r.RecordID, prev)
			}
			prev = r.RecordID
			seen[r.RecordID] = true
			v, ok := new(big.Rat).SetString(r.Amount)
			if !ok {
				t.Fatalf("amount %q is not a decimal string", r.Amount)
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
		t.Fatalf("visited %d rows over %d pages, want 5 rows / 3 pages", len(seen), pages)
	}
	declared, _ := new(big.Rat).SetString(f.query(t, []string{"1200"}, "DEBIT", "", 2, "").DeclaredTotals.Totals["USD"])
	if declared.Cmp(sum) != 0 {
		t.Fatalf("declared total %s != sum of returned records %s", declared.FloatString(2), sum.FloatString(2))
	}
}

func TestControlPopulation_PeriodCutoff(t *testing.T) {
	f := newCPFixture(t)
	f.post(t, sp("A"), domain.NewDate(2026, 8, 31), "1200", "4000", 1)
	f.post(t, sp("B"), domain.NewDate(2026, 9, 30), "1200", "4000", 2)
	f.post(t, sp("C"), domain.NewDate(2026, 10, 1), "1200", "4000", 4)

	if n := f.query(t, []string{"1200"}, "DEBIT", "2026-08-31", 100, "").DeclaredTotals.RowCount; n != 1 {
		t.Fatalf("cut-off 2026-08-31: %d rows, want 1", n)
	}
	sep := f.query(t, []string{"1200"}, "DEBIT", "2026-09-30", 100, "")
	if sep.DeclaredTotals.RowCount != 2 || sep.DeclaredTotals.Totals["USD"] != "3.00" {
		t.Fatalf("cut-off 2026-09-30 (inclusive, cumulative): %+v", sep.DeclaredTotals)
	}
	if n := f.query(t, []string{"1200"}, "DEBIT", "", 100, "").DeclaredTotals.RowCount; n != 3 {
		t.Fatalf("no cut-off: %d rows, want 3", n)
	}
}

func TestControlPopulation_TenantAndEntityIsolation(t *testing.T) {
	f := newCPFixture(t)
	d := domain.NewDate(2026, 9, 1)
	f.post(t, sp("MINE"), d, "1200", "4000", 5)

	otherTenant, otherEntity := uuid.New().String(), uuid.New().String()
	f.postAs(t, otherTenant, otherEntity, sp("THEIRS"), d, "1200", "4000", 99)
	// Same entity id under another tenant must not leak either.
	f.postAs(t, otherTenant, f.entity, sp("THEIRS-SAME-ENTITY"), d, "1200", "4000", 77)

	p := f.query(t, []string{"1200"}, "DEBIT", "", 100, "")
	if len(p.Records) != 1 || p.Records[0].Reference != "MINE" {
		t.Fatalf("expected only this tenant's posting, got %+v", p.Records)
	}

	// Asking as another tenant for this tenant's entity yields an empty population.
	empty, err := f.s.QueryAccountPostings(svcmiddleware.WithTenant(context.Background(), uuid.New().String()),
		uuid.New().String(), domain.AccountPostingsQuery{
			LegalEntityID: f.entity, AccountCodes: []string{"1200"}, NormalBalance: "DEBIT", Limit: 10,
		})
	if err != nil {
		t.Fatal(err)
	}
	if empty.DeclaredTotals.RowCount != 0 || len(empty.Records) != 0 {
		t.Fatalf("a foreign tenant saw rows: %+v", empty)
	}
}

func TestControlPopulation_NewPostingChangesWatermark_UnrelatedDoesNot(t *testing.T) {
	f := newCPFixture(t)
	d := domain.NewDate(2026, 9, 1)
	f.post(t, sp("INV-1"), d, "1200", "4000", 10)
	before := f.query(t, []string{"1200"}, "DEBIT", "", 100, "")
	again := f.query(t, []string{"1200"}, "DEBIT", "", 100, "")
	if before.Watermark != again.Watermark {
		t.Fatal("watermark must be stable when nothing changed")
	}

	f.post(t, sp("UNRELATED"), d, "5000", "6000", 3)
	if unrelated := f.query(t, []string{"1200"}, "DEBIT", "", 100, ""); unrelated.Watermark != before.Watermark {
		t.Fatal("a posting outside the population must not change its watermark")
	}

	f.post(t, sp("INV-2"), d, "1200", "4000", 1)
	after := f.query(t, []string{"1200"}, "DEBIT", "", 100, "")
	if after.Watermark == before.Watermark {
		t.Fatal("posting one more journal into the population must change the watermark")
	}
	if after.DeclaredTotals.RowCount != 2 {
		t.Fatalf("row_count = %d, want 2", after.DeclaredTotals.RowCount)
	}
}
