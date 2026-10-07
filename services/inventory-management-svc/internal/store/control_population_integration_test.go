//go:build integration

// Real-Postgres tests for the stock-count-lines control population. They run
// against an embedded Postgres started by TestMain (no TEST_DATABASE_URL needed)
// and do not change how the other store tests in this package find their
// database. The embedded superuser bypasses RLS, so the isolation tests prove the
// EXPLICIT tenant_id predicate, not the policy.
//
// Run: go test -tags=integration -count=1 -timeout=400s ./internal/store/
package store_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/inventory-management-svc/internal/domain"
	"zoiko.io/inventory-management-svc/internal/store"
)

var cpPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(runWithEmbeddedPostgres(m)) }

func runWithEmbeddedPostgres(m *testing.M) int {
	root, err := os.MkdirTemp("", "inv-ctrlpop-pg-")
	if err != nil {
		fmt.Printf("temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(root)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("free port: %v\n", err)
		return 1
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()

	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.PostgresVersion("16.15.0")).Port(port).Database("inv_ctrlpop_test").
		Username("postgres").Password("postgres").
		RuntimePath(filepath.Join(root, "runtime")))
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		return 1
	}
	defer func() { _ = pg.Stop() }()

	ctx := context.Background()
	dsn := fmt.Sprintf("host=localhost port=%d dbname=inv_ctrlpop_test user=postgres password=postgres sslmode=disable", port)
	cpPool, err = pgxpool.New(ctx, dsn)
	if err == nil {
		for i := 0; i < 75; i++ {
			if err = cpPool.Ping(ctx); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err != nil {
		fmt.Printf("embedded postgres not ready: %v\n", err)
		return 1
	}
	defer cpPool.Close()

	files, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
	sort.Strings(files)
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr == nil {
			_, rerr = cpPool.Exec(ctx, string(b))
		}
		if rerr != nil {
			fmt.Printf("migration %s: %v\n", f, rerr)
			return 1
		}
	}
	return m.Run()
}

// ── seed helpers ─────────────────────────────────────────────────────────────

func cpMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func cpItem(t *testing.T, tenant, entity, uom string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := cpPool.Exec(context.Background(), `
		INSERT INTO inventory_items (item_id, tenant_id, legal_entity_id, sku, description, base_uom, status, created_at, created_by_principal_id)
		VALUES ($1,$2,$3,$4,'Widget',$5,'ACTIVE',now(),'seed')`, id, tenant, entity, "sku-"+id, uom)
	cpMust(t, err)
	return id
}

func cpLocation(t *testing.T, tenant, entity string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := cpPool.Exec(context.Background(), `
		INSERT INTO inventory_locations (location_id, tenant_id, legal_entity_id, location_code, location_type, status, created_at, created_by_principal_id)
		VALUES ($1,$2,$3,$4,'WAREHOUSE','ACTIVE',now(),'seed')`, id, tenant, entity, "loc-"+id)
	cpMust(t, err)
	return id
}

// cpMovement inserts a movement directly; src/dst may be "" (external).
func cpMovement(t *testing.T, tenant, entity, typ, status, item, src, dst, qty, businessDate string) string {
	t.Helper()
	id := uuid.NewString()
	nz := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err := cpPool.Exec(context.Background(), `
		INSERT INTO inventory_movements (movement_id, tenant_id, legal_entity_id, movement_type, status, item_id,
			source_location_id, destination_location_id, quantity, uom, source_reference, source_idempotency_key,
			business_date, fiscal_period, created_at, created_by_principal_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7::uuid,$8::uuid,$9::numeric,'EACH','ref',$11,$10::date,'2026-09',now(),'seed')`,
		id, tenant, entity, typ, status, item, nz(src), nz(dst), qty, businessDate, "idem-"+id)
	cpMust(t, err)
	return id
}

func cpCount(t *testing.T, tenant, entity, cutoff string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := cpPool.Exec(context.Background(), `
		INSERT INTO inventory_stock_counts (count_id, tenant_id, legal_entity_id, fiscal_period, status, cutoff_at, created_at, created_by_principal_id, frozen_at)
		VALUES ($1,$2,$3,'2026-09','COUNTING',$4::timestamptz,now(),'seed',$4::timestamptz)`, id, tenant, entity, cutoff)
	cpMust(t, err)
	return id
}

