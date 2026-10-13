//go:build integration

package arctl

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/store"
)

type popEnv struct {
	*env
	legacy, union PopulationFunc
	newPath       PopulationFunc
	n1Item        kit.OpenItemRef
}

func newPopEnv(t *testing.T) *popEnv {
	e := newEnv(t)
	p := &popEnv{env: e}
	p.legacy = store.New(pool, zap.NewNop()).ControlPopulation
	p.newPath = NewPathInvoices(e.db)
	p.union = Union(p.legacy, p.newPath)
	// legacy 10 + 20 + 30; new path: N1 100 (40 cash applied), N2 200, N3 50 (fully applied), N4 70 with no open item
	e.legacy("10.00", "ISSUED")
	e.legacy("20.00", "SENT")
	e.legacy("30.00", "ISSUED")
	n1 := e.invoice("100.00", "USD", true)
	e.invoice("200.00", "USD", true)
	n3 := e.invoice("50.00", "USD", true)
	e.invoice("70.00", "USD", false)
	i1, err := e.ar07m.GetBySourceDocument(context.Background(), e.tenant, n1)
	require.NoError(t, err)
	p.n1Item = kit.OpenItemRef{OpenItemID: i1.OpenItemID}
	e.adjust(i1.OpenItemID, kit.AdjCashApplication, "40.00")
	i3, err := e.ar07m.GetBySourceDocument(context.Background(), e.tenant, n3)
	require.NoError(t, err)
	e.adjust(i3.OpenItemID, kit.AdjCashApplication, "50.00")
	return p
}

func (p *popEnv) pageAll(fn PopulationFunc, tenant, entity string, cutoff *time.Time, limit int) (recs []domain.ControlRecord, wms map[string]bool, last *domain.ControlPopulationPage) {
	p.t.Helper()
	wms = map[string]bool{}
	after := ""
	for i := 0; ; i++ {
		pg, err := fn(context.Background(), domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, CutOff: cutoff, Limit: limit, AfterRecordID: after})
		require.NoError(p.t, err)
		wms[pg.Watermark] = true
		recs = append(recs, pg.Records...)
		last = pg
		if pg.NextCursor == "" {
			return
		}
		require.NotEqual(p.t, after, pg.NextCursor)
		require.Less(p.t, i, 500)
		after = pg.NextCursor
	}
}

// contract asserts the control-population contract on one population: complete keyset paging in record_id order with no
// repeats, one watermark across all pages, declared row count and per-currency totals equal to what was paged.
func (p *popEnv) contract(name string, fn PopulationFunc, tenant, entity string, cutoff *time.Time, wantRows int, wantTotals map[string]string) {
	p.t.Helper()
	for _, limit := range []int{1, 2, 3, 1000} {
		recs, wms, last := p.pageAll(fn, tenant, entity, cutoff, limit)
		require.Len(p.t, recs, wantRows, "%s limit=%d", name, limit)
		require.Len(p.t, wms, 1, "%s: the watermark is stable across pages", name)
		require.EqualValues(p.t, wantRows, last.DeclaredTotals.RowCount, name)
		require.True(p.t, sort.SliceIsSorted(recs, func(i, j int) bool { return recs[i].RecordID < recs[j].RecordID }), name)
		seen := map[string]bool{}
		sums := map[string]kit.Amount{}
		for _, r := range recs {
			require.False(p.t, seen[r.RecordID], name)
			seen[r.RecordID] = true
			a, err := kit.ParseAmount(r.Amount)
			require.NoError(p.t, err, "%s: amounts are decimal strings", name)
			sums[r.Currency] += a
			require.NotEmpty(p.t, r.Reference)
			require.NotEmpty(p.t, r.Date)
			require.NotNil(p.t, r.Attributes)
		}
		require.Len(p.t, last.DeclaredTotals.Totals, len(sums), name)
		for cur, s := range sums {
			require.Equal(p.t, s.String(), kit.MustAmount(last.DeclaredTotals.Totals[cur]).String(), "%s: declared total %s equals the sum of the records", name, cur)
		}
		for cur, want := range wantTotals {
			require.Equal(p.t, kit.MustAmount(want).String(), kit.MustAmount(last.DeclaredTotals.Totals[cur]).String(), "%s %s", name, cur)
		}
	}
}

