package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// BNK-09 Treasury Transfer: a real maker-checker flow replacing the old
// InitiateTransfer, which only moved two internal cash_balances rows and
// called nothing external. PENDING_APPROVAL is the maker's initial state;
// APPROVED means the checker has signed off; SUBMITTED means
// payment-initiation-adapter-svc (BNK-06) has accepted the payment
// attempt; LEDGER_POSTED/INTERCOMPANY_PAIRED are cross-entity-only
// intermediate states; COMPLETED and REJECTED are terminal — see
// migration 000004's own reject_terminal_transfer_mutation trigger.
const (
	// TransferDraft is the doc's own first state ("Draft -> PendingApproval
	// -> ..."), reachable only when a caller opts in via
	// CreateTreasuryTransferParams.SaveAsDraft — CreateTreasuryTransfer's
	// default (unset) behavior is UNCHANGED for backward compatibility,
	// same posture as migration 000003's BNK-01 default.
	TransferDraft              = "DRAFT"
	TransferPendingApproval    = "PENDING_APPROVAL"
	TransferApproved           = "APPROVED"
	TransferSubmitted          = "SUBMITTED"
	TransferLedgerPosted       = "LEDGER_POSTED"
	TransferIntercompanyPaired = "INTERCOMPANY_PAIRED"
	TransferCompleted          = "COMPLETED"
	TransferRejected           = "REJECTED"
	// TransferReturned is reached when a transfer that already reached the
	// bank (SUBMITTED/LEDGER_POSTED/INTERCOMPANY_PAIRED) comes back
	// unexecuted — not terminal; ResolveTreasuryTransfer moves it onward.
	TransferReturned = "RETURNED"
	// TransferCancelled is reachable only before the bank ever saw the
	// transfer (DRAFT/PENDING_APPROVAL/APPROVED) via CancelBeforeSubmission,
	// or from RETURNED via ResolveTreasuryTransfer's "cancel" outcome —
	// terminal either way.
	TransferCancelled = "CANCELLED"
)

func CanApproveTransfer(status string) bool { return status == TransferPendingApproval }
func CanRejectTransfer(status string) bool  { return status == TransferPendingApproval }

// CanAmendTransfer/CanSubmitTransferForApproval: DRAFT-only. There is no
// approval yet to invalidate at this stage, which is what keeps the doc's
// SoD rule ("amount/date change invalidates approval") moot for these —
// amending is only ever legal before any approval exists.
func CanAmendTransfer(status string) bool             { return status == TransferDraft }
func CanSubmitTransferForApproval(status string) bool { return status == TransferDraft }

// CanCancelBeforeSubmission: anything strictly before the bank has seen
// the transfer — DRAFT, PENDING_APPROVAL, or APPROVED (but not yet
// SUBMITTED).
func CanCancelBeforeSubmission(status string) bool {
	switch status {
	case TransferDraft, TransferPendingApproval, TransferApproved:
		return true
	default:
		return false
	}
}

// CanMarkTransferReturned: only once the bank has actually seen it.
func CanMarkTransferReturned(status string) bool {
	switch status {
	case TransferSubmitted, TransferLedgerPosted, TransferIntercompanyPaired:
		return true
	default:
		return false
	}
}

func CanResolveTransfer(status string) bool { return status == TransferReturned }

// CanExecuteTransfer gates ExecuteTreasuryTransfer, which is a resumable
// saga driver — every non-terminal, post-approval status is a legal entry
// point, since the handler picks up from wherever the transfer currently
// is rather than requiring one specific status.
func CanExecuteTransfer(status string) bool {
	switch status {
	case TransferApproved, TransferSubmitted, TransferLedgerPosted, TransferIntercompanyPaired:
		return true
	default:
		return false
	}
}

