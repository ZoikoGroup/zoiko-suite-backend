//go:build integration

// Real-Postgres tests for the pay-slips / payroll-runs control populations.
// The embedded superuser bypasses RLS, so the tenant-isolation test proves the
// EXPLICIT tenant_id predicate, not the policy.
package store_test

import (
	"context"
	"encoding/json"
	"math/big"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/store"
)

const cpEntity = "le-int"

type cpRun struct {
	tenant, entity, payDate, status string
	shadow                          bool
	totalGross, totalNet            string
	employeeCount                   int
}

func insertCPRun(t *testing.T, pool *pgxpool.Pool, r cpRun) string {
	t.Helper()
	if r.entity == "" {
		r.entity = cpEntity
	}
	if r.status == "" {
		r.status = "COMPLETED"
	}
	if r.totalGross == "" {
		r.totalGross = "0"
	}
	if r.totalNet == "" {
		r.totalNet = "0"
	}
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO payroll_runs (run_id, tenant_id, legal_entity_id, run_number, pay_period_start, pay_period_end,
			pay_date, status, is_shadow_run, total_gross_pay, total_net_pay, employee_count, created_at, updated_at, correlation_id)
		VALUES ($1,$2,$3,$4,$5::date,$5::date,$5::date,$6,$7,$8::numeric,$9::numeric,$10,now(),now(),$11)`,
		id, r.tenant, r.entity, "PAY-"+id[:8], r.payDate, r.status, r.shadow, r.totalGross, r.totalNet, r.employeeCount, "corr-"+id)
	require.NoError(t, err)
	return id
}

// insertCPSlip stores a slip WITH a real-looking employee name, so the tests can
// prove it never appears in the response.
func insertCPSlip(t *testing.T, pool *pgxpool.Pool, tenant, runID, empNo, gross, tax, ben, net, currency string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO pay_slips (slip_id, tenant_id, run_id, employee_id, employee_number, employee_name,
			gross_pay, tax_withheld, benefits_deductions, net_pay, currency, effective_date, created_at)
		VALUES ($1,$2,$3,$4,$5,'SECRETNAME Personsurname',$6::numeric,$7::numeric,$8::numeric,$9::numeric,$10,
			(SELECT pay_date FROM payroll_runs WHERE run_id = $3), now())`,
		id, tenant, runID, uuid.NewString(), empNo, gross, tax, ben, net, currency)
	require.NoError(t, err)
	return id
}

func cpQuery(tenant, pop, measure string, period string, limit int, after string) domain.ControlPopulationQuery {
	q := domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: cpEntity, Population: pop, Measure: measure, Limit: limit, AfterRecordID: after}
	if period != "" {
		m, err := time.Parse("2006-01", period)
		if err != nil {
			panic(err)
		}
		q.PeriodStart = &m
	}
	return q
}

// drain pages through a whole population and returns all records plus the set of
// distinct watermarks seen.
func drain(t *testing.T, s *store.PgStore, ctx context.Context, q domain.ControlPopulationQuery) ([]domain.ControlRecord, map[string]bool, *domain.ControlPopulationPage) {
	t.Helper()
	var all []domain.ControlRecord
	marks := map[string]bool{}
	var first *domain.ControlPopulationPage
	for i := 0; i < 100; i++ {
		p, err := s.ControlPopulation(ctx, q)
		require.NoError(t, err)
		if first == nil {
			first = p
		}
		marks[p.Watermark] = true
		all = append(all, p.Records...)
		if p.NextCursor == "" {
			return all, marks, first
		}
		q.AfterRecordID = p.NextCursor
	}
	t.Fatal("paging did not terminate")
	return nil, nil, nil
}

