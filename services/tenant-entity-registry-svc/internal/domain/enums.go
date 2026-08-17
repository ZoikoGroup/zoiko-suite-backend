// Package domain defines all canonical enums for tenant-entity-registry-svc.
// Field names and enum values are verbatim from docs/architecture/04-data-model.md §05.1.
package domain

// TenantStatus is the operational state of a tenant account.
type TenantStatus string

const (
	TenantStatusActive    TenantStatus = "ACTIVE"
	TenantStatusSuspended TenantStatus = "SUSPENDED"
	TenantStatusArchived  TenantStatus = "ARCHIVED"
)

// TenantLifecycleState is the provisioning and offboarding lifecycle of a tenant.
// Distinct from TenantStatus — a tenant can be ACTIVE in status while still in ONBOARDING lifecycle.
type TenantLifecycleState string

const (
	TenantLifecycleOnboarding  TenantLifecycleState = "ONBOARDING"
	TenantLifecycleActive      TenantLifecycleState = "ACTIVE"
	TenantLifecycleSuspended   TenantLifecycleState = "SUSPENDED"
	TenantLifecycleOffboarding TenantLifecycleState = "OFFBOARDING"
)

// ValidTenantLifecycleTransitions maps valid source states to allowed target states.
// Any transition not in this map is rejected fail-closed.
var ValidTenantLifecycleTransitions = map[TenantLifecycleState][]TenantLifecycleState{
	TenantLifecycleOnboarding:  {TenantLifecycleActive},
	TenantLifecycleActive:      {TenantLifecycleSuspended, TenantLifecycleOffboarding},
	TenantLifecycleSuspended:   {TenantLifecycleActive, TenantLifecycleOffboarding},
	TenantLifecycleOffboarding: {}, // terminal state
}

// EntityType classifies the legal form of a legal entity.
type EntityType string

const (
	EntityTypeSubsidiary  EntityType = "SUBSIDIARY"
	EntityTypeBranch      EntityType = "BRANCH"
	EntityTypeHolding     EntityType = "HOLDING"
	EntityTypeOperational EntityType = "OPERATIONAL"
)

// EntityStatus is the operational state of a legal entity.
// Status transitions only — no hard-delete, no soft-delete (doctrine §2.11).
type EntityStatus string

const (
	EntityStatusActive    EntityStatus = "ACTIVE"
	EntityStatusDormant   EntityStatus = "DORMANT"
	EntityStatusSuspended EntityStatus = "SUSPENDED"
	EntityStatusDissolved EntityStatus = "DISSOLVED"
)

// ValidEntityStatusTransitions maps valid source states to allowed target states.
// Any transition not in this map must be rejected fail-closed.
var ValidEntityStatusTransitions = map[EntityStatus][]EntityStatus{
	EntityStatusActive:    {EntityStatusDormant, EntityStatusSuspended, EntityStatusDissolved},
	EntityStatusDormant:   {EntityStatusActive, EntityStatusDissolved},
	EntityStatusSuspended: {EntityStatusActive, EntityStatusDissolved},
	EntityStatusDissolved: {}, // terminal state
}

// WorkspaceStatus is the operational state of a workspace.
type WorkspaceStatus string

const (
	WorkspaceStatusActive   WorkspaceStatus = "ACTIVE"
	WorkspaceStatusArchived WorkspaceStatus = "ARCHIVED"
)

// BillingClassification is mandatory on every workspace per doc7 §T — it
// determines whether the workspace may ever generate a live Zoiko charge.
// Non-commercial classes (INTERNAL, DEMO, SANDBOX, QA_AUTOMATION,
// PILOT_NON_BILLABLE) must never create live charges regardless of
// entitlement state.
type BillingClassification string

