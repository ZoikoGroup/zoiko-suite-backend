package domain

// ZS-SVC-L-001 WFP-07 — Payroll Payment Interface.
//
// A PayrollPaymentInstruction asks Banking to pay a worker's net pay (NET_PAY) or an
// authority's liability (LIABILITY). WFP-07 owns the instruction subject, its approval
// fingerprint, the source links and the Banking handoff references. It never owns the
// beneficiary bank master, the provider message, execution, settlement or finality, and
// it never trusts bank details from payroll data: the beneficiary is the payee's ACTIVE
// destination in payee-banking-identity-svc, pinned by id and version.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	PayDraft       = "DRAFT"
	PayValidated   = "VALIDATED"
	PayApproved    = "APPROVED"
	PaySubmitted   = "SUBMITTED_TO_BANKING"
	PayHandedOff   = "HANDED_OFF"
	PayCancelled   = "CANCELLED"
	PaySuperseded  = "SUPERSEDED"
	PayTypeNetPay  = "NET_PAY"
	PayTypeLiab    = "LIABILITY"
	PayMethodBank  = "BANK_TRANSFER"
	PolicyApproval = "APPROVAL_REQUIRED"

	// AuthorizationSourcePayroll is how payment-initiation-adapter-svc recognizes an
	// instruction handed over by this service; it re-fetches the fingerprint from us.
	AuthorizationSourcePayroll = "PAYROLL_RUN_SVC"

	PaymentFingerprintVersion = 1
)

const (
	EventPayrollPaymentInstrCreated    = "payroll_payment_instruction_created"
	EventPayrollPaymentInstrCancelled  = "payroll_payment_instruction_cancelled"
	EventPayrollPaymentInstrSuperseded = "payroll_payment_instruction_superseded"
)

// BankingIdempotencyKey is deterministic per instruction, so a retry (after a timeout, a
// crash, another replica) reuses the same Banking attempt and can never pay twice.
func BankingIdempotencyKey(instructionID string) string {
	return "wfp-payroll-instruction:" + instructionID
}

// PayeeDestination is what WFP-07 pins of a beneficiary. It carries identities and
// digests only, never bank details.
type PayeeDestination struct {
	PayeeRef      string
	DestinationID string
	Version       string // the destination's updated_at, as Banking identity reports it
	Fingerprint   string // digest of institution|account|currency
	LegalEntityID string
	Currency      string
	// Principals who proposed, verified or approved the destination (their payee-master
	// maker/checker record). None of them may approve a payment to it.
	ChangedBy []string
}

func (d PayeeDestination) ChangedByPrincipal(p string) bool {
	if p == "" {
		return false
	}
	for _, c := range d.ChangedBy {
		if c == p {
			return true
		}
	}
	return false
}

type PaymentInstruction struct {
	InstructionID          string            `json:"instruction_id"`
	TenantID               string            `json:"tenant_id"`
	LegalEntityID          string            `json:"legal_entity_id"`
	InstructionType        string            `json:"instruction_type"`
	RunRefID               string            `json:"run_ref_id"`
	ResultID               string            `json:"result_id"`
	WorkerID               *string           `json:"worker_id,omitempty"`
	LiabilityID            *string           `json:"liability_id,omitempty"`
	PayeeRef               string            `json:"payee_ref"`
	DestinationID          string            `json:"destination_id"`
	DestinationVersion     string            `json:"destination_version"`
	DestinationFingerprint string            `json:"destination_fingerprint"`
	Amount                 string            `json:"amount"`
	Currency               string            `json:"currency"`
	ValueDate              string            `json:"value_date"`
	PayerAccountRef        string            `json:"payer_account_ref"`
	PaymentMethod          string            `json:"payment_method"`
	PaymentReference       string            `json:"payment_reference"`
	Fingerprint            string            `json:"fingerprint"`
	State                  string            `json:"state"`
	PolicyResult           *string           `json:"policy_result,omitempty"`
	PolicyVersionID        *string           `json:"policy_version_id,omitempty"`
	RequiredSignatures     int               `json:"required_signatures"`
	SignaturesCollected    int               `json:"signatures_collected"`
	ApprovedFingerprint    *string           `json:"approved_fingerprint,omitempty"`
	ApprovedAt             *time.Time        `json:"approved_at,omitempty"`
	InvalidationReason     *string           `json:"invalidation_reason,omitempty"`
	InvalidatedAt          *time.Time        `json:"invalidated_at,omitempty"`
	CancelReason           *string           `json:"cancel_reason,omitempty"`
	AttemptID              *string           `json:"attempt_id,omitempty"`
	BankingStatus          *string           `json:"banking_status,omitempty"`
	BankingError           *string           `json:"banking_error,omitempty"`
	SubmittedBy            *string           `json:"submitted_by,omitempty"`
	SubmittedAt            *time.Time        `json:"submitted_at,omitempty"`
	HandedOffAt            *time.Time        `json:"handed_off_at,omitempty"`
	IdempotencyKey         *string           `json:"idempotency_key,omitempty"`
	Version                int               `json:"version"`
	CorrelationID          string            `json:"correlation_id"`
	CreatedBy              string            `json:"created_by"`
	CreatedAt              time.Time         `json:"created_at"`
	UpdatedAt              time.Time         `json:"updated_at"`
	PaymentID              *string           `json:"payment_id,omitempty"`
	ReturnedAt             *time.Time        `json:"returned_at,omitempty"`
	ReturnReason           *string           `json:"return_reason,omitempty"`
	Approvals              []PaymentApproval `json:"approvals,omitempty"`
}

