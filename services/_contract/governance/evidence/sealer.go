package evidence

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
	// ErrEvidenceTampered is returned when hash verification of a sealed package fails (DG-028, NP-09).
	ErrEvidenceTampered = errors.New("evidence content hash verification failed; package integrity compromised (blocked per NP-09/DG-028)")

	// ErrCustodyEventMissing is returned when evidence is exported or accessed without mandatory custody audit (DG-030, NP-10).
	ErrCustodyEventMissing = errors.New("mandatory custody event missing or invalid actor/purpose (blocked per NP-10/DG-030)")
)

// Sealer provides cryptographic integrity operations and custody auditing.
type Sealer struct{}

func NewSealer() *Sealer {
	return &Sealer{}
}

// SealManifest creates a cryptographic seal across all member evidence objects.
func (s *Sealer) SealManifest(
	tenantID types.UUID,
	manifestCode string,
	packageType string,
	objects []EvidenceObject,
	sealedBy string,
	sealedAt time.Time,
) (*EvidenceManifest, error) {
	if len(objects) == 0 {
		return nil, errors.New("cannot seal empty evidence manifest")
	}

	manifestID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	digest := computeManifestDigest(objects)
	var totalBytes int64
	for _, obj := range objects {
		totalBytes += obj.ByteSize
	}

	return &EvidenceManifest{
		ManifestID:           manifestID,
		TenantID:             tenantID,
		ManifestCode:         manifestCode,
		PackageType:          packageType,
		ObjectCount:          len(objects),
		TotalBytes:           totalBytes,
		ManifestDigestSHA256: digest,
		Status:               ManifestStatusSealed,
		SealedAt:             sealedAt,
		SealedBy:             sealedBy,
	}, nil
}

// VerifyManifest recomputes the cryptographic digest and verifies that no evidence object has been altered post-sealing (NP-09).
func (s *Sealer) VerifyManifest(manifest *EvidenceManifest, objects []EvidenceObject) error {
	if manifest == nil {
		return errors.New("manifest cannot be nil")
	}

	computed := computeManifestDigest(objects)
	if computed != manifest.ManifestDigestSHA256 {
		manifest.Status = ManifestStatusTampered
		return fmt.Errorf("%w: expected digest %s, calculated %s", ErrEvidenceTampered, manifest.ManifestDigestSHA256, computed)
	}

	manifest.Status = ManifestStatusVerified
	return nil
}

// AuthorizeExport verifies that all custody parameters are present before permitting evidence package export (NP-10).
func (s *Sealer) AuthorizeExport(
	tenantID types.UUID,
	manifestID types.UUID,
	actorPrincipalID string,
	actorRole string,
	purpose string,
	occurredAt time.Time,
) (*CustodyEvent, error) {
	if actorPrincipalID == "" || actorRole == "" || purpose == "" {
		return nil, ErrCustodyEventMissing
	}

	eventID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	return &CustodyEvent{
		EventID:          eventID,
		TenantID:         tenantID,
		ManifestID:       &manifestID,
		EventType:        CustodyEventExported,
		ActorPrincipalID: actorPrincipalID,
		ActorRole:        actorRole,
		Purpose:          purpose,
		OccurredAt:       occurredAt,
	}, nil
}

func computeManifestDigest(objects []EvidenceObject) string {
	hashes := make([]string, len(objects))
	for i, obj := range objects {
		hashes[i] = obj.ContentHashSHA256
	}
	sort.Strings(hashes)

	h := sha256.New()
	for _, hash := range hashes {
		h.Write([]byte(hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}
