// Package domain defines all canonical types for identity-context-svc.
// Field names are verbatim from docs/architecture/04-data-model.md §06.1.
package domain

// TrustPosture is the session-level trust state attested at resolution time.
//
// ARCHITECTURAL NOTE (Q4 resolution):
//
//	This is a POINT-IN-TIME attestation produced by identity-context-svc.
//	The Authorization Service decides whether a given posture is SUFFICIENT
//	for a requested action and may return STEP_UP_REQUIRED. The client then
//	triggers a fresh Resolve() after re-authentication, producing a new
//	SessionContext with an elevated posture. No callback path into this
//	service is required or permitted.
type TrustPosture string

const (
	TrustPostureStandard    TrustPosture = "STANDARD"
	TrustPostureElevated    TrustPosture = "ELEVATED"
	TrustPostureMFAVerified TrustPosture = "MFA_VERIFIED"
	TrustPostureHighRisk    TrustPosture = "HIGH_RISK"
	TrustPostureBlocked     TrustPosture = "BLOCKED"
)

type PrincipalType string

const (
	PrincipalTypeHuman          PrincipalType = "HUMAN"
	PrincipalTypeServiceAccount PrincipalType = "SERVICE_ACCOUNT"
	PrincipalTypeAPIClient      PrincipalType = "API_CLIENT"
)

type PrincipalStatus string

const (
	PrincipalStatusActive    PrincipalStatus = "ACTIVE"
	PrincipalStatusSuspended PrincipalStatus = "SUSPENDED"
	PrincipalStatusDisabled  PrincipalStatus = "DISABLED"
)

type InvalidationReason string

const (
	InvalidationReasonLogout            InvalidationReason = "LOGOUT"
	InvalidationReasonAdminRevoke       InvalidationReason = "ADMIN_REVOKE"
	InvalidationReasonRiskEscalation    InvalidationReason = "RISK_ESCALATION"
	InvalidationReasonDelegationRevoked InvalidationReason = "DELEGATION_REVOKED"
)

// ValidInvalidationReason reports whether r is one of the four the
// session_contexts CHECK constraint accepts.
//
// It exists because the handler used to pass whatever arrived in the body
// straight to the store. An absent or misspelled reason reached Postgres,
// violated session_contexts_invalidation_reason_check, and came back to the
// caller as a 500 "failed to invalidate session" — an error that reads like
// the service is broken when the request simply named no reason. A constraint
// is a backstop, not an input validator.
func ValidInvalidationReason(r InvalidationReason) bool {
	switch r {
	case InvalidationReasonLogout,
		InvalidationReasonAdminRevoke,
		InvalidationReasonRiskEscalation,
		InvalidationReasonDelegationRevoked:
		return true
	}
	return false
}

type ScopeType string

const (
	ScopeTypeEntityScoped ScopeType = "ENTITY_SCOPED"
	ScopeTypeActionScoped ScopeType = "ACTION_SCOPED"
	ScopeTypeGlobal       ScopeType = "GLOBAL"
)

type AuthorityLimitType string

const (
	AuthorityLimitTypeFinancialThreshold AuthorityLimitType = "FINANCIAL_THRESHOLD"
	AuthorityLimitTypeWorkflowCategory   AuthorityLimitType = "WORKFLOW_CATEGORY"
	AuthorityLimitTypeDomainScoped       AuthorityLimitType = "DOMAIN_SCOPED"
)

type RevocationStatus string

const (
	RevocationStatusActive  RevocationStatus = "ACTIVE"
	RevocationStatusRevoked RevocationStatus = "REVOKED"
	RevocationStatusExpired RevocationStatus = "EXPIRED"
)

type SignalType string

const (
	SignalTypeIPReputation  SignalType = "IP_REPUTATION"
	SignalTypeDeviceAnomaly SignalType = "DEVICE_ANOMALY"
	SignalTypeGeoVelocity   SignalType = "GEO_VELOCITY"
	SignalTypeBehavioral    SignalType = "BEHAVIORAL_SCORE"
)
