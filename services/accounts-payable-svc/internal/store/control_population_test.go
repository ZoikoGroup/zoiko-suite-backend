package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	"zoiko.io/accounts-payable-svc/internal/store"
)

// seedInvoice inserts a header directly (not via CreateInvoice, which stamps
// created_at with now()) so a test controls the created_at the cut-off is
// measured against. Same TEST_DATABASE_URL harness as the rest of this package.
func seedInvoice(t *testing.T, pool *pgxpool.Pool, tenant, entity, number, amount, currency, status string, createdAt time.Time) string {
	t.Helper()
	id := uuid.NewString()
	scoped(t, pool, tenant, func(tx pgx.Tx) {
		_, err := tx.Exec(context.Background(), `
			INSERT INTO vendor_invoices (invoice_id, tenant_id, legal_entity_id, vendor_id, invoice_number,
				amount, currency_code, due_date, status, created_by_principal_id, correlation_id, created_at,
				invoice_date, supply_date, net_amount, tax_amount,
				source_hash, source_channel, invoice_number_normalized)
			VALUES ($1,$2,$3,'vendor-1',$4,$5::numeric,$6,'2026-12-31',$7,'seed',$8,$9,'2026-01-01','2026-01-01',$5::numeric,0,
				repeat('0',64),'seed',lower($4::varchar))`,
			id, tenant, entity, number, amount, currency, status, "c-"+id, createdAt)
		if err != nil {
			t.Fatalf("seed invoice: %v", err)
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	})
	return id
}

func utc(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }

func TestControlPopulation_PagingWatermarkAndTotals(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenant, entity := uuid.NewString(), uuid.NewString()
	seedInvoice(t, pool, tenant, entity, "A-1", "100.10", "USD", "RECEIVED", utc(2026, 1, 5, 10))
	seedInvoice(t, pool, tenant, entity, "A-2", "200.20", "USD", "APPROVED", utc(2026, 1, 6, 10))
	seedInvoice(t, pool, tenant, entity, "A-3", "300.30", "USD", "PAYMENT_REQUESTED", utc(2026, 2, 6, 10))
	seedInvoice(t, pool, tenant, entity, "A-4", "50.00", "EUR", "VALIDATED", utc(2026, 2, 7, 10))
	seedInvoice(t, pool, tenant, entity, "A-5", "0.05", "EUR", "RECEIVED", utc(2026, 2, 8, 10))

	ctx := context.Background()
	q := domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, Limit: 2}
	seen := map[string]bool{}
	sums := map[string]string{}
	var watermark string
	var declared domain.DeclaredTotals
	pages := 0
	for {
		page, err := s.ControlPopulation(ctx, q)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if watermark == "" {
			watermark, declared = page.Watermark, page.DeclaredTotals
		} else if page.Watermark != watermark {
			t.Fatalf("watermark changed between pages: %q vs %q", watermark, page.Watermark)
		}
		prev := q.AfterRecordID
		for _, r := range page.Records {
			if seen[r.RecordID] {
				t.Fatalf("record %s served twice", r.RecordID)
			}
			if r.RecordID <= prev {
				t.Fatalf("not ascending: %s after %s", r.RecordID, prev)
			}
			prev = r.RecordID
			seen[r.RecordID] = true
			// Sum with the DB numeric type to avoid float arithmetic.
			var sum string
			if err := pool.QueryRow(ctx, `SELECT ($1::numeric + $2::numeric)::text`, orZero(sums[r.Currency]), r.Amount).Scan(&sum); err != nil {
				t.Fatal(err)
			}
			sums[r.Currency] = sum
			if r.Attributes["status"] == "" || r.Attributes["vendor_id"] != "vendor-1" || r.Attributes["due_date"] != "2026-12-31" {
				t.Fatalf("attributes: %+v", r.Attributes)
			}
		}
		if page.NextRecordID == "" {
			break
		}
		q.AfterRecordID = page.NextRecordID
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("saw %d records in %d pages, want 5 in 3", len(seen), pages)
	}
	if declared.RowCount != 5 {
		t.Fatalf("declared row_count %d", declared.RowCount)
	}
	if declared.Totals["USD"] != "600.60" || declared.Totals["EUR"] != "50.05" {
		t.Fatalf("declared totals %+v", declared.Totals)
	}
	for cur, want := range declared.Totals {
		if sums[cur] != want {
			t.Fatalf("returned records sum %s=%s, declared %s", cur, sums[cur], want)
		}
	}
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func TestControlPopulation_PeriodCutoffExcludesLaterInvoices(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenant, entity := uuid.NewString(), uuid.NewString()
	seedInvoice(t, pool, tenant, entity, "P-1", "10.00", "USD", "RECEIVED", utc(2026, 1, 31, 23)) // last day: in
	seedInvoice(t, pool, tenant, entity, "P-2", "20.00", "USD", "RECEIVED", utc(2026, 2, 1, 0))   // after month end: out

	page, err := s.ControlPopulation(context.Background(), domain.ControlPopulationQuery{
		TenantID: tenant, LegalEntityID: entity, PeriodEnd: "2026-01-31", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].Reference != "P-1" || page.Records[0].Date != "2026-01-31" {
		t.Fatalf("records %+v", page.Records)
	}
	if page.DeclaredTotals.RowCount != 1 || page.DeclaredTotals.Totals["USD"] != "10.00" {
		t.Fatalf("declared %+v", page.DeclaredTotals)
	}
	all, err := s.ControlPopulation(context.Background(), domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, Limit: 10})
	if err != nil || len(all.Records) != 2 || all.Watermark == page.Watermark {
		t.Fatalf("no cut-off should include both with a different watermark: %v %+v", err, all)
	}
}

func TestControlPopulation_TenantAndEntityIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenantA, tenantB, entity, otherEntity := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	seedInvoice(t, pool, tenantA, entity, "T-A", "10.00", "USD", "RECEIVED", utc(2026, 1, 5, 10))
	seedInvoice(t, pool, tenantB, entity, "T-B", "99.00", "USD", "RECEIVED", utc(2026, 1, 5, 10))
	seedInvoice(t, pool, tenantA, otherEntity, "T-E", "77.00", "USD", "RECEIVED", utc(2026, 1, 5, 10))

	page, err := s.ControlPopulation(context.Background(), domain.ControlPopulationQuery{TenantID: tenantA, LegalEntityID: entity, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].Reference != "T-A" || page.DeclaredTotals.Totals["USD"] != "10.00" {
		t.Fatalf("leak: %+v %+v", page.Records, page.DeclaredTotals)
	}
}

func TestControlPopulation_StatusChangeChangesWatermark(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	tenant, entity := uuid.NewString(), uuid.NewString()
	id := seedInvoice(t, pool, tenant, entity, "W-1", "10.00", "USD", "RECEIVED", utc(2026, 1, 5, 10))
	q := domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, Limit: 10}

	before, err := s.ControlPopulation(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.ControlPopulation(context.Background(), q)
	if again.Watermark != before.Watermark {
		t.Fatal("watermark must be stable when nothing changed")
	}
	scoped(t, pool, tenant, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `UPDATE vendor_invoices SET status='VALIDATED' WHERE invoice_id=$1 AND tenant_id=$2`, id, tenant); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	after, err := s.ControlPopulation(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if after.Watermark == before.Watermark {
		t.Fatal("a status change must change the watermark")
	}
}
