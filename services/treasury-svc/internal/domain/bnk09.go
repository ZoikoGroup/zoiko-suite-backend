package domain

import "time"

// BNK-09 Treasury Transfer: a real maker-checker flow replacing the old
// InitiateTransfer, which only moved two internal cash_balances rows and
// called nothing external. PENDING_APPROVAL is the maker's initial state;
// APPROVED means the checker has signed off; SUBMITTED means
// payment-initiation-adapter-svc (BNK-06) has accepted the payment
// attempt; LEDGER_POSTED/INTERCOMPANY_PAIRED are cross-entity-only
// intermediate states; COMPLETED and REJECTED are terminal — see
// migration 000004's own reject_terminal_transfer_mutation trigger.
const (
	TransferPendingApproval    = "PENDING_APPROVAL"
	TransferApproved           = "APPROVED"
	TransferSubmitted          = "SUBMITTED"
	TransferLedgerPosted       = "LEDGER_POSTED"
	TransferIntercompanyPaired = "INTERCOMPANY_PAIRED"
	TransferCompleted          = "COMPLETED"
	TransferRejected           = "REJECTED"
)

func CanApproveTransfer(status string) bool { return status == TransferPendingApproval }
func CanRejectTransfer(status string) bool  { return status == TransferPendingApproval }

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
}

type ApproveTreasuryTransferParams struct {
	TenantID, TransferID, CheckerPrincipalID string
}

type RejectTreasuryTransferParams struct {
	TenantID, TransferID, CheckerPrincipalID, Reason string
}

var (
	ErrTransferNotFound               = errorString("treasury transfer not found")
	ErrInvalidTransferTransition      = errorString("treasury transfer is not in a state that permits this action")
	ErrTransferSelfApproval           = errorString("the principal who created a treasury transfer cannot also approve it")
	ErrTransferHashMismatch           = errorString("treasury transfer's protected fields do not match their state at creation")
	ErrPaymentAdapterUnavailable      = errorString("payment-initiation-adapter-svc unavailable")
	ErrPaymentRejected                = errorString("payment-initiation-adapter-svc rejected the payment attempt")
	ErrGLServiceUnavailable           = errorString("general-ledger-svc unavailable")
	ErrIntercompanyServiceUnavailable = errorString("intercompany-accounting-svc unavailable")
)
