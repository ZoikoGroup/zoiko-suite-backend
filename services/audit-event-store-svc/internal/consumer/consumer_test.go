package consumer_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"zoiko.io/audit-event-store-svc/internal/consumer"
	"zoiko.io/audit-event-store-svc/internal/store"
)

// ─── helpers ────────────────────────────────────────────────────────────────

// buildContextResolvedMsg constructs a valid identity.context.resolved message
// with the given eventID.  Any field can be overridden via the opts map.
func buildContextResolvedMsg(t *testing.T, eventID string, opts map[string]string) []byte {
	t.Helper()
	payload := map[string]string{
		"principal_id":       "principal-001",
		"tenant_id":          "tenant-abc",
		"legal_entity_id":    "entity-xyz",
		"session_context_id": "sess-123",
		"correlation_id":     "corr-456",
	}
	for k, v := range opts {
		if v == "" {
			delete(payload, k)
		} else {
			payload[k] = v
		}
	}
	rawPayload, err := json.Marshal(payload)
	require.NoError(t, err)

	env := map[string]interface{}{
		"event_type":     "identity.context.resolved",
		"emitted_at":     "2026-07-03T10:00:00Z",
		"schema_version": "1.0",
		"source_service": "identity-context-svc",
		"payload":        json.RawMessage(rawPayload),
	}
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return raw
}

func newConsumer(t *testing.T, s store.Store) *consumer.Consumer {
	t.Helper()
	log := zaptest.NewLogger(t, zaptest.WrapOptions(zap.Development()))
	return consumer.New(s, log)
}

// ─── Test 1: same event_id delivered twice → exactly ONE stored row ─────────

// TestDedupSameEventIDTwice verifies that delivering the same event_id twice
// results in exactly one stored row and that the second delivery does not
// return an error.
//
// This satisfies required test #1 and implicitly tests that the store's
// DO NOTHING path is exercised without error.
func TestDedupSameEventIDTwice(t *testing.T) {
	s := store.NewFakeStore()
	c := newConsumer(t, s)
	ctx := context.Background()

	const eventID = "evt-unique-001"
	msg := buildContextResolvedMsg(t, eventID, nil)

	// First delivery.
	err := c.Handle(ctx, eventID, msg)
	require.NoError(t, err, "first delivery must succeed")
	assert.Equal(t, 1, s.Count(), "after first delivery, exactly 1 row must exist")

	// Second delivery of the same event_id.
	err = c.Handle(ctx, eventID, msg)
	require.NoError(t, err, "second delivery of same event_id must not error")
	assert.Equal(t, 1, s.Count(), "after second delivery, still exactly 1 row must exist")
}

// ─── Test 2: concurrent delivery of the same event_id ────────────────────────

// TestDedupConcurrent fires two goroutines that attempt to insert the same
// event_id simultaneously.  It asserts:
//
//  1. Only one row exists after both goroutines complete.
//  2. Neither goroutine returns an unexpected error.
//
// This is the required concurrent-dedup test (#2).  The FakeStore's mutex
// replicates the database-level ON CONFLICT DO NOTHING serialisation guarantee.
// The race detector (-race) will catch any data races inside the store.
func TestDedupConcurrent(t *testing.T) {
	s := store.NewFakeStore()
	c := newConsumer(t, s)
	ctx := context.Background()

	const eventID = "evt-concurrent-001"
	msg := buildContextResolvedMsg(t, eventID, nil)

	var (
		wg   sync.WaitGroup
		errs = make([]error, 2)
	)

	// Synchronise both goroutines to start as close together as possible,
	// maximising the chance of a true concurrent collision.
	ready := make(chan struct{})

	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready // wait for the gun
			errs[i] = c.Handle(ctx, eventID, msg)
		}()
	}

	close(ready) // fire both goroutines simultaneously
	wg.Wait()

	for i, err := range errs {
		assert.NoErrorf(t, err, "goroutine %d must not error", i)
	}
	assert.Equal(t, 1, s.Count(),
		"after concurrent delivery, exactly 1 row must exist")
}

// ─── Test 3: malformed event is rejected cleanly ─────────────────────────────

