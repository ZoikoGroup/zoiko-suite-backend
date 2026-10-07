package certification

import (
	"errors"
	"fmt"
	"time"

	"zoiko.io/contract/types"
)

var (
	// ErrMissingAILineage is returned when an AI embedding or derived model product lacks source lineage (DG-054, NP-27).
	ErrMissingAILineage = errors.New("AI embedding/derived product has no source lineage; cannot be certified or used for controlled purpose (blocked per NP-27/DG-054)")

	// ErrUnauthorizedDeclassification is returned when a derived aggregate claims lower classification without rule (DG-054, NP-28).
	ErrUnauthorizedDeclassification = errors.New("derived aggregate claims declassification without approved rule; inherits most restrictive source classification (blocked per NP-28/DG-054)")

	// ErrUnregisteredTransfer is returned when a cross-region data copy is not registered in governance transfer logs (DG-009, NP-29).
	ErrUnregisteredTransfer = errors.New("cross-region data transfer not registered in governance transfer registry; copy quarantined (blocked per NP-29/DG-009)")

	// ErrGenericPatchProhibited is returned when an API caller attempts generic PATCH on governance states (NP-36).
	ErrGenericPatchProhibited = errors.New("generic HTTP PATCH on retention/hold/certification status is prohibited; only named commands accepted (blocked per NP-36)")

	// ErrFailClosedDBUnavailable is returned when governance store is unreachable during critical lifecycle decisions (NP-35).
	ErrFailClosedDBUnavailable = errors.New("governance registry store unavailable; failing closed on destructive/hold operations (blocked per NP-35)")
)

// Service provides certification issuance, derivative governance, and operational policy checks.
type Service struct{}

func NewService() *Service {
	return &Service{}
}

// IssueCertification issues a formal data certification tier (DG-057).
func (s *Service) IssueCertification(
	tenantID types.UUID,
	assetID types.UUID,
	class CertificationClass,
	manifestID *types.UUID,
	dqRunID *types.UUID,
	lineageHash string,
	certifiedBy string,
	certifiedAt time.Time,
	validDuration time.Duration,
) (*DataCertification, error) {
	// For C3 and C4, manifest, DQ run, and lineage hash are mandatory
	if class == ClassC3FinancialRegulatory || class == ClassC4LegalEvidentiary {
		if manifestID == nil || dqRunID == nil || lineageHash == "" {
			return nil, fmt.Errorf("class %s requires complete evidence manifest, DQ run, and lineage hash (DG-057)", class)
		}
	}

	certID, err := types.NewV7()
	if err != nil {
		return nil, err
	}

	return &DataCertification{
		CertificationID:    certID,
		TenantID:           tenantID,
		AssetID:            assetID,
		CertClass:          class,
		EvidenceManifestID: manifestID,
		DQRunID:            dqRunID,
		LineageHash:        lineageHash,
		Status:             CertStatusEffective,
		CertifiedAt:        certifiedAt,
		CertifiedBy:        certifiedBy,
		ExpiresAt:          certifiedAt.Add(validDuration),
	}, nil
}

// InvalidateOnOwnerChange invalidates active certification upon change in accountable owner (NP-30, DG-058).
func (s *Service) InvalidateOnOwnerChange(cert *DataCertification, newOwnerPrincipalID string, currentOwnerPrincipalID string) {
	if cert != nil && newOwnerPrincipalID != currentOwnerPrincipalID {
		cert.Status = CertStatusInvalidated
	}
}

// AssertAILineage verifies that an AI embedding or derivative has an upstream lineage provenance chain (NP-27, DG-054).
func (s *Service) AssertAILineage(hasLineage bool) error {
	if !hasLineage {
		return ErrMissingAILineage
	}
	return nil
}

// ResolveDerivedClassification enforces that derived data inherits the most restrictive classification (NP-28, DG-054).
func (s *Service) ResolveDerivedClassification(
	sourceClassifications []string,
	hasApprovedDeclassificationRule bool,
	claimedClassification string,
) (string, error) {
	// Ranking: RESTRICTED > CONFIDENTIAL > INTERNAL > PUBLIC
	rank := map[string]int{"RESTRICTED": 4, "CONFIDENTIAL": 3, "INTERNAL": 2, "PUBLIC": 1}

	maxRank := 1
	mostRestrictive := "PUBLIC"
	for _, c := range sourceClassifications {
		if r, ok := rank[c]; ok && r > maxRank {
			maxRank = r
			mostRestrictive = c
		}
	}

	if claimedClassification != mostRestrictive && !hasApprovedDeclassificationRule {
		return mostRestrictive, ErrUnauthorizedDeclassification
	}
	if hasApprovedDeclassificationRule {
		return claimedClassification, nil
	}
	return mostRestrictive, nil
}

// ValidateCrossRegionTransfer verifies that cross-border transfer records are pre-registered (NP-29).
func (s *Service) ValidateCrossRegionTransfer(transfer *DataTransferRecord) error {
	if transfer == nil || !transfer.IsRegistered || transfer.LawfulBasis == "" {
		return ErrUnregisteredTransfer
	}
	return nil
}

// AssertNamedCommandOnly enforces that API endpoints reject generic PATCH (NP-36).
func (s *Service) AssertNamedCommandOnly(httpMethod string, isNamedCommand bool) error {
	if httpMethod == "PATCH" || !isNamedCommand {
		return ErrGenericPatchProhibited
	}
	return nil
}

// FailClosedOnDBUnavailable implements fail-closed semantics when DB is down (NP-35).
func (s *Service) FailClosedOnDBUnavailable(dbConnected bool) error {
	if !dbConnected {
		return ErrFailClosedDBUnavailable
	}
	return nil
}
