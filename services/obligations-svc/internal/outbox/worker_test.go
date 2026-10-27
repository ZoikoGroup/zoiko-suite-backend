package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/outbox"
	"zoiko.io/obligations-svc/internal/store"
)

// fakeSource stands in for RelayOutbox: it hands each event to publish and
// records the outcome the way the store does (published, or failed and kept).
type fakeSource struct {
	events    []store.OutboxEvent
	relayErr  error
	published []string
	failed    map[string]string
	stats     store.OutboxStats
}

func (s *fakeSource) RelayOutbox(ctx context.Context, _ int, publish func(context.Context, store.OutboxEvent) error) (int, int, error) {
	if s.relayErr != nil {
		return 0, 0, s.relayErr
	}
	var ok, bad int
	for _, e := range s.events {
		if err := publish(ctx, e); err != nil {
			if s.failed == nil {
				s.failed = map[string]string{}
			}
			s.failed[e.OutboxID] = err.Error()
			bad++
			continue
		}
		s.published = append(s.published, e.OutboxID)
		ok++
	}
	return ok, bad, nil
}

func (s *fakeSource) GetOutboxStats(context.Context) (store.OutboxStats, error) { return s.stats, nil }

type fakePublisher struct {
	keys    []string
	values  [][]byte
	failFor map[string]error
}

func (p *fakePublisher) PublishEnvelope(_ context.Context, key string, envelope []byte) error {
	if err := p.failFor[key]; err != nil {
		return err
	}
	p.keys = append(p.keys, key)
	p.values = append(p.values, envelope)
	return nil
}

type fakeGauges struct {
	pending, failed int
	oldest          float64
}

func (g *fakeGauges) SetOutbox(p, f int, o float64) { g.pending, g.failed, g.oldest = p, f, o }

func envelopeFor(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"event_id": "evt-1", "event_type": "obligation.updated", "payload": payload})
	require.NoError(t, err)
	return raw
}

func TestRelayOnce_PublishesEachEnvelopeKeyedByTenantAndAggregate(t *testing.T) {
	envA := envelopeFor(t, map[string]any{"obligation_id": "o-1"})
	envB := envelopeFor(t, map[string]any{"obligation_definition_id": "d-1"})
	envC := envelopeFor(t, map[string]any{})
	source := &fakeSource{events: []store.OutboxEvent{
		{OutboxID: "row-a", TenantID: "t1", EventPayload: envA},
		{OutboxID: "row-b", TenantID: "t1", EventPayload: envB},
		{OutboxID: "row-c", TenantID: "t2", EventPayload: envC},
	}}
	publisher := &fakePublisher{}

	n, err := outbox.NewWorker(source, publisher, zap.NewNop()).RelayOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, [][]byte{envA, envB, envC}, publisher.values, "the stored envelope must be relayed byte for byte")
	assert.Equal(t, []string{"t1/o-1", "t1/d-1", "t2/row-c"}, publisher.keys,
		"key is hash(tenant, aggregate) per Event Catalogue §5.3, falling back to the row id")
	assert.ElementsMatch(t, []string{"row-a", "row-b", "row-c"}, source.published)
	assert.Empty(t, source.failed)
}

func TestRelayOnce_PublishFailureIsKeptForRetry(t *testing.T) {
	source := &fakeSource{events: []store.OutboxEvent{
		{OutboxID: "row-a", TenantID: "t1", EventPayload: envelopeFor(t, map[string]any{"obligation_id": "o-1"})},
	}}
	publisher := &fakePublisher{failFor: map[string]error{"t1/o-1": errors.New("kafka down")}}

	n, err := outbox.NewWorker(source, publisher, zap.NewNop()).RelayOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, source.published)
	assert.Equal(t, "kafka down", source.failed["row-a"])
}

func TestRelayOnce_SourceFailureIsReturned(t *testing.T) {
	source := &fakeSource{relayErr: errors.New("db down")}
	_, err := outbox.NewWorker(source, &fakePublisher{}, zap.NewNop()).RelayOnce(context.Background())
	require.Error(t, err)
}

func TestReportStats_FeedsTheAlertingGauges(t *testing.T) {
	source := &fakeSource{stats: store.OutboxStats{Pending: 7, Failed: 2, OldestPendingSecond: 42}}
	g := &fakeGauges{}
	outbox.NewWorker(source, &fakePublisher{}, zap.NewNop()).WithGauges(g).ReportStats(context.Background())
	assert.Equal(t, 7, g.pending)
	assert.Equal(t, 2, g.failed)
	assert.Equal(t, 42.0, g.oldest)
}