// cpLine inserts a count line; observed "" = not yet counted.
func cpLine(t *testing.T, tenant, count, item, loc, frozen, observed, status string) string {
	t.Helper()
	id := uuid.NewString()
	var obs any
	if observed != "" {
		obs = observed
	}
	_, err := cpPool.Exec(context.Background(), `
		INSERT INTO inventory_stock_count_lines (line_id, tenant_id, count_id, item_id, location_id, system_quantity, observed_quantity, status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8,now())`, id, tenant, count, item, loc, frozen, obs, status)
	cpMust(t, err)
	return id
}

func cpQuery(tenant, entity, count, side string, limit int, after string) domain.StockCountLinesQuery {
	return domain.StockCountLinesQuery{TenantID: tenant, LegalEntityID: entity, CountID: count, Side: side, Limit: limit, AfterRecordID: after}
}

func cpFetch(t *testing.T, q domain.StockCountLinesQuery) *domain.ControlPopulationPage {
	t.Helper()
	page, err := store.New(cpPool).ControlPopulationStockCountLines(context.Background(), q)
	if err != nil {
		t.Fatalf("ControlPopulationStockCountLines: %v", err)
	}
	return page
}

func cpAmounts(page *domain.ControlPopulationPage) map[string]string {
	out := map[string]string{}
	for _, r := range page.Records {
		out[r.RecordID] = r.Amount
	}
	return out
}

