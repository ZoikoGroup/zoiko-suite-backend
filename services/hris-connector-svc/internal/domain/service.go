package domain

import (
	"errors"
	"time"
)

var (
	ErrIntegrationNotFound = errors.New("HRIS integration not found")
	ErrSyncJobNotFound     = errors.New("HRIS sync job not found")
)

const (
	ProviderWorkday        = "WORKDAY"
	ProviderSuccessFactors = "SUCCESSFACTORS"
	ProviderADP            = "ADP"
	ProviderGeneric        = "GENERIC_HRIS"

	SyncPending    = "PENDING"
	SyncInProgress = "IN_PROGRESS"
	SyncCompleted  = "COMPLETED"
	SyncFailed     = "FAILED"
)

type HrisIntegration struct {
	IntegrationID string    `json:"integration_id"`
	SyncID        string    `json:"sync_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	ProviderName  string    `json:"provider_name,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	SyncType      string    `json:"sync_type,omitempty"`
	Direction     string    `json:"direction,omitempty"`
	ApiEndpoint   string    `json:"api_endpoint,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type SyncJob struct {
	JobID         string     `json:"job_id"`
	SyncID        string     `json:"sync_id"`
	IntegrationID string     `json:"integration_id"`
	TenantID      string     `json:"tenant_id"`
	SyncType      string     `json:"sync_type"`
	RecordsSynced int        `json:"records_synced"`
	Status        string     `json:"status"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

type CreateIntegrationRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	ProviderName  string `json:"provider_name"`
	Provider      string `json:"provider"`
	SyncType      string `json:"sync_type"`
	Direction     string `json:"direction"`
	ApiEndpoint   string `json:"api_endpoint"`
}

type TriggerSyncRequest struct {
	IntegrationID string `json:"integration_id"`
	SyncType      string `json:"sync_type"`
}