const (
	BillingClassificationCommercialStandalone BillingClassification = "COMMERCIAL_STANDALONE"
	BillingClassificationCommercialZoikoOne   BillingClassification = "COMMERCIAL_ZOIKO_ONE"
	BillingClassificationLegacyMigration      BillingClassification = "LEGACY_MIGRATION"
	BillingClassificationPilotNonBillable     BillingClassification = "PILOT_NON_BILLABLE"
	BillingClassificationInternal             BillingClassification = "INTERNAL"
	BillingClassificationDemo                 BillingClassification = "DEMO"
	BillingClassificationSandbox              BillingClassification = "SANDBOX"
	BillingClassificationQAAutomation         BillingClassification = "QA_AUTOMATION"
)

// ValidBillingClassifications is used to fail closed on an unrecognized
// classification value rather than silently defaulting one in.
var ValidBillingClassifications = map[BillingClassification]bool{
	BillingClassificationCommercialStandalone: true,
	BillingClassificationCommercialZoikoOne:   true,
	BillingClassificationLegacyMigration:      true,
	BillingClassificationPilotNonBillable:     true,
	BillingClassificationInternal:             true,
	BillingClassificationDemo:                 true,
	BillingClassificationSandbox:              true,
	BillingClassificationQAAutomation:         true,
}

// BillingSource records where the billing_classification's commercial
// authority comes from, per doc7 §P2 (e.g. a Zoiko One bundle vs. a direct
// standalone contract). NONE is the default for non-billable classes.
type BillingSource string

const (
	BillingSourceNone           BillingSource = "NONE"
	BillingSourceDirect         BillingSource = "DIRECT"
	BillingSourceZoikoOneBundle BillingSource = "ZOIKO_ONE_BUNDLE"
)

// HierarchyRelationshipType classifies the nature of a parent-child entity relationship.
type HierarchyRelationshipType string

const (
	HierarchyRelationshipOwnership   HierarchyRelationshipType = "OWNERSHIP"
	HierarchyRelationshipReporting   HierarchyRelationshipType = "REPORTING"
	HierarchyRelationshipOperational HierarchyRelationshipType = "OPERATIONAL"
)

// JurisdictionAssignmentType classifies why a jurisdiction applies to an entity.
type JurisdictionAssignmentType string

const (
	JurisdictionAssignmentPrimary  JurisdictionAssignmentType = "PRIMARY"
	JurisdictionAssignmentSecondary JurisdictionAssignmentType = "SECONDARY"
	JurisdictionAssignmentTaxOnly  JurisdictionAssignmentType = "TAX_ONLY"
	JurisdictionAssignmentFilingOnly JurisdictionAssignmentType = "FILING_ONLY"
)

// ResidencyMode controls how strict the data residency enforcement is.
type ResidencyMode string

const (
	ResidencyModeStrictRegion   ResidencyMode = "STRICT_REGION"
	ResidencyModePreferredRegion ResidencyMode = "PREFERRED_REGION"
	ResidencyModeFollowEntity   ResidencyMode = "FOLLOW_ENTITY"
)

// ConflictResolutionMode controls behavior when residency and jurisdiction obligations conflict.
type ConflictResolutionMode string

const (
	ConflictResolutionFailClosed    ConflictResolutionMode = "FAIL_CLOSED"
	ConflictResolutionLogAndProceed ConflictResolutionMode = "LOG_AND_PROCEED"
	ConflictResolutionEscalate      ConflictResolutionMode = "ESCALATE"
)

// TaxIdentityBundleStatus tracks the lifecycle of a TaxIdentityBundle header.
// Per Q3 resolution: this header stores structural metadata only.
// Actual tax identifier values and evidence reside in the Tax Service.
type TaxIdentityBundleStatus string

const (
	TaxIdentityBundlePending    TaxIdentityBundleStatus = "PENDING"
	TaxIdentityBundleActive     TaxIdentityBundleStatus = "ACTIVE"
	TaxIdentityBundleExpired    TaxIdentityBundleStatus = "EXPIRED"
	TaxIdentityBundleSuperseded TaxIdentityBundleStatus = "SUPERSEDED"
)
