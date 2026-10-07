package domain

import "time"

// SoDException is a GOV-04 compensating-control exception: one principal may
// hold both actions of one SoD rule, under a named compensating control, until
// it expires (000027).
type SoDException struct {
	SoDExceptionID      string     `json:"sod_exception_id"`
	TenantID            string     `json:"tenant_id"`
	SoDRuleID           string     `json:"sod_rule_id"`
	PrincipalID         string     `json:"principal_id"`
	CompensatingControl string     `json:"compensating_control"`
	Reason              string     `json:"reason"`
	Status              string     `json:"status"`
	RequestedBy         string     `json:"requested_by"`
	ApprovedBy          *string    `json:"approved_by,omitempty"`
	DecidedAt           *time.Time `json:"decided_at,omitempty"`
	EffectiveFrom       time.Time  `json:"effective_from"`
	ExpiresAt           time.Time  `json:"expires_at"`
	RevokedBy           *string    `json:"revoked_by,omitempty"`
	RevokedAt           *time.Time `json:"revoked_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// SoD exception states (GOV-04: Requested / Approved / Active / Expired /
// Revoked; Active is APPROVED within its period).
const (
	SoDExceptionRequested = "REQUESTED"
	SoDExceptionApproved  = "APPROVED"
	SoDExceptionRejected  = "REJECTED"
	SoDExceptionRevoked   = "REVOKED"
	SoDExceptionExpired   = "EXPIRED"
)

// CreateSoDExceptionParams is the request for an exception.
type CreateSoDExceptionParams struct {
	TenantID            string
	SoDRuleID           string
	PrincipalID         string
	CompensatingControl string
	Reason              string
	RequestedBy         string
	EffectiveFrom       *time.Time
	ExpiresAt           time.Time
}

// ErrSoDExceptionNotFound: no such exception in this tenant.
var ErrSoDExceptionNotFound = errorString("sod exception not found")

// ErrSoDExceptionState: the exception is not in a state the command applies to.
var ErrSoDExceptionState = errorString("sod exception is not in a state this command applies to")
