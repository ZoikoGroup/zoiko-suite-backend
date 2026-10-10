//go:build integration

// Real-Postgres tests for the open-invoices control population. They run against
// an embedded Postgres started by TestMain (no TEST_DATABASE_URL needed) and do
// not change how the other integration tests in this package find their database.
// The embedded superuser bypasses RLS, so the tenant-isolation test proves the
// EXPLICIT tenant_id predicate, not the policy.
//
// Run: go test -tags=integration -count=1 -timeout=400s ./internal/store/
package store_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/store"
)

var embeddedPool *pgxpool.Pool

func TestMain(m *testing.M) {
	port := uint32(17101 + uint32(os.Getpid()%499))
	// RuntimePath is pid-derived and private to this process, not the
	// library's shared default (~/.embedded-postgres-go/extracted) — a
	// second embedded-Postgres process anywhere on the machine (another
	// service's own integration suite, a manually started scratch instance)
	// contends for that same directory's binaries on Windows, where an
	// in-use .dll cannot be deleted/overwritten. That contention is exactly
	// what "unable to clean up runtime directory ... Access is denied"
	// means, and it has nothing to do with whether this package's own tests
	// are correct.
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).Port(port).Database("ar_ctrlpop_test").
		Username("postgres").Password("postgres").
		RuntimePath(filepath.Join(os.TempDir(), fmt.Sprintf("epg-ar-ctrlpop-%d", port))))
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}
	stop := func() { _ = pg.Stop() }

	ctx := context.Background()
	dsn := fmt.Sprintf("host=localhost port=%d dbname=ar_ctrlpop_test user=postgres password=postgres sslmode=disable", port)
	var err error
	embeddedPool, err = pgxpool.New(ctx, dsn)
	if err == nil {
		for i := 0; i < 75; i++ {
			if err = embeddedPool.Ping(ctx); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err != nil {
		fmt.Printf("embedded postgres not ready: %v\n", err)
		stop()
		os.Exit(1)
	}
	files, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
	sort.Strings(files)
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr == nil {
			_, rerr = embeddedPool.Exec(ctx, string(b))
		}
		if rerr != nil {
			fmt.Printf("migration %s: %v\n", f, rerr)
			stop()
			os.Exit(1)
		}
	}

	code := m.Run()
	embeddedPool.Close()
	stop()
	os.Exit(code)
}

type popInv struct {
	tenant, entity, number string
	amount                 string
	currency               string
	status                 string
	createdAt              time.Time
	paidAt                 *time.Time
}

func insertPopInv(t *testing.T, in popInv) string {
	t.Helper()
	id := uuid.NewString()
	if in.currency == "" {
		in.currency = "USD"
	}
	if in.status == "" {
		in.status = "ISSUED"
	}
	var sentAt, sentBy any
	if in.status != "ISSUED" {
		sentAt, sentBy = in.createdAt.Add(time.Minute), "sender"
	}
	var paidBy any
	if in.paidAt != nil {
		paidBy = "payer"
	}
	_, err := embeddedPool.Exec(context.Background(), `
		INSERT INTO customer_invoices (invoice_id, tenant_id, legal_entity_id, customer_id, invoice_number,
			amount, currency_code, due_date, status, created_by_principal_id, sent_by_principal_id,
			payment_received_by_principal_id, correlation_id, created_at, sent_at, payment_received_at,
			invoice_date, supply_date, net_amount, tax_amount)
		VALUES ($1,$2,$3,'cust-1',$4,$5::numeric,$6,'2026-12-31',$7,'creator',$8,$9,$10,$11,$12,$13,
			$11::timestamptz::date, $11::timestamptz::date, $5::numeric, 0)`,
		id, in.tenant, in.entity, in.number, in.amount, in.currency, in.status, sentBy, paidBy,
		"corr-"+id, in.createdAt, sentAt, in.paidAt)
	require.NoError(t, err)
	return id
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }

func popStore() *store.PgStore { return store.New(embeddedPool, zap.NewNop()) }

func fetchAll(t *testing.T, q domain.ControlPopulationQuery) ([]domain.ControlRecord, []*domain.ControlPopulationPage) {
	t.Helper()
	var recs []domain.ControlRecord
	var pages []*domain.ControlPopulationPage
	for i := 0; i < 100; i++ {
		p, err := popStore().ControlPopulation(context.Background(), q)
		require.NoError(t, err)
		pages = append(pages, p)
		recs = append(recs, p.Records...)
		if p.NextCursor == "" {
			return recs, pages
		}
		q.AfterRecordID = p.NextCursor
	}
	t.Fatal("paging did not terminate")
	return nil, nil
}

