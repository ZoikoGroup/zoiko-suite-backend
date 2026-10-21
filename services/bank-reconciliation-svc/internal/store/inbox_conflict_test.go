//go:build integration

// Package store_test — the evidence-conflict consumer on the shared eventing
// inbox, against real Postgres and the real store (ZS-SVC-M-001 INT-03).
//
// What these prove that the consumer's unit tests cannot: the conflict row, its
// outbox event and the inbox receipt are ONE transaction. Before this, the
// consumer wrote the conflict, then recorded a receipt in a second transaction
// whose failure was only logged ("a replay may reprocess this event"), and a
// failed receipt lookup was answered by processing anyway.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/events"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
	"zoiko.io/eventing/inbox"
)

func newConflictInbox(t *testing.T, maxAttempts int) *inbox.Inbox {
	t.Helper()
	in, err := inbox.New(testStore, events.ConsumerName, inbox.Config{MaxAttempts: maxAttempts})
	require.NoError(t, err)
	return in
}

func conflictRaw(t *testing.T, eventID, tenantID, lineID string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"event_id": eventID, "event_type": "PAYMENT_STATUS_CONFLICT_RAISED", "tenant_id": tenantID,
		"correlation_id": "corr-" + eventID,
		"payload": map[string]any{
			"payment_id": uuid.New().String(), "statement_line_id": lineID,
			"provider_request_id": "prov-1", "bank_rec_status": "MATCHED",
			"reported_status": "SETTLED", "current_status": "REJECTED",
		},
	})
	require.NoError(t, err)
	return b
}

func countConflicts(t *testing.T, tenantID string) int {
	t.Helper()
	var n int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM evidence_conflicts WHERE tenant_id = $1`, tenantID).Scan(&n))
	return n
}

func countConflictEvents(t *testing.T, tenantID string) int {
	t.Helper()
	var n int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM eventing_outbox WHERE tenant_id = $1 AND event_type LIKE '%evidence_conflict.raised'`, tenantID).Scan(&n))
	return n
}

