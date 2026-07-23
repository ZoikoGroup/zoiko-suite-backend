package domain

import (
	"errors"
	"time"
)

var (
	ErrBridgeNotFound = errors.New("bridge endpoint not found")
	ErrInvalidPayload = errors.New("invalid API bridge payload")
)

const (
	StatusActive   = "ACTIVE"
	StatusInactive = "INACTIVE"

	IngestionSuccess = "SUCCESS"
	IngestionFailed  = "FAILED"
)

type ApiBridge struct {
	BridgeID      string    `json:"bridge_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	BridgeName    string    `json:"bridge_name"`
	Protocol      string    `json:"protocol"`
	EndpointURL   string    `json:"endpoint_url"`
	AuthType      string    `json:"auth_type"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
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

type CreateBridgeRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	BridgeName    string `json:"bridge_name"`
	Protocol      string `json:"protocol"`
	EndpointURL   string `json:"endpoint_url"`
	AuthType      string `json:"auth_type"`
}

type IngestPayloadRequest struct {
	BridgeID       string `json:"bridge_id"`
	PayloadSummary string `json:"payload_summary"`
	RawData        string `json:"raw_data"`
}
