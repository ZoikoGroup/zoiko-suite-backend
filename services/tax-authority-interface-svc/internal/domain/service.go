package domain

import (
	"encoding/json"
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

	// Authority binding (INT-08). Empty ProviderKey keeps the legacy "record the intent" behaviour.
	ProviderKey         string     `json:"provider_key,omitempty"`
	AdapterKind         string     `json:"adapter_kind,omitempty"` // TAX (default) or EINVOICE
	ConnectorID         string     `json:"connector_id,omitempty"`
	CertificateRef      string     `json:"certificate_ref,omitempty"`
	CertificateNotAfter *time.Time `json:"certificate_not_after,omitempty"`
	EscalateBeforeHours int        `json:"escalate_before_hours,omitempty"`
}

type TaxFilingSubmission struct {
	SubmissionID string    `json:"submission_id"`
	InterfaceID  string    `json:"interface_id"`
	TenantID     string    `json:"tenant_id"`
	TaxPeriod    string    `json:"tax_period"`
	FilingType   string    `json:"filing_type"`
	TaxAmount    float64   `json:"tax_amount"`
	Status       string    `json:"status"`
	AckReference string    `json:"ack_reference,omitempty"`
	SubmittedAt  time.Time `json:"submitted_at"`
}

type CreateInterfaceRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	Jurisdiction  string `json:"jurisdiction"`
	AuthorityName string `json:"authority_name"`
	Protocol      string `json:"protocol"`

	ProviderKey         string     `json:"provider_key,omitempty"`
	AdapterKind         string     `json:"adapter_kind,omitempty"`
	ConnectorID         string     `json:"connector_id,omitempty"`
	CertificateRef      string     `json:"certificate_ref,omitempty"`
	CertificateNotAfter *time.Time `json:"certificate_not_after,omitempty"`
	EscalateBeforeHours int        `json:"escalate_before_hours,omitempty"`
}

type SubmitTaxFilingRequest struct {
	InterfaceID string  `json:"interface_id"`
	TaxPeriod   string  `json:"tax_period"`
	FilingType  string  `json:"filing_type"`
	TaxAmount   float64 `json:"tax_amount"`
	Payload     string  `json:"payload"`

	// Authority submission (INT-08). Document is the approved canonical filing; SubjectFingerprint is its canonical
	// hash (adapter.CanonicalHash) as approved by the TAX domain; SubjectRef is the TAX submission subject.
	SubjectRef         string          `json:"subject_ref,omitempty"`
	SubjectFingerprint string          `json:"subject_fingerprint,omitempty"`
	Document           json.RawMessage `json:"document,omitempty"`
	Rail               string          `json:"rail,omitempty"`
	DeadlineAt         *time.Time      `json:"deadline_at,omitempty"`
}
