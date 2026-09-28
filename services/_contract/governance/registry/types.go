package registry

import (
	"time"

	"zoiko.io/contract/types"
)

// CriticalityTier defines the governance criticality of a data asset (ZS-DATA-GOV-001 §3.1, §4.1).
type CriticalityTier string

const (
	CriticalityTier1Critical   CriticalityTier = "TIER_1_CRITICAL"   // Financial statements, tax, statutory filings, core PII
	CriticalityTier2Operational CriticalityTier = "TIER_2_OPERATIONAL" // Operational workflows, logs, internal analytics
	CriticalityTier3Supporting  CriticalityTier = "TIER_3_SUPPORTING"  // Caches, temporary workspaces, diagnostics
)

// AssetType classifies the physical or logical format of the data asset (§28).
type AssetType string

const (
	AssetTypeTable          AssetType = "TABLE"
	AssetTypeDataset        AssetType = "DATASET"
	AssetTypeEventStream    AssetType = "EVENT_STREAM"
	AssetTypeReport         AssetType = "REPORT"
	AssetTypeDerivedProduct AssetType = "DERIVED_PRODUCT"
)

// AssetStatus represents the lifecycle state of a data asset.
type AssetStatus string

const (
	AssetStatusRegistered AssetStatus = "REGISTERED"
	AssetStatusCertified  AssetStatus = "CERTIFIED"
	AssetStatusDeprecated AssetStatus = "DEPRECATED"
)

// GovernanceScope scopes governance decisions and assignments (§2, GOV-05, DG-009).
type GovernanceScope struct {
	LegalEntityID    *types.UUID `json:"legal_entity_id,omitempty"`
	AccountingBookID *types.UUID `json:"accounting_book_id,omitempty"`
	JurisdictionCode string      `json:"jurisdiction_code,omitempty"`
	BusinessProcess  string      `json:"business_process,omitempty"`
}

// DataDomain models a logical business domain boundary and ownership realm (§28, GOV-01).
type DataDomain struct {
	DomainID    types.UUID `json:"domain_id"`
	TenantID    types.UUID `json:"tenant_id"`
	DomainCode  string     `json:"domain_code"`
	DomainName  string     `json:"domain_name"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status"` // ACTIVE, INACTIVE, DEPRECATED
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// DataAsset represents a governed dataset, entity, record population or derived data product (§28).
type DataAsset struct {
	AssetID              types.UUID      `json:"asset_id"`
	TenantID             types.UUID      `json:"tenant_id"`
	DomainID             types.UUID      `json:"domain_id"`
	AssetCode            string          `json:"asset_code"`
	AssetName            string          `json:"asset_name"`
	AssetType            AssetType       `json:"asset_type"`
	CriticalityTier      CriticalityTier `json:"criticality_tier"`
	IsAuthoritative      bool            `json:"is_authoritative"`
	AuthoritativeService string          `json:"authoritative_service"` // Single service owning authoritative write path (GOV-01, DG-004)
	StorageLocation      string          `json:"storage_location"`
	Scope                GovernanceScope `json:"scope"`
	Status               AssetStatus     `json:"status"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// DataOwnerAssignment models effective-dated accountable business/domain ownership (§28, GOV-01, DG-001, DG-010).
type DataOwnerAssignment struct {
	AssignmentID         types.UUID `json:"assignment_id"`
	TenantID             types.UUID `json:"tenant_id"`
	AssetID              types.UUID `json:"asset_id"`
	OwnerPrincipalID     string     `json:"owner_principal_id"`
	OwnerRole            string     `json:"owner_role"` // e.g. "BUSINESS_OWNER", "DOMAIN_LEAD"
	EffectiveFrom        time.Time  `json:"effective_from"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	ApprovedBy           string     `json:"approved_by"`
	AssignmentEvidenceID types.UUID `json:"assignment_evidence_id"`
	CreatedAt            time.Time  `json:"created_at"`
}

// StewardshipAssignment models effective-dated operational stewardship (§28, GOV-02, DG-002).
type StewardshipAssignment struct {
	AssignmentID       types.UUID `json:"assignment_id"`
	TenantID           types.UUID `json:"tenant_id"`
	AssetID            types.UUID `json:"asset_id"`
	StewardPrincipalID string     `json:"steward_principal_id"`
	OperationalUnit    string     `json:"operational_unit"`
	EffectiveFrom      time.Time  `json:"effective_from"`
	EffectiveTo        *time.Time `json:"effective_to,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

// CustodianAssignment models physical platform/infrastructure custody (§28, GOV-02, DG-003).
// Physical custody does NOT imply business ownership.
type CustodianAssignment struct {
	AssignmentID           types.UUID `json:"assignment_id"`
	TenantID               types.UUID `json:"tenant_id"`
	AssetID                types.UUID `json:"asset_id"`
	CustodianSystemOrTeam  string     `json:"custodian_system_or_team"`
	InfrastructureProvider string     `json:"infrastructure_provider"` // e.g. "AWS_RDS", "KAFKA_CLUSTER", "S3_COLD"
	EffectiveFrom          time.Time  `json:"effective_from"`
	EffectiveTo            *time.Time `json:"effective_to,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
}

// DataContract models canonical schema, interface, and semantic contract (§28, DG-006).
type DataContract struct {
	ContractID        types.UUID `json:"contract_id"`
	TenantID          types.UUID `json:"tenant_id"`
	AssetID           types.UUID `json:"asset_id"`
	Version           string     `json:"version"` // e.g. "v1.0.0"
	SchemaDefinition  string     `json:"schema_definition"`
	CompatibilityMode string     `json:"compatibility_mode"` // "BACKWARD", "FULL", "NONE"
	Status            string     `json:"status"`             // "DRAFT", "ACTIVE", "SUPERSEDED"
	EffectiveFrom     time.Time  `json:"effective_from"`
	EffectiveTo       *time.Time `json:"effective_to,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

// SensitivityLevel specifies classification tiers (DG-007).
type SensitivityLevel string

const (
	SensitivityPublic       SensitivityLevel = "PUBLIC"
	SensitivityInternal     SensitivityLevel = "INTERNAL"
	SensitivityConfidential SensitivityLevel = "CONFIDENTIAL"
	SensitivityRestricted   SensitivityLevel = "RESTRICTED"
)

// DataClassificationBinding models security, privacy, and regulatory classification metadata (§28, DG-007).
type DataClassificationBinding struct {
	BindingID             types.UUID       `json:"binding_id"`
	TenantID              types.UUID       `json:"tenant_id"`
	AssetID               types.UUID       `json:"asset_id"`
	SensitivityLevel      SensitivityLevel `json:"sensitivity_level"`
	ConfidentialityClass  string           `json:"confidentiality_class"` // e.g. "FINANCIAL", "SECRET", "STANDARD"
	PrivacyClass          string           `json:"privacy_class"`         // e.g. "NONE", "PSEUDONYMOUS", "PII", "SPECIAL_CATEGORY"
	RegulatoryRegime      string           `json:"regulatory_regime"`     // e.g. "SOX", "GDPR", "PCI_DSS", "LOCAL_TAX"
	RetentionProfileRef   string           `json:"retention_profile_ref"`
	PurposeRestrictionRef string           `json:"purpose_restriction_ref,omitempty"` // DG-008
	EffectiveFrom         time.Time        `json:"effective_from"`
	EffectiveTo           *time.Time       `json:"effective_to,omitempty"`
	CreatedAt             time.Time        `json:"created_at"`
}
