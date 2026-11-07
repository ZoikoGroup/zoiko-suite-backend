package store_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/asset-management-svc/internal/domain"
	svcmiddleware "zoiko.io/asset-management-svc/internal/middleware"
	"zoiko.io/asset-management-svc/internal/store"
)

// FIN-CTRL-011 population against a real Postgres.

type depSeed struct {
	t      *testing.T
	pool   *pgxpool.Pool
	s      *store.PgStore
	tenant string
	entity string
	ctx    context.Context
}

func newDepSeed(t *testing.T) *depSeed {
	pool := openTestPool(t)
	tenant := uuid.NewString()
	return &depSeed{t: t, pool: pool, s: store.New(pool), tenant: tenant, entity: "le-dep-1",
		ctx: svcmiddleware.WithTenant(context.Background(), tenant)}
}

func (d *depSeed) exec(sql string, args ...any) {
	d.t.Helper()
	if _, err := d.pool.Exec(context.Background(), sql, args...); err != nil {
		d.t.Fatalf("seed: %v\n%s", err, sql)
	}
}

// asset registers an asset of a category with one current schedule, returning the asset and schedule version ids.
func (d *depSeed) asset(tenant, entity, category string) (assetID, scheduleVersionID string) {
	d.t.Helper()
	a := newTestAsset(tenant, entity)
	a.AssetCategory = category
	if err := d.s.CreateAsset(svcmiddleware.WithTenant(context.Background(), tenant), a); err != nil {
		d.t.Fatalf("CreateAsset: %v", err)
	}
	sv := uuid.NewString()
	d.exec(`INSERT INTO depreciation_schedules (schedule_version_id, schedule_id, version, tenant_id, legal_entity_id, asset_id, book_id,
		cost_basis, useful_life_months, in_service_date, status, created_at, created_by_principal_id)
		VALUES ($1, $2, 1, $3, $4, $5, 'book-1', 12000, 12, '2026-01-01', 'ACTIVE', now(), 'preparer-1')`,
		sv, uuid.NewString(), tenant, entity, a.AssetID)
	return a.AssetID, sv
}

func (d *depSeed) run(tenant, entity, period, status string) string {
	d.t.Helper()
	id := uuid.NewString()
	d.exec(`INSERT INTO depreciation_runs (run_id, tenant_id, legal_entity_id, fiscal_period, depreciation_expense_account_code,
		accumulated_depreciation_account_code, status, created_at, created_by_principal_id)
		VALUES ($1, $2, $3, $4, '6400', '1590', $5, now(), 'preparer-1')`, id, tenant, entity, period, status)
	return id
}

func (d *depSeed) line(tenant, runID, assetID, scheduleVersionID, amount string) {
	d.t.Helper()
	d.exec(`INSERT INTO depreciation_lines (line_id, tenant_id, run_id, schedule_version_id, asset_id, book_id, period_amount,
		accumulated_depreciation_after, created_at) VALUES ($1, $2, $3, $4, $5, 'book-1', $6::numeric, $6::numeric, now())`,
		uuid.NewString(), tenant, runID, scheduleVersionID, assetID, amount)
}

func (d *depSeed) query(tenant, entity, currency, period string, limit int, after string) *domain.ControlPopulationPage {
	d.t.Helper()
	p, err := d.s.QueryDepreciationRunPostings(svcmiddleware.WithTenant(context.Background(), tenant), tenant,
		domain.DepreciationRunPostingsQuery{LegalEntityID: entity, Currency: currency, PeriodID: period, Limit: limit, AfterRecordID: after})
	if err != nil {
		d.t.Fatalf("QueryDepreciationRunPostings: %v", err)
	}
	return p
}