// TestMalformedEventRejected verifies that a malformed event is:
//  1. Not stored (0 rows in the store).
//  2. Does not cause the consumer to return an error (it logs and moves on).
//
// This tests the required failure mode: bad events are rejected at the consumer
// layer, not stored partially, and must not crash the consumer loop.
func TestMalformedEventRejected(t *testing.T) {
	t.Run("missing_tenant_id", func(t *testing.T) {
		s := store.NewFakeStore()
		c := newConsumer(t, s)
		ctx := context.Background()

		msg := buildContextResolvedMsg(t, "evt-malformed-001", map[string]string{
			"tenant_id": "", // deliberately omit to trigger validation failure
		})
		err := c.Handle(ctx, "evt-malformed-001", msg)
		require.NoError(t, err, "malformed event must not return an error to the caller")
		assert.Equal(t, 0, s.Count(), "malformed event must not be stored")
	})

	t.Run("missing_legal_entity_id", func(t *testing.T) {
		s := store.NewFakeStore()
		c := newConsumer(t, s)
		ctx := context.Background()

		msg := buildContextResolvedMsg(t, "evt-malformed-002", map[string]string{
			"legal_entity_id": "",
		})
		err := c.Handle(ctx, "evt-malformed-002", msg)
		require.NoError(t, err)
		assert.Equal(t, 0, s.Count())
	})

	t.Run("missing_principal_id", func(t *testing.T) {
		s := store.NewFakeStore()
		c := newConsumer(t, s)
		ctx := context.Background()

		msg := buildContextResolvedMsg(t, "evt-malformed-003", map[string]string{
			"principal_id": "",
		})
		err := c.Handle(ctx, "evt-malformed-003", msg)
		require.NoError(t, err)
		assert.Equal(t, 0, s.Count())
	})

	t.Run("missing_session_context_id", func(t *testing.T) {
		s := store.NewFakeStore()
		c := newConsumer(t, s)
		ctx := context.Background()

		msg := buildContextResolvedMsg(t, "evt-malformed-004", map[string]string{
			"session_context_id": "",
		})
		err := c.Handle(ctx, "evt-malformed-004", msg)
		require.NoError(t, err)
		assert.Equal(t, 0, s.Count())
	})

	t.Run("missing_correlation_id", func(t *testing.T) {
		s := store.NewFakeStore()
		c := newConsumer(t, s)
		ctx := context.Background()

		msg := buildContextResolvedMsg(t, "evt-malformed-005", map[string]string{
			"correlation_id": "",
		})
		err := c.Handle(ctx, "evt-malformed-005", msg)
		require.NoError(t, err)
		assert.Equal(t, 0, s.Count())
	})

	t.Run("unparseable_json", func(t *testing.T) {
		s := store.NewFakeStore()
		c := newConsumer(t, s)
		ctx := context.Background()

		err := c.Handle(ctx, "evt-malformed-bad-json", []byte(`{not valid json`))
		require.NoError(t, err, "unparseable JSON must not return error to caller")
		assert.Equal(t, 0, s.Count())
	})

	t.Run("empty_event_id", func(t *testing.T) {
		s := store.NewFakeStore()
		c := newConsumer(t, s)
		ctx := context.Background()

		msg := buildContextResolvedMsg(t, "", nil)
		err := c.Handle(ctx, "", msg) // empty event_id
		require.NoError(t, err)
		assert.Equal(t, 0, s.Count())
	})
}

// ─── Test 4: entity.status.changed (existing consumer) ──────────────────────

// TestEntityStatusChangedStored verifies that a valid entity.status.changed
// event is stored correctly, confirming the existing consumer still works
// after the ContextResolved addition.
func TestEntityStatusChangedStored(t *testing.T) {
	s := store.NewFakeStore()
	c := newConsumer(t, s)
	ctx := context.Background()

	payload := map[string]string{
		"tenant_id":       "tenant-abc",
		"legal_entity_id": "entity-xyz",
		"previous_status": "ACTIVE",
		"new_status":      "SUSPENDED",
	}
	rawPayload, err := json.Marshal(payload)
	require.NoError(t, err)

	env := map[string]interface{}{
		"event_type":     "entity.status.changed",
		"emitted_at":     "2026-07-03T10:00:00Z",
		"schema_version": "1.0",
		"source_service": "tenant-entity-registry-svc",
		"payload":        json.RawMessage(rawPayload),
	}
	msg, err := json.Marshal(env)
	require.NoError(t, err)

	const eventID = "evt-entity-status-001"
	err = c.Handle(ctx, eventID, msg)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Count())

	stored, ok := s.Get(eventID)
	require.True(t, ok)
	assert.Equal(t, "entity.status.changed", stored.EventType)
	assert.Equal(t, "tenant-abc", stored.TenantID)
	assert.Equal(t, "entity-xyz", stored.LegalEntityID)
}

// ─── Test 5: valid ContextResolved is stored with correct fields ─────────────

// TestContextResolvedStoredCorrectly verifies that a well-formed
// identity.context.resolved event is persisted with all expected fields.
func TestContextResolvedStoredCorrectly(t *testing.T) {
	s := store.NewFakeStore()
	c := newConsumer(t, s)
	ctx := context.Background()

	const eventID = "evt-ctx-resolved-001"
	msg := buildContextResolvedMsg(t, eventID, map[string]string{
		"principal_id":       "user-007",
		"tenant_id":          "tenant-zz",
		"legal_entity_id":    "entity-99",
		"session_context_id": "sess-abc",
		"correlation_id":     "corr-xyz",
	})

	err := c.Handle(ctx, eventID, msg)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Count())

	stored, ok := s.Get(eventID)
	require.True(t, ok, "stored event must be retrievable by event_id")
	assert.Equal(t, "identity.context.resolved", stored.EventType)
	assert.Equal(t, "tenant-zz", stored.TenantID)
	assert.Equal(t, "entity-99", stored.LegalEntityID)
	assert.Equal(t, "user-007", stored.PrincipalID)
	assert.Equal(t, "identity-context-svc", stored.SourceService)
	assert.Equal(t, "1.0", stored.SchemaVersion)
	assert.NotEmpty(t, stored.Payload)
}
