package domain

import (
	"errors"
	"time"
)

// Sentinel errors
var (
	ErrContractNotFound           = errors.New("contract not found")
	ErrContractNotDraft           = errors.New("contract is not in DRAFT status")
	ErrContractTerminated         = errors.New("contract is already TERMINATED")
	ErrVersionConflict            = errors.New("contract version conflict")
	ErrGovernanceDecisionRequired = errors.New("governance_decision_id is required to approve a contract")

	// ErrWrongLifecycleStatus means the contract is not in the status a
	// command requires — e.g. ApproveContract called on a contract not in
	// REVIEW.
	ErrWrongLifecycleStatus = errors.New("contract is not in the required status for this action")

	// ErrSelfApprovalNotAllowed enforces LEG-05's named SoD requirement
	// ("self-approval blocked", docs/architecture/original_doc §7): the
	// principal who submitted a contract for review may not be the one who
	// approves it.
	ErrSelfApprovalNotAllowed = errors.New("principal may not approve a contract they submitted for review")

	// ErrSignatureNotSent means RecordExecution was called before
	// SendForSignature — execution evidence with no corresponding
	// signature request is not a real signing event.
	ErrSignatureNotSent = errors.New("signature has not been sent for this contract")

	// ErrTenantMissing means the request carried no X-Tenant-Id. It is an
	// unauthenticated request, not an empty tenant named "default".
	ErrTenantMissing = errors.New("tenant scope missing")
)

// ContractType enumerates the kinds of legal agreements supported.
type ContractType string

const (
	ContractTypeVendor      ContractType = "VENDOR"
	ContractTypeEmployment  ContractType = "EMPLOYMENT"
	ContractTypeNDA         ContractType = "NDA"
	ContractTypeMSA         ContractType = "MSA"
	ContractTypeSLA         ContractType = "SLA"
	ContractTypePartnership ContractType = "PARTNERSHIP"
	ContractTypeOther       ContractType = "OTHER"
)

// ContractStatus is the Instrument lifecycle dimension (LEG-05 §2.2/§7).
// It is deliberately NOT a catch-all: signature state lives in
// SignatureStatus, a separate, independently-evolving dimension — see
// migration 000003's doc comment for why one combined field is prohibited.
type ContractStatus string

const (
	ContractStatusDraft      ContractStatus = "DRAFT"
	ContractStatusReview     ContractStatus = "REVIEW"
	ContractStatusApproved   ContractStatus = "APPROVED"
	ContractStatusExecuted   ContractStatus = "EXECUTED"
	ContractStatusEffective  ContractStatus = "EFFECTIVE"
	ContractStatusTerminated ContractStatus = "TERMINATED"
	ContractStatusExpired    ContractStatus = "EXPIRED"
	ContractStatusArchived   ContractStatus = "ARCHIVED"
)

// IsFinal reports whether no further lifecycle command may act on the
// contract.
func (s ContractStatus) IsFinal() bool {
	return s == ContractStatusTerminated || s == ContractStatusExpired || s == ContractStatusArchived
}

// SignatureStatus is the Signature dimension (LEG-05 §2.2), independent of
// ContractStatus: a contract may be APPROVED with signature still SENT, not
// yet COMPLETED.
type SignatureStatus string

const (
	SignatureStatusNotRequested  SignatureStatus = "NOT_REQUESTED"
	SignatureStatusSent          SignatureStatus = "SENT"
	SignatureStatusPartiallySigned SignatureStatus = "PARTIALLY_SIGNED"
	SignatureStatusCompleted     SignatureStatus = "COMPLETED"
	SignatureStatusDeclined      SignatureStatus = "DECLINED"
	SignatureStatusExpired       SignatureStatus = "EXPIRED"
	SignatureStatusPendingUnknown SignatureStatus = "PENDING_UNKNOWN"
	SignatureStatusVoided        SignatureStatus = "VOIDED"
)