func TestControlPopulation_PaySlips_PagingWatermarkTotalsMeasure(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := context.Background()

	run := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-09-25"})
	want := map[string]bool{}
	for i, empNo := range []string{"E1", "E2", "E3", "E4", "E5"} {
		cur := "USD"
		if i >= 3 {
			cur = "EUR"
		}
		id := insertCPSlip(t, pool, tenant, run, empNo, "1000.5000", "200.2500", "50.0000", "750.2500", cur)
		want[id] = true
	}

	all, marks, first := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPaySlips, "gross", "2026-09", 2, ""))
	require.Len(t, all, 5)
	assert.Len(t, marks, 1, "watermark must be identical on every page")
	assert.True(t, sort.SliceIsSorted(all, func(i, j int) bool { return all[i].RecordID < all[j].RecordID }))
	seen := map[string]bool{}
	for _, r := range all {
		assert.False(t, seen[r.RecordID], "record visited twice")
		seen[r.RecordID] = true
		assert.True(t, want[r.RecordID])
		assert.Equal(t, run, r.Reference)
		assert.Equal(t, "2026-09-25", r.Date)
		assert.Equal(t, "1000.5000", r.Amount)
		assert.Equal(t, map[string]string{
			"run_id": run, "run_status": "COMPLETED", "is_shadow_run": "false", "employee_number": r.Attributes["employee_number"],
			"gross_pay": "1000.5000", "tax_withheld": "200.2500", "benefits_deductions": "50.0000", "net_pay": "750.2500",
		}, r.Attributes)
	}
	assert.Len(t, seen, 5)

	// declared totals cover the WHOLE set, per currency, not the page.
	assert.EqualValues(t, 5, first.DeclaredTotals.RowCount)
	assert.Equal(t, map[string]string{"USD": "3001.5000", "EUR": "2001.0000"}, first.DeclaredTotals.Totals)
	assert.Regexp(t, `^5:[0-9a-f]{32}$`, first.Watermark)

	// net measure changes the amount, not the attributes, and not the watermark.
	allNet, netMarks, netFirst := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPaySlips, "net", "2026-09", 2, ""))
	require.Len(t, allNet, 5)
	assert.Equal(t, "750.2500", allNet[0].Amount)
	assert.Equal(t, "1000.5000", allNet[0].Attributes["gross_pay"])
	assert.Equal(t, "750.2500", allNet[0].Attributes["net_pay"])
	assert.Equal(t, map[string]string{"USD": "2250.7500", "EUR": "1500.5000"}, netFirst.DeclaredTotals.Totals)
	assert.Equal(t, marks, netMarks)

	// No employee name anywhere in the payload.
	b, err := json.Marshal(first)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "SECRETNAME")
	assert.NotContains(t, string(b), "Personsurname")
}

func TestControlPopulation_FlowPeriodIsExactMonth(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := context.Background()

	dates := map[string]string{
		"2026-08-31": "prev", "2026-09-01": "in1", "2026-09-30": "in2", "2026-10-01": "next", "2027-09-15": "nextyear",
	}
	runByLabel := map[string]string{}
	for d, label := range dates {
		id := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: d, totalGross: "100", totalNet: "80"})
		insertCPSlip(t, pool, tenant, id, "E-"+label, "100", "10", "10", "80", "USD")
		runByLabel[label] = id
	}

	slips, _, first := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPaySlips, "gross", "2026-09", 10, ""))
	assert.Len(t, slips, 2)
	assert.EqualValues(t, 2, first.DeclaredTotals.RowCount)
	got := []string{slips[0].Attributes["employee_number"], slips[1].Attributes["employee_number"]}
	assert.ElementsMatch(t, []string{"E-in1", "E-in2"}, got)

	runs, _, rfirst := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "gross", "2026-09", 10, ""))
	assert.Len(t, runs, 2)
	assert.Equal(t, map[string]string{"USD": "200.0000"}, rfirst.DeclaredTotals.Totals)
	ids := []string{runs[0].RecordID, runs[1].RecordID}
	assert.ElementsMatch(t, []string{runByLabel["in1"], runByLabel["in2"]}, ids)

	// December -> January edge and no filter.
	dec := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-12-31"})
	jan := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2027-01-01"})
	decRuns, _, _ := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "gross", "2026-12", 10, ""))
	require.Len(t, decRuns, 1)
	assert.Equal(t, dec, decRuns[0].RecordID)
	all, _, _ := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "gross", "", 3, ""))
	assert.Len(t, all, 7, "absent period_id means no date filter")
	_ = jan
}