func receiptState(t *testing.T, tenantID, eventID string) (string, int) {
	t.Helper()
	var state string
	var attempts int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT state, attempt_count FROM eventing_inbox WHERE consumer = $1 AND tenant_id = $2 AND event_id = $3`,
		events.ConsumerName, tenantID, eventID).Scan(&state, &attempts))
	return state, attempts
}

func TestConflictInbox_ProcessedOnceAcrossRedeliveries(t *testing.T) {
	f := setupIsolationFixture(t, "inbox-once")
	c := events.NewConflictConsumer(zap.NewNop(), testStore, newConflictInbox(t, 3))
	ctx := context.Background()
	raw := conflictRaw(t, "evt-"+uuid.NewString(), f.tenantID, f.statementLineID)

	for i := 0; i < 3; i++ {
		require.NoError(t, c.Handle(ctx, raw))
	}
	assert.Equal(t, 1, countConflicts(t, f.tenantID), "three deliveries, one conflict")
	assert.Equal(t, 1, countConflictEvents(t, f.tenantID), "three deliveries, one outbox event")
}

func TestConflictInbox_ConcurrentDeliveriesProduceOneConflict(t *testing.T) {
	f := setupIsolationFixture(t, "inbox-race")
	c := events.NewConflictConsumer(zap.NewNop(), testStore, newConflictInbox(t, 3))
	raw := conflictRaw(t, "evt-"+uuid.NewString(), f.tenantID, f.statementLineID)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, c.Handle(context.Background(), raw))
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, countConflicts(t, f.tenantID))
	assert.Equal(t, 1, countConflictEvents(t, f.tenantID))
}

// The effect and the receipt commit together: a handler that raises the
// conflict and then fails leaves no conflict, no outbox event, and a retryable
// receipt.
func TestConflictInbox_FailureAfterEffectRollsBackConflictAndEvent(t *testing.T) {
	f := setupIsolationFixture(t, "inbox-atomic")
	in := newConflictInbox(t, 3)
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)
	raw := conflictRaw(t, "evt-"+uuid.NewString(), f.tenantID, f.statementLineID)
	msg, err := inbox.Parse(raw)
	require.NoError(t, err)

	line, err := testStore.GetStatementLine(ctx, f.statementLineID)
	require.NoError(t, err)
	_, err = in.Process(ctx, msg, func(hctx context.Context, tx pgx.Tx, r inbox.Receipt) (string, error) {
		if _, _, e := testStore.RaiseEvidenceConflictInTx(hctx, tx, f.tenantID, raiseReq(f, line.LegalEntityID, msg.EventID)); e != nil {
			return "", e
		}
		return "", errors.New("consumer died after raising the conflict")
	})
	require.Error(t, err)

	assert.Equal(t, 0, countConflicts(t, f.tenantID), "the conflict must roll back with the failed handler")
	assert.Equal(t, 0, countConflictEvents(t, f.tenantID), "so must its outbox event")
	state, attempts := receiptState(t, f.tenantID, msg.EventID)
	assert.Equal(t, inbox.StateFailed, state)
	assert.Equal(t, 1, attempts)

	// The redelivery now processes it, once.
	c := events.NewConflictConsumer(zap.NewNop(), testStore, in)
	require.NoError(t, c.Handle(context.Background(), raw))
	assert.Equal(t, 1, countConflicts(t, f.tenantID))
	assert.Equal(t, 1, countConflictEvents(t, f.tenantID))
	state, attempts = receiptState(t, f.tenantID, msg.EventID)
	assert.Equal(t, inbox.StateCompleted, state)
	assert.Equal(t, 2, attempts)
}

func TestConflictInbox_UnknownLineQuarantinedWithoutRaising(t *testing.T) {
	f := setupIsolationFixture(t, "inbox-unknown")
	c := events.NewConflictConsumer(zap.NewNop(), testStore, newConflictInbox(t, 2))
	c.SetRetryDelay(0)
	eventID := "evt-" + uuid.NewString()
	require.NoError(t, c.Handle(context.Background(), conflictRaw(t, eventID, f.tenantID, uuid.NewString())))

	assert.Equal(t, 0, countConflicts(t, f.tenantID))
	state, attempts := receiptState(t, f.tenantID, eventID)
	assert.Equal(t, inbox.StateQuarantine, state, "evidence of the failure is kept, not dropped")
	assert.Equal(t, 2, attempts)
}

// Another tenant's statement line is "unknown" to this tenant: the conflict is
// never raised across tenants.
func TestConflictInbox_CrossTenantLineIsRefused(t *testing.T) {
	a := setupIsolationFixture(t, "inbox-a")
	b := setupIsolationFixture(t, "inbox-b")
	c := events.NewConflictConsumer(zap.NewNop(), testStore, newConflictInbox(t, 1))
	c.SetRetryDelay(0)
	require.NoError(t, c.Handle(context.Background(), conflictRaw(t, "evt-"+uuid.NewString(), b.tenantID, a.statementLineID)))
	assert.Equal(t, 0, countConflicts(t, a.tenantID))
	assert.Equal(t, 0, countConflicts(t, b.tenantID))
}

func TestConflictInbox_SameEventIDUnderTwoTenantsAreIndependent(t *testing.T) {
	a := setupIsolationFixture(t, "inbox-ta")
	b := setupIsolationFixture(t, "inbox-tb")
	c := events.NewConflictConsumer(zap.NewNop(), testStore, newConflictInbox(t, 3))
	shared := "evt-" + uuid.NewString()
	require.NoError(t, c.Handle(context.Background(), conflictRaw(t, shared, a.tenantID, a.statementLineID)))
	require.NoError(t, c.Handle(context.Background(), conflictRaw(t, shared, b.tenantID, b.statementLineID)))
	assert.Equal(t, 1, countConflicts(t, a.tenantID))
	assert.Equal(t, 1, countConflicts(t, b.tenantID))
}

func TestConflictInbox_ConflictingRedeliveryIsNotAcknowledged(t *testing.T) {
	f := setupIsolationFixture(t, "inbox-conflict")
	c := events.NewConflictConsumer(zap.NewNop(), testStore, newConflictInbox(t, 3))
	id := "evt-" + uuid.NewString()
	require.NoError(t, c.Handle(context.Background(), conflictRaw(t, id, f.tenantID, f.statementLineID)))
	// same event id, different content: not a harmless duplicate
	err := c.Handle(context.Background(), conflictRaw(t, id, f.tenantID, f.statementLineID))
	require.Error(t, err, "a redelivery with different content must not be silently acknowledged as a duplicate")
	assert.Equal(t, 1, countConflicts(t, f.tenantID))
}

func TestConflictInbox_SchemaVerified(t *testing.T) {
	require.NoError(t, inbox.VerifySchema(context.Background(), testPool))
}

func raiseReq(f lineFixture, entityID, eventID string) domain.RaiseEvidenceConflictRequest {
	return domain.RaiseEvidenceConflictRequest{
		TenantID: f.tenantID, LegalEntityID: entityID, StatementLineID: f.statementLineID,
		PaymentID: uuid.NewString(), ProviderRequestID: "prov-1", BankRecStatus: "MATCHED",
		ProviderConfirmedStatus: "REJECTED", ConflictReason: "test", SourceEventID: eventID, CorrelationID: "corr-" + eventID,
	}
}
