// Package domain defines all canonical types for tenant-entity-registry-svc.
// Field names are verbatim from docs/architecture/04-data-model.md §05.1.
package domain

import "time"

// ---------------------------------------------------------------------------
// Tenant  (data-model §05.1)
// ---------------------------------------------------------------------------

type Tenant struct {
	TenantID                  string               `json:"tenant_id"`
	TenantCode                string               `json:"tenant_code"`
	LegalName                 string               `json:"legal_name"`
	TradingName               *string              `json:"trading_name"`
	Status                    TenantStatus         `json:"status"`
	DefaultCurrencyCode       string               `json:"default_currency_code"`
	PrimaryTimezone           string               `json:"primary_timezone"`
	PrimaryLocale             string               `json:"primary_locale"`
	DefaultDataResidencyPolicyID string            `json:"default_data_residency_policy_id"`
	LifecycleState            TenantLifecycleState `json:"lifecycle_state"`
	CreatedAt                 time.Time            `json:"created_at"`
	UpdatedAt                 time.Time            `json:"updated_at"`
	CreatedByPrincipalID      string               `json:"created_by_principal_id"`
	UpdatedByPrincipalID      string               `json:"updated_by_principal_id"`
}

// ---------------------------------------------------------------------------
// LegalEntity  (data-model §05.1)
// ---------------------------------------------------------------------------

type LegalEntity struct {
	LegalEntityID          string       `json:"legal_entity_id"`
	TenantID               string       `json:"tenant_id"`
	EntityCode             string       `json:"entity_code"`
	LegalName              string       `json:"legal_name"`
	TradingName            *string      `json:"trading_name"`
	RegistrationNumber     *string      `json:"registration_number"`
	// tax_registration_number INTENTIONALLY ABSENT — per Q3 resolution.
	// Actual tax identifier values reside in Tax Service to keep regulated PII in one place.
	TaxIdentityBundleID    *string      `json:"tax_identity_bundle_id"`
	EntityType             EntityType   `json:"entity_type"`
	IncorporationDate      *time.Time   `json:"incorporation_date"`
	DefaultCurrencyCode    string       `json:"default_currency_code"`
	FiscalCalendarID       string       `json:"fiscal_calendar_id"`
	ParentLegalEntityID    *string      `json:"parent_legal_entity_id"`
	EntityStatus           EntityStatus `json:"entity_status"`
	PrimaryJurisdictionID  string       `json:"primary_jurisdiction_id"`
	// DataResidencyPolicyID is MANDATORY per data-model §05.3 modeling rule 2.
	// No LegalEntity may be created without a valid residency policy.
	DataResidencyPolicyID  string       `json:"data_residency_policy_id"`
	CreatedAt              time.Time    `json:"created_at"`
	UpdatedAt              time.Time    `json:"updated_at"`
	CreatedByPrincipalID   string       `json:"created_by_principal_id"`
	UpdatedByPrincipalID   string       `json:"updated_by_principal_id"`
}

// ---------------------------------------------------------------------------
// EntityHierarchy  (data-model §05.1)
// Effective-dated — end-date to close, never hard-delete.
// ---------------------------------------------------------------------------

type EntityHierarchy struct {
	HierarchyID          string                    `json:"hierarchy_id"`
	TenantID             string                    `json:"tenant_id"`
	ParentLegalEntityID  string                    `json:"parent_legal_entity_id"`
	ChildLegalEntityID   string                    `json:"child_legal_entity_id"`
	RelationshipType     HierarchyRelationshipType `json:"relationship_type"`
	EffectiveFrom        time.Time                 `json:"effective_from"`
	EffectiveTo          *time.Time                `json:"effective_to"` // nil = open-ended
	CreatedAt            time.Time                 `json:"created_at"`
	UpdatedAt            time.Time                 `json:"updated_at"`
	CreatedByPrincipalID string                    `json:"created_by_principal_id"`
	UpdatedByPrincipalID string                    `json:"updated_by_principal_id"`
}

// ---------------------------------------------------------------------------
// EntityJurisdictionAssignment  (data-model §05.1)
// Effective-dated — end-date to close, never hard-delete.
// ---------------------------------------------------------------------------