// Contract is the core domain entity representing a legal agreement.
// All material records carry tenant_id, legal_entity_id, and effective dates.
type Contract struct {
	ContractID           string         `json:"contract_id"`
	TenantID             string         `json:"tenant_id"`
	LegalEntityID        string         `json:"legal_entity_id"`
	ContractType         ContractType   `json:"contract_type"`
	Title                string         `json:"title"`
	Description          string         `json:"description,omitempty"`
	CounterpartyID       string          `json:"counterparty_id"`
	CounterpartyName     string          `json:"counterparty_name"`
	Status               ContractStatus  `json:"status"`
	SignatureStatus      SignatureStatus `json:"signature_status"`
	Version              int             `json:"version"`
	EffectiveFrom        string          `json:"effective_from"`
	EffectiveTo          *string         `json:"effective_to,omitempty"`
	SubmittedAt          *time.Time      `json:"submitted_at,omitempty"`
	SubmittedBy          *string         `json:"submitted_by,omitempty"`
	ApprovedAt           *time.Time      `json:"approved_at,omitempty"`
	ApprovedBy           *string         `json:"approved_by,omitempty"`
	SignatureSentAt      *time.Time      `json:"signature_sent_at,omitempty"`
	ExecutedAt           *time.Time      `json:"executed_at,omitempty"`
	SignedBy             *string         `json:"signed_by,omitempty"`
	EffectiveAt          *time.Time      `json:"effective_at,omitempty"`
	AmendedAt            *time.Time      `json:"amended_at,omitempty"`
	AmendedBy            *string         `json:"amended_by,omitempty"`
	RenewedAt            *time.Time      `json:"renewed_at,omitempty"`
	RenewedBy            *string         `json:"renewed_by,omitempty"`
	TerminatedAt         *time.Time      `json:"terminated_at,omitempty"`
	TerminatedBy         *string         `json:"terminated_by,omitempty"`
	TerminationNote      *string         `json:"termination_note,omitempty"`
	Currency             string          `json:"currency"`
	TotalValue           float64         `json:"total_value"`
	DocumentVaultID      *string         `json:"document_vault_id,omitempty"`
	GovernanceDecisionID *string         `json:"governance_decision_id,omitempty"`
	CreatedBy            string          `json:"created_by"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// ContractVersion is an immutable snapshot of a contract at a point in time.
type ContractVersion struct {
	VersionID     string         `json:"version_id"`
	ContractID    string         `json:"contract_id"`
	TenantID      string         `json:"tenant_id"`
	VersionNumber int            `json:"version_number"`
	Status        ContractStatus `json:"status"`
	Title         string         `json:"title"`
	Description   string         `json:"description,omitempty"`
	EffectiveFrom string         `json:"effective_from"`
	EffectiveTo   *string        `json:"effective_to,omitempty"`
	ChangeSummary string         `json:"change_summary"`
	CreatedBy     string         `json:"created_by"`
	CreatedAt     time.Time      `json:"created_at"`
}

// --- Request / Response DTOs ---

type CreateContractRequest struct {
	LegalEntityID    string       `json:"legal_entity_id"`
	ContractType     ContractType `json:"contract_type"`
	Title            string       `json:"title"`
	Description      string       `json:"description,omitempty"`
	CounterpartyID   string       `json:"counterparty_id"`
	CounterpartyName string       `json:"counterparty_name"`
	EffectiveFrom    string       `json:"effective_from"`
	EffectiveTo      *string      `json:"effective_to,omitempty"`
	Currency         string       `json:"currency"`
	TotalValue       float64      `json:"total_value"`
	CreatedBy        string       `json:"created_by"`
}

type UpdateContractRequest struct {
	Title            string  `json:"title,omitempty"`
	Description      string  `json:"description,omitempty"`
	CounterpartyName string  `json:"counterparty_name,omitempty"`
	EffectiveTo      *string `json:"effective_to,omitempty"`
	Currency         string  `json:"currency,omitempty"`
	TotalValue       float64 `json:"total_value,omitempty"`
	ChangeSummary    string  `json:"change_summary"`
	UpdatedBy        string  `json:"updated_by"`
}

type SubmitReviewRequest struct {
	SubmittedBy string `json:"submitted_by"`
}

type ApproveContractRequest struct {
	ApprovedBy string `json:"approved_by"`
	// GovernanceDecisionID is required: the governance-decision-log-svc
	// decision that authorized this approval. Verified GRANTED before the
	// write proceeds — see handler.ApproveContract.
	GovernanceDecisionID string `json:"governance_decision_id"`
}

type SendForSignatureRequest struct {
	SentBy string `json:"sent_by"`
}

// RecordExecutionRequest carries the signing evidence produced once every
// required signature is in. LEG-08 (Electronic Signature Adapter) would
// normally be this service's source for SignatureStatus transitions; it
// does not exist anywhere in this repo yet, so execution is caller-asserted
// here — the same documented limitation pattern as board-resolutions-svc's
// voter roster.
type RecordExecutionRequest struct {
	SignedBy        string  `json:"signed_by"`
	DocumentVaultID *string `json:"document_vault_id,omitempty"`
}

type AmendContractRequest struct {
	AmendedBy        string  `json:"amended_by"`
	Title            string  `json:"title,omitempty"`
	Description      string  `json:"description,omitempty"`
	CounterpartyName string  `json:"counterparty_name,omitempty"`
	TotalValue       float64 `json:"total_value,omitempty"`
	ChangeSummary    string  `json:"change_summary"`
}

type RenewContractRequest struct {
	RenewedBy     string `json:"renewed_by"`
	NewEffectiveTo string `json:"new_effective_to"`
	ChangeSummary string `json:"change_summary"`
}

type TerminateContractRequest struct {
	TerminatedBy    string `json:"terminated_by"`
	TerminationNote string `json:"termination_note"`
}
