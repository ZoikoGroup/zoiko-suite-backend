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
	// Currency is resolved from BNK-01's verified bank account, never from
	// the request body. The store falls back to the legacy USD value only
	// for direct/in-process callers that do not pass one.
	Currency       string
	RequestedScope []string
	// CorrelationID is the caller-supplied idempotency/dedup key (ON
	// CONFLICT (tenant_id, correlation_id)). RequestCorrelationID is the
	// request-tracing correlation id (X-Correlation-ID) carried on the
	// emitted event — a distinct concern, not reused for the dedup key.
	CreatedByPrincipalID, CorrelationID string
	RequestCorrelationID                string
}

// ScopeSubset reports whether every granted scope was also requested. BNK-02
// forbids expanding scope without reauthorization: an authorization response
// that grants a scope the connection never asked for is a failed handshake,
// not a silent widening of consent.
func ScopeSubset(granted, requested []string) bool {
	requestedSet := make(map[string]struct{}, len(requested))
	for _, s := range requested {
		requestedSet[s] = struct{}{}
	}
	for _, s := range granted {
		if _, ok := requestedSet[s]; !ok {
			return false
		}
	}
	return true
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

// BankRegionPolicy is one tenant+legal-entity-scoped allowed-region entry
// (migration 006). A legal entity with zero policy rows has no configured
// restriction; IsRegionAllowed treats that as unrestricted rather than
// deny-by-default, so this is an opt-in control per legal entity.
type BankRegionPolicy struct {
	PolicyID             string    `json:"policy_id"`
	TenantID             string    `json:"tenant_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	Region               string    `json:"region"`
	CreatedByPrincipalID string    `json:"-"`
	CreatedAt            time.Time `json:"created_at"`
}

type CreateRegionPolicyParams struct {
	TenantID, LegalEntityID, Region, ActorPrincipalID string
}

var (
	ErrRegionPolicyAlreadyExists  = errors.New("this region is already an allowed region for this legal entity")
	ErrConnectionRegionNotAllowed = errors.New("connection region is not an allowed region for its legal entity")
)

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

	// BNK-03 ingestion evidence. These are part of the statement's
	// authoritative record, not handler-local bookkeeping: a caller that
	// cannot see the status, raw-source hash, source/batch ids and
	// quarantine reason cannot prove which import it is looking at or
	// whether the evidence was accepted. The raw payload itself is still
	// not stored (see SPEC_DEVIATIONS.md A10).
	ContentHash          string `json:"content_hash,omitempty"`
	SourceID             string `json:"source_id,omitempty"`
	ImportBatchID        string `json:"import_batch_id,omitempty"`
	Status               string `json:"status,omitempty"`
	QuarantineReason     string `json:"quarantine_reason,omitempty"`
	CreatedByPrincipalID string `json:"created_by_principal_id,omitempty"`
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
