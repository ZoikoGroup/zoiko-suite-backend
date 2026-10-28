package domain

import "time"

// Governed changes: what closed AU-02, AU-03, AU-13 and AU-17 of the
// 14 Oct 2026 auth-service audit (migrations 000034-000036).
//
//	AU-02  break-glass waits for independent, stepped-up approval
//	AU-03  authority limits are created, changed and retired under maker-checker
//	AU-13  decisions name the session they were made in
//	AU-17  SoD and ABAC rule changes are published only once approved

// Permissions the governed changes check, beside the existing iam.*.manage
// that gate requesting them. Registered in access-control-svc (its 000022).
const (
	// PermissionBreakGlassApprove: GOV-12 ApproveBreakGlass. Never the
	// session's subject or its requester.
	PermissionBreakGlassApprove = "iam.break_glass.approve"
	// PermissionAuthorityLimitManage requests a create, change or retirement.
	PermissionAuthorityLimitManage = "iam.authority_limit.manage"
	// PermissionAuthorityLimitApprove decides one; never the requester.
	PermissionAuthorityLimitApprove = "iam.authority_limit.approve"
	// PermissionSoDRulePublish publishes a requested SoD rule change (Appendix
	// A iam.sod_rule.read/publish); it also decides SoD exceptions.
	PermissionSoDRulePublish = "iam.sod_rule.publish"
	// PermissionPolicyPublish publishes a requested ABAC rule change (Appendix
	// A iam.policy.read/publish).
	PermissionPolicyPublish = "iam.policy.publish"
)

// BreakGlassApprovalWindow bounds how long a break-glass request waits for
// its approvers. An emergency that cannot find an approver in an hour is
// re-declared, not left open.
const BreakGlassApprovalWindow = time.Hour

// Rule publication (000036, AU-17).
const (
	PublicationPending   = "PENDING_APPROVAL"
	PublicationPublished = "PUBLISHED"
	PublicationRejected  = "REJECTED"

	RuleChangePublish    = "PUBLISH"
	RuleChangeReactivate = "REACTIVATE"
	RuleChangeRetire     = "RETIRE"
)

// RulePublication is the governed-publication state of a SoD or ABAC rule.
// ActiveFlag on the rule is what evaluation reads; it changes only when a
// requested change is approved.
type RulePublication struct {
	PublicationStatus string     `json:"publication_status"`
	PendingChange     *string    `json:"pending_change,omitempty"`
	ChangeRequestedBy *string    `json:"change_requested_by,omitempty"`
	ChangeRequestedAt *time.Time `json:"change_requested_at,omitempty"`
	ChangeReason      *string    `json:"change_reason,omitempty"`
	PublishedBy       *string    `json:"published_by,omitempty"`
	PublishedAt       *time.Time `json:"published_at,omitempty"`
	RejectedBy        *string    `json:"rejected_by,omitempty"`
	RejectedAt        *time.Time `json:"rejected_at,omitempty"`
}

// Authority limit states (000035, AU-03).
const (
	AuthorityLimitPending    = "PENDING_APPROVAL"
	AuthorityLimitActive     = "ACTIVE"
	AuthorityLimitRejected   = "REJECTED"
	AuthorityLimitRetired    = "RETIRED"
	AuthorityLimitSuperseded = "SUPERSEDED"

	AuthorityLimitPendingRetire = "RETIRE"
)

// Session kinds on decision evidence (000034, AU-13).
const (
	SessionKindPrivileged = "PRIVILEGED"
	SessionKindBreakGlass = "BREAK_GLASS"
	SessionKindSupport    = "SUPPORT"
)

// SessionActivity is what a reviewer reads about a session: every decision
// made in it, and a summary of what it touched (§13-15 review; GOV-12
// GetEmergencySessionEvidence; §15 "summary of resources accessed").
type SessionActivity struct {
	SessionKind string              `json:"session_kind"`
	SessionID   string              `json:"session_id"`
	Granted     int                 `json:"granted"`
	Denied      int                 `json:"denied"`
	Actions     []string            `json:"actions"`
	Resources   []string            `json:"resources"`
	Decisions   []AccessDecisionLog `json:"decisions"`
	// Truncated is true when more decisions exist than were returned.
	Truncated bool `json:"truncated"`
}

// Errors.
var (
	ErrAlreadyApproved       = errorString("this approver has already approved")
	ErrChangePending         = errorString("a change to this item is already awaiting approval")
	ErrNoPendingChange       = errorString("no change is awaiting approval")
	ErrCheckerNotIndependent = errorString("the checker is the maker")
	ErrRuleRejected          = errorString("a rejected rule cannot be changed")
	ErrAuthorityLimitInvalid = errorString("authority limit is invalid")
)
