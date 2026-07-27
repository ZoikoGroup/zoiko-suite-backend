package domain

import (
	"errors"
	"time"
)

var (
	ErrClauseNotFound   = errors.New("clause not found")
	ErrTemplateNotFound = errors.New("template not found")
	ErrInvalidStatus    = errors.New("invalid status transition")
)

type ClauseCategory string

const (
	ClauseCategoryConfidentiality ClauseCategory = "CONFIDENTIALITY"
	ClauseCategoryIndemnification ClauseCategory = "INDEMNIFICATION"
	ClauseCategoryTermination     ClauseCategory = "TERMINATION"
	ClauseCategoryLiability       ClauseCategory = "LIABILITY"
	ClauseCategoryGoverningLaw    ClauseCategory = "GOVERNING_LAW"
	ClauseCategoryPaymentTerms    ClauseCategory = "PAYMENT_TERMS"
	ClauseCategoryOther           ClauseCategory = "OTHER"
)

type Status string

const (
	StatusDraft    Status = "DRAFT"
	StatusActive   Status = "ACTIVE"
	StatusArchived Status = "ARCHIVED"
)

type Clause struct {
	ClauseID       string         `json:"clause_id"`
	TenantID       string         `json:"tenant_id"`
	LegalEntityID  string         `json:"legal_entity_id"`
	Title          string         `json:"title"`
	Category       ClauseCategory `json:"category"`
	Body           string         `json:"body"`
	Status         Status         `json:"status"`
	Version        int            `json:"version"`
	JurisdictionID string         `json:"jurisdiction_id"`
	EffectiveFrom  string         `json:"effective_from"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type ContractTemplate struct {
	TemplateID     string    `json:"template_id"`
	TenantID       string    `json:"tenant_id"`
	LegalEntityID  string    `json:"legal_entity_id"`
	Title          string    `json:"title"`
	ContractType   string    `json:"contract_type"`
	Description    string    `json:"description,omitempty"`
	ClauseIDs      []string  `json:"clause_ids"`
	Status         Status    `json:"status"`
	Version        int       `json:"version"`
	JurisdictionID string    `json:"jurisdiction_id"`
	EffectiveFrom  string    `json:"effective_from"`
	EffectiveTo    *string   `json:"effective_to,omitempty"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type CreateClauseRequest struct {
	LegalEntityID  string         `json:"legal_entity_id"`
	Title          string         `json:"title"`
	Category       ClauseCategory `json:"category"`
	Body           string         `json:"body"`
	JurisdictionID string         `json:"jurisdiction_id"`
	EffectiveFrom  string         `json:"effective_from"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	CreatedBy      string         `json:"created_by"`
}

type UpdateClauseRequest struct {
	Title          string         `json:"title,omitempty"`
	Category       ClauseCategory `json:"category,omitempty"`
	Body           string         `json:"body,omitempty"`
	JurisdictionID string         `json:"jurisdiction_id,omitempty"`
	EffectiveTo    *string        `json:"effective_to,omitempty"`
	UpdatedBy      string         `json:"updated_by"`
}

type CreateTemplateRequest struct {
	LegalEntityID  string   `json:"legal_entity_id"`
	Title          string   `json:"title"`
	ContractType   string   `json:"contract_type"`
	Description    string   `json:"description,omitempty"`
	ClauseIDs      []string `json:"clause_ids"`
	JurisdictionID string   `json:"jurisdiction_id"`
	EffectiveFrom  string   `json:"effective_from"`
	EffectiveTo    *string  `json:"effective_to,omitempty"`
	CreatedBy      string   `json:"created_by"`
}

type UpdateTemplateRequest struct {
	Title          string   `json:"title,omitempty"`
	ContractType   string   `json:"contract_type,omitempty"`
	Description    string   `json:"description,omitempty"`
	ClauseIDs      []string `json:"clause_ids,omitempty"`
	JurisdictionID string   `json:"jurisdiction_id,omitempty"`
	EffectiveTo    *string  `json:"effective_to,omitempty"`
	UpdatedBy      string   `json:"updated_by"`
}
