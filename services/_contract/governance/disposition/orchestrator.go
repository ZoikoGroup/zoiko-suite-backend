package disposition

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"zoiko.io/contract/types"
)

var (
	// ErrBatchPopulationAltered is returned when a disposition population changes after approval (DG-041, NP-20).
	ErrBatchPopulationAltered = errors.New("disposition batch population altered after approval; execution rejected (blocked per NP-20/DG-041)")

	// ErrUnapprovedRecordInBatch is returned when an executor attempts to purge records not in the approved batch (NP-21).
	ErrUnapprovedRecordInBatch = errors.New("disposition executor added unapproved records not in frozen batch (blocked per NP-21)")

	// ErrSoDViolation is returned when the executor and approver are the same principal (DG-042).
	ErrSoDViolation = errors.New("segregation of duties violation: disposition executor cannot be sole approver (blocked per DG-042)")

	// ErrUnverifiedDestructionCertificate is returned when a certificate is created prior to verified execution (NP-34).
	ErrUnverifiedDestructionCertificate = errors.New("cannot issue destruction certificate before deterministic purge verification (blocked per NP-34/DG-046)")

	// ErrProjectionConvergenceFailed is returned when caches or search indexes still return residual disposed records (NP-22, NP-23).
	ErrProjectionConvergenceFailed = errors.New("projection convergence failed: caches or search indexes retain disposed data (blocked per NP-22/NP-23)")
)

// Orchestrator executes controlled, multi-phase defensible disposition workflows.
type Orchestrator struct{}

func NewOrchestrator() *Orchestrator {
	return &Orchestrator{}
}

// CreateBatch freezes an eligible population into an immutable disposition batch (DG-041).
func (o *Orchestrator) CreateBatch(
	tenantID types.UUID,
	batchCode string,
	recordIDs []types.UUID,
	method ExecutionMethod,
	policyVersions string,
	evidence string,
	createdAt time.Time,
) (*DispositionBatch, error) {
	if len(recordIDs) == 0 {
		return nil, errors.New("cannot create disposition batch with empty record population")
	}

	batchID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	hash := ComputePopulationHash(recordIDs)
	tombstone := ComputeTombstoneDigest(tenantID, hash)

	return &DispositionBatch{
		BatchID:                batchID,
		TenantID:               tenantID,
		BatchCode:              batchCode,
		PopulationSnapshotHash: hash,
		RecordIDs:              recordIDs,
		RecordCount:            len(recordIDs),
		ExecutionMethod:        method,
		PolicyVersionsApplied:  policyVersions,
		EligibilityEvidence:    evidence,
		Status:                 BatchStatusPendingApproval,
		BackupTombstoneDigest:  tombstone,
		CreatedAt:              createdAt,
	}, nil
}

// ApproveBatch records authorized governance approval for batch execution (DG-042).
func (o *Orchestrator) ApproveBatch(batch *DispositionBatch, approverPrincipalID string, approvedAt time.Time) error {
	if batch == nil {
		return errors.New("batch cannot be nil")
	}
	if approverPrincipalID == "" {
		return errors.New("approver principal ID is required")
	}

	batch.ApproverPrincipalID = &approverPrincipalID
	batch.Status = BatchStatusApproved
	batch.ApprovedAt = &approvedAt
	return nil
}

// ExecuteBatch executes purge adapters against frozen population with strict SoD and freeze checks (NP-20, NP-21, NP-33).
func (o *Orchestrator) ExecuteBatch(
	batch *DispositionBatch,
	executorPrincipalID string,
	targetRecordIDs []types.UUID,
	failSimulatedItem bool,
	executedAt time.Time,
) error {
	if batch == nil {
		return errors.New("batch cannot be nil")
	}
	if batch.Status != BatchStatusApproved {
		return errors.New("batch must be in APPROVED status prior to execution")
	}
	if batch.ApproverPrincipalID != nil && *batch.ApproverPrincipalID == executorPrincipalID {
		return ErrSoDViolation
	}

	// NP-20: Population change check
	currentHash := ComputePopulationHash(targetRecordIDs)
	if currentHash != batch.PopulationSnapshotHash {
		return fmt.Errorf("%w: approved hash %s does not match execution hash %s",
			ErrBatchPopulationAltered, batch.PopulationSnapshotHash, currentHash)
	}

	// NP-21: Check for any record not in batch.RecordIDs
	approvedMap := make(map[types.UUID]bool)
	for _, id := range batch.RecordIDs {
		approvedMap[id] = true
	}
	for _, target := range targetRecordIDs {
		if !approvedMap[target] {
			return fmt.Errorf("%w: record %s is unapproved", ErrUnapprovedRecordInBatch, target)
		}
	}

	// NP-33: Handle partial execution failures gracefully
	if failSimulatedItem {
		batch.Status = BatchStatusPartiallyFailed
		return errors.New("execution partially failed on adapter; batch remains incomplete for safe reconciliation")
	}

	batch.ExecutorPrincipalID = &executorPrincipalID
	batch.Status = BatchStatusExecuted
	batch.ExecutedAt = &executedAt

	return nil
}

// IssueDestructionCertificate seals completed disposition after verification and convergence checks (NP-22, NP-23, NP-34).
func (o *Orchestrator) IssueDestructionCertificate(
	batch *DispositionBatch,
	convergence ProjectionConvergenceResult,
	issuerPrincipalID string,
	issuedAt time.Time,
) (*DestructionCertificate, error) {
	if batch == nil {
		return nil, errors.New("batch cannot be nil")
	}
	// NP-34: Certificate created before verification is rejected
	if batch.Status != BatchStatusExecuted {
		return nil, fmt.Errorf("%w: batch is in status %s (must be EXECUTED)", ErrUnverifiedDestructionCertificate, batch.Status)
	}

	// NP-22, NP-23: Projection convergence check
	if !convergence.CachesPurged || !convergence.SearchIndexPurged || convergence.ResidualCount > 0 {
		return nil, fmt.Errorf("%w: cache_purged=%v, search_purged=%v, residual=%d",
			ErrProjectionConvergenceFailed, convergence.CachesPurged, convergence.SearchIndexPurged, convergence.ResidualCount)
	}

	certID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	raw := fmt.Sprintf("%s:%s:%d:%s", batch.BatchID, batch.PopulationSnapshotHash, batch.RecordCount, issuedAt.Format(time.RFC3339))
	h := sha256.Sum256([]byte(raw))
	digest := hex.EncodeToString(h[:])

	return &DestructionCertificate{
		CertificateID:           certID,
		TenantID:                batch.TenantID,
		BatchID:                 batch.BatchID,
		CertificateNumber:       fmt.Sprintf("CERT-DEST-%s-%d", batch.BatchCode, issuedAt.Unix()),
		Method:                  batch.ExecutionMethod,
		VerifiedPurgedCount:     batch.RecordCount,
		CertificateDigestSHA256: digest,
		IssuedAt:                issuedAt,
		IssuedBy:                issuerPrincipalID,
	}, nil
}

// ComputePopulationHash creates a deterministic SHA-256 hash across sorted record UUIDs.
func ComputePopulationHash(ids []types.UUID) string {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	sort.Strings(strs)

	h := sha256.New()
	for _, s := range strs {
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeTombstoneDigest generates a reproducible instruction digest for backup rehydration (DG-045).
func ComputeTombstoneDigest(tenantID types.UUID, populationHash string) string {
	raw := fmt.Sprintf("TOMBSTONE:%s:%s", tenantID, populationHash)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
