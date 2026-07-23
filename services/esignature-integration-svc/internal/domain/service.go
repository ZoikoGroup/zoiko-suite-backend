package domain

import (
	"errors"
	"time"
)

var (
	ErrEnvelopeNotFound = errors.New("esignature envelope not found")
)

const (
	ProviderDocuSign  = "DOCUSIGN"
	ProviderAdobeSign = "ADOBE_SIGN"

	EnvelopePending   = "PENDING"
	EnvelopeSent      = "SENT"
	EnvelopeDelivered = "DELIVERED"
	EnvelopeSigned    = "SIGNED"
	EnvelopeVoided    = "VOIDED"
)

type SignatureEnvelope struct {
	EnvelopeID    string    `json:"envelope_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	Provider      string    `json:"provider"`
	DocumentTitle string    `json:"document_title"`
	SignerEmail   string    `json:"signer_email"`
	SignerName    string    `json:"signer_name"`
	Status        string    `json:"status"`
	ExternalRef   string    `json:"external_ref,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type CreateEnvelopeRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	Provider      string `json:"provider"`
	DocumentTitle string `json:"document_title"`
	SignerEmail   string `json:"signer_email"`
	SignerName    string `json:"signer_name"`
}

type UpdateStatusRequest struct {
	Status      string `json:"status"`
	ExternalRef string `json:"external_ref"`
}