func TestControlPopulation_ShadowAndNonCompletedAreReturnedAndMarked(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := context.Background()

	statuses := map[string]cpRun{
		"done":    {tenant: tenant, payDate: "2026-09-10", status: "COMPLETED"},
		"calc":    {tenant: tenant, payDate: "2026-09-10", status: "CALCULATED"},
		"blocked": {tenant: tenant, payDate: "2026-09-10", status: "BLOCKED"},
		"init":    {tenant: tenant, payDate: "2026-09-10", status: "INITIATED"},
		"shadow":  {tenant: tenant, payDate: "2026-09-10", status: "COMPLETED", shadow: true},
	}
	byID := map[string]string{}
	for label, r := range statuses {
		id := insertCPRun(t, pool, r)
		insertCPSlip(t, pool, tenant, id, "E-"+label, "10", "1", "1", "8", "USD")
		byID[id] = label
	}

	slips, _, _ := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPaySlips, "net", "2026-09", 2, ""))
	require.Len(t, slips, 5)
	for _, r := range slips {
		label := byID[r.Reference]
		assert.Equal(t, statuses[label].status, r.Attributes["run_status"])
		assert.Equal(t, statuses[label].shadow, r.Attributes["is_shadow_run"] == "true")
	}
	runs, _, _ := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "net", "2026-09", 2, ""))
	require.Len(t, runs, 5)
	for _, r := range runs {
		label := byID[r.RecordID]
		assert.Equal(t, statuses[label].status, r.Attributes["status"])
		assert.Equal(t, statuses[label].shadow, r.Attributes["is_shadow_run"] == "true")
	}
}

func TestControlPopulation_PayrollRuns_CurrencyRules(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := context.Background()

	single := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-09-05", totalGross: "300.0000", totalNet: "240.0000", employeeCount: 2})
	insertCPSlip(t, pool, tenant, single, "A", "100", "10", "10", "80", "GBP")
	insertCPSlip(t, pool, tenant, single, "B", "200", "20", "20", "160", "GBP")
	none := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-09-06", totalGross: "50", totalNet: "40"})
	mixed := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-09-07", totalGross: "70", totalNet: "60", employeeCount: 2})
	insertCPSlip(t, pool, tenant, mixed, "C", "30", "3", "3", "24", "USD")
	insertCPSlip(t, pool, tenant, mixed, "D", "40", "4", "4", "32", "EUR")

	runs, _, first := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "gross", "2026-09", 2, ""))
	require.Len(t, runs, 3)
	by := map[string]domain.ControlRecord{}
	for _, r := range runs {
		by[r.RecordID] = r
		assert.Equal(t, r.RecordID, r.Reference)
	}

	assert.Equal(t, "GBP", by[single].Currency)
	assert.Equal(t, "300.0000", by[single].Amount, "amount is the run's DECLARED total, not a sum of slips")
	assert.NotContains(t, by[single].Attributes, "currency_note")
	assert.Equal(t, "2", by[single].Attributes["employee_count"])
	assert.Equal(t, "COMPLETED", by[single].Attributes["status"])
	assert.Contains(t, by[single].Attributes, "snapshot_hash")

	assert.Equal(t, "XXX", by[none].Currency)
	assert.Equal(t, "no_slips", by[none].Attributes["currency_note"])
	assert.Equal(t, "XXX", by[mixed].Currency)
	assert.Equal(t, "mixed_currencies", by[mixed].Attributes["currency_note"])

	assert.Equal(t, map[string]string{"GBP": "300.0000", "XXX": "120.0000"}, first.DeclaredTotals.Totals)

	net, _, nfirst := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "net", "2026-09", 5, ""))
	require.Len(t, net, 3)
	assert.Equal(t, map[string]string{"GBP": "240.0000", "XXX": "100.0000"}, nfirst.DeclaredTotals.Totals)
}

