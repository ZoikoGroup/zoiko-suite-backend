//go:build integration

package store_test

// A2b part 2's many-to-one match path against the real database:
// statement_line_matched_journals (migration 000015), its append-only
// trigger, and the down migration's guard. The handler's own ledger
// verification (journals really exist, FINALIZED, correct entity, net sum)
// is covered by internal/handler/multi_journal_match_test.go against a
// stub ledger; here allocations are supplied directly, already "verified",
// the same separation bnk05_store_test.go's single-journal tests already
// use.
//
// TestMigration000015DownGuard runs FIRST in this file on purpose: a
// MATCHED row's allocation is permanent (the trigger refuses to delete it —
// see TestStatementLineMatchedJournals_AppendOnlyOnceConfirmed below), so
// proving the down migration succeeds "when nothing is confirmed yet" only
// works before any other test in this file confirms a multi-journal match.
// Go runs a file's top-level tests in declaration order, so this ordering
// is what makes that proof possible in this package's one shared database.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

// readMigrationFile reads one migration file verbatim, never a hand-copied
// inline string, so the down-guard test exercises exactly what ships.
func readMigrationFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deployments", "migrations", name))
	require.NoError(t, err)
	return string(b)
}

func twoAllocations(total int64) []domain.JournalAllocation {
	a, b := total/2, total-total/2
	return []domain.JournalAllocation{
		{JournalID: uuid.New().String(), NetCents: a},
		{JournalID: uuid.New().String(), NetCents: b},
	}
}

