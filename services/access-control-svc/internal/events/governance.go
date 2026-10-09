package events

import (
	"time"

	"zoiko.io/access-control-svc/internal/domain"
)

// The canonical IAM domain events (Authorization Standard §23) this service
// emits for its governance surface. Every name here is also in the outbox's
// CHECK constraint (migration 000010) and in asyncapi.yaml.
const (
	TypeRolePublished         = "iam.role.published"
	TypeAssignmentRequested   = "iam.assignment.requested"
	TypeAssignmentGranted     = "iam.assignment.granted"
	TypeAssignmentRevoked     = "iam.assignment.revoked"
	TypeAccessReviewStarted   = "iam.access_review.started"
	TypeAccessReviewCompleted = "iam.access_review.completed"
	// TypeEmploymentChanged is Doc 03 §8.3's employment.changed, published
	// for a LINKED employee's exit (000015 / 000016).
	TypeEmploymentChanged = "employment.changed"
)

// RolePublished builds iam.role.published: "role version activated". Emitted
// when a role is instantiated from a template version and when it is upgraded
// to a newer one, alongside role.created / role.updated, which are the events
// existing consumers (authorization-svc's cache, identity-context-svc's session
// revocation) already act on.
func RolePublished(r domain.RoleDefinition, permittedActions []string, actorID string) (Outbound, error) {
	return Build(TypeRolePublished, r.CorrelationID, r.TenantID, actorID, r.RoleDefinitionID, map[string]any{
		"role_id":           r.RoleDefinitionID,
		"role_code":         r.RoleCode,
		"template_code":     r.TemplateCode,
		"template_version":  r.TemplateVersion,
		"permitted_actions": permittedActions,
	})
}

func assignmentPayload(a domain.AssignmentRequest) map[string]any {
	return map[string]any{
		"request_id":    a.RequestID,
		"assignment_id": a.AuthzAssignmentID,
		// principal_id is the SUBJECT of the assignment, never the actor.
		// identity-context-svc ends this principal's sessions on
		// iam.assignment.revoked; reading the actor instead would log out the
		// administrator doing the revoking.
		"principal_id":        a.TargetPrincipalID,
		"target_principal_id": a.TargetPrincipalID,
		"role_id":             a.RoleDefinitionID,
		"legal_entity_id":     a.LegalEntityID,
		"risk_tier":           a.RiskTier,
		"status":              a.Status,
	}
}

// AssignmentRequested builds iam.assignment.requested: a request is waiting
// for an independent approver.
func AssignmentRequested(a domain.AssignmentRequest, actorID string) (Outbound, error) {
	p := assignmentPayload(a)
	p["approval_reason"] = a.ApprovalReason
	return Build(TypeAssignmentRequested, a.CorrelationID, a.TenantID, actorID, a.RequestID, p)
}

// AssignmentGranted builds iam.assignment.granted: the assignment is effective.
func AssignmentGranted(a domain.AssignmentRequest, actorID string) (Outbound, error) {
	p := assignmentPayload(a)
	p["effective_from"] = a.EffectiveFrom
	if a.EffectiveTo != nil {
		// identity-context-svc projects this as the assignment's end, so the
		// next resolve after it stops framing the role even before the
		// expiry sweep's iam.assignment.revoked arrives.
		p["effective_to"] = *a.EffectiveTo
	}
	return Build(TypeAssignmentGranted, a.CorrelationID, a.TenantID, actorID, assignmentKey(a), p)
}

// AssignmentEnded builds iam.assignment.revoked for an assignment whose
// effective_to has passed (§23: "assignment revoked/expired"). The actor is the
// principal who scheduled the end, or the sweep for an end set at grant time.
func AssignmentEnded(a domain.AssignmentRequest, actorID, reason string) (Outbound, error) {
	p := assignmentPayload(a)
	p["revocation_reason"] = reason
	if a.EffectiveTo != nil {
		p["effective_to"] = *a.EffectiveTo
	}
	return Build(TypeAssignmentRevoked, a.CorrelationID, a.TenantID, actorID, assignmentKey(a), p)
}