func TestControlPopulation_TenantAndEntityIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	mine, other := uuid.NewString(), uuid.NewString()
	ctx := context.Background()

	r1 := insertCPRun(t, pool, cpRun{tenant: mine, payDate: "2026-09-05"})
	insertCPSlip(t, pool, mine, r1, "MINE", "10", "1", "1", "8", "USD")
	// Same entity id, other tenant.
	r2 := insertCPRun(t, pool, cpRun{tenant: other, payDate: "2026-09-05"})
	insertCPSlip(t, pool, other, r2, "THEIRS", "999", "1", "1", "8", "USD")
	// Same tenant, other entity.
	r3 := insertCPRun(t, pool, cpRun{tenant: mine, entity: "le-other", payDate: "2026-09-05"})
	insertCPSlip(t, pool, mine, r3, "OTHERENTITY", "777", "1", "1", "8", "USD")

	for _, pop := range []string{domain.PopulationPaySlips, domain.PopulationPayrollRuns} {
		recs, _, first := drain(t, s, ctx, cpQuery(mine, pop, "gross", "", 10, ""))
		require.Len(t, recs, 1, pop)
		assert.EqualValues(t, 1, first.DeclaredTotals.RowCount)
		b, _ := json.Marshal(first)
		assert.NotContains(t, string(b), "THEIRS")
		assert.NotContains(t, string(b), "OTHERENTITY")
		assert.NotContains(t, string(b), r2)
		assert.NotContains(t, string(b), r3)
	}
	// A slip of another tenant attached to my run must not colour my run's currency.
	insertCPSlip(t, pool, other, r1, "FOREIGN", "1", "0", "0", "1", "JPY")
	runs, _, _ := drain(t, s, ctx, cpQuery(mine, domain.PopulationPayrollRuns, "gross", "", 10, ""))
	require.Len(t, runs, 1)
	assert.Equal(t, "USD", runs[0].Currency)
}

func TestControlPopulation_StatusChangeChangesWatermark(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := context.Background()

	run := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-09-05", status: "CALCULATED", totalGross: "10", totalNet: "8"})
	insertCPSlip(t, pool, tenant, run, "E1", "10", "1", "1", "8", "USD")

	for _, pop := range []string{domain.PopulationPaySlips, domain.PopulationPayrollRuns} {
		before, err := s.ControlPopulation(ctx, cpQuery(tenant, pop, "gross", "", 10, ""))
		require.NoError(t, err)
		again, err := s.ControlPopulation(ctx, cpQuery(tenant, pop, "net", "", 10, ""))
		require.NoError(t, err)
		assert.Equal(t, before.Watermark, again.Watermark, "unchanged data, same watermark")

		_, err = pool.Exec(ctx, `UPDATE payroll_runs SET status = CASE status WHEN 'CALCULATED' THEN 'COMPLETED' ELSE 'CALCULATED' END WHERE run_id = $1`, run)
		require.NoError(t, err)
		after, err := s.ControlPopulation(ctx, cpQuery(tenant, pop, "gross", "", 10, ""))
		require.NoError(t, err)
		assert.NotEqual(t, before.Watermark, after.Watermark, pop+": a status change must change the watermark")
	}

	// An amount change also moves the watermark.
	before, err := s.ControlPopulation(ctx, cpQuery(tenant, domain.PopulationPaySlips, "gross", "", 10, ""))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE pay_slips SET tax_withheld = tax_withheld + 1, net_pay = net_pay - 1 WHERE run_id = $1`, run)
	require.NoError(t, err)
	after, err := s.ControlPopulation(ctx, cpQuery(tenant, domain.PopulationPaySlips, "gross", "", 10, ""))
	require.NoError(t, err)
	assert.NotEqual(t, before.Watermark, after.Watermark)
}

func TestControlPopulation_EmptyPopulation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	for _, pop := range []string{domain.PopulationPaySlips, domain.PopulationPayrollRuns} {
		p, err := s.ControlPopulation(context.Background(), cpQuery(uuid.NewString(), pop, "gross", "2026-09", 10, ""))
		require.NoError(t, err)
		assert.Empty(t, p.Records)
		assert.Equal(t, "", p.NextCursor)
		assert.EqualValues(t, 0, p.DeclaredTotals.RowCount)
		assert.Empty(t, p.DeclaredTotals.Totals)
		assert.Regexp(t, `^0:[0-9a-f]{32}$`, p.Watermark)
	}
}

// (a) a sub-cent value is returned exactly as stored, never rounded to 3500.00.
func TestControlPopulation_SubCentAmountIsReturnedExactly(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := context.Background()

	run := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-09-05", totalGross: "4000.0001", totalNet: "3500.0049"})
	insertCPSlip(t, pool, tenant, run, "E1", "4000.0001", "400.0050", "99.9902", "3500.0049", "USD")

	slips, _, _ := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPaySlips, "net", "", 10, ""))
	require.Len(t, slips, 1)
	assert.Equal(t, "3500.0049", slips[0].Amount)
	assert.Equal(t, "3500.0049", slips[0].Attributes["net_pay"])
	assert.Equal(t, "4000.0001", slips[0].Attributes["gross_pay"])
	assert.Equal(t, "400.0050", slips[0].Attributes["tax_withheld"])
	assert.Equal(t, "99.9902", slips[0].Attributes["benefits_deductions"])

	runs, _, _ := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "net", "", 10, ""))
	require.Len(t, runs, 1)
	assert.Equal(t, "3500.0049", runs[0].Amount)
	runsG, _, _ := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "gross", "", 10, ""))
	assert.Equal(t, "4000.0001", runsG[0].Amount)
}

// (b) a 0.0001 change moves the watermark.
func TestControlPopulation_SubCentChangeChangesWatermark(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := context.Background()

	run := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-09-05", totalGross: "100.0000", totalNet: "80.0000"})
	insertCPSlip(t, pool, tenant, run, "E1", "100.0000", "10.0000", "10.0000", "80.0000", "USD")

	slipBefore, err := s.ControlPopulation(ctx, cpQuery(tenant, domain.PopulationPaySlips, "gross", "", 10, ""))
	require.NoError(t, err)
	runBefore, err := s.ControlPopulation(ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "gross", "", 10, ""))
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE pay_slips SET net_pay = net_pay + 0.0001 WHERE run_id = $1`, run)
	require.NoError(t, err)
	slipAfter, err := s.ControlPopulation(ctx, cpQuery(tenant, domain.PopulationPaySlips, "gross", "", 10, ""))
	require.NoError(t, err)
	assert.NotEqual(t, slipBefore.Watermark, slipAfter.Watermark)

	_, err = pool.Exec(ctx, `UPDATE payroll_runs SET total_net_pay = total_net_pay + 0.0001 WHERE run_id = $1`, run)
	require.NoError(t, err)
	runAfter, err := s.ControlPopulation(ctx, cpQuery(tenant, domain.PopulationPayrollRuns, "gross", "", 10, ""))
	require.NoError(t, err)
	assert.NotEqual(t, runBefore.Watermark, runAfter.Watermark)
}

