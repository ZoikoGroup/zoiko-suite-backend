//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payee-banking-identity-svc/internal/domain"
	"zoiko.io/payee-banking-identity-svc/internal/middleware"
	"zoiko.io/payee-banking-identity-svc/internal/store"
)

func openPool(t *testing.T) *pgxpool.Pool {
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

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS payee_destination_events, payee_destinations CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	_, file, _, _ := runtime.Caller(0)
	migrations, err := filepath.Glob(filepath.Join(filepath.Dir(file), "../../deployments/migrations/*.up.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	for _, m := range migrations { // Glob returns them sorted
		sqlText, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(sqlText)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(m), err)
		}
	}
	return pool
}

type fixture struct {
	pool   *pgxpool.Pool
	s      *store.PgStore
	tenant string
	entity string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := openPool(t)
	return &fixture{pool: pool, s: store.NewPgStore(pool, zap.NewNop()), tenant: uuid.New().String(), entity: "le-1"}
}

const (
	secretInstitution = "Secret Bank of Nowhere"
	secretAccount     = "GB29NWBK60161331926819"
	secretPayeeName   = "Confidential Payee Ltd"
)

type dest struct {
	tenant, entity string
	status         string
	proposedBy     string
	verifiedBy     string
	approvedBy     string
	approvedAt     bool
	proposedAt     string // PROPOSED event timestamp, RFC3339
	extraEvents    []string
}

// seed inserts a destination directly (the service's own commands cannot
// produce the bypass shapes the control is meant to find) plus its events.
func (f *fixture) seed(t *testing.T, d dest) string {
	t.Helper()
	if d.tenant == "" {
		d.tenant = f.tenant
	}
	if d.entity == "" {
		d.entity = f.entity
	}
	id := uuid.New().String()
	approvedAt := "NULL"
	if d.approvedAt {
		approvedAt = "now()"
	}
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO payee_destinations (destination_id, tenant_id, legal_entity_id, party_ref, financial_institution,
			account_identifier, account_last4, country_code, currency, payee_name, source_type, fingerprint, status,
			verified_by_principal_id, approved_by_principal_id, approved_at, proposed_by_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6, '6819', 'GB', 'GBP', $7, 'MANUAL_ENTRY', $8, $9, $10, $11, `+approvedAt+`, $12)`,
		id, d.tenant, d.entity, "party-"+id[:8], secretInstitution, secretAccount, secretPayeeName,
		"fp-"+id, d.status, d.verifiedBy, d.approvedBy, d.proposedBy)
	if err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	f.event(t, d.tenant, id, "PAYEE_DESTINATION_PROPOSED", d.proposedAt)
	for _, e := range d.extraEvents {
		f.event(t, d.tenant, id, e, d.proposedAt)
	}
	return id
}

func (f *fixture) event(t *testing.T, tenant, id, typ, at string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO payee_destination_events (tenant_id, destination_id, event_type, actor_principal_id, created_at)
		VALUES ($1, $2, $3, 'actor', $4::timestamptz)`, tenant, id, typ, at); err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

func (f *fixture) query(t *testing.T, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	return f.queryAs(t, f.tenant, f.entity, limit, after)
}

func (f *fixture) queryAs(t *testing.T, tenant, entity string, limit int, after string) *domain.ControlPopulationPage {
	t.Helper()
	p, err := f.s.QueryDestinationChanges(middleware.WithTenant(context.Background(), tenant), tenant, domain.DestinationChangesQuery{
		LegalEntityID: entity, ChangedFrom: "2026-09-01", ChangedTo: "2026-09-30", Limit: limit, AfterRecordID: after,
	})
	if err != nil {
		t.Fatalf("QueryDestinationChanges: %v", err)
	}
	return p
}

func byID(p *domain.ControlPopulationPage) map[string]domain.ControlPopulationRecord {
	m := map[string]domain.ControlPopulationRecord{}
	for _, r := range p.Records {
		m[r.RecordID] = r
	}
	return m
}

func active(at string) dest {
	return dest{status: "ACTIVE", proposedBy: "m", verifiedBy: "v", approvedBy: "a", approvedAt: true, proposedAt: at}
}

