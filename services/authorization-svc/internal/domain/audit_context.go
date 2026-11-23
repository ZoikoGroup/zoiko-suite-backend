package domain

import (
	"context"
	"strings"
	"time"
)

// The acting principal, correlation id and stated reason of a request, carried
// on the context to the store. withRLS installs them as app.actor_id,
// app.correlation_id and app.reason in the same statement that sets the
// tenant, and the configuration-history trigger records them (000025), so
// every role, bundle, SoD and ABAC change is attributable and its purpose kept
// (Governance Control Plane §16 "purpose / reason_code").
type auditKey struct{}

type auditContext struct {
	actor, correlation, reason string
}

// WithAudit returns ctx carrying the verified caller and correlation id.
func WithAudit(ctx context.Context, actor, correlationID string) context.Context {
	a := auditFrom(ctx)
	a.actor, a.correlation = actor, correlationID
	return context.WithValue(ctx, auditKey{}, a)
}

// WithReason returns ctx carrying the stated purpose of a privileged or
// destructive command.
func WithReason(ctx context.Context, reason string) context.Context {
	a := auditFrom(ctx)
	a.reason = strings.TrimSpace(reason)
	return context.WithValue(ctx, auditKey{}, a)
}

// AuditFrom returns the actor, correlation id and reason on ctx ("" if unset).
func AuditFrom(ctx context.Context) (actor, correlationID, reason string) {
	a := auditFrom(ctx)
	return a.actor, a.correlation, a.reason
}

func auditFrom(ctx context.Context) auditContext {
	if a, ok := ctx.Value(auditKey{}).(auditContext); ok {
		return a
	}
	return auditContext{}
}

// ── Privileged and protected actions (ZS-IAM-001 §9, §11, Appendix A) ──────

// ProtectedPlatformActions are platform-administration permissions. §9: a
// tenant custom role "cannot include protected platform-admin permissions" —
// so a bundle carrying one can be authored only with the platform-scope grant.
var ProtectedPlatformActions = map[string]bool{
	"SOD_RULE_MANAGE_GLOBAL":  true,
	"ABAC_RULE_MANAGE_GLOBAL": true,
}

// IsProtectedPlatformAction reports whether action is platform administration:
// the two platform-wide rule-authoring grants, and the Appendix A
// Security/Admin families (security.*, platform.*).
func IsProtectedPlatformAction(action string) bool {
	return ProtectedPlatformActions[action] ||
		strings.HasPrefix(action, "security.") || strings.HasPrefix(action, "platform.")
}

// IsPrivilegedAction reports whether action is access administration or
// platform administration. A role carrying one is privileged: assigning it
// needs an independent approver (§9, GOV-12), and it cannot be delegated
// (§11 "protected privileges normally non-delegable").
func IsPrivilegedAction(action string) bool {
	return strings.HasPrefix(action, "iam.") || IsProtectedPlatformAction(action)
}

// Assignment approval states (000025; GOV-12 checker states).
const (
	ApprovalApproved = "APPROVED"
	ApprovalPending  = "PENDING_APPROVAL"
	ApprovalRejected = "REJECTED"
	ApprovalExpired  = "EXPIRED"
	// ApprovalSuspended: an APPROVED assignment withheld without being ended
	// (000037). It grants nothing, because every decision query requires APPROVED.
	ApprovalSuspended = "SUSPENDED"
)

// ErrApprovalNotPending: the assignment is not awaiting a decision.
var ErrApprovalNotPending = errorString("the role assignment is not pending approval")

// ErrApprovalExpired: the pending assignment's approval window has passed.
var ErrApprovalExpired = errorString("the role assignment's approval window has expired")

// Assignment suspension (000037; ORG-06 s4.6 SuspendAssignment).
var (
	// ErrAssignmentNotSuspendable: only an APPROVED assignment that has not
	// ended can be suspended.
	ErrAssignmentNotSuspendable = errorString("only an approved assignment that has not ended can be suspended")
	// ErrAssignmentNotSuspended: only a SUSPENDED assignment can be resumed.
	ErrAssignmentNotSuspended = errorString("the role assignment is not suspended")
	// ErrAssignmentEnded: a suspended assignment whose window has closed cannot
	// be resumed; resuming would grant nothing and would say it had.
	ErrAssignmentEnded = errorString("the role assignment's effective window has ended and it cannot be resumed")
	// ErrSelfResume: nobody lifts the suspension of their own access.
	ErrSelfResume = errorString("a principal cannot resume their own suspended assignment")
)

// AssignmentSuspension is one suspension of an assignment: when, by whom and
// why, and (once closed) when, by whom and why it was lifted.
type AssignmentSuspension struct {
	SuspensionID              string     `json:"suspension_id"`
	PrincipalRoleAssignmentID string     `json:"principal_role_assignment_id"`
	SuspendedBy               string     `json:"suspended_by"`
	SuspendedAt               time.Time  `json:"suspended_at"`
	SuspensionReason          string     `json:"suspension_reason"`
	ResumedBy                 *string    `json:"resumed_by,omitempty"`
	ResumedAt                 *time.Time `json:"resumed_at,omitempty"`
	ResumeReason              *string    `json:"resume_reason,omitempty"`
}

// AssignmentsAsOfQuery asks which assignments were authorizing at one instant.
type AssignmentsAsOfQuery struct {
	TenantID    string
	PrincipalID string // empty = every principal in the tenant
	AsOf        time.Time
}
