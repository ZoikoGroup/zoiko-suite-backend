//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/store"
)

// Migration 000014 and pg_groups.go, through the NOBYPASSRLS app pool.

func TestGroupsAreTenantIsolatedWithLiveMembershipHistory(t *testing.T) {
	s := store.New(appPool)
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantGov)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	now := time.Now().UTC()
	g := &domain.Group{GroupID: uuid.NewString(), LegalEntityID: "le-1", GroupCode: "G_" + uuid.NewString()[:8], GroupName: "AP",
		Status: domain.GroupActive, Source: domain.GroupManual, CreatedByPrincipalID: "admin", CorrelationID: uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	created, err := s.CreateGroup(ctxA, g)
	require.NoError(t, err)
	require.True(t, created)

	replay := *g
	replay.GroupID = uuid.NewString()
	created, err = s.CreateGroup(ctxA, &replay)
	require.NoError(t, err)
	require.False(t, created, "a correlation replay answers the stored group")
	require.Equal(t, g.GroupID, replay.GroupID)

	dup := *g
	dup.GroupID, dup.CorrelationID = uuid.NewString(), uuid.NewString()
	_, err = s.CreateGroup(ctxA, &dup)
	require.ErrorIs(t, err, domain.ErrGroupCodeExists, "a duplicate code is a conflict, not an outage")

	_, err = s.GetGroup(ctxB, g.GroupID)
	require.ErrorIs(t, err, domain.ErrGroupNotFound, "another tenant cannot see it")
	_, err = s.AddGroupMember(ctxB, g.GroupID, "intruder", "admin-b")
	require.ErrorIs(t, err, domain.ErrGroupNotFound, "nor add to it")

	_, err = s.AddGroupMember(ctxA, g.GroupID, "user-1", "admin")
	require.NoError(t, err)
	_, err = s.AddGroupMember(ctxA, g.GroupID, "user-1", "admin")
	require.ErrorIs(t, err, domain.ErrGroupMemberExists, "one live membership per principal")

	require.NoError(t, s.RemoveGroupMember(ctxA, g.GroupID, "user-1", "admin", "moved"))
	require.ErrorIs(t, s.RemoveGroupMember(ctxA, g.GroupID, "user-1", "admin", "again"), domain.ErrGroupMemberNotFound)
	m, err := s.AddGroupMember(ctxA, g.GroupID, "user-1", "admin")
	require.NoError(t, err, "a former member can re-join; history is kept")
	require.False(t, m.AddedAt.IsZero())

	got, err := s.GetGroup(ctxA, g.GroupID)
	require.NoError(t, err)
	require.Len(t, got.Members, 1)
	var rows int
	require.NoError(t, ownerPool.QueryRow(context.Background(), `SELECT count(*) FROM iam_group_members WHERE group_id = $1`, g.GroupID).Scan(&rows))
	require.Equal(t, 2, rows, "the ended membership is kept as history")
}

