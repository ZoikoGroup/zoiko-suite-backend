package domain

import (
	"errors"
	"time"
)

var (
	ErrConnectionNotFound = errors.New("bank connection not found")
	ErrStatementNotFound  = errors.New("bank statement not found")
)

const (
	StatusConnected    = "CONNECTED"
	StatusDisconnected = "DISCONNECTED"

	FormatISO20022 = "ISO20022"
	FormatSWIFT    = "SWIFT_MT940"
	FormatBAI2     = "BAI2"
)

type BankConnection struct {
	ConnectionID  string    `json:"connection_id"`
	AccountID     string    `json:"account_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	BankName      string    `json:"bank_name"`
	BIC           string    `json:"bic,omitempty"`
	SwiftBIC      string    `json:"swift_bic,omitempty"`
	AccountNumber string    `json:"account_number,omitempty"`
	IBAN          string    `json:"iban,omitempty"`
	Currency      string    `json:"currency"`
	AccountType   string    `json:"account_type,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// BNK-02 consent/token/health lifecycle — see this package's own doc
	// comment above the status constants for the full state model.
	BankAccountID  string     `json:"bank_account_id,omitempty"`
	ProviderRef    string     `json:"provider_ref,omitempty"`
	ConsentScope   []string   `json:"consent_scope,omitempty"`
	TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`
	HealthStatus   string     `json:"health_status,omitempty"`
	Region         string     `json:"region,omitempty"`
	// TokenLeaseRef is deliberately NEVER included in this struct's JSON
	// output — it is an opaque vault reference, not a secret value, but
	// even a reference has no legitimate reason to leave this service.
	// Kept unexported from JSON via the field below only for internal use.
	TokenLeaseRef string `json:"-"`
}

// BNK-02 consent state machine. REQUESTED is the initial state (an
// InitiateConnection call before any provider/user authorization has
// happened); AUTHORIZING is mid-handshake; ACTIVE is fully usable;
// DEGRADED/SUSPENDED/RECONSENT_REQUIRED are all non-terminal problem
// states a connection can recover from; REVOKED is terminal — see
// migration 003's own reject_revoked_connection_mutation trigger. The
// pre-existing CONNECTED/DISCONNECTED values are untouched, kept for the
// original CreateConnection command's backward compatibility.
const (
	ConnStatusRequested         = "REQUESTED"
	ConnStatusAuthorizing       = "AUTHORIZING"
	ConnStatusActive            = "ACTIVE"
	ConnStatusDegraded          = "DEGRADED"
	ConnStatusSuspended         = "SUSPENDED"
	ConnStatusReconsentRequired = "RECONSENT_REQUIRED"
	ConnStatusRevoked           = "REVOKED"
)

func CanCompleteAuthorization(status string) bool {
	return status == ConnStatusRequested || status == ConnStatusAuthorizing
}
func CanActivate(status string) bool { return status == ConnStatusAuthorizing }
func CanRefresh(status string) bool {
	return status == ConnStatusActive || status == ConnStatusDegraded
}
func CanTriggerReconsent(status string) bool {
	return status == ConnStatusActive || status == ConnStatusDegraded || status == ConnStatusSuspended
}
func CanSuspendConnection(status string) bool {
	return status == ConnStatusActive || status == ConnStatusDegraded
}
func CanRevokeConnection(status string) bool { return status != ConnStatusRevoked }
func CanReconnectProvider(status string) bool {
	return status == ConnStatusDegraded || status == ConnStatusSuspended || status == ConnStatusReconsentRequired
}
func CanRotateCredential(status string) bool {
	return status == ConnStatusActive || status == ConnStatusDegraded
}

// ConnectionEvent is one append-only audit-trail row for the consent
// state machine — see migration 003's reject_connection_event_mutation.
type ConnectionEvent struct {
	EventID          string    `json:"event_id"`
	ConnectionID     string    `json:"connection_id"`
	TenantID         string    `json:"tenant_id"`
	EventType        string    `json:"event_type"`
	Detail           string    `json:"detail,omitempty"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// ── BNK-02 command params ───────────────────────────────────────────────────

type InitiateConnectionParams struct {
	TenantID, LegalEntityID, BankAccountID, ProviderRef, BankName, Region string
	RequestedScope                                                        []string
	CreatedByPrincipalID, CorrelationID                                   string
}

type CompleteConnectionAuthorizationParams struct {
	ConnectionID, TenantID, TokenLeaseRef string
	TokenExpiresAt                        time.Time
	GrantedScope                          []string
	ActorPrincipalID                      string
}

type ActivateConnectionParams struct {
	ConnectionID, TenantID, ActorPrincipalID string
}

type RefreshConnectionParams struct {
	ConnectionID, TenantID, NewTokenLeaseRef string
	NewTokenExpiresAt                        time.Time
	ActorPrincipalID                         string
}

type TriggerReconsentParams struct {
	ConnectionID, TenantID, Reason, ActorPrincipalID string
}

type SuspendConnectionParams struct {
	ConnectionID, TenantID, Reason, ActorPrincipalID string
}

type RevokeConnectionParams struct {
	ConnectionID, TenantID, Reason, ActorPrincipalID string
}

type ReconnectProviderParams struct {
	ConnectionID, TenantID, ActorPrincipalID string
}

type RotateConnectionCredentialParams struct {
	ConnectionID, TenantID, NewTokenLeaseRef string
	NewTokenExpiresAt                        time.Time
	ActorPrincipalID                         string
}

const (
	EventConnectionRequested         = "CONNECTION_REQUESTED"
	EventConnectionAuthorized        = "CONNECTION_AUTHORIZATION_COMPLETED"
	EventConnectionActivated         = "CONNECTION_ACTIVATED"
	EventConnectionRefreshed         = "CONNECTION_REFRESHED"
	EventConnectionReconsentTrigger  = "CONNECTION_RECONSENT_REQUIRED"
	EventConnectionSuspended         = "CONNECTION_SUSPENDED"
	EventConnectionRevoked           = "CONNECTION_REVOKED"
	EventConnectionReconnected       = "CONNECTION_RECONNECTED"
	EventConnectionCredentialRotated = "CONNECTION_CREDENTIAL_ROTATED"
)

var ErrInvalidConnectionTransition = errors.New("bank connection is not in a state that permits this action")

type BankStatement struct {
	StatementID      string    `json:"statement_id"`
	ConnectionID     string    `json:"connection_id"`
	TenantID         string    `json:"tenant_id"`
	StatementFormat  string    `json:"statement_format"`
	StatementDate    time.Time `json:"statement_date"`
	OpeningBalance   float64   `json:"opening_balance"`
	ClosingBalance   float64   `json:"closing_balance"`
	TransactionCount int       `json:"transaction_count"`
	IngestedAt       time.Time `json:"ingested_at"`
}

type CreateConnectionRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	BankName      string `json:"bank_name"`
	BIC           string `json:"bic"`
	SwiftBIC      string `json:"swift_bic"`
	AccountNumber string `json:"account_number"`
	IBAN          string `json:"iban"`
	Currency      string `json:"currency"`
	AccountType   string `json:"account_type"`
}

type IngestStatementRequest struct {
	ConnectionID    string    `json:"connection_id"`
	StatementFormat string    `json:"statement_format"`
	StatementDate   time.Time `json:"statement_date"`
	OpeningBalance  float64   `json:"opening_balance"`
	ClosingBalance  float64   `json:"closing_balance"`
	RawContent      string    `json:"raw_content"`
}
