//go:build integration

package store_test

// ListStatementLines' journal_ids filter (reconciliation-intelligence-svc's
// A2b part 3): this service is the only one that knows a journal is already
// somebody's matched or proposed counterpart, scalar (single-journal) or
// recorded in statement_line_matched_journals (A2b part 2's many-to-one
// path). These tests exercise the real SQL against Postgres, not a stub.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

func TestPgStore_ListStatementLines_JournalIDsFilter_FindsScalarMatchedJournal(t *testing.T) {
	f := setupIsolationFixture(t, "journal-lookup-matched")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	journalID := uuid.New().String()
	require.NoError(t, testStore.MatchStatementLine(ctx, f.tenantID, f.statementLineID, journalID, "actor-1"))

	// A journal nobody has touched must not come back alongside it.
	unrelated := uuid.New().String()
	got, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{
		TenantID:   f.tenantID,
		JournalIDs: []string{journalID, unrelated},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, f.statementLineID, got[0].StatementLineID)
	require.Equal(t, domain.StatementLineStatusMatched, got[0].Status)
	require.NotNil(t, got[0].MatchedJournalID)
	require.Equal(t, journalID, *got[0].MatchedJournalID)
}

func TestPgStore_ListStatementLines_JournalIDsFilter_FindsScalarProposedJournal(t *testing.T) {
	f := setupIsolationFixture(t, "journal-lookup-proposed")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	journalID := uuid.New().String()
	require.NoError(t, testStore.ProposeMatch(ctx, f.tenantID, f.statementLineID, journalID, "maker-1"))

	got, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{
		TenantID:   f.tenantID,
		JournalIDs: []string{journalID},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, domain.StatementLineStatusPendingConfirmation, got[0].Status)
	require.NotNil(t, got[0].ProposedJournalID)
	require.Equal(t, journalID, *got[0].ProposedJournalID)
}

// A multi-journal proposal (A2b part 2) has no scalar proposed_journal_id —
// it must still surface through statement_line_matched_journals, and the
// filter's caller must be able to see WHICH of its candidate ids the match
// recorded, via ProposedJournalIDs, not just that the line exists.
func TestPgStore_ListStatementLines_JournalIDsFilter_FindsMultiJournalAllocation(t *testing.T) {
	f := setupIsolationFixture(t, "journal-lookup-multi")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	allocations := twoAllocations(100000)
	require.NoError(t, testStore.ProposeMultiMatch(ctx, f.tenantID, f.statementLineID, allocations, "maker-1"))

	// Filtering by only ONE of the two allocated journals must still find
	// the line — a many-to-one match is claimed as a whole, not per journal.
	got, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{
		TenantID:   f.tenantID,
		JournalIDs: []string{allocations[0].JournalID},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Nil(t, got[0].ProposedJournalID, "scalar column stays NULL for a multi-journal proposal")
	require.ElementsMatch(t, []string{allocations[0].JournalID, allocations[1].JournalID}, got[0].ProposedJournalIDs)
}

func TestPgStore_ListStatementLines_JournalIDsFilter_NoMatchIsEmptyNotError(t *testing.T) {
	f := setupIsolationFixture(t, "journal-lookup-none")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)

	got, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{
		TenantID:   f.tenantID,
		JournalIDs: []string{uuid.New().String()},
	})
	require.NoError(t, err)
	require.Empty(t, got)
}

// The journal filter ANDs with the others, exactly like bank_account_id,
// statement_date and status AND with each other today — it is not a special
// case that bypasses them. The expected caller (reconciliation-intelligence-svc)
// asks with journal_ids alone, but a caller that also narrows by status gets
// the intersection, not the journal match alone.
func TestPgStore_ListStatementLines_JournalIDsFilter_CombinesWithStatusFilter(t *testing.T) {
	f := setupIsolationFixture(t, "journal-lookup-combines-status")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	journalID := uuid.New().String()
	require.NoError(t, testStore.MatchStatementLine(ctx, f.tenantID, f.statementLineID, journalID, "actor-1"))

	// The line is MATCHED now: a status filter asking for UNMATCHED must
	// exclude it even though the journal itself matches.
	none, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{
		TenantID:   f.tenantID,
		Status:     string(domain.StatementLineStatusUnmatched),
		JournalIDs: []string{journalID},
	})
	require.NoError(t, err)
	require.Empty(t, none, "a non-matching status filter must still exclude the line")

	// The matching status finds it, same journal filter.
	got, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{
		TenantID:   f.tenantID,
		Status:     string(domain.StatementLineStatusMatched),
		JournalIDs: []string{journalID},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestPgStore_ListStatementLines_JournalIDsFilter_RefusesTooManyIDs(t *testing.T) {
	f := setupIsolationFixture(t, "journal-lookup-too-many")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)

	ids := make([]string, domain.MaxJournalLookupIDs+1)
	for i := range ids {
		ids[i] = uuid.New().String()
	}
	_, err := testStore.ListStatementLines(ctx, domain.ListStatementLinesFilter{
		TenantID:   f.tenantID,
		JournalIDs: ids,
	})
	require.ErrorIs(t, err, domain.ErrTooManyJournalIDs)
}