func TestDepreciationRunPostings_OnePerLiveRunAndCategoryWithTheKernelReference(t *testing.T) {
	d := newDepSeed(t)
	itA, itAS := d.asset(d.tenant, d.entity, "IT")
	itB, itBS := d.asset(d.tenant, d.entity, "IT")
	veh, vehS := d.asset(d.tenant, d.entity, "Vehicles")
	zero, zeroS := d.asset(d.tenant, d.entity, "Zero")

	// R1 emitted: IT = 600.00 + 400.00, Vehicles = 250.50
	r1 := d.run(d.tenant, d.entity, "2026-01", "ACCOUNTING_EVENT_EMITTED")
	d.line(d.tenant, r1, itA, itAS, "600.00")
	d.line(d.tenant, r1, itB, itBS, "400.00")
	d.line(d.tenant, r1, veh, vehS, "250.50")
	// R2 approved but never emitted: still in the population, so the ledger side shows it missing
	r2 := d.run(d.tenant, d.entity, "2026-02", "APPROVED")
	d.line(d.tenant, r2, itA, itAS, "600.00")
	// not due to post yet, and reversed: never in the population
	for i, st := range []string{"DRAFT", "POPULATION_FROZEN", "VALIDATED", "SUPERSEDED"} {
		r := d.run(d.tenant, d.entity, "2026-0"+string(rune('3'+i)), st)
		d.line(d.tenant, r, itA, itAS, "999.00")
	}
	// a category that depreciated to zero is not posted by the kernel path, so it is not a record
	r7 := d.run(d.tenant, d.entity, "2026-07", "ACCOUNTING_EVENT_EMITTED")
	d.line(d.tenant, r7, zero, zeroS, "0.00")
	d.line(d.tenant, r7, itA, itAS, "5.00")
	// a period that is not YYYY-MM keeps its run (dated by creation) rather than dropping it
	r8 := d.run(d.tenant, d.entity, "FY26-Q4", "ACCOUNTING_EVENT_EMITTED")
	d.line(d.tenant, r8, veh, vehS, "7.25")
	// another entity and another tenant are never in scope
	otherEntity := d.run(d.tenant, "le-other", "2026-01", "ACCOUNTING_EVENT_EMITTED")
	oa, oas := d.asset(d.tenant, "le-other", "IT")
	d.line(d.tenant, otherEntity, oa, oas, "123.00")
	otherTenant := uuid.NewString()
	ta, tas := d.asset(otherTenant, d.entity, "IT")
	tr := d.run(otherTenant, d.entity, "2026-01", "ACCOUNTING_EVENT_EMITTED")
	d.line(otherTenant, tr, ta, tas, "321.00")

	p := d.query(d.tenant, d.entity, "GBP", "", 100, "")
	by := map[string]domain.ControlPopulationRecord{}
	for _, r := range p.Records {
		by[r.RecordID] = r
	}
	if len(p.Records) != 5 || p.DeclaredTotals.RowCount != 5 {
		t.Fatalf("expected R1:IT, R1:Vehicles, R2:IT, R7:IT and R8:Vehicles, got %d: %+v", len(p.Records), p.Records)
	}
	it := by[r1+":IT"]
	if it.Amount != "1000.00" || it.Reference != "ast-dep:"+r1+":IT" || it.Currency != "GBP" || it.Date != "2026-01-31" ||
		it.Attributes["run_status"] != "ACCOUNTING_EVENT_EMITTED" || it.Attributes["line_count"] != "2" || it.Attributes["asset_category"] != "IT" {
		t.Fatalf("R1 IT record %+v", it)
	}
	if v := by[r1+":Vehicles"]; v.Amount != "250.50" || v.Reference != "ast-dep:"+r1+":Vehicles" {
		t.Fatalf("R1 Vehicles record %+v", v)
	}
	if a := by[r2+":IT"]; a.Amount != "600.00" || a.Attributes["run_status"] != "APPROVED" || a.Date != "2026-02-28" {
		t.Fatalf("an approved, unemitted run is reported: %+v", a)
	}
	if _, ok := by[r7+":Zero"]; ok {
		t.Fatal("a zero category is not posted, so it is not a record")
	}
	if z := by[r7+":IT"]; z.Amount != "5.00" {
		t.Fatalf("R7 IT %+v", z)
	}
	if w := by[r8+":Vehicles"]; w.Amount != "7.25" || len(w.Date) != 10 {
		t.Fatalf("an unusual period name still yields a dated record: %+v", w)
	}
	if p.DeclaredTotals.Totals["GBP"] != "1862.75" || !strings.HasPrefix(p.Watermark, "am1:n=5;md5=") {
		t.Fatalf("totals %+v watermark %q", p.DeclaredTotals, p.Watermark)
	}

	// The period cut-off keeps runs of that fiscal period or earlier (YYYY-MM text order).
	jan := d.query(d.tenant, d.entity, "GBP", "2026-01", 100, "")
	if len(jan.Records) != 2 {
		t.Fatalf("period 2026-01 holds only run R1's two categories: %+v", jan.Records)
	}
	if feb := d.query(d.tenant, d.entity, "GBP", "2026-02", 100, ""); len(feb.Records) != 3 {
		t.Fatalf("period 2026-02: %+v", feb.Records)
	}

	// Tenant and entity isolation.
	if o := d.query(otherTenant, d.entity, "GBP", "", 100, ""); len(o.Records) != 1 || o.Records[0].Amount != "321.00" {
		t.Fatalf("tenant isolation: %+v", o.Records)
	}
	if o := d.query(uuid.NewString(), d.entity, "GBP", "", 100, ""); len(o.Records) != 0 {
		t.Fatalf("an unknown tenant sees nothing: %+v", o.Records)
	}
	if o := d.query(d.tenant, "le-none", "GBP", "", 100, ""); len(o.Records) != 0 {
		t.Fatalf("an unknown entity sees nothing: %+v", o.Records)
	}

	// Keyset paging is complete, ordered, repeat-free and under one watermark.
	for _, limit := range []int{1, 2, 100} {
		var ids []string
		marks := map[string]bool{}
		after := ""
		for guard := 0; ; guard++ {
			if guard > 10 {
				t.Fatal("paging did not terminate")
			}
			pg := d.query(d.tenant, d.entity, "GBP", "", limit, after)
			marks[pg.Watermark] = true
			for _, r := range pg.Records {
				ids = append(ids, r.RecordID)
			}
			if pg.NextRecordID == "" {
				break
			}
			after = pg.NextRecordID
		}
		if len(ids) != 5 || len(marks) != 1 || !sort.StringsAreSorted(ids) {
			t.Fatalf("limit=%d: %v, %d watermarks", limit, ids, len(marks))
		}
	}

	// The watermark moves when a run is approved into the population.
	before := p.Watermark
	r9 := d.run(d.tenant, d.entity, "2026-09", "APPROVED")
	d.line(d.tenant, r9, itA, itAS, "1.00")
	if after := d.query(d.tenant, d.entity, "GBP", "", 100, ""); after.Watermark == before || len(after.Records) != 6 {
		t.Fatalf("watermark must move: %q vs %q", before, after.Watermark)
	}
}
