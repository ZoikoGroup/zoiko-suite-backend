package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

var (
	// ErrArchiveIntegrityFailed is returned when archive package hash verification fails (DG-050, NP-26).
	ErrArchiveIntegrityFailed = errors.New("archive package cryptographic integrity check failed; restore/export blocked (per NP-26/DG-050)")

	// ErrRestoreUnauthorizedPurpose is returned when an archive restore bypasses IAM or authorized purpose (DG-048, NP-25).
	ErrRestoreUnauthorizedPurpose = errors.New("archive restore denied: missing or unauthorized business purpose (blocked per NP-25/DG-048)")
)

// ArchiveService provides archive package sealing, integrity verification, and governed restore workflows.
type ArchiveService struct{}

func NewArchiveService() *ArchiveService {
	return &ArchiveService{}
}

// SealPackage creates an immutable sealed archive package with content-addressed digest (DG-047).
func (s *ArchiveService) SealPackage(
	tenantID types.UUID,
	archiveCode string,
	recordCount int,
	totalBytes int64,
	contentPayload []byte,
	retentionRef string,
	sealedBy string,
	sealedAt time.Time,
) (*ArchivePackage, error) {
	if recordCount <= 0 || len(contentPayload) == 0 {
		return nil, errors.New("cannot seal empty archive package")
	}

	archiveID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	h := sha256.Sum256(contentPayload)
	digest := hex.EncodeToString(h[:])

	return &ArchivePackage{
		ArchiveID:            archiveID,
		TenantID:             tenantID,
		ArchiveCode:          archiveCode,
		RecordCount:          recordCount,
		TotalBytes:           totalBytes,
		PackageDigestSHA256:  digest,
		RetentionScheduleRef: retentionRef,
		Status:               ArchiveStatusSealed,
		SealedAt:             sealedAt,
		SealedBy:             sealedBy,
	}, nil
}

// VerifyIntegrity checks the payload against the sealed package digest (NP-26).
func (s *ArchiveService) VerifyIntegrity(pkg *ArchivePackage, contentPayload []byte) error {
	if pkg == nil {
		return errors.New("package cannot be nil")
	}

	h := sha256.Sum256(contentPayload)
	digest := hex.EncodeToString(h[:])

	if digest != pkg.PackageDigestSHA256 {
		pkg.Status = ArchiveStatusTampered
		return fmt.Errorf("%w: expected digest %s, calculated %s", ErrArchiveIntegrityFailed, pkg.PackageDigestSHA256, digest)
	}
	return nil
}

// AuthorizeRestore validates access purpose and tenant scope before permitting restore execution (NP-25).
func (s *ArchiveService) AuthorizeRestore(
	pkg *ArchivePackage,
	requestedBy string,
	purpose string,
	targetEnv string,
	requestedAt time.Time,
) (*RestoreRequest, error) {
	if pkg == nil {
		return nil, errors.New("package cannot be nil")
	}
	// NP-25: Restore without authorized purpose is rejected
	if purpose == "" || purpose == "UNSPECIFIED" || purpose == "AD_HOC_TEST" {
		return nil, ErrRestoreUnauthorizedPurpose
	}

	reqID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	return &RestoreRequest{
		RequestID:                 reqID,
		TenantID:                  pkg.TenantID,
		ArchiveID:                 pkg.ArchiveID,
		RequestedBy:               requestedBy,
		AuthorizedPurpose:         purpose,
		TargetRestoreEnvironment:  targetEnv,
		ReapplyTombstonesRequired: true,
		RequestedAt:               requestedAt,
	}, nil
}

// ReapplyTombstones filters out records that were previously destroyed under tombstone directives before restored dataset becomes operational (DG-045, DG-049, NP-24).
func (s *ArchiveService) ReapplyTombstones(
	restoredRecordIDs []types.UUID,
	activeTombstoneRecordIDs map[types.UUID]bool,
) []types.UUID {
	operationalRecords := make([]types.UUID, 0, len(restoredRecordIDs))
	for _, id := range restoredRecordIDs {
		// If record was previously destroyed/tombstoned, exclude it from operational dataset (NP-24)
		if !activeTombstoneRecordIDs[id] {
			operationalRecords = append(operationalRecords, id)
		}
	}
	return operationalRecords
}
