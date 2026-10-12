//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

// seedRun inserts a reconciliation run directly: these tests are about the
// period-status query, not the workflow that produces runs.
func seedRun(t *testing.T, tenant, entity, account, statementDate, status string, certifiedAt *time.Time, supersededBy *string) string {
	t.Helper()
	id := uuid.NewString()
	var certifier *string
	if certifiedAt != nil {
		c := "certifier-1"
		certifier = &c
	}
	_, err := testPool.Exec(context.Background(), `
		INSERT INTO reconciliation_runs (run_id, tenant_id, legal_entity_id, bank_account_id, statement_date,
		    status, certified_by_principal_id, certified_at, superseded_by_run_id, created_by_principal_id)
		VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8, $9, 'preparer-1')`,
		id, tenant, entity, account, statementDate, status, certifier, certifiedAt, supersededBy)
	require.NoError(t, err)
	return id
}

func TestPeriodReconciliationStatus(t *testing.T) {
	tenant, entity := uuid.NewString(), uuid.NewString()
	barclays, hsbc, lloyds, santander := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := func(d string) *time.Time { v, _ := time.Parse("2006-01-02", d); v = v.Add(12 * time.Hour); return &v }

	// Barclays: certified for 30 Oct — the proof.
	seedRun(t, tenant, entity, barclays, "2026-10-23", "CERTIFIED", at("2026-10-24"), nil)
	bar30 := seedRun(t, tenant, entity, barclays, "2026-10-30", "CERTIFIED", at("2026-11-02"), nil)

	// HSBC: certified for 23 Oct, but the 30 Oct run is stuck with exceptions.
	hsbc23 := seedRun(t, tenant, entity, hsbc, "2026-10-23", "CERTIFIED", at("2026-10-24"), nil)
	hsbc30 := seedRun(t, tenant, entity, hsbc, "2026-10-30", "EXCEPTIONS_OPEN", nil, nil)

	// Lloyds: the 30 Oct certification was superseded by a re-performed run.
	lloydsNew := seedRun(t, tenant, entity, lloyds, "2026-10-30", "CERTIFIED", at("2026-11-03"), nil)
	seedRun(t, tenant, entity, lloyds, "2026-10-30", "SUPERSEDED", at("2026-11-01"), &lloydsNew)

	// Santander: only a November statement — outside the period.
	seedRun(t, tenant, entity, santander, "2026-11-03", "CERTIFIED", at("2026-11-04"), nil)

	// Noise that must never appear: another entity, another tenant.
	seedRun(t, tenant, uuid.NewString(), barclays, "2026-10-30", "CERTIFIED", at("2026-11-02"), nil)
	seedRun(t, uuid.NewString(), entity, barclays, "2026-10-31", "CERTIFIED", at("2026-11-02"), nil)

	got, err := testStore.PeriodReconciliationStatus(context.Background(), tenant, entity, "2026-10-01", "2026-10-31")
	require.NoError(t, err)

	byAccount := map[string]domain.AccountReconciliationStatus{}
	for _, a := range got {
		byAccount[a.BankAccountID] = a
	}
	require.Len(t, byAccount, 3, "only accounts with an in-period run, for this tenant and entity")

	require.Equal(t, bar30, byAccount[barclays].LatestCertified.RunID, "latest certified statement wins")
	require.Equal(t, bar30, byAccount[barclays].LatestRun.RunID)

	require.Equal(t, hsbc23, byAccount[hsbc].LatestCertified.RunID)
	require.Equal(t, "2026-10-23", byAccount[hsbc].LatestCertified.StatementDate)
	require.Equal(t, hsbc30, byAccount[hsbc].LatestRun.RunID, "the newer unfinished run explains the gap")
	require.Equal(t, "EXCEPTIONS_OPEN", byAccount[hsbc].LatestRun.Status)

	require.Equal(t, lloydsNew, byAccount[lloyds].LatestCertified.RunID, "a superseded certification is not proof")
	require.Equal(t, lloydsNew, byAccount[lloyds].LatestRun.RunID, "SUPERSEDED runs are never the latest attempt")

	_, ok := byAccount[santander]
	require.False(t, ok, "a November statement says nothing about October")
}
