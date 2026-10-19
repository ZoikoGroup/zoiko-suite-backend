package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

// Real-PostgreSQL proof that BulkClassifyRecords is all-or-nothing, including
// the classification.proposed outbox events written in the same transaction.
// Every count is read through the pool on a separate connection, so only
// COMMITTED state is visible to it.

func classificationRows(t *testing.T, pool *pgxpool.Pool, docID string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM record_classifications WHERE document_id = $1`, docID).Scan(&n))
	return n
}

func proposedEvents(t *testing.T, pool *pgxpool.Pool, docID string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'classification.proposed'`, docID).Scan(&n))
	return n
}

type snapshot struct{ rows, events int }

func snap(t *testing.T, pool *pgxpool.Pool, ids ...string) map[string]snapshot {
	t.Helper()
	m := map[string]snapshot{}
	for _, id := range ids {
		m[id] = snapshot{classificationRows(t, pool, id), proposedEvents(t, pool, id)}
	}
	return m
}

func bulkParams(docIDs ...string) []domain.ClassifyRecordParams {
	ps := make([]domain.ClassifyRecordParams, 0, len(docIDs))
	for _, id := range docIDs {
		ps = append(ps, domain.ClassifyRecordParams{
			DocumentID: id, ClassificationValue: domain.ClassificationRestricted, Source: domain.ClassificationSourceHuman,
			ProposedByPrincipalID: "bulk-proposer", CorrelationID: "corr-bulk-" + id,
		})
	}
	return ps
}

func TestBulkClassifyRecords_AllValid_CommitsEveryProposalAndEvent(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	a, b, c := newDocForDeclareTest(t, s, "bulk-ok-a"), newDocForDeclareTest(t, s, "bulk-ok-b"), newDocForDeclareTest(t, s, "bulk-ok-c")
	ids := []string{a.DocumentID, b.DocumentID, c.DocumentID}
	before := snap(t, pool, ids...)

	out, err := s.BulkClassifyRecords(tenantCtx(), bulkParams(ids...))
	require.NoError(t, err)
	require.Len(t, out, 3)
	for i, id := range ids {
		assert.Equal(t, id, out[i].DocumentID, "results keep the request order")
		assert.Equal(t, domain.ClassificationStatusCandidate, out[i].Status)
		assert.Equal(t, before[id].rows+1, classificationRows(t, pool, id))
		assert.Equal(t, before[id].events+1, proposedEvents(t, pool, id), "one classification.proposed event per committed proposal")
	}
}

func TestBulkClassifyRecords_FailureAtFirstMiddleOrLast_LeavesNoRowsAndNoEvents(t *testing.T) {
	for name, position := range map[string]int{"first": 0, "middle": 1, "last": 2} {
		t.Run(name, func(t *testing.T) {
			pool := requireTestDB(t)
			s := store.New(pool, zap.NewNop())
			a, b := newDocForDeclareTest(t, s, "bulk-pos-a-"+name), newDocForDeclareTest(t, s, "bulk-pos-b-"+name)
			missing := uuid.NewString()
			ids := []string{a.DocumentID, b.DocumentID}
			batch := append(append([]string{}, ids[:position]...), append([]string{missing}, ids[position:]...)...)
			before := snap(t, pool, ids...)

			out, err := s.BulkClassifyRecords(tenantCtx(), bulkParams(batch...))
			require.Error(t, err)
			assert.Nil(t, out)
			require.ErrorIs(t, err, domain.ErrDocumentNotFound)
			var be *domain.BulkClassifyError
			require.True(t, errors.As(err, &be))
			assert.Equal(t, missing, be.DocumentID)

			assert.Equal(t, before, snap(t, pool, ids...), "every earlier proposal and its event must have rolled back")
		})
	}
}

// The failure is a business rule seen only inside the transaction, after earlier
// proposals were already inserted.
func TestBulkClassifyRecords_RuleFailureMidBatch_RollsBackEarlierProposals(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	a, b, c := newDocForDeclareTest(t, s, "bulk-rule-a"), newDocForDeclareTest(t, s, "bulk-rule-b"), newDocForDeclareTest(t, s, "bulk-rule-c")
	seeded, err := s.GetClassification(tenantCtx(), b.DocumentID)
	require.NoError(t, err)
	_, err = s.ConfirmClassification(tenantCtx(), domain.ConfirmClassificationParams{
		ClassificationID: seeded.ClassificationID, ConfirmedByPrincipalID: "steward-1",
	})
	require.NoError(t, err, "b now has a confirmed classification, so proposing again is refused")
	ids := []string{a.DocumentID, b.DocumentID, c.DocumentID}
	before := snap(t, pool, ids...)

	_, err = s.BulkClassifyRecords(tenantCtx(), bulkParams(ids...))
	require.ErrorIs(t, err, domain.ErrClassificationNotCandidate)
	var be *domain.BulkClassifyError
	require.True(t, errors.As(err, &be))
	assert.Equal(t, b.DocumentID, be.DocumentID)

	assert.Equal(t, before, snap(t, pool, ids...), "a (processed before the failure) and c (never reached) are both unchanged")
}

func TestBulkClassifyRecords_InvalidProposal_WritesNothing(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	a, b := newDocForDeclareTest(t, s, "bulk-inv-a"), newDocForDeclareTest(t, s, "bulk-inv-b")
	ids := []string{a.DocumentID, b.DocumentID}
	before := snap(t, pool, ids...)

	ps := bulkParams(ids...)
	for i := range ps {
		ps[i].Source = domain.ClassificationSourceAI // AI proposals require a confidence
	}
	_, err := s.BulkClassifyRecords(tenantCtx(), ps)
	require.ErrorIs(t, err, domain.ErrAIConfidenceRequired)
	assert.Equal(t, before, snap(t, pool, ids...))
}

// ClassifyRecord (single) shares the same transactional body and is unchanged.
func TestClassifyRecord_Single_StillWritesProposalAndEvent(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	a := newDocForDeclareTest(t, s, "single-classify")
	before := snap(t, pool, a.DocumentID)

	c, err := s.ClassifyRecord(tenantCtx(), bulkParams(a.DocumentID)[0])
	require.NoError(t, err)
	assert.Equal(t, domain.ClassificationStatusCandidate, c.Status)
	assert.Equal(t, before[a.DocumentID].rows+1, classificationRows(t, pool, a.DocumentID))
	assert.Equal(t, before[a.DocumentID].events+1, proposedEvents(t, pool, a.DocumentID))
}