type TreasuryTransfer struct {
	TransferID          string    `json:"transfer_id"`
	TenantID            string    `json:"tenant_id"`
	SourceBankAccountID string    `json:"source_bank_account_id"`
	TargetBankAccountID string    `json:"target_bank_account_id"`
	Amount              float64   `json:"amount"`
	CurrencyCode        string    `json:"currency_code"`
	CorrelationID       string    `json:"correlation_id,omitempty"`
	IsCrossEntity       bool      `json:"is_cross_entity"`
	ProtectedFieldHash  string    `json:"-"`
	Status              string    `json:"status"`
	MakerPrincipalID    string    `json:"maker_principal_id"`
	CheckerPrincipalID  string    `json:"checker_principal_id,omitempty"`
	RejectReason        string    `json:"reject_reason,omitempty"`
	PaymentAttemptID    string    `json:"payment_attempt_id,omitempty"`
	SourceJournalID     string    `json:"source_journal_id,omitempty"`
	IntercompanyEntryID string    `json:"intercompany_entry_id,omitempty"`
	CancelReason        string    `json:"cancel_reason,omitempty"`
	ReturnReason        string    `json:"return_reason,omitempty"`
	ResolutionNote      string    `json:"resolution_note,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type CreateTreasuryTransferParams struct {
	TenantID, SourceBankAccountID, TargetBankAccountID string
	Amount                                             float64
	CurrencyCode                                       string
	IsCrossEntity                                      bool
	CorrelationID                                      string
	MakerPrincipalID                                   string
	// SaveAsDraft creates the transfer in DRAFT instead of the default
	// PENDING_APPROVAL — opt-in only, see TransferDraft's own doc comment.
	SaveAsDraft bool
}

type ApproveTreasuryTransferParams struct {
	TenantID, TransferID, CheckerPrincipalID string
}

type RejectTreasuryTransferParams struct {
	TenantID, TransferID, CheckerPrincipalID, Reason string
}

// AmendTreasuryTransferParams: only the maker may amend, and only while
// still DRAFT. Every protected field is re-suppliable — the store
// recomputes protected_field_hash from the amended values.
type AmendTreasuryTransferParams struct {
	TenantID, TransferID                               string
	SourceBankAccountID, TargetBankAccountID            string
	Amount                                              float64
	CurrencyCode                                        string
	ActorPrincipalID                                    string
}

type SubmitTransferForApprovalParams struct {
	TenantID, TransferID, ActorPrincipalID string
}

// CancelBeforeSubmissionParams: only the maker may cancel their own
// transfer, matching the doc's command name exactly.
type CancelBeforeSubmissionParams struct {
	TenantID, TransferID, Reason, ActorPrincipalID string
}

// MarkTransferReturnedParams is an operator action (not maker-restricted)
// recording that the bank returned an already-submitted transfer
// unexecuted — the real trigger for the doc's TreasuryTransferReturned
// event.
type MarkTransferReturnedParams struct {
	TenantID, TransferID, Reason, ActorPrincipalID string
}

// ResolveTreasuryTransferParams: Resolution is "RESUBMIT" (back to
// PENDING_APPROVAL for a fresh maker-checker cycle — the prior approval
// is gone, the payment/ledger/intercompany correlation ids from the
// returned attempt are cleared) or "CANCEL" (-> CANCELLED, abandoned for
// good).
type ResolveTreasuryTransferParams struct {
	TenantID, TransferID, Resolution, Note, ActorPrincipalID string
}

const (
	ResolutionResubmit = "RESUBMIT"
	ResolutionCancel   = "CANCEL"
)

var (
	ErrTransferNotFound               = errorString("treasury transfer not found")
	ErrInvalidTransferTransition      = errorString("treasury transfer is not in a state that permits this action")
	ErrTransferSelfApproval           = errorString("the principal who created a treasury transfer cannot also approve it")
	ErrTransferHashMismatch           = errorString("treasury transfer's protected fields do not match their state at creation")
	ErrPaymentAdapterUnavailable      = errorString("payment-initiation-adapter-svc unavailable")
	ErrPaymentRejected                = errorString("payment-initiation-adapter-svc rejected the payment attempt")
	ErrGLServiceUnavailable           = errorString("general-ledger-svc unavailable")
	ErrIntercompanyServiceUnavailable = errorString("intercompany-accounting-svc unavailable")

	// ErrOnlyMakerMayModifyTransfer backs AmendTreasuryTransfer,
	// SubmitTransferForApproval and CancelBeforeSubmission — the doc names
	// these as the maker's own commands over their own not-yet-approved
	// transfer; a different principal has no legitimate reason to invoke
	// them (contrast with checker-only ApproveTreasuryTransfer/
	// RejectTreasuryTransfer, which enforce the opposite direction).
	ErrOnlyMakerMayModifyTransfer = errorString("only the principal who created this treasury transfer may amend, submit for approval, or cancel it")
	ErrInvalidResolution          = errorString("resolution must be RESUBMIT or CANCEL")
)

// TransferFingerprint (Wave 11b) is treasury-svc's own analogue of
// payment-proposal-svc's GetFingerprint (payment-proposal-svc/internal/
// handler/handler.go:663-684): a live, re-derivable hash of "the approved
// transfer subject" that payment-initiation-adapter-svc's PrepareAttempt
// (Wave 11a) independently re-fetches and compares by TransferID, instead
// of trusting a caller-supplied fingerprint outright — closing the
// AuthorizationSource carve-out Wave 11a left open for BNK-09-originated
// attempts.
//
// Field set decision: the doc names no concrete field set for BNK-09's
// own fingerprint. Rather than invent an unrelated design, this reuses
// exactly the fields ApproveTreasuryTransfer's own existing
// protected_field_hash already protects (Amount, CurrencyCode,
// SourceBankAccountID, TargetBankAccountID — see pg_store.go's
// protectedFieldHash), plus the transfer's identity (TransferID, Status)
// and the checker's identity (CheckerPrincipalID stands in for the plan's
// candidate "ApprovedByPrincipalID" — there is no separate
// approved_by_principal_id/approved_at column on treasury_transfers, and
// checker_principal_id/status/updated_at together are what the table
// actually records about approval, so this hashes what is real rather
// than adding columns to match a hypothetical field list). Algorithm/
// encoding mirrors payment-proposal-svc's GetFingerprint exactly:
// sha256 over the pipe-joined fields, hex-encoded, "sha256:" prefixed.
// Amount is formatted to 4 decimal places (not proposal-svc's 2) to match
// this table's own NUMERIC(20,4) precision and protected_field_hash's
// existing %.4f convention.
func TransferFingerprint(t *TreasuryTransfer) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%.4f|%s|%s|%s|%s",
		t.TransferID, t.Status, t.Amount, t.CurrencyCode, t.SourceBankAccountID, t.TargetBankAccountID, t.CheckerPrincipalID)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
