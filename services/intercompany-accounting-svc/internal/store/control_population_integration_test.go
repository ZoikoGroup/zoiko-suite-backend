//go:build integration

package store_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
	"zoiko.io/intercompany-accounting-svc/internal/store"
)

const (
	cpTenant1 = "tenant-cp-1"
	cpTenant2 = "tenant-cp-2"
)

// cpPool connects to the embedded Postgres and applies EVERY *.up.sql from a
// clean slate.
func cpPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS intercompany_entries CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "deployments", "migrations", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sql = bytes.TrimPrefix(sql, []byte("\xef\xbb\xbf")) // 000001 has a BOM; psql strips it
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return pool
}

type cpEntry struct {
	id, tenant, src, tgt, srcJournal, tgtJournal, amount, cur, status, createdAt string
}

func cpInsert(t *testing.T, p *pgxpool.Pool, e cpEntry) string {
	t.Helper()
	if e.id == "" {
		e.id = uuid.NewString()
	}
	if e.srcJournal == "" {
		e.srcJournal = uuid.NewString()
	}
	if e.cur == "" {
		e.cur = "USD"
	}
	if e.status == "" {
		e.status = "UNMATCHED"
	}
	if e.createdAt == "" {
		e.createdAt = "2026-09-01T10:00:00Z"
	}
	var tj any
	if e.tgtJournal != "" {
		tj = e.tgtJournal
	}
	_, err := p.Exec(context.Background(), `
		INSERT INTO intercompany_entries (intercompany_entry_id, tenant_id, source_legal_entity_id, target_legal_entity_id,
			source_journal_id, target_journal_id, amount, currency_code, match_status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10::timestamptz,$10::timestamptz)`,
		e.id, e.tenant, e.src, e.tgt, e.srcJournal, tj, e.amount, e.cur, e.status, e.createdAt)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return e.id
}

func cpCtx(tenant string) context.Context {
	return svcmiddleware.WithTenant(context.Background(), tenant)
}

func cpQuery(tenant, le, leg string, after string, limit int) domain.ControlPopulationQuery {
	return domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: le, Leg: leg, AfterRecordID: after, Limit: limit}
}

func cpSum(t *testing.T, amounts ...string) string {
	t.Helper()
	sum := new(big.Rat)
	for _, a := range amounts {
		r, ok := new(big.Rat).SetString(a)
		if !ok {
			t.Fatalf("bad amount %q", a)
		}
		sum.Add(sum, r)
	}
	return sum.FloatString(4)
}

func TestControlPopulation_SourceVsTargetScope(t *testing.T) {
	pool := cpPool(t)
	s := store.New(pool)
	tj1, tj2 := uuid.NewString(), uuid.NewString()

	// A->B (matched, has target journal), A->C (no target journal yet), B->A (matched)
	ab := cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-B", amount: "100.0000", tgtJournal: tj1, status: "MATCHED"})
	ac := cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-C", amount: "50.0000"})
	ba := cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-B", tgt: "le-A", amount: "7.0000", tgtJournal: tj2, status: "MATCHED"})

	src, err := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-A", "source", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.ControlRecord{}
	for _, r := range src.Records {
		got[r.RecordID] = r
	}
	if len(got) != 2 || got[ab].RecordID == "" || got[ac].RecordID == "" {
		t.Fatalf("source leg for le-A must be {ab, ac}, got %v", got)
	}
	if got[ab].Attributes["counterparty_entity_id"] != "le-B" || got[ac].Attributes["counterparty_entity_id"] != "le-C" {
		t.Fatalf("counterparty attribute wrong: %+v", got)
	}
	if got[ac].Attributes["target_journal_id"] != "" || got[ab].Attributes["target_journal_id"] != tj1 {
		t.Fatalf("target_journal_id attribute wrong: %+v", got)
	}
	if got[ab].Attributes["leg"] != "source" || got[ab].Attributes["match_status"] != "MATCHED" ||
		got[ab].Attributes["intercompany_entry_id"] != ab {
		t.Fatalf("attributes wrong: %+v", got[ab].Attributes)
	}
	if got[ab].Date != "2026-09-01" || got[ab].Currency != "USD" {
		t.Fatalf("date/currency wrong: %+v", got[ab])
	}

	// Target leg for le-A: only ba (ac has no target journal and le-C isn't A anyway).
	tgt, err := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-A", "target", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(tgt.Records) != 1 || tgt.Records[0].RecordID != ba {
		t.Fatalf("target leg for le-A must be {ba}, got %+v", tgt.Records)
	}
	if tgt.Records[0].Reference != tj2 || tgt.Records[0].Attributes["counterparty_entity_id"] != "le-B" ||
		tgt.Records[0].Attributes["leg"] != "target" {
		t.Fatalf("target record wrong: %+v", tgt.Records[0])
	}

	// Source reference = source_journal_id.
	var wantRef string
	if err := pool.QueryRow(context.Background(), `SELECT source_journal_id::text FROM intercompany_entries WHERE intercompany_entry_id=$1`, ab).Scan(&wantRef); err != nil {
		t.Fatal(err)
	}
	if got[ab].Reference != wantRef {
		t.Fatalf("source reference = %q want %q", got[ab].Reference, wantRef)
	}
}