type EntityJurisdictionAssignment struct {
	AssignmentID    string                     `json:"assignment_id"`
	TenantID        string                     `json:"tenant_id"`
	LegalEntityID   string                     `json:"legal_entity_id"`
	JurisdictionID  string                     `json:"jurisdiction_id"`
	AssignmentType  JurisdictionAssignmentType `json:"assignment_type"`
	EffectiveFrom   time.Time                  `json:"effective_from"`
	EffectiveTo     *time.Time                 `json:"effective_to"` // nil = open-ended
	SourceBasis     string                     `json:"source_basis"`
	CreatedAt       time.Time                  `json:"created_at"`
	UpdatedAt       time.Time                  `json:"updated_at"`
	CreatedByPrincipalID string                `json:"created_by_principal_id"`
	UpdatedByPrincipalID string                `json:"updated_by_principal_id"`
}

// ---------------------------------------------------------------------------
// DataResidencyPolicy  (data-model §05.1)
// ---------------------------------------------------------------------------

type DataResidencyPolicy struct {
	DataResidencyPolicyID  string                 `json:"data_residency_policy_id"`
	TenantID               string                 `json:"tenant_id"`
	PolicyName             string                 `json:"policy_name"`
	PolicyCode             string                 `json:"policy_code"`
	ResidencyMode          ResidencyMode          `json:"residency_mode"`
	ConflictResolutionMode ConflictResolutionMode `json:"conflict_resolution_mode"`
	// ResidencyRegionID is nil for policies created before this field
	// existed, or for any policy an operator hasn't assigned a concrete
	// region to yet. Added to close a real gap found while implementing
	// the Global Traffic & Residency Manager: ResidencyMode says HOW
	// STRICTLY to enforce residency, this says WHICH region.
	ResidencyRegionID      *string                `json:"residency_region_id"`
	ActiveFlag             bool                   `json:"active_flag"`
	CreatedAt              time.Time              `json:"created_at"`
	UpdatedAt              time.Time              `json:"updated_at"`
	CreatedByPrincipalID   string                 `json:"created_by_principal_id"`
	UpdatedByPrincipalID   string                 `json:"updated_by_principal_id"`
}

// ---------------------------------------------------------------------------
// ResidencyRegion  (data-model §05.1)
// Platform-managed, IaC-provisioned. No write API per Q1 resolution.
// ---------------------------------------------------------------------------

