//go:build integration

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/domain"
	svcmiddleware "zoiko.io/consolidation-svc/internal/middleware"
	"zoiko.io/consolidation-svc/internal/store"
)

func eliminationRows(tenantID, runID string) []domain.Elimination {
	tgt := "j-uk-1"
	reason := "target journal j-uk-2 could not be read: general-ledger-svc unavailable"
	now := time.Now().UTC().Truncate(time.Microsecond)
	base := func(id string) domain.Elimination {
		return domain.Elimination{
			EliminationID: uuid.NewString(), TenantID: tenantID, ConsolidationRunID: runID,
			IntercompanyEntryID: id, SourceLegalEntityID: "le-us", TargetLegalEntityID: "le-uk",
			FiscalPeriod: "2026-07", PeriodBasis: domain.PeriodBasisPair, MatchStatus: "MATCHED",
			SourceJournalID: "j-us-1", TargetJournalID: &tgt, Amount: "500.1", CurrencyCode: "USD",
			CounterpartyAmount: "400", CounterpartyCurrencyCode: "GBP",
			IntercompanyAccountCodes: []string{"1500-IC-REC", "2500-IC-PAY"}, CreatedAt: now,
		}
	}
	ok := base("ic-1")
	ok.Outcome = domain.EliminationOutcomeEliminated
	ok.Lines = []domain.EliminatedLine{
		{Side: domain.EliminationSideSource, JournalID: "j-us-1", LineNumber: 1, AccountCode: "1500-IC-REC", DebitAmount: "500.1", CreditAmount: "0"},
		{Side: domain.EliminationSideTarget, JournalID: "j-uk-1", LineNumber: 2, AccountCode: "2500-IC-PAY", DebitAmount: "0", CreditAmount: "400"},
	}
	blocked := base("ic-2")
	blocked.PeriodBasis = domain.PeriodBasisSourceJournal
	blocked.MatchStatus = "CLOSED"
	blocked.Outcome = domain.EliminationOutcomeBlocked
	blocked.BlockedReason = &reason
	return []domain.Elimination{ok, blocked}
}

// A run's elimination rows are written together and read back exactly as
// written, under the app role (row-level security applies), and never
// across tenants.
func TestPgStore_Eliminations_RoundTrip_RealDB(t *testing.T) {
	super := openTestPool(t)
	s := store.New(appPool(t, super), zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	run := newTestRun(tenantID, "le-group")
	require.NoError(t, createRun(s, ctx, run, "principal-1", "corr-1"))

	rows := eliminationRows(tenantID, run.ConsolidationRunID)
	require.NoError(t, s.CreateEliminations(ctx, rows))

	got, err := s.ListEliminationsByRun(ctx, run.ConsolidationRunID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "ic-1", got[0].IntercompanyEntryID)
	require.Equal(t, domain.EliminationOutcomeEliminated, got[0].Outcome)
	require.Equal(t, domain.Decimal("500.1"), got[0].Amount)
	require.Equal(t, domain.Decimal("400"), got[0].CounterpartyAmount)
	require.Equal(t, []string{"1500-IC-REC", "2500-IC-PAY"}, got[0].IntercompanyAccountCodes)
	require.Len(t, got[0].Lines, 2)
	require.Equal(t, rows[0].Lines[1].AccountCode, got[0].Lines[1].AccountCode)
	require.Equal(t, domain.Decimal("400"), got[0].Lines[1].CreditAmount)
	require.Equal(t, domain.EliminationOutcomeBlocked, got[1].Outcome)
	require.Equal(t, domain.PeriodBasisSourceJournal, got[1].PeriodBasis)
	require.Equal(t, *rows[1].BlockedReason, *got[1].BlockedReason)
	require.Empty(t, got[1].Lines)

	other := svcmiddleware.WithTenant(context.Background(), uuid.NewString())
	none, err := s.ListEliminationsByRun(other, run.ConsolidationRunID)
	require.NoError(t, err)
	require.Empty(t, none)
}

// One row per (run, pair), all or none: a duplicate pair rolls back the
// whole run's batch.
func TestPgStore_Eliminations_AllOrNone_RealDB(t *testing.T) {
	s := newTestStore(openTestPool(t))
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	run := newTestRun(tenantID, "le-group")
	require.NoError(t, createRun(s, ctx, run, "principal-1", "corr-1"))

	rows := eliminationRows(tenantID, run.ConsolidationRunID)
	rows[1].IntercompanyEntryID = rows[0].IntercompanyEntryID
	require.Error(t, s.CreateEliminations(ctx, rows))
	got, err := s.ListEliminationsByRun(ctx, run.ConsolidationRunID)
	require.NoError(t, err)
	require.Empty(t, got)
}

// The table refuses rows that contradict their outcome, and never changes a
// row once written.
func TestPgStore_Eliminations_ConstraintsAndAppendOnly_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool)
	tenantID := uuid.NewString()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	run := newTestRun(tenantID, "le-group")
	require.NoError(t, createRun(s, ctx, run, "principal-1", "corr-1"))

	for name, mutate := range map[string]func(e *domain.Elimination){
		"eliminated with no line":  func(e *domain.Elimination) { e.Lines = nil },
		"eliminated with a reason": func(e *domain.Elimination) { r := "x"; e.BlockedReason = &r },
		"eliminated, period undetermined": func(e *domain.Elimination) {
			e.PeriodBasis = domain.PeriodBasisUndetermined
		},
		"blocked with no reason": func(e *domain.Elimination) {
			e.Outcome = domain.EliminationOutcomeBlocked
			e.Lines = nil
		},
		"blocked with lines": func(e *domain.Elimination) {
			r := "x"
			e.Outcome, e.BlockedReason = domain.EliminationOutcomeBlocked, &r
		},
		"an ineligible status": func(e *domain.Elimination) { e.MatchStatus = "MISMATCH" },
	} {
		e := eliminationRows(tenantID, run.ConsolidationRunID)[0]
		mutate(&e)
		err := s.CreateEliminations(ctx, []domain.Elimination{e})
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "chk_consolidation_elimination", name)
	}

	require.NoError(t, s.CreateEliminations(ctx, eliminationRows(tenantID, run.ConsolidationRunID)))
	for _, stmt := range []string{
		`UPDATE consolidation_eliminations SET outcome = 'BLOCKED' WHERE tenant_id = $1`,
		`DELETE FROM consolidation_eliminations WHERE tenant_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, tenantID)
		require.Error(t, err)
		require.Contains(t, err.Error(), "append-only")
	}
}

// 000007's down refuses while any elimination is recorded, and round-trips
// on an empty table.
func TestMigration000007_Down_RealDB(t *testing.T) {
	pool := openTestPool(t)
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	ctx := context.Background()

	_, err := pool.Exec(ctx, read("000007_add_consolidation_eliminations.down.sql"))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, read("000007_add_consolidation_eliminations.up.sql"))
	require.NoError(t, err)

	s := newTestStore(pool)
	tenantID := uuid.NewString()
	tctx := svcmiddleware.WithTenant(ctx, tenantID)
	run := newTestRun(tenantID, "le-group")
	require.NoError(t, createRun(s, tctx, run, "principal-1", "corr-1"))
	require.NoError(t, s.CreateEliminations(tctx, eliminationRows(tenantID, run.ConsolidationRunID)))

	_, err = pool.Exec(ctx, read("000007_add_consolidation_eliminations.down.sql"))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "cannot roll back 000007"), err.Error())
	got, err := s.ListEliminationsByRun(tctx, run.ConsolidationRunID)
	require.NoError(t, err)
	require.Len(t, got, 2)
}
