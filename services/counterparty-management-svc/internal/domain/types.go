package domain

import (
	"errors"
	"time"
)

var (
	ErrCounterpartyNotFound      = errors.New("counterparty not found")
	ErrInvalidCounterpartyStatus = errors.New("invalid counterparty status transition")
)

type CounterpartyType string

const (
	TypeVendor               CounterpartyType = "VENDOR"
	TypeCustomer             CounterpartyType = "CUSTOMER"
	TypePartner              CounterpartyType = "PARTNER"
	TypeRegulator            CounterpartyType = "REGULATOR"
	TypeFinancialInstitution CounterpartyType = "FINANCIAL_INSTITUTION"
	TypeOther                CounterpartyType = "OTHER"
)

type CounterpartyStatus string

const (
	StatusOnboarding CounterpartyStatus = "ONBOARDING"
	StatusActive     CounterpartyStatus = "ACTIVE"
	StatusSuspended  CounterpartyStatus = "SUSPENDED"
	StatusTerminated CounterpartyStatus = "TERMINATED"
)

type RiskCategory string

const (
	RiskLow        RiskCategory = "LOW"
	RiskMedium     RiskCategory = "MEDIUM"
	RiskHigh       RiskCategory = "HIGH"
	RiskRestricted RiskCategory = "RESTRICTED"
)

type Counterparty struct {
	CounterpartyID     string             `json:"counterparty_id"`
	TenantID           string             `json:"tenant_id"`
	LegalEntityID      string             `json:"legal_entity_id"`
	Name               string             `json:"name"`
	CounterpartyType   CounterpartyType   `json:"counterparty_type"`
	RegistrationNumber string             `json:"registration_number,omitempty"`
	TaxID              string             `json:"tax_id,omitempty"`
	JurisdictionID     string             `json:"jurisdiction_id"`
	RiskCategory       RiskCategory       `json:"risk_category"`
	Status             CounterpartyStatus `json:"status"`
	ContactEmail       string             `json:"contact_email,omitempty"`
	Phone              string             `json:"phone,omitempty"`
	Address            string             `json:"address,omitempty"`
	ComplianceStatus   string             `json:"compliance_status"` // VERIFIED, PENDING, REJECTED
	EffectiveFrom      string             `json:"effective_from"`
	EffectiveTo        *string            `json:"effective_to,omitempty"`
	CreatedBy          string             `json:"created_by"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

type CreateCounterpartyRequest struct {
	LegalEntityID      string           `json:"legal_entity_id"`
	Name               string           `json:"name"`
	CounterpartyType   CounterpartyType `json:"counterparty_type"`
	RegistrationNumber string           `json:"registration_number,omitempty"`
	TaxID              string           `json:"tax_id,omitempty"`
	JurisdictionID     string           `json:"jurisdiction_id"`
	RiskCategory       RiskCategory     `json:"risk_category,omitempty"`
	ContactEmail       string           `json:"contact_email,omitempty"`
	Phone              string           `json:"phone,omitempty"`
	Address            string           `json:"address,omitempty"`
	EffectiveFrom      string           `json:"effective_from"`
	CreatedBy          string           `json:"created_by"`
}

type UpdateCounterpartyRequest struct {
	Name               string             `json:"name,omitempty"`
	CounterpartyType   CounterpartyType   `json:"counterparty_type,omitempty"`
	RegistrationNumber string             `json:"registration_number,omitempty"`
	TaxID              string             `json:"tax_id,omitempty"`
	JurisdictionID     string             `json:"jurisdiction_id,omitempty"`
	RiskCategory       RiskCategory       `json:"risk_category,omitempty"`
	Status             CounterpartyStatus `json:"status,omitempty"`
	ContactEmail       string             `json:"contact_email,omitempty"`
	Phone              string             `json:"phone,omitempty"`
	Address            string             `json:"address,omitempty"`
	EffectiveTo        *string            `json:"effective_to,omitempty"`
	UpdatedBy          string             `json:"updated_by"`
}

type UpdateComplianceStatusRequest struct {
	ComplianceStatus string `json:"compliance_status"` // VERIFIED, PENDING, REJECTED
	UpdatedBy        string `json:"updated_by"`
}
