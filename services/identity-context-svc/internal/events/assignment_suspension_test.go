package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
)

// ORG-06 SuspendAssignment as the session service consumes it: a suspension
// ends the subject's sessions and holds the assignment back from resolution; a
// resume lifts the hold and ends nothing.

type suspendCall struct {
	tenantID, assignmentID string
	at                     time.Time
}

// suspendableRoles is fakeRoles plus the suspension projection.
type suspendableRoles struct {
	*fakeRoles
	suspended, resumed []suspendCall
	suspendErr         error
	order              *[]string
}

func (f *suspendableRoles) SuspendRoleAssignment(_ context.Context, tenantID, id string, at time.Time) error {
	if f.order != nil {
		*f.order = append(*f.order, "suspend")
	}
	if f.suspendErr != nil {
		return f.suspendErr
	}
	f.suspended = append(f.suspended, suspendCall{tenantID, id, at})
	return nil
}

func (f *suspendableRoles) ResumeRoleAssignment(_ context.Context, tenantID, id string, at time.Time) error {
	f.resumed = append(f.resumed, suspendCall{tenantID, id, at})
	return nil
}

type orderedRevoker struct {
	*fakeRevoker
	order *[]string
}

func (f *orderedRevoker) EvictAllForPrincipal(ctx context.Context, p, t string, r domain.InvalidationReason) (int, error) {
	*f.order = append(*f.order, "evict")
	return f.fakeRevoker.EvictAllForPrincipal(ctx, p, t, r)
}

func suspHarness() (*events.Consumer, *fakeRevoker, *suspendableRoles, *[]string) {
	order := &[]string{}
	sessions := &fakeRevoker{}
	roles := &suspendableRoles{fakeRoles: &fakeRoles{byRole: map[string][]string{}}, order: order}
	c := events.NewConsumer(zap.NewNop(), &orderedRevoker{sessions, order}, roles, &fakeRisk{}, &fakeHolds{}, newFakeDeduper())
	return c, sessions, roles, order
}

func assignmentEvent(t *testing.T, eventType, eventID string, emitted time.Time, actor string, payload map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"event_id": eventID, "event_type": eventType, "tenant_id": "tenant-1", "actor_id": actor,
		"correlation_id": "corr-1", "emitted_at": emitted, "source_service": "authorization-svc",
		"payload": payload,
	})
	require.NoError(t, err)
	return raw
}

func TestAssignmentSuspended_EndsTheSubjectsSessionsAndHoldsTheProjection(t *testing.T) {
	c, sessions, roles, order := suspHarness()
	at := time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC)

	// authorization-svc's spelling of the assignment id.
	c.Handle(context.Background(), assignmentEvent(t, "iam.assignment.suspended", "s1", at, "admin-who-suspended",
		map[string]any{"principal_role_assignment_id": "a-1", "principal_id": "p-subject", "role_id": "role-9"}))

	require.Len(t, sessions.principals, 1)
	assert.Equal(t, "p-subject", sessions.principals[0].id, "the subject's sessions, never the administrator's")
	assert.Equal(t, "tenant-1", sessions.principals[0].tenantID)
	require.Len(t, roles.suspended, 1)
	assert.Equal(t, suspendCall{"tenant-1", "a-1", at}, roles.suspended[0], "suspended at the event's own time")
	assert.Equal(t, []string{"suspend", "evict"}, *order,
		"the projection is held back BEFORE the eviction, or the forced re-resolve frames the role again")
	assert.Empty(t, roles.ended, "a suspension is reversible: it must not be recorded as a revocation")
}

func TestAssignmentSuspended_AcceptsAccessControlsAssignmentIdToo(t *testing.T) {
	c, _, roles, _ := suspHarness()
	c.Handle(context.Background(), assignmentEvent(t, "iam.assignment.suspended", "s2", time.Now().UTC(), "admin",
		map[string]any{"assignment_id": "a-2", "principal_id": "p-1"}))
	require.Len(t, roles.suspended, 1)
	assert.Equal(t, "a-2", roles.suspended[0].assignmentID)
}

func TestAssignmentSuspended_RefusesToGuess(t *testing.T) {
	c, sessions, roles, _ := suspHarness()
	// No principal: never fall back to the actor, who is the suspender.
	c.Handle(context.Background(), assignmentEvent(t, "iam.assignment.suspended", "s3", time.Now().UTC(), "p-suspender",
		map[string]any{"principal_role_assignment_id": "a-3"}))
	assert.Empty(t, sessions.principals, "fell back to the actor")
	assert.Empty(t, roles.suspended)
}

