// Package domain defines all canonical types for identity-context-svc.
// Field names are verbatim from docs/architecture/04-data-model.md §06.1.
package domain

// TrustPosture is the session-level trust state attested at resolution time.
//
// ARCHITECTURAL NOTE (Q4 resolution):
//   This is a POINT-IN-TIME attestation produced by identity-context-svc.
//   The Authorization Service decides whether a given posture is SUFFICIENT
//   for a requested action and may return STEP_UP_REQUIRED. The client then
//   triggers a fresh Resolve() after re-authentication, producing a new
//   SessionContext with an elevated posture. No callback path into this
//   service is required or permitted.
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
