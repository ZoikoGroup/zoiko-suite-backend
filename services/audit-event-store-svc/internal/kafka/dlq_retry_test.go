package kafka

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/audit-event-store-svc/internal/consumer"
)

// Regression (2026-11-02 Kafka -> audit run): the first dead-letter of a topic
// failed while "<topic>.dlq" was auto-created, the message was left
// uncommitted, and the next commit on the partition silently dropped it.
func TestRunner_DLQPublishRetried_AcrossTopicCreationRace(t *testing.T) {
	rd, dlq := newFakeReader(msg(9, "evt-bad")), &fakeDLQ{failFirst: 2}
	h := &fakeHandler{calls: map[string]int{}, fn: func(string, int) error {
		return fmt.Errorf("%w: schema_version is missing", consumer.ErrMalformedEvent)
	}}
	m := run(t, rd, dlq, h)

	require.Len(t, dlq.sent, 1, "the message reaches the DLQ once the topic exists")
	assert.Equal(t, 3, dlq.calls, "two failed attempts, then success")
	assert.Equal(t, []int64{9}, rd.committedOffsets(), "committed only after the DLQ publish succeeded")
	assert.Equal(t, float64(1), outcome(m, "malformed_dead_lettered"))
}
