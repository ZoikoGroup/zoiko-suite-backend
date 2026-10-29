//go:build integration

package store_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// clearingFixture posts finalized journals on accounts the chart marks CLEARING, so
// ledger_entries.control_type is stamped by the store's normal posting path.
func (f *w4Fixture) clearingAccount(t *testing.T, code string) {
	t.Helper()
	f.account(t, f.tenant, code, false, false)
	if err := fixtureExec(context.Background(), f.pool,
		`UPDATE chart_of_accounts SET control_type='CLEARING' WHERE tenant_id=$1::uuid AND account_code=$2`, f.tenant, code); err != nil {
		t.Fatalf("mark %s CLEARING: %v", code, err)
	}
}

func (f *w4Fixture) aging(t *testing.T, before string, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	p, err := f.s.QueryClearingBalanceAging(f.ctx, f.tenant, domain.ClearingBalanceAgingQuery{
		LegalEntityID: f.entity, AgedBefore: before, Limit: limit, AfterRecordID: after})
	if err != nil {
		t.Fatalf("QueryClearingBalanceAging: %v", err)
	}
	return p
}

func agedByAccount(p *domain.ControlPopulationPage) map[string]domain.ControlPopulationRecord {
	m := map[string]domain.ControlPopulationRecord{}
	for _, r := range p.Records {
		m[r.Reference] = r
	}
	return m
}

func TestClearingBalanceAging_FIFOAgesOnlyTheOpenBalanceOlderThanTheCutOff(t *testing.T) {
	f := newW4Fixture(t)
	for _, c := range []string{"CLR-1", "CLR-2", "CLR-3", "CLR-5"} {
		f.clearingAccount(t, c)
	}
	post := func(period string, d domain.Date, lines ...domain.JournalLine) { f.manual(t, period, d, lines...) }

	// CLR-1 (credit balance): Cr 100 old, Dr 30 partial clearing, Cr 20 recent. Open 90; FIFO: 70 old + 20 recent.
	post("2026-07", domain.NewDate(2026, 7, 10), cr("CLR-1", 100), dr("6000", 100))
	post("2026-08", domain.NewDate(2026, 8, 1), dr("CLR-1", 30), cr("6000", 30))
	post("2026-09", domain.NewDate(2026, 9, 5), cr("CLR-1", 20), dr("6000", 20))
	// CLR-2: fully cleared, nothing open.
	post("2026-07", domain.NewDate(2026, 7, 1), dr("CLR-2", 50), cr("6000", 50))
	post("2026-08", domain.NewDate(2026, 8, 20), cr("CLR-2", 50), dr("6000", 50))
	// CLR-3: only recent activity, nothing aged.
	post("2026-09", domain.NewDate(2026, 9, 10), cr("CLR-3", 40), dr("6000", 40))
	// CLR-5: Cr 100 old, then Cr 50 and Dr 50 both recent. The recent debit clears the OLDEST 50: 50 old + 50 recent.
	post("2026-07", domain.NewDate(2026, 7, 1), cr("CLR-5", 100), dr("6000", 100))
	post("2026-09", domain.NewDate(2026, 9, 5), cr("CLR-5", 50), dr("6000", 50))
	post("2026-09", domain.NewDate(2026, 9, 6), dr("CLR-5", 50), cr("6000", 50))
	// A non-clearing account with a large old balance is never in scope.
	post("2026-06", domain.NewDate(2026, 6, 1), dr("6000", 5000), cr("7000", 5000))

	p := f.aging(t, "2026-09-01", 100, "")
	got := agedByAccount(p)
	if len(got) != 2 || p.DeclaredTotals.RowCount != 2 {
		t.Fatalf("only CLR-1 and CLR-5 have an aged balance, got %+v", p.Records)
	}
	c1, c5 := got["CLR-1"], got["CLR-5"]
	if c1.Amount != "70.00" || c1.Currency != "USD" || c1.Attributes["direction"] != "CREDIT" ||
		c1.Attributes["open_balance"] != "90.00" || c1.Attributes["recent_amount"] != "20.00" ||
		c1.Attributes["aged_amount"] != "70.00" || c1.Attributes["oldest_posting_date"] != "2026-07-10" ||
		c1.Attributes["age_days"] != "53" || c1.Date != "2026-08-01" {
		t.Fatalf("CLR-1 record %+v", c1)
	}
	if c5.Amount != "50.00" || c5.Attributes["open_balance"] != "100.00" || c5.Attributes["recent_amount"] != "50.00" {
		t.Fatalf("CLR-5 record %+v", c5)
	}
	if p.DeclaredTotals.Totals["USD"] != "120.00" || !strings.HasPrefix(p.Watermark, "gl9:n=2;") {
		t.Fatalf("totals %+v watermark %q", p.DeclaredTotals, p.Watermark)
	}

	// A later cut-off ages everything still open: 90 + 40 + 100.
	later := f.aging(t, "2026-10-01", 100, "")
	if gl := agedByAccount(later); len(gl) != 3 || later.DeclaredTotals.Totals["USD"] != "230.00" || gl["CLR-3"].Amount != "40.00" {
		t.Fatalf("later cut-off %+v totals %+v", later.Records, later.DeclaredTotals)
	}
	// An earlier cut-off than any posting ages nothing: everything is recent.
	if early := f.aging(t, "2026-06-01", 100, ""); len(early.Records) != 0 {
		t.Fatalf("nothing is older than 2026-06-01 among clearing accounts: %+v", early.Records)
	}
	if p.Watermark == later.Watermark {
		t.Fatal("different populations must not share a watermark")
	}

	// Tenant and entity isolation.
	other, err := f.s.QueryClearingBalanceAging(f.ctx, uuid.NewString(), domain.ClearingBalanceAgingQuery{LegalEntityID: f.entity, AgedBefore: "2026-10-01", Limit: 10})
	if err != nil || len(other.Records) != 0 {
		t.Fatalf("another tenant must see nothing: %v %+v", err, other)
	}
	otherEntity, err := f.s.QueryClearingBalanceAging(f.ctx, f.tenant, domain.ClearingBalanceAgingQuery{LegalEntityID: uuid.NewString(), AgedBefore: "2026-10-01", Limit: 10})
	if err != nil || len(otherEntity.Records) != 0 {
		t.Fatalf("another entity must see nothing: %v %+v", err, otherEntity)
	}
}

