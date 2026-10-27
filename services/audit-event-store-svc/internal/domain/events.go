package domain

import (
	"encoding/json"
	"time"
)

// QueryEventsParams holds filter criteria for listing audit events.
type QueryEventsParams struct {
	TenantID      string
	LegalEntityID string
	PrincipalID   string
	EventType     string
	CorrelationID string
	FromTime      *time.Time
	ToTime        *time.Time
	Limit         int
	Offset        int
}

// AuditEventRecord is the API representation of an audit event, matching both
// Doc 03 specifications and the frontend AuditEvent contract.
type AuditEventRecord struct {
	ID             string                 `json:"id"`
	EventID        string                 `json:"event_id"`
	EventType      string                 `json:"event_type"`
	Action         string                 `json:"action"`
	TenantID       string                 `json:"tenant_id"`
	LegalEntityID  string                 `json:"legal_entity_id"`
	PrincipalID    string                 `json:"principal_id"`
	PrincipalName  string                 `json:"principal_name,omitempty"`
	Domain         string                 `json:"domain,omitempty"`
	Resource       string                 `json:"resource,omitempty"`
	ResourceID     string                 `json:"resource_id,omitempty"`
	Status         string                 `json:"status,omitempty"`
	SourceService  string                 `json:"source_service"`
	SchemaVersion  string                 `json:"schema_version"`
	Payload        json.RawMessage        `json:"payload"`
	StoredAt       time.Time              `json:"stored_at"`
	Timestamp      string                 `json:"timestamp"`
	CorrelationID  string                 `json:"correlation_id"`
	CausationID    string                 `json:"causation_id,omitempty"`
	SequenceNumber int64                  `json:"sequence_number"`
	PayloadHash    string                 `json:"payload_hash"`
	HashSignature  string                 `json:"hash_signature"`
	PreviousHash   string                 `json:"previous_hash"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
}

// QueryEventsResult wraps the queried audit events along with total count
// and hash-chain validity.
type QueryEventsResult struct {
	Events         []AuditEventRecord `json:"events"`
	Total          int64              `json:"total"`
	HashChainValid bool               `json:"hash_chain_valid"`
}
