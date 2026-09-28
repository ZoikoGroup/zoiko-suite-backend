package masterdata

import (
	"errors"
	"fmt"

	"zoiko.io/contract/types"
)

// EntityStatus represents the legal lifecycle state of a legal entity.
type EntityStatus string

const (
	EntityStatusDraft     EntityStatus = "DRAFT"
	EntityStatusActive    EntityStatus = "ACTIVE"
	EntityStatusInactive  EntityStatus = "INACTIVE"
	EntityStatusDissolved EntityStatus = "DISSOLVED"
)

// EstablishmentType defines the physical or tax-relevant nature of an establishment.
type EstablishmentType string

const (
	EstablishmentTypeRegisteredOffice      EstablishmentType = "REGISTERED_OFFICE"
	EstablishmentTypeBranch                EstablishmentType = "BRANCH"
	EstablishmentTypeWarehouse             EstablishmentType = "WAREHOUSE"
	EstablishmentTypePermanentEstablishment EstablishmentType = "PERMANENT_ESTABLISHMENT"
	EstablishmentTypeSite                  EstablishmentType = "SITE"
)

// TaxRelevance represents the tax categorization of an establishment.
type TaxRelevance string

const (
	TaxRelevanceNone       TaxRelevance = "NONE"
	TaxRelevancePotential  TaxRelevance = "POTENTIAL"
	TaxRelevanceRegistered TaxRelevance = "REGISTERED"
	TaxRelevancePE         TaxRelevance = "PE"
)

// CorporateRelationshipType defines the legal relationship between entities.
type CorporateRelationshipType string

const (
	CorporateRelationshipTypeParentSubsidiary   CorporateRelationshipType = "PARENT_SUBSIDIARY"
	CorporateRelationshipTypeBranch             CorporateRelationshipType = "BRANCH"
	CorporateRelationshipTypeJointVenture       CorporateRelationshipType = "JOINT_VENTURE"
	CorporateRelationshipTypeAssociate          CorporateRelationshipType = "ASSOCIATE"
	CorporateRelationshipTypeBeneficialOwnership CorporateRelationshipType = "BENEFICIAL_OWNERSHIP"
)

// ConsolidationMethod defines accounting consolidation treatment under IFRS/GAAP.
type ConsolidationMethod string

const (
	ConsolidationMethodFull          ConsolidationMethod = "FULL"
	ConsolidationMethodEquity        ConsolidationMethod = "EQUITY"
	ConsolidationMethodProportionate ConsolidationMethod = "PROPORTIONATE"
	ConsolidationMethodNone          ConsolidationMethod = "NONE"
)

// LegalEntity represents a canonical incorporated organization whose books, tax, contracts and filings ZoikoSuite manages (ORG-ENTITY).
type LegalEntity struct {
	LegalEntityID               types.UUID             `json:"legal_entity_id"`
	TenantID                    types.UUID             `json:"tenant_id"`
	LegalName                   string                 `json:"legal_name"`
	EntityTypeCode              string                 `json:"entity_type_code"` // Corporation, LLC, Partnership, Branch
	IncorporationJurisdictionID types.UUID             `json:"incorporation_jurisdiction_id"`
	RegistrationNumber          string                 `json:"registration_number,omitempty"`
	FunctionalCurrencyCode      string                 `json:"functional_currency_code"` // ISO 4217 (e.g. USD, EUR, GBP)
	FiscalCalendarID            types.UUID             `json:"fiscal_calendar_id"`
	Status                      EntityStatus           `json:"status"`
	Temporal                    types.BitemporalRecord `json:"temporal"`
}

// Validate checks legal entity requirements.
func (le LegalEntity) Validate() error {
	if le.LegalEntityID.IsNil() {
		return errors.New("legal_entity requires legal_entity_id")
	}
	if le.TenantID.IsNil() {
		return errors.New("legal_entity requires tenant_id")
	}
	if le.LegalName == "" {
		return errors.New("legal_entity requires legal_name")
	}
	if le.EntityTypeCode == "" {
		return errors.New("legal_entity requires entity_type_code")
	}
	if le.FunctionalCurrencyCode == "" || len(le.FunctionalCurrencyCode) != 3 {
		return fmt.Errorf("invalid functional_currency_code: %q", le.FunctionalCurrencyCode)
	}
	if err := le.Temporal.Validate(); err != nil {
		return fmt.Errorf("legal_entity temporal invalid: %w", err)
	}
	return nil
}

// Establishment represents a physical or tax-relevant establishment/branch/place of business (ORG-EST).
type Establishment struct {
	EstablishmentID   types.UUID             `json:"establishment_id"`
	TenantID          types.UUID             `json:"tenant_id"`
	LegalEntityID     types.UUID             `json:"legal_entity_id"`
	EstablishmentType EstablishmentType      `json:"establishment_type"`
	AddressID         types.UUID             `json:"address_id"`
	JurisdictionID    types.UUID             `json:"jurisdiction_id"`
	TaxRelevance      TaxRelevance           `json:"tax_relevance"`
	Temporal          types.BitemporalRecord `json:"temporal"`
}

// CorporateRelationship represents effective-dated ownership or control between legal entities (ORG-REL).
type CorporateRelationship struct {
	RelationshipID         types.UUID                `json:"relationship_id"`
	TenantID               types.UUID                `json:"tenant_id"`
	ParentEntityOrPartyID  types.UUID                `json:"parent_entity_or_party_id"`
	ChildLegalEntityID     types.UUID                `json:"child_legal_entity_id"`
	RelationshipType       CorporateRelationshipType `json:"relationship_type"`
	OwnershipPercentage    types.RateDecimal         `json:"ownership_percentage"` // 0.0 to 100.0 exact decimal
	VotingPercentage       types.RateDecimal         `json:"voting_percentage"`
	ConsolidationMethod    ConsolidationMethod       `json:"consolidation_method"`
	EvidenceRef            types.UUID                `json:"evidence_ref,omitempty"`
	Temporal               types.BitemporalRecord    `json:"temporal"`
}

// Validate checks corporate relationship constraints.
func (cr CorporateRelationship) Validate() error {
	if cr.RelationshipID.IsNil() || cr.TenantID.IsNil() {
		return errors.New("corporate_relationship requires relationship_id and tenant_id")
	}
	if cr.ParentEntityOrPartyID.IsNil() || cr.ChildLegalEntityID.IsNil() {
		return errors.New("corporate_relationship requires parent and child IDs")
	}
	if cr.ParentEntityOrPartyID == cr.ChildLegalEntityID {
		return errors.New("self-referential corporate relationship is illegal")
	}
	if err := cr.Temporal.Validate(); err != nil {
		return fmt.Errorf("corporate_relationship temporal invalid: %w", err)
	}
	return nil
}