func sumByCurrency(t *testing.T, recs []domain.ControlRecord) map[string]string {
	t.Helper()
	acc := map[string]*big.Rat{}
	for _, r := range recs {
		v, ok := new(big.Rat).SetString(r.Amount)
		require.True(t, ok, r.Amount)
		if acc[r.Currency] == nil {
			acc[r.Currency] = new(big.Rat)
		}
		acc[r.Currency].Add(acc[r.Currency], v)
	}
	out := map[string]string{}
	for c, v := range acc {
		out[c] = v.FloatString(2)
	}
	return out
}

func seedFive(t *testing.T, tenant, entity string) {
	for i, amt := range []string{"100.10", "200.20", "300.30", "50.05", "75.00"} {
		cur := "USD"
		if i == 4 {
			cur = "EUR"
		}
		insertPopInv(t, popInv{tenant: tenant, entity: entity, number: fmt.Sprintf("INV-%d", i),
			amount: amt, currency: cur, createdAt: day(2026, 9, 1+i)})
	}
}

func TestControlPopulation_PagingWalksEveryRowOnceWithStableWatermark(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	seedFive(t, tenant, entity)

	recs, pages := fetchAll(t, domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, Limit: 2})
	require.Len(t, pages, 3)
	require.Len(t, recs, 5)
	seen := map[string]bool{}
	var prev string
	for _, r := range recs {
		assert.False(t, seen[r.RecordID], "record %s returned twice", r.RecordID)
		seen[r.RecordID] = true
		assert.Greater(t, r.RecordID, prev, "keyset order must be ascending on record_id")
		prev = r.RecordID
	}
	for _, p := range pages {
		assert.Equal(t, pages[0].Watermark, p.Watermark)
		assert.EqualValues(t, 5, p.DeclaredTotals.RowCount)
	}
	assert.Equal(t, "", pages[2].NextCursor)
	assert.NotEmpty(t, pages[0].Watermark)
}

func TestControlPopulation_DeclaredTotalsEqualSumOfRecords(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	seedFive(t, tenant, entity)

	recs, pages := fetchAll(t, domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, Limit: 2})
	assert.Equal(t, sumByCurrency(t, recs), pages[0].DeclaredTotals.Totals)
	assert.Equal(t, map[string]string{"USD": "650.65", "EUR": "75.00"}, pages[0].DeclaredTotals.Totals)
	assert.EqualValues(t, len(recs), pages[0].DeclaredTotals.RowCount)
	// The record shape: decimal strings, YYYY-MM-DD, attributes.
	r := recs[0]
	assert.Regexp(t, `^\d+\.\d{2}$`, r.Amount)
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, r.Date)
	assert.Equal(t, "ISSUED", r.Attributes["status"])
	assert.Equal(t, "cust-1", r.Attributes["customer_id"])
	assert.Equal(t, "2026-12-31", r.Attributes["due_date"])
	assert.Regexp(t, `^\d+\.\d{2}$`, r.Attributes["invoice_amount"])
}

func TestControlPopulation_PaidOutstandingRespectsCutOff(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	paidEarly, paidLate := day(2026, 9, 10), day(2026, 10, 5)
	idEarly := insertPopInv(t, popInv{tenant: tenant, entity: entity, number: "A", amount: "500.00",
		status: "PAID", createdAt: day(2026, 9, 1), paidAt: &paidEarly})
	idLate := insertPopInv(t, popInv{tenant: tenant, entity: entity, number: "B", amount: "700.00",
		status: "PAID", createdAt: day(2026, 9, 2), paidAt: &paidLate})
	cutOff := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	recs, pages := fetchAll(t, domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, CutOff: &cutOff, Limit: 10})
	byID := map[string]domain.ControlRecord{}
	for _, r := range recs {
		byID[r.RecordID] = r
	}
	assert.Equal(t, "0.00", byID[idEarly].Amount, "paid at/before cut-off is fully settled")
	assert.Equal(t, "500.00", byID[idEarly].Attributes["invoice_amount"])
	assert.Equal(t, "700.00", byID[idLate].Amount, "paid after the cut-off was still open at the cut-off")
	assert.Equal(t, "PAID", byID[idLate].Attributes["status"], "paid invoices are still returned")
	assert.Equal(t, map[string]string{"USD": "700.00"}, pages[0].DeclaredTotals.Totals)

	// Paid on the cut-off day itself counts as settled.
	onDay := day(2026, 9, 30)
	idOnDay := insertPopInv(t, popInv{tenant: tenant, entity: entity, number: "C", amount: "10.00",
		status: "PAID", createdAt: day(2026, 9, 3), paidAt: &onDay})
	recs, _ = fetchAll(t, domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, CutOff: &cutOff, Limit: 10})
	for _, r := range recs {
		if r.RecordID == idOnDay {
			assert.Equal(t, "0.00", r.Amount)
		}
	}

	// No period: no cut-off, every PAID invoice is settled.
	recs, pages = fetchAll(t, domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, Limit: 10})
	for _, r := range recs {
		assert.Equal(t, "0.00", r.Amount)
	}
	assert.Equal(t, map[string]string{"USD": "0.00"}, pages[0].DeclaredTotals.Totals)
}