func TestDestinationChanges_SodGaps(t *testing.T) {
	f := newFixture(t)
	const at = "2026-09-10T12:00:00Z"
	clean := f.seed(t, dest{status: "ACTIVE", proposedBy: "maker", verifiedBy: "checker", approvedBy: "approver", approvedAt: true, proposedAt: at})
	noApprover := f.seed(t, dest{status: "ACTIVE", proposedBy: "maker", verifiedBy: "checker", approvedBy: "", proposedAt: at})
	blankApprover := f.seed(t, dest{status: "ACTIVE", proposedBy: "maker", verifiedBy: "checker", approvedBy: "   ", proposedAt: at})
	selfApproved := f.seed(t, dest{status: "ACTIVE", proposedBy: "maker", verifiedBy: "checker", approvedBy: " maker ", approvedAt: true, proposedAt: at})
	selfVerified := f.seed(t, dest{status: "APPROVAL_PENDING", proposedBy: "maker", verifiedBy: "maker ", approvedBy: "approver", approvedAt: true, proposedAt: at})
	noVerifier := f.seed(t, dest{status: "APPROVAL_PENDING", proposedBy: "maker", verifiedBy: "", approvedBy: "approver", approvedAt: true, proposedAt: at})
	// Self-approved AND self-verified: the earlier rule (SELF_APPROVED) wins.
	both := f.seed(t, dest{status: "ACTIVE", proposedBy: "maker", verifiedBy: "maker", approvedBy: "maker", approvedAt: true, proposedAt: at})
	// Reached approval and was later superseded: still an approved change.
	superseded := f.seed(t, dest{status: "SUPERSEDED", proposedBy: "maker", verifiedBy: "checker", approvedBy: "approver", proposedAt: at,
		extraEvents: []string{"PAYEE_DESTINATION_APPROVED"}})
	// Never approved: not in the population.
	candidate := f.seed(t, dest{status: "CANDIDATE", proposedBy: "maker", proposedAt: at})
	verifiedOnly := f.seed(t, dest{status: "VERIFIED", proposedBy: "maker", verifiedBy: "checker", proposedAt: at})
	supNever := f.seed(t, dest{status: "SUPERSEDED", proposedBy: "maker", verifiedBy: "checker", proposedAt: at})

	p := f.query(t, 100, "")
	got := byID(p)
	want := map[string]string{
		clean: "", noApprover: "NO_APPROVER", blankApprover: "NO_APPROVER", selfApproved: "SELF_APPROVED",
		selfVerified: "SELF_VERIFIED", noVerifier: "NO_VERIFIER", both: "SELF_APPROVED", superseded: "",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d records, got %d: %+v", len(want), len(got), p.Records)
	}
	for id, gap := range want {
		r, ok := got[id]
		if !ok {
			t.Fatalf("destination %s missing from population", id)
		}
		if r.Attributes["sod_gap"] != gap {
			t.Errorf("%s: sod_gap = %q, want %q (%v)", id, r.Attributes["sod_gap"], gap, r.Attributes)
		}
		if r.Reference != id || r.Amount != "0" || r.Currency != "XXX" || r.Date != "2026-09-10" {
			t.Errorf("%s: unexpected wire fields %+v", id, r)
		}
	}
	for _, id := range []string{candidate, verifiedOnly, supNever} {
		if _, ok := got[id]; ok {
			t.Errorf("unapproved destination %s must not be in the population", id)
		}
	}
	if p.DeclaredTotals.RowCount != int64(len(want)) || p.DeclaredTotals.Totals["XXX"] != "0" || len(p.DeclaredTotals.Totals) != 1 {
		t.Errorf("declared totals: %+v", p.DeclaredTotals)
	}
	if !strings.HasPrefix(p.Watermark, "pb1:n=8;md5=") {
		t.Errorf("watermark: %s", p.Watermark)
	}
	if a := got[selfVerified].Attributes; a["party_ref"] == "" || a["status"] != "APPROVAL_PENDING" || a["proposed_by"] != "maker" {
		t.Errorf("attributes: %v", a)
	}
}

func TestDestinationChanges_DateRangeBoundaries(t *testing.T) {
	f := newFixture(t)
	before := f.seed(t, active("2026-08-31T23:59:59Z"))
	first := f.seed(t, active("2026-09-01T00:00:00Z"))
	last := f.seed(t, active("2026-09-30T23:59:59.999Z"))
	after := f.seed(t, active("2026-10-01T00:00:00Z"))
	// 2026-09-30T23:30-05:00 is 2026-10-01 in UTC: out of range.
	offsetOut := f.seed(t, active("2026-09-30T23:30:00-05:00"))
	// 2026-09-01T01:00+03:00 is 2026-08-31 in UTC: out of range.
	offsetOut2 := f.seed(t, active("2026-09-01T01:00:00+03:00"))

	got := byID(f.query(t, 100, ""))
	if len(got) != 2 || got[first].Date != "2026-09-01" || got[last].Date != "2026-09-30" {
		t.Fatalf("unexpected population: %+v", got)
	}
	for _, id := range []string{before, after, offsetOut, offsetOut2} {
		if _, ok := got[id]; ok {
			t.Errorf("%s must be out of range", id)
		}
	}
}

