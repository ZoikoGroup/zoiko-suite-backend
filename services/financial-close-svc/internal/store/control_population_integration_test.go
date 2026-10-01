//go:build integration

package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-close-svc/internal/domain"
)

type tieFixture struct{ tenant, entity, batch string }

// newTieBatch inserts a batch (and crosswalk lines [debit, credit]) directly.
func newTieBatch(t *testing.T, tenant string, expDebits, expCredits string, lines [][2]string) tieFixture {
	t.Helper()
	ctx := context.Background()
	f := tieFixture{tenant: tenant, entity: "le-" + uuid.NewString()[:8], batch: uuid.NewString()}
	_, err := testPool.Exec(ctx, `
		INSERT INTO migration_batches (batch_id, tenant_id, legal_entity_id, fiscal_period, source_system_name,
			source_extract_hash, expected_row_count, expected_total_debits, expected_total_credits,
			status, created_at, created_by_principal_id)
		VALUES ($1,$2,$3,'2026-09','legacy','hash-1',$4,$5::numeric,$6::numeric,'LOADED','2026-09-01T23:30:00-05:00','p')`,
		f.batch, tenant, f.entity, len(lines), expDebits, expCredits)
	require.NoError(t, err)
	for i, l := range lines {
		_, err := testPool.Exec(ctx, `
			INSERT INTO migration_crosswalk_entries (entry_id, tenant_id, batch_id, source_reference_id,
				source_account_code, target_account_code, debit_amount, credit_amount)
			VALUES ($1,$2,$3,$4,'S','T',$5::numeric,$6::numeric)`,
			uuid.NewString(), tenant, f.batch, fmt.Sprintf("src-%d", i), l[0], l[1])
		require.NoError(t, err)
	}
	return f
}

func tie(t *testing.T, f tieFixture, tenant string) (*domain.ControlPopulationPage, error) {
	t.Helper()
	return testStore.MigrationBatchTieout(context.Background(), domain.MigrationBatchTieoutQuery{
		TenantID: tenant, LegalEntityID: f.entity, BatchID: f.batch, Limit: 1000,
	})
}

func TestMigrationBatchTieout_TiesExactly(t *testing.T) {
	tenant := uuid.NewString()
	f := newTieBatch(t, tenant, "300.00", "300.00", [][2]string{{"100.00", "0"}, {"200.00", "0"}, {"0", "300.00"}})
	page, err := tie(t, f, tenant)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	r := page.Records[0]
	assert.Equal(t, f.batch, r.RecordID)
	assert.Equal(t, f.batch, r.Reference)
	assert.Equal(t, "300.00", r.Amount)
	assert.Equal(t, "XXX", r.Currency)
	assert.Equal(t, "2026-09-02", r.Date, "UTC date of 2026-09-01T23:30-05:00")
	assert.Equal(t, "300.00", r.Attributes["crosswalk_debits"])
	assert.Equal(t, "300.00", r.Attributes["expected_total_debits"])
	assert.Equal(t, "300.00", r.Attributes["crosswalk_credits"])
	assert.Equal(t, "3", r.Attributes["crosswalk_row_count"])
	assert.Equal(t, "3", r.Attributes["expected_row_count"])
	assert.Equal(t, "LOADED", r.Attributes["batch_status"])
	assert.Equal(t, "hash-1", r.Attributes["source_extract_hash"])
	assert.Equal(t, "", r.Attributes["journal_id"])
	assert.Equal(t, map[string]string{"XXX": "300.00"}, page.DeclaredTotals.Totals)
	assert.EqualValues(t, 1, page.DeclaredTotals.RowCount)
	assert.Regexp(t, `^fc1:n=1;md5=[0-9a-f]{32}$`, page.Watermark)

	again, err := tie(t, f, tenant)
	require.NoError(t, err)
	assert.Equal(t, page.Watermark, again.Watermark, "unchanged data, unchanged watermark")
}

func TestMigrationBatchTieout_DebitsDiffer(t *testing.T) {
	tenant := uuid.NewString()
	f := newTieBatch(t, tenant, "300.00", "300.00", [][2]string{{"100.00", "0"}, {"199.99", "0"}, {"0", "300.00"}})
	page, err := tie(t, f, tenant)
	require.NoError(t, err)
	a := page.Records[0].Attributes
	assert.Equal(t, "300.00", a["expected_total_debits"])
	assert.Equal(t, "299.99", a["crosswalk_debits"])
}

func TestMigrationBatchTieout_EmptyCrosswalkSumsAreZeroNotNull(t *testing.T) {
	tenant := uuid.NewString()
	f := newTieBatch(t, tenant, "50.00", "50.00", nil)
	page, err := tie(t, f, tenant)
	require.NoError(t, err)
	a := page.Records[0].Attributes
	assert.Equal(t, "0.00", a["crosswalk_debits"])
	assert.Equal(t, "0.00", a["crosswalk_credits"])
	assert.Equal(t, "0", a["crosswalk_row_count"])
}

func TestMigrationBatchTieout_TenantIsolation(t *testing.T) {
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	f := newTieBatch(t, tenantA, "10.00", "10.00", [][2]string{{"10.00", "0"}})
	_, err := tie(t, f, tenantB)
	assert.ErrorIs(t, err, domain.ErrMigrationBatchNotFound)
	_, err = tie(t, tieFixture{entity: "le-other", batch: f.batch}, tenantA)
	assert.ErrorIs(t, err, domain.ErrMigrationBatchNotFound, "wrong legal entity")
	_, err = tie(t, tieFixture{entity: f.entity, batch: uuid.NewString()}, tenantA)
	assert.ErrorIs(t, err, domain.ErrMigrationBatchNotFound, "unknown batch")
}

func TestMigrationBatchTieout_DecimalExactness(t *testing.T) {
	tenant := uuid.NewString()
	f := newTieBatch(t, tenant, "0.30", "0.30", [][2]string{{"0.10", "0"}, {"0.20", "0"}, {"0", "0.30"}})
	page, err := tie(t, f, tenant)
	require.NoError(t, err)
	a := page.Records[0].Attributes
	assert.Equal(t, "0.30", a["crosswalk_debits"], "0.1+0.2 must be exactly 0.30")
	assert.Equal(t, a["expected_total_debits"], a["crosswalk_debits"])
}

func TestMigrationBatchTieout_CursorPastRecord(t *testing.T) {
	tenant := uuid.NewString()
	f := newTieBatch(t, tenant, "1.00", "1.00", nil)
	page, err := testStore.MigrationBatchTieout(context.Background(), domain.MigrationBatchTieoutQuery{
		TenantID: tenant, LegalEntityID: f.entity, BatchID: f.batch, Limit: 10, AfterRecordID: f.batch,
	})
	require.NoError(t, err)
	assert.Empty(t, page.Records)
	assert.Equal(t, "", page.NextCursor)
	assert.EqualValues(t, 1, page.DeclaredTotals.RowCount)
}
