package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/document-vault-svc/internal/outbox"
)

type recordingPublisher struct{ ids []string }

func (p *recordingPublisher) PublishOutbox(_ context.Context, outboxEventID, _ string, _ []byte) error {
	p.ids = append(p.ids, outboxEventID)
	return nil
}

// A tenant-level event (a legal hold, an export package) has no legal entity and is
// stored with a NULL legal_entity_id. The relay must deliver it, and must not let it
// block the events behind it: scanning NULL into a plain string used to fail every
// poll, so one such row stopped all later delivery.
func TestOutboxRelay_DeliversEventsWithNoLegalEntity_AndDoesNotBlockLaterOnes(t *testing.T) {
	pool := requireTestDB(t)
	ctx := context.Background()
	clearOutbox(t, pool)

	insert := func(aggregateID, eventType, legalEntityID string) string {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		id := "00000000-0000-4000-8000-" + aggregateID
		require.NoError(t, outbox.Insert(ctx, tx, outbox.Event{
			OutboxEventID: id, AggregateType: "LEGAL_HOLD", AggregateID: aggregateID, EventType: eventType,
			TenantID: testTenantID, LegalEntityID: legalEntityID, CorrelationID: "corr-relay",
			Payload: map[string]any{"ok": true},
		}))
		require.NoError(t, tx.Commit(ctx))
		return id
	}
	tenantLevel := insert("000000000001", "legal_hold_activated", "")
	time.Sleep(5 * time.Millisecond) // the relay orders by created_at
	entityLevel := insert("000000000002", "document_created", "22222222-2222-2222-2222-222222222222")

	pub := &recordingPublisher{}
	outbox.NewRelay(pool, pub, time.Second, 50, zap.NewNop()).RelayOnce(ctx)

	require.Equal(t, []string{tenantLevel, entityLevel}, pub.ids, "both events must be delivered, the tenant-level one first")
	var unpublished int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&unpublished))
	require.Zero(t, unpublished)
}
