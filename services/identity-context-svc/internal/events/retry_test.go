package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// S1-2 / R-3: a revocation whose side effect fails is retried, never lost.
// The claim used to be kept on failure and the offset was already committed,
// so the redelivery (if any) found the event "seen" and skipped it.

func TestFailedEvictionIsRetriedNotLost(t *testing.T) {
	h := newHarness()
	h.sessions.err = errors.New("redis down")
	raw := event(t, "iam.assignment.revoked", "e-retry", "tenant-1",
		map[string]any{"principal_id": "p-subject", "role_id": "role-9", "assignment_id": "a-1"})

	err := h.consumer.Handle(context.Background(), raw)
	require.Error(t, err, "a failed eviction must surface as retryable")
	assert.Empty(t, h.sessions.principals)

	h.sessions.err = nil
	require.NoError(t, h.consumer.Handle(context.Background(), raw), "the retry must not be skipped as a duplicate")
	require.Len(t, h.sessions.principals, 1)
	assert.Equal(t, "p-subject", h.sessions.principals[0].id)

	require.NoError(t, h.consumer.Handle(context.Background(), raw))
	assert.Len(t, h.sessions.principals, 1, "once applied, a redelivery is still deduplicated")
}

// Every holder of an updated role is attempted; one failure fails the event.
func TestRoleUpdatedPartialFailureIsRetried(t *testing.T) {
	h := newHarness()
	h.roles.byRole = map[string][]string{"role-9": {"p-1", "p-2"}}
	h.sessions.err = errors.New("redis down")
	raw := event(t, "role.updated", "e-role", "tenant-1", map[string]any{"role_id": "role-9"})
	require.Error(t, h.consumer.Handle(context.Background(), raw))
	h.sessions.err = nil
	require.NoError(t, h.consumer.Handle(context.Background(), raw))
	assert.Len(t, h.sessions.principals, 2)
}

func TestApplyGivesUpOnlyAfterBoundedAttempts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := newHarness()
	h.sessions.err = errors.New("redis down")
	msg := kafka.Message{Value: event(t, "iam.assignment.revoked", "e-x", "tenant-1", map[string]any{"principal_id": "p"})}
	assert.False(t, h.consumer.ApplyForTest(ctx, msg), "a cancelled context stops the retry without committing")
}