func TestControlPopulation_TargetExcludesEntriesWithoutTargetJournal(t *testing.T) {
	pool := cpPool(t)
	s := store.New(pool)
	cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-B", amount: "10.0000"})
	cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-B", amount: "20.0000", status: "AWAITING_COUNTERPARTY"})

	page, err := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-B", "target", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || page.DeclaredTotals.RowCount != 0 || len(page.DeclaredTotals.Totals) != 0 {
		t.Fatalf("expected empty target leg, got %+v", page)
	}
	if page.Watermark != "0:" {
		t.Fatalf("empty watermark = %q", page.Watermark)
	}
}

func TestControlPopulation_ExactAmountAndAllStatuses(t *testing.T) {
	pool := cpPool(t)
	s := store.New(pool)
	statuses := []string{"UNMATCHED", "AWAITING_COUNTERPARTY", "MATCHED", "MISMATCH", "DISPUTED", "RESOLVED"}
	for i, st := range statuses {
		amount := "1.0000"
		if i == 0 {
			amount = "1234.5678"
		}
		cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-B", amount: amount, status: st})
	}
	page, err := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-A", "source", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != len(statuses) {
		t.Fatalf("all statuses must be returned: got %d want %d", len(page.Records), len(statuses))
	}
	seen := map[string]bool{}
	exact := false
	for _, r := range page.Records {
		seen[r.Attributes["match_status"]] = true
		if r.Amount == "1234.5678" {
			exact = true
		}
	}
	for _, st := range statuses {
		if !seen[st] {
			t.Fatalf("status %s missing", st)
		}
	}
	if !exact {
		t.Fatalf("1234.5678 must round-trip exactly, got %+v", page.Records)
	}
	if page.DeclaredTotals.Totals["USD"] != "1239.5678" {
		t.Fatalf("total = %q want 1239.5678", page.DeclaredTotals.Totals["USD"])
	}
}

func TestControlPopulation_PagingStableWatermarkAndExactTotals(t *testing.T) {
	pool := cpPool(t)
	s := store.New(pool)
	amounts := map[string][]string{"USD": {"0.1000", "0.2000", "1234.5678", "99999999999999.9999"}, "EUR": {"5.0001"}}
	want := map[string]bool{}
	for cur, list := range amounts {
		for _, a := range list {
			want[cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-B", amount: a, cur: cur})] = true
		}
	}

	var wm string
	visited := map[string]int{}
	var order []string
	after := ""
	pages := 0
	for {
		page, err := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-A", "source", after, 2))
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if wm == "" {
			wm = page.Watermark
		} else if page.Watermark != wm {
			t.Fatalf("watermark changed between pages: %q vs %q", wm, page.Watermark)
		}
		if page.DeclaredTotals.RowCount != 5 {
			t.Fatalf("row_count = %d want 5", page.DeclaredTotals.RowCount)
		}
		if page.DeclaredTotals.Totals["USD"] != cpSum(t, amounts["USD"]...) || page.DeclaredTotals.Totals["EUR"] != cpSum(t, amounts["EUR"]...) {
			t.Fatalf("declared totals not exact: %+v", page.DeclaredTotals.Totals)
		}
		for _, r := range page.Records {
			visited[r.RecordID]++
			order = append(order, r.RecordID)
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Records) != 2 {
			t.Fatalf("non-final page must be full, got %d", len(page.Records))
		}
		// the handler base64url-encodes; the store returns the raw last id
		if _, err := base64.RawURLEncoding.DecodeString(base64.RawURLEncoding.EncodeToString([]byte(page.NextCursor))); err != nil {
			t.Fatal(err)
		}
		after = page.NextCursor
	}
	if pages != 3 {
		t.Fatalf("pages = %d want 3", pages)
	}
	if len(visited) != 5 {
		t.Fatalf("visited %d distinct rows want 5", len(visited))
	}
	for id, n := range visited {
		if n != 1 || !want[id] {
			t.Fatalf("row %s visited %d times / unexpected", id, n)
		}
	}
	if !sort.StringsAreSorted(order) {
		t.Fatalf("records not in ascending record_id order: %v", order)
	}
}

func TestControlPopulation_TenantIsolation(t *testing.T) {
	pool := cpPool(t)
	s := store.New(pool)
	mine := cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-B", amount: "1.0000"})
	cpInsert(t, pool, cpEntry{tenant: cpTenant2, src: "le-A", tgt: "le-B", amount: "999.0000"})

	page, err := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-A", "source", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].RecordID != mine || page.DeclaredTotals.Totals["USD"] != "1.0000" {
		t.Fatalf("tenant leak: %+v", page)
	}
	// context tenant must agree with the query tenant
	if _, err := s.ControlPopulation(cpCtx(cpTenant2), cpQuery(cpTenant1, "le-A", "source", "", 100)); err == nil {
		t.Fatal("expected error when context tenant differs from query tenant")
	}
}

func TestControlPopulation_StatusChangeChangesWatermark(t *testing.T) {
	pool := cpPool(t)
	s := store.New(pool)
	id := cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-B", amount: "10.0000"})
	cpInsert(t, pool, cpEntry{tenant: cpTenant1, src: "le-A", tgt: "le-B", amount: "20.0000"})

	before, err := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-A", "source", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-A", "source", "", 1))
	if again.Watermark != before.Watermark {
		t.Fatal("watermark must be independent of page size and stable when nothing changed")
	}

	if _, err := pool.Exec(context.Background(), `UPDATE intercompany_entries SET match_status='MISMATCH' WHERE intercompany_entry_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	after, err := s.ControlPopulation(cpCtx(cpTenant1), cpQuery(cpTenant1, "le-A", "source", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	if after.Watermark == before.Watermark {
		t.Fatalf("match_status change must change the watermark (%q)", after.Watermark)
	}
}