func newTenant() (tenant, entity string) {
	return "tenant-" + uuid.NewString(), uuid.NewString()
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestControlPop_BookQuantityAsOfCutoff_IgnoresLaterAndNonCommitted_AndFrozenIsIndependent(t *testing.T) {
	tenant, entity := newTenant()
	item := cpItem(t, tenant, entity, "KG")
	l1, l2 := cpLocation(t, tenant, entity), cpLocation(t, tenant, entity)

	cpMovement(t, tenant, entity, "RECEIPT", "COMMITTED", item, "", l1, "10", "2026-09-01")
	cpMovement(t, tenant, entity, "ISSUE", "COMMITTED", item, l1, "", "3", "2026-09-05")
	cpMovement(t, tenant, entity, "TRANSFER", "COMMITTED", item, l1, l2, "2", "2026-09-06")
	cpMovement(t, tenant, entity, "TRANSFER", "COMMITTED", item, l2, l1, "1", "2026-09-07")
	cpMovement(t, tenant, entity, "RECEIPT", "COMMITTED", item, "", l1, "4", "2026-09-15")  // on the cut-off day: included
	cpMovement(t, tenant, entity, "RECEIPT", "COMMITTED", item, "", l1, "7", "2026-09-16")  // after cut-off: ignored
	cpMovement(t, tenant, entity, "RECEIPT", "DRAFT", item, "", l1, "50", "2026-09-02")     // not committed: ignored
	cpMovement(t, tenant, entity, "RECEIPT", "VALIDATED", item, "", l1, "60", "2026-09-02") // not committed: ignored
	// Another tenant's committed movement on the very same (item, location) must not count.
	cpMovement(t, "other-"+tenant, entity, "RECEIPT", "COMMITTED", item, "", l1, "1000", "2026-09-01")

	count := cpCount(t, tenant, entity, "2026-09-15T23:30:00Z")
	// Frozen system_quantity is deliberately wrong (tampered): the book side must not use it.
	line1 := cpLine(t, tenant, count, item, l1, "999", "", "PENDING")
	line2 := cpLine(t, tenant, count, item, l2, "888", "", "PENDING")

	page := cpFetch(t, cpQuery(tenant, entity, count, "book", 100, ""))
	got := cpAmounts(page)
	// l1: 10 - 3 - 2 + 1 + 4 = 10 ; l2: 2 - 1 = 1
	if got[line1] != "10.0000" || got[line2] != "1.0000" {
		t.Fatalf("book amounts = %v", got)
	}
	var rec domain.ControlRecord
	for _, r := range page.Records {
		if r.RecordID == line1 {
			rec = r
		}
	}
	if rec.Attributes["frozen_system_quantity"] != "999.0000" {
		t.Fatalf("frozen_system_quantity = %q", rec.Attributes["frozen_system_quantity"])
	}
	if rec.Currency != "XXX" || rec.Date != "2026-09-15" || rec.Reference != count+"|"+item+"|"+l1 {
		t.Fatalf("record shape: %+v", rec)
	}
	for k, want := range map[string]string{
		"uom": "KG", "item_id": item, "location_id": l1, "count_status": "COUNTING", "line_status": "PENDING",
		"variance_approved": "false", "adjustment_movement_id": "",
	} {
		if rec.Attributes[k] != want {
			t.Fatalf("attribute %s = %q, want %q", k, rec.Attributes[k], want)
		}
	}
	if page.DeclaredTotals.RowCount != 2 || page.DeclaredTotals.Totals["XXX"] != "11.0000" {
		t.Fatalf("declared totals = %+v", page.DeclaredTotals)
	}
}

func TestControlPop_PhysicalSideOmitsUnobservedLines(t *testing.T) {
	tenant, entity := newTenant()
	item := cpItem(t, tenant, entity, "EACH")
	l1, l2, l3 := cpLocation(t, tenant, entity), cpLocation(t, tenant, entity), cpLocation(t, tenant, entity)
	count := cpCount(t, tenant, entity, "2026-09-15T00:00:00Z")
	a := cpLine(t, tenant, count, item, l1, "5", "4.5", "COUNTED")
	b := cpLine(t, tenant, count, item, l2, "5", "", "PENDING")
	c := cpLine(t, tenant, count, item, l3, "5", "0", "COUNTED") // a counted zero is an observation

	phys := cpFetch(t, cpQuery(tenant, entity, count, "physical", 100, ""))
	got := cpAmounts(phys)
	if len(got) != 2 || got[a] != "4.5000" || got[c] != "0.0000" {
		t.Fatalf("physical = %v", got)
	}
	if _, ok := got[b]; ok {
		t.Fatal("unobserved line must be omitted from the physical side")
	}
	if phys.DeclaredTotals.RowCount != 2 || phys.DeclaredTotals.Totals["XXX"] != "4.5000" {
		t.Fatalf("physical declared totals = %+v", phys.DeclaredTotals)
	}
	book := cpFetch(t, cpQuery(tenant, entity, count, "book", 100, ""))
	if len(book.Records) != 3 || book.DeclaredTotals.RowCount != 3 {
		t.Fatalf("book side must include every line, got %d", len(book.Records))
	}
}

func TestControlPop_Paging_VisitsEveryRowOnce_IdenticalWatermark_TotalsTie(t *testing.T) {
	tenant, entity := newTenant()
	item := cpItem(t, tenant, entity, "EACH")
	count := cpCount(t, tenant, entity, "2026-09-15T00:00:00Z")
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		loc := cpLocation(t, tenant, entity)
		cpMovement(t, tenant, entity, "RECEIPT", "COMMITTED", item, "", loc, fmt.Sprintf("%d.25", i+1), "2026-09-01")
		want[cpLine(t, tenant, count, item, loc, "0", fmt.Sprintf("%d.5", i+1), "COUNTED")] = true
	}
	for _, side := range []string{"book", "physical"} {
		seen := map[string]bool{}
		var prev, watermark string
		sum := new(big.Rat)
		after := ""
		pages := 0
		for {
			page := cpFetch(t, cpQuery(tenant, entity, count, side, 2, after))
			pages++
			if watermark == "" {
				watermark = page.Watermark
			} else if page.Watermark != watermark {
				t.Fatalf("%s: watermark changed between pages: %q vs %q", side, watermark, page.Watermark)
			}
			if page.DeclaredTotals.RowCount != 5 {
				t.Fatalf("%s: declared row_count = %d", side, page.DeclaredTotals.RowCount)
			}
			for _, r := range page.Records {
				if seen[r.RecordID] {
					t.Fatalf("%s: row %s visited twice", side, r.RecordID)
				}
				if prev != "" && r.RecordID <= prev {
					t.Fatalf("%s: not ascending by record_id", side)
				}
				prev = r.RecordID
				seen[r.RecordID] = true
				v, ok := new(big.Rat).SetString(r.Amount)
				if !ok {
					t.Fatalf("bad amount %q", r.Amount)
				}
				sum.Add(sum, v)
			}
			total, _ := new(big.Rat).SetString(page.DeclaredTotals.Totals["XXX"])
			if page.NextCursor == "" {
				if len(page.Records) == 0 {
					t.Fatalf("%s: empty last page", side)
				}
				if total.Cmp(sum) != 0 {
					t.Fatalf("%s: declared total %s != returned sum %s", side, total.RatString(), sum.RatString())
				}
				break
			}
			after = page.NextCursor
		}
		if pages != 3 || len(seen) != 5 {
			t.Fatalf("%s: pages=%d seen=%d", side, pages, len(seen))
		}
		for id := range want {
			if !seen[id] {
				t.Fatalf("%s: row %s never visited", side, id)
			}
		}
	}
}