// assignmentKey is the Kafka key of an assignment's grant and revocation
// events: authorization-svc's assignment id, the same key a review-driven
// revocation uses (AssignmentRevokedByReview). Keyed by the request id, a
// grant and a review's revoke of the same assignment could land on different
// partitions and reorder, and a late grant re-opened a revoked projection in
// identity-context-svc (Group 1 audit gap S1-4). The request id is the
// fallback for a request never provisioned.
func assignmentKey(a domain.AssignmentRequest) string {
	if a.AuthzAssignmentID != "" {
		return a.AuthzAssignmentID
	}
	return a.RequestID
}

// AssignmentRevoked builds iam.assignment.revoked.
func AssignmentRevoked(a domain.AssignmentRequest, correlationID, actorID, reason string) (Outbound, error) {
	p := assignmentPayload(a)
	p["revocation_reason"] = reason
	return Build(TypeAssignmentRevoked, correlationID, a.TenantID, actorID, assignmentKey(a), p)
}

// AssignmentRevokedByReview builds iam.assignment.revoked for an assignment a
// review item revoked. The assignment may not have come through a request
// here, so the payload is built from the item.
func AssignmentRevokedByReview(it domain.ReviewItem, correlationID, actorID, reason string) (Outbound, error) {
	return Build(TypeAssignmentRevoked, correlationID, it.TenantID, actorID, it.AuthzAssignmentID, map[string]any{
		"assignment_id":       it.AuthzAssignmentID,
		"principal_id":        it.TargetPrincipalID,
		"target_principal_id": it.TargetPrincipalID,
		"role_id":             it.RoleDefinitionID,
		"legal_entity_id":     it.LegalEntityID,
		"review_item_id":      it.ItemID,
		"campaign_id":         it.CampaignID,
		"revocation_reason":   reason,
	})
}

// AccessReviewStarted builds iam.access_review.started.
func AccessReviewStarted(c domain.ReviewCampaign, items int, actorID string) (Outbound, error) {
	return Build(TypeAccessReviewStarted, c.CorrelationID, c.TenantID, actorID, c.CampaignID, map[string]any{
		"campaign_id":   c.CampaignID,
		"campaign_name": c.CampaignName,
		"review_type":   c.ReviewType,
		"due_at":        c.DueAt,
		"items":         items,
	})
}

// AccessReviewCompleted builds iam.access_review.completed, with the outcome
// counts §24 asks the evidence to carry.
func AccessReviewCompleted(c domain.ReviewCampaign, correlationID, actorID string, summary map[string]int) (Outbound, error) {
	return Build(TypeAccessReviewCompleted, correlationID, c.TenantID, actorID, c.CampaignID, map[string]any{
		"campaign_id":   c.CampaignID,
		"campaign_name": c.CampaignName,
		"review_type":   c.ReviewType,
		"summary":       summary,
	})
}

// EmploymentChanged builds employment.changed for a linked employee's exit,
// keyed by the principal so it orders with that principal's other events.
// status_changed_at lets authorization-svc refuse a stale replay.
func EmploymentChanged(l domain.SubjectLink, newStatus, oldStatus, workerType, sourceEventID, correlationID string, at time.Time) (Outbound, error) {
	return Build(TypeEmploymentChanged, correlationID, l.TenantID, "access-control-svc", l.PrincipalID, map[string]any{
		"principal_id":      l.PrincipalID,
		"employee_id":       l.EmployeeID,
		"tenant_id":         l.TenantID,
		"legal_entity_id":   l.LegalEntityID,
		"employment_status": newStatus,
		"previous_status":   oldStatus,
		"worker_type":       workerType,
		"status_changed_at": at.UTC(),
		"source_event_id":   sourceEventID,
		"mapping":           "administered subject link",
	})
}
