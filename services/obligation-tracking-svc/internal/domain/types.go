package domain

import (
	"errors"
	"time"
)

var (
	ErrObligationNotFound         = errors.New("obligation not found")
	ErrObligationAlreadyFulfilled = errors.New("obligation is already fulfilled")
	ErrObligationAlreadyBreached  = errors.New("obligation is already breached")
)

type ObligationType string

const (
	ObligationTypeContractual   ObligationType = "CONTRACTUAL"
	ObligationTypeRegulatory    ObligationType = "REGULATORY"
	ObligationTypeStatutory     ObligationType = "STATUTORY"
	ObligationTypeInternalPolicy ObligationType = "INTERNAL_POLICY"
)

type ObligationStatus string

const (
	ObligationStatusPending    ObligationStatus = "PENDING"
	ObligationStatusInProgress ObligationStatus = "IN_PROGRESS"
	ObligationStatusFulfilled  ObligationStatus = "FULFILLED"
	ObligationStatusBreached   ObligationStatus = "BREACHED"
	ObligationStatusWaived     ObligationStatus = "WAIVED"
)

type RiskLevel string

const (
	RiskLevelLow      RiskLevel = "LOW"
	RiskLevelMedium   RiskLevel = "MEDIUM"
	RiskLevelHigh     RiskLevel = "HIGH"
	RiskLevelCritical RiskLevel = "CRITICAL"
)

type Obligation struct {
	ObligationID   string           `json:"obligation_id"`
	TenantID       string           `json:"tenant_id"`
	LegalEntityID  string           `json:"legal_entity_id"`
	SourceType     string           `json:"source_type"` // CONTRACT, REGULATION, CLAUSE
	SourceID       string           `json:"source_id"`
	Title          string           `json:"title"`
	Description    string           `json:"description,omitempty"`
	ObligationType ObligationType   `json:"obligation_type"`
	RiskLevel      RiskLevel        `json:"risk_level"`
	Status         ObligationStatus `json:"status"`
	DueDate        string           `json:"due_date"`
	AssignedTo     string           `json:"assigned_to,omitempty"`
	FulfilledAt    *time.Time       `json:"fulfilled_at,omitempty"`
	FulfilledBy    *string          `json:"fulfilled_by,omitempty"`
	FulfillmentNote *string         `json:"fulfillment_note,omitempty"`
	EffectiveFrom  string           `json:"effective_from"`
	EffectiveTo    *string          `json:"effective_to,omitempty"`
	CreatedBy      string           `json:"created_by"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

type CreateObligationRequest struct {
	LegalEntityID  string         `json:"legal_entity_id"`
	SourceType     string         `json:"source_type"`
	SourceID       string         `json:"source_id"`
	Title          string         `json:"title"`
	Description    string         `json:"description,omitempty"`
	ObligationType ObligationType `json:"obligation_type"`
	RiskLevel      RiskLevel      `json:"risk_level"`
	DueDate        string         `json:"due_date"`
	AssignedTo     string         `json:"assigned_to,omitempty"`
	EffectiveFrom  string         `json:"effective_from"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	CreatedBy      string         `json:"created_by"`
}

type UpdateObligationRequest struct {
	Title          string           `json:"title,omitempty"`
	Description    string           `json:"description,omitempty"`
	ObligationType ObligationType   `json:"obligation_type,omitempty"`
	RiskLevel      RiskLevel        `json:"risk_level,omitempty"`
	DueDate        string           `json:"due_date,omitempty"`
	AssignedTo     string           `json:"assigned_to,omitempty"`
	Status         ObligationStatus `json:"status,omitempty"`
	EffectiveTo    *string          `json:"effective_to,omitempty"`
	UpdatedBy      string           `json:"updated_by"`
}

type FulfillObligationRequest struct {
	FulfilledBy     string `json:"fulfilled_by"`
	FulfillmentNote string `json:"fulfillment_note,omitempty"`
}