type PaymentApproval struct {
	ApprovalID    string     `json:"approval_id"`
	Approver      string     `json:"approver"`
	Fingerprint   string     `json:"fingerprint"`
	CreatedAt     time.Time  `json:"created_at"`
	InvalidatedAt *time.Time `json:"invalidated_at,omitempty"`
}

// ComputeInstructionFingerprint binds the payroll result, the subject, the payee
// destination and its version, the amount, currency, value date and the paying-account
// context. Approval is of this digest: anything it covers changing means the approval no
// longer applies. Every field is quoted so none can bleed into the next.
func ComputeInstructionFingerprint(i *PaymentInstruction) string {
	subject := ""
	if i.WorkerID != nil {
		subject = "w:" + *i.WorkerID
	}
	if i.LiabilityID != nil {
		subject = "l:" + *i.LiabilityID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "v%d|%q|%q|%q|%q|%q|%q|%q|%q|%q|%q|%q|%q|%q|%q|%q",
		PaymentFingerprintVersion, i.TenantID, i.LegalEntityID, i.InstructionType, i.ResultID, subject,
		i.PayeeRef, i.DestinationID, i.DestinationVersion, i.DestinationFingerprint,
		i.Amount, i.Currency, i.ValueDate, i.PayerAccountRef, i.PaymentMethod, i.PaymentReference)
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HandedOffStatus reports whether a Banking attempt status means the provider has the
// instruction. Anything else (PREPARED, PENDING_UNKNOWN, a rejection, a quarantine) is
// NOT a handoff and never reads as one; a timeout is unknown, not failed and not paid.
func HandedOffStatus(attemptStatus string) bool { return attemptStatus == "SUBMITTED" }

// PaymentReferenceFor builds the narrative sent to Banking. It carries no worker name and
// no bank detail.
func PaymentReferenceFor(instrType, period, classCode string) string {
	if instrType == PayTypeLiab {
		return strings.TrimSpace(fmt.Sprintf("Payroll liability %s %s", classCode, period))
	}
	return "Payroll " + period
}

// ── requests ─────────────────────────────────────────────────────────────────

// CreatePaymentInstructionRequest deliberately has no tenant_id (WFP-NP-001) and no bank
// details: the beneficiary is resolved server-side from the payee master.
type CreatePaymentInstructionRequest struct {
	RunRefID        string  `json:"run_ref_id"`
	InstructionType string  `json:"instruction_type"`
	WorkerID        *string `json:"worker_id,omitempty"`
	LiabilityID     *string `json:"liability_id,omitempty"`
	// Amount applies to LIABILITY only (default: the liability's outstanding amount). A
	// NET_PAY amount is always the accepted net pay of the worker.
	Amount          string `json:"amount,omitempty"`
	PayerAccountRef string `json:"payer_account_ref"`
	ValueDate       string `json:"value_date"`
	PaymentMethod   string `json:"payment_method,omitempty"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
}

func (r *CreatePaymentInstructionRequest) Validate() error {
	if r.PaymentMethod == "" {
		r.PaymentMethod = PayMethodBank
	}
	if strings.TrimSpace(r.PayerAccountRef) == "" {
		return fmt.Errorf("payer_account_ref is required")
	}
	if _, err := time.Parse("2006-01-02", r.ValueDate); err != nil {
		return fmt.Errorf("value_date must be a date (YYYY-MM-DD)")
	}
	if r.PaymentMethod != PayMethodBank {
		return fmt.Errorf("payment_method must be %s", PayMethodBank)
	}
	switch r.InstructionType {
	case PayTypeNetPay:
		if r.WorkerID == nil || strings.TrimSpace(*r.WorkerID) == "" || r.LiabilityID != nil {
			return fmt.Errorf("a NET_PAY instruction needs worker_id and no liability_id")
		}
		if r.Amount != "" {
			return fmt.Errorf("a NET_PAY amount is the accepted net pay; it cannot be supplied")
		}
	case PayTypeLiab:
		if r.LiabilityID == nil || strings.TrimSpace(*r.LiabilityID) == "" || r.WorkerID != nil {
			return fmt.Errorf("a LIABILITY instruction needs liability_id and no worker_id")
		}
		if r.Amount != "" {
			a, err := ParseDecimal(r.Amount)
			if err != nil || a.Sign() <= 0 {
				return fmt.Errorf("amount must be a positive decimal with at most 4 places")
			}
		}
	default:
		return fmt.Errorf("instruction_type must be %s or %s", PayTypeNetPay, PayTypeLiab)
	}
	if len(r.IdempotencyKey) > 128 {
		return fmt.Errorf("idempotency_key is at most 128 characters")
	}
	return nil
}

type PaymentTransitionRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason,omitempty"`
}

type SupersedeInstructionsRequest struct {
	RunRefID string `json:"run_ref_id"`
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrInstructionNotFound        = errorString("payment instruction not found")
	ErrInstructionVersionConflict = errorString("stale expected_version: the instruction changed since it was read")
	ErrInstructionState           = errorString("the instruction is not in a state that allows this command")
	ErrInstructionResultNotActive = errorString("the payroll result behind this instruction is no longer ACCEPTED")
	ErrNothingToPay               = errorString("there is nothing to pay: the amount is zero")
	ErrSubjectNotPayable          = errorString("the liability cannot be paid now (not payable, under review or disputed)")
	ErrExceedsOutstanding         = errorString("the instruction would pay more than the liability's outstanding amount")
	ErrWorkerNotInResult          = errorString("the worker is not in the accepted payroll result")
	ErrPayeeMissing               = errorString("the worker has no VERIFIED payee reference on the effective financial profile")
	ErrPayeeUnavailable           = errorString("the payee master could not be reached; the beneficiary is unknown, not assumed")
	ErrNoActivePayee              = errorString("the payee has no ACTIVE destination in the payee master")
	ErrPayeeMismatch              = errorString("the payee destination does not match the payment (entity or currency)")
	ErrPayerAccountInvalid        = errorString("the paying account is not an ACTIVE, ownership-verified account of this legal entity in this currency")
	ErrPayerUnavailable           = errorString("treasury-svc could not be reached to validate the paying account")
	ErrPolicyUnavailable          = errorString("policy-svc could not be reached to assess the approval threshold")
	ErrWorkerProfileUnavailable   = errorString("employee-master-svc could not be reached to resolve the worker's payee")
	ErrApprovalInvalidated        = errorString("the payee, amount, account or result changed since validation; the approval no longer applies")
	ErrSoDPreparerOrReceiver      = errorString("separation of duties: the preparer and the principal who received the result cannot approve its payment")
	ErrSoDPayeeChanger            = errorString("separation of duties: a principal who changed this payee's bank destination cannot approve a payment to it")
	ErrAlreadyApproved            = errorString("this principal has already approved this instruction")
	ErrNotApproved                = errorString("only an APPROVED instruction can be submitted to Banking")
	ErrHandoffUnavailable         = errorString("payment-initiation-adapter-svc could not be reached; the instruction stays pending, it is not failed")
)
