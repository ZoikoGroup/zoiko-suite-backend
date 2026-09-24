package domain

// Types for the remaining ORG-02 / ORG-03 gaps closed by migration 000008:
// onboarding idempotency, FailedProvisioning, LEI, Draft → Verified → Active,
// and non-destructive merge.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"regexp"
	"time"
)

// ---------------------------------------------------------------------------
// Onboarding (ORG-02 §4.2 "Create by approved onboarding correlation /
// external customer key")
// ---------------------------------------------------------------------------

// ProvisioningFingerprint hashes the parts of a provisioning request that
// define WHICH tenant is being created. A replay with the same key and the
// same fingerprint is the same onboarding; the same key with a different
// fingerprint is a different request reusing someone else's key.
func ProvisioningFingerprint(req ProvisionTenantRequest) string {
	raw, _ := json.Marshal(struct {
		TenantCode           string `json:"tenant_code"`
		LegalName            string `json:"legal_name"`
		TradingName          string `json:"trading_name"`
		DefaultCurrencyCode  string `json:"default_currency_code"`
		PrimaryTimezone      string `json:"primary_timezone"`
		PrimaryLocale        string `json:"primary_locale"`
		OnboardingRequestRef string `json:"onboarding_request_ref"`
	}{req.TenantCode, req.LegalName, req.TradingName, req.DefaultCurrencyCode,
		req.PrimaryTimezone, req.PrimaryLocale, req.OnboardingRequestRef})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// LEI (ORG-03 mandatory control; ISO 17442)
// ---------------------------------------------------------------------------

// LEIStatus is a GLEIF registration status.
type LEIStatus string

// ValidLEIStatus reports whether s is a GLEIF registration status.
func ValidLEIStatus(s LEIStatus) bool {
	switch s {
	case "ISSUED", "LAPSED", "PENDING_TRANSFER", "PENDING_ARCHIVAL", "MERGED",
		"RETIRED", "ANNULLED", "DUPLICATE", "TRANSFERRED", "CANCELLED":
		return true
	}
	return false
}

var leiShape = regexp.MustCompile(`^[A-Z0-9]{18}[0-9]{2}$`)

// ValidLEI reports whether lei is a well-formed ISO 17442 LEI: 20 characters,
// 18 alphanumerics then two check digits, passing ISO 7064 MOD 97-10 (the
// whole code, letters as 10–35, is ≡ 1 mod 97).
func ValidLEI(lei string) bool {
	if !leiShape.MatchString(lei) {
		return false
	}
	digits := make([]byte, 0, 40)
	for _, c := range lei {
		if c >= 'A' && c <= 'Z' {
			v := int(c-'A') + 10
			digits = append(digits, byte('0'+v/10), byte('0'+v%10))
		} else {
			digits = append(digits, byte(c))
		}
	}
	n, ok := new(big.Int).SetString(string(digits), 10)
	if !ok {
		return false
	}
	return new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

// ---------------------------------------------------------------------------
// Draft → Verified → Active (ORG-03 §4.3)
// ---------------------------------------------------------------------------

// RequestEntityVerificationRequest is the body of POST /v1/entities/{id}/verification.
type RequestEntityVerificationRequest struct {
	// VerificationEvidenceRef points at what the entity was verified against
	// — a registry extract, a filing. Required: a verification nobody can
	// trace is not one.
	VerificationEvidenceRef string `json:"verification_evidence_ref"`
	Reason                  string `json:"reason"`
	ExpectedVersion         int64  `json:"expected_version"`
	CorrelationID           string `json:"correlation_id"`
}

// ActivateLegalEntityRequest is the body of POST /v1/entities/{id}/activation.
type ActivateLegalEntityRequest struct {
	Reason          string `json:"reason"`
	ExpectedVersion int64  `json:"expected_version"`
	CorrelationID   string `json:"correlation_id"`
}

// ---------------------------------------------------------------------------
// Non-destructive merge (ORG-03 §4.3 MergeDuplicateCandidate)
// ---------------------------------------------------------------------------

// MergeDuplicateCandidateRequest is the body of POST /v1/entities/{duplicateID}/merge.
type MergeDuplicateCandidateRequest struct {
	SurvivorLegalEntityID string `json:"survivor_legal_entity_id"`
	Reason                string `json:"reason"`
	EvidenceRef           string `json:"evidence_ref"`
	ExpectedVersion       int64  `json:"expected_version"`
	CorrelationID         string `json:"correlation_id"`
}

// UnmergeEntityRequest is the body of POST /v1/entities/{duplicateID}/unmerge.
type UnmergeEntityRequest struct {
	Reason          string `json:"reason"`
	ExpectedVersion int64  `json:"expected_version"`
	CorrelationID   string `json:"correlation_id"`
}

// EntityMergeRecord is one merge in an entity's lineage. Never deleted; an
// unmerge completes the record rather than removing it.
type EntityMergeRecord struct {
	MergeRecordID          string       `json:"merge_record_id"`
	TenantID               string       `json:"tenant_id"`
	DuplicateLegalEntityID string       `json:"duplicate_legal_entity_id"`
	SurvivorLegalEntityID  string       `json:"survivor_legal_entity_id"`
	PriorEntityStatus      EntityStatus `json:"prior_entity_status"`
	Reason                 string       `json:"reason"`
	EvidenceRef            *string      `json:"evidence_ref"`

	MergedByPrincipalID        string    `json:"merged_by_principal_id"`
	MergeApprovedByPrincipalID string    `json:"merge_approved_by_principal_id"`
	MergeApprovalRequestID     string    `json:"merge_approval_request_id"`
	MergedAt                   time.Time `json:"merged_at"`

	UnmergedByPrincipalID        *string    `json:"unmerged_by_principal_id"`
	UnmergeApprovedByPrincipalID *string    `json:"unmerge_approved_by_principal_id"`
	UnmergeApprovalRequestID     *string    `json:"unmerge_approval_request_id"`
	UnmergedAt                   *time.Time `json:"unmerged_at"`
	UnmergeReason                *string    `json:"unmerge_reason"`
}
