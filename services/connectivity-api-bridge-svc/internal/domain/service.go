package domain

import (
	"errors"
	"time"
)

var (
	ErrBridgeNotFound = errors.New("bridge endpoint not found")
	ErrInvalidPayload = errors.New("invalid API bridge payload")
)

// Connector lifecycle (ZS-SVC-M-001 INT-05):
// Draft -> PendingApproval -> Active -> Degraded -> Suspended -> Revoked -> Archived.
const (
	StatusDraft           = "DRAFT"
	StatusPendingApproval = "PENDING_APPROVAL"
	StatusActive          = "ACTIVE"
	StatusDegraded        = "DEGRADED"
	StatusSuspended       = "SUSPENDED"
	StatusRevoked         = "REVOKED"
	StatusArchived        = "ARCHIVED"
	// StatusInactive is the pre-lifecycle constant; no code path sets it, and
	// the table still accepts it so no historical row is invalid.
	StatusInactive = "INACTIVE"

	IngestionSuccess = "SUCCESS"
	IngestionFailed  = "FAILED"
)

// Sync modes.
const (
	SyncPull = "PULL"
	SyncPush = "PUSH"
	SyncNone = "NONE"
)

// ApiBridge is the Connector (ZS-SVC-M-001 §9): an approved external-system
// connection configuration. The type keeps its historical name because the REST
// resource and table did; the fields below the status line are the connector
// governance added by migration 003.
type ApiBridge struct {
	BridgeID      string    `json:"bridge_id"`
	ConnectionID  string    `json:"connection_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	BridgeName    string    `json:"bridge_name,omitempty"`
	ProviderName  string    `json:"provider_name,omitempty"`
	Protocol      string    `json:"protocol"`
	EndpointURL   string    `json:"endpoint_url,omitempty"`
	BaseURL       string    `json:"base_url,omitempty"`
	AuthType      string    `json:"auth_type,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// ── Protected configuration: changing any of these needs re-approval. ──
	Purpose                string   `json:"purpose,omitempty"`
	ProviderKey            string   `json:"provider_key,omitempty"`
	ProviderProfileVersion int      `json:"provider_profile_version,omitempty"`
	CredentialRef          string   `json:"credential_ref,omitempty"`
	DestinationRegion      string   `json:"destination_region,omitempty"`
	SyncMode               string   `json:"sync_mode,omitempty"`
	Scopes                 []string `json:"scopes"`

	// DestinationHost is derived from EndpointURL, never supplied.
	DestinationHost string `json:"destination_host,omitempty"`

	// CredentialVerifiedAt is when the credential reference was last confirmed
	// against the secret vault. ConsentExpiresAt, when set, is when the
	// consent/credential behind it lapses.
	CredentialVerifiedAt *time.Time `json:"credential_verified_at,omitempty"`
	ConsentExpiresAt     *time.Time `json:"consent_expires_at,omitempty"`

	// ── Governance state ──
	Version             int64      `json:"version"`
	ConfigFingerprint   string     `json:"config_fingerprint,omitempty"`
	ApprovedFingerprint string     `json:"approved_fingerprint,omitempty"`
	ApprovedVersion     int64      `json:"approved_version,omitempty"`
	CreatedBy           string     `json:"created_by,omitempty"`
	LastChangedBy       string     `json:"last_changed_by,omitempty"`
	ApprovedBy          string     `json:"approved_by,omitempty"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	StatusReason        string     `json:"status_reason,omitempty"`
	// LegacyUnapproved marks a connector that existed before approval did. It
	// keeps working; its first protected change sends it through approval.
	LegacyUnapproved bool `json:"legacy_unapproved,omitempty"`
}

type IngestionLog struct {
	LogID           string    `json:"log_id"`
	BridgeID        string    `json:"bridge_id"`
	TenantID        string    `json:"tenant_id"`
	PayloadSummary  string    `json:"payload_summary"`
	IngestionStatus string    `json:"ingestion_status"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	IngestedAt      time.Time `json:"ingested_at"`
}

// CreateBridgeRequest keeps its historical fields and gains the governed ones.
// Every new field is optional at creation: a DRAFT may be incomplete, and
// completeness is enforced when it is submitted for approval.
type CreateBridgeRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	BridgeName    string `json:"bridge_name"`
	ProviderName  string `json:"provider_name"`
	Protocol      string `json:"protocol"`
	EndpointURL   string `json:"endpoint_url"`
	BaseURL       string `json:"base_url"`
	AuthType      string `json:"auth_type"`

	Purpose                string   `json:"purpose"`
	ProviderKey            string   `json:"provider_key"`
	ProviderProfileVersion int      `json:"provider_profile_version"`
	CredentialRef          string   `json:"credential_ref"`
	DestinationRegion      string   `json:"destination_region"`
	SyncMode               string   `json:"sync_mode"`
	Scopes                 []string `json:"scopes"`
}

type IngestPayloadRequest struct {
	BridgeID       string `json:"bridge_id"`
	PayloadSummary string `json:"payload_summary"`
	RawData        string `json:"raw_data"`
	// Scope is the connector scope this ingestion exercises. Empty means
	// "ingest", the one scope every connector has had since before scopes.
	Scope string `json:"scope"`
}