func TestPopulation_OpenInvoicesUnionLegacyAndNewPath(t *testing.T) {
	p := newPopEnv(t)
	// current: legacy 60 + N1 60 (100-40) + N2 200 + N3 0 + N4 70 (no open item: gross) = 390
	p.contract("open-invoices", p.union, p.tenant, p.entity, nil, 7, map[string]string{"USD": "390.00"})
	// the legacy half is unchanged: served alone it is the legacy page
	p.contract("legacy half", p.legacy, p.tenant, p.entity, nil, 3, map[string]string{"USD": "60.00"})
	recs, _, _ := p.pageAll(p.union, p.tenant, p.entity, nil, 100)
	byRef := map[string]domain.ControlRecord{}
	for _, r := range recs {
		byRef[r.Attributes["invoice_amount"]+"/"+r.Attributes["origin"]] = r
	}
	require.Equal(t, "60.0000", byRef["100.0000/AR04"].Amount, "partially paid new-path invoice is reported at the AR-07 residual, not its gross")
	require.Equal(t, "PARTIALLY_SETTLED", byRef["100.0000/AR04"].Attributes["status"])
	require.Equal(t, "0.0000", byRef["50.0000/AR04"].Amount, "fully applied invoice is outstanding 0")
	require.Equal(t, "SETTLED", byRef["50.0000/AR04"].Attributes["status"])
	require.Equal(t, "missing", byRef["70.0000/AR04"].Attributes["open_item"], "an issued invoice with no open item is visible")
	require.Equal(t, "70.0000", byRef["70.0000/AR04"].Amount)

	// as of the end of September the cash applications (recorded in October) had not happened: 60 + 100 + 200 + 50 + 70
	cut := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	p.contract("open-invoices@09-30", p.union, p.tenant, p.entity, &cut, 7, map[string]string{"USD": "480.00"})
	// before anything was issued
	early := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	p.contract("open-invoices@08-31", p.union, p.tenant, p.entity, &early, 0, nil)

	// consistent with AR-07: the new-path invoices that have an open item sum to the open_items population
	items, _, itemsLast := p.pageAll(p.ar07m.ControlPopulation, p.tenant, p.entity, nil, 100)
	require.Len(t, items, 2) // N1 and N2: a settled item is not an open item
	require.Equal(t, "260.0000", itemsLast.DeclaredTotals.Totals["USD"])
	var fromInvoices kit.Amount
	for _, r := range recs {
		if r.Attributes["origin"] == "AR04" && r.Attributes["open_item"] == "present" {
			fromInvoices += kit.MustAmount(r.Amount)
		}
	}
	require.Equal(t, itemsLast.DeclaredTotals.Totals["USD"], fromInvoices.String())
	p.contract("open_items", p.ar07m.ControlPopulation, p.tenant, p.entity, nil, 2, map[string]string{"USD": "260.00"})

	// the watermark moves when the population moves
	_, w0, _ := p.pageAll(p.union, p.tenant, p.entity, nil, 100)
	p.adjust(p.n1Item.OpenItemID, kit.AdjCashApplication, "10.00")
	_, w1, _ := p.pageAll(p.union, p.tenant, p.entity, nil, 100)
	require.NotEqual(t, w0, w1)

	// isolation: another tenant and another entity see an empty population
	for _, tc := range []struct{ tenant, entity string }{{uuid.NewString(), p.entity}, {p.tenant, uuid.NewString()}} {
		recs, _, last := p.pageAll(p.union, tc.tenant, tc.entity, nil, 10)
		require.Empty(t, recs)
		require.EqualValues(t, 0, last.DeclaredTotals.RowCount)
	}
}

