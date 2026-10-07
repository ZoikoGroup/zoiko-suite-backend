package masterdata

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

// PartyKind represents the fundamental category of an actor (Organization or Person).
type PartyKind string

const (
	PartyKindOrganization PartyKind = "ORGANIZATION"
	PartyKindPerson       PartyKind = "PERSON"
)

// MergeStatus indicates whether the party is active or has been merged into a golden record.
type MergeStatus string

const (
	MergeStatusActive MergeStatus = "ACTIVE"
	MergeStatusMerged MergeStatus = "MERGED"
)

// SensitivityClass defines data classification under privacy and compliance policy.
type SensitivityClass string

const (
	SensitivityClassPublic       SensitivityClass = "PUBLIC"
	SensitivityClassInternal     SensitivityClass = "INTERNAL"
	SensitivityClassConfidential SensitivityClass = "CONFIDENTIAL"
	SensitivityClassRestricted   SensitivityClass = "RESTRICTED"
)

// RoleType defines the contextual business relationship of a Party to a Tenant or Legal Entity.
// Per ZS-DATA-001 Section 10 & Anti-Pattern B2, Customers, Suppliers, Employees and Contractors
// are contextual roles, NEVER duplicated identities.
type RoleType string

const (
	RoleTypeCustomer      RoleType = "CUSTOMER"
	RoleTypeSupplier      RoleType = "SUPPLIER"
	RoleTypeEmployee      RoleType = "EMPLOYEE"
	RoleTypeContractor    RoleType = "CONTRACTOR"
	RoleTypeBank          RoleType = "BANK"
	RoleTypeAdviser       RoleType = "ADVISER"
	RoleTypeTaxAuthority  RoleType = "TAX_AUTHORITY"
	RoleTypeRegulator     RoleType = "REGULATOR"
	RoleTypeShareholder   RoleType = "SHAREHOLDER"
)

// RoleStatus represents the operational status of a party role.
type RoleStatus string

const (
	RoleStatusActive    RoleStatus = "ACTIVE"
	RoleStatusSuspended RoleStatus = "SUSPENDED"
	RoleStatusClosed    RoleStatus = "CLOSED"
)

// IdentifierScheme represents standardized external identification systems.
type IdentifierScheme string

const (
	IdentifierSchemeLEI        IdentifierScheme = "LEI"
	IdentifierSchemeCompanyReg IdentifierScheme = "COMPANY_REG"
	IdentifierSchemeVAT        IdentifierScheme = "VAT"
	IdentifierSchemeGSTIN      IdentifierScheme = "GSTIN"
	IdentifierSchemeEIN        IdentifierScheme = "EIN"
	IdentifierSchemePAN        IdentifierScheme = "PAN"
	IdentifierSchemeDUNS       IdentifierScheme = "DUNS"
	IdentifierSchemeNationalID IdentifierScheme = "NATIONAL_ID"
	IdentifierSchemePassport   IdentifierScheme = "PASSPORT"
)

// VerificationStatus tracks the integrity and external validation of an identifier.
type VerificationStatus string

const (
	VerificationStatusUnverified VerificationStatus = "UNVERIFIED"
	VerificationStatusVerified   VerificationStatus = "VERIFIED"
	VerificationStatusExpired    VerificationStatus = "EXPIRED"
	VerificationStatusRejected   VerificationStatus = "REJECTED"
)

// AddressType defines the business use of an address.
type AddressType string

const (
	AddressTypeLegal    AddressType = "LEGAL"
	AddressTypeBilling  AddressType = "BILLING"
	AddressTypeShipping AddressType = "SHIPPING"
	AddressTypePhysical AddressType = "PHYSICAL"
	AddressTypeBranch   AddressType = "BRANCH"
)

// Party represents the stable canonical identity anchor for an organization or person (ORG-PARTY).
type Party struct {
	PartyID                         types.UUID       `json:"party_id"`
	TenantID                        types.UUID       `json:"tenant_id"`
	PartyKind                       PartyKind        `json:"party_kind"`
	DisplayName                     string           `json:"display_name"`
	LegalName                       string           `json:"legal_name,omitempty"`
	CountryOfRegistrationOrResidence string          `json:"country_of_registration_or_residence,omitempty"` // ISO 3166-1 alpha-2
	MergeStatus                     MergeStatus      `json:"merge_status"`
	MergedIntoPartyID               *types.UUID      `json:"merged_into_party_id,omitempty"`
	SensitivityClass                SensitivityClass `json:"sensitivity_class"`
	CreatedAt                       time.Time        `json:"created_at"`
	CreatedBy                       string           `json:"created_by"`
	UpdatedAt                       time.Time        `json:"updated_at"`
}