func TestAssignmentSuspended_IsAppliedOnceWhenRedelivered(t *testing.T) {
	c, sessions, roles, _ := suspHarness()
	raw := assignmentEvent(t, "iam.assignment.suspended", "dup-1", time.Now().UTC(), "admin",
		map[string]any{"principal_role_assignment_id": "a-4", "principal_id": "p-4"})
	c.Handle(context.Background(), raw)
	c.Handle(context.Background(), raw)
	assert.Len(t, sessions.principals, 1)
	assert.Len(t, roles.suspended, 1)
}

// With no emitted_at the consumer's own clock stands in, rather than the event
// being dropped: ending the sessions matters more than the exact instant.
func TestAssignmentSuspended_WithoutATimestampStillActs(t *testing.T) {
	c, sessions, roles, _ := suspHarness()
	raw, _ := json.Marshal(map[string]any{
		"event_id": "s5", "event_type": "iam.assignment.suspended", "tenant_id": "tenant-1",
		"payload": map[string]any{"principal_role_assignment_id": "a-5", "principal_id": "p-5"},
	})
	c.Handle(context.Background(), raw)
	require.Len(t, sessions.principals, 1)
	require.Len(t, roles.suspended, 1)
	assert.False(t, roles.suspended[0].at.IsZero())
}

// A directory that predates suspension still ends the sessions.
func TestAssignmentSuspended_WithoutAProjectionStillEndsSessions(t *testing.T) {
	h := newHarness() // fakeRoles has no SuspendRoleAssignment
	h.consumer.Handle(context.Background(), assignmentEvent(t, "iam.assignment.suspended", "s6", time.Now().UTC(), "admin",
		map[string]any{"principal_role_assignment_id": "a-6", "principal_id": "p-6"}))
	require.Len(t, h.sessions.principals, 1)
}

func TestAssignmentSuspended_ProjectionFailureIsRetriedNotSwallowed(t *testing.T) {
	c, sessions, roles, _ := suspHarness()
	roles.suspendErr = errors.New("db down")
	c.Handle(context.Background(), assignmentEvent(t, "iam.assignment.suspended", "s7", time.Now().UTC(), "admin",
		map[string]any{"principal_role_assignment_id": "a-7", "principal_id": "p-7"}))
	assert.Empty(t, sessions.principals,
		"sessions must not be ended on a half-applied suspension: the retry will do both, in order")
}

func TestAssignmentResumed_LiftsTheHoldAndEndsNothing(t *testing.T) {
	c, sessions, roles, _ := suspHarness()
	at := time.Date(2026, 6, 2, 8, 0, 0, 0, time.UTC)
	c.Handle(context.Background(), assignmentEvent(t, "iam.assignment.resumed", "r1", at, "admin-2",
		map[string]any{"principal_role_assignment_id": "a-1", "principal_id": "p-subject"}))

	require.Len(t, roles.resumed, 1)
	assert.Equal(t, suspendCall{"tenant-1", "a-1", at}, roles.resumed[0])
	assert.Empty(t, sessions.principals, "a resume only adds access")
	assert.Empty(t, roles.suspended)
}

func TestAssignmentResumed_NamingNoAssignmentIsIgnored(t *testing.T) {
	c, _, roles, _ := suspHarness()
	c.Handle(context.Background(), assignmentEvent(t, "iam.assignment.resumed", "r2", time.Now().UTC(), "admin",
		map[string]any{"principal_id": "p-1"}))
	assert.Empty(t, roles.resumed)
}

// A suspended event older than any live session is skipped like a stale
// revocation: ending sessions that cannot exist is noise.
func TestAssignmentSuspended_ADeliveryLongAfterTheFactIsSkipped(t *testing.T) {
	c, sessions, roles, _ := suspHarness()
	c.WithRevocationHorizon(time.Hour)
	c.Handle(context.Background(), assignmentEvent(t, "iam.assignment.suspended", "old-1", time.Now().Add(-48*time.Hour).UTC(), "admin",
		map[string]any{"principal_role_assignment_id": "a-8", "principal_id": "p-8"}))
	assert.Empty(t, sessions.principals)
	assert.Empty(t, roles.suspended)
}
