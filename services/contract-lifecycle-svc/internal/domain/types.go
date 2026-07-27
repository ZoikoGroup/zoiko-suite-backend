package domain

import (
	"errors"
	"time"
)

// Sentinel errors
var (
	ErrContractNotFound     = errors.New("contract not found")
	ErrContractNotDraft     = errors.New("contract is not in DRAFT status")
	ErrContractAlreadyActive = errors.New("contract is already ACTIVE")
	ErrContractTerminated   = errors.New("contract is already TERMINATED")
	ErrVersionConflict      = errors.New("contract version conflict")
)

// ContractType enumerates the kinds of legal agreements supported.
type ContractType string

const (
	ContractTypeVendor      ContractType = "VENDOR"
	ContractTypeEmployment  ContractType = "EMPLOYMENT"
	ContractTypeNDA         ContractType = "NDA"
	ContractTypeMSA         ContractType = "MSA"
	ContractTypeSLA         ContractType = "SLA"
	ContractTypePartnership ContractType = "PARTNERSHIP"
	ContractTypeOther       ContractType = "OTHER"
)

// ContractStatus represents the lifecycle state of a contract.
type ContractStatus string

const (
	ContractStatusDraft           ContractStatus = "DRAFT"
	ContractStatusPendingApproval ContractStatus = "PENDING_APPROVAL"
	ContractStatusActive          ContractStatus = "ACTIVE"
	ContractStatusExpired         ContractStatus = "EXPIRED"
	ContractStatusTerminated      ContractStatus = "TERMINATED"
	ContractStatusSuspended       ContractStatus = "SUSPENDED"
)

// Contract is the core domain entity representing a legal agreement.
// All material records carry tenant_id, legal_entity_id, and effective dates.
type Contract struct {
	ContractID      string         `json:"contract_id"`
	TenantID        string         `json:"tenant_id"`
	LegalEntityID   string         `json:"legal_entity_id"`
	ContractType    ContractType   `json:"contract_type"`
	Title           string         `json:"title"`
	Description     string         `json:"description,omitempty"`
	CounterpartyID  string         `json:"counterparty_id"`
	CounterpartyName string        `json:"counterparty_name"`
	Status          ContractStatus `json:"status"`
	Version         int            `json:"version"`
	EffectiveFrom   string         `json:"effective_from"`
	EffectiveTo     *string        `json:"effective_to,omitempty"`
	SignedAt        *time.Time     `json:"signed_at,omitempty"`
	SignedBy        *string        `json:"signed_by,omitempty"`
	TerminatedAt    *time.Time     `json:"terminated_at,omitempty"`
	TerminatedBy    *string        `json:"terminated_by,omitempty"`
	TerminationNote *string        `json:"termination_note,omitempty"`
	Currency        string         `json:"currency"`
	TotalValue      float64        `json:"total_value"`
	DocumentVaultID *string        `json:"document_vault_id,omitempty"`
	CreatedBy       string         `json:"created_by"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// ContractVersion is an immutable snapshot of a contract at a point in time.
type ContractVersion struct {
	VersionID       string         `json:"version_id"`
	ContractID      string         `json:"contract_id"`
	TenantID        string         `json:"tenant_id"`
	VersionNumber   int            `json:"version_number"`
	Status          ContractStatus `json:"status"`
	Title           string         `json:"title"`
	Description     string         `json:"description,omitempty"`
	EffectiveFrom   string         `json:"effective_from"`
	EffectiveTo     *string        `json:"effective_to,omitempty"`
	ChangeSummary   string         `json:"change_summary"`
	CreatedBy       string         `json:"created_by"`
	CreatedAt       time.Time      `json:"created_at"`
}

// --- Request / Response DTOs ---

type CreateContractRequest struct {
	LegalEntityID    string       `json:"legal_entity_id"`
	ContractType     ContractType `json:"contract_type"`
	Title            string       `json:"title"`
	Description      string       `json:"description,omitempty"`
	CounterpartyID   string       `json:"counterparty_id"`
	CounterpartyName string       `json:"counterparty_name"`
	EffectiveFrom    string       `json:"effective_from"`
	EffectiveTo      *string      `json:"effective_to,omitempty"`
	Currency         string       `json:"currency"`
	TotalValue       float64      `json:"total_value"`
	CreatedBy        string       `json:"created_by"`
}

type UpdateContractRequest struct {
	Title            string       `json:"title,omitempty"`
	Description      string       `json:"description,omitempty"`
	CounterpartyName string       `json:"counterparty_name,omitempty"`
	EffectiveTo      *string      `json:"effective_to,omitempty"`
	Currency         string       `json:"currency,omitempty"`
	TotalValue       float64      `json:"total_value,omitempty"`
	ChangeSummary    string       `json:"change_summary"`
	UpdatedBy        string       `json:"updated_by"`
}

type SubmitForApprovalRequest struct {
	SubmittedBy string `json:"submitted_by"`
}

type ActivateContractRequest struct {
	SignedBy        string     `json:"signed_by"`
	SignedAt        time.Time  `json:"signed_at"`
	DocumentVaultID *string    `json:"document_vault_id,omitempty"`
}

type TerminateContractRequest struct {
	TerminatedBy    string `json:"terminated_by"`
	TerminationNote string `json:"termination_note"`
}
