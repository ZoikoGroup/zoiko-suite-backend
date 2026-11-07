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

func (f *w4Fixture) maxEntrySeq(t *testing.T) int64 {
	t.Helper()
	var wm int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(max(entry_seq), 0) FROM ledger_entries WHERE tenant_id=$1::uuid AND legal_entity_id=$2::uuid`, f.tenant, f.entity).Scan(&wm); err != nil {
		t.Fatalf("max entry_seq: %v", err)
	}
	return wm
}

func (f *w4Fixture) postSeal(t *testing.T, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	p, err := f.s.QueryPostSealEntries(f.ctx, f.tenant, domain.PostSealEntriesQuery{LegalEntityID: f.entity, Limit: limit, AfterRecordID: after})
	if err != nil {
		t.Fatalf("QueryPostSealEntries: %v", err)
	}
	return p
}

func TestPostSealEntries_OnlyEntriesPastTheWatermarkOfASealedPeriodAreFindings(t *testing.T) {
	f := newW4Fixture(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := fixtureExec(context.Background(), f.pool, sql, args...); err != nil {
			t.Fatalf("sql: %v", err)
		}
	}

	// Before the close: two journals in July and one in August.
	f.manual(t, "2026-07", domain.NewDate(2026, 7, 10), dr("1200", 100), cr("4000", 100))
	f.manual(t, "2026-07", domain.NewDate(2026, 7, 20), dr("1200", 50), cr("4000", 50))
	f.manual(t, "2026-08", domain.NewDate(2026, 8, 5), dr("1200", 7), cr("4000", 7))
	wm := f.maxEntrySeq(t)

	// July is sealed at the entity watermark, exactly as ApplySeal records it.
	exec(`INSERT INTO period_seals (tenant_id, legal_entity_id, fiscal_period, status, watermark, sealed_by, sealed_at)
	      VALUES ($1::uuid, $2::uuid, '2026-07', 'SEALED', $3, 'closer-1', now())`, f.tenant, f.entity, wm)
	if p := f.postSeal(t, 100, ""); len(p.Records) != 0 || p.DeclaredTotals.RowCount != 0 {
		t.Fatalf("nothing was posted after the seal yet: %+v", p.Records)
	}

	// After the seal. The normal posting path refuses a sealed period (assertPeriodOpen), so a real slip can only
	// come from an out-of-band write: relabel a journal posted in August into the sealed July the way a direct
	// database edit would. The second August journal stays put.
	slip := f.manual(t, "2026-08", domain.NewDate(2026, 8, 9), dr("1200", 25), cr("4000", 25))
	exec(`UPDATE ledger_entries SET fiscal_period='2026-07' WHERE tenant_id=$1::uuid AND journal_id=$2::uuid`, f.tenant, slip)
	f.manual(t, "2026-08", domain.NewDate(2026, 8, 10), dr("1200", 3), cr("4000", 3))

	p := f.postSeal(t, 100, "")
	if len(p.Records) != 2 || p.DeclaredTotals.RowCount != 2 {
		t.Fatalf("only the two entries of the post-seal July journal are findings, got %+v", p.Records)
	}
	amounts := map[string]bool{}
	for _, r := range p.Records {
		amounts[r.Amount] = true
		if r.Reference != slip || r.Attributes["fiscal_period"] != "2026-07" || r.Attributes["seal_watermark"] == "" ||
			r.Attributes["sealed_by"] != "closer-1" || r.Currency != "USD" {
			t.Fatalf("record %+v", r)
		}
	}
	if !amounts["25.00"] || !amounts["-25.00"] {
		t.Fatalf("signed net amounts expected, got %v", amounts)
	}
	if p.DeclaredTotals.Totals["USD"] != "0.00" || !strings.HasPrefix(p.Watermark, "gl10:n=2;") {
		t.Fatalf("totals %+v watermark %q", p.DeclaredTotals, p.Watermark)
	}

	// Tenant and entity isolation.
	if other, err := f.s.QueryPostSealEntries(f.ctx, uuid.NewString(), domain.PostSealEntriesQuery{LegalEntityID: f.entity, Limit: 10}); err != nil || len(other.Records) != 0 {
		t.Fatalf("another tenant must see nothing: %v %+v", err, other)
	}
	if other, err := f.s.QueryPostSealEntries(f.ctx, f.tenant, domain.PostSealEntriesQuery{LegalEntityID: uuid.NewString(), Limit: 10}); err != nil || len(other.Records) != 0 {
		t.Fatalf("another entity must see nothing: %v %+v", err, other)
	}

	// Keyset paging is complete, ordered and under one watermark.
	var ids []string
	marks := map[string]bool{}
	after := ""
	for guard := 0; ; guard++ {
		if guard > 10 {
			t.Fatal("paging did not terminate")
		}
		pg := f.postSeal(t, 1, after)
		marks[pg.Watermark] = true
		for _, r := range pg.Records {
			ids = append(ids, r.RecordID)
		}
		if pg.NextRecordID == "" {
			break
		}
		after = pg.NextRecordID
	}
	if len(ids) != 2 || len(marks) != 1 || !sort.StringsAreSorted(ids) {
		t.Fatalf("ids %v, %d watermarks", ids, len(marks))
	}

	// A reopen under authority makes entries posted while reopened authorized: the period leaves the scope ...
	exec(`UPDATE period_seals SET status='REOPENED', reopened_by='controller-1', reopened_at=now(), reopen_authority='CFO-APPROVAL-1', reopen_reason='late accrual'
	      WHERE tenant_id=$1::uuid AND legal_entity_id=$2::uuid AND fiscal_period='2026-07'`, f.tenant, f.entity)
	if p := f.postSeal(t, 100, ""); len(p.Records) != 0 {
		t.Fatalf("a reopened period is not a finding: %+v", p.Records)
	}
	// ... and the re-seal moves the watermark above everything posted, so it stays clear.
	exec(`UPDATE period_seals SET status='SEALED', watermark=$3, sealed_by='closer-2', sealed_at=now(), seal_count=seal_count+1
	      WHERE tenant_id=$1::uuid AND legal_entity_id=$2::uuid AND fiscal_period='2026-07'`, f.tenant, f.entity, f.maxEntrySeq(t))
	if p := f.postSeal(t, 100, ""); len(p.Records) != 0 {
		t.Fatalf("after a re-seal nothing is past the watermark: %+v", p.Records)
	}
}
