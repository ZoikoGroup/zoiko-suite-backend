package domain

import (
	"errors"
	"time"
)

var (
	ErrInterfaceNotFound = errors.New("tax authority interface not found")
	ErrFilingNotFound    = errors.New("tax filing submission not found")
)

const (
	TaxFilingPending   = "PENDING"
	TaxFilingSubmitted = "SUBMITTED"
	TaxFilingAccepted  = "ACCEPTED"
	TaxFilingRejected  = "REJECTED"
)

type TaxInterface struct {
	InterfaceID   string    `json:"interface_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	Jurisdiction  string    `json:"jurisdiction"`
	AuthorityName string    `json:"authority_name"`
	Protocol      string    `json:"protocol"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type TaxFilingSubmission struct {
	SubmissionID   string    `json:"submission_id"`
	InterfaceID    string    `json:"interface_id"`
	TenantID       string    `json:"tenant_id"`
	TaxPeriod      string    `json:"tax_period"`
	FilingType     string    `json:"filing_type"`
	TaxAmount      float64   `json:"tax_amount"`
	Status         string    `json:"status"`
	AckReference   string    `json:"ack_reference,omitempty"`
	SubmittedAt    time.Time `json:"submitted_at"`
}

type CreateInterfaceRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	Jurisdiction  string `json:"jurisdiction"`
	AuthorityName string `json:"authority_name"`
	Protocol      string `json:"protocol"`
}

type SubmitTaxFilingRequest struct {
	InterfaceID string  `json:"interface_id"`
	TaxPeriod   string  `json:"tax_period"`
	FilingType  string  `json:"filing_type"`
	TaxAmount   float64 `json:"tax_amount"`
	Payload     string  `json:"payload"`
}
