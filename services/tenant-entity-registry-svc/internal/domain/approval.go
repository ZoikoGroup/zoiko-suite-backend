package domain

// Verified maker-checker for ORG-02 §4.2 and ORG-03 §4.3.
//
// An ApprovalRequest is a governed command that has been proposed and not yet
// executed. The maker is the verified caller who proposed it; the approver is
// the verified caller of /approve. Neither identity is ever read from a request
// body — that is the whole difference between this and the approver field it
// replaces, which the maker filled in themselves.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// ApprovalSubjectType names what kind of command is awaiting approval.
type ApprovalSubjectType string

const (
	// ApprovalSubjectTenantCreation — §4.2 "Tenant creation … require[s]
	// maker-checker". The tenant exists in ONBOARDING while this is pending and
	// cannot be activated until it is approved.
	ApprovalSubjectTenantCreation ApprovalSubjectType = "TENANT_CREATION"
	// ApprovalSubjectTenantCommand — the maker-checker lifecycle commands
	// (InitiateTermination, CompleteTermination).
	ApprovalSubjectTenantCommand ApprovalSubjectType = "TENANT_COMMAND"
	// ApprovalSubjectLegalProfileAmendment — §4.3 "Maker cannot approve
	// legal-name/registry/jurisdiction change".
	ApprovalSubjectLegalProfileAmendment ApprovalSubjectType = "LEGAL_PROFILE_AMENDMENT"
	// ApprovalSubjectRegistryConflictResolution — §4.3 "no self-approval of
	// merge". A resolution concludes whether two registry claims are the same
	// entity, which is the decision a merge would act on.
	ApprovalSubjectRegistryConflictResolution ApprovalSubjectType = "REGISTRY_CONFLICT_RESOLUTION"
	// ApprovalSubjectLegalEntityVerification — DRAFT → VERIFIED. The approver
	// may be neither the requester nor the entity's creator.
	ApprovalSubjectLegalEntityVerification ApprovalSubjectType = "LEGAL_ENTITY_VERIFICATION"
	// ApprovalSubjectLegalEntityMerge / Unmerge — MergeDuplicateCandidate
	// and its reversal ("no self-approval of merge").
	ApprovalSubjectLegalEntityMerge   ApprovalSubjectType = "LEGAL_ENTITY_MERGE"
	ApprovalSubjectLegalEntityUnmerge ApprovalSubjectType = "LEGAL_ENTITY_UNMERGE"
)

// ApprovalStatus is the state of an approval request.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "PENDING"
	ApprovalApproved ApprovalStatus = "APPROVED"
	ApprovalRejected ApprovalStatus = "REJECTED"
	// ApprovalStale — the subject moved after the proposal (a newer version, a
	// different lifecycle state, a conflict already resolved), so the command
	// as proposed can no longer apply. The maker re-proposes against the
	// current state; the old approval is never stretched to cover it.
	ApprovalStale ApprovalStatus = "STALE"
	// ApprovalExpired — nobody decided within the TTL.
	ApprovalExpired ApprovalStatus = "EXPIRED"
)

// ApprovalRequest is a proposed governed command awaiting a second party.
type ApprovalRequest struct {
	ApprovalRequestID string              `json:"approval_request_id"`
	TenantID          string              `json:"tenant_id"`
	SubjectType       ApprovalSubjectType `json:"subject_type"`
	SubjectID         string              `json:"subject_id"`
	CommandName       string              `json:"command_name"`
	// Payload is the command exactly as proposed, and exactly what runs on
	// approval. Returned so the approver can review it.
	Payload         json.RawMessage `json:"payload"`
	ExpectedVersion int64           `json:"expected_version"`
	// PayloadFingerprint must be echoed back on /approve. It proves the
	// approver is approving the proposal they reviewed.
	PayloadFingerprint string `json:"payload_fingerprint"`
	Reason             string `json:"reason"`

	RequestedByPrincipalID string    `json:"requested_by_principal_id"`
	RequestedAt            time.Time `json:"requested_at"`
	ExpiresAt              time.Time `json:"expires_at"`

	Status               ApprovalStatus `json:"status"`
	DecidedByPrincipalID *string        `json:"decided_by_principal_id"`
	DecidedAt            *time.Time     `json:"decided_at"`
	DecisionNote         *string        `json:"decision_note"`
	CorrelationID        *string        `json:"correlation_id"`
}

// ApprovalDecision is a verified approver's decision, carried into the store
// so the approval is recorded in the SAME transaction as the fact it
// authorises. Either both land or neither does.
type ApprovalDecision struct {
	ApprovalRequestID    string
	TenantID             string
	DecidedByPrincipalID string
	Note                 string
}

// ApproveRequestBody is the body of POST /v1/approval-requests/{id}/approve.
type ApproveRequestBody struct {
	// PayloadFingerprint is the fingerprint the approver was shown. Required.
	PayloadFingerprint string `json:"payload_fingerprint"`
	Note               string `json:"note"`
	CorrelationID      string `json:"correlation_id"`
}

// RejectRequestBody is the body of POST /v1/approval-requests/{id}/reject.
type RejectRequestBody struct {
	Note          string `json:"note"`
	CorrelationID string `json:"correlation_id"`
}

// ApprovalOutcome is the result of an approval: the decided request and the
// result of the command it released (a TenantCommandResult, a profile
// version, or nil where the command produces no body).
type ApprovalOutcome struct {
	ApprovalRequest *ApprovalRequest `json:"approval_request"`
	Result          any              `json:"result,omitempty"`
}

// TenantCreationPayload is what a TENANT_CREATION approval binds: the tenant's
// identity as provisioned. Locale and timezone are deliberately absent — they
// are ChangeDefaultLocale's to change during onboarding, and an approval of
// WHO the tenant is should not go stale because its default locale moved.
type TenantCreationPayload struct {
	TenantCode          string  `json:"tenant_code"`
	LegalName           string  `json:"legal_name"`
	TradingName         *string `json:"trading_name"`
	DefaultCurrencyCode string  `json:"default_currency_code"`
	// Onboarding evidence (§4.2 "onboarding request"), bound by the approval.
	ExternalCustomerKey  *string `json:"external_customer_key"`
	OnboardingRequestRef *string `json:"onboarding_request_ref"`
}

// ApprovalFingerprint is the SHA-256 over everything an approval binds.
//
// payload must be the TYPED command struct, not raw JSON. Postgres JSONB
// reorders object keys, so hashing the stored bytes would change the
// fingerprint across a round trip; re-marshalling the decoded struct gives
// Go's fixed field order and the same digest every time.
func ApprovalFingerprint(subjectType ApprovalSubjectType, subjectID, command string, expectedVersion int64, payload any) (string, error) {
	raw, err := json.Marshal(struct {
		SubjectType     ApprovalSubjectType `json:"subject_type"`
		SubjectID       string              `json:"subject_id"`
		Command         string              `json:"command"`
		ExpectedVersion int64               `json:"expected_version"`
		Payload         any                 `json:"payload"`
	}{subjectType, subjectID, command, expectedVersion, payload})
	if err != nil {
		return "", fmt.Errorf("fingerprint: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
