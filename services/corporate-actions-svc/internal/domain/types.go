package domain

import (
	"errors"
	"time"
)

var (
	ErrCorporateActionNotFound = errors.New("corporate action not found")
	ErrActionAlreadyExecuted   = errors.New("corporate action is already executed")
)

type ActionType string

const (
	ActionTypeMerger             ActionType = "MERGER"
	ActionTypeAcquisition        ActionType = "ACQUISITION"
	ActionTypeShareIssuance      ActionType = "SHARE_ISSUANCE"
	ActionTypeShareBuyback       ActionType = "SHARE_BUYBACK"
	ActionTypeRestructure        ActionType = "RESTRUCTURE"
	ActionTypeDividendDeclaration ActionType = "DIVIDEND_DECLARATION"
	ActionTypeNameChange         ActionType = "NAME_CHANGE"
)

type ActionStatus string

const (
	ActionStatusProposed      ActionStatus = "PROPOSED"
	ActionStatusBoardApproved ActionStatus = "BOARD_APPROVED"
	ActionStatusExecuted       ActionStatus = "EXECUTED"
	ActionStatusCancelled      ActionStatus = "CANCELLED"
)

type CorporateAction struct {
	ActionID        string       `json:"action_id"`
	TenantID        string       `json:"tenant_id"`
	LegalEntityID   string       `json:"legal_entity_id"`
	Title           string       `json:"title"`
	ActionType      ActionType   `json:"action_type"`
	Description     string       `json:"description,omitempty"`
	ResolutionID    string       `json:"resolution_id,omitempty"`
	EffectiveDate   string       `json:"effective_date"`
	Status          ActionStatus `json:"status"`
	ValuationAmount float64      `json:"valuation_amount,omitempty"`
	Currency        string       `json:"currency,omitempty"`
	ExecutedAt      *time.Time   `json:"executed_at,omitempty"`
	ExecutedBy      *string      `json:"executed_by,omitempty"`
	DocumentVaultID *string      `json:"document_vault_id,omitempty"`
	EffectiveFrom   string       `json:"effective_from"`
	EffectiveTo     *string      `json:"effective_to,omitempty"`
	CreatedBy       string       `json:"created_by"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

type CreateCorporateActionRequest struct {
	LegalEntityID   string     `json:"legal_entity_id"`
	Title           string     `json:"title"`
	ActionType      ActionType `json:"action_type"`
	Description     string     `json:"description,omitempty"`
	ResolutionID    string     `json:"resolution_id,omitempty"`
	EffectiveDate   string     `json:"effective_date"`
	ValuationAmount float64    `json:"valuation_amount,omitempty"`
	Currency        string     `json:"currency,omitempty"`
	EffectiveFrom   string     `json:"effective_from"`
	CreatedBy       string     `json:"created_by"`
}

type UpdateCorporateActionRequest struct {
	Title           string       `json:"title,omitempty"`
	ActionType      ActionType   `json:"action_type,omitempty"`
	Description     string       `json:"description,omitempty"`
	ResolutionID    string       `json:"resolution_id,omitempty"`
	EffectiveDate   string       `json:"effective_date,omitempty"`
	Status          ActionStatus `json:"status,omitempty"`
	ValuationAmount float64      `json:"valuation_amount,omitempty"`
	Currency        string       `json:"currency,omitempty"`
	EffectiveTo     *string      `json:"effective_to,omitempty"`
	UpdatedBy       string       `json:"updated_by"`
}

type ExecuteCorporateActionRequest struct {
	ExecutedBy      string  `json:"executed_by"`
	DocumentVaultID *string `json:"document_vault_id,omitempty"`
}
