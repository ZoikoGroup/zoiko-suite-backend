package certification

import (
	"time"

	"zoiko.io/contract/types"
)

// CertificationClass represents the 5 canonical data certification tiers (ZS-DATA-GOV-001 §23.1).
type CertificationClass string

const (
	ClassC0Uncertified         CertificationClass = "C0_UNCERTIFIED"          // Development/diagnostic only
	ClassC1Operational         CertificationClass = "C1_OPERATIONAL"          // Normal operational consumption
	ClassC2Controlled          CertificationClass = "C2_CONTROLLED"           // Material business decisions
	ClassC3FinancialRegulatory CertificationClass = "C3_FINANCIAL_REGULATORY" // Financial statements & statutory outputs
	ClassC4LegalEvidentiary    CertificationClass = "C4_LEGAL_EVIDENTIARY"    // Litigation / custody assurance
)

// CertificationStatus tracks the lifecycle state of a data certification (§23, DG-057, DG-058).
type CertificationStatus string

const (
	CertStatusEffective   CertificationStatus = "EFFECTIVE"
	CertStatusExpired     CertificationStatus = "EXPIRED"
	CertStatusInvalidated CertificationStatus = "INVALIDATED" // Material owner or code change (NP-30)
)

// DataCertification models a formal attestation of data quality, lineage, and compliance fitness (§23, §28).
type DataCertification struct {
	CertificationID    types.UUID          `json:"certification_id"`
	TenantID           types.UUID          `json:"tenant_id"`
	AssetID            types.UUID          `json:"asset_id"`
	CertClass          CertificationClass  `json:"cert_class"`
	EvidenceManifestID *types.UUID         `json:"evidence_manifest_id,omitempty"`
	DQRunID            *types.UUID         `json:"dq_run_id,omitempty"`
	LineageHash        string              `json:"lineage_hash,omitempty"`
	Status             CertificationStatus `json:"status"`
	CertifiedAt        time.Time           `json:"certified_at"`
	CertifiedBy        string              `json:"certified_by"`
	ExpiresAt          time.Time           `json:"expires_at"`
}

// DataUseAuthorization records permitted business and secondary purposes (§24, §28, DG-008, DG-055).
type DataUseAuthorization struct {
	AuthorizationID types.UUID `json:"authorization_id"`
	TenantID        types.UUID `json:"tenant_id"`
	AssetID         types.UUID `json:"asset_id"`
	ConsumerID      string     `json:"consumer_id"` // Principal, service, or model workload
	ApprovedPurpose string     `json:"approved_purpose"`
	Status          string     `json:"status"` // "ACTIVE", "REVOKED"
	ValidFrom       time.Time  `json:"valid_from"`
	ValidTo         *time.Time `json:"valid_to,omitempty"`
}

// DataTransferRecord provides evidence of cross-region, cross-jurisdiction, or external transfers (§28, NP-29).
type DataTransferRecord struct {
	TransferID    types.UUID `json:"transfer_id"`
	TenantID      types.UUID `json:"tenant_id"`
	AssetID       types.UUID `json:"asset_id"`
	SourceRegion  string     `json:"source_region"`
	TargetRegion  string     `json:"target_region"`
	Recipient     string     `json:"recipient"`
	LawfulBasis   string     `json:"lawful_basis"`
	IsRegistered  bool       `json:"is_registered"`
	TransferredAt time.Time  `json:"transferred_at"`
}