func TestDestinationChanges_TenantAndEntityIsolation(t *testing.T) {
	f := newFixture(t)
	other := uuid.New().String()
	const at = "2026-09-10T12:00:00Z"
	mine := f.seed(t, active(at))
	otherTenantDest := active(at)
	otherTenantDest.tenant = other
	theirs := f.seed(t, otherTenantDest)
	otherEntityDest := active(at)
	otherEntityDest.entity = "le-2"
	otherEntity := f.seed(t, otherEntityDest)

	got := byID(f.query(t, 100, ""))
	if len(got) != 1 {
		t.Fatalf("expected only own destination, got %+v", got)
	}
	if _, ok := got[mine]; !ok {
		t.Error("own destination missing")
	}
	if _, ok := got[theirs]; ok {
		t.Error("cross-tenant destination leaked")
	}
	if _, ok := got[otherEntity]; ok {
		t.Error("other-entity destination leaked")
	}
	tp := f.queryAs(t, other, f.entity, 100, "")
	if len(tp.Records) != 1 || tp.Records[0].RecordID != theirs {
		t.Fatalf("other tenant should see only its own record: %+v", tp.Records)
	}
}

func TestDestinationChanges_PagingStableWatermark(t *testing.T) {
	f := newFixture(t)
	const total = 7
	for i := 0; i < total; i++ {
		f.seed(t, active("2026-09-15T00:00:00Z"))
	}
	seen := map[string]bool{}
	var first *domain.ControlPopulationPage
	prev, after := "", ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
		p := f.query(t, 3, after)
		if first == nil {
			first = p
		} else if p.Watermark != first.Watermark || p.DeclaredTotals.RowCount != first.DeclaredTotals.RowCount {
			t.Fatalf("watermark/count changed between pages: %s vs %s", p.Watermark, first.Watermark)
		}
		for _, r := range p.Records {
			if r.RecordID <= prev {
				t.Fatalf("records not strictly ascending: %s after %s", r.RecordID, prev)
			}
			prev = r.RecordID
			seen[r.RecordID] = true
		}
		if p.NextRecordID == "" {
			break
		}
		if p.NextRecordID != p.Records[len(p.Records)-1].RecordID || len(p.Records) != 3 {
			t.Fatalf("bad cursor / page size: %+v", p)
		}
		after = p.NextRecordID
	}
	if len(seen) != total || first.DeclaredTotals.RowCount != total {
		t.Fatalf("visited %d of %d (declared %d)", len(seen), total, first.DeclaredTotals.RowCount)
	}

	// The watermark changes when (and only when) the population changes.
	if f.query(t, 3, "").Watermark != first.Watermark {
		t.Fatal("watermark must be reproducible for an unchanged population")
	}
	f.seed(t, active("2026-09-16T00:00:00Z"))
	if f.query(t, 3, "").Watermark == first.Watermark {
		t.Fatal("watermark must change when a destination is added")
	}
}

func TestDestinationChanges_WatermarkCoversAttributes(t *testing.T) {
	f := newFixture(t)
	id := f.seed(t, active("2026-09-15T00:00:00Z"))
	w1 := f.query(t, 10, "").Watermark
	if _, err := f.pool.Exec(context.Background(), `UPDATE payee_destinations SET approved_by_principal_id = 'someone-else' WHERE destination_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if f.query(t, 10, "").Watermark == w1 {
		t.Fatal("watermark must change when an attribute changes")
	}
}

func TestDestinationChanges_NoBankDetailsInJSON(t *testing.T) {
	f := newFixture(t)
	f.seed(t, active("2026-09-15T00:00:00Z"))
	p := f.query(t, 10, "")
	if len(p.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(p.Records))
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	js := strings.ToLower(string(b))
	for _, banned := range []string{
		strings.ToLower(secretInstitution), strings.ToLower(secretAccount), strings.ToLower(secretPayeeName), "6819",
		"account_identifier", "account_last4", "financial_institution", "iban", "sort_code", "payee_name", "country", "fingerprint",
	} {
		if strings.Contains(js, banned) {
			t.Errorf("bank detail %q leaked into %s", banned, js)
		}
	}
	keys := map[string]bool{}
	for k := range p.Records[0].Attributes {
		keys[k] = true
	}
	for _, k := range []string{"party_ref", "status", "proposed_by", "verified_by", "approved_by", "sod_gap"} {
		if !keys[k] {
			t.Errorf("missing attribute %s", k)
		}
		delete(keys, k)
	}
	if len(keys) != 0 {
		t.Errorf("unexpected attributes: %v", keys)
	}
}