// Legacy-only tenants get the legacy page byte for byte through the union.
func TestPopulation_LegacyOnlyTenantUnchangedByUnion(t *testing.T) {
	e := newEnv(t)
	e.legacy("10.00", "ISSUED")
	e.legacy("20.50", "SENT")
	legacy := store.New(pool, zap.NewNop()).ControlPopulation
	q := domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, Limit: 100}
	want, err := legacy(context.Background(), q)
	require.NoError(t, err)
	got, err := Union(legacy, NewPathInvoices(e.db))(context.Background(), q)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, "30.50", got.DeclaredTotals.Totals["USD"])
}

func TestPopulation_CreditDebitNotes(t *testing.T) {
	p := newPopEnv(t)
	p.note("CREDIT_NOTE", "5.00", "USD")
	p.note("DEBIT_NOTE", "8.00", "USD")
	p.note("DEBIT_NOTE", "1.50", "USD")
	fn := CreditDebitNotes(p.db)
	p.contract("credit-debit-notes", fn, p.tenant, p.entity, nil, 3, map[string]string{"USD": "4.50"}) // -5 + 8 + 1.5
	recs, _, _ := p.pageAll(fn, p.tenant, p.entity, nil, 10)
	kinds := map[string]int{}
	for _, r := range recs {
		kinds[r.Attributes["kind"]]++
		if r.Attributes["kind"] == "CREDIT_NOTE" {
			require.Equal(t, "-5.0000", r.Amount)
		}
	}
	require.Equal(t, map[string]int{"CREDIT_NOTE": 1, "DEBIT_NOTE": 2}, kinds)
	before := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	p.contract("credit-debit-notes@09-01", fn, p.tenant, p.entity, &before, 0, nil)
	p.contract("isolation", fn, uuid.NewString(), p.entity, nil, 0, nil)
}

func TestPopulation_CashReceipts(t *testing.T) {
	p := newPopEnv(t)
	p.receipt("100.00", "0")
	p.receipt("250.00", "100")
	fn := CashReceipts(p.db)
	p.contract("cash-receipts", fn, p.tenant, p.entity, nil, 2, map[string]string{"USD": "350.00"})
	recs, _, _ := p.pageAll(fn, p.tenant, p.entity, nil, 10)
	for _, r := range recs {
		require.Contains(t, []string{"UNAPPLIED", "PARTIALLY_APPLIED"}, r.Attributes["status"])
		require.Contains(t, r.Attributes, "unapplied_amount")
	}
	before := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	p.contract("cash-receipts@09-01", fn, p.tenant, p.entity, &before, 0, nil)
	p.contract("isolation", fn, p.tenant, uuid.NewString(), nil, 0, nil)
}

// The 200000-record cap: above it the provider answers ErrPopulationTooLarge (HTTP 422) rather than a truncated page.
func TestPopulation_CapExceeded(t *testing.T) {
	e := newEnv(t)
	_, err := pool.Exec(context.Background(), `INSERT INTO ar08_receipts (receipt_id, tenant_id, legal_entity_id, bank_transaction_id, settlement_ref, currency, amount, applied_amount,
		unapplied_amount, value_date, state, registered_by)
		SELECT gen_random_uuid(), $1::uuid, $2::uuid, gen_random_uuid(), 'S' || g, 'USD', 1, 0, 1, '2026-09-15', 'UNAPPLIED', 'bulk' FROM generate_series(1, 200001) g`, e.tenant, e.entity)
	require.NoError(t, err)
	_, err = CashReceipts(e.db)(context.Background(), domain.ControlPopulationQuery{TenantID: e.tenant, LegalEntityID: e.entity, Limit: 10})
	require.ErrorIs(t, err, domain.ErrPopulationTooLarge)
}