func TestControlPopulation_PeriodCutOffExcludesLaterInvoices(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	inScope := insertPopInv(t, popInv{tenant: tenant, entity: entity, number: "IN", amount: "10.00",
		createdAt: time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC)})
	insertPopInv(t, popInv{tenant: tenant, entity: entity, number: "OUT", amount: "20.00",
		createdAt: time.Date(2026, 10, 1, 0, 30, 0, 0, time.UTC)})
	cutOff := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)

	recs, pages := fetchAll(t, domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, CutOff: &cutOff, Limit: 10})
	require.Len(t, recs, 1)
	assert.Equal(t, inScope, recs[0].RecordID)
	assert.EqualValues(t, 1, pages[0].DeclaredTotals.RowCount)
	assert.Equal(t, map[string]string{"USD": "10.00"}, pages[0].DeclaredTotals.Totals)

	recs, _ = fetchAll(t, domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, Limit: 10})
	assert.Len(t, recs, 2, "no period means no date cut-off")
}

func TestControlPopulation_TenantAndEntityIsolation(t *testing.T) {
	tenantA, tenantB, entity := uuid.NewString(), uuid.NewString(), uuid.NewString()
	seedFive(t, tenantA, entity)

	// Same legal entity id, different tenant: nothing.
	recs, pages := fetchAll(t, domain.ControlPopulationQuery{TenantID: tenantB, LegalEntityID: entity, Limit: 10})
	assert.Empty(t, recs)
	assert.EqualValues(t, 0, pages[0].DeclaredTotals.RowCount)
	assert.Empty(t, pages[0].DeclaredTotals.Totals)

	// Same tenant, different entity: nothing.
	recs, _ = fetchAll(t, domain.ControlPopulationQuery{TenantID: tenantA, LegalEntityID: uuid.NewString(), Limit: 10})
	assert.Empty(t, recs)

	// The owner still sees its five.
	recs, _ = fetchAll(t, domain.ControlPopulationQuery{TenantID: tenantA, LegalEntityID: entity, Limit: 10})
	assert.Len(t, recs, 5)
}

func TestControlPopulation_StatusChangeChangesWatermark(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	id := insertPopInv(t, popInv{tenant: tenant, entity: entity, number: "W1", amount: "42.00", createdAt: day(2026, 9, 1)})
	insertPopInv(t, popInv{tenant: tenant, entity: entity, number: "W2", amount: "43.00", createdAt: day(2026, 9, 2)})
	q := domain.ControlPopulationQuery{TenantID: tenant, LegalEntityID: entity, Limit: 10}

	_, before := fetchAll(t, q)
	_, again := fetchAll(t, q)
	assert.Equal(t, before[0].Watermark, again[0].Watermark, "unchanged data, unchanged watermark")

	// ISSUED -> SENT: same outstanding amount, different status.
	_, err := embeddedPool.Exec(context.Background(),
		`UPDATE customer_invoices SET status='SENT', sent_by_principal_id='s', sent_at=now() WHERE invoice_id=$1`, id)
	require.NoError(t, err)
	_, sent := fetchAll(t, q)
	assert.NotEqual(t, before[0].Watermark, sent[0].Watermark)

	// SENT -> PAID: outstanding drops to zero.
	_, err = embeddedPool.Exec(context.Background(),
		`UPDATE customer_invoices SET status='PAID', payment_received_by_principal_id='p', payment_received_at=now() WHERE invoice_id=$1`, id)
	require.NoError(t, err)
	_, paid := fetchAll(t, q)
	assert.NotEqual(t, sent[0].Watermark, paid[0].Watermark)
}
