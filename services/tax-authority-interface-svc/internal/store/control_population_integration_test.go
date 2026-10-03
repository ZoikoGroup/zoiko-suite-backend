//go:build integration

package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/store"
)

func seedIface(t *testing.T, pool *pgxpool.Pool, id, tenant, entity string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tax_interfaces (interface_id, tenant_id, legal_entity_id, jurisdiction, authority_name, protocol)
		 VALUES ($1,$2,$3,'GB','HMRC','REST')`, id, tenant, entity); err != nil {
		t.Fatalf("seed interface: %v", err)
	}
}

// seedSub inserts a submission; ack == nil means NULL.
func seedSub(t *testing.T, pool *pgxpool.Pool, id, iface, tenant string, amount string, ack *string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tax_filing_submissions (submission_id, interface_id, tenant_id, tax_period, filing_type, tax_amount, status, ack_reference, submitted_at)
		 VALUES ($1,$2,$3,'2025-Q4','VAT',$4::numeric,'PENDING',$5,$6)`, id, iface, tenant, amount, ack, at); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
}

func sp(s string) *string { return &s }

func query(t *testing.T, st *store.PgStore, tenant, entity string, before time.Time, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	page, err := st.QueryUnacknowledgedFilings(context.Background(), tenant, domain.UnacknowledgedFilingsQuery{
		LegalEntityID: entity, SubmittedBefore: before, Limit: limit, AfterRecordID: after,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return page
}

func ids(p *domain.ControlPopulationPage) string {
	var out []string
	for _, r := range p.Records {
		out = append(out, r.RecordID)
	}
	return strings.Join(out, ",")
}

func TestUnacknowledgedFilings_Semantics(t *testing.T) {
	admin := openAdminPool(t)
	st := store.NewPgStore(admin)
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	seedIface(t, admin, "if-1", "tenant-a", "le-1")
	seedIface(t, admin, "if-2", "tenant-a", "le-2")
	seedIface(t, admin, "if-b", "tenant-b", "le-1")

	seedSub(t, admin, "s-acked", "if-1", "tenant-a", "100.0000", sp("HMRC-ACK-1"), cutoff.Add(-48*time.Hour))
	seedSub(t, admin, "s-null", "if-1", "tenant-a", "250.5000", nil, cutoff.Add(-48*time.Hour))
	seedSub(t, admin, "s-empty", "if-1", "tenant-a", "0.1000", sp(""), cutoff.Add(-72*time.Hour))
	seedSub(t, admin, "s-blank", "if-1", "tenant-a", "0.2000", sp("   \t"), cutoff.Add(-72*time.Hour))
	seedSub(t, admin, "s-atcutoff", "if-1", "tenant-a", "9.0000", nil, cutoff)                     // strict: excluded
	seedSub(t, admin, "s-justbefore", "if-1", "tenant-a", "1.0000", nil, cutoff.Add(-time.Second)) // included
	seedSub(t, admin, "s-after", "if-1", "tenant-a", "9.0000", nil, cutoff.Add(time.Hour))
	seedSub(t, admin, "s-other-entity", "if-2", "tenant-a", "7.0000", nil, cutoff.Add(-time.Hour))
	seedSub(t, admin, "s-tenant-b", "if-b", "tenant-b", "5.0000", nil, cutoff.Add(-time.Hour))

	p := query(t, st, "tenant-a", "le-1", cutoff, 1000, "")
	if got, want := ids(p), "s-blank,s-empty,s-justbefore,s-null"; got != want {
		t.Fatalf("records: got %s want %s", got, want)
	}
	if p.DeclaredTotals.RowCount != 4 {
		t.Fatalf("row_count %d", p.DeclaredTotals.RowCount)
	}
	if got := p.DeclaredTotals.Totals["XXX"]; got != "251.8000" {
		t.Fatalf("XXX total %q (exact numeric text expected)", got)
	}
	if !strings.HasPrefix(p.Watermark, "tx1:n=4;md5=") {
		t.Fatalf("watermark %q", p.Watermark)
	}
	for _, r := range p.Records {
		if r.Currency != "XXX" || r.Reference != r.RecordID {
			t.Fatalf("bad record %+v", r)
		}
		for _, k := range []string{"interface_id", "tax_period", "filing_type", "status", "age_days"} {
			if _, ok := r.Attributes[k]; !ok {
				t.Fatalf("missing attribute %s in %+v", k, r)
			}
		}
	}
	// s-null: 48h before cutoff => age 2, date 2026-02-27
	for _, r := range p.Records {
		if r.RecordID == "s-null" {
			if r.Date != "2026-02-27" || r.Attributes["age_days"] != "2" || r.Amount != "250.5000" {
				t.Fatalf("s-null wrong: %+v", r)
			}
		}
		if r.RecordID == "s-justbefore" && r.Attributes["age_days"] != "0" {
			t.Fatalf("s-justbefore age_days %s", r.Attributes["age_days"])
		}
		if r.RecordID == "s-empty" && r.Attributes["age_days"] != "3" {
			t.Fatalf("s-empty age_days %s", r.Attributes["age_days"])
		}
	}

	// Tenant isolation: tenant-b sees only its own; tenant-a cannot see le-1 of b.
	pb := query(t, st, "tenant-b", "le-1", cutoff, 1000, "")
	if ids(pb) != "s-tenant-b" {
		t.Fatalf("tenant-b: %s", ids(pb))
	}
	// A tenant naming another tenant's entity/interface gets nothing.
	if px := query(t, st, "tenant-b", "le-2", cutoff, 1000, ""); len(px.Records) != 0 || px.DeclaredTotals.RowCount != 0 {
		t.Fatalf("cross-tenant leak: %s", ids(px))
	}
	if px := query(t, st, "", "le-1", cutoff, 1000, ""); len(px.Records) != 0 {
		t.Fatalf("empty tenant must see nothing: %s", ids(px))
	}
	// Empty population still has a stable, well-formed watermark.
	if pe := query(t, st, "tenant-a", "le-1", cutoff.Add(-30*24*time.Hour), 10, ""); pe.Watermark != "tx1:n=0;md5=d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("empty watermark %q", pe.Watermark)
	}

	// Acknowledging a filing removes it and changes the watermark.
	if _, err := admin.Exec(context.Background(), `UPDATE tax_filing_submissions SET ack_reference='ACK-X' WHERE submission_id='s-null'`); err != nil {
		t.Fatal(err)
	}
	p2 := query(t, st, "tenant-a", "le-1", cutoff, 1000, "")
	if ids(p2) != "s-blank,s-empty,s-justbefore" || p2.Watermark == p.Watermark {
		t.Fatalf("after ack: %s wm=%s", ids(p2), p2.Watermark)
	}
}

func TestUnacknowledgedFilings_PagingStableWatermark(t *testing.T) {
	admin := openAdminPool(t)
	st := store.NewPgStore(admin)
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	seedIface(t, admin, "if-1", "tenant-a", "le-1")
	for i := 0; i < 7; i++ {
		seedSub(t, admin, fmt.Sprintf("s-%02d", i), "if-1", "tenant-a", "1.2500", nil, cutoff.Add(-time.Duration(i+1)*time.Hour))
	}

	var all []string
	wm, after := "", ""
	pages := 0
	for {
		p := query(t, st, "tenant-a", "le-1", cutoff, 3, after)
		pages++
		if wm == "" {
			wm = p.Watermark
		} else if p.Watermark != wm {
			t.Fatalf("watermark changed between pages: %s vs %s", wm, p.Watermark)
		}
		if p.DeclaredTotals.RowCount != 7 || p.DeclaredTotals.Totals["XXX"] != "8.7500" {
			t.Fatalf("declared totals must cover the whole population: %+v", p.DeclaredTotals)
		}
		for _, r := range p.Records {
			all = append(all, r.RecordID)
		}
		if p.NextRecordID == "" {
			break
		}
		after = p.NextRecordID
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
	}
	if pages != 3 || strings.Join(all, ",") != "s-00,s-01,s-02,s-03,s-04,s-05,s-06" {
		t.Fatalf("pages=%d all=%v", pages, all)
	}
}

func TestUnacknowledgedFilings_AgeDaysIndependentOfNow(t *testing.T) {
	admin := openAdminPool(t)
	st := store.NewPgStore(admin)
	seedIface(t, admin, "if-1", "tenant-a", "le-1")
	// Far in the past relative to real time; age is measured to the parameter only.
	sub := time.Date(2001, 1, 1, 12, 0, 0, 0, time.UTC)
	seedSub(t, admin, "s-old", "if-1", "tenant-a", "3.0000", nil, sub)

	p1 := query(t, st, "tenant-a", "le-1", sub.Add(36*time.Hour), 10, "")
	if len(p1.Records) != 1 || p1.Records[0].Attributes["age_days"] != "1" {
		t.Fatalf("36h => 1 day, got %+v", p1.Records)
	}
	p2 := query(t, st, "tenant-a", "le-1", sub.Add(10*24*time.Hour+time.Minute), 10, "")
	if p2.Records[0].Attributes["age_days"] != "10" {
		t.Fatalf("got %s", p2.Records[0].Attributes["age_days"])
	}
	// Same inputs, same output (repeatable).
	p3 := query(t, st, "tenant-a", "le-1", sub.Add(36*time.Hour), 10, "")
	if p3.Watermark != p1.Watermark {
		t.Fatal("watermark not reproducible")
	}
}

func TestUnacknowledgedFilings_UnderNoBypassRole(t *testing.T) {
	admin := openAdminPool(t)
	app := appRolePool(t, admin)
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	seedIface(t, admin, "if-1", "tenant-a", "le-1")
	seedIface(t, admin, "if-b", "tenant-b", "le-1")
	seedSub(t, admin, "s-a", "if-1", "tenant-a", "1.0000", nil, cutoff.Add(-time.Hour))
	seedSub(t, admin, "s-b", "if-b", "tenant-b", "2.0000", nil, cutoff.Add(-time.Hour))

	st := store.NewPgStore(app)
	if got := ids(query(t, st, "tenant-a", "le-1", cutoff, 10, "")); got != "s-a" {
		t.Fatalf("tenant-a under RLS: %s", got)
	}
	if got := ids(query(t, st, "tenant-b", "le-1", cutoff, 10, "")); got != "s-b" {
		t.Fatalf("tenant-b under RLS: %s", got)
	}
}
