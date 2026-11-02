package events

// ORG-06 SuspendAssignment, as this service sees it (authorization-svc
// migration 000037): iam.assignment.suspended and iam.assignment.resumed.

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// RoleAssignmentSuspender holds a projected assignment back from session
// resolution and lifts the hold. Separate from RoleAssignmentProjector so a
// directory that predates suspension keeps satisfying that interface.
type RoleAssignmentSuspender interface {
	SuspendRoleAssignment(ctx context.Context, tenantID, assignmentID string, at time.Time) error
	ResumeRoleAssignment(ctx context.Context, tenantID, assignmentID string, at time.Time) error
}

// assignmentRef is the subject of an assignment event. authorization-svc names
// the assignment principal_role_assignment_id; access-control-svc names it
// assignment_id. Either identifies it, and the principal is the subject, never
// the actor (who is the administrator that suspended it).
type assignmentRef struct {
	PrincipalID               string `json:"principal_id"`
	TargetPrincipalID         string `json:"target_principal_id"`
	AssignmentID              string `json:"assignment_id"`
	PrincipalRoleAssignmentID string `json:"principal_role_assignment_id"`
	RoleID                    string `json:"role_id"`
}

func (a assignmentRef) principal() string { return firstNonEmpty(a.PrincipalID, a.TargetPrincipalID) }
func (a assignmentRef) assignment() string {
	return firstNonEmpty(a.AssignmentID, a.PrincipalRoleAssignmentID)
}

// eventTime is when the change happened: the producer's emitted_at, so that the
// latest of a redelivered or reordered pair wins, not the latest consumed.
func (c *Consumer) eventTime(ev inbound) time.Time {
	if !ev.EmittedAt.IsZero() {
		return ev.EmittedAt.UTC()
	}
	return c.now()
}

// handleAssignmentSuspended holds the assignment back from resolution, then
// ends the subject's sessions.
//
// The projection is closed first, for the reason handleAssignmentRevoked gives:
// the re-resolve that eviction forces must not frame the suspended role back
// into the next envelope. Unlike a revocation nothing is tombstoned, because a
// suspension is reversible.
func (c *Consumer) handleAssignmentSuspended(ctx context.Context, ev inbound) error {
	var p assignmentRef
	_ = json.Unmarshal(ev.Payload, &p)
	principalID := p.principal()
	if principalID == "" {
		c.log.Error("iam.assignment.suspended names no principal — cannot end sessions",
			zap.String("event_id", ev.EventID))
		return nil
	}
	if !c.requireTenant(ev) || !c.claim(ctx, ev.EventID) {
		return nil
	}
	if s, ok := c.roles.(RoleAssignmentSuspender); ok && p.assignment() != "" {
		if err := s.SuspendRoleAssignment(ctx, ev.TenantID, p.assignment(), c.eventTime(ev)); err != nil {
			c.log.Error("failed to suspend projected role assignment — will retry",
				zap.String("assignment_id", p.assignment()), zap.Error(err))
			return c.retry(ctx, ev, err)
		}
	}
	n, err := c.sessions.EvictAllForPrincipal(ctx, principalID, ev.TenantID, domain.InvalidationReasonAdminRevoke)
	if err != nil {
		c.log.Error("failed to revoke sessions after assignment suspension — will retry",
			zap.String("principal_id", principalID),
			zap.String("role_id", p.RoleID),
			zap.String("correlation_id", ev.CorrelationID),
			zap.Error(err))
		return c.retry(ctx, ev, err)
	}
	c.log.Warn("assignment suspended — sessions revoked",
		zap.String("principal_id", principalID),
		zap.String("role_id", p.RoleID),
		zap.Int("sessions_revoked", n),
		zap.String("correlation_id", ev.CorrelationID))
	return nil
}

// handleAssignmentResumed lifts the hold. It revokes nothing: a resume only
// adds access, so the next resolve simply frames the role again.
func (c *Consumer) handleAssignmentResumed(ctx context.Context, ev inbound) error {
	s, ok := c.roles.(RoleAssignmentSuspender)
	if !ok {
		return nil
	}
	var p assignmentRef
	_ = json.Unmarshal(ev.Payload, &p)
	if p.assignment() == "" {
		c.log.Error("iam.assignment.resumed names no assignment — not projected",
			zap.String("event_id", ev.EventID))
		return nil
	}
	if !c.requireTenant(ev) || !c.claim(ctx, ev.EventID) {
		return nil
	}
	if err := s.ResumeRoleAssignment(ctx, ev.TenantID, p.assignment(), c.eventTime(ev)); err != nil {
		c.log.Error("failed to resume projected role assignment — will retry",
			zap.String("assignment_id", p.assignment()), zap.Error(err))
		return c.retry(ctx, ev, err)
	}
	c.log.Info("iam.assignment.resumed — role assignment framed again",
		zap.String("principal_id", p.principal()), zap.String("assignment_id", p.assignment()))
	return nil
}