// TestMigration000015DownGuard proves the down migration succeeds while no
// row belongs to a CONFIRMED multi-journal match, and refuses once one
// exists. See the file-level doc comment for why "succeeds" must run first.
func TestMigration000015DownGuard(t *testing.T) {
	ctx := context.Background()
	downSQL := readMigrationFile(t, "000015_statement_line_matched_journals.down.sql")
	upSQL := readMigrationFile(t, "000015_statement_line_matched_journals.up.sql")

	t.Run("succeeds when every allocation row belongs to a non-MATCHED line", func(t *testing.T) {
		f := setupIsolationFixture(t, "down-guard-pending")
		tctx := svcmiddleware.WithTenant(ctx, f.tenantID)
		require.NoError(t, testStore.ProposeMultiMatch(tctx, f.tenantID, f.statementLineID, twoAllocations(100000), "maker-1"))
		// Still PENDING_CONFIRMATION — never confirmed.

		_, err := testPool.Exec(ctx, downSQL)
		require.NoError(t, err, "the down migration must succeed while no allocation row belongs to a CONFIRMED match")

		var exists bool
		require.NoError(t, testPool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'statement_line_matched_journals')`).Scan(&exists))
		require.False(t, exists)

		// Restore the up shape before the next subtest (and every other test
		// in this file) runs.
		_, err = testPool.Exec(ctx, upSQL)
		require.NoError(t, err)
	})

	t.Run("refuses while a CONFIRMED multi-journal match exists", func(t *testing.T) {
		f := setupIsolationFixture(t, "down-guard-confirmed")
		tctx := svcmiddleware.WithTenant(ctx, f.tenantID)
		require.NoError(t, testStore.ProposeMultiMatch(tctx, f.tenantID, f.statementLineID, twoAllocations(100000), "maker-1"))
		_, err := testStore.ConfirmMatch(tctx, f.tenantID, f.statementLineID, "checker-1")
		require.NoError(t, err)

		_, err = testPool.Exec(ctx, downSQL)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot roll back 000015")
		// No restore needed: the guard raises before DROP TABLE ever runs,
		// so the table, its trigger and policy are untouched — re-running
		// upSQL here would fail on "relation already exists".
	})
}

// TestPgStore_ProposeMultiMatch_RecordsAllocationsLeavingScalarColumnNull
// proves the additive design: the scalar proposed_journal_id stays NULL for
// a multi-journal proposal, and every named journal is recorded instead.
func TestPgStore_ProposeMultiMatch_RecordsAllocationsLeavingScalarColumnNull(t *testing.T) {
	f := setupIsolationFixture(t, "multi-propose")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	allocations := twoAllocations(100000)

	require.NoError(t, testStore.ProposeMultiMatch(ctx, f.tenantID, f.statementLineID, allocations, "maker-1"))

	l, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusPendingConfirmation, l.Status)
	require.Nil(t, l.ProposedJournalID, "scalar column must stay NULL for a multi-journal proposal")
	require.Len(t, l.ProposedJournalIDs, 2)
	require.ElementsMatch(t, []string{allocations[0].JournalID, allocations[1].JournalID}, l.ProposedJournalIDs)
}

func TestPgStore_ProposeMultiMatch_RequiresAtLeastTwoAllocations(t *testing.T) {
	f := setupIsolationFixture(t, "multi-too-few")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)

	err := testStore.ProposeMultiMatch(ctx, f.tenantID, f.statementLineID, []domain.JournalAllocation{{JournalID: uuid.New().String(), NetCents: 1000}}, "maker-1")
	require.ErrorIs(t, err, domain.ErrInvalidJournalIDs)

	l, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusUnmatched, l.Status, "a refused proposal must not move the line")
}

// TestPgStore_ConfirmMatch_MultiJournal_PromotesToMatched proves the
// checker step works identically for the many-to-one path: a different
// principal confirms, the line reaches MATCHED, and MatchedJournalIDs
// carries the same allocation (matched_journal_id stays NULL).
func TestPgStore_ConfirmMatch_MultiJournal_PromotesToMatched(t *testing.T) {
	f := setupIsolationFixture(t, "multi-confirm")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	allocations := twoAllocations(100000)

	require.NoError(t, testStore.ProposeMultiMatch(ctx, f.tenantID, f.statementLineID, allocations, "maker-1"))

	confirmed, err := testStore.ConfirmMatch(ctx, f.tenantID, f.statementLineID, "checker-1")
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusMatched, confirmed.Status)
	require.Nil(t, confirmed.MatchedJournalID)
	require.ElementsMatch(t, []string{allocations[0].JournalID, allocations[1].JournalID}, confirmed.MatchedJournalIDs)

	// Re-read proves it persisted, not just the in-memory response.
	l, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{allocations[0].JournalID, allocations[1].JournalID}, l.MatchedJournalIDs)
}

func TestPgStore_ConfirmMatch_MultiJournal_RejectsSelfConfirmation(t *testing.T) {
	f := setupIsolationFixture(t, "multi-self-confirm")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	allocations := twoAllocations(100000)

	require.NoError(t, testStore.ProposeMultiMatch(ctx, f.tenantID, f.statementLineID, allocations, "maker-1"))

	_, err := testStore.ConfirmMatch(ctx, f.tenantID, f.statementLineID, "maker-1")
	require.ErrorIs(t, err, domain.ErrMatchSelfConfirmation)
}

// TestPgStore_RejectProposedMatch_MultiJournal_DeletesAllocationRows proves
// a withdrawn multi-journal proposal is actually removed from
// statement_line_matched_journals, not just unset on the Go side — the
// trigger permits this because the parent is EXCEPTION, not MATCHED.
func TestPgStore_RejectProposedMatch_MultiJournal_DeletesAllocationRows(t *testing.T) {
	f := setupIsolationFixture(t, "multi-reject")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	allocations := twoAllocations(100000)

	require.NoError(t, testStore.ProposeMultiMatch(ctx, f.tenantID, f.statementLineID, allocations, "maker-1"))
	require.NoError(t, testStore.RejectProposedMatch(ctx, f.tenantID, f.statementLineID, "wrong journals", "checker-1"))

	l, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusException, l.Status)
	require.Empty(t, l.ProposedJournalIDs)

	var remaining int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT COUNT(*) FROM statement_line_matched_journals WHERE statement_line_id = $1`, f.statementLineID).Scan(&remaining))
	require.Equal(t, 0, remaining, "rejected allocation rows must actually be deleted, not merely unlinked")

	// A fresh proposal on the now-EXCEPTION line must still succeed.
	require.NoError(t, testStore.ProposeMultiMatch(ctx, f.tenantID, f.statementLineID, twoAllocations(100000), "maker-2"))
}

// TestStatementLineMatchedJournals_AppendOnlyOnceConfirmed proves migration
// 000015's trigger: a row belonging to a CONFIRMED (MATCHED) line can never
// be updated or deleted, the same posture reconciliation_certificates
// already has — evidence of an established fact is not editable.
func TestStatementLineMatchedJournals_AppendOnlyOnceConfirmed(t *testing.T) {
	f := setupIsolationFixture(t, "multi-immutable")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	allocations := twoAllocations(100000)

	require.NoError(t, testStore.ProposeMultiMatch(ctx, f.tenantID, f.statementLineID, allocations, "maker-1"))
	_, err := testStore.ConfirmMatch(ctx, f.tenantID, f.statementLineID, "checker-1")
	require.NoError(t, err)

	_, err = testPool.Exec(ctx, `UPDATE statement_line_matched_journals SET net_amount_cents = 1 WHERE statement_line_id = $1`, f.statementLineID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "never updated")

	_, err = testPool.Exec(ctx, `DELETE FROM statement_line_matched_journals WHERE statement_line_id = $1`, f.statementLineID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "confirmed match")
}
