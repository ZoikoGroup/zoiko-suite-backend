package domain

import (
	"strings"
	"time"
)

// Session controls (ZS-IAM-001 §13-15; migration 000030).
//
// Action names come in two spellings — the §5 taxonomy (payment.release) and
// the estate's legacy SCREAMING_SNAKE (PAYMENT_RELEASE) — so every classifier
// here works on the name's tokens, split on "." and "_", case-folded.

// Permissions the session controls check, beside the iam.*.manage that gate
// issuing a session at all.
const (
	// PermissionPAMEligible: §13 "Eligibility role identifies who may request
	// specific elevated capabilities". The SUBJECT of a JIT session holds it.
	PermissionPAMEligible = "iam.pam.eligible"
	// PermissionPAMApprove: §13 "independent approval for high-impact
	// privileges". Never the subject or the requester.
	PermissionPAMApprove = "iam.pam.approve"
	// PermissionBreakGlassEligible: §14 "small named security/operations
	// population". The SUBJECT of a break-glass session holds it.
	PermissionBreakGlassEligible = "iam.break_glass.eligible"
	// PermissionSessionReview: §13/§14 retrospective review and attestation.
	PermissionSessionReview = "iam.session.review"
)

// Privileged session states beyond ACTIVE / REVOKED / EXPIRED (000030).
const (
	PrivilegedSessionStatusPendingApproval = "PENDING_APPROVAL"
	PrivilegedSessionStatusRejected        = "REJECTED"
)

// Post-session review states (000030).
const (
	SessionReviewNotRequired = "NOT_REQUIRED"
	SessionReviewPending     = "PENDING"
	SessionReviewAttested    = "ATTESTED"
	SessionReviewFlagged     = "FLAGGED"
)

// PrivilegedSessionApprovalWindow bounds how long a JIT request waits for its
// approver before it expires.
const PrivilegedSessionApprovalWindow = 24 * time.Hour

// Session errors.
var (
	ErrSessionNotPending       = errorString("the session is not awaiting approval")
	ErrSessionReviewNotPending = errorString("the session has no pending review")
)

func actionTokens(action string) []string {
	return strings.FieldsFunc(strings.ToUpper(strings.TrimSpace(action)), func(r rune) bool {
		return r == '.' || r == '_'
	})
}

// financialTokens mark posting, payment and filing powers — the ones §14 says
// "remain separately constrained" under break-glass, and that make a JIT
// grant high-impact.
var financialTokens = map[string]bool{
	"PAYMENT": true, "PAYMENTS": true, "PAYOUT": true, "DISBURSE": true, "DISBURSEMENT": true,
	"REMIT": true, "REMITTANCE": true, "WIRE": true, "RELEASE": true,
	"POST": true, "POSTING": true, "JOURNAL": true,
	"FILE": true, "FILING": true, "SUBMISSION": true,
}

// IsFinancialAction reports whether an action posts, pays or files.
func IsFinancialAction(action string) bool {
	for _, t := range actionTokens(action) {
		if financialTokens[t] {
			return true
		}
	}
	return false
}

// IsHighImpactAction is what makes a JIT session need independent approval:
// access/platform administration (IsPrivilegedAction) or a financial power.
func IsHighImpactAction(action string) bool {
	return IsPrivilegedAction(action) || IsFinancialAction(action)
}

// readVerbs are the final tokens of an action that only reads.
var readVerbs = map[string]bool{
	"READ": true, "VIEW": true, "LIST": true, "GET": true, "SEARCH": true,
	"QUERY": true, "LOOKUP": true, "INSPECT": true, "DISCOVER": true,
}

// IsReadAction reports whether an action only reads: its verb — the last
// token — is a read verb. Anything else is treated as a write, which is the
// closed direction for a read-only support session (§15 "default read-only").
func IsReadAction(action string) bool {
	toks := actionTokens(action)
	return len(toks) > 0 && readVerbs[toks[len(toks)-1]]
}

// IsWildcardAction: "*" (or any action containing it) names everything, which
// §13/§14 "narrowest resources/actions necessary" rules out.
func IsWildcardAction(action string) bool {
	return strings.Contains(action, "*")
}
