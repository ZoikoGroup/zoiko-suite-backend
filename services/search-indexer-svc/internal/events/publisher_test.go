package events

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeWriter struct {
	mu   sync.Mutex
	msgs []kafka.Message
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msgs...)
	return nil
}

func decode(t *testing.T, msg kafka.Message) (envelope, map[string]any) {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(msg.Value, &env))
	var payload map[string]any
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	return env, payload
}

// Every esr.* event carries the Doc 03 §19 envelope fields, so a consumer can
// dedupe and correlate without special-casing this producer.
func TestPublisher_EnvelopeShape(t *testing.T) {
	w := &fakeWriter{}
	p := NewPublisher(zap.NewNop(), w)

	require.NoError(t, p.GenerationActivated(context.Background(),
		"obligation", "old-gen", "new-gen", "actor-1", "corr-1"))

	require.Len(t, w.msgs, 1)
	env, payload := decode(t, w.msgs[0])

	assert.Equal(t, EventGenerationActivated, env.EventType)
	assert.Equal(t, "search-indexer-svc", env.SourceService)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.True(t, strings.HasPrefix(env.EventID, "evt-"))
	assert.False(t, env.EmittedAt.IsZero())
	assert.Equal(t, "new-gen", payload["new_generation"])
	assert.Equal(t, "old-gen", payload["previous_generation"])
}

// The Kafka key is the SCOPE, so every event about one search surface lands on
// one partition and arrives in order. A consumer reading generation.ready
// after generation.activated for the same scope would otherwise be routine
// rather than a bug.
func TestPublisher_KeyIsTheScope(t *testing.T) {
	w := &fakeWriter{}
	p := NewPublisher(zap.NewNop(), w)

	require.NoError(t, p.GenerationReady(context.Background(),
		"obligation", "g-1", "c-1", "idx-1", "digest", "corr-1"))
	require.NoError(t, p.CheckpointAdvanced(context.Background(),
		"obligation", "idx-1", 100, 50, "g-1", "CURRENT"))

	require.Len(t, w.msgs, 2)
	assert.Equal(t, "obligation", string(w.msgs[0].Key))
	assert.Equal(t, "obligation", string(w.msgs[1].Key))
}

// X-Event-ID is always set, so a consumer dedupes on a stable id rather than
// falling back to topic:partition:offset — a fallback that cannot absorb a
// producer-side retry landing on a different offset.
func TestPublisher_SetsEventIDHeader(t *testing.T) {
	w := &fakeWriter{}
	p := NewPublisher(zap.NewNop(), w)

	require.NoError(t, p.SearchDegraded(context.Background(),
		"tenant-1", "obligation", "shard failure", "PARTIAL", []string{"idx-1"}, "corr-1"))

	require.Len(t, w.msgs, 1)
	var header string
	for _, h := range w.msgs[0].Headers {
		if h.Key == "X-Event-ID" {
			header = string(h.Value)
		}
	}
	require.NotEmpty(t, header)

	env, _ := decode(t, w.msgs[0])
	assert.Equal(t, env.EventID, header, "the header and the envelope must agree")
}

// §11.2 specifies "actor/workload HASH" for esr.security_filter.denied and not
// for the others. This test pins that the event carries whatever the caller
// passed as a hash and never a raw principal id — the hashing itself is the
// handler's, and this is the contract it fills.
func TestPublisher_SecurityFilterDeniedCarriesAHashNotAPrincipal(t *testing.T) {
	w := &fakeWriter{}
	p := NewPublisher(zap.NewNop(), w)

	require.NoError(t, p.SecurityFilterDenied(context.Background(),
		"tenant-1", "obligation", "ESR-003", "abc123hash", "corr-1"))

	require.Len(t, w.msgs, 1)
	_, payload := decode(t, w.msgs[0])
	assert.Equal(t, "abc123hash", payload["actor_hash"])
	assert.NotContains(t, payload, "actor_id")
	assert.NotContains(t, payload, "principal_id")
}

// INV-17 / §9.2. No esr.* event carries a tenant's content — these describe
// the search plane, and their consumers are SRE, audit, DQC and privacy.
func TestPublisher_EventsCarryNoTenantContent(t *testing.T) {
	w := &fakeWriter{}
	p := NewPublisher(zap.NewNop(), w)
	ctx := context.Background()

	require.NoError(t, p.RestrictionPropagated(ctx, "tenant-1", "obligation",
		"obligation", "ob-1", "PRV_ERASURE", "evt-1", time.Now().UTC()))
	require.NoError(t, p.RestrictionFailed(ctx, "tenant-1", "obligation",
		"obligation", "ob-1", "PRV_ERASURE", 120, 1))
	require.NoError(t, p.ReindexFailed(ctx, "obligation", "g-1", "VALIDATION", "count mismatch", "corr"))

	for _, msg := range w.msgs {
		_, payload := decode(t, msg)
		// A source_ref may travel; the document behind it may not.
		for key := range payload {
			assert.NotContains(t, []string{"fields", "body", "content", "snippet", "query"}, key,
				"no esr.* event may carry indexed content")
		}
	}
}

// A restriction event is emitted at VERIFIED, and carries the verification
// time — PRV/DRC consume it as evidence the obligation was met in the search
// plane, so it must not be emitted on an attempt.
func TestPublisher_RestrictionPropagatedCarriesVerifiedAt(t *testing.T) {
	w := &fakeWriter{}
	p := NewPublisher(zap.NewNop(), w)
	verified := time.Date(2026, 9, 21, 10, 30, 0, 0, time.UTC)

	require.NoError(t, p.RestrictionPropagated(context.Background(), "tenant-1", "obligation",
		"obligation", "ob-1", "PRV_ERASURE", "evt-1", verified))

	require.Len(t, w.msgs, 1)
	_, payload := decode(t, w.msgs[0])
	assert.Contains(t, payload["verified_at"], "2026-09-21T10:30:00")
}

// A publish failure is returned, not swallowed. Callers decide whether the
// event mattered; a producer that hid the error would make that impossible.
func TestPublisher_ReturnsWriteErrors(t *testing.T) {
	w := &fakeWriter{err: assertErr("broker unreachable")}
	p := NewPublisher(zap.NewNop(), w)

	err := p.GenerationReady(context.Background(), "obligation", "g-1", "c-1", "idx", "d", "corr")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker unreachable")
}

// A correlation id is always present: an event with none is uncorrelatable,
// and falling back to the event id at least keeps it self-referential rather
// than empty.
func TestPublisher_FallsBackToEventIDForCorrelation(t *testing.T) {
	w := &fakeWriter{}
	p := NewPublisher(zap.NewNop(), w)

	require.NoError(t, p.CheckpointAdvanced(context.Background(),
		"obligation", "idx-1", 1, 0, "g-1", "CURRENT"))

	env, _ := decode(t, w.msgs[0])
	assert.NotEmpty(t, env.CorrelationID)
	assert.Equal(t, env.EventID, env.CorrelationID)
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
