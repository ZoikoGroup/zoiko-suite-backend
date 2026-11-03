package events_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authzAssignment is authorization-svc's iam.assignment.* payload as
// authz_assignment_events() (migration 000026) builds it: the id is
// principal_role_assignment_id, not assignment_id.
func authzAssignment(id, principal, role string) map[string]any {
	return map[string]any{"principal_role_assignment_id": id, "principal_id": principal, "role_id": role,
		"legal_entity_id": nil, "effective_from": "2026-11-03T10:00:00Z", "effective_to": nil,
		"approval_status": "APPROVED", "assigned_by": "admin-1"}
}

func TestIAM09_AuthzGrantReachesRoleProfile(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.granted", "az-g1", "tenant-1",
		authzAssignment("pra-1", "p-7", "role-5")))

	require.Contains(t, h.roles.projected, "pra-1", "authorization-svc's grant is projected")
	assert.Equal(t, "p-7", h.roles.projected["pra-1"].PrincipalID)
	assert.Contains(t, h.roles.byRole["role-5"], "p-7")
	assert.Empty(t, h.sessions.principals, "a grant only adds access")
}

func TestIAM09_SuspendedAssignmentLeavesProfile(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.granted", "az-g2", "tenant-1",
		authzAssignment("pra-2", "p-8", "role-6")))
	require.Contains(t, h.roles.byRole["role-6"], "p-8")

	h.consumer.Handle(context.Background(), event(t, "iam.assignment.suspended", "az-s2", "tenant-1",
		authzAssignment("pra-2", "p-8", "role-6")))
	assert.NotContains(t, h.roles.byRole["role-6"], "p-8", "a suspended assignment frames no role")
	assert.Equal(t, []string{"pra-2"}, h.roles.suspended)
	assert.Empty(t, h.roles.ended, "a suspension is not a revocation: no tombstone")
	require.Len(t, h.sessions.principals, 1, "the holder's sessions end on suspension")
	assert.Equal(t, "p-8", h.sessions.principals[0].id)

	h.consumer.Handle(context.Background(), event(t, "iam.assignment.resumed", "az-r2", "tenant-1",
		authzAssignment("pra-2", "p-8", "role-6")))
	assert.Contains(t, h.roles.byRole["role-6"], "p-8", "a resumed assignment frames the role again")
}

func TestIAM09_ExpiredAssignmentEndsLikeARevocation(t *testing.T) {
	h := newHarness()
	h.consumer.Handle(context.Background(), event(t, "iam.assignment.expired", "az-e3", "tenant-1",
		authzAssignment("pra-3", "p-9", "role-7")))
	assert.Equal(t, []string{"pra-3"}, h.roles.ended)
	require.Len(t, h.sessions.principals, 1)
}
