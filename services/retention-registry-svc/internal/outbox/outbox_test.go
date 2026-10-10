package outbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/retention-registry-svc/internal/outbox"
)

type mockPublisher struct {
	published [][]byte
	err       error
}

func (m *mockPublisher) PublishOutbox(_ context.Context, _, _ string, payload []byte) error {
	if m.err != nil {
		return m.err
	}
	m.published = append(m.published, payload)
	return nil
}

func TestInsert_NilTx_ReturnsError(t *testing.T) {
	err := outbox.Insert(context.Background(), nil, outbox.Event{
		AggregateType: "retention_policy",
		AggregateID:   "rp-1",
		EventType:     "retention_policy.created",
		Payload:       map[string]string{"foo": "bar"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "transaction is nil")
}

func TestRelayOnce_NilPool_ReturnsZero(t *testing.T) {
	pub := &mockPublisher{}
	relay := outbox.NewRelay(nil, pub, 100*time.Millisecond, 10, zap.NewNop())

	count, err := relay.RelayOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestRelay_Start_StopsOnContextCancellation(t *testing.T) {
	pub := &mockPublisher{}
	relay := outbox.NewRelay(nil, pub, 10*time.Millisecond, 10, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		relay.Start(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Fatal("relay.Start failed to stop after context cancellation")
	}
}

func TestMockPublisher_ErrorPropagation(t *testing.T) {
	pub := &mockPublisher{err: errors.New("kafka unavailable")}
	err := pub.PublishOutbox(context.Background(), "id-1", "rp-1", []byte(`{}`))
	assert.Error(t, err)
	assert.Equal(t, "kafka unavailable", err.Error())
}