func TestClearingBalanceAging_DebitBalancesAndKeysetPaging(t *testing.T) {
	f := newW4Fixture(t)
	for _, c := range []string{"CLR-A", "CLR-B", "CLR-C"} {
		f.clearingAccount(t, c)
		// a debit-direction open balance of 10 per account
		f.manual(t, "2026-07", domain.NewDate(2026, 7, 1), dr(c, 10), cr("6000", 10))
	}
	var ids []string
	marks := map[string]bool{}
	after := ""
	for guard := 0; ; guard++ {
		if guard > 10 {
			t.Fatal("paging did not terminate")
		}
		p := f.aging(t, "2026-09-01", 1, after)
		marks[p.Watermark] = true
		if p.DeclaredTotals.RowCount != 3 || p.DeclaredTotals.Totals["USD"] != "30.00" {
			t.Fatalf("declared totals cover the whole population: %+v", p.DeclaredTotals)
		}
		for _, r := range p.Records {
			if r.Attributes["direction"] != "DEBIT" || r.Amount != "10.00" {
				t.Fatalf("record %+v", r)
			}
			ids = append(ids, r.RecordID)
		}
		if p.NextRecordID == "" {
			break
		}
		after = p.NextRecordID
	}
	if len(ids) != 3 || len(marks) != 1 || !sort.StringsAreSorted(ids) {
		t.Fatalf("ids %v, %d watermarks", ids, len(marks))
	}
}
