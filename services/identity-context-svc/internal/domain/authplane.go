package domain

import (
	"errors"
	"time"
)

// ── Login sessions, codes and refresh tokens (AU-01, AU-06, AU-10) ──────────

// AuthSession is one completed login (OIDC sid).
type AuthSession struct {
	SID            string
	TenantID       string
	PrincipalID    string
	ClientID       string
	AuthTime       time.Time
	AMR            []string
	AssuranceLevel string
	IdPID          string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	EndedAt        *time.Time
	EndReason      string
	ClientIPHash   string
}

// Live reports whether the session has neither ended nor expired at now.
func (s *AuthSession) Live(now time.Time) bool {
	return s != nil && s.EndedAt == nil && now.Before(s.ExpiresAt)
}

// Reasons a login session ends.
const (
	SessionEndLogout          = "LOGOUT"
	SessionEndRefreshReuse    = "REFRESH_TOKEN_REUSE"
	SessionEndCodeReuse       = "AUTHORIZATION_CODE_REUSE"
	SessionEndDeprovisioned   = "DEPROVISIONED"
	SessionEndPasswordChanged = "PASSWORD_CHANGED"
	SessionEndAdminRevoke     = "ADMIN_REVOKE"
	SessionEndPrincipalStatus = "PRINCIPAL_STATUS_CHANGED"
)

// AuthorizationCode is a stored (hashed) OAuth authorization code.
type AuthorizationCode struct {
	CodeHash            string
	TenantID            string
	ClientID            string
	RedirectURI         string
	SID                 string
	PrincipalID         string
	Scope               string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	DPoPJKT             string
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
}

// RefreshToken is a stored (hashed) refresh token in a rotation family.
type RefreshToken struct {
	TokenHash       string
	FamilyID        string
	TenantID        string
	PrincipalID     string
	ClientID        string
	SID             string
	Scope           string
	DPoPJKT         string
	IssuedAt        time.Time
	ExpiresAt       time.Time
	FamilyExpiresAt time.Time
	UsedAt          *time.Time
	RevokedAt       *time.Time
	RevokeReason    string
}

// Errors for the OAuth artefacts.
var (
	ErrCodeInvalid         = errors.New("authorization code invalid, expired or already used")
	ErrCodeReplayed        = errors.New("authorization code replayed")
	ErrRefreshInvalid      = errors.New("refresh token invalid or expired")
	ErrRefreshReused       = errors.New("refresh token reused")
	ErrLoginSessionEnded   = errors.New("login session ended")
	ErrCredentialTokenUsed = errors.New("link invalid, expired or already used")
)

// ── Subject lifecycle (AU-09) ────────────────────────────────────────────────

// PrincipalStatusPendingActivation is a subject created and invited but not
// yet activated: it cannot authenticate.
const PrincipalStatusPendingActivation PrincipalStatus = "PENDING_ACTIVATION"

// Principal sources.
const (
	PrincipalSourceLocal      = "LOCAL"
	PrincipalSourceSCIM       = "SCIM"
	PrincipalSourceFederation = "FEDERATION_JIT"
)

// NewPrincipal is a subject to create.
type NewPrincipal struct {
	PrincipalID   string
	TenantID      string
	PrincipalType PrincipalType
	Email         string
	DisplayName   string
	ExternalID    string
	Source        string
	Status        PrincipalStatus
	CreatedBy     string
	// IdentityProviderSubject defaults to "local:<principal_id>".
	IdentityProviderSubject string
}

// Credential token purposes.
const (
	CredentialTokenActivation = "ACTIVATION"
	CredentialTokenReset      = "RESET"
)

// CredentialToken is a one-time activation or reset link (hashed).
type CredentialToken struct {
	TokenHash   string
	TenantID    string
	PrincipalID string
	Purpose     string
	RequestID   string
	ExpiresAt   time.Time
}

// PasswordResetRequest is an administrator-initiated reset awaiting
// independent approval.
type PasswordResetRequest struct {
	RequestID         string               `json:"request_id"`
	TenantID          string               `json:"tenant_id"`
	PrincipalID       string               `json:"principal_id"`
	RequestedBy       string               `json:"requested_by"`
	Reason            string               `json:"reason"`
	ApprovalsRequired int                  `json:"approvals_required"`
	Approvals         []ResetApproval      `json:"approvals"`
	Status            string               `json:"status"`
	CreatedAt         time.Time            `json:"created_at"`
	DecidedAt         *time.Time           `json:"decided_at,omitempty"`
	ExpiresAt         time.Time            `json:"expires_at"`
}