func TestControlPop_TenantAndEntityIsolation(t *testing.T) {
	tenant, entity := newTenant()
	item := cpItem(t, tenant, entity, "EACH")
	loc := cpLocation(t, tenant, entity)
	count := cpCount(t, tenant, entity, "2026-09-15T00:00:00Z")
	cpLine(t, tenant, count, item, loc, "1", "1", "COUNTED")

	s := store.New(cpPool)
	ctx := context.Background()

	// Another tenant asking for this tenant's count -> not found (even with the right entity).
	if _, err := s.ControlPopulationStockCountLines(ctx, cpQuery("other-"+tenant, entity, count, "book", 10, "")); !errors.Is(err, domain.ErrStockCountNotFound) {
		t.Fatalf("cross-tenant: got %v", err)
	}
	// Same tenant, different legal entity -> not found.
	if _, err := s.ControlPopulationStockCountLines(ctx, cpQuery(tenant, uuid.NewString(), count, "physical", 10, "")); !errors.Is(err, domain.ErrStockCountNotFound) {
		t.Fatalf("cross-entity: got %v", err)
	}
	// Unknown count -> not found.
	if _, err := s.ControlPopulationStockCountLines(ctx, cpQuery(tenant, entity, uuid.NewString(), "book", 10, "")); !errors.Is(err, domain.ErrStockCountNotFound) {
		t.Fatalf("unknown count: got %v", err)
	}
	// The owner still sees it.
	if page := cpFetch(t, cpQuery(tenant, entity, count, "physical", 10, "")); len(page.Records) != 1 {
		t.Fatalf("owner records = %d", len(page.Records))
	}
}

func TestControlPop_CommittedMovementBetweenExtractionsChangesBookWatermarkOnly(t *testing.T) {
	tenant, entity := newTenant()
	item := cpItem(t, tenant, entity, "EACH")
	loc := cpLocation(t, tenant, entity)
	cpMovement(t, tenant, entity, "RECEIPT", "COMMITTED", item, "", loc, "10", "2026-09-01")
	count := cpCount(t, tenant, entity, "2026-09-15T00:00:00Z")
	cpLine(t, tenant, count, item, loc, "10", "10", "COUNTED")

	book1 := cpFetch(t, cpQuery(tenant, entity, count, "book", 10, ""))
	phys1 := cpFetch(t, cpQuery(tenant, entity, count, "physical", 10, ""))
	if again := cpFetch(t, cpQuery(tenant, entity, count, "book", 10, "")); again.Watermark != book1.Watermark {
		t.Fatal("watermark must be stable while nothing changes")
	}

	// A non-committed movement, and a later-dated one, do not change the dataset.
	cpMovement(t, tenant, entity, "RECEIPT", "DRAFT", item, "", loc, "5", "2026-09-02")
	cpMovement(t, tenant, entity, "RECEIPT", "COMMITTED", item, "", loc, "5", "2026-09-30")
	if same := cpFetch(t, cpQuery(tenant, entity, count, "book", 10, "")); same.Watermark != book1.Watermark {
		t.Fatal("watermark must not change for movements outside the book population")
	}

	// A committed movement inside the cut-off changes the book quantity and watermark.
	cpMovement(t, tenant, entity, "ISSUE", "COMMITTED", item, loc, "", "2", "2026-09-10")
	book2 := cpFetch(t, cpQuery(tenant, entity, count, "book", 10, ""))
	if book2.Watermark == book1.Watermark {
		t.Fatal("book watermark must change when a committed movement lands inside the cut-off")
	}
	if book2.Records[0].Amount != "8.0000" || book2.DeclaredTotals.Totals["XXX"] != "8.0000" {
		t.Fatalf("book after issue = %+v", book2.Records[0])
	}
	if phys2 := cpFetch(t, cpQuery(tenant, entity, count, "physical", 10, "")); phys2.Watermark != phys1.Watermark {
		t.Fatal("physical watermark must not depend on movements")
	}
}