// Validate ensures all mandatory fields meet canonical requirements.
func (p Party) Validate() error {
	if p.PartyID.IsNil() {
		return errors.New("party requires non-nil party_id")
	}
	if p.TenantID.IsNil() {
		return errors.New("party requires non-nil tenant_id")
	}
	if p.DisplayName == "" {
		return errors.New("party requires display_name")
	}
	if p.PartyKind != PartyKindOrganization && p.PartyKind != PartyKindPerson {
		return fmt.Errorf("invalid party_kind: %q", p.PartyKind)
	}
	if p.MergeStatus == "" {
		p.MergeStatus = MergeStatusActive
	}
	if p.MergeStatus == MergeStatusMerged && (p.MergedIntoPartyID == nil || p.MergedIntoPartyID.IsNil()) {
		return errors.New("merged party requires non-nil merged_into_party_id")
	}
	return nil
}

// PartyRole represents the contextual relationship of a Party to a Tenant or Legal Entity (ORG-ROLE).
type PartyRole struct {
	PartyRoleID   types.UUID             `json:"party_role_id"`
	TenantID      types.UUID             `json:"tenant_id"`
	PartyID       types.UUID             `json:"party_id"`
	RoleType      RoleType               `json:"role_type"`
	LegalEntityID *types.UUID            `json:"legal_entity_id,omitempty"` // Optional scope when role is entity-specific
	Status        RoleStatus             `json:"status"`
	Temporal      types.BitemporalRecord `json:"temporal"`
}

// Validate checks role invariants.
func (r PartyRole) Validate() error {
	if r.PartyRoleID.IsNil() {
		return errors.New("party_role requires party_role_id")
	}
	if r.TenantID.IsNil() || r.PartyID.IsNil() {
		return errors.New("party_role requires tenant_id and party_id")
	}
	if r.RoleType == "" {
		return errors.New("party_role requires role_type")
	}
	if err := r.Temporal.Validate(); err != nil {
		return fmt.Errorf("party_role temporal invalid: %w", err)
	}
	return nil
}

// PartyIdentifier represents a typed external or regulatory identifier for a party (ORG-ID).
type PartyIdentifier struct {
	PartyIdentifierID      types.UUID         `json:"party_identifier_id"`
	TenantID               types.UUID         `json:"tenant_id"`
	PartyID                types.UUID         `json:"party_id"`
	SchemeCode             IdentifierScheme   `json:"scheme_code"`
	IssuerOrJurisdictionID string             `json:"issuer_or_jurisdiction_id,omitempty"`
	IdentifierValue        string             `json:"identifier_value"` // Raw/vault reference
	MaskedValue            string             `json:"masked_value,omitempty"` // Presentation safe
	ValidFrom              *time.Time         `json:"valid_from,omitempty"`
	ValidTo                *time.Time         `json:"valid_to,omitempty"`
	VerificationStatus     VerificationStatus `json:"verification_status"`
	CreatedAt              time.Time          `json:"created_at"`
}

// Address represents a physical, registered, or billing address (ORG-ADDR).
type Address struct {
	AddressID       types.UUID  `json:"address_id"`
	TenantID        types.UUID  `json:"tenant_id"`
	AddressType     AddressType `json:"address_type"`
	Line1           string      `json:"line1"`
	Line2           string      `json:"line2,omitempty"`
	City            string      `json:"city"`
	SubdivisionCode string      `json:"subdivision_code,omitempty"` // ISO 3166-2 (State/Province)
	PostalCode      string      `json:"postal_code"`
	CountryCode     string      `json:"country_code"` // ISO 3166-1 alpha-2
	CreatedAt       time.Time   `json:"created_at"`
}

// AddressAssignment links an Address to a Party or Legal Entity with effective dates.
type AddressAssignment struct {
	AssignmentID types.UUID             `json:"assignment_id"`
	TenantID     types.UUID             `json:"tenant_id"`
	AddressID    types.UUID             `json:"address_id"`
	SubjectID    types.UUID             `json:"subject_id"` // PartyID or LegalEntityID
	SubjectType  string                 `json:"subject_type"` // "PARTY" or "LEGAL_ENTITY"
	AddressType  AddressType            `json:"address_type"`
	IsPrimary    bool                   `json:"is_primary"`
	Temporal     types.BitemporalRecord `json:"temporal"`
}