// (c) declared totals are the exact sum of the returned 4-decimal records.
func TestControlPopulation_DeclaredTotalsEqualExactSumOfRecords(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant := uuid.NewString()
	ctx := context.Background()

	run := insertCPRun(t, pool, cpRun{tenant: tenant, payDate: "2026-09-05"})
	for i, v := range []string{"1000.0001", "2000.0049", "0.0003", "12.3456", "7.0007"} {
		cur := "USD"
		if i == 4 {
			cur = "EUR"
		}
		insertCPSlip(t, pool, tenant, run, "E"+v, v, "0.0000", "0.0000", v, cur)
	}
	for _, measure := range []string{"gross", "net"} {
		recs, _, first := drain(t, s, ctx, cpQuery(tenant, domain.PopulationPaySlips, measure, "", 2, ""))
		require.Len(t, recs, 5)
		sums := map[string]*big.Rat{}
		for _, r := range recs {
			v, ok := new(big.Rat).SetString(r.Amount)
			require.True(t, ok, r.Amount)
			if sums[r.Currency] == nil {
				sums[r.Currency] = new(big.Rat)
			}
			sums[r.Currency].Add(sums[r.Currency], v)
		}
		require.Len(t, first.DeclaredTotals.Totals, len(sums))
		for cur, want := range sums {
			got, ok := new(big.Rat).SetString(first.DeclaredTotals.Totals[cur])
			require.True(t, ok)
			assert.Zero(t, want.Cmp(got), "%s %s: declared %s", measure, cur, first.DeclaredTotals.Totals[cur])
		}
		assert.Equal(t, "3012.3509", first.DeclaredTotals.Totals["USD"])
		assert.Equal(t, "7.0007", first.DeclaredTotals.Totals["EUR"])
	}
}
