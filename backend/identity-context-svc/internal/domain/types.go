// Package domain defines all canonical types for identity-context-svc.
// Field names are verbatim from docs/architecture/04-data-model.md §06.1.
package domain

import "time"

// ---------------------------------------------------------------------------
// Principal  (data-model §06.1)
// ---------------------------------------------------------------------------

// Principal is owned by this service. PII fields (Email, DisplayName) are
// subject to the data_residency_policy_id of the owning tenant.
type Principal struct {
	PrincipalID              string          `json:"principal_id"`
	TenantID                 string          `json:"tenant_id"`
	PrincipalType            PrincipalType   `json:"principal_type"`
	IdentityProviderSubject  string          `json:"identity_provider_subject"`
	Email                    string          `json:"email"`        // PII
	DisplayName              string          `json:"display_name"` // PII
	Status                   PrincipalStatus `json:"status"`
	CreatedAt                time.Time       `json:"created_at"`
}

// ---------------------------------------------------------------------------
// PrincipalRoleAssignment  (data-model §06.1)
// ---------------------------------------------------------------------------

type PrincipalRoleAssignment struct {
	AssignmentID  string     `json:"assignment_id"`
	PrincipalID   string     `json:"principal_id"`
	RoleID        string     `json:"role_id"`
	LegalEntityID *string    `json:"legal_entity_id"` // nullable — entity-scoped
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   time.Time  `json:"effective_to"`
	AssignedBy    string     `json:"assigned_by"`
}

// ---------------------------------------------------------------------------
// DelegatedAuthority  (data-model §06.1)
// ---------------------------------------------------------------------------

type DelegatedAuthority struct {
	DelegatedAuthorityID   string             `json:"delegated_authority_id"`
	DelegatorPrincipalID   string             `json:"delegator_principal_id"`
	DelegatePrincipalID    string             `json:"delegate_principal_id"`
	ScopeType              ScopeType          `json:"scope_type"`
	LegalEntityID          *string            `json:"legal_entity_id"` // nullable
	AuthorityLimitType     AuthorityLimitType `json:"authority_limit_type"`
	AuthorityLimitValue    float64            `json:"authority_limit_value"`
	EffectiveFrom          time.Time          `json:"effective_from"`
	EffectiveTo            time.Time          `json:"effective_to"`
	RevocationStatus       RevocationStatus   `json:"revocation_status"`
}

// ---------------------------------------------------------------------------
// SessionContext  (data-model §06.1 — added per Q1 resolution)
//
// Ephemeral by lifecycle, evidential by obligation.
// Written once per resolved session; NEVER mutated.
// PII-classified: device fingerprints, IP signals, geolocation derivatives.
// Mandatory data_residency_policy_id per doctrine and data-model §06.3.
// ---------------------------------------------------------------------------

type SessionContext struct {
	SessionContextID     string             `json:"session_context_id"`
	PrincipalID          string             `json:"principal_id"`
	TenantID             string             `json:"tenant_id"`
	LegalEntityID        string             `json:"legal_entity_id"`
	CorrelationID        string             `json:"correlation_id"`
	TrustPosture         TrustPosture       `json:"trust_posture"`
	// MFAVerified is a point-in-time attestation only.
	// AuthZ Service evaluates sufficiency; no callback into this service required (Q4).
	MFAVerified          bool               `json:"mfa_verified"`
	DeviceTrustScore     int                `json:"device_trust_score"`
	// AdaptiveRiskScore is sourced from RiskSignalCache ONLY.
	// resolve() NEVER calls the Intelligence Plane or any Tier 2/3 service (Q3).
	AdaptiveRiskScore    int                `json:"adaptive_risk_score"`
	RiskSignalSource     string             `json:"risk_signal_source"`
	// EnvelopeJWTJTI is the JWT ID of the issued IdentityContextEnvelope.
	EnvelopeJWTJTI       string             `json:"envelope_jwt_jti"`
	IssuedAt             time.Time          `json:"issued_at"`
	ExpiresAt            time.Time          `json:"expires_at"`
	// InvalidatedAt is append-only. This record is NEVER deleted.
	InvalidatedAt        *time.Time         `json:"invalidated_at"`
	InvalidationReason   *InvalidationReason `json:"invalidation_reason"`
	// DataResidencyPolicyID is MANDATORY — PII fields are residency-constrained.
	DataResidencyPolicyID string            `json:"data_residency_policy_id"`
	SourceService        string             `json:"source_service"`
	SchemaVersion        string             `json:"schema_version"`
}