type ResidencyRegion struct {
	ResidencyRegionID string `json:"residency_region_id"`
	RegionCode        string `json:"region_code"`
	RegionName        string `json:"region_name"`
	CloudProvider     string `json:"cloud_provider"`
	CountryCode       string `json:"country_code"`
	SovereignFlag     bool   `json:"sovereign_flag"`
	ActiveFlag        bool   `json:"active_flag"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	CreatedByPrincipalID string `json:"created_by_principal_id"`
	UpdatedByPrincipalID string `json:"updated_by_principal_id"`
}

// ---------------------------------------------------------------------------
// TaxIdentityBundle  (data-model §05.1 — header only, per Q3 resolution)
//
// This record stores ONLY the structural metadata linking an entity to its
// jurisdiction and validity window. The actual tax_registration_number and
// all evidence artifacts are owned by the Tax Service.
// ---------------------------------------------------------------------------

type TaxIdentityBundle struct {
	TaxIdentityBundleID string                  `json:"tax_identity_bundle_id"`
	TenantID            string                  `json:"tenant_id"`
	LegalEntityID       string                  `json:"legal_entity_id"`
	// JurisdictionID validated against Jurisdiction Rules Service at creation time.
	JurisdictionID      string                  `json:"jurisdiction_id"`
	Status              TaxIdentityBundleStatus `json:"status"`
	EffectiveFrom       time.Time               `json:"effective_from"`
	EffectiveTo         *time.Time              `json:"effective_to"`
	CreatedAt           time.Time               `json:"created_at"`
	UpdatedAt           time.Time               `json:"updated_at"`
	CreatedByPrincipalID string                 `json:"created_by_principal_id"`
	UpdatedByPrincipalID string                 `json:"updated_by_principal_id"`
	DataClassification   string                  `json:"data_classification"`
}

// ---------------------------------------------------------------------------
// Wire types (request / response)
// ---------------------------------------------------------------------------

type ProvisionTenantRequest struct {
	TenantCode                   string `json:"tenant_code"`
	LegalName                    string `json:"legal_name"`
	TradingName                  string `json:"trading_name,omitempty"`
	DefaultCurrencyCode          string `json:"default_currency_code"`
	PrimaryTimezone              string `json:"primary_timezone"`
	PrimaryLocale                string `json:"primary_locale"`
	DefaultDataResidencyPolicyID string `json:"default_data_residency_policy_id"`
}

type TransitionTenantLifecycleRequest struct {
	TargetState   TenantLifecycleState `json:"target_state"`
	CorrelationID string               `json:"correlation_id"`
}

type CreateEntityRequest struct {
	TenantID              string     `json:"tenant_id"`
	EntityCode            string     `json:"entity_code"`
	LegalName             string     `json:"legal_name"`
	TradingName           string     `json:"trading_name,omitempty"`
	EntityType            EntityType `json:"entity_type"`
	DefaultCurrencyCode   string     `json:"default_currency_code"`
	FiscalCalendarID      string     `json:"fiscal_calendar_id"`
	PrimaryJurisdictionID string     `json:"primary_jurisdiction_id"`
	DataResidencyPolicyID string     `json:"data_residency_policy_id"`
	CorrelationID         string     `json:"correlation_id"`
}

type UpdateEntityRequest struct {
	LegalName           *string `json:"legal_name,omitempty"`
	TradingName         *string `json:"trading_name,omitempty"`
	DefaultCurrencyCode *string `json:"default_currency_code,omitempty"`
	CorrelationID       string  `json:"correlation_id"`
	// ActorPrincipalID is populated server-side from the envelope JWT by the
	// service layer before passing to the store. It is never accepted from the
	// HTTP request body (json:"-") to prevent client-side privilege injection.
	ActorPrincipalID    string  `json:"-"`
}

type TransitionEntityStatusRequest struct {
	NewStatus     EntityStatus `json:"new_status"`
	CorrelationID string       `json:"correlation_id"`
}

type EntityStatusResponse struct {
	EntityID      string       `json:"entity_id"`
	TenantID      string       `json:"tenant_id"`
	EntityStatus  EntityStatus `json:"entity_status"`
}

type AssignJurisdictionRequest struct {
	JurisdictionID string                     `json:"jurisdiction_id"`
	AssignmentType JurisdictionAssignmentType `json:"assignment_type"`
	EffectiveFrom  time.Time                  `json:"effective_from"`
	SourceBasis    string                     `json:"source_basis"`
	CorrelationID  string                     `json:"correlation_id"`
}

type CreateHierarchyRequest struct {
	TenantID            string                    `json:"tenant_id"`
	ParentLegalEntityID string                    `json:"parent_legal_entity_id"`
	ChildLegalEntityID  string                    `json:"child_legal_entity_id"`
	RelationshipType    HierarchyRelationshipType `json:"relationship_type"`
	EffectiveFrom       time.Time                 `json:"effective_from"`
	CorrelationID       string                    `json:"correlation_id"`
}

type CreateResidencyPolicyRequest struct {
	TenantID               string                 `json:"tenant_id"`
	PolicyName             string                 `json:"policy_name"`
	PolicyCode             string                 `json:"policy_code"`
	ResidencyMode          ResidencyMode          `json:"residency_mode"`
	ConflictResolutionMode ConflictResolutionMode `json:"conflict_resolution_mode"`
	// ResidencyRegionID is optional — omit it for a policy that doesn't
	// yet have a concrete region assigned (see DataResidencyPolicy's
	// field comment).
	ResidencyRegionID      *string                `json:"residency_region_id,omitempty"`
	CorrelationID          string                 `json:"correlation_id"`
}

// ResolvedTenantRegion is the response shape for GET
// /v1/tenants/{tenantID}/residency-region — the real lookup GTRM's
// ingress layer uses to resolve which region a tenant's traffic belongs
// in, replacing the header-stand-in used in the Phase 1 routing demo.
type ResolvedTenantRegion struct {
	TenantID   string `json:"tenant_id"`
	RegionCode string `json:"region_code"`
	RegionName string `json:"region_name"`
}

// CreateTaxIdentityBundleRequest creates a TaxIdentityBundle header.
//
// Per Q3 resolution: this service stores ONLY the structural header
// (legal_entity_id, jurisdiction_id, effective dates, status).
// The actual tax registration number and evidence artifacts are owned
// by the Tax Service — do NOT add tax identifier fields here.
type CreateTaxIdentityBundleRequest struct {
	// JurisdictionID is validated synchronously against the Jurisdiction Rules
	// Service before persistence (fail-closed per Q2 resolution).
	JurisdictionID string     `json:"jurisdiction_id"`
	EffectiveFrom  time.Time  `json:"effective_from"`
	EffectiveTo    *time.Time `json:"effective_to,omitempty"`
	CorrelationID  string     `json:"correlation_id"`
}

// TransitionTaxIdentityBundleStatusRequest applies a status transition on a bundle header.
type TransitionTaxIdentityBundleStatusRequest struct {
	NewStatus     TaxIdentityBundleStatus `json:"new_status"`
	CorrelationID string                  `json:"correlation_id"`
}
