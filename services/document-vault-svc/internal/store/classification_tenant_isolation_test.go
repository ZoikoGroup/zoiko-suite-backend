package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/domain"
	"zoiko.io/document-vault-svc/internal/store"
)

// BIZ-02 implementation gate: tenant isolation for classification. Tenant B
// must not be able to read, list or change tenant A's classification decisions,
// and nothing B attempts may change A's data. Runs as a NOSUPERUSER
// NOBYPASSRLS role so row level security is what is exercised.
func TestPgStore_Classification_TenantIsolation(t *testing.T) {
	admin := requireTestDB(t)
	app := appRolePool(t, admin)
	s := store.New(app, zap.NewNop())
	ctxA, ctxB := tenantCtx(), otherTenantCtx()

	doc, v1 := newDoc("cls-iso")
	require.NoError(t, s.CreateDocument(ctxA, doc, v1, "corr-cls-1"))
	proposal, err := s.ClassifyRecord(ctxA, domain.ClassifyRecordParams{
		DocumentID: doc.DocumentID, ClassificationValue: domain.ClassificationConfidential,
		Source: domain.ClassificationSourceHuman, ProposedByPrincipalID: "maker-1", CorrelationID: "corr-cls-2",
	})
	require.NoError(t, err)
	confirmed, err := s.ConfirmClassification(ctxA, domain.ConfirmClassificationParams{ClassificationID: proposal.ClassificationID, ConfirmedByPrincipalID: "checker-1"})
	require.NoError(t, err)
	require.Equal(t, domain.ClassificationStatusConfirmed, confirmed.Status)

	historyBefore, err := s.ListClassificationHistory(ctxA, doc.DocumentID)
	require.NoError(t, err)

	// Reads: B sees nothing of A's.
	_, err = s.FindClassificationByID(ctxB, proposal.ClassificationID)
	require.ErrorIs(t, err, domain.ErrClassificationNotFound)
	_, err = s.GetClassification(ctxB, doc.DocumentID)
	require.Error(t, err, "GetClassification of another tenant's document")
	_, err = s.GetAsOfClassification(ctxB, doc.DocumentID, time.Now().Add(time.Hour))
	require.Error(t, err, "GetAsOfClassification of another tenant's document")
	history, err := s.ListClassificationHistory(ctxB, doc.DocumentID)
	if err == nil {
		require.Empty(t, history)
	}
	_, err = s.ExplainPolicyMapping(ctxB, proposal.ClassificationID)
	require.ErrorIs(t, err, domain.ErrClassificationNotFound)
	unclassified, err := s.ListUnclassified(ctxB, doc.LegalEntityID, 50, 0)
	require.NoError(t, err)
	for _, d := range unclassified {
		require.NotEqual(t, doc.DocumentID, d.DocumentID, "B must not list A's documents")
	}

	// Writes: B cannot classify, confirm or reclassify A's document.
	_, err = s.ClassifyRecord(ctxB, domain.ClassifyRecordParams{
		DocumentID: doc.DocumentID, ClassificationValue: domain.ClassificationPublic,
		Source: domain.ClassificationSourceHuman, ProposedByPrincipalID: "attacker", CorrelationID: "corr-cls-b1",
	})
	require.Error(t, err, "ClassifyRecord on another tenant's document")
	_, err = s.Reclassify(ctxB, domain.ReclassifyParams{
		DocumentID: doc.DocumentID, ClassificationValue: domain.ClassificationPublic,
		Source: domain.ClassificationSourceHuman, ProposedByPrincipalID: "attacker", CorrelationID: "corr-cls-b2",
	})
	require.Error(t, err, "Reclassify on another tenant's document")
	_, err = s.ConfirmClassification(ctxB, domain.ConfirmClassificationParams{ClassificationID: proposal.ClassificationID, ConfirmedByPrincipalID: "attacker"})
	require.Error(t, err, "ConfirmClassification on another tenant's classification")

	// A's data is unchanged by everything B attempted.
	cur, err := s.GetClassification(ctxA, doc.DocumentID)
	require.NoError(t, err)
	require.Equal(t, domain.ClassificationConfidential, cur.ClassificationValue)
	require.Equal(t, domain.ClassificationStatusConfirmed, cur.Status)
	aHistory, err := s.ListClassificationHistory(ctxA, doc.DocumentID)
	require.NoError(t, err)
	require.Len(t, aHistory, len(historyBefore), "B's attempts must not have added classification history to A's document")

	// RLS itself, bypassing the store's own tenant predicates.
	ctx := context.Background()
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, otherTenantID)
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM record_classifications`).Scan(&n))
	require.Zero(t, n, "RLS leak: tenant B context sees %d rows in record_classifications", n)
}
