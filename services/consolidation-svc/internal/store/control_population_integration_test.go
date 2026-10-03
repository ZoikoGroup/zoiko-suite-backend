//go:build integration

// Real-Postgres tests for the balance-contributions control population. The
// embedded superuser bypasses RLS, so the tenant-isolation test proves the
// EXPLICIT tenant_id predicate, not the policy.
package store_test

import (
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/consolidation-svc/internal/domain"
	"zoiko.io/consolidation-svc/internal/store"
)

func popPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func seedRun(t *testing.T, pool *pgxpool.Pool, tenant, currency, status string) string {
	t.Helper()
	id := uuid.NewString()
	// 23:30 UTC on 2026-09-01: proves the date is the UTC date.
	_, err := pool.Exec(context.Background(), `
		INSERT INTO consolidation_runs (consolidation_run_id, tenant_id, group_legal_entity_id,
			fiscal_period, target_currency, status, exception_count, started_at)
		VALUES ($1,$2,'group-1','2026-08',$3,$4,0,'2026-09-01T23:30:00Z')`, id, tenant, currency, status)
	require.NoError(t, err)
	return id
}

func seedContribution(t *testing.T, pool *pgxpool.Pool, tenant, runID, entity, account, amount string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO balance_contributions (balance_contribution_id, tenant_id, consolidation_run_id,
			account_code, source_legal_entity_id, gross_amount, generated_at)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,now())`, id, tenant, runID, account, entity, amount)
	require.NoError(t, err)
	return id
}

func fetchAllContrib(t *testing.T, s *store.PgStore, q domain.BalanceContributionsQuery) ([]domain.ControlRecord, []*domain.ControlPopulationPage) {
	t.Helper()
	var recs []domain.ControlRecord
	var pages []*domain.ControlPopulationPage
	for i := 0; i < 100; i++ {
		p, err := s.BalanceContributionsPopulation(context.Background(), q)
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

func TestBalanceContributions_ScopeCurrencyAndExactAmounts(t *testing.T) {
	pool := popPool(t)
	s := store.New(pool)
	tenant := uuid.NewString()
	run := seedRun(t, pool, tenant, "GBP", "COMPLETED")
	otherRun := seedRun(t, pool, tenant, "USD", "COMPLETED")

	exact := seedContribution(t, pool, tenant, run, "child-a", "1000-Cash", "1234.5678")
	seedContribution(t, pool, tenant, run, "child-b", "1000-Cash", "999.0000")      // other entity
	seedContribution(t, pool, tenant, otherRun, "child-a", "1000-Cash", "555.0000") // other run

	recs, pages := fetchAllContrib(t, s, domain.BalanceContributionsQuery{TenantID: tenant, RunID: run, LegalEntityID: "child-a", Limit: 10})
	require.Len(t, recs, 1)
	r := recs[0]
	assert.Equal(t, exact, r.RecordID)
	assert.Equal(t, "1000-Cash", r.Reference)
	assert.Equal(t, "1234.5678", r.Amount, "4-decimal NUMERIC must round-trip exactly")
	assert.Equal(t, "GBP", r.Currency, "currency is the run's target_currency, not the other run's")
	assert.Equal(t, "2026-09-01", r.Date)
	assert.Equal(t, map[string]string{
		"consolidation_run_id":   run,
		"source_legal_entity_id": "child-a",
		"currency_basis":         "target_currency_label",
		"run_status":             "COMPLETED",
	}, r.Attributes)
	assert.EqualValues(t, 1, pages[0].DeclaredTotals.RowCount)
	assert.Equal(t, map[string]string{"GBP": "1234.5678"}, pages[0].DeclaredTotals.Totals)
	assert.Regexp(t, `^1:[0-9a-f]{32}$`, pages[0].Watermark)
}

func TestBalanceContributions_PagingAndDeclaredTotals(t *testing.T) {
	pool := popPool(t)
	s := store.New(pool)
	tenant := uuid.NewString()
	run := seedRun(t, pool, tenant, "USD", "RUNNING")
	amounts := []string{"100.1000", "200.2000", "300.3000", "0.0001", "-75.0025"}
	for i, a := range amounts {
		seedContribution(t, pool, tenant, run, "child-a", "acct-"+string(rune('a'+i)), a)
	}

	q := domain.BalanceContributionsQuery{TenantID: tenant, RunID: run, LegalEntityID: "child-a", Limit: 2}
	recs, pages := fetchAllContrib(t, s, q)
	require.Len(t, pages, 3)
	require.Len(t, recs, 5)
	seen := map[string]bool{}
	prev := ""
	sum := new(big.Rat)
	for _, r := range recs {
		assert.False(t, seen[r.RecordID], "record %s returned twice", r.RecordID)
		seen[r.RecordID] = true
		assert.Greater(t, r.RecordID, prev, "keyset order must be ascending on record_id")
		prev = r.RecordID
		v, ok := new(big.Rat).SetString(r.Amount)
		require.True(t, ok)
		sum.Add(sum, v)
	}
	for _, p := range pages {
		assert.Equal(t, pages[0].Watermark, p.Watermark)
		assert.EqualValues(t, 5, p.DeclaredTotals.RowCount)
		assert.Equal(t, map[string]string{"USD": "525.5976"}, p.DeclaredTotals.Totals)
	}
	assert.Equal(t, "525.5976", sum.FloatString(4), "declared total equals the exact sum of the records")
	assert.Equal(t, "", pages[2].NextCursor)
	assert.Equal(t, "RUNNING", recs[0].Attributes["run_status"])

	// The watermark is a whole-set digest: a new row for the same scope changes it.
	seedContribution(t, pool, tenant, run, "child-a", "acct-z", "1.0000")
	_, after := fetchAllContrib(t, s, q)
	assert.NotEqual(t, pages[0].Watermark, after[0].Watermark)
	assert.EqualValues(t, 6, after[0].DeclaredTotals.RowCount)
}

func TestBalanceContributions_TenantIsolationAndUnknownRun(t *testing.T) {
	pool := popPool(t)
	s := store.New(pool)
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	run := seedRun(t, pool, tenantA, "USD", "COMPLETED")
	seedContribution(t, pool, tenantA, run, "child-a", "1000", "10.0000")

	// Tenant B naming tenant A's run: not found, and no rows.
	_, err := s.BalanceContributionsPopulation(context.Background(),
		domain.BalanceContributionsQuery{TenantID: tenantB, RunID: run, LegalEntityID: "child-a", Limit: 10})
	assert.ErrorIs(t, err, domain.ErrRunNotFound)

	// A run that does not exist at all.
	_, err = s.BalanceContributionsPopulation(context.Background(),
		domain.BalanceContributionsQuery{TenantID: tenantA, RunID: uuid.NewString(), LegalEntityID: "child-a", Limit: 10})
	assert.ErrorIs(t, err, domain.ErrRunNotFound)

	// Even if a contribution row carried tenant B's id under tenant A's run id,
	// tenant A's read must not include it (explicit tenant_id predicate).
	seedContribution(t, pool, tenantB, run, "child-a", "leak", "99.0000")
	recs, pages := fetchAllContrib(t, s, domain.BalanceContributionsQuery{TenantID: tenantA, RunID: run, LegalEntityID: "child-a", Limit: 10})
	require.Len(t, recs, 1)
	assert.Equal(t, "1000", recs[0].Reference)
	assert.EqualValues(t, 1, pages[0].DeclaredTotals.RowCount)
}

func TestBalanceContributions_EmptyForEntityWithNoContributions(t *testing.T) {
	pool := popPool(t)
	s := store.New(pool)
	tenant := uuid.NewString()
	run := seedRun(t, pool, tenant, "USD", "COMPLETED")
	seedContribution(t, pool, tenant, run, "child-a", "1000", "10.0000")

	p, err := s.BalanceContributionsPopulation(context.Background(),
		domain.BalanceContributionsQuery{TenantID: tenant, RunID: run, LegalEntityID: "child-none", Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, p.Records)
	assert.NotNil(t, p.Records)
	assert.Equal(t, "", p.NextCursor)
	assert.EqualValues(t, 0, p.DeclaredTotals.RowCount)
	assert.Empty(t, p.DeclaredTotals.Totals)
	assert.Equal(t, "0:d41d8cd98f00b204e9800998ecf8427e", p.Watermark, "count 0 with the md5 of the empty string")
}
