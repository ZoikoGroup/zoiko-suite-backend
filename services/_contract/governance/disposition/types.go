package disposition

import (
	"time"

	"zoiko.io/contract/types"
)

// BatchStatus tracks the execution state of a disposition batch (§19, DG-041).
type BatchStatus string

const (
	BatchStatusPendingApproval BatchStatus = "PENDING_APPROVAL"
	BatchStatusApproved        BatchStatus = "APPROVED"
	BatchStatusExecuted        BatchStatus = "EXECUTED"
	BatchStatusPartiallyFailed BatchStatus = "PARTIALLY_FAILED" // NP-33: Handled safely
	BatchStatusRejected        BatchStatus = "REJECTED"
)

// ExecutionMethod indicates how the data is destroyed or sanitized (§19.1).
type ExecutionMethod string

const (
	MethodSecurePurge   ExecutionMethod = "SECURE_PURGE"
	MethodCryptoShred   ExecutionMethod = "CRYPTO_SHRED"
	MethodAnonymization ExecutionMethod = "ANONYMIZE"
	MethodArchiveMove   ExecutionMethod = "ARCHIVE_MOVE"
)

// DispositionBatch models a frozen population of eligible objects undergoing governed destruction (§19.1, §28, DG-041).
type DispositionBatch struct {
	BatchID                types.UUID      `json:"batch_id"`
	TenantID               types.UUID      `json:"tenant_id"`
	BatchCode              string          `json:"batch_code"`
	PopulationSnapshotHash string          `json:"population_snapshot_hash"` // SHA-256 of frozen record IDs (NP-20)
	RecordIDs              []types.UUID    `json:"record_ids"`               // Bounded, frozen list of records
	RecordCount            int             `json:"record_count"`
	ExecutionMethod        ExecutionMethod `json:"execution_method"`
	PolicyVersionsApplied  string          `json:"policy_versions_applied"`
	EligibilityEvidence    string          `json:"eligibility_evidence"`
	Exceptions             string          `json:"exceptions,omitempty"`
	Status                 BatchStatus     `json:"status"`
	ApproverPrincipalID    *string         `json:"approver_principal_id,omitempty"` // SoD: Cannot be executor (DG-042)
	ExecutorPrincipalID    *string         `json:"executor_principal_id,omitempty"`
	BackupTombstoneDigest  string          `json:"backup_tombstone_digest"`         // Tombstone instruction for restore convergence (DG-045)
	CreatedAt              time.Time       `json:"created_at"`
	ApprovedAt             *time.Time      `json:"approved_at,omitempty"`
	ExecutedAt             *time.Time      `json:"executed_at,omitempty"`
}

// DestructionCertificate provides sealed cryptographic proof of lawful disposition (§19.1, §28, DG-046, NP-34).
type DestructionCertificate struct {
	CertificateID          types.UUID      `json:"certificate_id"`
	TenantID               types.UUID      `json:"tenant_id"`
	BatchID                types.UUID      `json:"batch_id"`
	CertificateNumber      string          `json:"certificate_number"`
	Method                 ExecutionMethod `json:"method"`
	VerifiedPurgedCount    int             `json:"verified_purged_count"`
	CertificateDigestSHA256 string         `json:"certificate_digest_sha256"`
	IssuedAt               time.Time       `json:"issued_at"`
	IssuedBy               string          `json:"issued_by"`
}

// ProjectionConvergenceResult verifies that caches and search indexes have purged residual copies (DG-044, NP-22, NP-23).
type ProjectionConvergenceResult struct {
	CachesPurged       bool
	SearchIndexPurged  bool
	AnalyticsMartPurged bool
	ResidualCount      int
}