// ResetApproval is one approver's decision.
type ResetApproval struct {
	ApproverID string    `json:"approver_id"`
	At         time.Time `json:"at"`
	Decision   string    `json:"decision"`
}

// ── Federation (AU-07) ───────────────────────────────────────────────────────

// IdentityProvider is a tenant's enterprise IdP.
type IdentityProvider struct {
	IdPID           string    `json:"idp_id"`
	TenantID        string    `json:"tenant_id"`
	Protocol        string    `json:"protocol"`
	DisplayName     string    `json:"display_name"`
	Issuer          string    `json:"issuer,omitempty"`
	ClientID        string    `json:"client_id,omitempty"`
	ClientSecretRef string    `json:"client_secret_ref,omitempty"`
	Scopes          string    `json:"scopes,omitempty"`
	SAMLEntityID    string    `json:"saml_idp_entity_id,omitempty"`
	SAMLSSOURL      string    `json:"saml_sso_url,omitempty"`
	SAMLCertPEM     string    `json:"saml_cert_pem,omitempty"`
	ClaimEmail      string    `json:"claim_email"`
	ClaimName       string    `json:"claim_name"`
	ClaimGroups     string    `json:"claim_groups,omitempty"`
	JITProvisioning bool      `json:"jit_provisioning"`
	LinkByEmail     bool      `json:"link_by_email"`
	AllowedDomains  []string  `json:"allowed_domains"`
	Status          string    `json:"status"`
	Version         int       `json:"version"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// FederatedIdentity binds an IdP subject to a local principal.
type FederatedIdentity struct {
	IdPID           string
	ExternalSubject string
	TenantID        string
	PrincipalID     string
	Email           string
}

// ── SCIM (AU-08) ─────────────────────────────────────────────────────────────

// SCIMConnection authenticates one tenant's provisioning client.
type SCIMConnection struct {
	ConnectionID string     `json:"connection_id"`
	TenantID     string     `json:"tenant_id"`
	IdPID        string     `json:"idp_id,omitempty"`
	Status       string     `json:"status"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSyncAt   *time.Time `json:"last_sync_at,omitempty"`
}

// Group is an administrative collection of subjects.
type Group struct {
	GroupID     string    `json:"group_id"`
	TenantID    string    `json:"tenant_id"`
	DisplayName string    `json:"display_name"`
	ExternalID  string    `json:"external_id,omitempty"`
	Source      string    `json:"source"`
	Members     []string  `json:"members,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GroupRoleMapping maps a group to a candidate role assignment.
type GroupRoleMapping struct {
	MappingID     string     `json:"mapping_id"`
	TenantID      string     `json:"tenant_id"`
	GroupID       string     `json:"group_id"`
	RoleID        string     `json:"role_id"`
	LegalEntityID string     `json:"legal_entity_id,omitempty"`
	Status        string     `json:"status"`
	Version       int        `json:"version"`
	Reason        string     `json:"reason"`
	ProposedBy    string     `json:"proposed_by"`
	ProposedAt    time.Time  `json:"proposed_at"`
	DecidedBy     string     `json:"decided_by,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
}

// Mapping statuses.
const (
	MappingProposed = "PROPOSED"
	MappingActive   = "ACTIVE"
	MappingRejected = "REJECTED"
	MappingRetired  = "RETIRED"
)

// ── WebAuthn (AU-11) ─────────────────────────────────────────────────────────

// WebAuthnCredential is a registered passkey / security key.
type WebAuthnCredential struct {
	CredentialID    string     `json:"credential_id"`
	TenantID        string     `json:"tenant_id"`
	PrincipalID     string     `json:"principal_id"`
	PublicKey       []byte     `json:"-"`
	AttestationType string     `json:"attestation_type"`
	AAGUID          string     `json:"aaguid,omitempty"`
	SignCount       uint32     `json:"sign_count"`
	Transports      []string   `json:"transports"`
	BackupEligible  bool       `json:"backup_eligible"`
	BackupState     bool       `json:"backup_state"`
	UserVerified    bool       `json:"user_verified"`
	Nickname        string     `json:"nickname,omitempty"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"created_at"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
}