// ---------------------------------------------------------------------------
// RiskSignalCache  (data-model §06.1)
//
// Asynchronously populated. Read by resolve() from Redis only.
// Written by a separate async consumer — never on the hot path.
// Superseded entries are linked, not deleted (data-model §06.3).
// ---------------------------------------------------------------------------

type RiskSignalCache struct {
	RiskSignalID  string     `json:"risk_signal_id"`
	PrincipalID   string     `json:"principal_id"`
	TenantID      string     `json:"tenant_id"`
	SignalType    SignalType  `json:"signal_type"`
	SignalValue   int        `json:"signal_value"`
	SignalSource  string     `json:"signal_source"`
	ValidFrom     time.Time  `json:"valid_from"`
	ValidTo       time.Time  `json:"valid_to"`
	SupersededBy  *string    `json:"superseded_by"` // FK to newer record
	CreatedAt     time.Time  `json:"created_at"`
}

// ---------------------------------------------------------------------------
// IdentityContextEnvelope
//
// The signed JWT payload (Q2 resolution — independently verifiable by any
// downstream service). All six dimensions are REQUIRED. Partial envelopes
// are a design-time error — never construct one with zero-values.
// ---------------------------------------------------------------------------

type IdentityContextEnvelope struct {
	// JWT standard fields
	JTI string `json:"jti"`
	ISS string `json:"iss"`
	AUD string `json:"aud"`
	IAT int64  `json:"iat"`
	EXP int64  `json:"exp"`

	// Dimension 1 — authenticated principal
	Principal PrincipalClaims `json:"principal"`

	// Dimension 2 — tenant
	TenantID string `json:"tenant_id"`

	// Dimension 3 — active legal entity
	LegalEntityID string `json:"legal_entity_id"`

	// Dimension 4 — role profile
	RoleProfile RoleProfileClaims `json:"role_profile"`

	// Dimension 5 — delegated authority
	DelegatedAuthority []DelegatedAuthorityClaim `json:"delegated_authority"`

	// Dimension 6 — session trust posture
	SessionTrustPosture SessionTrustClaims `json:"session_trust_posture"`

	// Propagation
	CorrelationID string `json:"correlation_id"`
	SchemaVersion string `json:"schema_version"`
}

type PrincipalClaims struct {
	PrincipalID   string        `json:"principal_id"`
	TenantID      string        `json:"tenant_id"`
	PrincipalType PrincipalType `json:"principal_type"`
	DisplayName   string        `json:"display_name"`
}

type RoleProfileClaims struct {
	RoleAssignments    []RoleAssignmentClaim `json:"role_assignments"`
	PermissionBundleIDs []string             `json:"permission_bundle_ids"`
}

type RoleAssignmentClaim struct {
	RoleID        string  `json:"role_id"`
	LegalEntityID *string `json:"legal_entity_id"`
}

type DelegatedAuthorityClaim struct {
	DelegatedAuthorityID string             `json:"delegated_authority_id"`
	DelegatorPrincipalID string             `json:"delegator_principal_id"`
	ScopeType            ScopeType          `json:"scope_type"`
	LegalEntityID        *string            `json:"legal_entity_id"`
	AuthorityLimitType   AuthorityLimitType `json:"authority_limit_type"`
	AuthorityLimitValue  float64            `json:"authority_limit_value"`
}

type SessionTrustClaims struct {
	Posture           TrustPosture `json:"posture"`
	MFAVerified       bool         `json:"mfa_verified"`
	AdaptiveRiskScore int          `json:"adaptive_risk_score"`
	SessionContextID  string       `json:"session_context_id"`
}

// ---------------------------------------------------------------------------
// Wire types (request/response)
// ---------------------------------------------------------------------------

type ResolveRequest struct {
	BearerToken   string `json:"bearer_token,omitempty"`
	SAMLAssertion string `json:"saml_assertion,omitempty"`
	LegalEntityID string `json:"legal_entity_id"`
	CorrelationID string `json:"correlation_id"`
}

type ResolveResponse struct {
	EnvelopeJWT string `json:"envelope_jwt"`
}

type GetSessionResponse struct {
	EnvelopeJWT string `json:"envelope_jwt"`
}

type InvalidateSessionRequest struct {
	Reason InvalidationReason `json:"reason"`
}

type UpdateStatusRequest struct {
	Status PrincipalStatus `json:"status"`
	Reason string          `json:"reason,omitempty"`
}

// VerifiedClaims is the parsed output of a verified IdP token.
type VerifiedClaims struct {
	Subject  string
	TenantID string
	MFADone  bool
}
