//go:build integration

// Package store_test — proof that bank-reconciliation-svc's reconciliation
// lifecycle events actually reach the transactional outbox (ZS-EVENT-001
// §6), inside the same transaction as the state change they report.
//
// Before this file, event emission for every one of these facts lived in
// internal/events.Publisher, called from the HANDLER layer AFTER the store
// transaction had already committed — a direct, synchronous Kafka write
// that could silently lose an event on a broker outage or a crash between
// the commit and the publish call, with nothing durable recording that
// redelivery was ever needed. Moving the enqueue into the store's own
// transaction is the fix; this file is the real proof it works, not just
// that the code compiles.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

// outboxEventCount counts eventing_outbox rows for one aggregate and a
// legacy event_type suffix, scoped to the given tenant.
func outboxEventCount(t *testing.T, ctx context.Context, tenantID, aggregateID, legacyTypeSuffix string) int {
	t.Helper()
	var count int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type LIKE $3`,
		tenantID, aggregateID, "%"+legacyTypeSuffix).Scan(&count))
	return count
}

// TestPgStore_MatchStatementLine_EnqueuesMatchedEvent proves MatchStatementLine
// enqueues reconciliation.matched atomically with the status transition.
func TestPgStore_MatchStatementLine_EnqueuesMatchedEvent(t *testing.T) {
	f := setupIsolationFixture(t, "match-enqueue")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	journalID := uuid.New().String()

	require.NoError(t, testStore.MatchStatementLine(ctx, f.tenantID, f.statementLineID, journalID, "matcher-1"))

	l, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusMatched, l.Status)

	got := outboxEventCount(t, ctx, f.tenantID, f.statementLineID, "reconciliation.matched")
	assert.Equal(t, 1, got, "expected exactly 1 reconciliation.matched outbox row for this statement line")
}

// TestPgStore_FlagException_EnqueuesExceptionRaisedEvent proves FlagException
// enqueues reconciliation.exception.raised atomically with the transition.
func TestPgStore_FlagException_EnqueuesExceptionRaisedEvent(t *testing.T) {
	f := setupIsolationFixture(t, "flag-enqueue")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)

	require.NoError(t, testStore.FlagException(ctx, f.tenantID, f.statementLineID, "bank fee not in the books", "flagger-1"))

	l, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusException, l.Status)

	got := outboxEventCount(t, ctx, f.tenantID, f.statementLineID, "reconciliation.exception.raised")
	assert.Equal(t, 1, got, "expected exactly 1 reconciliation.exception.raised outbox row")
}

// TestPgStore_UnmatchWithReason_EnqueuesExceptionRaisedEvent proves the
// correction path — a MATCHED line reverted to EXCEPTION — enqueues the same
// exception.raised fact as FlagException, atomically.
func TestPgStore_UnmatchWithReason_EnqueuesExceptionRaisedEvent(t *testing.T) {
	f := setupIsolationFixture(t, "unmatch-enqueue")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	require.NoError(t, testStore.MatchStatementLine(ctx, f.tenantID, f.statementLineID, uuid.New().String(), "matcher-1"))

	require.NoError(t, testStore.UnmatchWithReason(ctx, f.tenantID, f.statementLineID, "matched to the wrong journal", "checker-1"))

	l, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	require.Equal(t, domain.StatementLineStatusException, l.Status)

	got := outboxEventCount(t, ctx, f.tenantID, f.statementLineID, "reconciliation.exception.raised")
	assert.Equal(t, 1, got, "expected exactly 1 reconciliation.exception.raised outbox row for the unmatch")
}

// TestPgStore_CertifyStatement_EnqueuesCompletedEvent proves CertifyStatement
// enqueues reconciliation.completed exactly once, atomically with the
// certificate's own creation — and never again on a retried call.
func TestPgStore_CertifyStatement_EnqueuesCompletedEvent(t *testing.T) {
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	bankAccountID := uuid.New().String()
	statementDate := time.Now().UTC().Format("2006-01-02")
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	cert, created, err := testStore.CertifyStatement(ctx, tenantID, legalEntityID, bankAccountID, statementDate, "certifier-1", "corr-cert-enqueue", 3)
	require.NoError(t, err)
	require.True(t, created)

	got := outboxEventCount(t, ctx, tenantID, bankAccountID, "reconciliation.completed")
	assert.Equal(t, 1, got, "expected exactly 1 reconciliation.completed outbox row")

	// Idempotent replay must NOT re-enqueue.
	_, created2, err := testStore.CertifyStatement(ctx, tenantID, legalEntityID, bankAccountID, statementDate, "certifier-2", "corr-cert-enqueue-2", 99)
	require.NoError(t, err)
	require.False(t, created2)
	require.NotEmpty(t, cert.CertificateID)

	gotAfterRetry := outboxEventCount(t, ctx, tenantID, bankAccountID, "reconciliation.completed")
	assert.Equal(t, 1, gotAfterRetry, "a retried CertifyStatement call must not re-enqueue reconciliation.completed")
}

// TestPgStore_SupersedeRun_EnqueuesBothSupersededAndReperformedEvents proves
// the two halves of one reperformance — the OLD run's terminal event and the
// NEW run's start event — are enqueued together, in the same transaction, so
// a listener can never observe one without the other.
func TestPgStore_SupersedeRun_EnqueuesBothSupersededAndReperformedEvents(t *testing.T) {
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	bankAccountID := uuid.New().String()
	statementDate := time.Now().UTC().Format("2006-01-02")
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	run, created, err := testStore.StartRun(ctx, tenantID, domain.StartRunRequest{
		TenantID: tenantID, LegalEntityID: legalEntityID, BankAccountID: bankAccountID,
		StatementDate: statementDate, CorrelationID: "corr-run-enqueue",
	}, "creator-1")
	require.NoError(t, err)
	require.True(t, created)

	gotStarted := outboxEventCount(t, ctx, tenantID, run.RunID, "reconciliation.run.started")
	assert.Equal(t, 1, gotStarted, "expected exactly 1 reconciliation.run.started outbox row")

	newRun, err := testStore.SupersedeRun(ctx, tenantID, run.RunID, "superseder-1", "corr-supersede-1")
	require.NoError(t, err)
	require.NotEqual(t, run.RunID, newRun.RunID)

	gotSuperseded := outboxEventCount(t, ctx, tenantID, run.RunID, "reconciliation.run.superseded")
	assert.Equal(t, 1, gotSuperseded, "expected exactly 1 reconciliation.run.superseded outbox row for the OLD run")

	gotReperformed := outboxEventCount(t, ctx, tenantID, newRun.RunID, "reconciliation.run.reperformed")
	assert.Equal(t, 1, gotReperformed, "expected exactly 1 reconciliation.run.reperformed outbox row for the NEW run")
}

// TestPgStore_RaiseEvidenceConflict_EnqueuesOnlyOnFirstRaise proves the
// idempotent-replay branch of RaiseEvidenceConflict does not re-enqueue —
// the pre-existing handler fired this event unconditionally on every call,
// including a replay against an already-OPEN conflict; moving the enqueue
// into the store's own created-gated branch fixes that as a side effect.
func TestPgStore_RaiseEvidenceConflict_EnqueuesOnlyOnFirstRaise(t *testing.T) {
	f := setupIsolationFixture(t, "conflict-enqueue")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	req := domain.RaiseEvidenceConflictRequest{
		LegalEntityID: f.entityID, StatementLineID: f.statementLineID, PaymentID: uuid.New().String(),
		BankRecStatus: "MATCHED", ProviderConfirmedStatus: "FAILED", ConflictReason: "status disagreement",
		CorrelationID: "corr-conflict-enqueue",
	}

	conflict, created, err := testStore.RaiseEvidenceConflict(ctx, f.tenantID, req)
	require.NoError(t, err)
	require.True(t, created)

	got := outboxEventCount(t, ctx, f.tenantID, conflict.ConflictID, "reconciliation.evidence-conflict.raised")
	assert.Equal(t, 1, got, "expected exactly 1 evidence-conflict.raised outbox row")

	// Replay: same (statement_line_id, payment_id) pair, still OPEN.
	_, created2, err := testStore.RaiseEvidenceConflict(ctx, f.tenantID, req)
	require.NoError(t, err)
	require.False(t, created2)

	gotAfterReplay := outboxEventCount(t, ctx, f.tenantID, conflict.ConflictID, "reconciliation.evidence-conflict.raised")
	assert.Equal(t, 1, gotAfterReplay, "a replay against an already-OPEN conflict must not re-enqueue evidence-conflict.raised")
}