func TestGroupAssignmentLinksItsMemberRequests(t *testing.T) {
	s := store.New(appPool)
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)
	roleID := seedRole(t, tenantGov, "GRP_"+uuid.NewString()[:8])
	now := time.Now().UTC()
	g := &domain.Group{GroupID: uuid.NewString(), LegalEntityID: "le-1", GroupCode: "G_" + uuid.NewString()[:8], GroupName: "AP",
		Status: domain.GroupActive, Source: domain.GroupSCIM, CreatedByPrincipalID: "admin", CorrelationID: uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	_, err := s.CreateGroup(ctx, g)
	require.NoError(t, err)

	ga := &domain.GroupAssignment{GroupAssignmentID: uuid.NewString(), GroupID: g.GroupID, RoleDefinitionID: roleID, LegalEntityID: "le-1",
		EffectiveFrom: now, Justification: "AP", Status: domain.GroupAssignmentActive, CreatedByPrincipalID: "admin", CorrelationID: uuid.NewString(), CreatedAt: now}
	created, err := s.CreateGroupAssignment(ctx, ga)
	require.NoError(t, err)
	require.True(t, created)
	prior, err := s.FindGroupAssignmentByCorrelation(ctx, ga.CorrelationID)
	require.NoError(t, err)
	require.Equal(t, ga.GroupAssignmentID, prior.GroupAssignmentID)

	a := &domain.AssignmentRequest{RequestID: uuid.NewString(), TenantID: tenantGov, TargetPrincipalID: "user-1",
		RoleDefinitionID: roleID, LegalEntityID: "le-1", EffectiveFrom: now, Justification: "AP",
		RiskTier: domain.RiskStandard, Status: domain.AssignmentPendingApproval, RequestedByPrincipalID: "admin",
		CorrelationID: "grp-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now, GroupAssignmentID: ga.GroupAssignmentID}
	_, err = s.CreateAssignmentRequest(ctx, a, "admin")
	require.NoError(t, err)

	reqs, err := s.ListGroupAssignmentRequests(ctx, ga.GroupAssignmentID, "user-1")
	require.NoError(t, err)
	require.Len(t, reqs, 1)
	require.Equal(t, ga.GroupAssignmentID, reqs[0].GroupAssignmentID, "the link survives the round trip")
	reqs, err = s.ListGroupAssignmentRequests(ctx, ga.GroupAssignmentID, "user-2")
	require.NoError(t, err)
	require.Empty(t, reqs)

	active, err := s.ListGroupAssignments(ctx, g.GroupID, true)
	require.NoError(t, err)
	require.Len(t, active, 1)
	out, err := s.MarkGroupAssignmentRevoked(ctx, ga.GroupAssignmentID, "admin", "disbanded")
	require.NoError(t, err)
	require.Equal(t, domain.GroupAssignmentRevoked, out.Status)
	_, err = s.MarkGroupAssignmentRevoked(ctx, ga.GroupAssignmentID, "admin", "again")
	require.ErrorIs(t, err, domain.ErrGroupAssignmentRevoked)
	active, err = s.ListGroupAssignments(ctx, g.GroupID, true)
	require.NoError(t, err)
	require.Empty(t, active, "a revoked group assignment is not fanned out to new members")
}

// Migration 000015: links are tenant-owned, re-linkable, and an observation
// returns what was recorded before it.
func TestSubjectLinksObserveReturnsThePriorState(t *testing.T) {
	s := store.New(appPool)
	ctxA := svcmiddleware.WithTenant(context.Background(), tenantGov)
	ctxB := svcmiddleware.WithTenant(context.Background(), tenantB)
	emp := "E-" + uuid.NewString()[:8]
	l := &domain.SubjectLink{EmployeeID: emp, PrincipalID: "user-1", LegalEntityID: "le-1", LinkedByPrincipalID: "admin", CorrelationID: uuid.NewString()}
	require.NoError(t, s.UpsertSubjectLink(ctxA, l))
	require.Equal(t, tenantGov, l.TenantID)

	got, err := s.FindSubjectLink(ctxB, emp)
	require.NoError(t, err)
	require.Nil(t, got, "another tenant cannot resolve the link")
	none, err := s.ObserveSubject(ctxB, emp, "M-1", "ACTIVE")
	require.NoError(t, err)
	require.Nil(t, none, "nor record against it")

	before, err := s.ObserveSubject(ctxA, emp, "M-1", "ACTIVE")
	require.NoError(t, err)
	require.Empty(t, before.LastManagerEmployeeID)
	before, err = s.ObserveSubject(ctxA, emp, "M-2", "")
	require.NoError(t, err)
	require.Equal(t, "M-1", before.LastManagerEmployeeID)
	require.Equal(t, "ACTIVE", before.LastStatus)

	l2 := &domain.SubjectLink{EmployeeID: emp, PrincipalID: "user-2", LegalEntityID: "le-1", LinkedByPrincipalID: "admin", CorrelationID: uuid.NewString()}
	require.NoError(t, s.UpsertSubjectLink(ctxA, l2))
	require.Equal(t, "user-2", l2.PrincipalID, "re-linking replaces the principal")
	require.Equal(t, "M-2", l2.LastManagerEmployeeID, "and keeps what was observed")
}
