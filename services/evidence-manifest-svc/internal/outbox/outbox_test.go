package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/outbox"
)

func TestInsert_NilTx_ReturnsError(t *testing.T) {
	err := outbox.Insert(context.Background(), nil, outbox.Event{
		AggregateType: "MANIFEST",
		AggregateID:   uuid.NewString(),
		EventType:     "evidence.manifest.generated",
		TenantID:      uuid.NewString(),
		CorrelationID: uuid.NewString(),
		Payload:       map[string]any{"test": "data"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transaction is nil")
}

// ── Mock pool, tx, rows, and publisher for Relay tests ───────────────────────

type mockTx struct {
	pgx.Tx
	execCalls    []string
	execArgs     [][]any
	queries      []string
	rowsToReturn pgx.Rows
	queryErr     error
	committed    bool
	rolledBack   bool
}

func (m *mockTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.execCalls = append(m.execCalls, sql)
	m.execArgs = append(m.execArgs, args)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (m *mockTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	m.queries = append(m.queries, sql)
	return m.rowsToReturn, m.queryErr
}

var _ pgx.Tx = (*mockTx)(nil)

func (m *mockTx) Commit(_ context.Context) error {
	m.committed = true
	return nil
}

func (m *mockTx) Rollback(_ context.Context) error {
	m.rolledBack = true
	return nil
}

type mockRows struct {
	pgx.Rows
	items   []outbox.StoredEvent
	current int
}

func (r *mockRows) Next() bool {
	if r.current < len(r.items) {
		r.current++
		return true
	}
	return false
}

func (r *mockRows) Scan(dest ...any) error {
	item := r.items[r.current-1]
	*dest[0].(*string) = item.OutboxEventID
	*dest[1].(*string) = item.AggregateType
	*dest[2].(*string) = item.AggregateID
	*dest[3].(*string) = item.EventType
	*dest[4].(*string) = item.TenantID
	*dest[5].(*string) = item.LegalEntityID
	*dest[6].(**string) = item.ActorID
	*dest[7].(*string) = item.CorrelationID
	*dest[8].(*[]byte) = []byte("{}")
	*dest[9].(*json.RawMessage) = item.Payload
	*dest[10].(*int) = item.Attempts
	return nil
}

func (r *mockRows) Close() {}

type mockPool struct {
	tx       *mockTx
	beginErr error
}

func (p *mockPool) Begin(_ context.Context) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return p.tx, nil
}

func (p *mockPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

func (p *mockPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

type mockPublisher struct {
	published []string
	pubErr    error
}

func (m *mockPublisher) PublishOutbox(_ context.Context, outboxEventID, _ string, _ []byte) error {
	if m.pubErr != nil {
		return m.pubErr
	}
	m.published = append(m.published, outboxEventID)
	return nil
}

// ── Relay Tests ─────────────────────────────────────────────────────────────

func TestRelayOnce_EmptyQueue_DoesNothing(t *testing.T) {
	tx := &mockTx{
		rowsToReturn: &mockRows{items: nil},
	}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay.RelayOnce(context.Background())

	assert.Empty(t, pub.published)
	assert.Empty(t, tx.execCalls)
}

func TestRelayOnce_Success_MarksPublished(t *testing.T) {
	eventID := uuid.NewString()
	tx := &mockTx{
		rowsToReturn: &mockRows{
			items: []outbox.StoredEvent{
				{
					OutboxEventID: eventID,
					AggregateType: "MANIFEST",
					AggregateID:   "manifest-123",
					EventType:     "evidence.manifest.generated",
					TenantID:      uuid.NewString(),
					LegalEntityID: uuid.NewString(),
					CorrelationID: "corr-123",
					Payload:       []byte(`{"test":"payload"}`),
					Attempts:      0,
				},
			},
		},
	}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay.RelayOnce(context.Background())

	require.Len(t, pub.published, 1)
	assert.Equal(t, eventID, pub.published[0])
	require.Len(t, tx.execCalls, 1)
	assert.Contains(t, tx.execCalls[0], "SET published_at = now()")
	assert.True(t, tx.committed)
}

func TestRelayOnce_PublishError_IncrementsAttemptsAndRecordsError(t *testing.T) {
	eventID := uuid.NewString()
	tx := &mockTx{
		rowsToReturn: &mockRows{
			items: []outbox.StoredEvent{
				{
					OutboxEventID: eventID,
					AggregateType: "MANIFEST",
					AggregateID:   "manifest-123",
					EventType:     "evidence.manifest.generated",
					TenantID:      uuid.NewString(),
					LegalEntityID: uuid.NewString(),
					CorrelationID: "corr-123",
					Payload:       []byte(`{"test":"payload"}`),
					Attempts:      1,
				},
			},
		},
	}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{pubErr: errors.New("kafka broker connection refused")}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay.RelayOnce(context.Background())

	assert.Empty(t, pub.published)
	require.Len(t, tx.execCalls, 1)
	assert.Contains(t, tx.execCalls[0], "publish_attempts = publish_attempts + 1, last_error = $2")
	assert.Equal(t, eventID, tx.execArgs[0][0])
	assert.Equal(t, "kafka broker connection refused", tx.execArgs[0][1])
	assert.True(t, tx.committed)
}
